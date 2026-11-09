package download

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileLastModified(t *testing.T) {
	modified := time.Date(2026, 11, 8, 23, 20, 43, 0, time.UTC)
	requests := 0
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("User-Agent") != UserAgent {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		if fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if since, err := http.ParseTime(r.Header.Get("If-Modified-Since")); err == nil && !modified.After(since) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		w.Write([]byte("data"))
	}))
	defer server.Close()

	client := server.Client()
	path := filepath.Join(t.TempDir(), "dump.gz")
	ctx := context.Background()
	if status, err := File(ctx, client, server.URL, path); err != nil || status != Downloaded {
		t.Fatalf("first fetch: %s %v", status, err)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(modified) {
		t.Fatalf("mtime not set to Last-Modified: %v %v", info, err)
	}
	if _, err := os.Stat(path + ".etag"); err == nil {
		t.Error("an .etag file without an ETag")
	}
	if status, err := File(ctx, client, server.URL, path); err != nil || status != Cached {
		t.Fatalf("second fetch: %s %v", status, err)
	}
	fail = true
	if _, err := File(ctx, client, server.URL, path); err == nil {
		t.Fatal("want an error from a failing server")
	}
	if data, _ := os.ReadFile(path); string(data) != "data" {
		t.Errorf("a failed fetch changed the file: %q", data)
	}
	if requests != 3 {
		t.Errorf("requests = %d", requests)
	}
}

func TestFileETag(t *testing.T) {
	version := "v1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"`+version+`"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"`+version+`"`)
		w.Write([]byte(version))
	}))
	defer server.Close()
	ctx, client := context.Background(), server.Client()
	path := filepath.Join(t.TempDir(), "file")

	for _, step := range []struct{ version, status string }{{"v1", Downloaded}, {"v1", Cached}, {"v2", Downloaded}} {
		version = step.version
		if status, err := File(ctx, client, server.URL, path); err != nil || status != step.status {
			t.Fatalf("%s: %s %v, want %s", step.version, status, err, step.status)
		}
		data, _ := os.ReadFile(path)
		etag, _ := os.ReadFile(path + ".etag")
		if string(data) != step.version || string(etag) != `"`+step.version+`"` {
			t.Errorf("%s: file %q, etag %q", step.version, data, etag)
		}
	}
}

// A 304 is only an answer when there is a local copy to keep.
func TestFileNotModifiedWithoutCopy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "file")
	if status, err := File(context.Background(), server.Client(), server.URL, path); err == nil {
		t.Errorf("status %s, want an error", status)
	}
}
