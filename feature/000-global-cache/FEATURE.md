# Keyless BuildKit Cache

## Quick Navigation

| Section | Description |
|---------|-------------|
| [Overview](#overview) | What this feature does |
| [Architecture](#architecture) | How it works |
| [API Reference](#api-reference) | Registry endpoints |
| [Usage](#usage) | How to use the cache |
| [Database Schema](#database-schema) | PostgreSQL tables |
| [Files](#files-modifiedcreated) | Code locations |

## Overview

**Problem**: BuildKit remote caches require manual `name` parameter to identify cache entries. Different projects with identical build steps don't share cache.

**Solution**: Content-addressed caching - everything identified by SHA256 digest. Same Dockerfile instruction + same inputs = same digest = automatic cache reuse.

```bash
# Before (manual naming)
--cache-to type=s3,bucket=cache,name=myapp-main

# After (zero config)
--cache-to type=registryv2,registry=registry.example.com
```

## Architecture

```
BuildKit Client
     |
     | /v2/buildkit/cache/* API
     v
GitLab Container Registry (forked)
     |
     +-- PostgreSQL  <-- Cache metadata & index
     |
     +-- S3          <-- Content-addressed blobs
```

### Data Flow

**Export (after build):**
1. BuildKit uploads blobs to registry S3 (`/v2/buildkit-cache/blobs/uploads/`)
2. Registers metadata: `POST /v2/buildkit/cache/blobs`
3. Stores CacheConfig manifest at `buildkit-cache:cache-manifest`

**Import (before build):**
1. Fetch manifest from `buildkit-cache:cache-manifest`
2. Parse CacheConfig to reconstruct cache chains
3. Download blobs on-demand during build

## API Reference

Base path: `/v2/buildkit/cache/`

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/blobs/{digest}` | HEAD | Check if cache entry exists |
| `/blobs/{digest}` | GET | Get cache metadata |
| `/blobs` | POST | Create cache entry |
| `/blobs/{digest}` | PATCH | Update last_used_at |
| `/query?parent={digest}` | GET | Find children of parent |
| `/gc?older_than=7d&limit=100` | GET | Query old entries |
| `/mounts/{mount_id}` | GET/PUT | Cache mount info |

### Example Requests

```bash
# Check cache exists
curl -I http://registry:5050/v2/buildkit/cache/blobs/sha256:abc123...

# Get metadata
curl http://registry:5050/v2/buildkit/cache/blobs/sha256:abc123...

# Create entry
curl -X POST http://registry:5050/v2/buildkit/cache/blobs \
  -H "Content-Type: application/json" \
  -d '{"digest":"sha256:...","blob_digest":"sha256:...","type":"regular","size":1234}'
```

## Usage

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

**Parameters:**
- `registry` (required): Registry URL
- `insecure=true`: For HTTP registries

## Database Schema

### `buildkit_cache_entries`
```sql
CREATE TABLE buildkit_cache_entries (
    id BIGSERIAL PRIMARY KEY,
    digest VARCHAR(255) NOT NULL UNIQUE,
    blob_digest BYTEA NOT NULL,
    cache_type VARCHAR(50) NOT NULL,  -- 'regular' or 'exec.cachemount'
    description TEXT,
    size_bytes BIGINT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    last_used_at TIMESTAMPTZ DEFAULT NOW(),
    CONSTRAINT fk_blob FOREIGN KEY (blob_digest) REFERENCES blobs(digest) ON DELETE CASCADE
);
```

### `buildkit_cache_chain`
```sql
CREATE TABLE buildkit_cache_chain (
    id BIGSERIAL PRIMARY KEY,
    child_digest VARCHAR(255) NOT NULL REFERENCES buildkit_cache_entries(digest),
    parent_digest VARCHAR(255) REFERENCES buildkit_cache_entries(digest),
    chain_position INTEGER NOT NULL
);
```

### `buildkit_cache_mounts`
```sql
CREATE TABLE buildkit_cache_mounts (
    id BIGSERIAL PRIMARY KEY,
    mount_id VARCHAR(255) NOT NULL,
    target_path VARCHAR(500) NOT NULL,
    cache_digest VARCHAR(255) NOT NULL REFERENCES buildkit_cache_entries(digest),
    UNIQUE(mount_id, target_path)
);
```

## Files Modified/Created

### gitlab-container-registry
```
registry/
├── api/buildkit/v1/
│   ├── routes.go              # Route definitions
│   └── errors.go              # Error codes
├── datastore/
│   ├── models/models.go       # BuildKit models
│   ├── buildkit_cache.go      # Store implementation
│   └── migrations/premigrations/
│       ├── 20260106163803_create_buildkit_cache_tables.go
│       └── 20260106180000_add_buildkit_cache_blob_fk.go
└── handlers/
    ├── app.go                 # Route registration
    └── buildkit_cache.go      # HTTP handlers
```

### buildkit
```
cache/remotecache/registryv2/
├── registryv2.go    # Entry point, Config, resolvers
├── client.go        # HTTP client for registry API
├── exporter.go      # Cache export logic
├── importer.go      # Cache import logic
├── readerat.go      # ReaderAt helper
└── e2e_test.go      # E2E tests (10 test cases)
```

## Known Limitations

### Cache Blobs Not Linked to Manifest

Cache blobs are uploaded to `buildkit-cache` repository but not linked to the manifest's `layers` field. If GC runs, blobs may be treated as orphans.

**Current workaround**: GC not enabled for `buildkit-cache` repository.

**Future fix options**:
1. Modify GC to check `buildkit_cache_entries.blob_digest`
2. Periodically update manifest to include all cache blobs
3. Implement separate cache blob lifecycle management
