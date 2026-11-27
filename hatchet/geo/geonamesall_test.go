package geo

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// allZip is an allCountries.zip with the one entry, stored (not compressed), so that a test
// script can grep it.
func allZip(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(body))
	zw.Close()
	return buf.Bytes()
}

// swapHandler is a handler a test can change between requests.
type swapHandler struct {
	mu   sync.Mutex
	h    http.HandlerFunc
	hits int
}

func (s *swapHandler) set(h http.HandlerFunc) { s.mu.Lock(); s.h = h; s.mu.Unlock() }
func (s *swapHandler) count() int             { s.mu.Lock(); defer s.mu.Unlock(); return s.hits }
func (s *swapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits++
	h := s.h
	s.mu.Unlock()
	h(w, r)
}

func TestFetchGeoNamesAll(t *testing.T) {
	body := allZip(t, geonamesAllText, "x\n")
	sw := &swapHandler{h: func(w http.ResponseWriter, r *http.Request) { w.Write(body) }}
	server := httptest.NewServer(sw)
	defer server.Close()
	p := &Pipeline{DataDir: t.TempDir(), refURLs: &evalURLs{geonamesAll: server.URL + "/allCountries.zip"}}
	dir := p.evalDir()
	os.MkdirAll(dir, 0o750)
	now := time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)
	file, status := p.fetchGeoNamesAll(context.Background(), server.Client(), dir, now)
	if file != filepath.Join(dir, "allCountries.zip") || status == "" || sw.count() != 1 {
		t.Fatalf("first: %q %q %d", file, status, sw.count())
	}
	// Checked within 7 days: no request.
	if file, _ = p.fetchGeoNamesAll(context.Background(), server.Client(), dir, now.Add(24*time.Hour)); file == "" || sw.count() != 1 {
		t.Errorf("recent: %q %d", file, sw.count())
	}
	// Older than 7 days: asked again.
	if file, _ = p.fetchGeoNamesAll(context.Background(), server.Client(), dir, now.Add(8*24*time.Hour)); file == "" || sw.count() != 2 {
		t.Errorf("old: %q %d", file, sw.count())
	}
	// A failed download uses the saved copy.
	sw.set(http.NotFound)
	if file, status = p.fetchGeoNamesAll(context.Background(), server.Client(), dir, now.Add(20*24*time.Hour)); file == "" || !strings.HasPrefix(status, "stale: ") {
		t.Errorf("failed download: %q %q", file, status)
	}
	// No copy and no download: skipped.
	p2 := &Pipeline{DataDir: t.TempDir(), refURLs: p.refURLs}
	os.MkdirAll(p2.evalDir(), 0o750)
	if file, status = p2.fetchGeoNamesAll(context.Background(), server.Client(), p2.evalDir(), now); file != "" || !strings.HasPrefix(status, "skipped: ") {
		t.Errorf("no copy: %q %q", file, status)
	}
	// Not a zip: dropped, so the next run fetches again.
	sw.set(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) })
	if file, status = p2.fetchGeoNamesAll(context.Background(), server.Client(), p2.evalDir(), now); file != "" || status != "skipped: not a GeoNames archive" {
		t.Errorf("html: %q %q", file, status)
	}
	if _, err := os.Stat(filepath.Join(p2.evalDir(), "allCountries.zip")); err == nil {
		t.Error("the bad archive was kept")
	}
}

// The flag goes with the list after -geonames, only with GeoNames; a list the builder cannot read
// drops only itself; the retry without GeoNames drops both; the city-fallback line is counted.
func TestBuildMMDBGeoNamesAll(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> "+argsFile+"\n"+
		"for a in \"$@\"; do if [ -f \"$a\" ] && grep -q BAD \"$a\" 2>/dev/null; then echo \"error: $a: bad\" >&2; exit 1; fi; done\n"+
		"case \"$*\" in *-geonames-all*) echo 'city-fallback: 90 rows placed from the full list, 12 names placed, 1 ambiguous, 3 still unplaced';; esac\n"+
		"echo 'layer 10-rir.csv.gz: 10 rows, 9 prefixes, 0 skipped'\n"+
		"echo 'wrote x (1 layers, 9 prefixes)'\n"), 0o755)
	cities := zipBytes(t, geoNamesSample)
	all := allZip(t, geonamesAllText, "1\tx\n")
	var haveAll, html atomic.Bool
	haveAll.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "allCountries") {
			if !haveAll.Load() {
				http.NotFound(w, r)
				return
			}
			if html.Load() {
				w.Write([]byte("<html>"))
				return
			}
			w.Write(all)
			return
		}
		w.Write(cities)
	}))
	defer server.Close()
	p := &Pipeline{DataDir: dir, MMDBBuilder: script, refURLs: &evalURLs{geonames: server.URL + "/cities1000.zip", geonamesAll: server.URL + "/allCountries.zip"}}
	touchLayers(t, p, RIRLayer, NamesFile)
	zipPath := filepath.Join(p.evalDir(), "allCountries.zip")
	txt := filepath.Join(p.evalDir(), "cities1000.txt")
	build := func() (BuildMMDBOutput, []string) {
		os.Remove(argsFile)
		out, err := p.BuildMMDB(context.Background(), Source{DateTag: "20261109"})
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(argsFile)
		return out, strings.Split(strings.TrimSpace(string(data)), "\n")
	}

	// A good list: passed right after -geonames.
	out, calls := build()
	if len(calls) != 1 || !strings.Contains(calls[0], " -geonames "+txt+" -geonames-all "+zipPath+" ") || out.GeoNamesAll != "on" || out.GeoNamesAllPlaced != 12 {
		t.Errorf("good: %v %q %d", calls, out.GeoNamesAll, out.GeoNamesAllPlaced)
	}

	// No list (no copy and no download): not passed, the build goes on.
	os.Remove(zipPath)
	os.Remove(zipPath + ".checked")
	haveAll.Store(false)
	out, calls = build()
	if len(calls) != 1 || strings.Contains(calls[0], "geonames-all") || out.GeoNamesAll != "skipped: no full list" || out.GeoNamesAllPlaced != 0 || out.Coordinates != "on" {
		t.Errorf("no list: %v %q", calls, out.GeoNamesAll)
	}

	// A copy that is fetched and rejected (not a zip): unreadable, not "no full list".
	os.Remove(zipPath)
	os.Remove(zipPath + ".checked")
	html.Store(true)
	haveAll.Store(true)
	out, calls = build()
	if len(calls) != 1 || strings.Contains(calls[0], "geonames-all") || out.GeoNamesAll != "skipped: full list unreadable" {
		t.Errorf("rejected: %v %q", calls, out.GeoNamesAll)
	}
	html.Store(false)

	// A list the builder cannot read: built again without it, everything else stays.
	os.WriteFile(zipPath, allZip(t, geonamesAllText, "BAD\n"), 0o640)
	markChecked(zipPath, time.Now())
	out, calls = build()
	if len(calls) != 2 || !strings.Contains(calls[0], "-geonames-all") || strings.Contains(calls[1], "geonames-all") || !strings.Contains(calls[1], " -geonames "+txt) ||
		out.GeoNamesAll != "skipped: full list unreadable" || out.Coordinates != "on" {
		t.Errorf("unreadable: %v %q %q", calls, out.GeoNamesAll, out.Coordinates)
	}

	// GeoNames itself unusable: the retry without it drops the list too.
	os.WriteFile(zipPath, allZip(t, geonamesAllText, "fine\n"), 0o640)
	os.WriteFile(txt, []byte("BAD\n"), 0o640)
	out, calls = build()
	if len(calls) != 2 || strings.Contains(calls[1], "-geonames") || out.GeoNamesAll != "skipped: no GeoNames" {
		t.Errorf("no GeoNames: %v %q", calls, out.GeoNamesAll)
	}

	// Without a GeoNames copy at all the list is not even fetched.
	p2 := &Pipeline{DataDir: t.TempDir(), MMDBBuilder: script, refURLs: &evalURLs{geonames: server.URL + "/missing", geonamesAll: server.URL + "/allCountries.zip"}}
	server2 := httptest.NewServer(http.NotFoundHandler())
	defer server2.Close()
	p2.refURLs.geonames = server2.URL + "/cities1000.zip"
	touchLayers(t, p2, RIRLayer, NamesFile)
	os.Remove(argsFile)
	out2, err := p2.BuildMMDB(context.Background(), Source{DateTag: "20261109"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(argsFile)
	if strings.Contains(string(data), "geonames") || out2.GeoNamesAll != "skipped: no GeoNames" {
		t.Errorf("no GeoNames at all: %q %q", data, out2.GeoNamesAll)
	}
}

// publish passes the list to convert when the build's copy is there, drops only it when the convert
// fails with it, and never fetches it.
func TestConvertPublishedGeoNamesAll(t *testing.T) {
	date := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	script := `echo "$@" >> "$(dirname "$0")/calls"; case "$*" in *-geonames-all*) echo "bad list" >&2; exit 1;; esac; echo ok`
	p := evalPipeline(t, script)
	os.MkdirAll(p.evalDir(), 0o750)
	os.WriteFile(filepath.Join(p.evalDir(), geonamesEntry), []byte(strings.TrimSpace(geoNamesSample)+"\n"), 0o640)
	mmdb := filepath.Join(p.DataDir, "ip2geo-openipdata-20261120.mmdb")
	zipPath := filepath.Join(p.evalDir(), "allCountries.zip")

	var out PublishOutput
	if _, err := p.convertPublished(context.Background(), date, mmdb, &out); err != nil || out.GeoNamesAll != "" || out.GeoNames != "on" {
		t.Fatalf("no copy: %v %+v", err, out)
	}
	os.WriteFile(zipPath, allZip(t, geonamesAllText, "x\n"), 0o640)
	out = PublishOutput{}
	if _, err := p.convertPublished(context.Background(), date, mmdb, &out); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(p.DataDir, "calls"))
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	// The first run (no copy) has one call; then the one with the list, and the one without it.
	if len(lines) != 3 || !strings.Contains(lines[1], " -geonames-all "+zipPath) || strings.Contains(lines[2], "geonames-all") || !strings.Contains(lines[2], "-geonames ") ||
		out.GeoNamesAll != "skipped: convert failed with the full list" || out.GeoNames != "on" {
		t.Errorf("calls = %q, out = %+v", calls, out)
	}
}
