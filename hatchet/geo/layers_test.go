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

	"openipdata.org/hatchet/publish"
)

// Download starts from an empty layers directory, and every source writes its
// layer in the unified format under its numbered name.
func TestDownloadRebuildsLayers(t *testing.T) {
	var stats strings.Builder
	for i := 0; i < minStatsBlocks; i++ {
		fmt.Fprintf(&stats, "arin|US|ipv4|10.%d.%d.0|256|20100101|allocated|abc\n", i/256, i%256)
	}
	files := map[string]string{}
	for i := 0; i < minGeoCountries; i++ {
		code := string([]byte{byte('A' + i/26), byte('A' + i%26)})
		files["geo-ip-blocks-master/country/"+code+"/"+code+".json"] =
			fmt.Sprintf(`{"country": "Country %s", "countryCode": %q, "prefixes": {"ipv4": ["11.%d.0.0/16"]}}`, code, code, i)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(stats.String())) })
	mux.HandleFunc("/blocks", func(w http.ResponseWriter, r *http.Request) { w.Write(geoTarball(t, files)) })
	server := httptest.NewServer(mux)
	defer server.Close()

	p := &Pipeline{DataDir: t.TempDir(), stats: []struct{ Name, URL string }{{"arin", server.URL + "/stats"}}, geoBlocks: server.URL + "/blocks"}
	touchLayers(t, p, IXLayer, "55-old.csv.gz", RIRLayer)
	// There is no Postgres here: the run stops at the PeeringDB layer, after the two web layers.
	src, err := p.Download(context.Background(), time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "PGSQL") {
		t.Fatalf("err = %v", err)
	}
	names, _ := p.layerFiles()
	if !reflect.DeepEqual(names, []string{RIRLayer, IPverseLayer}) {
		t.Errorf("layer files = %v: nothing from the earlier run may stay", names)
	}
	if src.Rows[RIRLayer] != minStatsBlocks || src.Rows[IPverseLayer] != minGeoCountries {
		t.Errorf("rows = %v", src.Rows)
	}
	for name, row := range map[string]string{RIRLayer: "\n10.0.0.0/24,US,,,\n", IPverseLayer: "\n11.0.0.0/16,AA,,,\n"} {
		if got := readGzip(t, filepath.Join(p.layersDir(), name)); !strings.HasPrefix(got, "prefix,country,region,city,source\n") || !strings.Contains(got, row) {
			t.Errorf("%s = %q", name, got[:min(len(got), 80)])
		}
	}
	if _, err := os.Stat(filepath.Join(p.layersDir(), NamesFile)); err != nil {
		t.Error(err)
	}
}

// The R2 layers are downloaded as they are. A missing optional one (60-whois) is
// skipped with a warning and, like an old one, skips the health check ping; the
// required one (70-geofeeds) fails the run when missing.
func TestDownloadR2Layers(t *testing.T) {
	now := time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)
	layers := t.TempDir()
	type object struct {
		body     string
		uploaded time.Time
		err      error
	}
	var logs []string
	run := func(objects map[string]object) (Source, error) {
		logs = nil
		p := &Pipeline{Progress: func(s string) { logs = append(logs, s) }}
		src := Source{R2Uploaded: map[string]string{}}
		get := func(_ context.Context, key, path string) (time.Time, error) {
			o, ok := objects[key]
			if !ok {
				t.Errorf("unexpected key %q", key)
			}
			if o.err != nil {
				return time.Time{}, o.err
			}
			return o.uploaded, os.WriteFile(path, []byte(o.body), 0o640)
		}
		return src, p.downloadR2Layers(context.Background(), get, layers, now, &src)
	}
	notFound := fmt.Errorf("download r2://bucket/key: %w", publish.ErrNotFound)
	recent := now.Add(-time.Hour)

	// Both there and fresh: the files are copied as they are, nothing is skipped or stale.
	src, err := run(map[string]object{
		"internal/layers/60-whois.csv.gz":    {"whois bytes", recent, nil},
		"internal/layers/70-geofeeds.csv.gz": {"feeds bytes", recent, nil},
	})
	if err != nil || len(src.SkippedLayers) != 0 || len(src.StaleLayers) != 0 || len(src.R2Uploaded) != 2 ||
		src.R2Uploaded[WhoisLayer] != recent.Format(time.RFC3339) {
		t.Fatalf("fresh layers: %+v %v", src, err)
	}
	if data, _ := os.ReadFile(filepath.Join(layers, WhoisLayer)); string(data) != "whois bytes" {
		t.Errorf("%s = %q", WhoisLayer, data)
	}

	// No whois object: skipped with a warning, no ping; the geofeeds are still used.
	os.Remove(filepath.Join(layers, WhoisLayer))
	src, err = run(map[string]object{
		"internal/layers/60-whois.csv.gz":    {err: notFound},
		"internal/layers/70-geofeeds.csv.gz": {"feeds bytes", recent, nil},
	})
	if err != nil || !reflect.DeepEqual(src.SkippedLayers, []string{WhoisLayer}) || !reflect.DeepEqual(src.StaleLayers, []string{WhoisLayer}) {
		t.Fatalf("no whois layer: %+v %v", src, err)
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "WARNING") || !strings.Contains(logs[0], WhoisLayer) {
		t.Errorf("logs = %q", logs)
	}
	if _, err := os.Stat(filepath.Join(layers, WhoisLayer)); !os.IsNotExist(err) {
		t.Errorf("a file for a missing layer: %v", err)
	}

	// Older than 3 days: still used, with a warning, but no ping.
	old := now.Add(-rowsMaxAge - time.Hour)
	src, err = run(map[string]object{
		"internal/layers/60-whois.csv.gz":    {"whois bytes", recent, nil},
		"internal/layers/70-geofeeds.csv.gz": {"feeds bytes", old, nil},
	})
	if err != nil || len(src.SkippedLayers) != 0 || !reflect.DeepEqual(src.StaleLayers, []string{FeedsLayer}) || !strings.Contains(strings.Join(logs, "\n"), "WARNING") {
		t.Errorf("old geofeeds: %+v %v %q", src, err, logs)
	}
	// Exactly 3 days is not old.
	if src, err = run(map[string]object{
		"internal/layers/60-whois.csv.gz":    {"whois bytes", now.Add(-rowsMaxAge), nil},
		"internal/layers/70-geofeeds.csv.gz": {"feeds bytes", recent, nil},
	}); err != nil || len(src.StaleLayers) != 0 {
		t.Errorf("3 days old: %+v %v", src, err)
	}

	// The geofeeds are required: missing, or any other error, fails.
	for _, e := range []error{notFound, errors.New("boom")} {
		if _, err = run(map[string]object{
			"internal/layers/60-whois.csv.gz":    {"whois bytes", recent, nil},
			"internal/layers/70-geofeeds.csv.gz": {err: e},
		}); err == nil || !strings.Contains(err.Error(), FeedsLayer) {
			t.Errorf("geofeeds error %v: %v", e, err)
		}
	}
	// A whois error that is not "missing" fails too.
	if _, err = run(map[string]object{"internal/layers/60-whois.csv.gz": {err: errors.New("boom")}}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("whois error: %v", err)
	}
}
