# BuildKit + Registry Local Development Setup

## Prerequisites
- Lima VM with nerdctl
- Go 1.25+

## 1. Start Lima VM
```bash
limactl start
```

## 2. Start Infrastructure Containers

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

## 3. Start Registry

```bash
cd ~/hf/gitlab-container-registry

# Run database migrations (first time only)
go run ./cmd/registry database migrate up config.yml

# Start registry (runs on port 5052)
go run ./cmd/registry serve config.yml
```

## 4. Build and Push Test Image

```bash
# Create test Dockerfile
lima mkdir -p /tmp/test-build
lima bash -c 'cat > /tmp/test-build/Dockerfile << EOF
FROM alpine:3.19
RUN echo "Hello from buildkit test" > /hello.txt
CMD ["cat", "/hello.txt"]
EOF'

# Build and push
lima nerdctl build \
  --output type=image,name=host.lima.internal:5052/test/hello:latest,push=true,registry.insecure=true \
  /tmp/test-build/
```

## 5. Verify

```bash
# Check registry catalog
curl -s http://localhost:5052/v2/_catalog

# Check tags
curl -s http://localhost:5052/v2/test/hello/tags/list

# Check manifest
curl -s -H "Accept: application/vnd.docker.distribution.manifest.v2+json" \
  http://localhost:5052/v2/test/hello/manifests/latest

# Pull and run
lima nerdctl pull --insecure-registry host.lima.internal:5052/test/hello:latest
lima nerdctl run --rm host.lima.internal:5052/test/hello:latest

# Check S3 storage
lima nerdctl exec minio-s3 mc ls local/registry-bucket --recursive

# Check PostgreSQL
lima nerdctl exec registry-postgres psql -U registry -c "SELECT name, path FROM repositories;"
lima nerdctl exec registry-postgres psql -U registry -c "SELECT encode(digest, 'hex'), size FROM blobs;"
```

## Ports

| Service    | Port |
|------------|------|
| Registry   | 5052 |
| PostgreSQL | 5432 |
| Minio S3   | 6666 |

## Cleanup

```bash
lima nerdctl rm -f registry-postgres minio-s3
```
