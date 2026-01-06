package registryv2

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/containerd/containerd/v2/pkg/labels"
	cacheimporttypes "github.com/moby/buildkit/cache/remotecache/v1/types"
	"github.com/moby/buildkit/util/progress"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

const (
	uploadParallelism = 4
)

// finalize exports the cache chains to the registry.
func (e *exporter) finalize(ctx context.Context) (map[string]string, error) {
	cacheConfig, descs, err := e.chains.Marshal(ctx)
	if err != nil {
		return nil, err
	}

	if len(cacheConfig.Layers) == 0 {
		return nil, nil
	}

	eg, groupCtx := errgroup.WithContext(ctx)
	tasks := make(chan int, uploadParallelism)

	go func() {
		for i := range cacheConfig.Layers {
			tasks <- i
		}
		close(tasks)
	}()

	for range uploadParallelism {
		eg.Go(func() error {
			for index := range tasks {
				layer := cacheConfig.Layers[index]
				blob := layer.Blob

				dgstPair, ok := descs[blob]
				if !ok {
					return errors.Errorf("missing blob %s", blob)
				}
				if dgstPair.Descriptor.Annotations == nil {
					return errors.Errorf("invalid descriptor without annotations")
				}

				v, ok := dgstPair.Descriptor.Annotations[labels.LabelUncompressed]
				if !ok {
					return errors.Errorf("invalid descriptor without uncompressed annotation")
				}
				diffID, err := digest.Parse(v)
				if err != nil {
					return errors.Wrapf(err, "failed to parse uncompressed annotation")
				}

				// Check if entry already exists
				existing, err := e.client.Exists(groupCtx, blob)
				if err != nil {
					return errors.Wrapf(err, "failed to check cache existence")
				}

				if existing != nil {
					// Entry exists, touch it if needed
					if time.Since(existing.LastUsedAt) > e.config.TouchRefresh {
						if err := e.client.Touch(groupCtx, blob); err != nil {
							return errors.Wrapf(err, "failed to touch cache entry")
						}
					}
				} else {
					// Create new entry
					layerDone := progress.OneOff(groupCtx, fmt.Sprintf("exporting layer %s", blob))

					// First, upload the blob to the registry's blob store
					ra, err := dgstPair.Provider.ReaderAt(groupCtx, dgstPair.Descriptor)
					if err != nil {
						return layerDone(errors.Wrap(err, "error reading layer blob from provider"))
					}

					blobDigest, err := e.uploadBlob(groupCtx, ra, dgstPair.Descriptor.Size)
					ra.Close()
					if err != nil {
						return layerDone(errors.Wrap(err, "error uploading blob"))
					}

					// Collect parent digests
					var parents []digest.Digest
					if layer.ParentIndex >= 0 && layer.ParentIndex < len(cacheConfig.Layers) {
						parents = append(parents, cacheConfig.Layers[layer.ParentIndex].Blob)
					}

					// Create cache entry in the registry
					description := ""
					if v, ok := dgstPair.Descriptor.Annotations["buildkit/description"]; ok {
						description = v
					}

					createReq := &CreateRequest{
						Digest:      blob,
						BlobDigest:  blobDigest,
						CacheType:   "regular",
						Description: description,
						SizeBytes:   dgstPair.Descriptor.Size,
						Parents:     parents,
					}

					if _, err := e.client.Create(groupCtx, createReq); err != nil {
						return layerDone(errors.Wrap(err, "error creating cache entry"))
					}

					layerDone(nil)
				}

				// Update annotations with layer info
				cacheConfig.Layers[index].Annotations = &cacheimporttypes.LayerAnnotations{
					DiffID:    diffID,
					Size:      dgstPair.Descriptor.Size,
					MediaType: dgstPair.Descriptor.MediaType,
				}
				if v, ok := dgstPair.Descriptor.Annotations["buildkit/createdat"]; ok {
					var t time.Time
					if err := (&t).UnmarshalText([]byte(v)); err == nil {
						cacheConfig.Layers[index].Annotations.CreatedAt = t.UTC()
					}
				}
			}
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return nil, nil
}

// uploadBlob uploads a blob to the registry and returns its digest.
func (e *exporter) uploadBlob(ctx context.Context, ra io.ReaderAt, size int64) (digest.Digest, error) {
	// Read the entire blob
	data := make([]byte, size)
	if _, err := ra.ReadAt(data, 0); err != nil && err != io.EOF {
		return "", errors.Wrap(err, "failed to read blob data")
	}

	// Upload using the OCI distribution API
	return e.client.UploadBlob(ctx, data)
}
