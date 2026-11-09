package publish

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseR2(t *testing.T) {
	good := "https://KEYID123:s3cr3tkey@abc123.r2.cloudflarestorage.com/openipdata-dbs"
	cfg, err := parseR2(good)
	if err != nil {
		t.Fatal(err)
	}
	want := r2Config{endpoint: "abc123.r2.cloudflarestorage.com", bucket: "openipdata-dbs", keyID: "KEYID123", secret: "s3cr3tkey"}
	if cfg != want {
		t.Errorf("cfg = %+v, want %+v", cfg, want)
	}
	if cfg, _ := parseR2(good + "/"); cfg.bucket != "openipdata-dbs" {
		t.Errorf("trailing slash: bucket %q", cfg.bucket)
	}

	for _, bad := range []string{
		"",
		"s3cr3tkey",
		"http://KEYID123:s3cr3tkey@abc123.r2.cloudflarestorage.com/openipdata-dbs",   // not https
		"https://KEYID123@abc123.r2.cloudflarestorage.com/openipdata-dbs",            // no secret
		"https://KEYID123:s3cr3tkey@abc123.r2.cloudflarestorage.com",                 // no bucket
		"https://KEYID123:s3cr3tkey@abc123.r2.cloudflarestorage.com/a/b",             // nested path
		"https://KEYID123:s3cr3tkey@abc123.r2.cloudflarestorage.com/bucket?x=1",      // query
		"https://KEYID123:s3cr3t key@abc123.r2.cloudflarestorage.com/openipdata-dbs", // unparsable
	} {
		_, err := parseR2(bad)
		if err != ErrInvalidR2 || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("parseR2(%q) = %v, want ErrInvalidR2 without the secret", bad, err)
		}
	}
	if _, err := NewR2(" "); err == nil || err.Error() != "R2 is not set" {
		t.Errorf("NewR2(blank) = %v", err)
	}
}

func TestR2Upload(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"0123456789abcdef"`)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	r2, err := newR2("https://KEYID123:s3cr3tkey@"+host+"/openipdata-dbs", server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "asn.sqlite3")
	if err := os.WriteFile(path, []byte("sqlite bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	location, err := r2.Upload(context.Background(), "2026-11-09/asn.sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if location != "r2://openipdata-dbs/2026-11-09/asn.sqlite3" {
		t.Errorf("location = %q", location)
	}
	if gotPath != "/openipdata-dbs/2026-11-09/asn.sqlite3" || string(gotBody) != "sqlite bytes" {
		t.Errorf("PUT %s body %q", gotPath, gotBody)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=KEYID123/") || strings.Contains(gotAuth, "s3cr3t") {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestR2Download(t *testing.T) {
	modified := time.Date(2026, 11, 9, 1, 2, 3, 0, time.UTC)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openipdata-dbs/internal/rows.csv.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		w.Header().Set("ETag", `"0123456789abcdef"`)
		w.Header().Set("Content-Length", "10")
		if r.Method == http.MethodGet {
			w.Write([]byte("rows bytes"))
		}
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	r2, err := newR2("https://KEYID123:s3cr3tkey@"+host+"/openipdata-dbs", server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rows.csv.gz")
	got, err := r2.Download(context.Background(), "internal/rows.csv.gz", path)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "rows bytes" || !got.Equal(modified) {
		t.Errorf("file %q, modified %v", data, got)
	}
	if _, err := r2.Download(context.Background(), "internal/missing", path+"2"); err == nil {
		t.Error("a missing object downloaded")
	}
}
