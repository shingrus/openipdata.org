package bgp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

func TestDownload(t *testing.T) {
	var requests atomic.Int32
	var conditional atomic.Int32
	body := "dump bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 15, 3, 40, 0, 0, time.UTC)
	p := &Pipeline{DataDir: t.TempDir(), url: srv.URL, client: srv.Client(), now: func() time.Time { return now }}
	ctx := context.Background()

	out, err := p.Download(ctx)
	if err != nil || out.Status != "downloaded" || out.Bytes != int64(len(body)) {
		t.Fatalf("%+v %v", out, err)
	}
	if data, _ := os.ReadFile(p.MRTPath()); string(data) != body {
		t.Errorf("file = %q", data)
	}
	// Within the age gate nobody is asked.
	now = now.Add(5 * time.Hour)
	if out, err = p.Download(ctx); err != nil || out.Status != "recent" || requests.Load() != 1 {
		t.Errorf("gate: %+v %v %d", out, err, requests.Load())
	}
	// After it the server is asked, with the saved ETag: 304 keeps the file.
	now = now.Add(2 * time.Hour)
	if out, err = p.Download(ctx); err != nil || out.Status != "cached" || requests.Load() != 2 || conditional.Load() != 1 {
		t.Errorf("after the gate: %+v %v %d", out, err, requests.Load())
	}
	// The check was recorded: the gate holds again.
	if out, _ = p.Download(ctx); out.Status != "recent" {
		t.Errorf("gate again: %+v", out)
	}
	// Only one dump is kept.
	entries, _ := os.ReadDir(p.dir())
	for _, e := range entries {
		if e.Name() != mrtFile && e.Name() != mrtFile+checkedSuffix && e.Name() != mrtFile+".etag" {
			t.Errorf("extra file %s", e.Name())
		}
	}
}

func TestDownloadErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	p := &Pipeline{DataDir: t.TempDir(), url: srv.URL, client: srv.Client()}
	if _, err := p.Download(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %v", err)
	}
	if _, err := os.Stat(p.MRTPath()); err == nil {
		t.Error("a file after a failed download")
	}
	if _, err := os.Stat(p.MRTPath() + checkedSuffix); err == nil {
		t.Error("a failed download counts as checked")
	}
}

// A body over the cap fails, whether the server says so or not.
func TestDownloadCap(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !chunked {
				w.Header().Set("Content-Length", "100")
			}
			w.Write(make([]byte, 100))
		}))
		client := &http.Client{Transport: &capTransport{rt: http.DefaultTransport, max: 50}}
		p := &Pipeline{DataDir: t.TempDir(), url: srv.URL, client: client}
		if _, err := p.Download(context.Background()); err == nil || !strings.Contains(err.Error(), "download limit") {
			t.Errorf("chunked=%v: %v", chunked, err)
		}
		if _, err := os.Stat(p.MRTPath()); err == nil {
			t.Error("file kept")
		}
		srv.Close()
	}
	// Exactly the cap is fine.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 50)) }))
	defer srv.Close()
	p := &Pipeline{DataDir: t.TempDir(), url: srv.URL, client: &http.Client{Transport: &capTransport{rt: http.DefaultTransport, max: 50}}}
	if out, err := p.Download(context.Background()); err != nil || out.Bytes != 50 {
		t.Errorf("at the cap: %+v %v", out, err)
	}
}

type putCall struct {
	key, path string
	opts      publish.PutOptions
}

func TestUpload(t *testing.T) {
	var pings atomic.Int32
	hc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { pings.Add(1) }))
	defer hc.Close()
	var calls []putCall
	status := publish.PutUnchanged
	var putErr error
	p := &Pipeline{DataDir: t.TempDir(), HCPing: hc.URL, upload: func(ctx context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error) {
		calls = append(calls, putCall{key, path, opts})
		return "r2://bucket/" + key, status, putErr
	}}
	ctx := context.Background()
	// Nothing to upload yet.
	if _, err := p.Upload(ctx, false); err == nil || len(calls) != 0 || pings.Load() != 0 {
		t.Fatalf("no file: %v", err)
	}
	os.MkdirAll(p.dir(), 0o750)
	os.WriteFile(p.OutPath(), []byte("12345"), 0o640)

	// An unchanged object is renewed, not sent: the same status comes back, and the ping goes out.
	out, err := p.Upload(ctx, false)
	if err != nil || out.Status != publish.PutUnchanged || out.Bytes != 5 || pings.Load() != 1 {
		t.Fatalf("%+v %v %d", out, err, pings.Load())
	}
	if len(calls) != 1 || calls[0].key != "internal/bgp/communities.csv.gz" || calls[0].path != p.OutPath() || !calls[0].opts.Refresh || calls[0].opts.Force {
		t.Errorf("call = %+v", calls)
	}
	// force_upload is passed on.
	status = publish.PutUploaded
	if out, err = p.Upload(ctx, true); err != nil || out.Status != publish.PutUploaded || !calls[1].opts.Force {
		t.Errorf("force: %+v %v %+v", out, err, calls)
	}
	// A failed upload fails the task and sends no ping.
	putErr = errors.New("boom")
	before := pings.Load()
	if _, err = p.Upload(ctx, false); err == nil || pings.Load() != before {
		t.Errorf("failed upload: %v pings %d", err, pings.Load()-before)
	}
	// A failing ping does not fail the upload.
	putErr = nil
	hc.Close()
	if _, err = p.Upload(ctx, false); err != nil {
		t.Errorf("failed ping: %v", err)
	}
}

func TestCheckUpload(t *testing.T) {
	if err := (&Pipeline{}).CheckUpload(); err == nil {
		t.Error("an empty R2 setting is accepted")
	}
}

func TestExtractGate(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	os.MkdirAll(p.dir(), 0o750)
	gzipTo(t, p.MRTPath(), dump(10, func(int) []uint32 { return nil }))
	// The default minimum is a real table's size.
	if _, err := p.Extract(context.Background(), ""); !errors.Is(err, ErrTooFewPrefixes) {
		t.Errorf("default gate: %v", err)
	}
	p.minPrefixes = 5
	out, err := p.Extract(context.Background(), "")
	if err != nil || out.Prefixes != 10 {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(p.dir(), OutFile)); err != nil {
		t.Error(err)
	}
	// A local file can be named.
	other := filepath.Join(t.TempDir(), "x.gz")
	gzipTo(t, other, dump(7, func(int) []uint32 { return nil }))
	if out, err = p.Extract(context.Background(), other); err != nil || out.Prefixes != 7 {
		t.Errorf("local file: %+v %v", out, err)
	}
}
