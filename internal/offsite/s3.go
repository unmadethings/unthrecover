package offsite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/unmadethings/unthrecover/internal/config"
)

// S3 is an S3-compatible backend (AWS S3, Google Cloud Storage via HMAC keys,
// Cloudflare R2, Backblaze B2, Wasabi, Hetzner, MinIO, SeaweedFS, Garage).
type S3 struct {
	client *minio.Client
	bucket string
}

// partSize bounds memory per upload and allows objects up to 10,000 parts.
const partSize = 64 << 20

// NewS3 creates the backend. Keys are read from the configured environment
// variables and never logged.
func NewS3(o config.Offsite) (*S3, error) {
	ak, okA := os.LookupEnv(o.AccessKeyEnv)
	sk, okS := os.LookupEnv(o.SecretKeyEnv)
	if !okA || !okS || ak == "" || sk == "" {
		return nil, fmt.Errorf("offsite: environment variables %s and %s must be set", o.AccessKeyEnv, o.SecretKeyEnv)
	}
	u, err := url.Parse(o.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("offsite.endpoint: %w", err)
	}
	lookup := minio.BucketLookupAuto
	if o.PathStyle {
		lookup = minio.BucketLookupPath
	}
	c, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(ak, sk, ""),
		Secure:       u.Scheme == "https",
		Region:       o.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("offsite: %w", err)
	}
	return &S3{client: c, bucket: o.Bucket}, nil
}

// Target implements Backend.
func (s *S3) Target() string { return "s3://" + s.bucket }

// Check verifies that the bucket exists and is reachable with the keys.
func (s *S3) Check(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("offsite: cannot access bucket %q: %w", s.bucket, err)
	}
	if !ok {
		return fmt.Errorf("offsite: bucket %q does not exist (create it first; unth never creates buckets)", s.bucket)
	}
	return nil
}

// Put implements Backend.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream", PartSize: partSize, DisableContentSha256: false,
	})
	return err
}

// Get implements Backend.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, mapS3Err(err)
	}
	// GetObject is lazy: Stat surfaces "not found" before reading.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, mapS3Err(err)
	}
	return obj, nil
}

// List implements Backend.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	for o := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, o.Err
		}
		out = append(out, o.Key)
	}
	return out, nil
}

// Delete implements Backend.
func (s *S3) Delete(ctx context.Context, key string) error {
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if errors.Is(mapS3Err(err), ErrNotExist) {
		return nil
	}
	return err
}

func mapS3Err(err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || resp.StatusCode == 404 || strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("%w: %v", ErrNotExist, err)
	}
	return err
}
