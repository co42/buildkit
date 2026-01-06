package registryv2

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// Client is an HTTP client for the registry v2 BuildKit cache API.
type Client struct {
	baseURL    string
	httpClient *http.Client
	token      string
	username   string
	password   string
}

// NewClient creates a new registry v2 cache client.
func NewClient(config Config) (*Client, error) {
	baseURL := strings.TrimSuffix(config.RegistryURL, "/")

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: config.Insecure,
		},
	}

	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		token:    config.Token,
		username: config.Username,
		password: config.Password,
	}, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	u := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else if c.username != "" && c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, nil
}

// Exists checks if a cache entry exists and returns its metadata.
// Returns nil, nil if the entry does not exist.
func (c *Client) Exists(ctx context.Context, dgst digest.Digest) (*CacheEntry, error) {
	path := fmt.Sprintf("/v2/buildkit/cache/blobs/%s", dgst.String())
	req, err := c.newRequest(ctx, http.MethodHead, path, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to check cache existence")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Parse headers into CacheEntry
	entry := &CacheEntry{
		Digest: dgst,
	}

	if v := resp.Header.Get("X-Cache-Blob-Digest"); v != "" {
		entry.BlobDigest = digest.Digest(v)
	}
	if v := resp.Header.Get("X-Cache-Type"); v != "" {
		entry.CacheType = v
	}
	if v := resp.Header.Get("X-Cache-Size"); v != "" {
		size, _ := strconv.ParseInt(v, 10, 64)
		entry.SizeBytes = size
	}
	if v := resp.Header.Get("X-Cache-Created-At"); v != "" {
		t, _ := time.Parse(time.RFC3339, v)
		entry.CreatedAt = t
	}
	if v := resp.Header.Get("X-Cache-Last-Used-At"); v != "" {
		t, _ := time.Parse(time.RFC3339, v)
		entry.LastUsedAt = t
	}

	return entry, nil
}

// Get retrieves the full cache entry metadata.
func (c *Client) Get(ctx context.Context, dgst digest.Digest) (*CacheEntry, error) {
	path := fmt.Sprintf("/v2/buildkit/cache/blobs/%s", dgst.String())
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cache entry")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(body))
	}

	var entry CacheEntry
	if err := json.NewDecoder(resp.Body).Decode(&entry); err != nil {
		return nil, errors.Wrap(err, "failed to decode cache entry")
	}

	return &entry, nil
}

// CreateRequest is the request body for creating a cache entry.
type CreateRequest struct {
	Digest      digest.Digest   `json:"digest"`
	BlobDigest  digest.Digest   `json:"blob_digest"`
	CacheType   string          `json:"type"`
	Description string          `json:"description,omitempty"`
	SizeBytes   int64           `json:"size"`
	Parents     []digest.Digest `json:"parents,omitempty"`
}

// Create creates a new cache entry.
func (c *Client) Create(ctx context.Context, req *CreateRequest) (*CacheEntry, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v2/buildkit/cache/blobs", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create cache entry")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to create cache entry: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	var entry CacheEntry
	if err := json.NewDecoder(resp.Body).Decode(&entry); err != nil {
		return nil, errors.Wrap(err, "failed to decode created cache entry")
	}

	return &entry, nil
}

// Touch updates the last_used_at timestamp for a cache entry.
func (c *Client) Touch(ctx context.Context, dgst digest.Digest) error {
	path := fmt.Sprintf("/v2/buildkit/cache/blobs/%s", dgst.String())

	body := []byte(fmt.Sprintf(`{"last_used_at": "%s"}`, time.Now().UTC().Format(time.RFC3339)))
	req, err := c.newRequest(ctx, http.MethodPatch, path, bytes.NewReader(body))
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to touch cache entry")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to touch cache entry: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// QueryByParent queries cache entries by parent digest.
func (c *Client) QueryByParent(ctx context.Context, parentDigest digest.Digest) ([]CacheEntry, error) {
	path := fmt.Sprintf("/v2/buildkit/cache/query?parent=%s", url.QueryEscape(parentDigest.String()))
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to query cache entries")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to query cache entries: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Entries []CacheEntry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, errors.Wrap(err, "failed to decode query result")
	}

	return result.Entries, nil
}

// GetMount retrieves a cache mount by ID.
func (c *Client) GetMount(ctx context.Context, mountID string) (*CacheMount, error) {
	path := fmt.Sprintf("/v2/buildkit/cache/mounts/%s", url.PathEscape(mountID))
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get mount")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get mount: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	var mount CacheMount
	if err := json.NewDecoder(resp.Body).Decode(&mount); err != nil {
		return nil, errors.Wrap(err, "failed to decode mount")
	}

	return &mount, nil
}

// SetMount creates or updates a cache mount.
func (c *Client) SetMount(ctx context.Context, mount *CacheMount) error {
	path := fmt.Sprintf("/v2/buildkit/cache/mounts/%s", url.PathEscape(mount.MountID))

	body, err := json.Marshal(mount)
	if err != nil {
		return err
	}

	req, err := c.newRequest(ctx, http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to set mount")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to set mount: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// ReaderAt implements content.Provider for blob access.
// This allows the importer to read blob content from the registry.
func (c *Client) ReaderAt(ctx context.Context, desc ocispecs.Descriptor) (content.ReaderAt, error) {
	readerAtCloser := toReaderAtCloser(func(offset int64) (io.ReadCloser, error) {
		return c.getBlobReader(ctx, desc.Digest, offset)
	})
	return &readerAt{ReaderAtCloser: readerAtCloser, size: desc.Size}, nil
}

// getBlobReader returns a reader for a blob starting at the given offset.
func (c *Client) getBlobReader(ctx context.Context, dgst digest.Digest, offset int64) (io.ReadCloser, error) {
	// The blob is stored in the registry's blob store, accessible via the standard v2 API
	// We need to get the blob_digest from the cache entry first, then fetch the actual blob
	entry, err := c.Get(ctx, dgst)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, fmt.Errorf("cache entry not found: %s", dgst)
	}

	// Fetch the actual blob using the blob_digest
	// The blob is stored in the registry's standard blob store
	path := fmt.Sprintf("/v2/_buildkit_cache/blobs/%s", entry.BlobDigest.String())
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get blob")
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("failed to get blob: status %d", resp.StatusCode)
	}

	return resp.Body, nil
}

type readerAt struct {
	ReaderAtCloser
	size int64
}

func (r *readerAt) Size() int64 {
	return r.size
}
