package geo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/mlab"
	"openipdata.org/hatchet/publish"
)

func TestDownloadMLabSites(t *testing.T) {
	now := time.Date(2026, 11, 20, 7, 0, 0, 0, time.UTC)
	root := t.TempDir()
	layers, downloads := filepath.Join(root, "layers"), filepath.Join(root, "downloads")
	reset := func() {
		os.RemoveAll(layers)
		os.MkdirAll(layers, 0o750)
		os.MkdirAll(downloads, 0o750)
	}
	body := "site layer bytes"
	good := func(uploaded time.Time) map[string]fakeObject {
		return map[string]fakeObject{mlab.SiteLayerKey: {body, "e1", uploaded, nil}}
	}
	var logs, fetched []string
	run := func(objects map[string]fakeObject) (Source, error) {
		logs, fetched = nil, nil
		p := &Pipeline{Progress: func(s string) { logs = append(logs, s) }}
		src := Source{Rows: map[string]int{}, R2Uploaded: map[string]string{}}
		return src, p.downloadMLabSites(context.Background(), fakeFetch(objects, &fetched), downloads, layers, now, &src)
	}
	skipped := func(src Source, err error, what string) {
		t.Helper()
		// A skipped layer never skips the health check ping (no StaleLayers).
		if err != nil || !reflect.DeepEqual(src.SkippedLayers, []string{MLabSitesLayer}) || len(src.StaleLayers) != 0 {
			t.Errorf("%s: %+v %v", what, src, err)
		}
		if _, err := os.Stat(filepath.Join(layers, MLabSitesLayer)); err == nil {
			t.Errorf("%s: a file for a skipped layer", what)
		}
		if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "WARNING") || !strings.Contains(logs[len(logs)-1], MLabSitesLayer) {
			t.Errorf("%s: logs %q", what, logs)
		}
	}

	reset()
	src, err := run(good(now.Add(-time.Hour)))
	if err != nil || len(src.SkippedLayers) != 0 || len(src.StaleLayers) != 0 || len(fetched) != 1 {
		t.Fatalf("fresh: %+v %v %v", src, err, fetched)
	}
	if data, _ := os.ReadFile(filepath.Join(layers, MLabSitesLayer)); string(data) != body {
		t.Errorf("layer = %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(downloads, MLabSitesLayer+".etag")); string(data) != "e1" {
		t.Errorf("saved ETag = %q", data)
	}
	reset()
	src, err = run(good(now.Add(-time.Hour)))
	if err != nil || len(fetched) != 0 || !reflect.DeepEqual(src.R2Reused, []string{MLabSitesLayer}) {
		t.Errorf("unchanged: %+v %v %v", src, err, fetched)
	}
	reset()
	if src, err = run(good(now.Add(-mlabMaxAge))); err != nil || len(src.SkippedLayers) != 0 {
		t.Errorf("45 days: %+v %v", src, err)
	}
	reset()
	src, err = run(good(now.Add(-mlabMaxAge - time.Hour)))
	skipped(src, err, "old object")
	reset()
	src, err = run(map[string]fakeObject{mlab.SiteLayerKey: {err: fmt.Errorf("download: stat: %w", publish.ErrNotFound)}})
	skipped(src, err, "missing object")
	reset()
	src, err = run(map[string]fakeObject{mlab.SiteLayerKey: {err: errors.New("boom")}})
	skipped(src, err, "R2 error")
	reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := func(context.Context, string, string, string) (publish.ObjectInfo, bool, error) {
		return publish.ObjectInfo{}, false, context.Canceled
	}
	p := &Pipeline{}
	if err := p.downloadMLabSites(ctx, cancelled, downloads, layers, now, &Source{Rows: map[string]int{}, R2Uploaded: map[string]string{}}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// The layer is optional, between 35-rdns-wide and 36-mlab, and has -same-country.
func TestMLabSitesLayerRules(t *testing.T) {
	if MLabSitesLayer != "35-speedtest-site.csv.gz" || layerRules[MLabSitesLayer] != "-same-country" {
		t.Errorf("rule %q", layerRules[MLabSitesLayer])
	}
	if !ValidSkip(MLabSitesLayer) || MLabSitesLayer <= RDNSWideLayer || MLabSitesLayer >= MLabLayer || MLabSitesLayer <= PDBLayer {
		t.Errorf("layer order or skip list")
	}
	p := &Pipeline{DataDir: t.TempDir()}
	touchLayers(t, p, RIRLayer, PDBLayer, RDNSWideLayer, MLabSitesLayer, MLabLayer, FeedsLayer)
	got, err := p.geoArgs("o.mmdb", nil)
	if err != nil {
		t.Fatal(err)
	}
	if line := strings.Join(got, " "); !strings.Contains(line, "-same-country "+PDBLayer+","+RDNSWideLayer+","+MLabSitesLayer+","+MLabLayer+" ") {
		t.Errorf("args: %s", line)
	}
	src := Source{}
	if err := p.SkipLayers(&src, []string{MLabSitesLayer}); err != nil || !src.skipped(MLabSitesLayer) {
		t.Errorf("skip: %v %+v", err, src)
	}
}

// The holdout build passes the layer as it is.
func TestPrepareHoldoutMLabSites(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"), 0o755)
	p := &Pipeline{DataDir: dir, MMDBBuilder: script, heldOutURLs: heldOutSet(testHeldOut)}
	layers := p.layersDir()
	os.MkdirAll(layers, 0o750)
	writeGzip(t, filepath.Join(layers, RIRLayer), feedHeader)
	writeGzip(t, filepath.Join(layers, MLabSitesLayer), feedHeader+"10.0.9.0/24,DE,,Berlin,speedtest-site\n")
	feeds := feedHeader
	for i := 0; i < 3; i++ {
		feeds += fmt.Sprintf("10.0.%d.0/24,DE,,Berlin,https://feed0.example/geo.csv\n10.1.%d.0/24,FR,,Paris,https://feed1.example/geo.csv\n", i, i)
	}
	writeGzip(t, filepath.Join(layers, FeedsLayer), feeds)
	eval := p.evalDir()
	os.MkdirAll(eval, 0o750)
	mmdb, _, status, err := p.prepareHoldout(context.Background(), eval)
	if err != nil || mmdb == "" {
		t.Fatalf("%q %q %v", mmdb, status, err)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), filepath.Join(layers, MLabSitesLayer)) || !strings.Contains(string(args), "-same-country "+MLabSitesLayer) {
		t.Errorf("args = %s", args)
	}
}
