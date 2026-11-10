// Package publish uploads build artifacts to Cloudflare R2 and to a rolling
// GitHub release, and pings an optional healthchecks.io check afterwards. It
// also downloads the files one workflow leaves in R2 for another.
package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrInvalidR2 replaces every problem with the R2 setting. The value holds the
// secret key, so errors never repeat it (Hatchet stores returned errors).
var ErrInvalidR2 = errors.New("invalid R2 configuration (want https://<key-id>:<secret>@<account-id>.r2.cloudflarestorage.com/<bucket>)")

// ErrNotFound means Download found no object at the key.
var ErrNotFound = errors.New("object not found")

// R2 uploads to and downloads from one Cloudflare R2 bucket through its S3-compatible API.
type R2 struct {
	client *minio.Client
	bucket string
}

type r2Config struct {
	endpoint, bucket, keyID, secret string
}

// parseR2 reads https://<key-id>:<secret>@<account-id>.r2.cloudflarestorage.com/<bucket>.
func parseR2(raw string) (r2Config, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User == nil || u.Host == "" {
		return r2Config{}, ErrInvalidR2
	}
	secret, _ := u.User.Password()
	bucket := strings.Trim(u.Path, "/")
	cfg := r2Config{endpoint: u.Host, bucket: bucket, keyID: u.User.Username(), secret: secret}
	if cfg.keyID == "" || cfg.secret == "" || bucket == "" || strings.Contains(bucket, "/") ||
		u.RawQuery != "" || u.Fragment != "" {
		return r2Config{}, ErrInvalidR2
	}
	return cfg, nil
}

// NewR2 connects to the bucket named in the R2 setting.
func NewR2(raw string) (*R2, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("R2 is not set")
	}
	return newR2(raw, nil)
}

// newR2 takes an optional transport for tests.
func newR2(raw string, transport http.RoundTripper) (*R2, error) {
	cfg, err := parseR2(raw)
	if err != nil {
		return nil, err
	}
	client, err := minio.New(cfg.endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.keyID, cfg.secret, ""),
		Secure:    true,
		Region:    "auto",
		Transport: transport,
	})
	if err != nil {
		return nil, ErrInvalidR2
	}
	return &R2{client: client, bucket: cfg.bucket}, nil
}

// Upload puts the local file at key and returns its r2://bucket/key location.
func (r *R2) Upload(ctx context.Context, key, path string) (string, error) {
	// A fixed type: mime.TypeByExtension would depend on the host's mime.types.
	opts := minio.PutObjectOptions{ContentType: "application/octet-stream"}
	location := fmt.Sprintf("r2://%s/%s", r.bucket, key)
	if _, err := r.client.FPutObject(ctx, r.bucket, key, path, opts); err != nil {
		return "", fmt.Errorf("upload %s to %s: %w", filepath.Base(path), location, err)
	}
	return location, nil
}

// Download saves the object at key to path (through a temporary file) and
// returns its upload time.
func (r *R2) Download(ctx context.Context, key, path string) (time.Time, error) {
	location := fmt.Sprintf("r2://%s/%s", r.bucket, key)
	info, err := r.client.StatObject(ctx, r.bucket, key, minio.StatObjectOptions{})
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return time.Time{}, fmt.Errorf("download %s: %w", location, ErrNotFound)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("download %s: %w", location, err)
	}
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	if err := r.client.FGetObject(ctx, r.bucket, key, tmp, minio.GetObjectOptions{}); err != nil {
		return time.Time{}, fmt.Errorf("download %s: %w", location, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return time.Time{}, err
	}
	return info.LastModified, nil
}
