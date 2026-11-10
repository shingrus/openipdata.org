package geo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
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
)

func layerRows(t *testing.T, fill func(w *csv.Writer) error) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "layer.csv")
	if _, err := writeLayer(path, fill); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	return string(data)
}

func TestWriteStatsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats")
	os.WriteFile(path, []byte("2|arin|20261109|4|19700101|20261109|-0500\n"+
		"arin|*|ipv4|*|3|summary\n"+
		"arin|US|ipv4|45.10.0.0|1024|20100101|allocated|abc\n"+
		"arin|ca|ipv4|45.20.0.0|768|20100101|assigned|abc\n"+
		"arin|US|ipv6|2600:1000::|28|20100101|allocated|abc\n"+
		"arin|US|ipv4|45.30.0.0|256|20100101|reserved|abc\n"+
		"arin||ipv4|45.40.0.0|256|20100101|allocated|abc\n"+
		"arin|ZZ|ipv4|45.50.0.0|256|20100101|assigned|abc\n"+
		"arin|EU|ipv4|45.60.0.0|256|20100101|assigned|abc\n"+
		"arin|ap|ipv4|45.70.0.0|256|20100101|assigned|abc\n"+
		"arin|US|asn|64512|1|20100101|allocated|abc\n"), 0o640)
	var blocks int
	got := layerRows(t, func(w *csv.Writer) (err error) {
		blocks, err = writeStatsRows(path, w)
		return err
	})
	// Blocks without a real country keep their row, with no country.
	want := "prefix,country,city\n45.10.0.0/22,US,\n45.20.0.0/23,CA,\n45.20.2.0/24,CA,\n2600:1000::/28,US,\n" +
		"45.40.0.0/24,,\n45.50.0.0/24,,\n45.60.0.0/24,,\n45.70.0.0/24,,\n"
	if got != want || blocks != 7 {
		t.Errorf("%d blocks, layer:\n%s", blocks, got)
	}
}

func geoTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestWriteGeoIPBlocksRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geo.tar.gz")
	os.WriteFile(path, geoTarball(t, map[string]string{
		"geo-ip-blocks-master/country/de/de.json": `{"country": "Germany", "countryCode": "DE",
			"prefixes": {"ipv4": ["63.176.0.0/13", "45.10.0.1/24"], "ipv6": ["2a00::/12"]}}`,
		"geo-ip-blocks-master/country/de/de-ipv4.txt": "63.176.0.0/13\n",
		"geo-ip-blocks-master/country/xx/xx.json":     `{"country": "Nowhere", "countryCode": "123", "prefixes": {"ipv4": ["1.0.0.0/8"]}}`,
		"geo-ip-blocks-master/README.md":              "readme",
	}), 0o640)
	var names map[string]string
	got := layerRows(t, func(w *csv.Writer) (err error) {
		names, _, err = writeGeoIPBlocksRows(path, w)
		return err
	})
	if got != "prefix,country,city\n63.176.0.0/13,DE,\n45.10.0.0/24,DE,\n" || !reflect.DeepEqual(names, map[string]string{"DE": "Germany"}) {
		t.Errorf("names %v, layer:\n%s", names, got)
	}
}

func TestASNPrefixes(t *testing.T) {
	got := asnPrefixes(`{"asn": 1, "metadata": {}, "prefixes": {"ipv4": ["45.10.0.1/24", "bad"], "ipv6": ["2a00::/32"]}}`)
	if fmt.Sprint(got) != "[45.10.0.0/24 2a00::/32]" {
		t.Errorf("prefixes = %v", got)
	}
	if asnPrefixes("not json") != nil {
		t.Error("prefixes from bad json")
	}
}

func TestFetch(t *testing.T) {
	// Older than staleFor: the fallback counts from the last check, not from this.
	modified := time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)
	fail := false
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		w.Write([]byte("data"))
	}))
	defer server.Close()
	ctx, client := context.Background(), server.Client()
	path := filepath.Join(t.TempDir(), "file")
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)

	if status, err := fetch(ctx, client, server.URL, path, now); err != nil || status != statusDownloaded {
		t.Fatalf("first: %s %v", status, err)
	}
	if info, _ := os.Stat(path); !info.ModTime().Equal(modified) {
		t.Errorf("mtime = %v", info.ModTime())
	}
	if status, err := fetch(ctx, client, server.URL, path, now); err != nil || status != statusCached {
		t.Errorf("second: %s %v", status, err)
	}
	fail = true
	if status, err := fetch(ctx, client, server.URL, path, now); err != nil || status != statusStale {
		t.Errorf("server down, recent copy: %s %v", status, err)
	}
	if _, err := fetch(ctx, client, server.URL, path, now.Add(staleFor)); err == nil {
		t.Error("server down, old copy: no error")
	}
	if data, _ := os.ReadFile(path); string(data) != "data" || requests != 4 {
		t.Errorf("file %q after %d requests", data, requests)
	}
}

func TestBuildMMDBArgs(t *testing.T) {
	// A stand-in mmdbbuilder that records its arguments and prints a summary.
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"+
		"echo 'layer rir.csv: 10 rows, 9 prefixes, 1 skipped'\n"+
		"echo 'layer geofeed-rows.csv.gz: 5 rows, 5 prefixes, 0 skipped'\n"+
		"echo 'wrote x (2 layers, 14 prefixes)'\n"), 0o755)
	p := &Pipeline{DataDir: dir, MMDBBuilder: script}
	out, err := p.BuildMMDB(context.Background(), Source{DateTag: "20261109"})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	layers := filepath.Join(dir, "geo", "layers")
	want := strings.Join([]string{"geo", "-out", filepath.Join(dir, "out", "geo", "20261109", "ip2geo-openipdata-20261109.mmdb"),
		"-names", filepath.Join(layers, NamesFile), "-same-country", PDBLayer, "-within-first", FeedsLayer, filepath.Join(layers, RIRLayer), filepath.Join(layers, BlocksLayer),
		filepath.Join(layers, PDBLayer), filepath.Join(layers, IXLayer), filepath.Join(layers, FeedsLayer)}, " ")
	if strings.TrimSpace(string(args)) != want {
		t.Errorf("args = %s\nwant   %s", args, want)
	}
	if !reflect.DeepEqual(out.Prefixes, map[string]int{"rir.csv": 9, "geofeed-rows.csv.gz": 5}) ||
		!reflect.DeepEqual(out.Skipped, map[string]int{"rir.csv": 1}) {
		t.Errorf("out = %+v", out)
	}
}

// Rows that left the source stay in public.asn; the query must skip the old ones.
func TestPDBQueryChecksSeenAt(t *testing.T) {
	if !strings.Contains(pdbQuery, "a.seen_at > now() - interval '7 days'") ||
		!strings.Contains(pdbQuery, "NOT EXISTS (SELECT 1 FROM public.asn WHERE seen_at IS NOT NULL)") {
		t.Error("pdbQuery does not filter on seen_at")
	}
}

// Global or anycast networks are in many places: their dominant city is left out.
func TestPDBQueryLeavesOutGlobalScope(t *testing.T) {
	if !strings.Contains(pdbQuery, "upper(btrim(coalesce(g.info_scope, ''))) <> 'GLOBAL'") {
		t.Error("pdbQuery does not leave out info_scope Global")
	}
}

// The exchange prefixes are read, never written, and only with a country.
func TestIXQuery(t *testing.T) {
	if !strings.Contains(ixQuery, "FROM public.geo_ix_pdb") || !strings.Contains(ixQuery, "length(btrim(country)) = 2") ||
		!strings.Contains(ixQuery, "btrim(coalesce(city, '')) <> ''") {
		t.Errorf("ixQuery = %s", ixQuery)
	}
}

// The exchange layer sits after the PeeringDB layer and before the geofeeds, and is no
// -same-country layer.
func TestLayerOrder(t *testing.T) {
	want := []string{RIRLayer, BlocksLayer, PDBLayer, IXLayer, FeedsLayer}
	if !reflect.DeepEqual(layerOrder, want) {
		t.Errorf("layerOrder = %v, want %v", layerOrder, want)
	}
	p := &Pipeline{DataDir: "/data"}
	args := strings.Join(p.geoArgs("out.mmdb", "/kept/feeds", Source{}), " ")
	if !strings.Contains(args, "-same-country "+PDBLayer+" ") || !strings.Contains(args, IXLayer) {
		t.Errorf("args = %s", args)
	}
	// A skipped exchange layer is left out; the others keep their order.
	args = strings.Join(p.geoArgs("out.mmdb", "/kept/feeds", Source{IXSkipped: true}), " ")
	if strings.Contains(args, IXLayer) || !strings.HasSuffix(args, "/data/geo/layers/"+PDBLayer+" /kept/feeds") {
		t.Errorf("skipped args = %s", args)
	}
}

// While public.geo_ix_pdb has no rows the layer is skipped, a stale file is removed, and a partial
// table still fails.
func TestDownloadIX(t *testing.T) {
	layers := t.TempDir()
	path := filepath.Join(layers, IXLayer)
	rowsOf := func(n int, err error) func(context.Context, *csv.Writer) (int, error) {
		return func(_ context.Context, w *csv.Writer) (int, error) {
			for i := 0; i < n; i++ {
				w.Write([]string{fmt.Sprintf("10.%d.0.0/24", i), "DE", "Berlin"})
			}
			return n, err
		}
	}
	run := func(write func(context.Context, *csv.Writer) (int, error)) (Source, []string, error) {
		var logs []string
		p := &Pipeline{writeIX: write, Progress: func(s string) { logs = append(logs, s) }}
		src := Source{Rows: map[string]int{}}
		err := p.downloadIX(context.Background(), layers, &src)
		return src, logs, err
	}

	src, _, err := run(rowsOf(minIXPrefixes, nil))
	if err != nil || src.IXSkipped || src.Rows[IXLayer] != minIXPrefixes {
		t.Fatalf("full table: %+v %v", src, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	// No rows: skipped with a warning, and the file from the run above is gone.
	src, logs, err := run(rowsOf(0, ErrNoIXRows))
	if err != nil || !src.IXSkipped || len(src.Rows) != 0 {
		t.Fatalf("empty table: %+v %v", src, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale ix.csv kept: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "WARNING") {
		t.Errorf("logs = %v", logs)
	}
	// Skipping again with no file is fine.
	if src, _, err = run(rowsOf(0, ErrNoIXRows)); err != nil || !src.IXSkipped {
		t.Errorf("second skip: %+v %v", src, err)
	}

	// A partial table (or one whose rows all lack a city) fails.
	for _, n := range []int{1, minIXPrefixes - 1, 0} {
		if src, _, err = run(rowsOf(n, nil)); err == nil || src.IXSkipped || !strings.Contains(err.Error(), "fewer than") {
			t.Errorf("%d rows: %+v %v", n, src, err)
		}
	}
	// Other errors fail too.
	if _, _, err = run(rowsOf(0, errors.New("boom"))); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error: %v", err)
	}
}
