package bgp

import (
	"bytes"
	"compress/gzip"
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

// gz is data as a gzip file.
func gz(data []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func TestDownload(t *testing.T) {
	var requests atomic.Int32
	var conditional atomic.Int32
	body := string(gz([]byte("dump bytes")))
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
	body := gz(make([]byte, 50))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	p := &Pipeline{DataDir: t.TempDir(), url: srv.URL, client: &http.Client{Transport: &capTransport{rt: http.DefaultTransport, max: int64(len(body))}}}
	if out, err := p.Download(context.Background()); err != nil || out.Bytes != int64(len(body)) {
		t.Errorf("at the cap: %+v %v", out, err)
	}
}

// noise is bytes that do not compress away, so a gzip of it has a long data part.
func noise(n int) []byte {
	out := make([]byte, n)
	x := uint32(1)
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = byte(x >> 24)
	}
	return out
}

// The three ways a dump arrives broken: a wrong byte in the middle (what RIPE's file
// had on the VM), a file that ends early, and something that is no gzip at all.
func brokenDumps() map[string][]byte {
	good := gz(noise(1 << 16))
	flipped := bytes.Clone(good)
	flipped[len(flipped)/2] ^= 0xff
	return map[string][]byte{"wrong byte": flipped, "cut short": good[:len(good)-20], "no gzip": []byte("<html>busy</html>")}
}

func TestCheckGzip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.gz")
	os.WriteFile(path, gz(noise(1<<16)), 0o640)
	if err := checkGzip(context.Background(), path); err != nil {
		t.Errorf("good file: %v", err)
	}
	for name, data := range brokenDumps() {
		os.WriteFile(path, data, 0o640)
		if err := checkGzip(context.Background(), path); !errors.Is(err, ErrDamaged) {
			t.Errorf("%s: %v", name, err)
		}
	}
	os.WriteFile(path, nil, 0o640)
	if err := checkGzip(context.Background(), path); !errors.Is(err, ErrDamaged) {
		t.Errorf("empty file: %v", err)
	}
	// A cancelled check is not damage, and a missing file is its own error.
	os.WriteFile(path, gz(noise(1<<16)), 0o640)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checkGzip(ctx, path); !errors.Is(err, context.Canceled) || errors.Is(err, ErrDamaged) {
		t.Errorf("cancelled: %v", err)
	}
	if err := checkGzip(context.Background(), path+".missing"); !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrDamaged) {
		t.Errorf("missing: %v", err)
	}
}

// A dump that arrives damaged is fetched once more; two damaged ones fail the task and
// leave nothing behind, so the next run asks for the whole file again.
func TestDownloadDamaged(t *testing.T) {
	good := gz(noise(1 << 16))
	for name, bad := range brokenDumps() {
		var requests atomic.Int32
		badAnswers := int32(1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
				t.Errorf("%s: a conditional request after a damaged file", name)
			}
			w.Header().Set("ETag", `"v1"`)
			if requests.Add(1) <= badAnswers {
				w.Write(bad)
				return
			}
			w.Write(good)
		}))
		p := &Pipeline{DataDir: t.TempDir(), url: srv.URL, client: srv.Client()}
		var logged []string
		p.Progress = func(s string) { logged = append(logged, s) }

		// Damaged once: the second fetch is kept.
		out, err := p.Download(context.Background())
		if err != nil || out.Status != "downloaded" || requests.Load() != 2 || out.Bytes != int64(len(good)) {
			t.Errorf("%s: %+v %v, %d requests", name, out, err, requests.Load())
		}
		if data, _ := os.ReadFile(p.MRTPath()); !bytes.Equal(data, good) {
			t.Errorf("%s: the kept file is not the good one", name)
		}
		if _, err := os.Stat(p.MRTPath() + checkedSuffix); err != nil {
			t.Errorf("%s: no checked stamp after a good fetch", name)
		}
		if len(logged) == 0 || !strings.Contains(logged[0], "arrived damaged") {
			t.Errorf("%s: log = %q", name, logged)
		}

		// Damaged twice: an error, and no dump, ETag or stamp is left.
		p = &Pipeline{DataDir: t.TempDir(), url: srv.URL, client: srv.Client()}
		requests.Store(0)
		badAnswers = 2
		if _, err := p.Download(context.Background()); !errors.Is(err, ErrDamaged) || requests.Load() != 2 {
			t.Errorf("%s twice: %v, %d requests", name, err, requests.Load())
		}
		if entries, _ := os.ReadDir(p.dir()); len(entries) != 0 {
			t.Errorf("%s twice: %d files left", name, len(entries))
		}
		srv.Close()
	}
}

// Extract deletes Download's dump when it is damaged (with its ETag and stamp, so the age
// gate cannot keep it), and never a file the caller named.
func TestExtractDamagedDump(t *testing.T) {
	for name, bad := range brokenDumps() {
		p := &Pipeline{DataDir: t.TempDir(), minPrefixes: 1}
		os.MkdirAll(p.dir(), 0o750)
		os.WriteFile(p.MRTPath(), bad, 0o640)
		os.WriteFile(p.MRTPath()+".etag", []byte(`"v1"`), 0o640)
		os.WriteFile(p.MRTPath()+checkedSuffix, nil, 0o640)
		if _, err := p.Extract(context.Background(), ""); !errors.Is(err, ErrDamaged) {
			t.Errorf("%s: %v", name, err)
		}
		if entries, _ := os.ReadDir(p.dir()); len(entries) != 0 {
			t.Errorf("%s: %d files left", name, len(entries))
		}
		other := filepath.Join(t.TempDir(), "x.gz")
		os.WriteFile(other, bad, 0o640)
		if _, err := p.Extract(context.Background(), other); !errors.Is(err, ErrDamaged) {
			t.Errorf("%s, named file: %v", name, err)
		}
		if _, err := os.Stat(other); err != nil {
			t.Errorf("%s: the named file was deleted", name)
		}
	}
	// A good gzip with a broken table inside is damage too.
	p := &Pipeline{DataDir: t.TempDir(), minPrefixes: 1}
	os.MkdirAll(p.dir(), 0o750)
	gzipTo(t, p.MRTPath(), dump(10, func(int) []uint32 { return nil })[:200])
	if _, err := p.Extract(context.Background(), ""); !errors.Is(err, ErrDamaged) {
		t.Errorf("broken table: %v", err)
	}
	if _, err := os.Stat(p.MRTPath()); err == nil {
		t.Error("broken table: the dump was kept")
	}
	// A dump that is only too small is kept: it is not damaged.
	p = &Pipeline{DataDir: t.TempDir()}
	os.MkdirAll(p.dir(), 0o750)
	gzipTo(t, p.MRTPath(), dump(10, func(int) []uint32 { return nil }))
	if _, err := p.Extract(context.Background(), ""); !errors.Is(err, ErrTooFewPrefixes) {
		t.Errorf("small table: %v", err)
	}
	if _, err := os.Stat(p.MRTPath()); err != nil {
		t.Error("small table: the dump was deleted")
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
