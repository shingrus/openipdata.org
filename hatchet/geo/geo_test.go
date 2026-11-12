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
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/ipdata"
)

func layerRows(t *testing.T, fill func(w *csv.Writer) error) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "layer.csv.gz")
	if _, err := writeLayer(path, fill); err != nil {
		t.Fatal(err)
	}
	return readGzip(t, path)
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
	want := "prefix,country,region,city,source\n45.10.0.0/22,US,,,\n45.20.0.0/23,CA,,,\n45.20.2.0/24,CA,,,\n2600:1000::/28,US,,,\n" +
		"45.40.0.0/24,,,,\n45.50.0.0/24,,,,\n45.60.0.0/24,,,,\n45.70.0.0/24,,,,\n"
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
	if got != "prefix,country,region,city,source\n63.176.0.0/13,DE,,,\n45.10.0.0/24,DE,,,\n" || !reflect.DeepEqual(names, map[string]string{"DE": "Germany"}) {
		t.Errorf("names %v, layer:\n%s", names, got)
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

// touchLayers writes empty layer files into the layers directory of p.
func touchLayers(t *testing.T, p *Pipeline, names ...string) {
	t.Helper()
	os.MkdirAll(p.layersDir(), 0o750)
	for _, name := range names {
		writeGzip(t, filepath.Join(p.layersDir(), name), "prefix,country,region,city,source\n")
	}
}

func TestBuildMMDBArgs(t *testing.T) {
	// A stand-in mmdbbuilder that records its arguments and prints a summary.
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"+
		"echo 'layer 10-rir.csv.gz: 10 rows, 9 prefixes, 1 skipped'\n"+
		"echo 'layer 70-geofeeds.csv.gz: 5 rows, 5 prefixes, 0 skipped'\n"+
		"echo 'wrote x (2 layers, 14 prefixes)'\n"), 0o755)
	p := &Pipeline{DataDir: dir, MMDBBuilder: script}
	touchLayers(t, p, FeedsLayer, RIRLayer, PDBLayer, IXLayer, NamesFile)
	out, err := p.BuildMMDB(context.Background(), Source{DateTag: "20261109"})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	layers := filepath.Join(dir, "geo", "layers")
	// Every layer file, by name, and no more; names.tsv is not a layer.
	want := strings.Join([]string{"geo", "-out", filepath.Join(dir, "out", "geo", "20261109", "ip2geo-openipdata-20261109.mmdb"),
		"-names", filepath.Join(layers, NamesFile), "-same-country", PDBLayer, "-within-first", FeedsLayer,
		filepath.Join(layers, RIRLayer), filepath.Join(layers, PDBLayer), filepath.Join(layers, IXLayer), filepath.Join(layers, FeedsLayer)}, " ")
	if strings.TrimSpace(string(args)) != want {
		t.Errorf("args = %s\nwant   %s", args, want)
	}
	if !reflect.DeepEqual(out.Prefixes, map[string]int{RIRLayer: 9, FeedsLayer: 5}) ||
		!reflect.DeepEqual(out.Skipped, map[string]int{RIRLayer: 1}) {
		t.Errorf("out = %+v", out)
	}
	// No layers directory: an error, no build.
	os.RemoveAll(layers)
	if _, err := p.BuildMMDB(context.Background(), Source{DateTag: "20261109"}); err == nil {
		t.Error("no error without a layers directory")
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
	if strings.Contains(hostingQuery, "a.country") || !strings.Contains(hostingQuery, ipdata.SeenAtFilter) || !strings.Contains(pdbQuery, ipdata.SeenAtFilter) {
		t.Errorf("hostingQuery = %s", hostingQuery)
	}
	for _, bad := range []string{"INSERT", "UPDATE", "ALTER", "DELETE"} {
		if strings.Contains(strings.ToUpper(hostingQuery), bad) {
			t.Errorf("hostingQuery contains %s", bad)
		}
	}
}

// The layer names carry the order, in steps of 10, and the R2 layers are named as
// sync-geofeeds uploads them.
func TestLayerNames(t *testing.T) {
	names := []string{RIRLayer, IPverseLayer, PDBLayer, RDNSWideLayer, HostingLayer, IXLayer, WhoisLayer, RDNSLayer, FeedsLayer}
	want := []string{"10-rir.csv.gz", "20-ipverse.csv.gz", "30-pdb.csv.gz", "35-rdns-wide.csv.gz", "40-hosting.csv.gz", "50-ix.csv.gz", "60-whois.csv.gz", "65-rdns.csv.gz", "70-geofeeds.csv.gz"}
	if !reflect.DeepEqual(names, want) || !sort.StringsAreSorted(names) {
		t.Errorf("layer names = %v", names)
	}
	for _, l := range r2Layers {
		if path.Base(l.Key) != l.Name || !strings.HasPrefix(l.Key, "internal/layers/") {
			t.Errorf("R2 layer %s has key %s", l.Name, l.Key)
		}
	}
	for name := range layerRules {
		if !slices.Contains(names, name) {
			t.Errorf("rule for %q, which is no layer", name)
		}
	}
}

// The build passes every layer file, sorted by name, whatever order the directory lists
// them in; each rule names the layers of its flag that are there, and a rule for an
// absent file is not passed; the flags come sorted. Files that are not layers are left out.
func TestGeoArgs(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	layers := p.layersDir() + "/"
	args := func(feeds string) string {
		got, err := p.geoArgs("out.mmdb", feeds)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, " ")
	}
	touchLayers(t, p, FeedsLayer, RDNSLayer, WhoisLayer, IXLayer, HostingLayer, RDNSWideLayer, PDBLayer, IPverseLayer, RIRLayer, NamesFile, "notes.txt", "55-extra.csv.gz")
	os.WriteFile(filepath.Join(p.layersDir(), "60-whois.csv.gz.tmp"), nil, 0o640)
	prefix := "geo -out out.mmdb -names " + layers + NamesFile
	want := prefix + " -first-country " + HostingLayer + " -same-country " + PDBLayer + "," + RDNSWideLayer + "," + RDNSLayer + " -within-first " + WhoisLayer + "," + FeedsLayer + " " +
		strings.Join([]string{layers + RIRLayer, layers + IPverseLayer, layers + PDBLayer, layers + RDNSWideLayer, layers + HostingLayer, layers + IXLayer,
			layers + "55-extra.csv.gz", layers + WhoisLayer, layers + RDNSLayer, layers + FeedsLayer}, " ")
	if got := args(""); got != want {
		t.Errorf("all layers:\n%s\nwant:\n%s", got, want)
	}
	// The holdout build puts its own file in place of the geofeeds one.
	if got := args("/kept/" + FeedsLayer); !strings.HasSuffix(got, layers+RDNSLayer+" /kept/"+FeedsLayer) || strings.Contains(got, layers+FeedsLayer) {
		t.Errorf("with a kept feeds file: %s", got)
	}

	// Absent files: no flag for them, the others keep their order.
	for _, c := range []struct {
		absent []string
		flags  string
	}{
		{[]string{IXLayer, "55-extra.csv.gz"}, "-first-country " + HostingLayer + " -same-country " + PDBLayer + "," + RDNSWideLayer + "," + RDNSLayer + " -within-first " + WhoisLayer + "," + FeedsLayer},
		{[]string{HostingLayer}, "-same-country " + PDBLayer + "," + RDNSWideLayer + "," + RDNSLayer + " -within-first " + WhoisLayer + "," + FeedsLayer},
		{[]string{WhoisLayer}, "-first-country " + HostingLayer + " -same-country " + PDBLayer + "," + RDNSWideLayer + "," + RDNSLayer + " -within-first " + FeedsLayer},
		{[]string{RDNSWideLayer, RDNSLayer}, "-first-country " + HostingLayer + " -same-country " + PDBLayer + " -within-first " + WhoisLayer + "," + FeedsLayer},
		{[]string{PDBLayer, RDNSWideLayer, HostingLayer, WhoisLayer}, "-same-country " + RDNSLayer + " -within-first " + FeedsLayer},
		{[]string{PDBLayer, RDNSWideLayer, HostingLayer, WhoisLayer, RDNSLayer}, "-within-first " + FeedsLayer},
		{[]string{PDBLayer, RDNSWideLayer, HostingLayer, WhoisLayer, RDNSLayer, FeedsLayer}, ""},
	} {
		touchLayers(t, p, FeedsLayer, RDNSLayer, WhoisLayer, IXLayer, HostingLayer, RDNSWideLayer, PDBLayer, IPverseLayer, RIRLayer, "55-extra.csv.gz")
		for _, name := range c.absent {
			os.Remove(filepath.Join(p.layersDir(), name))
		}
		got := args("")
		var files []string
		for _, name := range []string{RIRLayer, IPverseLayer, PDBLayer, RDNSWideLayer, HostingLayer, IXLayer, "55-extra.csv.gz", WhoisLayer, RDNSLayer, FeedsLayer} {
			if _, err := os.Stat(filepath.Join(p.layersDir(), name)); err == nil {
				files = append(files, layers+name)
			}
		}
		wantArgs := prefix + " " + strings.TrimSpace(c.flags+" "+strings.Join(files, " "))
		if got != wantArgs {
			t.Errorf("without %v:\n%s\nwant:\n%s", c.absent, got, wantArgs)
		}
	}

	// -within-first and -first-country judge rows against the first layer passed:
	// a file that sorts before 10-rir would take its place, so it is an error.
	touchLayers(t, p, FeedsLayer, RIRLayer, "05-first.csv.gz")
	if _, err := p.geoArgs("out.mmdb", ""); err == nil || !strings.Contains(err.Error(), RIRLayer) {
		t.Errorf("a layer before %s: %v", RIRLayer, err)
	}
	os.Remove(filepath.Join(p.layersDir(), RIRLayer))
	os.Remove(filepath.Join(p.layersDir(), "05-first.csv.gz"))
	if _, err := p.geoArgs("out.mmdb", ""); err == nil {
		t.Error("no error without the RIR layer")
	}

	// No layers directory: an error.
	if _, err := (&Pipeline{DataDir: t.TempDir()}).geoArgs("out.mmdb", ""); err == nil {
		t.Error("no error without a layers directory")
	}
}

// While public.geo_ix_pdb has no rows the layer is skipped (no file), and a partial
// table still fails.
func TestDownloadIX(t *testing.T) {
	layers := t.TempDir()
	path := filepath.Join(layers, IXLayer)
	rowsOf := func(n int, err error) func(context.Context, *csv.Writer) (int, error) {
		return func(_ context.Context, w *csv.Writer) (int, error) {
			for i := 0; i < n; i++ {
				layerRow(w, fmt.Sprintf("10.%d.0.0/24", i), "DE", "Berlin")
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

	// No rows: skipped with a warning (Download starts from an empty directory, so no file is left).
	os.Remove(path)
	src, logs, err := run(rowsOf(0, ErrNoRows))
	if err != nil || !src.skipped(IXLayer) || len(src.Rows) != 0 {
		t.Fatalf("empty table: %+v %v", src, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a file for a skipped layer: %v", err)
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
// (or a source that does not exist yet) skips it with a warning and writes no file.
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
					layerRow(w, fmt.Sprintf("10.%d.0.0/24", i), "FR", "Roubaix")
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
	os.Remove(path)
	src, logs, err := run(0, ErrNoRows)
	if err != nil || !reflect.DeepEqual(src.SkippedLayers, []string{HostingLayer}) || len(src.Rows) != 0 {
		t.Fatalf("no rows: %+v %v", src, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a file for a skipped layer: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "WARNING") || !strings.Contains(logs[0], HostingLayer) {
		t.Errorf("logs = %v", logs)
	}
	if _, _, err = run(0, errors.New("boom")); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error: %v", err)
	}
}

// skip_layers names optional layers; they are removed from the layers directory
// and listed in the task output.
func TestSkipLayers(t *testing.T) {
	for _, bad := range [][]string{{"names.csv"}, {HostingLayer, RIRLayer}, {""}, {FeedsLayer}} {
		if err := ValidateSkipLayers(bad); err == nil || !strings.Contains(err.Error(), bad[len(bad)-1]) ||
			!strings.Contains(err.Error(), HostingLayer) || !strings.Contains(err.Error(), IXLayer) || !strings.Contains(err.Error(), WhoisLayer) ||
			!strings.Contains(err.Error(), RDNSLayer) || !strings.Contains(err.Error(), RDNSWideLayer) {
			t.Errorf("ValidateSkipLayers(%v) = %v", bad, err)
		}
	}
	if err := ValidateSkipLayers([]string{RDNSWideLayer, HostingLayer, IXLayer, WhoisLayer, RDNSLayer}); err != nil {
		t.Errorf("ValidateSkipLayers(optional layers) = %v", err)
	}

	var logs []string
	p := &Pipeline{DataDir: t.TempDir(), Progress: func(s string) { logs = append(logs, s) }}
	touchLayers(t, p, RIRLayer, HostingLayer, IXLayer)
	src := Source{Rows: map[string]int{HostingLayer: 5}}
	if err := p.SkipLayers(&src, []string{HostingLayer}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src.SkippedLayers, []string{HostingLayer}) {
		t.Fatalf("SkippedLayers = %v", src.SkippedLayers)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], HostingLayer) || !strings.Contains(logs[0], "skip_layers") {
		t.Errorf("logs = %v", logs)
	}
	got, err := p.geoArgs("out.mmdb", "")
	args := strings.Join(got, " ")
	if err != nil || strings.Contains(args, HostingLayer) || strings.Contains(args, "-first-country") || !strings.Contains(args, IXLayer) {
		t.Errorf("args = %s, %v", args, err)
	}

	// A layer that is already skipped (no rows, no file) is not added twice.
	src = Source{SkippedLayers: []string{IXLayer}}
	if err := p.SkipLayers(&src, []string{IXLayer, HostingLayer, HostingLayer}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src.SkippedLayers, []string{IXLayer, HostingLayer}) {
		t.Errorf("SkippedLayers = %v", src.SkippedLayers)
	}
}

func TestSourceSkippedLayersJSON(t *testing.T) {
	data, _ := json.Marshal(Source{SkippedLayers: []string{HostingLayer, IXLayer}})
	if !strings.Contains(string(data), `"skipped_layers":["40-hosting.csv.gz","50-ix.csv.gz"]`) {
		t.Errorf("json = %s", data)
	}
	data, _ = json.Marshal(Source{})
	if strings.Contains(string(data), "skipped_layers") {
		t.Errorf("json = %s", data)
	}
}

// The rDNS layers come from public.rdns_geo_layer by scope: the wide rows are
// 35-rdns-wide and the traced ones 65-rdns. Both are optional (no rows: skipped
// with a warning, no file), have no minimum, and both only refine a country.
func TestDownloadRDNSLayers(t *testing.T) {
	layers := t.TempDir()
	rows := map[string]int{rdnsWide: 3, rdnsTraced: 2}
	var scopes []string
	write := func(_ context.Context, scope string, w *csv.Writer) (int, error) {
		scopes = append(scopes, scope)
		for i := 0; i < rows[scope]; i++ {
			layerRow(w, fmt.Sprintf("10.%d.0.0/24", i), "DE", "Berlin")
		}
		if rows[scope] == 0 {
			return 0, ErrNoRows
		}
		return rows[scope], nil
	}
	run := func() (Source, []string) {
		var logs []string
		p := &Pipeline{writeRDNS: write, Progress: func(s string) { logs = append(logs, s) }}
		src := Source{Rows: map[string]int{}}
		for _, l := range []struct{ name, scope string }{{RDNSWideLayer, rdnsWide}, {RDNSLayer, rdnsTraced}} {
			err := p.downloadOptional(context.Background(), layers, &src, l.name, 0, "rDNS "+l.scope+" rows", "hint",
				func(ctx context.Context, w *csv.Writer) (int, error) { return p.writeRDNS(ctx, l.scope, w) })
			if err != nil {
				t.Fatal(err)
			}
		}
		return src, logs
	}
	src, _ := run()
	if len(src.SkippedLayers) != 0 || src.Rows[RDNSWideLayer] != 3 || src.Rows[RDNSLayer] != 2 {
		t.Fatalf("both scopes: %+v", src)
	}
	if got := readGzip(t, filepath.Join(layers, RDNSLayer)); got != "prefix,country,region,city,source\n10.0.0.0/24,DE,,Berlin,\n10.1.0.0/24,DE,,Berlin,\n" {
		t.Errorf("%s = %q", RDNSLayer, got)
	}

	rows[rdnsWide] = 0
	os.Remove(filepath.Join(layers, RDNSWideLayer))
	src, logs := run()
	if !reflect.DeepEqual(src.SkippedLayers, []string{RDNSWideLayer}) || src.Rows[RDNSLayer] != 2 || len(logs) != 2 || !strings.Contains(logs[0], "WARNING") {
		t.Fatalf("no wide rows: %+v %v", src, logs)
	}
	if _, err := os.Stat(filepath.Join(layers, RDNSWideLayer)); !os.IsNotExist(err) {
		t.Errorf("a file for a skipped layer: %v", err)
	}
}

func TestRDNSQueryReadsOneScope(t *testing.T) {
	for _, part := range []string{"FROM public.rdns_geo_layer", "scope = $1", "ORDER BY prefix"} {
		if !strings.Contains(rdnsQuery, part) {
			t.Errorf("rdnsQuery lacks %q", part)
		}
	}
	for _, bad := range []string{"INSERT", "UPDATE", "ALTER", "DELETE", "CREATE"} {
		if strings.Contains(strings.ToUpper(rdnsQuery), bad) {
			t.Errorf("rdnsQuery contains %s", bad)
		}
	}
	if _, err := writeRDNSRows(context.Background(), "", "other", nil); err == nil {
		t.Error("an unknown scope was accepted")
	}
}

// Both rDNS layers only refine: they never change a country.
func TestRDNSLayerRules(t *testing.T) {
	for _, name := range []string{RDNSWideLayer, RDNSLayer} {
		if layerRules[name] != "-same-country" || !slices.Contains(optionalLayers, name) {
			t.Errorf("%s: rule %q, optional %v", name, layerRules[name], slices.Contains(optionalLayers, name))
		}
	}
}
