package push

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/go-units"
	"github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"io"
	"net/http"
	"strings"
	"time"

	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// Purpose of this code is to avoid pushing layers to a docker registry
// The current implementation of the docker registry [distribution](https://github.com/distribution/distribution)
// is broken for object storage backend
// The implementation work in two ways:
// - pushing to an /_upload directory
// - moving to the right directory in /blobs/digest/data
// As this suit for filesystem, this is not working in object storage as the `move` function is a `copy` + `delete`
// resulting in a first upload, then a re-download + re-upload for the copy.
// We could fix the registry by directly uploading to the right place,
// but we also need to modify buildkit to add the digest on the first command
//
// Proposed solution is to by-pass the registry to upload layers and push them directly to S3
// File architecture is as follow
//		<root>/v2
//			-> repositories/
// 				-><name>/
// 					-> _manifests/
// 						revisions
//							-> <manifest digest path>
//								-> link
// 						tags/<tag>
//							-> current/link
// 							-> index
//								-> <algorithm>/<hex digest>/link
// 					-> _layers/
// 						<layer links to blob store>
//			-> blob/<algorithm>
//				<split directory content addressable storage>
// We will only focus on layers, we delegate the manifest push to the registry
// as it's more complicated and don't involve any high throughput
// So, in the end will only push directly to `blob/<algorithm>`
// And create the `_layers` link to the `blob`

var s3Client s3ClientHolder
var enabled = false

type s3ClientHolder struct {
	*s3.Client
	*manager.Uploader
	bucket string
}

func init() {
	enabledEnv := os.Getenv("AWS_S3_DIRECT_PUSH_ENABLED")
	if enabledEnv != "true" {
		return
	}
	enabled = true

	key := os.Getenv("AWS_ACCESS_KEY_ID")
	secret := os.Getenv("AWS_SECRET_ACCESS_KEY")
	bucket := os.Getenv("AWS_S3_BUCKET")
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	if key == "" || secret == "" || bucket == "" {
		panic("missing env values for S3 direct push")
	}
	client := s3.NewFromConfig(aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(key, secret, ""),
	})
	s3Client = s3ClientHolder{
		Client: client,
		Uploader: manager.NewUploader(client, func(uploader *manager.Uploader) {
			uploader.Concurrency = 50
			uploader.PartSize = 1024 * 1024 * 20
		}),
		bucket: bucket,
	}
}

type S3Pusher struct {
	ref          string
	s3Client     s3ClientHolder
	remotePusher remotes.Pusher
	logger       *logrus.Entry
}

type S3Writer struct {
	pusher       *S3Pusher
	offset       int64
	total        int64
	expected     digest.Digest
	startedAt    time.Time
	updatedAt    time.Time
	descriptor   ocispecs.Descriptor
	writer       *io.PipeWriter
	errChan      chan error
	finishedChan chan *manager.UploadOutput
}

func (p *S3Pusher) Push(ctx context.Context, descriptor ocispecs.Descriptor) (content.Writer, error) {
	logger := p.logger.WithField("digest", descriptor.Digest.Hex())

	switch descriptor.MediaType {
	case images.MediaTypeDockerSchema2Manifest, images.MediaTypeDockerSchema2ManifestList,
		ocispecs.MediaTypeImageManifest, ocispecs.MediaTypeImageIndex:
		logger.Debug("Skipping manifests")
		return p.remotePusher.Push(ctx, descriptor)
	}

	path, err := blobPath(descriptor.Digest)
	if err != nil {
		return nil, err
	}

	head, err := p.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(p.s3Client.bucket),
		Key:    aws.String(path),
	})

	logger.Debugf("Checked if file exist %s, result: %+v, err: %+v", path, head, err)

	// If no err on head, object exist
	if err == nil && head.ContentLength != nil && *head.ContentLength == descriptor.Size {
		logger.Debugf("File exist, creating link")
		if err := p.createLink(ctx, p.ref, descriptor.Digest); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("content %v already on s3: %w", descriptor.Digest, cerrdefs.ErrAlreadyExists)
	}
	if err != nil {
		var responseError *awshttp.ResponseError
		if errors.As(err, &responseError) && responseError.ResponseError.HTTPStatusCode() != http.StatusNotFound {
			return nil, err
		}
	}

	reader, writer := io.Pipe()
	errChan := make(chan error, 0)
	finishedChan := make(chan *manager.UploadOutput, 0)

	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		upload, err := p.s3Client.Upload(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(p.s3Client.bucket),
			Key:         aws.String(path),
			Body:        reader,
			ContentType: aws.String(descriptor.MediaType),
		})
		if err != nil {
			errChan <- err
			return err
		}
		finishedChan <- upload
		return nil
	})

	return &S3Writer{
		offset:       0,
		total:        0,
		writer:       writer,
		errChan:      errChan,
		finishedChan: finishedChan,
		startedAt:    time.Now(),
		updatedAt:    time.Now(),
		descriptor:   descriptor,
		pusher:       p,
	}, nil
}

func (p *S3Pusher) createLink(ctx context.Context, ref string, digest digest.Digest) error {
	start := time.Now()

	logger := p.logger.WithField("digest", digest.Hex())
	logger.Debugf("Create the link object")

	n := strings.SplitN(ref, "/", 2)
	if len(n) != 2 {
		return fmt.Errorf("error in ref %s", ref)
	}

	n = strings.SplitN(n[1], ":", 2)
	if len(n) != 2 {
		return fmt.Errorf("error in ref %s", ref)
	}

	s, err := linkPath(n[0], digest)
	if err != nil {
		return err
	}

	// Should we head to check if exist ?
	_, err = p.s3Client.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(p.s3Client.bucket),
		Key:    aws.String(s),
		Body:   bytes.NewReader([]byte(digest.String())),
	})

	if err == nil {
		logger.Debugf("Link created in %s", time.Since(start))
	} else {
		logger.WithError(err).Debugf("Error creating link %s", s)
	}
	return err
}

func (w *S3Writer) Status() (content.Status, error) {
	return content.Status{
		Ref:       w.pusher.ref,
		Offset:    w.offset,
		Total:     w.total,
		StartedAt: w.startedAt,
		UpdatedAt: w.updatedAt,
	}, nil
}

func (w *S3Writer) Digest() digest.Digest {
	return w.expected
}

func (w *S3Writer) Write(p []byte) (n int, err error) {
	n, err = w.writer.Write(p)
	w.offset += int64(len(p))
	w.updatedAt = time.Now()
	return n, err
}

func (w *S3Writer) Commit(ctx context.Context, size int64, expected digest.Digest, opts ...content.Opt) error {
	defer func() {
		close(w.finishedChan)
		close(w.errChan)
	}()

	w.expected = expected
	logger := w.pusher.logger.WithField("digest", expected.Hex())
	logger.Debug("Committing")

	if err := w.writer.Close(); err != nil {
		return err
	}

	select {
	case _ = <-w.finishedChan:
		break
	case err := <-w.errChan:
		return err
	}

	logger.Debugf("Push done in %s for %s",
		time.Since(w.startedAt),
		units.HumanSize(float64(size)))

	return w.pusher.createLink(ctx, w.pusher.ref, expected)
}

func (w *S3Writer) Close() (err error) {
	// Check whether read has already thrown an error
	if _, err := w.writer.Write([]byte{}); err != nil && err != io.ErrClosedPipe {
		return fmt.Errorf("pipe error before commit: %w", err)
	}
	return w.writer.Close()
}

func (w *S3Writer) Truncate(size int64) error {
	return errors.New("cannot truncate remote upload")
}

var blobAlgorithmReplacer = strings.NewReplacer(
	"+", "/",
	".", "/",
	";", "/",
)

func blobPath(dgst digest.Digest) (string, error) {
	if err := dgst.Validate(); err != nil {
		return "", err
	}
	algorithm := blobAlgorithmReplacer.Replace(string(dgst.Algorithm()))
	hex := dgst.Encoded()
	return fmt.Sprintf("docker/registry/v2/blobs/%s/%s/%s/data", algorithm, hex[:2], hex), nil
}

func linkPath(ref string, dgst digest.Digest) (string, error) {
	if err := dgst.Validate(); err != nil {
		return "", err
	}
	algorithm := blobAlgorithmReplacer.Replace(string(dgst.Algorithm()))
	return fmt.Sprintf("docker/registry/v2/repositories/%s/_layers/%s/%s/link", ref, algorithm, dgst.Encoded()), nil
}
