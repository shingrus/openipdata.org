package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	if _, err := r2.Download(context.Background(), "internal/missing", path+"2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing object: %v", err)
	}
}

// fakeR2 is an in-memory bucket behind the S3 calls the R2 type makes: HEAD, GET,
// PUT, and PUT with x-amz-copy-source (a copy). It records the requests.
type fakeR2 struct {
	t        *testing.T
	server   *httptest.Server
	objects  map[string]*fakeObject
	calls    []string
	clock    time.Time // the modified time of the next write
	failCopy bool
}

type fakeObject struct {
	body     []byte
	meta     http.Header // x-amz-meta-*
	ctype    string
	modified time.Time
	etag     string
}

func newFakeR2(t *testing.T) (*fakeR2, *R2) {
	f := &fakeR2{t: t, objects: map[string]*fakeObject{}, clock: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	r2, err := newR2("https://KEYID123:s3cr3tkey@"+strings.TrimPrefix(f.server.URL, "https://")+"/bucket", f.server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return f, r2
}

func (f *fakeR2) put(key string, body []byte, meta http.Header, ctype string) {
	f.clock = f.clock.Add(24 * time.Hour)
	sum := sha256.Sum256(body)
	f.objects[key] = &fakeObject{body: body, meta: meta, ctype: ctype, modified: f.clock, etag: hex.EncodeToString(sum[:16])}
}

func (f *fakeR2) serve(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	f.calls = append(f.calls, r.Method+" "+key)
	meta := http.Header{}
	for k, v := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			meta[http.CanonicalHeaderKey(k)] = v
		}
	}
	obj := f.objects[key]
	switch {
	case r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "":
		if f.failCopy {
			http.Error(w, `<Error><Code>NotImplemented</Code></Error>`, http.StatusNotImplemented)
			return
		}
		if src := r.Header.Get("x-amz-copy-source"); strings.TrimPrefix(src, "/") != "bucket/"+key || obj == nil {
			f.t.Errorf("copy source %q for %q", src, key)
		}
		if r.Header.Get("x-amz-metadata-directive") != "REPLACE" {
			f.t.Errorf("copy onto itself without the REPLACE directive")
		}
		f.put(key, obj.body, meta, r.Header.Get("Content-Type"))
		fmt.Fprintf(w, `<CopyObjectResult><LastModified>%s</LastModified><ETag>"%s"</ETag></CopyObjectResult>`,
			f.objects[key].modified.Format(time.RFC3339), f.objects[key].etag)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.put(key, body, meta, r.Header.Get("Content-Type"))
		w.Header().Set("ETag", `"`+f.objects[key].etag+`"`)
	case obj == nil:
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		if r.Method == http.MethodGet {
			w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`))
		}
	default: // HEAD, GET
		for k, v := range obj.meta {
			w.Header()[k] = v
		}
		w.Header().Set("Last-Modified", obj.modified.Format(http.TimeFormat))
		w.Header().Set("ETag", `"`+obj.etag+`"`)
		w.Header().Set("Content-Type", obj.ctype)
		w.Header().Set("Content-Length", fmt.Sprint(len(obj.body)))
		if r.Method == http.MethodGet {
			w.Write(obj.body)
		}
	}
}

func (f *fakeR2) reset() { f.calls = nil }

func writeTemp(t *testing.T, body string) string {
	path := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sumOf(body string) string {
	s := sha256.Sum256([]byte(body))
	return hex.EncodeToString(s[:])
}

// Upload stores the SHA-256 as user metadata, and Stat reads it back.
func TestR2UploadWritesSHA256(t *testing.T) {
	f, r2 := newFakeR2(t)
	if _, err := r2.Upload(context.Background(), "d/a.bin", writeTemp(t, "hello")); err != nil {
		t.Fatal(err)
	}
	if got := f.objects["d/a.bin"].meta.Get("X-Amz-Meta-Sha256"); got != sumOf("hello") {
		t.Errorf("metadata = %q (%v)", got, f.objects["d/a.bin"].meta)
	}
	info, err := r2.Stat(context.Background(), "d/a.bin")
	if err != nil || info.SHA256 != sumOf("hello") || info.ETag == "" || info.LastModified.IsZero() {
		t.Errorf("info = %+v %v", info, err)
	}
	if _, err := r2.Stat(context.Background(), "d/missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing object: %v", err)
	}
}

// Put sends the bytes only when the object does not already hold them.
func TestR2PutSkipsIdenticalUpload(t *testing.T) {
	ctx := context.Background()
	f, r2 := newFakeR2(t)
	path := writeTemp(t, "mmdb v1")

	// Nothing there: uploaded.
	loc, status, err := r2.Put(ctx, "2026-11-12/a.mmdb", path, PutOptions{})
	if err != nil || status != PutUploaded || loc != "r2://bucket/2026-11-12/a.mmdb" {
		t.Fatalf("first put: %q %q %v", loc, status, err)
	}
	// The same bytes: one HEAD, no PUT.
	f.reset()
	if _, status, err = r2.Put(ctx, "2026-11-12/a.mmdb", path, PutOptions{}); err != nil || status != PutUnchanged {
		t.Fatalf("same bytes: %q %v", status, err)
	}
	if !reflect.DeepEqual(f.calls, []string{"HEAD 2026-11-12/a.mmdb"}) {
		t.Errorf("calls = %v", f.calls)
	}
	// Other bytes at the key: uploaded. Another day's folder (another key): uploaded.
	other := writeTemp(t, "mmdb v2")
	if _, status, _ = r2.Put(ctx, "2026-11-12/a.mmdb", other, PutOptions{}); status != PutUploaded || string(f.objects["2026-11-12/a.mmdb"].body) != "mmdb v2" {
		t.Errorf("changed bytes: %q", status)
	}
	if _, status, _ = r2.Put(ctx, "2026-11-13/a.mmdb", other, PutOptions{}); status != PutUploaded || f.objects["2026-11-13/a.mmdb"] == nil {
		t.Errorf("new day: %q", status)
	}
	// Force sends the same bytes again.
	f.reset()
	if _, status, _ = r2.Put(ctx, "2026-11-13/a.mmdb", other, PutOptions{Force: true}); status != PutUploaded || !reflect.DeepEqual(f.calls, []string{"PUT 2026-11-13/a.mmdb"}) {
		t.Errorf("force: %q %v", status, f.calls)
	}
	// An object an older tool uploaded has no sha256: it is replaced once.
	f.put("old/a.mmdb", []byte("mmdb v2"), http.Header{}, "application/octet-stream")
	if _, status, _ = r2.Put(ctx, "old/a.mmdb", other, PutOptions{}); status != PutUploaded || f.objects["old/a.mmdb"].meta.Get("X-Amz-Meta-Sha256") == "" {
		t.Errorf("object without metadata: %q", status)
	}
}

// With Refresh the same bytes are not sent, but the object's modified time is renewed
// by a copy onto itself that keeps the sha256 and the content type.
func TestR2PutRefreshesIdenticalObject(t *testing.T) {
	ctx := context.Background()
	f, r2 := newFakeR2(t)
	path := writeTemp(t, "rows")
	if _, status, err := r2.Put(ctx, "internal/rows", path, PutOptions{Refresh: true}); err != nil || status != PutUploaded {
		t.Fatalf("first: %q %v", status, err)
	}
	before := f.objects["internal/rows"].modified
	f.reset()
	_, status, err := r2.Put(ctx, "internal/rows", path, PutOptions{Refresh: true})
	if err != nil || status != PutRefreshed {
		t.Fatalf("second: %q %v", status, err)
	}
	if !reflect.DeepEqual(f.calls, []string{"HEAD internal/rows", "PUT internal/rows"}) {
		t.Errorf("calls = %v", f.calls)
	}
	obj := f.objects["internal/rows"]
	if !obj.modified.After(before) || string(obj.body) != "rows" || obj.meta.Get("X-Amz-Meta-Sha256") != sumOf("rows") || obj.ctype != "application/octet-stream" {
		t.Errorf("object after refresh: %+v", obj)
	}
	info, _ := r2.Stat(ctx, "internal/rows")
	if !info.LastModified.Equal(obj.modified) || info.SHA256 != sumOf("rows") {
		t.Errorf("stat after refresh: %+v", info)
	}
	// If the server refuses the copy, the file is uploaded: no worse than before.
	f.failCopy = true
	f.reset()
	if _, status, err = r2.Put(ctx, "internal/rows", path, PutOptions{Refresh: true}); err != nil || status != PutUploaded {
		t.Errorf("copy refused: %q %v", status, err)
	}
	// Force never refreshes, it uploads.
	f.failCopy = false
	f.reset()
	if _, status, _ = r2.Put(ctx, "internal/rows", path, PutOptions{Refresh: true, Force: true}); status != PutUploaded || !reflect.DeepEqual(f.calls, []string{"PUT internal/rows"}) {
		t.Errorf("force: %q %v", status, f.calls)
	}
}

// DownloadIfChanged skips the download when the ETag is the one the caller saved.
func TestR2DownloadIfChanged(t *testing.T) {
	ctx := context.Background()
	f, r2 := newFakeR2(t)
	f.put("internal/rows", []byte("rows v1"), http.Header{}, "application/octet-stream")
	path := filepath.Join(t.TempDir(), "rows")

	info, downloaded, err := r2.DownloadIfChanged(ctx, "internal/rows", path, "")
	if err != nil || !downloaded || info.ETag != f.objects["internal/rows"].etag || !info.LastModified.Equal(f.objects["internal/rows"].modified) {
		t.Fatalf("first: %+v %v %v", info, downloaded, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "rows v1" {
		t.Errorf("file = %q", data)
	}
	// Same ETag: only a HEAD, the file is left alone, the info still tells the modified time.
	f.reset()
	os.WriteFile(path, []byte("kept"), 0o644)
	info2, downloaded, err := r2.DownloadIfChanged(ctx, "internal/rows", path, info.ETag)
	if err != nil || downloaded || !info2.LastModified.Equal(info.LastModified) || !reflect.DeepEqual(f.calls, []string{"HEAD internal/rows"}) {
		t.Errorf("unchanged: %+v %v %v calls %v", info2, downloaded, err, f.calls)
	}
	if data, _ := os.ReadFile(path); string(data) != "kept" {
		t.Errorf("an unchanged object rewrote the file: %q", data)
	}
	// Another ETag: downloaded.
	f.put("internal/rows", []byte("rows v2"), http.Header{}, "application/octet-stream")
	if info3, downloaded, err := r2.DownloadIfChanged(ctx, "internal/rows", path, info.ETag); err != nil || !downloaded || info3.ETag == info.ETag {
		t.Errorf("changed: %+v %v %v", info3, downloaded, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "rows v2" {
		t.Errorf("file = %q", data)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
	// A missing object is ErrNotFound, and nothing is written.
	if _, _, err := r2.DownloadIfChanged(ctx, "internal/none", path+"2", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	// None of it puts the secret into an error.
	f.server.Close()
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, _, err := r2.DownloadIfChanged(short, "internal/rows", path, ""); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("server down: %v", err)
	}
}
