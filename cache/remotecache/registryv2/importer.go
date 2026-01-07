package registryv2

import (
	"context"
	"encoding/json"

	"github.com/containerd/containerd/v2/pkg/labels"
	v1 "github.com/moby/buildkit/cache/remotecache/v1"
	cacheimporttypes "github.com/moby/buildkit/cache/remotecache/v1/types"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/worker"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// resolve loads the cache from the registry and returns a cache manager.
func (i *importer) resolve(ctx context.Context, desc ocispecs.Descriptor, id string, w worker.Worker) (solver.CacheManager, error) {
	cc, err := i.load(ctx)
	if err != nil {
		return nil, err
	}

	keysStorage, resultStorage, err := v1.NewCacheKeyStorage(cc, w)
	if err != nil {
		return nil, err
	}

	return solver.NewCacheManager(ctx, id, keysStorage, resultStorage), nil
}

// load fetches the cache manifest from the registry and builds the cache chains.
func (i *importer) load(ctx context.Context) (*v1.CacheChains, error) {
	// Load the cache manifest from the registry
	configData, err := i.client.LoadCacheManifest(ctx)
	if err != nil {
		// If loading fails, return empty cache chains
		return v1.NewCacheChains(), nil
	}

	if configData == nil {
		// No cache manifest exists yet
		return v1.NewCacheChains(), nil
	}

	// Parse the config to get layer information
	var config cacheimporttypes.CacheConfig
	if err := json.Unmarshal(configData, &config); err != nil {
		return v1.NewCacheChains(), nil
	}

	// Build the layer provider map from the cache config layers
	// The v1.Parse function looks up layers by their Blob digest
	allLayers := i.buildLayerProviderFromConfig(config)

	// Parse the cache config using the standard v1 parser
	cc := v1.NewCacheChains()
	if err := v1.ParseConfig(config, allLayers, cc); err != nil {
		// If parsing fails, return empty cache chains
		return v1.NewCacheChains(), nil
	}

	return cc, nil
}

// buildLayerProviderFromConfig builds a DescriptorProvider from the cache config layers.
// Each layer in the config has a Blob digest that v1.Parse uses to look up the provider.
func (i *importer) buildLayerProviderFromConfig(config cacheimporttypes.CacheConfig) v1.DescriptorProvider {
	layers := v1.DescriptorProvider{}

	for _, layer := range config.Layers {
		// Create descriptor for this layer
		desc := ocispecs.Descriptor{
			Digest: layer.Blob,
		}

		// Use annotations from the layer if available
		if layer.Annotations != nil {
			desc.MediaType = layer.Annotations.MediaType
			desc.Size = layer.Annotations.Size
			desc.Annotations = make(map[string]string)

			if layer.Annotations.DiffID != "" {
				desc.Annotations[labels.LabelUncompressed] = layer.Annotations.DiffID.String()
			}
			if !layer.Annotations.CreatedAt.IsZero() {
				txt, _ := layer.Annotations.CreatedAt.MarshalText()
				desc.Annotations["buildkit/createdat"] = string(txt)
			}
		} else {
			// Default media type if not specified
			desc.MediaType = "application/vnd.oci.image.layer.v1.tar+gzip"
		}

		// Map by the layer Blob digest (what v1.Parse looks up)
		layers[layer.Blob] = v1.DescriptorProviderPair{
			Descriptor: desc,
			Provider:   i.client,
		}
	}

	return layers
}
