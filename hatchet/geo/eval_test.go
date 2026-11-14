package geo

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/download"
)

func writeGzip(t *testing.T, path, body string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(body))
	gz.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
}

func readGzip(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// feed1 and feed7 are held out; any other URL is kept.
var testHeldOut = map[string]bool{"https://feed1.example/geo.csv": true, "https://feed7.example/geo.csv": true}

func heldOutSet(urls map[string]bool) func(context.Context) (map[string]bool, error) {
	return func(context.Context) (map[string]bool, error) { return urls, nil }
}

func TestSplitFeeds(t *testing.T) {
	dir := t.TempDir()
	const header = "prefix,country,region,city,source\n"
	feeds := filepath.Join(dir, "geofeed-rows.csv.gz")
	writeGzip(t, feeds, header+
		// feed0 and feed2 are kept, feed1 and feed7 are held out.
		"45.0.0.0/16,US,,Dallas,https://feed0.example/geo.csv\n"+
		"2a00::/32,DE,,Berlin,https://feed0.example/geo.csv\n"+
		"49.0.1.0/24,ES,,,https://feed0.example/geo.csv\n"+
		"45.0.1.0/24,CA,,Toronto,https://feed1.example/geo.csv\n"+ // inside a kept row
		"46.0.0.0/24,FR,,\"Paris, 8e\",https://feed1.example/geo.csv\n"+
		"47.0.0.0/16,NL,,Utrecht,https://feed1.example/geo.csv\n"+ // contains a kept row
		"2a01::/32,SE,,Stockholm,https://feed1.example/geo.csv\n"+ // IPv6: not scored
		"47.0.5.0/24,GB,,London,https://feed2.example/geo.csv\n"+
		"48.0.0.9,BE,,Brussels,https://feed7.example/geo.csv\n"+ // a single address
		"50.0.0.0/24,,,,https://feed7.example/geo.csv\n"+ // no country
		"49.0.0.0/24,ES,,Madrid,https://feed7.example/geo.csv\n") // next to a kept row, not over it

	kept, truth := filepath.Join(dir, "kept.csv.gz"), filepath.Join(dir, "truth.csv")
	stats, err := splitFeeds(feeds, kept, truth, testHeldOut)
	if err != nil {
		t.Fatal(err)
	}
	if want := (holdoutStats{Feeds: 4, HeldFeeds: 2, KeptRows: 4, HeldRows: 7, TruthRows: 3}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	if got := readGzip(t, kept); got != header+
		"45.0.0.0/16,US,,Dallas,https://feed0.example/geo.csv\n"+
		"2a00::/32,DE,,Berlin,https://feed0.example/geo.csv\n"+
		"49.0.1.0/24,ES,,,https://feed0.example/geo.csv\n"+
		"47.0.5.0/24,GB,,London,https://feed2.example/geo.csv\n" {
		t.Errorf("kept rows:\n%s", got)
	}
	wantTruth := "prefix,country,city\n46.0.0.0/24,FR,\"Paris, 8e\"\n48.0.0.9/32,BE,Brussels\n49.0.0.0/24,ES,Madrid\n"
	first, _ := os.ReadFile(truth)
	if string(first) != wantTruth {
		t.Errorf("truth:\n%s", first)
	}

	// Deterministic: the same input gives the same files.
	firstKept := readGzip(t, kept)
	if _, err := splitFeeds(feeds, kept, truth, testHeldOut); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(truth)
	if !bytes.Equal(first, second) || readGzip(t, kept) != firstKept {
		t.Error("a second split gave different files")
	}

	if _, err := splitFeeds(feeds, kept, filepath.Join(dir, "missing", "truth.csv"), testHeldOut); err == nil {
		t.Error("no error for an unwritable truth file")
	}
	writeGzip(t, feeds, "prefix,country\n45.0.0.0/8,US\n")
	if _, err := splitFeeds(feeds, kept, truth, testHeldOut); err == nil {
		t.Error("no error for a file without source")
	}
}

// The holdout build runs with the real build's layers and rules, but with the kept
// feeds file, which must keep the layer's file name.
func TestPrepareHoldoutArgs(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"), 0o755)
	p := &Pipeline{DataDir: dir, MMDBBuilder: script, heldOutURLs: heldOutSet(testHeldOut)}
	layers := p.layersDir()
	os.MkdirAll(layers, 0o750)
	for _, name := range []string{RIRLayer, PDBLayer, HostingLayer, IXLayer, WhoisLayer} {
		writeGzip(t, filepath.Join(layers, name), "prefix,country,region,city,source\n")
	}
	writeGzip(t, filepath.Join(layers, FeedsLayer), "prefix,country,region,city,source\n"+
		"45.0.0.0/16,US,,,https://feed0.example/geo.csv\n46.0.0.0/24,FR,,Paris,https://feed1.example/geo.csv\n")

	eval := p.evalDir()
	os.MkdirAll(eval, 0o750)
	mmdb, truth, status, err := p.prepareHoldout(context.Background(), eval)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	kept := filepath.Join(eval, "holdout", FeedsLayer)
	want := strings.Join([]string{"geo", "-out", filepath.Join(eval, "holdout.mmdb"),
		"-names", filepath.Join(layers, NamesFile), "-first-country", HostingLayer, "-same-country", PDBLayer, "-within-first", WhoisLayer + "," + FeedsLayer,
		filepath.Join(layers, RIRLayer), filepath.Join(layers, PDBLayer), filepath.Join(layers, HostingLayer), filepath.Join(layers, IXLayer), filepath.Join(layers, WhoisLayer), kept}, " ")
	if strings.TrimSpace(string(args)) != want {
		t.Errorf("args = %s\nwant   %s", args, want)
	}
	if mmdb != filepath.Join(eval, "holdout.mmdb") || truth != filepath.Join(eval, "holdout-truth.csv") ||
		status != "1 of 2 feeds held out, 1 of 2 rows, 1 scored" {
		t.Errorf("%s %s %q", mmdb, truth, status)
	}

	// A layer that is not in the directory (skipped) is left out of the holdout build as well,
	// with its rule.
	os.Remove(filepath.Join(layers, HostingLayer))
	os.Remove(filepath.Join(layers, WhoisLayer))
	if _, _, _, err = p.prepareHoldout(context.Background(), eval); err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(argsFile)
	if strings.Contains(string(args), "40-") || strings.Contains(string(args), "60-") || strings.Contains(string(args), "-first-country") ||
		!strings.Contains(string(args), "-within-first "+FeedsLayer+" ") ||
		!strings.HasSuffix(strings.TrimSpace(string(args)), filepath.Join(layers, IXLayer)+" "+kept) {
		t.Errorf("skipped args = %s", args)
	}

	// Without held-out feeds there is nothing to score and no build.
	os.Remove(argsFile)
	p.heldOutURLs = heldOutSet(map[string]bool{})
	if mmdb, _, status, err = p.prepareHoldout(context.Background(), eval); err != nil || mmdb != "" || !strings.HasPrefix(status, "skipped: ") {
		t.Errorf("empty set: %q %q %v", mmdb, status, err)
	}
	// A failed lookup is an error, never another sample.
	p.heldOutURLs = func(context.Context) (map[string]bool, error) { return nil, errors.New("boom") }
	if _, _, _, err = p.prepareHoldout(context.Background(), eval); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("lookup error = %v", err)
	}
	p.heldOutURLs = heldOutSet(testHeldOut)
	if _, err := os.Stat(argsFile); err == nil {
		t.Error("mmdbbuilder ran without held-out feeds")
	}

	// Nothing held out outside the kept feeds: nothing to score, no build.
	os.Remove(argsFile)
	writeGzip(t, filepath.Join(layers, FeedsLayer), "prefix,country,region,city,source\n45.0.0.0/16,US,,,https://feed0.example/geo.csv\n")
	if mmdb, _, status, err = p.prepareHoldout(context.Background(), eval); err != nil || mmdb != "" || !strings.HasPrefix(status, "skipped: ") {
		t.Errorf("no held-out rows: %q %q %v", mmdb, status, err)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Error("mmdbbuilder ran with nothing to score")
	}
}

func TestGateDecision(t *testing.T) {
	for _, c := range []struct {
		name          string
		before, after map[string]float64
		refuse        bool
	}{
		{"same", map[string]float64{"atlas_country_pct": 97, "holdout_country_pct": 80}, map[string]float64{"atlas_country_pct": 97, "holdout_country_pct": 80}, false},
		{"better", map[string]float64{"atlas_country_pct": 90}, map[string]float64{"atlas_country_pct": 99}, false},
		{"atlas drop", map[string]float64{"atlas_country_pct": 97.5}, map[string]float64{"atlas_country_pct": 95.4}, true},
		{"holdout drop", map[string]float64{"holdout_country_pct": 80}, map[string]float64{"holdout_country_pct": 77.99}, true},
		{"exactly 2.0", map[string]float64{"atlas_country_pct": 97.5, "holdout_country_pct": 80.3}, map[string]float64{"atlas_country_pct": 95.5, "holdout_country_pct": 78.3}, false},
		{"only one drops", map[string]float64{"atlas_country_pct": 97, "holdout_country_pct": 80}, map[string]float64{"atlas_country_pct": 97, "holdout_country_pct": 70}, true},
		{"missing now", map[string]float64{"atlas_country_pct": 97}, map[string]float64{"holdout_country_pct": 10}, false},
		{"missing before", map[string]float64{}, map[string]float64{"atlas_country_pct": 10}, false},
		{"other scores do not count", map[string]float64{"country_pct": 99, "dbip_country_agree_pct": 95}, map[string]float64{"country_pct": 50, "dbip_country_agree_pct": 10}, false},
		{"no scores", nil, nil, false},
	} {
		err := gateDecision(c.before, c.after)
		if c.refuse != errors.Is(err, ErrGate) || (err != nil && !c.refuse) {
			t.Errorf("%s: err = %v, want refuse=%v", c.name, err, c.refuse)
		}
	}
	err := gateDecision(map[string]float64{"atlas_country_pct": 97.5}, map[string]float64{"atlas_country_pct": 90})
	if err == nil || !strings.Contains(err.Error(), "atlas_country_pct fell from 97.50 to 90.00") {
		t.Errorf("message = %v", err)
	}
}

const testToken = "tok-4f9c1d7e-SECRET"

// tarGzMMDB is a MaxMind archive: GeoLite2-City_YYYYMMDD/GeoLite2-City.mmdb among other files.
func tarGzMMDB(t *testing.T, mmdb string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range [][2]string{
		{"GeoLite2-City_20261110/COPYRIGHT.txt", "copyright"},
		{"GeoLite2-City_20261110/GeoLite2-City.mmdb", mmdb},
	} {
		tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg})
		tw.Write([]byte(f[1]))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

const testIP2LocationRow = "\"16777216\",\"16777471\",\"AU\",\"Australia\",\"Queensland\",\"South Brisbane\",\"-27.4\",\"153.0\",\"4101\",\"+10:00\"\n"

func zipIP2Location(t *testing.T, csv string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range [][2]string{{"LICENSE-CC-BY-SA-4.0.TXT", "license"}, {"IP2LOCATION-LITE-DB11.CSV", csv}} {
		w, _ := zw.Create(f[0])
		w.Write([]byte(f[1]))
	}
	zw.Close()
	return buf.Bytes()
}

// An error from a request whose URL holds the key must not repeat it, for any
// kind of failure; the age gate keeps the daily limit; a failed or bad
// download falls back to the copy of an earlier run.
func TestMaxMindKeyNeverShown(t *testing.T) {
	dir := t.TempDir()
	body := tarGzMMDB(t, "some data\xab\xcd\xefMaxMind.com{metadata}")
	status, requests := http.StatusOK, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		q := r.URL.Query()
		if q.Get("license_key") != testToken || q.Get("edition_id") != "GeoLite2-City" || q.Get("suffix") != "tar.gz" {
			t.Errorf("query %q", r.URL.RawQuery)
		}
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		w.Write(body)
	}))
	p := &Pipeline{MaxMindKey: testToken, refURLs: &evalURLs{maxmind: server.URL + "/app/geoip_download"}}
	ctx, client := context.Background(), server.Client()
	path := filepath.Join(dir, "geolite2-city.mmdb")
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)

	if got, st := p.fetchMaxMind(ctx, client, dir, now); got != path || st != download.Downloaded || !looksLikeMMDB(path) {
		t.Fatalf("download: %q %q", got, st)
	}
	// Within 3 days there is no request at all, even one that would fail.
	status = http.StatusInternalServerError
	if got, st := p.fetchMaxMind(ctx, client, dir, now.Add(2*24*time.Hour)); got != path || st != download.Cached || requests != 1 {
		t.Errorf("age gate: %q %q after %d requests", got, st, requests)
	}

	// An HTTP error keeps its status, not the URL; the cached copy is used.
	status = http.StatusUnauthorized
	later := now.Add(4 * 24 * time.Hour)
	got, st := p.fetchMaxMind(ctx, client, dir, later)
	if got != path || st != "stale: MaxMind: HTTP 401" || requests != 2 {
		t.Errorf("HTTP 401: %q %q", got, st)
	}

	// A refused connection: Go's own error holds the URL with the key.
	server.Close()
	_, rawErr := download.File(ctx, client, server.URL+"/app/geoip_download?license_key="+testToken, filepath.Join(dir, "raw"))
	if rawErr == nil || !strings.Contains(rawErr.Error(), testToken) {
		t.Fatalf("the test needs an error that repeats the key, got %v", rawErr)
	}
	got, st = p.fetchMaxMind(ctx, client, dir, later)
	if got != path || strings.Contains(st, testToken) || st != "stale: "+errMaxMind.Error() {
		t.Errorf("connection refused with a copy: %q %q", got, st)
	}
	if strings.Contains(refError("MaxMind", errMaxMind, rawErr).Error(), testToken) {
		t.Error("refError repeats the key")
	}
	os.Remove(filepath.Join(dir, "geolite2-city.tar.gz"))
	os.Remove(path)
	got, st = p.fetchMaxMind(ctx, client, dir, later)
	if got != "" || strings.Contains(st, testToken) || st != "skipped: "+errMaxMind.Error() {
		t.Errorf("connection refused, no copy: %q %q", got, st)
	}

	// No key, no request.
	p.MaxMindKey = ""
	if got, st = p.fetchMaxMind(ctx, client, dir, later); got != "" || st != "skipped: MAXMIND_LICENSE_KEY is not set" {
		t.Errorf("no key: %q %q", got, st)
	}
}

// A download that is not an archive with the MMDB drops the bad archive but
// keeps using an older unpacked copy.
func TestFetchMaxMindBadArchive(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer server.Close()
	p := &Pipeline{MaxMindKey: testToken, refURLs: &evalURLs{maxmind: server.URL}}
	ctx, now := context.Background(), time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)

	for name, bad := range map[string][]byte{
		"not gzip":       []byte("Invalid license key"),
		"no mmdb":        tarGzMMDB(t, "not a database"),
		"gzip, not tar":  gzipBytes("plain text"),
		"only a license": nil,
	} {
		body = bad
		dir := t.TempDir()
		old := filepath.Join(dir, "geolite2-city.mmdb")
		os.WriteFile(old, []byte("old\xab\xcd\xefMaxMind.com{metadata}"), 0o640)
		path, status := p.fetchMaxMind(ctx, server.Client(), dir, now)
		if path != old || !strings.HasPrefix(status, "stale: ") {
			t.Errorf("%s: old copy: %q %q", name, path, status)
		}
		for _, f := range []string{"geolite2-city.tar.gz", "geolite2-city.tar.gz.etag", "geolite2-city.tar.gz.checked"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				t.Errorf("%s: %s was kept", name, f)
			}
		}
		if path, status = p.fetchMaxMind(ctx, server.Client(), t.TempDir(), now); path != "" || !strings.HasPrefix(status, "skipped: ") {
			t.Errorf("%s: no old copy: %q %q", name, path, status)
		}
	}
}

func gzipBytes(s string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(s))
	gz.Close()
	return buf.Bytes()
}

func TestIP2LocationTokenNeverShown(t *testing.T) {
	dir := t.TempDir()
	zipped := zipIP2Location(t, testIP2LocationRow)
	status, requests := http.StatusOK, 0
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		q := r.URL.Query()
		if q.Get("token") != testToken || q.Get("file") != "DB11LITE" {
			t.Errorf("query %q", r.URL.RawQuery)
		}
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		w.Write(body)
	}))
	p := &Pipeline{IP2LocationToken: testToken, refURLs: &evalURLs{ip2location: server.URL + "/download/"}}
	ctx, client := context.Background(), server.Client()
	path := filepath.Join(dir, "ip2location-lite-db11.csv")
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)

	body = zipped
	if got, st := p.fetchIP2Location(ctx, client, dir, now); got != path || st != download.Downloaded || !looksLikeIP2LocationCSV(path) {
		t.Fatalf("download: %q %q", got, st)
	}
	// Within 7 days there is no request.
	status = http.StatusInternalServerError
	if got, st := p.fetchIP2Location(ctx, client, dir, now.Add(6*24*time.Hour)); got != path || st != download.Cached || requests != 1 {
		t.Errorf("age gate: %q %q after %d requests", got, st, requests)
	}

	later := now.Add(8 * 24 * time.Hour)
	status = http.StatusForbidden
	if got, st := p.fetchIP2Location(ctx, client, dir, later); got != path || st != "stale: IP2Location: HTTP 403" || requests != 2 {
		t.Errorf("HTTP 403: %q %q", got, st)
	}

	// HTTP 200 with a text (limit reached, bad token) is a failed download:
	// the old copy stays, the bad file goes, and the next run asks again.
	status = http.StatusOK
	body = []byte("THIS FILE CAN ONLY BE DOWNLOADED 5 TIMES PER DAY")
	got, st := p.fetchIP2Location(ctx, client, dir, later)
	if got != path || !strings.HasPrefix(st, "stale: ") || strings.Contains(st, testToken) || !looksLikeIP2LocationCSV(path) {
		t.Errorf("text body: %q %q", got, st)
	}
	for _, f := range []string{"ip2location-lite-db11.zip", "ip2location-lite-db11.zip.etag", "ip2location-lite-db11.zip.checked"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s was kept", f)
		}
	}
	// A second run within a day makes no request; after a day it asks again.
	if got, st = p.fetchIP2Location(ctx, client, dir, later.Add(23*time.Hour)); got != path || !strings.HasPrefix(st, "stale: ") || requests != 3 {
		t.Errorf("backoff: %q %q after %d requests", got, st, requests)
	}
	later = later.Add(25 * time.Hour)
	body = zipped
	if got, st = p.fetchIP2Location(ctx, client, dir, later); got != path || st != download.Downloaded || requests != 4 {
		t.Errorf("after the text: %q %q after %d requests", got, st, requests)
	}
	// A zip without the CSV is bad as well; without an old copy nothing is used.
	body = func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("README.TXT")
		w.Write([]byte("hi"))
		zw.Close()
		return buf.Bytes()
	}()
	if got, st = p.fetchIP2Location(ctx, client, t.TempDir(), later); got != "" || !strings.HasPrefix(st, "skipped: ") {
		t.Errorf("zip without CSV: %q %q", got, st)
	}

	// A refused connection: Go's own error holds the URL with the token.
	server.Close()
	_, rawErr := download.File(ctx, client, server.URL+"/download/?token="+testToken, filepath.Join(dir, "raw"))
	if rawErr == nil || !strings.Contains(rawErr.Error(), testToken) {
		t.Fatalf("the test needs an error that repeats the token, got %v", rawErr)
	}
	later = later.Add(8 * 24 * time.Hour)
	got, st = p.fetchIP2Location(ctx, client, dir, later)
	if got != path || strings.Contains(st, testToken) || st != "stale: "+errIP2Location.Error() {
		t.Errorf("connection refused with a copy: %q %q", got, st)
	}
	if strings.Contains(refError("IP2Location", errIP2Location, rawErr).Error(), testToken) {
		t.Error("refError repeats the token")
	}
	os.Remove(filepath.Join(dir, "ip2location-lite-db11.zip"))
	os.Remove(path)
	got, st = p.fetchIP2Location(ctx, client, dir, later)
	if got != "" || strings.Contains(st, testToken) || st != "skipped: "+errIP2Location.Error() {
		t.Errorf("connection refused, no copy: %q %q", got, st)
	}

	// No token, no request.
	p.IP2LocationToken = ""
	if got, st = p.fetchIP2Location(ctx, client, dir, later); got != "" || st != "skipped: IP2LOCATION_TOKEN is not set" {
		t.Errorf("no token: %q %q", got, st)
	}
}

func TestFetchDBIP(t *testing.T) {
	mmdb := "records\xab\xcd\xefMaxMind.com{metadata}"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(mmdb))
	zw.Close()
	var asked []string
	notOut := true // this month's file is not published yet
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if notOut && strings.Contains(r.URL.Path, "2026-12") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Last-Modified", time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC).Format(http.TimeFormat))
		if r.Header.Get("If-Modified-Since") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write(gz.Bytes())
	}))
	defer server.Close()
	p := &Pipeline{refURLs: &evalURLs{dbip: server.URL + "/free/dbip-city-lite-%s.mmdb.gz"}}
	dir := t.TempDir()
	ctx, now := context.Background(), time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC)

	path, status := p.fetchDBIP(ctx, server.Client(), dir, now)
	if data, _ := os.ReadFile(path); string(data) != mmdb || status != download.Downloaded {
		t.Fatalf("first: %q %q", path, status)
	}
	if want := []string{"/free/dbip-city-lite-2026-12.mmdb.gz", "/free/dbip-city-lite-2026-11.mmdb.gz"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("asked %v, want %v", asked, want)
	}
	if _, status = p.fetchDBIP(ctx, server.Client(), dir, now); status != download.Cached {
		t.Errorf("second: %q", status)
	}

	// The server failing keeps the copy in use; without a copy the reference is skipped.
	server.Close()
	if path, status = p.fetchDBIP(ctx, http.DefaultClient, dir, now); path == "" || !strings.HasPrefix(status, "stale: ") {
		t.Errorf("server down: %q %q", path, status)
	}
	if path, status = p.fetchDBIP(ctx, http.DefaultClient, t.TempDir(), now); path != "" || !strings.HasPrefix(status, "skipped: ") {
		t.Errorf("server down, no copy: %q %q", path, status)
	}
}

func TestWriteAtlasCSV(t *testing.T) {
	dump := `{"meta": {"count": 7}, "objects": [
		{"id": 1, "address_v4": "81.2.69.142", "country_code": "gb", "latitude": 51.5, "longitude": -0.12, "status": 1},
		{"id": 2, "address_v4": "81.2.69.143", "country_code": "GB", "status": 3},
		{"id": 3, "address_v4": null, "country_code": "DE", "status": 1},
		{"id": 4, "address_v4": "192.168.1.5", "country_code": "DE", "status": 1},
		{"id": 5, "address_v4": "100.64.1.1", "country_code": "DE", "status": 1},
		{"id": 6, "address_v4": "8.8.8.8", "country_code": "", "status": 1},
		{"id": 7, "address_v4": "2001:db8::1", "country_code": "DE", "status": 1},
		{"id": 8, "address_v4": "93.184.216.34", "country_code": "US", "latitude": null, "longitude": null, "status": 1}
	], "extra": 1}`
	var out bytes.Buffer
	n, err := writeAtlasCSV(strings.NewReader(dump), &out)
	if err != nil || n != 2 {
		t.Fatalf("%d probes, %v", n, err)
	}
	if want := "ip,country,lat,lon\n81.2.69.142,GB,51.5,-0.12\n93.184.216.34,US,,\n"; out.String() != want {
		t.Errorf("csv:\n%s", out.String())
	}
	for _, bad := range []string{`[]`, `{"objects": 5}`, `{"objects": [{"id": `, ``} {
		if _, err := writeAtlasCSV(strings.NewReader(bad), io.Discard); err == nil {
			t.Errorf("no error for %q", bad)
		}
	}
}

func TestLooksLikeMMDB(t *testing.T) {
	dir := t.TempDir()
	good, bad := filepath.Join(dir, "good"), filepath.Join(dir, "bad")
	os.WriteFile(good, append(bytes.Repeat([]byte{1}, 300<<10), "\xab\xcd\xefMaxMind.com{}"...), 0o640)
	os.WriteFile(bad, []byte("<html>401 Unauthorized</html>"), 0o640)
	if !looksLikeMMDB(good) || looksLikeMMDB(bad) || looksLikeMMDB(filepath.Join(dir, "missing")) {
		t.Error("looksLikeMMDB is wrong")
	}
}

// A fetchDBIP whose new download does not unpack drops the bad gzip but keeps
// using an older unpacked MMDB.
func TestFetchDBIPBadDownloadKeepsOldMMDB(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not gzip at all"))
	}))
	defer server.Close()
	p := &Pipeline{refURLs: &evalURLs{dbip: server.URL + "/dbip-%s.mmdb.gz"}}
	ctx, now := context.Background(), time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC)

	dir := t.TempDir()
	mmdb := filepath.Join(dir, "dbip-city-lite.mmdb")
	os.WriteFile(mmdb, []byte("old\xab\xcd\xefMaxMind.com{metadata}"), 0o640)
	path, status := p.fetchDBIP(ctx, server.Client(), dir, now)
	if path != mmdb || !strings.HasPrefix(status, "stale: ") {
		t.Errorf("old copy: %q %q", path, status)
	}
	for _, name := range []string{"dbip-city-lite.mmdb.gz", "dbip-city-lite.mmdb.gz.etag"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was kept", name)
		}
	}

	// An old file that is not an MMDB is no help.
	dir = t.TempDir()
	os.WriteFile(filepath.Join(dir, "dbip-city-lite.mmdb"), []byte("junk"), 0o640)
	if path, status = p.fetchDBIP(ctx, server.Client(), dir, now); path != "" || !strings.HasPrefix(status, "skipped: ") {
		t.Errorf("bad old copy: %q %q", path, status)
	}
	// Nor is a missing one.
	if path, status = p.fetchDBIP(ctx, server.Client(), t.TempDir(), now); path != "" || !strings.HasPrefix(status, "skipped: ") {
		t.Errorf("no old copy: %q %q", path, status)
	}
}

// evalPipeline is a Pipeline whose references are all unavailable and whose
// mmdbbuilder is the given shell script body.
func evalPipeline(t *testing.T, script string) *Pipeline {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	dir := t.TempDir()
	builder := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(builder, []byte("#!/bin/sh\n"+script+"\n"), 0o755)
	p := &Pipeline{DataDir: dir, MMDBBuilder: builder,
		refURLs:     &evalURLs{dbip: server.URL + "/dbip-%s", atlas: server.URL + "/atlas"},
		heldOutURLs: heldOutSet(testHeldOut)}
	os.MkdirAll(p.layersDir(), 0o750)
	writeGzip(t, filepath.Join(p.layersDir(), RIRLayer), "prefix,country,region,city,source\n")
	writeGzip(t, filepath.Join(p.layersDir(), FeedsLayer), "prefix,country,region,city,source\n"+
		"45.0.0.0/16,US,,,https://feed0.example/geo.csv\n46.0.0.0/24,FR,,Paris,https://feed1.example/geo.csv\n")
	return p
}

// Problems with the evaluation itself end up in Error, not in a task failure.
func TestEvaluateRecordsFailures(t *testing.T) {
	src := Source{DateTag: "2026-11-10"}
	for _, tc := range []struct{ name, script, want string }{
		{"builder fails", `echo boom >&2; exit 1`, "boom"},
		{"unreadable output", `case "$1" in eval) echo not-json;; esac`, "unreadable output"},
		// Postgres is not set up in the test: the insert fails.
		{"no Postgres", `case "$1" in eval) echo '{"atlas_country_pct": 97.5}';; esac`, "PGSQL is not set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := evalPipeline(t, tc.script)
			var logs []string
			p.Progress = func(s string) { logs = append(logs, s) }
			out, err := p.Evaluate(context.Background(), src, "unused.mmdb")
			if err != nil {
				t.Fatalf("Evaluate returned %v", err)
			}
			if !strings.Contains(out.Error, tc.want) || out.ID != 0 {
				t.Errorf("Error = %q, ID = %d, want an error with %q", out.Error, out.ID, tc.want)
			}
			if !strings.Contains(strings.Join(logs, "\n"), "eval: failed") {
				t.Errorf("failure not logged: %v", logs)
			}
		})
	}

	// Cancellation is still an error.
	p := evalPipeline(t, `echo boom >&2; exit 1`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Evaluate(ctx, src, "unused.mmdb"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// Publish refuses a failed evaluation (no scores row, or an error) unless
// forced; the refusal comes before any upload.
func TestPublishRefusesFailedEvaluation(t *testing.T) {
	p := &Pipeline{}
	src := Source{DateTag: "2026-11-10"}
	for name, ev := range map[string]EvalOutput{
		"error":     {Error: "mmdbbuilder eval: boom", ID: 7},
		"no row":    {Metrics: map[string]float64{"atlas_country_pct": 99}},
		"no scores": {},
	} {
		_, err := p.Publish(context.Background(), src, "x.mmdb", ev, false)
		if !errors.Is(err, ErrGate) || !strings.Contains(err.Error(), "evaluation failed: ") ||
			!strings.Contains(err.Error(), "force_publish") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Forced, it goes on (and here stops at the missing R2 setting).
	_, err := p.Publish(context.Background(), src, "x.mmdb", EvalOutput{Error: "boom"}, true)
	if errors.Is(err, ErrGate) || err == nil || !strings.Contains(err.Error(), "R2 is not set") {
		t.Errorf("forced: %v", err)
	}
}
