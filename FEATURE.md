# Keyless BuildKit Cache Feature

## Problem Statement

Currently, BuildKit remote caches (S3, registry) require users to manually specify a `name` parameter to identify cache entries:

```bash
--cache-to type=s3,bucket=cache,name=myapp-main  # Manual naming required
--cache-to type=registry,ref=registry.example.com/cache:myapp-main
```

This creates several issues:
1. **Manual configuration burden** - Users must decide on naming conventions
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
    blob_digest BYTEA NOT NULL,               -- Reference to blobs table
    cache_type VARCHAR(50) NOT NULL,          -- 'regular' or 'exec.cachemount'
    description TEXT,                         -- e.g., "RUN apt-get update"
    size_bytes BIGINT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_blob FOREIGN KEY (blob_digest) REFERENCES blobs(digest) ON DELETE CASCADE
);
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

- [ ] **Step 8: Integration Testing**
  - Build same Dockerfile twice, verify cache hit
  - Different projects with same content, verify sharing
  - Test concurrent builds

## Files Modified/Created

### gitlab-container-registry
```
registry/
+-- api/buildkit/v1/
|   +-- routes.go                    # NEW - Route definitions
|   +-- errors.go                    # NEW - Error codes
+-- datastore/
|   +-- models/models.go             # MODIFIED - Added BuildKit models
|   +-- buildkit_cache.go            # NEW - BuildKitCacheStore
|   +-- migrations/premigrations/
|       +-- 20260106163803_create_buildkit_cache_tables.go  # NEW
+-- handlers/
    +-- app.go                       # MODIFIED - Route registration
    +-- buildkit_cache.go            # NEW - HTTP handlers
```

### buildkit
```
cache/remotecache/registryv2/
+-- registryv2.go    # DONE - Main entry point, Config, resolver functions
+-- client.go        # DONE - HTTP client for registry API
+-- exporter.go      # DONE - Cache export logic
+-- importer.go      # DONE - Cache import logic
+-- readerat.go      # DONE - ReaderAt helper for blob reading
```

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

## Success Criteria

- [ ] User builds without specifying `name` parameter
- [ ] Same content across projects = automatic cache reuse
- [ ] Cache survives registry restarts
- [ ] Works with multiple concurrent builds
- [ ] GC can clean old cache entries

## References

- Original implementation spec: User's initial message in this conversation
- DEV.md: Local development setup instructions
- BuildKit remote cache docs: https://github.com/moby/buildkit#remote-cache
