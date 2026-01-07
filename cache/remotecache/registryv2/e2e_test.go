package registryv2

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// End-to-end tests for the registryv2 cache backend.
// These tests require the full infrastructure to be running:
// - GitLab Container Registry with PostgreSQL (on REGISTRY_URL, default http://localhost:5050)
// - BuildKit daemon accessible via buildctl (BUILDCTL_ADDR or unix:///run/buildkit/buildkitd.sock)
//
// To run these tests:
//   1. Start the registry and buildkitd
//   2. Set environment variables if not using defaults:
//      - REGISTRY_URL=http://localhost:5050
//      - BUILDCTL_ADDR=unix:///run/buildkit/buildkitd.sock
//   3. Run: go test -v ./cache/remotecache/registryv2/... -run TestE2E

// getRegistryURL returns the registry URL from environment or default.
func getRegistryURL() string {
	if url := os.Getenv("REGISTRY_URL"); url != "" {
		return url
	}
	return "http://localhost:5050"
}

// getBuildctlAddr returns the buildctl address from environment or default.
func getBuildctlAddr() string {
	if addr := os.Getenv("BUILDCTL_ADDR"); addr != "" {
		return addr
	}
	return "unix:///run/buildkit/buildkitd.sock"
}

// getMetricsURL returns the buildkitd metrics endpoint URL.
func getMetricsURL() string {
	if url := os.Getenv("BUILDKIT_METRICS_URL"); url != "" {
		return url
	}
	return "http://localhost:6060/metrics"
}

// getMetricValue queries the Prometheus metrics endpoint and returns the value for a metric.
// metricName is the base metric name (e.g., "buildkit_cache_requests_total")
// labels is a map of label names to values to match (e.g., {"result": "hit"})
// Returns the metric value, or -1 if not found.
func getMetricValue(t *testing.T, metricName string, labels map[string]string) float64 {
	t.Helper()

	resp, err := http.Get(getMetricsURL())
	if err != nil {
		t.Logf("Failed to fetch metrics: %v", err)
		return -1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Logf("Metrics endpoint returned status %d", resp.StatusCode)
		return -1
	}

	// Parse metric lines - look for lines starting with metricName
	// Format: metric_name{label1="value1",label2="value2"} value
	// OTel adds extra labels like otel_scope_name, so we need to check if our
	// required labels are present anywhere in the label set
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, metricName+"{") && !strings.HasPrefix(line, metricName+" ") {
			continue
		}

		// Check if all required labels are present
		allLabelsMatch := true
		for k, v := range labels {
			// Look for label="value" pattern anywhere in the line
			labelPattern := k + `="` + regexp.QuoteMeta(v) + `"`
			if !strings.Contains(line, labelPattern) {
				allLabelsMatch = false
				break
			}
		}

		if !allLabelsMatch {
			continue
		}

		// Extract the value at the end of the line
		valuePattern := regexp.MustCompile(`\}\s+([0-9.e+-]+)$`)
		matches := valuePattern.FindStringSubmatch(line)
		if matches != nil {
			val, err := strconv.ParseFloat(matches[1], 64)
			if err == nil {
				return val
			}
		}
	}

	return -1
}

// getCacheHitCount returns the current cache hit count from metrics.
func getCacheHitCount(t *testing.T) float64 {
	return getMetricValue(t, "buildkit_cache_requests_total", map[string]string{"result": "hit"})
}

// getCacheMissCount returns the current cache miss count from metrics.
func getCacheMissCount(t *testing.T) float64 {
	return getMetricValue(t, "buildkit_cache_requests_total", map[string]string{"result": "miss"})
}

// cacheMetricsSnapshot holds cache metrics at a point in time.
type cacheMetricsSnapshot struct {
	hits   float64
	misses float64
}

// getCacheMetricsSnapshot returns the current cache metrics.
func getCacheMetricsSnapshot(t *testing.T) cacheMetricsSnapshot {
	return cacheMetricsSnapshot{
		hits:   getCacheHitCount(t),
		misses: getCacheMissCount(t),
	}
}

// verifyCacheHits checks that we got the expected number of cache hits.
// It verifies both by parsing the build output for "CACHED" strings and by checking metrics.
// minExpectedHits is the minimum number of cache hits expected.
func verifyCacheHits(t *testing.T, output string, beforeMetrics cacheMetricsSnapshot, minExpectedHits int) {
	t.Helper()

	// Check logs for CACHED occurrences
	cachedCount := strings.Count(strings.ToUpper(output), "CACHED")
	t.Logf("Cache hits from logs: %d CACHED occurrences", cachedCount)

	// Check metrics
	afterMetrics := getCacheMetricsSnapshot(t)
	metricsHits := int(afterMetrics.hits - beforeMetrics.hits)
	if beforeMetrics.hits < 0 || afterMetrics.hits < 0 {
		t.Log("Metrics not available, skipping metrics check")
		// Fall back to log-only check
		require.GreaterOrEqual(t, cachedCount, minExpectedHits,
			"Expected at least %d cache hits from logs, got %d", minExpectedHits, cachedCount)
		return
	}

	t.Logf("Cache hits from metrics: %d (before: %.0f, after: %.0f)", metricsHits, beforeMetrics.hits, afterMetrics.hits)

	// Both should indicate cache hits
	require.GreaterOrEqual(t, cachedCount, minExpectedHits,
		"Expected at least %d cache hits from logs, got %d", minExpectedHits, cachedCount)

	// Metrics should show at least as many hits (may be more due to internal caching)
	require.GreaterOrEqual(t, metricsHits, minExpectedHits,
		"Expected at least %d cache hits from metrics, got %d", minExpectedHits, metricsHits)

	t.Logf("Cache verification passed: logs=%d, metrics=%d (expected >= %d)", cachedCount, metricsHits, minExpectedHits)
}

// verifyCacheHitOrExtract checks cache hits that may show as CACHED or extracting (for remote cache).
func verifyCacheHitOrExtract(t *testing.T, output string, beforeMetrics cacheMetricsSnapshot, minExpectedHits int) {
	t.Helper()

	// Check logs for CACHED or extracting occurrences
	cachedCount := strings.Count(strings.ToUpper(output), "CACHED")
	extractCount := strings.Count(strings.ToLower(output), "extracting sha256:")
	totalLogHits := cachedCount + extractCount
	t.Logf("Cache hits from logs: %d CACHED + %d extracted = %d total", cachedCount, extractCount, totalLogHits)

	// Check metrics
	afterMetrics := getCacheMetricsSnapshot(t)
	metricsHits := int(afterMetrics.hits - beforeMetrics.hits)
	if beforeMetrics.hits < 0 || afterMetrics.hits < 0 {
		t.Log("Metrics not available, skipping metrics check")
		require.GreaterOrEqual(t, totalLogHits, minExpectedHits,
			"Expected at least %d cache hits from logs, got %d", minExpectedHits, totalLogHits)
		return
	}

	t.Logf("Cache hits from metrics: %d (before: %.0f, after: %.0f)", metricsHits, beforeMetrics.hits, afterMetrics.hits)

	require.GreaterOrEqual(t, totalLogHits, minExpectedHits,
		"Expected at least %d cache hits from logs, got %d", minExpectedHits, totalLogHits)

	require.GreaterOrEqual(t, metricsHits, minExpectedHits,
		"Expected at least %d cache hits from metrics, got %d", minExpectedHits, metricsHits)

	t.Logf("Cache verification passed: logs=%d, metrics=%d (expected >= %d)", totalLogHits, metricsHits, minExpectedHits)
}

// skipIfNoInfrastructure skips the test if the required infrastructure is not available.
func skipIfNoInfrastructure(t *testing.T) {
	t.Helper()

	// Check if registry is accessible
	registryURL := getRegistryURL()
	resp, err := http.Get(registryURL + "/v2/")
	if err != nil {
		t.Skipf("Registry not accessible at %s: %v", registryURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnauthorized {
		t.Skipf("Registry returned unexpected status %d", resp.StatusCode)
	}

	// Check if buildctl is available
	if _, err := exec.LookPath("buildctl"); err != nil {
		t.Skip("buildctl not found in PATH")
	}
}

// runBuildctl runs buildctl with the given arguments.
func runBuildctl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	allArgs := append([]string{"--addr", getBuildctlAddr()}, args...)
	cmd := exec.Command("buildctl", allArgs...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// checkRateLimitError checks if the error is a Docker Hub rate limit error and skips the test.
func checkRateLimitError(t *testing.T, output string, err error) {
	t.Helper()
	if err != nil && strings.Contains(output, "429 Too Many Requests") {
		t.Skip("Docker Hub rate limit reached - skipping test")
	}
}

// runBuildctlOrSkipOnRateLimit runs buildctl and skips the test if rate limited.
func runBuildctlOrSkipOnRateLimit(t *testing.T, args ...string) string {
	t.Helper()
	output, err := runBuildctl(t, args...)
	checkRateLimitError(t, output, err)
	require.NoError(t, err, "Build failed: %s", output)
	return output
}

// pruneBuildkitCache clears all local BuildKit cache.
func pruneBuildkitCache(t *testing.T) {
	t.Helper()
	output, err := runBuildctl(t, "prune", "--all")
	if err != nil {
		t.Logf("prune output: %s", output)
	}
	// Give it a moment to complete
	time.Sleep(500 * time.Millisecond)
}

// createDockerfile creates a temporary Dockerfile with the given content.
func createDockerfile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	dockerfilePath := dir + "/Dockerfile"
	err := os.WriteFile(dockerfilePath, []byte(content), 0644)
	require.NoError(t, err)
	return dir
}

// TestE2E_BasicCacheExportImport tests basic cache export and import.
func TestE2E_BasicCacheExportImport(t *testing.T) {
	skipIfNoInfrastructure(t)

	registryURL := getRegistryURL()

	// Create a simple Dockerfile
	dockerfile := `FROM alpine:latest
RUN echo "hello from basic test" > /hello.txt
`
	contextDir := createDockerfile(t, dockerfile)

	// Clear caches
	pruneBuildkitCache(t)

	// Build with cache export
	output := runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-basic:latest,push=false",
	)
	t.Logf("Export build output:\n%s", output)

	// Verify cache entries were created in registry
	resp, err := http.Get(registryURL + "/v2/buildkit/cache/query?parent=")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	t.Logf("Cache entries after export: %s", string(body))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Prune local cache
	pruneBuildkitCache(t)

	// Snapshot metrics before import build
	beforeMetrics := getCacheMetricsSnapshot(t)

	// Rebuild with cache import - should be faster (cache hit)
	output = runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--import-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-basic:latest,push=false",
	)
	t.Logf("Import build output:\n%s", output)

	// Verify cache hits via both logs and metrics
	verifyCacheHits(t, output, beforeMetrics, 1)
}

// TestE2E_MultipleRunInstructions tests caching with multiple RUN instructions.
func TestE2E_MultipleRunInstructions(t *testing.T) {
	skipIfNoInfrastructure(t)

	registryURL := getRegistryURL()

	dockerfile := `FROM alpine:latest
RUN echo "step 1" > /step1.txt
RUN echo "step 2" > /step2.txt
RUN echo "step 3" > /step3.txt
RUN echo "step 4" > /step4.txt
`
	contextDir := createDockerfile(t, dockerfile)

	pruneBuildkitCache(t)

	// Export cache
	output := runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-multi-run:latest,push=false",
	)
	t.Logf("Multi-run export:\n%s", output)

	// Prune and rebuild
	pruneBuildkitCache(t)

	// Snapshot metrics before import build
	beforeMetrics := getCacheMetricsSnapshot(t)

	output = runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--import-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-multi-run:latest,push=false",
	)
	t.Logf("Multi-run import:\n%s", output)

	// Verify cache hits via both logs and metrics
	verifyCacheHits(t, output, beforeMetrics, 4)
}

// TestE2E_CacheSharingBetweenBuilds tests that identical layers are shared across builds.
func TestE2E_CacheSharingBetweenBuilds(t *testing.T) {
	skipIfNoInfrastructure(t)

	registryURL := getRegistryURL()

	// First build with shared base layer
	dockerfile1 := `FROM alpine:latest
RUN echo "shared layer" > /shared.txt
RUN echo "unique to build 1" > /unique.txt
`
	contextDir1 := createDockerfile(t, dockerfile1)

	pruneBuildkitCache(t)

	output := runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir1,
		"--local", "dockerfile="+contextDir1,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-share1:latest,push=false",
	)
	t.Logf("Build 1 output:\n%s", output)

	// Second build with same shared layer but different unique layer
	dockerfile2 := `FROM alpine:latest
RUN echo "shared layer" > /shared.txt
RUN echo "unique to build 2" > /unique.txt
`
	contextDir2 := createDockerfile(t, dockerfile2)

	pruneBuildkitCache(t)

	// Snapshot metrics before build 2
	beforeMetrics := getCacheMetricsSnapshot(t)

	// Build 2 should hit cache for the shared layer
	output = runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir2,
		"--local", "dockerfile="+contextDir2,
		"--import-cache", "type=registryv2,registry="+registryURL,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-share2:latest,push=false",
	)
	t.Logf("Build 2 output:\n%s", output)

	// Verify cache hits via both logs and metrics (shared layer should be cached)
	verifyCacheHitOrExtract(t, output, beforeMetrics, 1)
}

// TestE2E_MultiStageBuild tests caching with multi-stage builds.
func TestE2E_MultiStageBuild(t *testing.T) {
	skipIfNoInfrastructure(t)

	registryURL := getRegistryURL()

	dockerfile := `FROM alpine:latest AS builder
RUN echo "build artifact" > /artifact.txt

FROM alpine:latest AS runtime
COPY --from=builder /artifact.txt /app/artifact.txt
RUN echo "runtime setup" > /setup.txt
`
	contextDir := createDockerfile(t, dockerfile)

	pruneBuildkitCache(t)

	// Export cache
	output := runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-multistage:latest,push=false",
	)
	t.Logf("Multi-stage export:\n%s", output)

	// Prune and rebuild
	pruneBuildkitCache(t)

	// Snapshot metrics before import build
	beforeMetrics := getCacheMetricsSnapshot(t)

	output = runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--import-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-multistage:latest,push=false",
	)
	t.Logf("Multi-stage import:\n%s", output)

	// Verify cache hits via both logs and metrics
	verifyCacheHits(t, output, beforeMetrics, 1)
}

// TestE2E_PackageInstallation tests caching of package installation commands.
func TestE2E_PackageInstallation(t *testing.T) {
	skipIfNoInfrastructure(t)

	registryURL := getRegistryURL()

	// This tests a realistic scenario - package installation which is typically slow
	dockerfile := `FROM alpine:latest
RUN apk add --no-cache curl wget
RUN echo "packages installed" > /done.txt
`
	contextDir := createDockerfile(t, dockerfile)

	pruneBuildkitCache(t)

	// First build - this will be slow (downloading packages)
	start := time.Now()
	output := runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-packages:latest,push=false",
	)
	firstBuildDuration := time.Since(start)
	t.Logf("First build (cold): %v\n%s", firstBuildDuration, output)

	// Prune and rebuild
	pruneBuildkitCache(t)

	// Snapshot metrics before cached build
	beforeMetrics := getCacheMetricsSnapshot(t)

	// Second build - should be fast (cached)
	start = time.Now()
	output = runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--import-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-packages:latest,push=false",
	)
	secondBuildDuration := time.Since(start)
	t.Logf("Second build (cached): %v\n%s", secondBuildDuration, output)

	// Verify cache hits via both logs and metrics
	verifyCacheHits(t, output, beforeMetrics, 1)
}

// TestE2E_IncrementalChanges tests that only changed layers are rebuilt.
func TestE2E_IncrementalChanges(t *testing.T) {
	skipIfNoInfrastructure(t)

	registryURL := getRegistryURL()

	// Initial build with multiple layers
	dockerfile1 := `FROM alpine:latest
RUN echo "layer 1 - stable" > /layer1.txt
RUN echo "layer 2 - stable" > /layer2.txt
RUN echo "layer 3 - version 1" > /layer3.txt
`
	contextDir := createDockerfile(t, dockerfile1)

	pruneBuildkitCache(t)

	output := runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-incremental:latest,push=false",
	)
	t.Logf("Initial build:\n%s", output)

	// Modify only the last layer
	dockerfile2 := `FROM alpine:latest
RUN echo "layer 1 - stable" > /layer1.txt
RUN echo "layer 2 - stable" > /layer2.txt
RUN echo "layer 3 - version 2" > /layer3.txt
`
	err := os.WriteFile(contextDir+"/Dockerfile", []byte(dockerfile2), 0644)
	require.NoError(t, err)

	pruneBuildkitCache(t)

	// Snapshot metrics before incremental build
	beforeMetrics := getCacheMetricsSnapshot(t)

	output = runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--import-cache", "type=registryv2,registry="+registryURL,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-incremental:latest,push=false",
	)
	t.Logf("Incremental build:\n%s", output)

	// Verify cache hits via both logs and metrics (first two stable layers should be cached)
	verifyCacheHitOrExtract(t, output, beforeMetrics, 2)
}

// TestE2E_ArgAndEnv tests caching with ARG and ENV instructions.
func TestE2E_ArgAndEnv(t *testing.T) {
	skipIfNoInfrastructure(t)

	registryURL := getRegistryURL()

	dockerfile := `FROM alpine:latest
ARG VERSION=1.0
ENV APP_VERSION=$VERSION
RUN echo "version: $APP_VERSION" > /version.txt
RUN echo "static content" > /static.txt
`
	contextDir := createDockerfile(t, dockerfile)

	pruneBuildkitCache(t)

	// Build with default ARG
	output := runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-arg:latest,push=false",
	)
	t.Logf("ARG build:\n%s", output)

	pruneBuildkitCache(t)

	// Snapshot metrics before rebuild
	beforeMetrics := getCacheMetricsSnapshot(t)

	// Rebuild with same ARG - should hit cache
	output = runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--import-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-arg:latest,push=false",
	)
	t.Logf("ARG rebuild:\n%s", output)

	// Verify cache hits via both logs and metrics
	verifyCacheHits(t, output, beforeMetrics, 1)
}

// TestE2E_CopyWithContext tests caching with COPY from build context.
func TestE2E_CopyWithContext(t *testing.T) {
	skipIfNoInfrastructure(t)

	registryURL := getRegistryURL()

	contextDir := t.TempDir()

	// Create source file
	err := os.WriteFile(contextDir+"/source.txt", []byte("source content v1"), 0644)
	require.NoError(t, err)

	dockerfile := `FROM alpine:latest
COPY source.txt /app/source.txt
RUN echo "processing done" > /done.txt
`
	err = os.WriteFile(contextDir+"/Dockerfile", []byte(dockerfile), 0644)
	require.NoError(t, err)

	pruneBuildkitCache(t)

	// First build
	output := runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-copy:latest,push=false",
	)
	t.Logf("COPY build:\n%s", output)

	pruneBuildkitCache(t)

	// Snapshot metrics before rebuild
	beforeMetrics := getCacheMetricsSnapshot(t)

	// Rebuild without changes - should hit cache
	output = runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--import-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=test-copy:latest,push=false",
	)
	t.Logf("COPY rebuild:\n%s", output)

	// Verify cache hits via both logs and metrics
	verifyCacheHits(t, output, beforeMetrics, 1)
}

// TestE2E_VerifyCacheAPI tests the registry cache API directly.
func TestE2E_VerifyCacheAPI(t *testing.T) {
	// Only check registry, not buildctl - this test only uses HTTP
	registryURL := getRegistryURL()
	resp, err := http.Get(registryURL + "/v2/")
	if err != nil {
		t.Skipf("Registry not accessible at %s: %v", registryURL, err)
	}
	resp.Body.Close()

	// Test query endpoint
	resp, err = http.Get(registryURL + "/v2/buildkit/cache/query?parent=")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	t.Logf("Query response: %s", string(body))

	// Test GC endpoint
	resp, err = http.Get(registryURL + "/v2/buildkit/cache/gc")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	t.Logf("GC response: %s", string(body))
}

// TestE2E_FullScenario tests a complete CI/CD-like workflow.
func TestE2E_FullScenario(t *testing.T) {
	skipIfNoInfrastructure(t)

	registryURL := getRegistryURL()

	t.Log("=== Full E2E Scenario: Simulating realistic CI/CD workflow ===")

	// Simulate a realistic Go application build
	dockerfile := `FROM alpine:latest

# Install dependencies (should be cached across builds)
RUN apk add --no-cache ca-certificates tzdata

# Create app directory
RUN mkdir -p /app

# Add application (simulated)
RUN echo "package main" > /app/main.go && \
    echo 'import "fmt"' >> /app/main.go && \
    echo 'func main() { fmt.Println("Hello") }' >> /app/main.go

# Final setup
RUN echo "Build complete: $(date)" > /app/build.log
`
	contextDir := createDockerfile(t, dockerfile)

	// === First build (cold) ===
	t.Log("Step 1: Initial build (cold cache)")
	pruneBuildkitCache(t)

	start := time.Now()
	output := runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=full-scenario:v1,push=false",
	)
	coldDuration := time.Since(start)
	t.Logf("Cold build took: %v", coldDuration)

	// === Second build (warm - should use cache) ===
	t.Log("Step 2: Rebuild after prune (should hit cache)")
	pruneBuildkitCache(t)

	// Snapshot metrics before warm build
	beforeMetrics := getCacheMetricsSnapshot(t)

	start = time.Now()
	output = runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--import-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=full-scenario:v1,push=false",
	)
	warmDuration := time.Since(start)
	t.Logf("Warm build took: %v", warmDuration)

	// Verify cache hits via both logs and metrics
	verifyCacheHits(t, output, beforeMetrics, 3)

	// === Third build (incremental change) ===
	t.Log("Step 3: Incremental change (only last layer changes)")

	// Modify only the last RUN instruction
	dockerfileV2 := `FROM alpine:latest

# Install dependencies (should be cached across builds)
RUN apk add --no-cache ca-certificates tzdata

# Create app directory
RUN mkdir -p /app

# Add application (simulated)
RUN echo "package main" > /app/main.go && \
    echo 'import "fmt"' >> /app/main.go && \
    echo 'func main() { fmt.Println("Hello") }' >> /app/main.go

# Final setup - CHANGED
RUN echo "Build complete v2: $(date)" > /app/build.log
`
	err := os.WriteFile(contextDir+"/Dockerfile", []byte(dockerfileV2), 0644)
	require.NoError(t, err)

	pruneBuildkitCache(t)

	// Snapshot metrics before incremental build
	beforeMetrics = getCacheMetricsSnapshot(t)

	start = time.Now()
	output = runBuildctlOrSkipOnRateLimit(t,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context="+contextDir,
		"--local", "dockerfile="+contextDir,
		"--import-cache", "type=registryv2,registry="+registryURL,
		"--export-cache", "type=registryv2,registry="+registryURL,
		"--output", "type=image,name=full-scenario:v2,push=false",
	)
	incrementalDuration := time.Since(start)
	t.Logf("Incremental build took: %v", incrementalDuration)

	// Verify cache hits via both logs and metrics (first 3 layers should be cached)
	verifyCacheHitOrExtract(t, output, beforeMetrics, 3)

	// === Summary ===
	t.Log("=== Summary ===")
	t.Logf("Cold build:        %v", coldDuration)
	t.Logf("Warm build:        %v", warmDuration)
	t.Logf("Incremental build: %v", incrementalDuration)

	if coldDuration > 3*time.Second {
		speedup := float64(coldDuration) / float64(warmDuration)
		t.Logf("Cache speedup: %.1fx", speedup)
	}

	t.Log("=== Full scenario completed successfully ===")
}

func init() {
	// Ensure tests can find buildctl in common locations
	paths := []string{
		"/usr/local/bin",
		"/usr/bin",
		os.Getenv("HOME") + "/go/bin",
	}
	currentPath := os.Getenv("PATH")
	for _, p := range paths {
		if !strings.Contains(currentPath, p) {
			currentPath = p + ":" + currentPath
		}
	}
	os.Setenv("PATH", currentPath)
}
