# BuildKit Prometheus Metrics - Implementation Plan

## Overview

Add Prometheus metrics to BuildKit for cache observability.

- **Cache hit/miss & build duration**: Work for all cache backends (shared code)
- **Byte transfer**: S3 and registryv2 only (each backend requires separate instrumentation)

## Metrics Summary

| Metric | Type | Labels | Backends |
|--------|------|--------|----------|
| `buildkit_cache_requests_total` | Counter | `result` (hit/miss) | All |
| `buildkit_build_duration_seconds` | Histogram | `status` (success/error) | All |
| `buildkit_cache_transfer_bytes_total` | Counter | `direction`, `backend` | S3, registryv2 |

## Implementation Details

### Step 1: Create Metrics Package

**File**: `util/buildkitmetrics/metrics.go`

```go
package buildkitmetrics

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    // Cache hit/miss counter
    CacheRequests = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "buildkit",
            Name:      "cache_requests_total",
            Help:      "Total cache requests by result (hit or miss)",
        },
        []string{"result"},
    )

    // Build duration histogram
    BuildDuration = promauto.NewHistogramVec(
        prometheus.HistogramOpts{
            Namespace: "buildkit",
            Name:      "build_duration_seconds",
            Help:      "Build duration in seconds",
            Buckets:   prometheus.ExponentialBuckets(1, 2, 12), // 1s to ~1h
        },
        []string{"status"},
    )

    // Cache bytes transferred
    CacheTransferBytes = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "buildkit",
            Name:      "cache_transfer_bytes_total",
            Help:      "Total bytes transferred for cache operations",
        },
        []string{"direction", "backend"},
    )
)
```

### Step 2: Instrument Cache Hit/Miss

**File**: `solver/edge.go`

**Location**: `execIfPossible()` function (line ~907)

```go
import "github.com/moby/buildkit/util/buildkitmetrics"

func (e *edge) execIfPossible(f *pipeFactory) bool {
    if len(e.cacheRecords) > 0 {
        // CACHE HIT
        buildkitmetrics.CacheRequests.WithLabelValues("hit").Inc()
        
        if e.keysDidChange {
            e.postpone(f)
            return true
        }
        e.execReq = f.NewFuncRequest(e.loadCache)
        e.execCacheLoad = true
        // ... rest
    } else if e.allDepsCompleted {
        // CACHE MISS
        buildkitmetrics.CacheRequests.WithLabelValues("miss").Inc()
        
        if e.keysDidChange {
            e.postpone(f)
            return true
        }
        e.execReq = f.NewFuncRequest(e.execOp)
        // ... rest
    }
    return false
}
```

### Step 3: Instrument Build Duration

**File**: `control/control.go`

**Location**: `Controller.Solve()` function (line ~380)

```go
import (
    "time"
    "github.com/moby/buildkit/util/buildkitmetrics"
)

func (c *Controller) Solve(ctx context.Context, req *controlapi.SolveRequest) (*controlapi.SolveResponse, error) {
    startTime := time.Now()
    status := "success"
    
    defer func() {
        duration := time.Since(startTime).Seconds()
        buildkitmetrics.BuildDuration.WithLabelValues(status).Observe(duration)
    }()
    
    // ... existing code ...
    
    resp, err := c.solver.Solve(ctx, ...)
    if err != nil {
        status = "error"
        return nil, err
    }
    
    return resp, nil
}
```

### Step 4: Instrument Cache Bytes Transfer

For byte counting, we wrap the `ReaderAt` and upload functions. Each backend needs instrumentation:

#### Option A: Instrument at `readerat.go` (Download - all backends)

Each backend has a `readerat.go` with a `ReadAt` method. Wrap to count bytes:

**File**: `cache/remotecache/registryv2/readerat.go`

```go
func (r *readerAtCloser) ReadAt(p []byte, off int64) (n int, err error) {
    n, err = r.ra.ReadAt(p, off)
    if n > 0 {
        buildkitmetrics.CacheTransferBytes.WithLabelValues("download", "registryv2").Add(float64(n))
    }
    return n, err
}
```

Same pattern for `s3/readerat.go`, etc.

#### Option B: Instrument at upload functions

**File**: `cache/remotecache/registryv2/exporter.go`

```go
func (ce *exporter) uploadBlob(ctx context.Context, ra io.ReaderAt, size int64) (digest.Digest, error) {
    // ... existing code ...
    
    // After successful upload:
    buildkitmetrics.CacheTransferBytes.WithLabelValues("upload", "registryv2").Add(float64(size))
    
    return dgst, nil
}
```

**File**: `cache/remotecache/s3/s3.go`

```go
func (sr *s3Backend) saveMutableAt(ctx context.Context, key string, body io.Reader) error {
    // Wrap reader to count bytes
    countingReader := &countingReader{r: body, backend: "s3"}
    
    input := &s3.PutObjectInput{
        Bucket: &sr.bucket,
        Key:    aws.String(key),
        Body:   countingReader,
    }
    // ...
}

type countingReader struct {
    r       io.Reader
    backend string
    count   int64
}

func (cr *countingReader) Read(p []byte) (n int, err error) {
    n, err = cr.r.Read(p)
    if n > 0 {
        cr.count += int64(n)
        buildkitmetrics.CacheTransferBytes.WithLabelValues("upload", cr.backend).Add(float64(n))
    }
    return n, err
}
```

## File Changes Summary

| File | Change |
|------|--------|
| `util/buildkitmetrics/metrics.go` | NEW - Metric definitions |
| `solver/edge.go` | Add hit/miss counter |
| `control/control.go` | Add build duration histogram |
| `cache/remotecache/s3/readerat.go` | Add download byte counter (S3) |
| `cache/remotecache/s3/s3.go` | Add upload byte counter (S3) |
| `cache/remotecache/registryv2/readerat.go` | Add download byte counter (registryv2) |
| `cache/remotecache/registryv2/exporter.go` | Add upload byte counter (registryv2) |

**Total: 7 files** (1 new, 6 modified)

## Verification

After implementation, verify metrics appear:

```bash
# Start buildkitd with debug address
buildkitd --debugaddr 0.0.0.0:6060

# Check metrics endpoint
curl http://localhost:6060/metrics | grep buildkit_

# Expected output:
# buildkit_cache_requests_total{result="hit"} 42
# buildkit_cache_requests_total{result="miss"} 7
# buildkit_build_duration_seconds_bucket{status="success",le="1"} 2
# buildkit_cache_transfer_bytes_total{direction="download",backend="s3"} 1234567
```

## Grafana Queries

```promql
# Cache hit rate (last 5 minutes)
sum(rate(buildkit_cache_requests_total{result="hit"}[5m])) / 
sum(rate(buildkit_cache_requests_total[5m]))

# Average build duration
histogram_quantile(0.5, rate(buildkit_build_duration_seconds_bucket[5m]))

# Cache download throughput (MB/s)
sum(rate(buildkit_cache_transfer_bytes_total{direction="download"}[5m])) / 1024 / 1024

# Cache upload throughput (MB/s)  
sum(rate(buildkit_cache_transfer_bytes_total{direction="upload"}[5m])) / 1024 / 1024
```

## Implementation Order

1. Create `util/buildkitmetrics/metrics.go` with all metric definitions
2. Add cache hit/miss counter in `solver/edge.go`
3. Add build duration in `control/control.go`
4. Add byte counters to S3 backend (current production)
5. Add byte counters to registryv2 backend
6. Test: Verify all metrics appear on `/metrics` endpoint
