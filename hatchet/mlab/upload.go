package mlab

import (
	"context"
	"fmt"
	"os"
	"time"

	"openipdata.org/hatchet/publish"
)

// uploadFunc is publish.R2.Put. Tests replace it.
type uploadFunc func(ctx context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error)

// CheckUpload fails when the R2 setting is missing or invalid, so a run stops before the work.
func (p *Pipeline) CheckUpload() error {
	_, err := publish.NewR2(p.R2)
	return err
}

// UploadOutput is the upload task's result.
type UploadOutput struct {
	Status          publish.PutStatus `json:"status"` // uploaded, unchanged or refreshed
	Bytes           int64             `json:"bytes"`
	DurationSeconds float64           `json:"duration_seconds"`
}

// Upload puts the layer file at LayerKey with its sha256 in the object's metadata, as sync-bgp and
// sync-geofeeds do: an object that already holds the same bytes is not sent again, and a
// server-side copy renews its modified time. force always sends the bytes. No health check is
// pinged: this workflow is started by hand.
func (p *Pipeline) Upload(ctx context.Context, force bool) (UploadOutput, error) {
	started := time.Now()
	var out UploadOutput
	fi, err := os.Stat(p.LayerPath())
	if err != nil {
		return out, fmt.Errorf("no %s (has locate run?): %w", LayerFile, err)
	}
	put := p.upload
	if put == nil {
		r2, err := publish.NewR2(p.R2)
		if err != nil {
			return out, err
		}
		put = r2.Put
	}
	_, status, err := put(ctx, LayerKey, p.LayerPath(), publish.PutOptions{Force: force, Refresh: true})
	if err != nil {
		return out, err
	}
	out.Status, out.Bytes = status, fi.Size()
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("upload: %s (%d bytes)", status, out.Bytes)
	return out, nil
}
