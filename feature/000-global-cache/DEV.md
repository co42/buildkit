# Development Setup

Local development environment for the keyless BuildKit cache feature.

## Quick Navigation

| Section | Description |
|---------|-------------|
| [Prerequisites](#prerequisites) | What you need |
| [Infrastructure](#infrastructure) | Start PostgreSQL & Minio |
| [Registry](#registry) | Run the container registry |
| [BuildKit](#buildkit) | Build and deploy buildkitd |
| [Testing](#testing) | Test cache export/import |
| [E2E Tests](#e2e-tests) | Run test suite |
| [Debugging](#debugging) | Troubleshooting commands |

## Prerequisites

- Lima VM with nerdctl
- Go 1.25+

```bash
limactl start
```

## Infrastructure

### PostgreSQL
```bash
lima nerdctl run --name registry-postgres \
  -e POSTGRES_USER=registry \
  -e POSTGRES_PASSWORD=mysecretpassword \
  -p 5432:5432 -d postgres:17
```

### Minio (S3)
```bash
lima nerdctl run -d --name minio-s3 \
  -p 6666:9000 \
  -e MINIO_ROOT_USER=testuser \
  -e MINIO_ROOT_PASSWORD=testpassword \
  minio/minio server /data

# Create bucket
lima nerdctl exec minio-s3 mc alias set local http://localhost:9000 testuser testpassword
lima nerdctl exec minio-s3 mc mb local/registry-bucket --ignore-existing
```

## Registry

```bash
cd ~/hf/gitlab-container-registry

# Run migrations (first time)
go run ./cmd/registry database migrate up config.yml

# Start registry (port 5050)
go run ./cmd/registry serve config.yml
```

## BuildKit

```bash
cd ~/hf/buildkit

# Build for Linux
GOOS=linux GOARCH=arm64 go build -o bin/buildkitd ./cmd/buildkitd

# Deploy to Lima
limactl copy bin/buildkitd default:/tmp/buildkitd
limactl shell default -- sudo killall -9 buildkitd 2>/dev/null
limactl shell default -- sudo cp /tmp/buildkitd /usr/local/bin/buildkitd
limactl shell default -- sudo /usr/local/bin/buildkitd \
  --addr unix:///run/buildkit/buildkitd.sock \
  --addr tcp://0.0.0.0:1234 --debug &
```

## Testing

### Cache Export
```bash
# Create test Dockerfile
cat > /tmp/Dockerfile.test << 'EOF'
FROM alpine:latest
RUN echo "layer 1" > /test1.txt
RUN echo "layer 2" > /test2.txt
EOF

HOST_IP=$(limactl shell default -- ip route | grep default | awk '{print $3}')

limactl shell default -- sudo buildctl \
  --addr unix:///run/buildkit/buildkitd.sock build \
  --frontend dockerfile.v0 \
  --local context=/tmp --local dockerfile=/tmp \
  --export-cache type=registryv2,registry=http://${HOST_IP}:5050 \
  --output type=image,name=test:latest,push=false
```

### Cache Import
```bash
# Clear local cache
limactl shell default -- sudo buildctl \
  --addr unix:///run/buildkit/buildkitd.sock prune --all

# Rebuild with import
limactl shell default -- sudo buildctl \
  --addr unix:///run/buildkit/buildkitd.sock build \
  --frontend dockerfile.v0 \
  --local context=/tmp --local dockerfile=/tmp \
  --import-cache type=registryv2,registry=http://${HOST_IP}:5050 \
  --output type=image,name=test:latest,push=false
```

### Verify
```bash
# Check cache entries
PGPASSWORD=mysecretpassword psql -h localhost -U registry -d registry \
  -c "SELECT digest, cache_type, size_bytes FROM buildkit_cache_entries;"

# Query API
curl -s "http://localhost:5050/v2/buildkit/cache/gc" | jq .
```

## E2E Tests

Tests run inside Lima VM (buildkitd requires Linux):

```bash
cd ~/hf/buildkit

# Build test binary
GOOS=linux GOARCH=arm64 go test -c -o registryv2_test.bin ./cache/remotecache/registryv2/

# Copy and run
limactl copy registryv2_test.bin default:/tmp/registryv2_test.bin
HOST_IP=$(limactl shell default -- ip route | grep default | awk '{print $3}')

limactl shell default -- sudo chmod +x /tmp/registryv2_test.bin
limactl shell default -- sudo REGISTRY_URL=http://${HOST_IP}:5050 \
  /tmp/registryv2_test.bin -test.v -test.run "TestE2E_"
```

### Test Cases

| Test | Description |
|------|-------------|
| `TestE2E_BasicCacheExportImport` | Basic export/import cycle |
| `TestE2E_MultipleRunInstructions` | Multiple RUN cache hits |
| `TestE2E_CacheSharingBetweenBuilds` | Shared layers across builds |
| `TestE2E_MultiStageBuild` | Multi-stage with COPY --from |
| `TestE2E_PackageInstallation` | Real `apk add` caching |
| `TestE2E_IncrementalChanges` | Only changed layers rebuild |
| `TestE2E_FullScenario` | Complete CI/CD workflow |

## Debugging

### Cache State
```bash
# Entry count by type
PGPASSWORD=mysecretpassword psql -h localhost -U registry -d registry \
  -c "SELECT cache_type, COUNT(*), pg_size_pretty(SUM(size_bytes)) FROM buildkit_cache_entries GROUP BY cache_type;"

# Largest entries
PGPASSWORD=mysecretpassword psql -h localhost -U registry -d registry \
  -c "SELECT digest, pg_size_pretty(size_bytes) FROM buildkit_cache_entries ORDER BY size_bytes DESC LIMIT 5;"

# Oldest unused
PGPASSWORD=mysecretpassword psql -h localhost -U registry -d registry \
  -c "SELECT digest, last_used_at FROM buildkit_cache_entries ORDER BY last_used_at ASC LIMIT 5;"
```

### S3 Storage
```bash
lima nerdctl exec minio-s3 mc ls local/registry-bucket --recursive | wc -l
lima nerdctl exec minio-s3 mc du local/registry-bucket
```

### API Health
```bash
time curl -s http://localhost:5050/v2/buildkit/cache/gc?limit=100 > /dev/null
```

## Ports

| Service | Port |
|---------|------|
| Registry | 5050 |
| PostgreSQL | 5432 |
| Minio S3 | 6666 |
| buildkitd | 1234 |

## Cleanup

```bash
lima nerdctl rm -f registry-postgres minio-s3
limactl shell default -- sudo killall buildkitd
```
