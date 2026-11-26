package publish

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
)

// ErrPresign replaces every presign failure (the text could repeat parts of the setting).
var ErrPresign = errors.New("could not make a presigned URL")

// PresignGet returns a URL that lets its holder download the object at key until expiry has
// passed. The URL carries a signature: callers must never log it.
func (r *R2) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	u, err := r.client.PresignedGetObject(ctx, r.bucket, key, expiry, nil)
	if err != nil {
		return "", ErrPresign
	}
	return u.String(), nil
}

// PresignPut returns a URL that lets its holder upload the object at key until expiry has
// passed. The URL carries a signature: callers must never log it.
func (r *R2) PresignPut(ctx context.Context, key string, expiry time.Duration) (string, error) {
	u, err := r.client.PresignedPutObject(ctx, r.bucket, key, expiry)
	if err != nil {
		return "", ErrPresign
	}
	return u.String(), nil
}

// Delete removes the object at key; a missing object is not an error.
func (r *R2) Delete(ctx context.Context, key string) error {
	if err := r.client.RemoveObject(ctx, r.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete r2://%s/%s: %w", r.bucket, key, err)
	}
	return nil
}

// DeletePrefix removes every object whose key starts with prefix and returns how many it removed.
func (r *R2) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	var keys []string
	for obj := range r.client.ListObjects(ctx, r.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return 0, fmt.Errorf("list r2://%s/%s: %w", r.bucket, prefix, obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	n := 0
	for _, k := range keys {
		if err := r.Delete(ctx, k); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
