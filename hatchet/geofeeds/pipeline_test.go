package geofeeds

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gzipped(t *testing.T, text string) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write([]byte(text))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchFile(t *testing.T) {
	modified := time.Date(2026, 11, 8, 23, 20, 43, 0, time.UTC)
	requests := 0
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
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

	client := newHTTPClient(time.Minute, true)
	path := filepath.Join(t.TempDir(), "dump.gz")
	ctx := context.Background()
	if status, err := fetchFile(ctx, client, server.URL, path); err != nil || status != statusDownloaded {
		t.Fatalf("first fetch: %s %v", status, err)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(modified) {
		t.Fatalf("mtime not set to Last-Modified: %v %v", info, err)
	}
	if status, err := fetchFile(ctx, client, server.URL, path); err != nil || status != statusCached {
		t.Fatalf("second fetch: %s %v", status, err)
	}
	fail = true
	if _, err := fetchFile(ctx, client, server.URL, path); err == nil {
		t.Fatal("want an error from a failing server")
	}
	if data, _ := os.ReadFile(path); string(data) != "data" {
		t.Errorf("a failed fetch changed the file: %q", data)
	}
	if requests != 3 {
		t.Errorf("requests = %d", requests)
	}
}

func TestDiscover(t *testing.T) {
	whois := gzipped(t, "inetnum: 192.0.2.0 - 192.0.2.255\nremarks: Geofeed https://isp.example/geofeed.csv\n\n"+
		"inetnum: 198.51.100.0 - 198.51.100.255\ngeofeed: https://isp.example/geofeed.csv\n\n"+
		"inetnum: 203.0.113.0 - 203.0.113.255\nremarks: no feed here\n")
	irr := gzipped(t, "route: 203.0.113.0/24\nremarks: geofeed https://other.example/feed.csv\n")
	day := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/ripe.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(whois) })
	mux.HandleFunc("/irr.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(irr) })
	mux.HandleFunc("/broken.gz", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/caida/2026/11/08/standard_geofeeds.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ripe_remarks_1_https://isp.example/geofeed.csv\narin_nonstandard_1_https://arin.example/g.csv\n"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p := &Pipeline{
		DataDir: t.TempDir(),
		dumps: []dump{
			{"ripe", "ripe", server.URL + "/ripe.gz"},
			{"irr", "irr", server.URL + "/irr.gz"},
			{"lacnic-irr", "irr", server.URL + "/broken.gz"}, // a missing IRR dump is not fatal
		},
		caidaBase:    server.URL + "/caida",
		now:          func() time.Time { return day },
		allowPrivate: true,
	}
	out, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s := out.Sources["ripe"]; s.Status != statusDownloaded || s.Objects != 3 || s.Blocks != 2 || s.URLs != 1 {
		t.Errorf("ripe stats = %+v", s)
	}
	if s := out.Sources["lacnic-irr"]; s.Status != statusFailed || s.Error == "" {
		t.Errorf("lacnic-irr stats = %+v", s)
	}
	if s := out.Sources["caida"]; s.Status != statusDownloaded || s.URLs != 2 {
		t.Errorf("caida stats = %+v", s)
	}
	if out.Blocks != 3 {
		t.Errorf("blocks = %d", out.Blocks)
	}

	urls, err := os.ReadFile(filepath.Join(p.dir(), URLsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"https://arin.example/g.csv,caida,0",
		"https://isp.example/geofeed.csv,caida|ripe,2",
		"https://other.example/feed.csv,irr,1",
		"https://mask-api.icloud.com/egress-ip-ranges.csv,list,0",
	} {
		if !strings.Contains(string(urls), want+"\n") {
			t.Errorf("urls.csv lacks %q:\n%s", want, urls)
		}
	}
	blocks, err := os.ReadFile(filepath.Join(p.dir(), BlocksFile))
	if err != nil {
		t.Fatal(err)
	}
	wantBlocks := "source,prefix,url\n" +
		"ripe,192.0.2.0/24,https://isp.example/geofeed.csv\n" +
		"ripe,198.51.100.0/24,https://isp.example/geofeed.csv\n" +
		"irr,203.0.113.0/24,https://other.example/feed.csv\n"
	if string(blocks) != wantBlocks {
		t.Errorf("blocks.csv:\n%s\nwant:\n%s", blocks, wantBlocks)
	}

	// One whois dump missing fails the run and keeps the previous outputs.
	before, _ := os.ReadFile(filepath.Join(p.dir(), URLsFile))
	p.dumps = []dump{{"ripe", "ripe", server.URL + "/ripe.gz"}, {"lacnic", "lacnic", server.URL + "/broken.gz"}}
	if _, err := p.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "lacnic") {
		t.Errorf("want an error naming the missing dump, got %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(p.dir(), URLsFile)); string(after) != string(before) {
		t.Error("a failed run replaced urls.csv")
	}

	// A corrupt cached dump is removed, so the next run downloads it again.
	os.WriteFile(filepath.Join(p.dir(), "dumps", "ripe.gz"), []byte("not gzip"), 0o640)
	p.dumps = []dump{{"ripe", "ripe", server.URL + "/broken.gz"}}
	if _, err := p.Discover(context.Background()); err == nil {
		t.Error("want an error when no whois dump can be read")
	}
	if _, err := os.Stat(filepath.Join(p.dir(), "dumps", "ripe.gz")); !os.IsNotExist(err) {
		t.Errorf("corrupt dump kept: %v", err)
	}
}

// A dump that breaks halfway adds none of its blocks.
func TestReadDumpBroken(t *testing.T) {
	whole := gzipped(t, "inetnum: 192.0.2.0 - 192.0.2.255\ngeofeed: https://isp.example/geofeed.csv\n\n"+
		"inetnum: 198.51.100.0 - 198.51.100.255\ngeofeed: https://isp.example/geofeed.csv\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(whole[:len(whole)-8]) // no gzip trailer
	}))
	defer server.Close()
	p := &Pipeline{DataDir: t.TempDir(), allowPrivate: true}
	c := &collector{urls: map[string]*found{}, blocks: csv.NewWriter(io.Discard)}
	path := filepath.Join(t.TempDir(), "ripe.gz")
	stats := p.readDump(context.Background(), newHTTPClient(time.Minute, true), c, dump{"ripe", "ripe", server.URL}, path)
	if stats.Status != statusFailed || stats.Blocks != 0 || len(c.urls) != 0 || c.nRows != 0 {
		t.Errorf("stats = %+v, %d URLs, %d rows", stats, len(c.urls), c.nRows)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the broken dump was kept")
	}
}
