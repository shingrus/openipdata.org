package geo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/arin"
	"openipdata.org/hatchet/publish"
)

func TestDownloadHolderCities(t *testing.T) {
	now := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	root := t.TempDir()
	p := &Pipeline{DataDir: root}
	geoDir, layers, downloads := p.dir(), p.layersDir(), filepath.Join(p.dir(), "downloads")
	body := "holder bytes"
	dst := filepath.Join(geoDir, holderCitiesFile)
	// reset removes the copy in the geo directory; the saved copy in downloads stays unless fresh.
	reset := func() {
		os.Remove(dst)
		os.MkdirAll(layers, 0o750)
		os.MkdirAll(downloads, 0o750)
	}
	good := func(uploaded time.Time) map[string]fakeObject {
		return map[string]fakeObject{arin.HolderCitiesKey: {body, "e1", uploaded, nil}}
	}
	var logs, fetched []string
	run := func(objects map[string]fakeObject) (Source, error) {
		logs, fetched = nil, nil
		p.Progress = func(s string) { logs = append(logs, s) }
		src := Source{Rows: map[string]int{}, R2Uploaded: map[string]string{}}
		return src, p.downloadHolderCities(context.Background(), fakeFetch(objects, &fetched), downloads, now, &src)
	}
	skipped := func(src Source, err error, what string) {
		t.Helper()
		if err != nil || !reflect.DeepEqual(src.SkippedLayers, []string{HolderCitySource}) || len(src.StaleLayers) != 0 {
			t.Errorf("%s: %+v %v", what, src, err)
		}
		if _, err := os.Stat(dst); err == nil {
			t.Errorf("%s: a file for a skipped guess", what)
		}
		if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "WARNING") {
			t.Errorf("%s: logs %q", what, logs)
		}
	}
	if HolderCitySource != "85-holder-city" || holdersFlag != "-holders" || holderCitiesFile != "holder-cities.csv.gz" {
		t.Error("names")
	}

	// A fresh object is copied next to the geo directory's files, not into the layers.
	os.RemoveAll(geoDir)
	reset()
	src, err := run(good(now.Add(-time.Hour)))
	if err != nil || len(src.SkippedLayers) != 0 || len(src.StaleLayers) != 0 || len(fetched) != 1 {
		t.Fatalf("fresh: %+v %v %v", src, err, fetched)
	}
	if data, _ := os.ReadFile(dst); string(data) != body {
		t.Errorf("file = %q", data)
	}
	if _, err := os.Stat(filepath.Join(layers, holderCitiesFile)); err == nil {
		t.Error("the file is in the layers directory")
	}
	// Unchanged: the saved copy is used.
	reset()
	src, err = run(good(now.Add(-time.Hour)))
	if err != nil || len(fetched) != 0 || !reflect.DeepEqual(src.R2Reused, []string{HolderCitySource}) {
		t.Errorf("unchanged: %+v %v %v", src, err, fetched)
	}
	if data, _ := os.ReadFile(dst); string(data) != body {
		t.Errorf("reused file = %q", data)
	}
	// Skips, also when a copy of an earlier run is there.
	stale := func() { os.WriteFile(dst, []byte("old run"), 0o640) }
	reset()
	stale()
	src, err = run(good(now.Add(-holderCitiesMaxAge - time.Hour)))
	skipped(src, err, "old object")
	reset()
	stale()
	src, err = run(map[string]fakeObject{arin.HolderCitiesKey: {err: fmt.Errorf("download: stat: %w", publish.ErrNotFound)}})
	skipped(src, err, "missing object")
	reset()
	stale()
	src, err = run(map[string]fakeObject{arin.HolderCitiesKey: {err: errors.New("boom")}})
	skipped(src, err, "R2 error")
	// A cancelled run fails.
	reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := func(context.Context, string, string, string) (publish.ObjectInfo, bool, error) {
		return publish.ObjectInfo{}, false, context.Canceled
	}
	if err := p.downloadHolderCities(ctx, cancelled, downloads, now, &Source{Rows: map[string]int{}, R2Uploaded: map[string]string{}}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	// Not a layer: no rule, not optional.
	if _, ok := layerRules[holderCitiesFile]; ok || ValidSkip(holderCitiesFile) {
		t.Error("the holder blocks file is a layer")
	}
}

func TestHolderArgs(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	os.MkdirAll(p.dir(), 0o750)
	path := filepath.Join(p.dir(), holderCitiesFile)
	check := func(asnMain bool, wantArgs []string, wantStatus string) {
		t.Helper()
		args, status := p.holderArgs(asnMain)
		if !reflect.DeepEqual(args, wantArgs) || status != wantStatus {
			t.Errorf("asnMain %v: %v %q, want %v %q", asnMain, args, status, wantArgs, wantStatus)
		}
	}
	check(true, nil, "skipped: no holder blocks")
	writeGzip(t, path, feedHeader+"10.0.0.0/18,US,OR,Portland,holder-city:arin\n")
	check(false, nil, "skipped: no ASN prefixes")
	check(true, []string{"-holders", path}, "on")
	writeGzip(t, path, feedHeader) // no data row
	check(true, nil, "skipped: holder blocks unreadable")
	writeGzip(t, path, "prefix,country\n10.0.0.0/18,US\n") // no city column
	check(true, nil, "skipped: holder blocks unreadable")
	os.WriteFile(path, []byte("not gzip"), 0o640)
	check(true, nil, "skipped: holder blocks unreadable")
	// A copy cut short after its first rows: the builder would fail on it, so it is not passed.
	var rows strings.Builder
	rows.WriteString(feedHeader)
	for i := range 5000 {
		fmt.Fprintf(&rows, "10.%d.%d.0/24,US,OR,Portland,holder-city:arin\n", i/256, i%256)
	}
	writeGzip(t, path, rows.String())
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	check(true, []string{"-holders", path}, "on")
	os.WriteFile(path, whole[:len(whole)/2], 0o640)
	check(true, nil, "skipped: holder blocks unreadable")
}

// The flag is passed with the file when the ASN main fill is on and the file is good, also by
// the retry without GeoNames; never otherwise.
func TestBuildMMDBHolderCity(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> "+argsFile+"\n"+
		"for a in \"$@\"; do if [ -f \"$a\" ] && grep -q BAD \"$a\" 2>/dev/null; then echo \"error: $a: bad\" >&2; exit 1; fi; done\n"+
		"echo 'layer 10-rir.csv.gz: 10 rows, 9 prefixes, 0 skipped'\n"+
		"echo 'layer 85-holder-city: 3 rows, 8 prefixes, 0 skipped'\n"+
		"echo 'wrote x (2 layers, 13 prefixes)'\n"), 0o755)
	archive := zipBytes(t, geoNamesSample)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
	defer server.Close()
	p := &Pipeline{DataDir: dir, MMDBBuilder: script, refURLs: &evalURLs{geonames: server.URL + "/cities1000.zip"}}
	touchLayers(t, p, RIRLayer, NamesFile)
	geonames := filepath.Join(p.evalDir(), "cities1000.txt")
	prefixes := filepath.Join(p.dir(), asnPrefixesFile)
	holders := filepath.Join(p.dir(), holderCitiesFile)
	build := func() (BuildMMDBOutput, string) {
		os.Remove(argsFile)
		out, err := p.BuildMMDB(context.Background(), Source{DateTag: "20261109"})
		if err != nil {
			t.Fatal(err)
		}
		args, _ := os.ReadFile(argsFile)
		return out, string(args)
	}

	// The file without the ASN prefixes: not passed.
	writeGzip(t, holders, feedHeader+"10.0.0.0/18,US,OR,Portland,holder-city:arin\n")
	out, args := build()
	if strings.Contains(args, "-holders") || out.HolderCity != "skipped: no ASN prefixes" {
		t.Errorf("no ASN prefixes: %s, %q", args, out.HolderCity)
	}
	// With both: passed after -asn-main and before -fallback.
	writeAnnouncedFile(prefixes, []asnPrefix{{1, netip.MustParsePrefix("10.0.0.0/20")}})
	out, args = build()
	want := " -geonames " + geonames + " -asn-main " + prefixes + " -holders " + holders + " -fallback "
	if !strings.Contains(args, want) || out.HolderCity != "on" || out.Prefixes[HolderCitySource] != 8 {
		t.Errorf("args %s\nwant %s\nout %+v", args, want, out)
	}
	// No file: not passed.
	os.Remove(holders)
	out, args = build()
	if strings.Contains(args, "-holders") || out.HolderCity != "skipped: no holder blocks" {
		t.Errorf("no file: %s, %q", args, out.HolderCity)
	}
	// A bad file: not passed, the build succeeds.
	os.WriteFile(holders, []byte("not gzip"), 0o640)
	out, args = build()
	if strings.Contains(args, "-holders") || out.HolderCity != "skipped: holder blocks unreadable" {
		t.Errorf("bad file: %s, %q", args, out.HolderCity)
	}
	// The retry without GeoNames passes it too.
	writeGzip(t, holders, feedHeader+"10.0.0.0/18,US,OR,Portland,holder-city:arin\n")
	os.WriteFile(geonames, []byte("BAD\n"), 0o640)
	out, args = build()
	lines := strings.Split(strings.TrimSpace(args), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "-geonames") || strings.Contains(lines[1], "-geonames") ||
		!strings.Contains(lines[0], "-holders "+holders) || !strings.Contains(lines[1], "-holders "+holders+" -fallback ") || out.HolderCity != "on" {
		t.Errorf("retry: %s", args)
	}
}

// The holdout build passes the file as it is.
func TestPrepareHoldoutHolderCity(t *testing.T) {
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
	holders := filepath.Join(p.dir(), holderCitiesFile)
	writeGzip(t, holders, feedHeader+"10.0.0.0/18,US,OR,Portland,holder-city:arin\n")
	writeAnnouncedFile(filepath.Join(p.dir(), asnPrefixesFile), []asnPrefix{{1, netip.MustParsePrefix("10.0.0.0/20")}})
	eval := p.evalDir()
	os.MkdirAll(eval, 0o750)
	mmdb, _, status, err := p.prepareHoldout(context.Background(), eval)
	if err != nil || mmdb == "" {
		t.Fatalf("%q %q %v", mmdb, status, err)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "-holders "+holders+" -fallback") {
		t.Errorf("args = %s", args)
	}
}
