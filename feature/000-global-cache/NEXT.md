```
**Export (after build):**
1. BuildKit uploads blobs to registry S3 (`/v2/buildkit-cache/blobs/uploads/`)
2. Registers metadata: `POST /v2/buildkit/cache/blobs`
3. Stores CacheConfig manifest at `buildkit-cache:cache-manifest`

**Import (before build):**
1. Fetch manifest from `buildkit-cache:cache-manifest`
2. Parse CacheConfig to reconstruct cache chains
3. Download blobs on-demand during build
```

Check to validate that it indeed works like that.

```
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
```

That too

```
## Database Schema
```

That too

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

That too and merge both migrations files
