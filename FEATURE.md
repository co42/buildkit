# Keyless BuildKit Cache Feature

## Problem Statement

Currently, BuildKit remote caches (S3, registry) require users to manually specify a `name` parameter to identify cache entries:

```bash
--cache-to type=s3,bucket=cache,name=myapp-main  # Manual naming required
--cache-to type=registry,ref=registry.example.com/cache:myapp-main
```

This creates several issues:
1. **Manual configuration burden** - Users must choose the right key
2. **No automatic sharing** - Different projects with identical build steps don't share cache
3. **Cache invalidation complexity** - Names must be managed across CI pipelines

## Goal

Make remote cache work like local cache - **zero configuration, content-addressed, globally shared**:

```bash
--cache-to type=registryv2,registry=registry.example.com  # No name needed!
--cache-from type=registryv2,registry=registry.example.com
```

**Key insight**: If two projects run `RUN apt-get install python3`, they should automatically share the same cached layer because the content is identical.

## How It Works

### Content-Addressed Caching

Everything is identified by SHA256 digest:
- Same Dockerfile instruction + same inputs = same digest
- Same digest = automatic cache reuse across all projects
- No project names, branch names, or manual configuration needed

### Architecture

```
BuildKit Client
     |
     | New API routes (/v2/buildkit/cache/*)
     v
GitLab Container Registry (forked)
     |
     +-- PostgreSQL <-- Global cache index & metadata
     |
     +-- S3 <-- Content-addressed blobs (existing infrastructure)
```

The registry already has:
- S3 blob storage (content-addressed by SHA256)
- PostgreSQL for metadata
- REST API infrastructure
- Blob deduplication

We're adding:
- Cache-specific metadata tables in PostgreSQL
- New API routes for BuildKit to query/update cache entries

### Data Flow

**Export (Build Complete):**
1. BuildKit finishes build, has layers to cache
2. Exporter uploads blobs to registry S3 (existing `/v2/blobs` API)
3. Exporter registers metadata: `POST /v2/buildkit/cache/blobs` for each layer
4. Database stores: digest, type, description, parent relationship

**Import (New Build):**
1. BuildKit needs layer with digest `sha256:abc123...`
2. Importer checks: `HEAD /v2/buildkit/cache/blobs/sha256:abc123...`
3. If exists (200 OK): Cache HIT
   - Get metadata: `GET /v2/buildkit/cache/blobs/sha256:abc123...`
   - Walk parent chain if needed
   - Download blob from S3 when needed
4. If not exists (404): Cache MISS, execute instruction

### Example

```
Project A builds: RUN apt-get install python3
--> Creates cache with digest sha256:abc123...

Project B builds: RUN apt-get install python3  
--> Generates same digest sha256:abc123...
--> Cache HIT automatically! No configuration needed.
```

## Database Schema

Three new tables in the registry's PostgreSQL:

### `buildkit_cache_entries` - Main cache records
```sql
CREATE TABLE buildkit_cache_entries (
    id BIGSERIAL PRIMARY KEY,
    digest VARCHAR(255) NOT NULL UNIQUE,      -- Cache key (sha256:...)
    blob_digest BYTEA NOT NULL,               -- Blob digest (no FK for now, see Phase 2 Step 12)
    cache_type VARCHAR(50) NOT NULL,          -- 'regular' or 'exec.cachemount'
    description TEXT,                         -- e.g., "RUN apt-get update"
    size_bytes BIGINT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT check_buildkit_cache_type CHECK (cache_type IN ('regular', 'exec.cachemount'))
);
-- Note: FK constraint to blobs table will be added in Phase 2 Step 12 after blob upload is implemented
```

### `buildkit_cache_chain` - Layer parent-child relationships
```sql
CREATE TABLE buildkit_cache_chain (
    id BIGSERIAL PRIMARY KEY,
    child_digest VARCHAR(255) NOT NULL,
    parent_digest VARCHAR(255),               -- NULL for base layers
    chain_position INTEGER NOT NULL,
    CONSTRAINT fk_child FOREIGN KEY (child_digest) REFERENCES buildkit_cache_entries(digest) ON DELETE CASCADE,
    CONSTRAINT fk_parent FOREIGN KEY (parent_digest) REFERENCES buildkit_cache_entries(digest) ON DELETE SET NULL
);
```

### `buildkit_cache_mounts` - Cache mount tracking
```sql
CREATE TABLE buildkit_cache_mounts (
    id BIGSERIAL PRIMARY KEY,
    mount_id VARCHAR(255) NOT NULL,           -- e.g., "go-build-cache"
    target_path VARCHAR(500) NOT NULL,        -- e.g., "/root/.cache/go-build"
    cache_digest VARCHAR(255) NOT NULL,
    CONSTRAINT fk_cache FOREIGN KEY (cache_digest) REFERENCES buildkit_cache_entries(digest) ON DELETE CASCADE,
    UNIQUE(mount_id, target_path)
);
```

## API Endpoints

All endpoints under `/v2/buildkit/cache/`:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/blobs/{digest}` | HEAD | Check if cache entry exists (200/404) |
| `/blobs/{digest}` | GET | Get cache metadata |
| `/blobs` | POST | Create new cache entry |
| `/blobs/{digest}` | PATCH | Update last_used_at timestamp |
| `/query?parent={digest}` | GET | Find all children of a parent |
| `/gc?older_than=7d&limit=100` | GET | Query old entries for garbage collection |
| `/mounts/{mount_id}` | GET | Get cache mount info |
| `/mounts/{mount_id}` | PUT | Create/update cache mount |

### Example API Calls

```bash
# Check cache exists
curl -X HEAD http://registry:5052/v2/buildkit/cache/blobs/sha256:abc123...

# Get cache metadata
curl http://registry:5052/v2/buildkit/cache/blobs/sha256:abc123...
# Response:
# {
#   "digest": "sha256:abc123...",
#   "type": "regular",
#   "description": "RUN apt-get update",
#   "size": 1234567,
#   "created_at": "2026-01-06T10:00:00Z",
#   "last_used_at": "2026-01-06T14:30:00Z",
#   "blob_location": "/v2/blobs/sha256:abc123...",
#   "parent": "sha256:def456..."
# }

# Create cache entry
curl -X POST http://registry:5052/v2/buildkit/cache/blobs \
  -H "Content-Type: application/json" \
  -d '{
    "digest": "sha256:abc123...",
    "blob_digest": "sha256:abc123...",
    "type": "regular",
    "description": "RUN apt-get update",
    "size": 1234567,
    "parent": "sha256:def456..."
  }'

# Query children by parent
curl "http://registry:5052/v2/buildkit/cache/query?parent=sha256:def456..."

# GC query (find old entries)
curl "http://registry:5052/v2/buildkit/cache/gc?older_than=7d&limit=100"
```

## BuildKit Integration

New cache backend in BuildKit:

### File Structure
```
buildkit/cache/remotecache/registryv2/
+-- registryv2.go    # Main entry point, register backend
+-- client.go        # HTTP client for registry API
+-- exporter.go      # Cache export (upload) logic
+-- importer.go      # Cache import (download) logic
```

### Usage
```bash
# Export cache after build
docker buildx build \
  --cache-to type=registryv2,registry=registry.example.com \
  .

# Import cache before build  
docker buildx build \
  --cache-from type=registryv2,registry=registry.example.com \
  .
```

### Optional Parameters
- `insecure=true` - For HTTP registries
- `auth=<token>` - Authentication if needed

## Implementation Steps

### Registry Side (gitlab-container-registry)

- [x] **Step 1: Database Schema**
  - Created migration `20260106163803_create_buildkit_cache_tables.go`
  - Added 3 tables with proper indexes and foreign keys

- [x] **Step 2: Data Models & Store**
  - Added models in `registry/datastore/models/models.go`
  - Created `BuildKitCacheStore` in `registry/datastore/buildkit_cache.go`
  - Defined routes in `registry/api/buildkit/v1/routes.go`
  - Added error codes in `registry/api/buildkit/v1/errors.go`

- [x] **Step 3: API Handlers**
  - Implemented all handlers in `registry/handlers/buildkit_cache.go`
  - Registered routes in `registry/handlers/app.go`
  - All 7 endpoints tested and working

### BuildKit Side (buildkit)

- [x] **Step 4: Create registryv2 package skeleton**
  - Created `cache/remotecache/registryv2/` directory
  - Added `registryv2.go` with Config, ResolveCacheExporterFunc, ResolveCacheImporterFunc
  - Added `readerat.go` helper for blob reading

- [x] **Step 5: Implement client.go**
  - HTTP client wrapper for registry API
  - Methods: `Exists()`, `Get()`, `Create()`, `Touch()`, `QueryByParent()`, `GetMount()`, `SetMount()`
  - ReaderAt implementation for blob access

- [x] **Step 6: Implement exporter.go**
  - Parallel upload with configurable parallelism
  - Check existing entries before upload
  - Touch existing entries to update last_used_at
  - Create cache entries with parent relationships

- [x] **Step 7: Implement importer.go**
  - Load all cache entries from registry
  - Build cache chains from parent relationships
  - Create DescriptorProviderPairs for v1.CacheChains

- [x] **Step 8: Integration Testing**
  - Build with cache export works - entries stored in PostgreSQL
  - Build with cache import works - manifest loaded from registry
  - Cache entries persist across builds

### Phase 2: Blob Storage Integration

- [x] **Step 9: Blob Upload in Exporter**
  - Uses registry's standard OCI blob upload API (`POST /v2/<name>/blobs/uploads/`)
  - Uploads blob content before creating cache entry
  - Uses dedicated repository `buildkit-cache` for all cache blobs (auto-created by registry)
  - Checks blob existence before upload to avoid duplicates

- [x] **Step 10: Blob Download in Importer**
  - Implemented `GetBlob` to fetch from `/v2/buildkit-cache/blobs/{digest}`
  - Supports Range requests for partial reads (offset parameter)
  - Integrated with containerd's content.Provider interface via `ReaderAt`

### Phase 3: Cache Chain Reconstruction

- [x] **Step 11: Cache Chain Reconstruction**
  - Store full `CacheConfig` (layers + records) as manifest in registry
  - Manifest stored at `buildkit-cache:cache-manifest` tag
  - Uses standard `v1.ParseConfig` to reconstruct cache chains
  - Proper cache keys enable BuildKit to match RUN instructions

- [x] **Step 12: Add Foreign Key Constraint**
  - Added FK constraint via migration `20260106180000_add_buildkit_cache_blob_fk`
  - `CONSTRAINT fk_buildkit_cache_blob_digest FOREIGN KEY (blob_digest) REFERENCES blobs(digest) ON DELETE CASCADE`
  - Cache entries are automatically cleaned up when blobs are garbage collected

- [x] **Step 13: End-to-End Cache Hit Testing**
  - Verified: Clear local cache, rebuild shows `CACHED` for RUN layers
  - Layers retrieved from remote registry cache
  - Full cache hit working for identical build steps

## Files Modified/Created

### gitlab-container-registry
```
registry/
+-- api/buildkit/v1/
|   +-- routes.go                    # Route definitions
|   +-- errors.go                    # Error codes
+-- datastore/
|   +-- models/models.go             # Added BuildKit models
|   +-- buildkit_cache.go            # BuildKitCacheStore with FindRootEntries
|   +-- migrations/premigrations/
|       +-- 20260106163803_create_buildkit_cache_tables.go  # Schema
|       +-- 20260106180000_add_buildkit_cache_blob_fk.go    # FK constraint
+-- handlers/
    +-- app.go                       # Route registration
    +-- buildkit_cache.go            # HTTP handlers
```

### buildkit
```
cache/remotecache/registryv2/
+-- registryv2.go    # DONE - Main entry point, Config, resolver functions
+-- client.go        # DONE - HTTP client for registry API
+-- exporter.go      # DONE - Cache export logic
+-- importer.go      # DONE - Cache import logic
+-- readerat.go      # DONE - ReaderAt helper for blob reading
+-- e2e_test.go      # DONE - Comprehensive E2E tests (10 test cases)
```

## E2E Test Suite

Comprehensive end-to-end tests are in `cache/remotecache/registryv2/e2e_test.go`. These tests use real infrastructure (registry + buildkitd) and verify cache behavior across multiple scenarios.

### Running Tests

Tests must run inside Lima VM (buildkitd requires Linux):

```bash
# Build test binary for Linux
GOOS=linux GOARCH=arm64 go test -c -o registryv2_test.bin ./cache/remotecache/registryv2/

# Copy and run in Lima
limactl copy registryv2_test.bin default:/tmp/registryv2_test.bin
limactl shell default -- sudo REGISTRY_URL=http://192.168.5.2:5050 /tmp/registryv2_test.bin -test.v -test.run "TestE2E_"
```

### Test Cases

| Test | What It Verifies |
|------|------------------|
| `TestE2E_BasicCacheExportImport` | Basic export/import cycle with simple Dockerfile |
| `TestE2E_MultipleRunInstructions` | 4 RUN instructions all hit cache on rebuild |
| `TestE2E_CacheSharingBetweenBuilds` | Identical layers shared across different builds |
| `TestE2E_MultiStageBuild` | Multi-stage builds with COPY --from caching |
| `TestE2E_PackageInstallation` | Real `apk add curl wget` caching (slow operations) |
| `TestE2E_IncrementalChanges` | Only changed layers rebuild, stable layers cached |
| `TestE2E_ArgAndEnv` | ARG and ENV instruction caching |
| `TestE2E_CopyWithContext` | COPY from build context caching |
| `TestE2E_VerifyCacheAPI` | Direct HTTP API endpoint tests |
| `TestE2E_FullScenario` | Complete CI/CD workflow: cold, warm, incremental builds |

See DEV.md for detailed instructions on running tests.

## Testing the Current Implementation

Start the dev environment (see DEV.md), then:

```bash
# Verify API is working
curl http://localhost:5052/v2/buildkit/cache/gc
# Expected: {"entries":[]}

# Create a cache entry
curl -X POST http://localhost:5052/v2/buildkit/cache/blobs \
  -H "Content-Type: application/json" \
  -d '{
    "digest":"sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
    "blob_digest":"sha256:5711127a7748d32f5a69380c27daf1382f2c6674ea7a60d2a3e338818590fea1",
    "type":"regular",
    "description":"Test entry",
    "size":1000
  }'

# Verify it exists
curl http://localhost:5052/v2/buildkit/cache/blobs/sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef
```

## Current Status

### Phase 1 Complete (Metadata Infrastructure)
- [x] User builds without specifying `name` parameter
- [x] Cache metadata stored in PostgreSQL
- [x] Cache entries persist across registry restarts
- [x] API endpoints for cache management working

### Phase 2 Complete (Blob Storage Integration)
- [x] Blobs uploaded to dedicated `buildkit-cache` repository
- [x] Blobs downloaded during cache import
- [x] Base layers (e.g., alpine) retrieved from remote cache
- [x] Registry auto-creates repository on first blob upload

### Phase 3 Complete (Cache Chain Reconstruction)
- [x] RUN instruction cache hits working
- [x] Full CacheConfig stored as manifest in registry
- [x] v1.ParseConfig reconstructs cache chains correctly
- [x] FK constraint ensures GC cleans cache entries with blobs

### All Phases Complete!
The keyless BuildKit cache feature is fully functional:
- Same content across projects = automatic cache reuse
- No manual `name` parameter needed
- Works with standard BuildKit `--import-cache` and `--export-cache` flags

## Known Limitations & Future Work

### Cache Blobs Not Linked to Manifest

**Issue**: Cache layer blobs are uploaded to the `buildkit-cache` repository but are not properly linked to a manifest. The current cache manifest only stores the `CacheConfig` JSON (layers metadata + records), but the `layers` field in the OCI manifest is empty (`"layers": []`).

**Impact**: 
- Blobs exist in `repository_blobs` table but are not referenced by any manifest layer
- If garbage collection runs, these blobs may be treated as orphans and deleted
- The `buildkit_cache_entries` table has FK to `blobs.digest`, so cache entries would cascade-delete with the blobs

**Workaround**: 
- Currently GC is not explicitly configured, so blobs are safe for now
- For production: either disable GC for the `buildkit-cache` repository, or implement proper blob linkage

**Future Fix Options**:
1. Modify GC to check `buildkit_cache_entries.blob_digest` before deleting orphan blobs
2. Create per-layer manifests (or periodically update the main manifest to include all cache blobs)
3. Implement separate cache blob lifecycle management with its own retention policy

## References

- Original implementation spec: User's initial message in this conversation
- DEV.md: Local development setup instructions
- BuildKit remote cache docs: https://github.com/moby/buildkit#remote-cache
