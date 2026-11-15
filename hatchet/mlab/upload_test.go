package mlab

import (
	"context"
	"errors"
	"os"
	"testing"

	"openipdata.org/hatchet/publish"
)

type putCall struct {
	key, path string
	opts      publish.PutOptions
}

func TestUpload(t *testing.T) {
	var calls []putCall
	status := publish.PutUnchanged
	var putErr error
	p := &Pipeline{DataDir: t.TempDir(), upload: func(ctx context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error) {
		calls = append(calls, putCall{key, path, opts})
		return "r2://bucket/" + key, status, putErr
	}}
	ctx := context.Background()
	if _, err := p.Upload(ctx, false); err == nil || len(calls) != 0 {
		t.Fatalf("no file: %v", err)
	}
	os.MkdirAll(p.dir(), 0o750)
	os.WriteFile(p.LayerPath(), []byte("12345"), 0o640)
	out, err := p.Upload(ctx, false)
	if err != nil || out.Status != publish.PutUnchanged || out.Bytes != 5 {
		t.Fatalf("%+v %v", out, err)
	}
	if len(calls) != 1 || calls[0].key != "internal/layers/36-mlab.csv.gz" || calls[0].path != p.LayerPath() || !calls[0].opts.Refresh || calls[0].opts.Force {
		t.Errorf("call = %+v", calls)
	}
	status = publish.PutUploaded
	if out, err = p.Upload(ctx, true); err != nil || out.Status != publish.PutUploaded || !calls[1].opts.Force {
		t.Errorf("force: %+v %v %+v", out, err, calls)
	}
	putErr = errors.New("boom")
	if _, err = p.Upload(ctx, false); err == nil {
		t.Error("a failed upload did not fail the task")
	}
}

func TestCheckUpload(t *testing.T) {
	if err := (&Pipeline{}).CheckUpload(); err == nil {
		t.Error("an empty R2 setting is accepted")
	}
}
