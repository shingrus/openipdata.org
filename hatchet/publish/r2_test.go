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
