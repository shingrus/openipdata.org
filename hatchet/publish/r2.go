// Package publish uploads build artifacts to Cloudflare R2 and to a rolling
// GitHub release, and pings an optional healthchecks.io check afterwards. It
// also downloads the files one workflow leaves in R2 for another.
package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// sha256Meta is the user metadata (sent as X-Amz-Meta-Sha256) that holds the
// hex SHA-256 of an uploaded object, so a later run can tell that the bytes it
// would send are already there.
const sha256Meta = "sha256"

// FileSHA256 returns the hex SHA-256 of the file at path.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ObjectInfo is what Stat reads from an object's headers.
type ObjectInfo struct {
	ETag         string // without quotes
	LastModified time.Time
	SHA256       string // the sha256 metadata Upload writes; empty for an object without it
}

// Stat reads the headers of the object at key, without its content. A missing
// object is ErrNotFound.
func (r *R2) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	location := fmt.Sprintf("r2://%s/%s", r.bucket, key)
	info, err := r.client.StatObject(ctx, r.bucket, key, minio.StatObjectOptions{})
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return ObjectInfo{}, fmt.Errorf("stat %s: %w", location, ErrNotFound)
	}
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("stat %s: %w", location, err)
	}
	out := ObjectInfo{ETag: info.ETag, LastModified: info.LastModified}
	for k, v := range info.UserMetadata {
		if strings.EqualFold(strings.TrimPrefix(strings.ToLower(k), "x-amz-meta-"), sha256Meta) {
			out.SHA256 = strings.ToLower(v)
		}
	}
	return out, nil
}

// Upload puts the local file at key, with its SHA-256 as metadata, and returns its
// r2://bucket/key location.
func (r *R2) Upload(ctx context.Context, key, path string) (string, error) {
	sum, err := FileSHA256(path)
	if err != nil {
		return "", err
	}
	return r.upload(ctx, key, path, sum)
}

func (r *R2) upload(ctx context.Context, key, path, sum string) (string, error) {
	// A fixed type: mime.TypeByExtension would depend on the host's mime.types.
	opts := minio.PutObjectOptions{ContentType: "application/octet-stream", UserMetadata: map[string]string{sha256Meta: sum}}
	location := fmt.Sprintf("r2://%s/%s", r.bucket, key)
	if _, err := r.client.FPutObject(ctx, r.bucket, key, path, opts); err != nil {
		return "", fmt.Errorf("upload %s to %s: %w", filepath.Base(path), location, err)
	}
	return location, nil
}

// PutStatus says what Put did.
type PutStatus string

const (
	PutUploaded  PutStatus = "uploaded"  // the bytes were sent
	PutUnchanged PutStatus = "unchanged" // the object already has this content; nothing was sent
	PutRefreshed PutStatus = "refreshed" // the same, and its modified time was renewed (no bytes sent)
)

// PutOptions tune Put.
type PutOptions struct {
	// Force sends the file even when the object already has this content.
	Force bool
	// Refresh renews the modified time of an object that already has this content:
	// readers that treat an old object as stale (build-geo-data does for the layers
	// of sync-geofeeds) must still see an unchanged file as fresh.
	Refresh bool
}

// Put makes the object at key hold the file, sending the bytes only when they are
// not there: the object's sha256 metadata (written by Upload) is compared with the
// file's. An object with no such metadata (uploaded by an older tool) is replaced.
// With the same content there, Refresh renews its modified time through a
// server-side copy onto itself; should that fail, the file is uploaded as usual.
func (r *R2) Put(ctx context.Context, key, path string, opts PutOptions) (string, PutStatus, error) {
	location := fmt.Sprintf("r2://%s/%s", r.bucket, key)
	sum, err := FileSHA256(path)
	if err != nil {
		return "", "", err
	}
	if !opts.Force {
		// A failed Stat is no reason to fail: the upload says what is wrong with R2.
		if info, err := r.Stat(ctx, key); err == nil && info.SHA256 == sum {
			if !opts.Refresh {
				return location, PutUnchanged, nil
			}
			if err := r.refresh(ctx, key, sum); err == nil {
				return location, PutRefreshed, nil
			}
		}
	}
	if _, err := r.upload(ctx, key, path, sum); err != nil {
		return "", "", err
	}
	return location, PutUploaded, nil
}

// refresh copies the object onto itself, replacing its metadata (which keeps
// the sha256 and the content type), so the object's modified time is now.
func (r *R2) refresh(ctx context.Context, key, sum string) error {
	dst := minio.CopyDestOptions{
		Bucket: r.bucket, Object: key, ReplaceMetadata: true,
		ContentType: "application/octet-stream", UserMetadata: map[string]string{sha256Meta: sum},
	}
	_, err := r.client.CopyObject(ctx, dst, minio.CopySrcOptions{Bucket: r.bucket, Object: key})
	return err
}

// Download saves the object at key to path (through a temporary file) and
// returns its upload time.
func (r *R2) Download(ctx context.Context, key, path string) (time.Time, error) {
	info, err := r.Stat(ctx, key)
	if err != nil {
		return time.Time{}, fmt.Errorf("download: %w", err)
	}
	if err := r.fetch(ctx, key, path); err != nil {
		return time.Time{}, err
	}
	return info.LastModified, nil
}

// DownloadIfChanged is Download for a caller that keeps the last copy: when the
// object's ETag is etag (a non-empty value the caller saved from an earlier call)
// nothing is downloaded and downloaded is false; the caller's copy at path is
// the object. Otherwise the object is saved to path as Download does. info says
// what the object is now (its ETag to save, its upload time) either way.
func (r *R2) DownloadIfChanged(ctx context.Context, key, path, etag string) (info ObjectInfo, downloaded bool, err error) {
	if info, err = r.Stat(ctx, key); err != nil {
		return ObjectInfo{}, false, fmt.Errorf("download: %w", err)
	}
	if etag != "" && info.ETag == etag {
		return info, false, nil
	}
	if err := r.fetch(ctx, key, path); err != nil {
		return ObjectInfo{}, false, err
	}
	return info, true, nil
}

// fetch saves the object at key to path through a temporary file.
func (r *R2) fetch(ctx context.Context, key, path string) error {
	location := fmt.Sprintf("r2://%s/%s", r.bucket, key)
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	if err := r.client.FGetObject(ctx, r.bucket, key, tmp, minio.GetObjectOptions{}); err != nil {
		return fmt.Errorf("download %s: %w", location, err)
	}
	return os.Rename(tmp, path)
}
