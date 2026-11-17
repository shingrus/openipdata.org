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

	"openipdata.org/hatchet/geofeeds"
	"openipdata.org/hatchet/publish"
)

func TestDownloadWhoisText(t *testing.T) {
	now := time.Date(2026, 11, 16, 7, 0, 0, 0, time.UTC)
	root := t.TempDir()
	layers, downloads := filepath.Join(root, "layers"), filepath.Join(root, "downloads")
	reset := func() {
		os.RemoveAll(layers)
		os.MkdirAll(layers, 0o750)
		os.MkdirAll(downloads, 0o750)
	}
	body := "layer bytes"
	good := func(uploaded time.Time) map[string]fakeObject {
		return map[string]fakeObject{geofeeds.WhoisTextKey: {body, "e1", uploaded, nil}}
	}
	var logs, fetched []string
	run := func(objects map[string]fakeObject) (Source, error) {
		logs, fetched = nil, nil
		p := &Pipeline{Progress: func(s string) { logs = append(logs, s) }}
		src := Source{Rows: map[string]int{}, R2Uploaded: map[string]string{}}
		return src, p.downloadWhoisText(context.Background(), fakeFetch(objects, &fetched), downloads, layers, now, &src)
	}
	skipped := func(src Source, err error, what string, stale bool) {
		t.Helper()
		// A missing object or an R2 error never skips the health check ping (no StaleLayers);
		// an object that is too old does.
		if err != nil || !reflect.DeepEqual(src.SkippedLayers, []string{WhoisTextLayer}) || (len(src.StaleLayers) == 1) != stale || len(src.StaleLayers) > 1 {
			t.Errorf("%s: %+v %v", what, src, err)
		}
		if _, err := os.Stat(filepath.Join(layers, WhoisTextLayer)); err == nil {
			t.Errorf("%s: a file for a skipped layer", what)
		}
		if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "WARNING") || !strings.Contains(logs[len(logs)-1], WhoisTextLayer) {
			t.Errorf("%s: logs %q", what, logs)
		}
	}

	// A fresh object is put in as it is, with its ETag saved.
	reset()
	src, err := run(good(now.Add(-time.Hour)))
	if err != nil || len(src.SkippedLayers) != 0 || len(src.StaleLayers) != 0 || len(fetched) != 1 {
		t.Fatalf("fresh: %+v %v %v", src, err, fetched)
	}
	if data, _ := os.ReadFile(filepath.Join(layers, WhoisTextLayer)); string(data) != body {
		t.Errorf("layer = %q", data)
	}
	// Unchanged: the saved copy is used, not downloaded again.
	reset()
	src, err = run(good(now.Add(-time.Hour)))
	if err != nil || len(fetched) != 0 || !reflect.DeepEqual(src.R2Reused, []string{WhoisTextLayer}) {
		t.Errorf("unchanged: %+v %v %v", src, err, fetched)
	}
	if data, _ := os.ReadFile(filepath.Join(layers, WhoisTextLayer)); string(data) != body {
		t.Errorf("reused layer = %q", data)
	}
	// Older than rowsMaxAge: still used, but stale (no ping), with one warning.
	reset()
	src, err = run(good(now.Add(-5 * 24 * time.Hour)))
	if err != nil || len(src.SkippedLayers) != 0 || !reflect.DeepEqual(src.StaleLayers, []string{WhoisTextLayer}) {
		t.Errorf("5 days: %+v %v", src, err)
	}
	if data, _ := os.ReadFile(filepath.Join(layers, WhoisTextLayer)); string(data) != body {
		t.Errorf("5 days: layer = %q", data)
	}
	if n := strings.Count(strings.Join(logs, "\n"), "WARNING"); n != 1 {
		t.Errorf("5 days: %d warnings: %q", n, logs)
	}
	// Exactly 14 days is used (and stale); older is skipped (and stale).
	reset()
	if src, err = run(good(now.Add(-whoisTextMaxAge))); err != nil || len(src.SkippedLayers) != 0 || len(src.StaleLayers) != 1 {
		t.Errorf("14 days: %+v %v", src, err)
	}
	reset()
	src, err = run(good(now.Add(-15 * 24 * time.Hour)))
	skipped(src, err, "15 days", true)
	// Exactly rowsMaxAge is not stale.
	reset()
	if src, err = run(good(now.Add(-rowsMaxAge))); err != nil || len(src.SkippedLayers) != 0 || len(src.StaleLayers) != 0 {
		t.Errorf("3 days: %+v %v", src, err)
	}
	// Missing, or any other R2 error, skips too.
	reset()
	src, err = run(map[string]fakeObject{geofeeds.WhoisTextKey: {err: fmt.Errorf("download: stat: %w", publish.ErrNotFound)}})
	skipped(src, err, "missing object", false)
	reset()
	src, err = run(map[string]fakeObject{geofeeds.WhoisTextKey: {err: errors.New("boom")}})
	skipped(src, err, "R2 error", false)
}

// The layer is optional, between 37-whois-arin and 38-asn-geofeed, has -same-country and is not one of
// the layers sync-geofeeds feeds the holdout from.
func TestWhoisTextLayerRules(t *testing.T) {
	if WhoisTextLayer != "37-whois-text.csv.gz" || layerRules[WhoisTextLayer] != "-same-country" {
		t.Errorf("rule %q", layerRules[WhoisTextLayer])
	}
	if !ValidSkip(WhoisTextLayer) || WhoisTextLayer <= MLabLayer || WhoisTextLayer >= ASNFeedLayer {
		t.Errorf("layer order or skip list")
	}
	for _, l := range r2Layers {
		if l.Name == WhoisTextLayer {
			t.Errorf("the layer is optional, so it is not in r2Layers")
		}
	}
	p := &Pipeline{DataDir: t.TempDir()}
	touchLayers(t, p, RIRLayer, MLabLayer, WhoisTextLayer, FeedsLayer)
	got, err := p.geoArgs("o.mmdb", nil)
	if err != nil {
		t.Fatal(err)
	}
	if line := strings.Join(got, " "); !strings.Contains(line, "-same-country "+MLabLayer+","+WhoisTextLayer+" ") {
		t.Errorf("args: %s", line)
	}
}
