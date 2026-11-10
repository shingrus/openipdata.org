package geo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		"-names", filepath.Join(layers, NamesFile), "-same-country", PDBLayer, "-within-first", FeedsLayer, "-first-country", HostingLayer, filepath.Join(layers, RIRLayer), filepath.Join(layers, BlocksLayer),
		filepath.Join(layers, PDBLayer), filepath.Join(layers, HostingLayer), filepath.Join(layers, IXLayer), filepath.Join(layers, FeedsLayer)}, " ")
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

// Global-scope networks keep their dominant city: leaving them out halved the
// holdout city score.
func TestPDBQueryKeepsGlobalScope(t *testing.T) {
	if strings.Contains(pdbQuery, "info_scope") {
		t.Error("pdbQuery filters on info_scope")
	}
}

// The exchange prefixes are read, never written, and only with a country.
func TestIXQuery(t *testing.T) {
	if !strings.Contains(ixQuery, "FROM public.geo_ix_pdb") || !strings.Contains(ixQuery, "length(btrim(country)) = 2") ||
		!strings.Contains(ixQuery, "btrim(coalesce(city, '')) <> ''") {
		t.Errorf("ixQuery = %s", ixQuery)
	}
}

// The hosting query is read only and narrow: Content networks that are not Global
// scope, with all facilities in one city, and the same seen_at rule as the PeeringDB
// query. It does not compare the registration country.
func TestHostingQuery(t *testing.T) {
	for _, part := range []string{
		"FROM public.asn a JOIN public.asn_geo_pdb g USING (asn)",
		"g.info_type = 'Content'",
		"coalesce(g.info_scope, '') <> 'Global'",
		"g.fac_count >= 1",
		"btrim(coalesce(g.fac_city, '')) <> ''",
		"length(btrim(coalesce(g.fac_country, ''))) = 2",
		"upper(btrim(g.fac_country)), btrim(g.fac_city)",
		"a.seen_at > now() - interval '7 days'",
		"NOT EXISTS (SELECT 1 FROM public.asn WHERE seen_at IS NOT NULL)",
	} {
		if !strings.Contains(hostingQuery, part) {
			t.Errorf("hostingQuery lacks %q:\n%s", part, hostingQuery)
		}
	}
	if strings.Contains(hostingQuery, "a.country") || !strings.Contains(hostingQuery, seenAtFilter) || !strings.Contains(pdbQuery, seenAtFilter) {
		t.Errorf("hostingQuery = %s", hostingQuery)
	}
	for _, bad := range []string{"INSERT", "UPDATE", "ALTER", "DELETE"} {
		if strings.Contains(strings.ToUpper(hostingQuery), bad) {
			t.Errorf("hostingQuery contains %s", bad)
		}
	}
}

// The hosting and exchange layers sit after the PeeringDB layer and before the geofeeds,
// and are no -same-country layers. The hosting layer is a -first-country layer.
func TestLayerOrder(t *testing.T) {
	want := []string{RIRLayer, BlocksLayer, PDBLayer, HostingLayer, IXLayer, FeedsLayer}
	if !reflect.DeepEqual(layerOrder, want) {
		t.Errorf("layerOrder = %v, want %v", layerOrder, want)
	}
	p := &Pipeline{DataDir: "/data"}
	args := strings.Join(p.geoArgs("out.mmdb", "/kept/feeds", Source{}), " ")
	layers := "/data/geo/layers/"
	if !strings.Contains(args, "-same-country "+PDBLayer+" ") || !strings.Contains(args, "-within-first "+FeedsLayer+" ") ||
		!strings.Contains(args, "-first-country "+HostingLayer+" ") ||
		!strings.HasSuffix(args, layers+PDBLayer+" "+layers+HostingLayer+" "+layers+IXLayer+" /kept/feeds") {
		t.Errorf("args = %s", args)
	}
	// Skipped optional layers are left out; the others keep their order.
	for _, c := range []struct {
		skipped []string
		suffix  string
	}{
		{[]string{IXLayer}, layers + PDBLayer + " " + layers + HostingLayer + " /kept/feeds"},
		{[]string{HostingLayer}, layers + PDBLayer + " " + layers + IXLayer + " /kept/feeds"},
		{[]string{HostingLayer, IXLayer}, layers + PDBLayer + " /kept/feeds"},
	} {
		args = strings.Join(p.geoArgs("out.mmdb", "/kept/feeds", Source{SkippedLayers: c.skipped}), " ")
		if !strings.HasSuffix(args, c.suffix) || strings.Contains(args, layers+c.skipped[0]) {
			t.Errorf("skipped %v: args = %s", c.skipped, args)
		}
		// Without the hosting layer there is no flag for it.
		if hostingSkipped := slices.Contains(c.skipped, HostingLayer); hostingSkipped == strings.Contains(args, "-first-country") {
			t.Errorf("skipped %v: -first-country in args = %s", c.skipped, args)
		}
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
		err := p.downloadOptional(context.Background(), layers, &src, IXLayer, minIXPrefixes, "PeeringDB exchange prefixes", "hint", p.writeIX)
		return src, logs, err
	}

	src, _, err := run(rowsOf(minIXPrefixes, nil))
	if err != nil || src.skipped(IXLayer) || src.Rows[IXLayer] != minIXPrefixes {
		t.Fatalf("full table: %+v %v", src, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	// No rows: skipped with a warning, and the file from the run above is gone.
	src, logs, err := run(rowsOf(0, ErrNoRows))
	if err != nil || !src.skipped(IXLayer) || len(src.Rows) != 0 {
		t.Fatalf("empty table: %+v %v", src, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale ix.csv kept: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "WARNING") {
		t.Errorf("logs = %v", logs)
	}
	// Skipping again with no file is fine.
	if src, _, err = run(rowsOf(0, ErrNoRows)); err != nil || !src.skipped(IXLayer) {
		t.Errorf("second skip: %+v %v", src, err)
	}

	// A partial table (or one whose rows all lack a city) fails.
	for _, n := range []int{1, minIXPrefixes - 1, 0} {
		if src, _, err = run(rowsOf(n, nil)); err == nil || src.skipped(IXLayer) || !strings.Contains(err.Error(), "fewer than") {
			t.Errorf("%d rows: %+v %v", n, src, err)
		}
	}
	// Other errors fail too.
	if _, _, err = run(rowsOf(0, errors.New("boom"))); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error: %v", err)
	}
}

// The hosting layer has no minimum: any number of rows is used, and none at all
// (or a source that does not exist yet) skips it with a warning and removes a stale file.
func TestDownloadHosting(t *testing.T) {
	layers := t.TempDir()
	path := filepath.Join(layers, HostingLayer)
	run := func(n int, werr error) (Source, []string, error) {
		var logs []string
		p := &Pipeline{Progress: func(s string) { logs = append(logs, s) }}
		src := Source{Rows: map[string]int{}}
		err := p.downloadOptional(context.Background(), layers, &src, HostingLayer, 0, "PeeringDB hosting networks", "hint",
			func(_ context.Context, w *csv.Writer) (int, error) {
				for i := 0; i < n; i++ {
					w.Write([]string{fmt.Sprintf("10.%d.0.0/24", i), "FR", "Roubaix"})
				}
				return n, werr
			})
		return src, logs, err
	}

	src, _, err := run(1, nil)
	if err != nil || len(src.SkippedLayers) != 0 || src.Rows[HostingLayer] != 1 {
		t.Fatalf("one row: %+v %v", src, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	src, logs, err := run(0, ErrNoRows)
	if err != nil || !reflect.DeepEqual(src.SkippedLayers, []string{HostingLayer}) || len(src.Rows) != 0 {
		t.Fatalf("no rows: %+v %v", src, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale hosting.csv kept: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "WARNING") || !strings.Contains(logs[0], HostingLayer) {
		t.Errorf("logs = %v", logs)
	}
	if _, _, err = run(0, errors.New("boom")); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error: %v", err)
	}
}

// Skipped layers appear in the task output as a readable list of file names.
func TestSkipLayers(t *testing.T) {
	for _, bad := range [][]string{{"names.csv"}, {HostingLayer, "rir.csv"}, {""}} {
		if err := ValidateSkipLayers(bad); err == nil || !strings.Contains(err.Error(), bad[len(bad)-1]) ||
			!strings.Contains(err.Error(), HostingLayer) || !strings.Contains(err.Error(), IXLayer) {
			t.Errorf("ValidateSkipLayers(%v) = %v", bad, err)
		}
	}
	if err := ValidateSkipLayers([]string{HostingLayer, IXLayer}); err != nil {
		t.Errorf("ValidateSkipLayers(optional layers) = %v", err)
	}

	var logs []string
	p := &Pipeline{DataDir: "/data", Progress: func(s string) { logs = append(logs, s) }}
	src := Source{Rows: map[string]int{HostingLayer: 5}}
	p.SkipLayers(&src, []string{HostingLayer})
	if !reflect.DeepEqual(src.SkippedLayers, []string{HostingLayer}) {
		t.Fatalf("SkippedLayers = %v", src.SkippedLayers)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], HostingLayer) || !strings.Contains(logs[0], "skip_layers") {
		t.Errorf("logs = %v", logs)
	}
	args := strings.Join(p.geoArgs("out.mmdb", "/kept/feeds", src), " ")
	if strings.Contains(args, HostingLayer) || strings.Contains(args, "-first-country") || !strings.Contains(args, IXLayer) {
		t.Errorf("args = %s", args)
	}

	// A layer that is already skipped (no rows) is not added twice.
	src = Source{SkippedLayers: []string{IXLayer}}
	p.SkipLayers(&src, []string{IXLayer, HostingLayer, HostingLayer})
	if !reflect.DeepEqual(src.SkippedLayers, []string{IXLayer, HostingLayer}) {
		t.Errorf("SkippedLayers = %v", src.SkippedLayers)
	}
}

func TestSourceSkippedLayersJSON(t *testing.T) {
	data, _ := json.Marshal(Source{SkippedLayers: []string{HostingLayer, IXLayer}})
	if !strings.Contains(string(data), `"skipped_layers":["hosting.csv","ix.csv"]`) {
		t.Errorf("json = %s", data)
	}
	data, _ = json.Marshal(Source{})
	if strings.Contains(string(data), "skipped_layers") {
		t.Errorf("json = %s", data)
	}
}
