package geo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/mlab"
	"openipdata.org/hatchet/publish"
)

const rttFileHeader = "prefix,country,city,rtt_ms\n"

func TestDownloadRTTCountry(t *testing.T) {
	now := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	p := &Pipeline{DataDir: t.TempDir()}
	geoDir, downloads := p.dir(), filepath.Join(p.dir(), "downloads")
	dst := filepath.Join(geoDir, rttCountryFile)
	body := "rtt bytes"
	reset := func() {
		os.Remove(dst)
		os.MkdirAll(downloads, 0o750)
	}
	good := func(uploaded time.Time) map[string]fakeObject {
		return map[string]fakeObject{mlab.SiteRTTKey: {body, "e1", uploaded, nil}}
	}
	var fetched []string
	run := func(objects map[string]fakeObject) (Source, error) {
		fetched = nil
		src := Source{Rows: map[string]int{}, R2Uploaded: map[string]string{}}
		return src, p.downloadRTTCountry(context.Background(), fakeFetch(objects, &fetched), downloads, now, &src)
	}
	skipped := func(src Source, err error, what string) {
		t.Helper()
		if err != nil || !reflect.DeepEqual(src.SkippedLayers, []string{RTTCountrySource}) || len(src.StaleLayers) != 0 {
			t.Errorf("%s: %+v %v", what, src, err)
		}
		if _, err := os.Stat(dst); err == nil {
			t.Errorf("%s: a file for a skipped check", what)
		}
	}
	if rttCountryFlag != "-rtt-country" || rttCountryFile != "speedtest-rtt.csv.gz" || mlab.SiteRTTKey != "internal/mlab/speedtest-rtt.csv.gz" {
		t.Error("names")
	}
	os.RemoveAll(geoDir)
	reset()
	src, err := run(good(now.Add(-time.Hour)))
	if err != nil || len(src.SkippedLayers) != 0 || len(src.StaleLayers) != 0 || len(fetched) != 1 {
		t.Fatalf("fresh: %+v %v %v", src, err, fetched)
	}
	if data, _ := os.ReadFile(dst); string(data) != body {
		t.Errorf("file = %q", data)
	}
	reset()
	src, err = run(good(now.Add(-time.Hour)))
	if err != nil || len(fetched) != 0 || !reflect.DeepEqual(src.R2Reused, []string{RTTCountrySource}) {
		t.Errorf("unchanged: %+v %v %v", src, err, fetched)
	}
	// The same age limit as the other M-Lab files: 45 days.
	reset()
	if src, err = run(good(now.Add(-mlabMaxAge))); err != nil || len(src.SkippedLayers) != 0 {
		t.Errorf("at the limit: %+v %v", src, err)
	}
	stale := func() { os.WriteFile(dst, []byte("old run"), 0o640) }
	reset()
	stale()
	src, err = run(good(now.Add(-mlabMaxAge - time.Hour)))
	skipped(src, err, "old object")
	reset()
	stale()
	src, err = run(map[string]fakeObject{mlab.SiteRTTKey: {err: fmt.Errorf("download: stat: %w", publish.ErrNotFound)}})
	skipped(src, err, "missing object")
	reset()
	stale()
	src, err = run(map[string]fakeObject{mlab.SiteRTTKey: {err: errors.New("boom")}})
	skipped(src, err, "R2 error")
	if _, ok := layerRules[rttCountryFile]; ok || ValidSkip(rttCountryFile) {
		t.Error("the RTT file is a layer")
	}
}

func TestRTTCountryArgs(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	os.MkdirAll(p.dir(), 0o750)
	path := filepath.Join(p.dir(), rttCountryFile)
	check := func(geonames bool, wantArgs []string, wantStatus string) {
		t.Helper()
		args, status := p.rttCountryArgs(geonames)
		if !reflect.DeepEqual(args, wantArgs) || status != wantStatus {
			t.Errorf("geonames %v: %v %q, want %v %q", geonames, args, status, wantArgs, wantStatus)
		}
	}
	check(true, nil, "skipped: no reply times")
	writeGzip(t, path, rttFileHeader+"10.0.0.0/24,GB,London,8\n")
	check(false, nil, "skipped: no GeoNames")
	check(true, []string{"-rtt-country", path}, "on")
	writeGzip(t, path, rttFileHeader) // no data row
	check(true, nil, "skipped: reply times unreadable")
	writeGzip(t, path, "prefix,country,city\n10.0.0.0/24,GB,London\n") // no rtt_ms column
	check(true, nil, "skipped: reply times unreadable")
	os.WriteFile(path, []byte("not gzip"), 0o640)
	check(true, nil, "skipped: reply times unreadable")
}

// The flag is passed with the file only together with -geonames; the retry without GeoNames
// drops it; the output says what happened.
func TestBuildMMDBRTTCountry(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> "+argsFile+"\n"+
		"for a in \"$@\"; do if [ -f \"$a\" ] && grep -q BAD \"$a\" 2>/dev/null; then echo \"error: $a: bad\" >&2; exit 1; fi; done\n"+
		"echo 'rtt-country: 100 rows, 2 skipped, 40 /24s, 7 /24s changed, 7 parts changed'\n"+
		"echo 'layer 10-rir.csv.gz: 10 rows, 9 prefixes, 0 skipped'\n"+
		"echo 'wrote x (1 layers, 9 prefixes)'\n"), 0o755)
	archive := zipBytes(t, geoNamesSample)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
	defer server.Close()
	p := &Pipeline{DataDir: dir, MMDBBuilder: script, refURLs: &evalURLs{geonames: server.URL + "/cities1000.zip"}}
	touchLayers(t, p, RIRLayer, NamesFile)
	geonames := filepath.Join(p.evalDir(), "cities1000.txt")
	rtt := filepath.Join(p.dir(), rttCountryFile)
	build := func() (BuildMMDBOutput, string) {
		os.Remove(argsFile)
		out, err := p.BuildMMDB(context.Background(), Source{DateTag: "20261109"})
		if err != nil {
			t.Fatal(err)
		}
		args, _ := os.ReadFile(argsFile)
		return out, string(args)
	}

	// No file: not passed.
	out, args := build()
	if strings.Contains(args, "-rtt-country") || out.RTTCountry != "skipped: no reply times" {
		t.Errorf("no file: %s, %q", args, out.RTTCountry)
	}
	// With GeoNames and a good file: passed after -fallback.
	writeGzip(t, rtt, rttFileHeader+"10.0.0.0/24,GB,London,8\n")
	out, args = build()
	if !strings.Contains(args, " -fallback -rtt-country "+rtt+" ") || out.RTTCountry != "on" || out.RTTCountryChanged != 7 {
		t.Errorf("args %s\nout %+v", args, out)
	}
	// A bad file: not passed, the build succeeds.
	os.WriteFile(rtt, []byte("not gzip"), 0o640)
	out, args = build()
	if strings.Contains(args, "-rtt-country") || out.RTTCountry != "skipped: reply times unreadable" {
		t.Errorf("bad file: %s, %q", args, out.RTTCountry)
	}
	// The retry without GeoNames does not pass it.
	writeGzip(t, rtt, rttFileHeader+"10.0.0.0/24,GB,London,8\n")
	os.WriteFile(geonames, []byte("BAD\n"), 0o640)
	out, args = build()
	lines := strings.Split(strings.TrimSpace(args), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "-rtt-country") || strings.Contains(lines[1], "-rtt-country") || strings.Contains(lines[1], "-geonames") || out.RTTCountry != "skipped: no GeoNames" {
		t.Errorf("retry: %s, %q", args, out.RTTCountry)
	}
}

// The holdout build has no GeoNames, so it passes no check.
func TestPrepareHoldoutNoRTTCountry(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"), 0o755)
	p := &Pipeline{DataDir: dir, MMDBBuilder: script, heldOutURLs: heldOutSet(testHeldOut)}
	layers := p.layersDir()
	os.MkdirAll(layers, 0o750)
	writeGzip(t, filepath.Join(layers, RIRLayer), feedHeader)
	feeds := feedHeader
	for i := 0; i < 3; i++ {
		feeds += fmt.Sprintf("10.0.%d.0/24,DE,,Berlin,https://feed0.example/geo.csv\n10.1.%d.0/24,FR,,Paris,https://feed1.example/geo.csv\n", i, i)
	}
	writeGzip(t, filepath.Join(layers, FeedsLayer), feeds)
	writeGzip(t, filepath.Join(p.dir(), rttCountryFile), rttFileHeader+"10.0.0.0/24,GB,London,8\n")
	eval := p.evalDir()
	os.MkdirAll(eval, 0o750)
	if _, _, _, err := p.prepareHoldout(context.Background(), eval); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	if strings.Contains(string(args), "-rtt-country") || strings.Contains(string(args), "-geonames") {
		t.Errorf("args = %s", args)
	}
}
