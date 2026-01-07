// Package buildkitmetrics provides Prometheus metrics for BuildKit.
// Metrics are exposed via OpenTelemetry and available at /metrics endpoint.
package buildkitmetrics

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "github.com/moby/buildkit"

var (
	once sync.Once

	// cacheRequests counts cache hits and misses
	cacheRequests metric.Int64Counter

	// buildDuration records build duration in seconds
	buildDuration metric.Float64Histogram

	// cacheTransferBytes counts bytes transferred for cache operations
	cacheTransferBytes metric.Int64Counter
)

// Init initializes the metrics. Safe to call multiple times.
func Init() {
	once.Do(func() {
		meter := otel.Meter(meterName)

		var err error

		cacheRequests, err = meter.Int64Counter(
			"buildkit_cache_requests_total",
			metric.WithDescription("Total cache requests by result (hit or miss)"),
		)
		if err != nil {
			panic(err)
		}

		buildDuration, err = meter.Float64Histogram(
			"buildkit_build_duration_seconds",
			metric.WithDescription("Build duration in seconds"),
			metric.WithExplicitBucketBoundaries(1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096),
		)
		if err != nil {
			panic(err)
		}

		cacheTransferBytes, err = meter.Int64Counter(
			"buildkit_cache_transfer_bytes_total",
			metric.WithDescription("Total bytes transferred for cache operations"),
		)
		if err != nil {
			panic(err)
		}
	})
}

// RecordCacheHit records a cache hit.
func RecordCacheHit(ctx context.Context) {
	if cacheRequests != nil {
		cacheRequests.Add(ctx, 1, metric.WithAttributes(
			attribute.String("result", "hit"),
		))
	}
}

// RecordCacheMiss records a cache miss.
func RecordCacheMiss(ctx context.Context) {
	if cacheRequests != nil {
		cacheRequests.Add(ctx, 1, metric.WithAttributes(
			attribute.String("result", "miss"),
		))
	}
}

// RecordBuildDuration records a build duration in seconds.
func RecordBuildDuration(ctx context.Context, seconds float64, status string) {
	if buildDuration != nil {
		buildDuration.Record(ctx, seconds, metric.WithAttributes(
			attribute.String("status", status),
		))
	}
}

// RecordCacheTransfer records bytes transferred for cache operations.
// direction should be "upload" or "download".
// backend identifies the cache backend (e.g., "registryv2", "s3").
func RecordCacheTransfer(ctx context.Context, bytes int64, direction, backend string) {
	if cacheTransferBytes != nil {
		cacheTransferBytes.Add(ctx, bytes, metric.WithAttributes(
			attribute.String("direction", direction),
			attribute.String("backend", backend),
		))
	}
}
