// Package registryv2 provides a BuildKit remote cache backend using
// the GitLab Container Registry v2 API with global content-addressed caching.
//
// Unlike the standard registry cache which stores manifests by name,
// this backend stores cache entries by their content digest, enabling
// automatic global cache sharing across all builds.
package registryv2

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/moby/buildkit/cache/remotecache"
	v1 "github.com/moby/buildkit/cache/remotecache/v1"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/compression"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	attrRegistryURL  = "registry"
	attrInsecure     = "insecure"
	attrTouchRefresh = "touch_refresh"
	attrToken        = "token"
	attrUsername     = "username"
	attrPassword     = "password"
)

// Config holds the configuration for the registryv2 cache backend.
type Config struct {
	// RegistryURL is the base URL of the registry (e.g., "http://localhost:5000")
	RegistryURL string

	// Insecure allows HTTP connections (default requires HTTPS)
	Insecure bool

	// TouchRefresh is the duration after which to update last_used_at
	TouchRefresh time.Duration

	// Token is the bearer token for authentication
	Token string

	// Username for basic auth
	Username string

	// Password for basic auth
	Password string
}

func getConfig(attrs map[string]string) (Config, error) {
	registryURL, ok := attrs[attrRegistryURL]
	if !ok {
		registryURL = os.Getenv("BUILDKIT_CACHE_REGISTRY_URL")
		if registryURL == "" {
			return Config{}, errors.New("registry URL not specified (use 'registry' attribute or BUILDKIT_CACHE_REGISTRY_URL env)")
		}
	}

	insecure := false
	if v, ok := attrs[attrInsecure]; ok {
		b, err := strconv.ParseBool(v)
		if err == nil {
			insecure = b
		}
	}

	touchRefresh := 24 * time.Hour
	if v, ok := attrs[attrTouchRefresh]; ok {
		d, err := time.ParseDuration(v)
		if err == nil {
			touchRefresh = d
		}
	}

	token := attrs[attrToken]
	if token == "" {
		token = os.Getenv("BUILDKIT_CACHE_REGISTRY_TOKEN")
	}

	username := attrs[attrUsername]
	password := attrs[attrPassword]

	return Config{
		RegistryURL:  registryURL,
		Insecure:     insecure,
		TouchRefresh: touchRefresh,
		Token:        token,
		Username:     username,
		Password:     password,
	}, nil
}

// ResolveCacheExporterFunc returns a function that resolves cache exporters.
func ResolveCacheExporterFunc() remotecache.ResolveCacheExporterFunc {
	return func(ctx context.Context, g session.Group, attrs map[string]string) (remotecache.Exporter, error) {
		config, err := getConfig(attrs)
		if err != nil {
			return nil, err
		}

		client, err := NewClient(config)
		if err != nil {
			return nil, err
		}

		cc := v1.NewCacheChains()
		return &exporter{
			CacheExporterTarget: cc,
			chains:              cc,
			client:              client,
			config:              config,
		}, nil
	}
}

// ResolveCacheImporterFunc returns a function that resolves cache importers.
func ResolveCacheImporterFunc() remotecache.ResolveCacheImporterFunc {
	return func(ctx context.Context, g session.Group, attrs map[string]string) (remotecache.Importer, ocispecs.Descriptor, error) {
		config, err := getConfig(attrs)
		if err != nil {
			return nil, ocispecs.Descriptor{}, err
		}

		client, err := NewClient(config)
		if err != nil {
			return nil, ocispecs.Descriptor{}, err
		}

		return &importer{
			client: client,
			config: config,
		}, ocispecs.Descriptor{}, nil
	}
}

// exporter implements remotecache.Exporter for registryv2.
type exporter struct {
	solver.CacheExporterTarget
	chains *v1.CacheChains
	client *Client
	config Config
}

func (e *exporter) Name() string {
	return "exporting cache to registry v2"
}

func (e *exporter) Config() remotecache.Config {
	return remotecache.Config{
		Compression: compression.New(compression.Default),
	}
}

func (e *exporter) Finalize(ctx context.Context) (map[string]string, error) {
	return e.finalize(ctx)
}

// importer implements remotecache.Importer for registryv2.
type importer struct {
	client *Client
	config Config
}

func (i *importer) Resolve(ctx context.Context, desc ocispecs.Descriptor, id string, w worker.Worker) (solver.CacheManager, error) {
	return i.resolve(ctx, desc, id, w)
}

// CacheEntry represents a cache entry stored in the registry.
type CacheEntry struct {
	Digest      digest.Digest   `json:"digest"`
	BlobDigest  digest.Digest   `json:"blob_digest"`
	CacheType   string          `json:"cache_type"`
	Description string          `json:"description,omitempty"`
	SizeBytes   int64           `json:"size_bytes"`
	CreatedAt   time.Time       `json:"created_at"`
	LastUsedAt  time.Time       `json:"last_used_at"`
	Parents     []digest.Digest `json:"parents,omitempty"`
}

// CacheMount represents a cache mount entry.
type CacheMount struct {
	MountID     string        `json:"mount_id"`
	TargetPath  string        `json:"target_path"`
	CacheDigest digest.Digest `json:"cache_digest"`
}
