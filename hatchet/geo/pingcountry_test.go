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

	"openipdata.org/hatchet/ping"
	"openipdata.org/hatchet/publish"
)

const pingFileHeader = "prefix,DE/Frankfurt,GB/London,US/New York\n"
const pingFileRow = "10.0.0.0/24,12.5,,30\n"

func TestDownloadPingCountry(t *testing.T) {
	now := time.Date(2026, 11, 26, 7, 0, 0, 0, time.UTC)
	p := &Pipeline{DataDir: t.TempDir()}
	geoDir, downloads := p.dir(), filepath.Join(p.dir(), "downloads")
	dst := filepath.Join(geoDir, pingCountryFile)
	body := "ping bytes"
	reset := func() {
		os.Remove(dst)
		os.MkdirAll(downloads, 0o750)
	}
	good := func(uploaded time.Time) map[string]fakeObject {
		return map[string]fakeObject{ping.RTTKey: {body, "e1", uploaded, nil}}
	}
	var fetched []string
	run := func(objects map[string]fakeObject) (Source, error) {
		fetched = nil
		src := Source{Rows: map[string]int{}, R2Uploaded: map[string]string{}}
		return src, p.downloadPingCountry(context.Background(), fakeFetch(objects, &fetched), downloads, now, &src)
	}
	skipped := func(src Source, err error, what string) {
		t.Helper()
		if err != nil || !reflect.DeepEqual(src.SkippedLayers, []string{PingCountrySource}) || len(src.StaleLayers) != 0 {
			t.Errorf("%s: %+v %v", what, src, err)
		}
		if _, err := os.Stat(dst); err == nil {
			t.Errorf("%s: a file for a skipped check", what)
		}
	}
	if pingCountryFlag != "-ping-country" || pingCountryFile != "hosting-rtt.csv.gz" || ping.RTTKey != "internal/ping/hosting-rtt.csv.gz" || PingCountrySource != "ping-country" || pingMaxAge != 45*24*time.Hour {
		t.Error("names")
	}
	os.RemoveAll(geoDir)
	reset()
	src, err := run(good(now.Add(-time.Hour)))
	if err != nil || len(src.SkippedLayers) != 0 || len(src.StaleLayers) != 0 || len(fetched) != 1 || src.R2Uploaded[PingCountrySource] != now.Add(-time.Hour).Format(time.RFC3339) {
		t.Fatalf("fresh: %+v %v %v", src, err, fetched)
	}
	if data, _ := os.ReadFile(dst); string(data) != body {
		t.Errorf("file = %q", data)
	}
	reset()
	src, err = run(good(now.Add(-time.Hour)))
	if err != nil || len(fetched) != 0 || !reflect.DeepEqual(src.R2Reused, []string{PingCountrySource}) {
		t.Errorf("unchanged: %+v %v %v", src, err, fetched)
	}
	if data, _ := os.ReadFile(dst); string(data) != body {
		t.Errorf("reused file = %q", data)
	}
	reset()
	if src, err = run(good(now.Add(-pingMaxAge))); err != nil || len(src.SkippedLayers) != 0 {
		t.Errorf("at the limit: %+v %v", src, err)
	}
	stale := func() { os.WriteFile(dst, []byte("old run"), 0o640) }
	reset()
	stale()
	src, err = run(good(now.Add(-pingMaxAge - time.Hour)))
	skipped(src, err, "old object")
	reset()
	stale()
	src, err = run(map[string]fakeObject{ping.RTTKey: {err: fmt.Errorf("download: stat: %w", publish.ErrNotFound)}})
	skipped(src, err, "missing object")
	reset()
	stale()
	src, err = run(map[string]fakeObject{ping.RTTKey: {err: errors.New("boom")}})
	skipped(src, err, "R2 error")
	if _, ok := layerRules[pingCountryFile]; ok || ValidSkip(pingCountryFile) {
		t.Error("the ping file is a layer")
	}
	// A cancelled context is an error, not a skip.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := func(context.Context, string, string, string) (publish.ObjectInfo, bool, error) {
		return publish.ObjectInfo{}, false, ctx.Err()
	}
	if err := p.downloadPingCountry(ctx, cancelled, downloads, now, &Source{R2Uploaded: map[string]string{}}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestPingCountryArgs(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	os.MkdirAll(p.dir(), 0o750)
	path := filepath.Join(p.dir(), pingCountryFile)
	check := func(geonames bool, wantArgs []string, wantStatus string) {
		t.Helper()
		args, status := p.pingCountryArgs(geonames)
		if !reflect.DeepEqual(args, wantArgs) || status != wantStatus {
			t.Errorf("geonames %v: %v %q, want %v %q", geonames, args, status, wantArgs, wantStatus)
		}
	}
	check(true, nil, "skipped: no ping times")
	writeGzip(t, path, pingFileHeader+pingFileRow)
	check(false, nil, "skipped: no GeoNames")
	check(true, []string{"-ping-country", path}, "on")
	writeGzip(t, path, pingFileHeader) // no data row
	check(true, nil, "skipped: ping times unreadable")
	writeGzip(t, path, "prefix,DE/Frankfurt,GB/London\n10.0.0.0/24,1,2\n") // 2 site columns
	check(true, nil, "skipped: ping times unreadable")
	writeGzip(t, path, "net,DE/Frankfurt,GB/London,US/New York\n10.0.0.0/24,1,2,3\n") // no prefix column
	check(true, nil, "skipped: ping times unreadable")
	os.WriteFile(path, []byte("not gzip"), 0o640)
	check(true, nil, "skipped: ping times unreadable")
}

func TestPingCountrySummary(t *testing.T) {
	m := pingCountrySummary.FindStringSubmatch("x\nping-country: 100 rows, 2 skipped, 40 /24s, 9 /24s changed, 12 parts changed\n")
	if m == nil || m[1] != "9" {
		t.Errorf("%v", m)
	}
	if pingCountrySummary.MatchString("rtt-country: 100 rows, 2 skipped, 40 /24s, 9 /24s changed, 12 parts changed") {
		t.Error("matches the RTT line")
	}
}

func TestPingCitySummary(t *testing.T) {
	m := pingCitySummary.FindStringSubmatch("x\nping-city: 120 times, 2 skipped, 31 /24s\n")
	if m == nil || m[1] != "31" {
		t.Errorf("%v", m)
	}
	if pingCitySummary.MatchString("ping-country: 100 rows, 2 skipped, 40 /24s, 9 /24s changed, 12 parts changed") || pingCitySummary.MatchString("ping-city: 4 site columns ignored") {
		t.Error("matches another line")
	}
	if !pingCitySkipped.MatchString("ping-city: skipped: fewer than 3 site columns that GeoNames places\n") || pingCitySkipped.MatchString("ping-city: 120 times, 2 skipped, 31 /24s") {
		t.Error("skipped line")
	}
}

// The flags are passed with the file only together with -geonames, after the RTT flags; the retry
// without GeoNames drops it; the output says what happened.
func TestBuildMMDBPingCountry(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> "+argsFile+"\n"+
		"for a in \"$@\"; do if [ -f \"$a\" ] && grep -q BAD \"$a\" 2>/dev/null; then echo \"error: $a: bad\" >&2; exit 1; fi; done\n"+
		"echo 'rtt-country: 100 rows, 2 skipped, 40 /24s, 7 /24s changed, 7 parts changed'\n"+
		"case \" $* \" in *' -ping-country '*) if [ -n \"$PING_SKIP\" ]; then echo 'ping-country: skipped: fewer than 3 site columns that GeoNames places'; else echo 'ping-country: 90 rows, 1 skipped, 30 /24s, 5 /24s changed, 6 parts changed'; fi;; esac\n"+
		"case \" $* \" in *' -ping-city '*) if [ -n \"$PING_SKIP\" ]; then echo 'ping-city: skipped: fewer than 3 site columns that GeoNames places'; else echo 'ping-city: 120 times, 2 skipped, 31 /24s'; echo 'ping-city: 4 site columns ignored'; fi;; esac\n"+
		"echo 'layer 10-rir.csv.gz: 10 rows, 9 prefixes, 0 skipped'\n"+
		"echo 'wrote x (1 layers, 9 prefixes)'\n"), 0o755)
	archive := zipBytes(t, geoNamesSample)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
	defer server.Close()
	p := &Pipeline{DataDir: dir, MMDBBuilder: script, refURLs: &evalURLs{geonames: server.URL + "/cities1000.zip"}}
	touchLayers(t, p, RIRLayer, NamesFile)
	geonames := filepath.Join(p.evalDir(), "cities1000.txt")
	pingPath := filepath.Join(p.dir(), pingCountryFile)
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
	if strings.Contains(args, "-ping-country") || strings.Contains(args, "-ping-city") || out.PingCountry != "skipped: no ping times" || out.PingCity != out.PingCountry || out.PingCountryChanged != 0 || out.PingCityNetworks != 0 {
		t.Errorf("no file: %s, %+v", args, out)
	}
	// A good file: passed after the RTT flags.
	writeGzip(t, rtt, rttFileHeader+"10.0.0.0/24,GB,London,8\n")
	writeGzip(t, pingPath, pingFileHeader+pingFileRow)
	out, args = build()
	if !strings.Contains(args, " -rtt-country "+rtt+" -rtt-city "+rtt+" -ping-country "+pingPath+" -ping-city "+pingPath) || out.PingCountry != "on" || out.PingCity != "on" || out.PingCountryChanged != 5 || out.PingCityNetworks != 31 || out.RTTCountry != "on" {
		t.Errorf("args %s\nout %+v", args, out)
	}
	// Without the RTT file the ping flag is still passed.
	os.Remove(rtt)
	out, args = build()
	if !strings.Contains(args, "-ping-country "+pingPath+" -ping-city "+pingPath) || strings.Contains(args, "-rtt-country") || out.PingCountry != "on" || out.PingCity != "on" {
		t.Errorf("ping only: %s, %+v", args, out)
	}
	writeGzip(t, rtt, rttFileHeader+"10.0.0.0/24,GB,London,8\n")
	// The builder skipped the check (sites not placed): the output says so.
	t.Setenv("PING_SKIP", "1")
	out, args = build()
	if !strings.Contains(args, "-ping-country") || out.PingCountry != "skipped: sites not placed" || out.PingCity != "skipped: sites not placed" || out.PingCountryChanged != 0 || out.PingCityNetworks != 0 {
		t.Errorf("builder skip: %s, %+v", args, out)
	}
	t.Setenv("PING_SKIP", "")
	// A bad file: not passed, the build succeeds.
	os.WriteFile(pingPath, []byte("not gzip"), 0o640)
	out, args = build()
	if strings.Contains(args, "-ping-country") || strings.Contains(args, "-ping-city") || out.PingCountry != "skipped: ping times unreadable" || out.PingCity != out.PingCountry || out.RTTCountry != "on" {
		t.Errorf("bad file: %s, %+v", args, out)
	}
	// The retry without GeoNames does not pass it.
	writeGzip(t, pingPath, pingFileHeader+pingFileRow)
	os.WriteFile(geonames, []byte("BAD\n"), 0o640)
	out, args = build()
	lines := strings.Split(strings.TrimSpace(args), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "-ping-country") || strings.Contains(lines[1], "-ping-country") || strings.Contains(lines[1], "-ping-city") || strings.Contains(lines[1], "-geonames") || out.PingCountry != "skipped: no GeoNames" || out.PingCity != "skipped: no GeoNames" {
		t.Errorf("retry: %s, %+v", args, out)
	}
}

// The holdout build has no GeoNames, so it passes no check.
func TestPrepareHoldoutNoPingCountry(t *testing.T) {
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
	writeGzip(t, filepath.Join(p.dir(), pingCountryFile), pingFileHeader+pingFileRow)
	eval := p.evalDir()
	os.MkdirAll(eval, 0o750)
	if _, _, _, err := p.prepareHoldout(context.Background(), eval); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	if strings.Contains(string(args), "-ping-country") || strings.Contains(string(args), "-geonames") {
		t.Errorf("args = %s", args)
	}
}
