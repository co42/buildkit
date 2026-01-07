# Global Cache - Progress Tracker

## Quick Navigation

| Section | Description |
|---------|-------------|
| [Status](#status) | Current state |
| [Completed](#completed) | What's done |
| [In Progress](#in-progress) | Current work |
| [Backlog](#backlog) | Future work |

## Status

**Current Phase**: Production Readiness - Prometheus Metrics Implementation

The core feature is complete. All 3 implementation phases are done and E2E tests pass. Now adding Prometheus metrics for production observability.

## Completed

### Phase 1: Metadata Infrastructure
- [x] Database schema (3 tables with indexes/FKs)
- [x] Data models and store (`BuildKitCacheStore`)
- [x] API handlers (all 7 endpoints)
- [x] Route registration

### Phase 2: Blob Storage Integration
- [x] Blob upload via OCI API
- [x] Blob download with Range support
- [x] Dedicated `buildkit-cache` repository
- [x] Blob existence check before upload

### Phase 3: Cache Chain Reconstruction
- [x] CacheConfig manifest storage
- [x] `v1.ParseConfig` integration
- [x] FK constraint for GC cascade
- [x] E2E cache hit verification

### Testing
- [x] 10 E2E test cases covering all scenarios
- [x] Multi-stage builds
- [x] Incremental changes
- [x] Cache sharing between builds

## In Progress

### Prometheus Metrics Implementation

**Goal**: Add observability metrics to BuildKit for cache effectiveness tracking in production.

**Infrastructure**: BuildKit already has Prometheus support:
- `/metrics` endpoint on `--debugaddr` (e.g., `0.0.0.0:6060`)
- OpenTelemetry + Prometheus exporter in `cmd/buildkitd/main.go`

**Detailed plan**: See [METRICS_PLAN.md](./METRICS_PLAN.md)

#### Metrics to Implement

| Metric | Type | Labels | Backends |
|--------|------|--------|----------|
| `buildkit_cache_requests_total` | Counter | `result` (hit/miss) | All |
| `buildkit_build_duration_seconds` | Histogram | `status` (success/error) | All |
| `buildkit_cache_transfer_bytes_total` | Counter | `direction`, `backend` | S3, registryv2 |

#### Implementation Steps

1. [ ] Create `util/buildkitmetrics/metrics.go` with metric definitions
2. [ ] Add cache hit/miss counter in `solver/edge.go`
3. [ ] Add build duration histogram in `control/control.go`
4. [ ] Add byte counters to S3 backend
5. [ ] Add byte counters to registryv2 backend
6. [ ] Test: Verify metrics on `/metrics` endpoint

#### Files to Modify

| File | Change |
|------|--------|
| `util/buildkitmetrics/metrics.go` | NEW - Metric definitions |
| `solver/edge.go` | Cache hit/miss counter |
| `control/control.go` | Build duration histogram |
| `cache/remotecache/s3/readerat.go` | Download bytes (S3) |
| `cache/remotecache/s3/s3.go` | Upload bytes (S3) |
| `cache/remotecache/registryv2/readerat.go` | Download bytes (registryv2) |
| `cache/remotecache/registryv2/exporter.go` | Upload bytes (registryv2) |

## Backlog

### Observability
- [ ] Grafana dashboard for cache metrics
- [ ] Alerting rules (low hit rate, slow builds)
- [ ] Cache age distribution metrics

### Production Hardening
- [ ] GC policy for cache entries
- [ ] Authentication for cache API
- [ ] Rate limiting
- [ ] Load testing

### Future Enhancements
- [ ] Multi-registry fallback
- [ ] Cache compression
- [ ] Cache warming for common layers
- [ ] Per-project analytics

## Known Issues

### Cache Blobs Not Linked to Manifest
Blobs uploaded to `buildkit-cache` are not in manifest's `layers` field. GC could delete them as orphans.

**Status**: Not blocking - GC disabled for now.
