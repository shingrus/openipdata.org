package arin

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

// city is a line of cities1000.txt (made-up places).
func city(id int, name, ascii, alts, country, admin1 string, pop int) string {
	f := make([]string, 19)
	f[0], f[1], f[2], f[3] = fmt.Sprint(id), name, ascii, alts
	f[4], f[5], f[6], f[7], f[8], f[10] = "40.0", "-90.0", "P", "PPL", country, admin1
	f[14] = fmt.Sprint(pop)
	return strings.Join(f, "\t")
}

var testCities = []string{
	city(1, "Springfield", "Springfield", "", "US", "IL", 100000),
	city(2, "Springfield", "Springfield", "", "US", "MO", 150000), // the more populous one wins the name
	city(3, "Portland", "Portland", "", "US", "OR", 600000),
	city(4, "New York", "New York", "Big Apple,NYC,紐約", "US", "NY", 8000000),
	city(5, "Foo Town", "Foo Town", "", "US", "TX", 10),
	city(6, "Bar City", "Bar City", "Foo Town", "US", "TX", 5000), // an alternate name loses to a primary one
	city(9, "Gamma City", "Gamma City", "Delta", "US", "OH", 500),
	city(10, "Gamma City", "Gamma City", "", "US", "OH", 90000), // takes the name "Gamma City" from place 9
	city(11, "Toronto", "Toronto", "", "CA", "08", 2700000),
	city(12, "Calgary", "Calgary", "", "CA", "01", 1200000),
	city(13, "Berlin", "Berlin", "", "DE", "16", 3000000), // another country: never indexed
	city(14, "Tie One", "Tie One", "", "US", "WA", 700),
	city(15, "Tie One", "Tie One", "", "US", "OR", 700), // equal population: the first stays
	"short line",
}

func zipOf(t *testing.T, lines []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("cities1000.txt")
	io.WriteString(w, strings.Join(lines, "\n")+"\n")
	zw.Close()
	return buf.Bytes()
}

func testIndex(t *testing.T) placeIndex {
	t.Helper()
	idx, err := readPlaces(strings.NewReader(strings.Join(testCities, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestPlacingRule(t *testing.T) {
	idx := testIndex(t)
	get := func(country, name string) place { p, _ := idx.place(country, name); return p }
	if p := get("US", " springfield "); p.id != 2 || p.admin1 != "MO" {
		t.Errorf("the more populous place wins: %+v", p)
	}
	if p := get("US", "Foo Town"); p.id != 5 {
		t.Errorf("a primary name beats an alternate: %+v", p)
	}
	if p := get("US", "big apple"); p.id != 4 || p.name != "New York" || p.primary {
		t.Errorf("alternate name: %+v", p)
	}
	if _, ok := idx.place("US", "NYC"); !ok { // "nyc" has Latin letters
		t.Error("a Latin alternate is not indexed")
	}
	if _, ok := idx.place("US", "紐約"); ok {
		t.Error("an alternate with no Latin letter is indexed")
	}
	if p := get("US", "Tie One"); p.id != 14 {
		t.Errorf("a tie keeps the first: %+v", p)
	}
	if _, ok := idx.place("DE", "Berlin"); ok {
		t.Error("a place of another country is indexed")
	}
	if p := get("CA", "Toronto"); p.id != 11 {
		t.Errorf("Toronto: %+v", p)
	}
}

func rec(start, end, country, state, city string) record {
	return record{Start: u32(mustAddr(start)), End: u32(mustAddr(end)), Country: country, State: state, City: city}
}

func TestBuildRows(t *testing.T) {
	idx := testIndex(t)
	var out ExportOutput
	recs := []record{
		rec("10.0.1.0", "10.0.1.255", "US", "OR", "Portland"),    // fine
		rec("10.0.0.0", "10.0.15.255", "US", "OR", "portland"),   // a /20 around the /24 above: nested, both kept
		rec("10.0.2.0", "10.0.2.255", "US", "IL", "Springfield"), // the name is Springfield, MO: another state
		rec("10.0.3.0", "10.0.3.255", "US", "MO", "Springfield"), // fine
		rec("10.0.4.0", "10.0.4.255", "US", "NY", "Big Apple"),   // written under the main name
		rec("10.0.5.0", "10.0.5.255", "US", "ZZ", "Nowhere"),     // unknown to GeoNames
		rec("10.0.6.0", "10.0.6.255", "US", "OH", "Delta"),       // its main name places another place: ambiguous
		rec("10.0.7.0", "10.0.7.255", "CA", "ON", "Toronto"),     // fine
		rec("10.0.8.0", "10.0.8.255", "CA", "QC", "Toronto"),     // Toronto is in ON
		rec("10.0.9.0", "10.0.9.255", "CA", "AB", "Calgary"),     // fine, code 01
		rec("10.0.10.0", "10.0.10.255", "CA", "BC", "Calgary"),   // another province
		rec("10.1.0.0", "10.1.31.255", "US", "OR", "Portland"),   // 8192 addresses: wider than a /20, not counted
		rec("10.2.0.0", "10.2.2.255", "US", "OR", "Portland"),    // not a power of two: a /23 and a /24
		rec("10.3.0.0", "10.3.0.255", "US", "TX", "Foo Town"),
		rec("10.3.1.0", "10.3.1.255", "", "", ""), // no city: never exported
	}
	rows := buildRows(recs, nil, idx, &out)
	var got []string
	for _, r := range rows {
		got = append(got, strings.Join([]string{r.prefix, r.country, r.state, r.city}, ","))
	}
	want := []string{
		"10.0.0.0/20,US,OR,Portland", // the broader block first
		"10.0.1.0/24,US,OR,Portland",
		"10.0.3.0/24,US,MO,Springfield",
		"10.0.4.0/24,US,NY,New York",
		"10.0.7.0/24,CA,ON,Toronto",
		"10.0.9.0/24,CA,AB,Calgary",
		"10.2.0.0/23,US,OR,Portland",
		"10.2.2.0/24,US,OR,Portland",
		"10.3.0.0/24,US,TX,Foo Town",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if out.Records != 13 || out.Unplaced != 1 || out.OtherState != 3 || out.Ambiguous != 1 {
		t.Errorf("counts %+v", out)
	}
	// The same prefix from two records is written once.
	out = ExportOutput{}
	rows = buildRows([]record{
		rec("10.0.1.0", "10.0.1.255", "US", "OR", "Portland"),
		rec("10.0.1.0", "10.0.1.255", "US", "TX", "Foo Town"),
	}, nil, idx, &out)
	if len(rows) != 1 {
		t.Errorf("duplicate prefix: %v", rows)
	}
	// Two records that overlap give the same /24: the smaller record is the more specific
	// answer and wins, whatever the names are. 10.0.1.0-10.0.2.255 splits into two /24s.
	out = ExportOutput{}
	rows = buildRows([]record{
		rec("10.0.1.0", "10.0.2.255", "US", "TX", "Foo Town"),
		rec("10.0.1.0", "10.0.1.255", "US", "OR", "Portland"),
	}, nil, idx, &out)
	got = nil
	for _, r := range rows {
		got = append(got, r.prefix+","+r.city)
	}
	if want := "10.0.1.0/24,Portland 10.0.2.0/24,Foo Town"; strings.Join(got, " ") != want {
		t.Errorf("overlapping records: %v, want %s", got, want)
	}
}

func readLayer(t *testing.T, path string) string {
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
	data, _ := io.ReadAll(gz)
	return string(data)
}

type geoServer struct {
	*httptest.Server
	hits int
}

func newGeo(t *testing.T, body []byte, status int) *geoServer {
	g := &geoServer{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.hits++
		if status != 200 {
			http.Error(w, "no", status)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(g.Close)
	return g
}

func TestExport(t *testing.T) {
	geo := newGeo(t, zipOf(t, testCities), 200)
	e := newEnv(t, nil, 8)
	e.p.geoURL, e.p.dlClient = geo.URL, geo.Client()
	e.db.pruned = 4
	e.db.nets = []record{
		rec("10.0.3.0", "10.0.3.255", "US", "MO", "Springfield"),
		rec("10.0.1.0", "10.0.1.255", "US", "OR", "Portland"),
		rec("10.0.0.0", "10.0.15.255", "US", "OR", "Portland"),
		rec("10.0.2.0", "10.0.2.255", "US", "IL", "Springfield"),
		rec("10.1.0.0", "10.1.31.255", "US", "OR", "Portland"),
	}
	out, err := e.p.Export(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Skipped != "" || out.GeoNames != "downloaded" || out.Records != 4 || out.Rows != 3 || out.OtherState != 1 || out.Pruned != 4 || out.StoredRecords != 5 || out.Bytes <= 0 {
		t.Errorf("output %+v", out)
	}
	want := "prefix,country,region,city,source\n" +
		"10.0.0.0/20,US,OR,Portland,whois-city:arin\n" +
		"10.0.1.0/24,US,OR,Portland,whois-city:arin\n" +
		"10.0.3.0/24,US,MO,Springfield,whois-city:arin\n"
	first, _ := os.ReadFile(e.p.LayerPath())
	if got := readLayer(t, e.p.LayerPath()); got != want {
		t.Errorf("layer\n%s\nwant\n%s", got, want)
	}
	e.noAddress(t, out)

	// A young file is not written again, unless forced; the output is the same every time. The
	// file's time is set by hand: the test's clock is fixed, the file system's is not.
	os.Chtimes(e.p.LayerPath(), testNow.Add(-time.Hour), testNow.Add(-time.Hour))
	geo.hits = 0
	if out, err = e.p.Export(context.Background(), false); err != nil || out.Skipped != "recent" || geo.hits != 0 {
		t.Errorf("recent: %+v %v", out, err)
	}
	if _, err = e.p.Export(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if second, _ := os.ReadFile(e.p.LayerPath()); !bytes.Equal(first, second) {
		t.Error("the layer file differs between runs")
	}
	// Older than six hours it is written again. GeoNames was asked less than 7 days ago: not again.
	old := testNow.Add(-7 * time.Hour)
	os.Chtimes(e.p.LayerPath(), old, old)
	if out, err = e.p.Export(context.Background(), false); err != nil || out.Skipped != "" || out.GeoNames != "recent" || geo.hits != 0 {
		t.Errorf("old file: %+v %v hits %d", out, err, geo.hits)
	}
}

func TestExportSkips(t *testing.T) {
	// No GeoNames at all: the old layer stays.
	bad := newGeo(t, nil, 404)
	e := newEnv(t, nil, 8)
	e.p.geoURL, e.p.dlClient = bad.URL, bad.Client()
	os.MkdirAll(e.p.dir(), 0o750)
	os.WriteFile(e.p.LayerPath(), []byte("old"), 0o640)
	out, err := e.p.Export(context.Background(), true)
	if err != nil || out.Skipped != "no GeoNames" {
		t.Errorf("no GeoNames: %+v %v", out, err)
	}
	if data, _ := os.ReadFile(e.p.LayerPath()); string(data) != "old" {
		t.Error("the old layer was replaced")
	}
	// A damaged copy is no GeoNames either, and is deleted.
	broken := newGeo(t, []byte("not a zip"), 200)
	e.p.geoURL, e.p.dlClient = broken.URL, broken.Client()
	if out, err = e.p.Export(context.Background(), true); err != nil || out.Skipped != "no GeoNames" {
		t.Errorf("damaged: %+v %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(e.p.dir(), geoNamesFile)); err == nil {
		t.Error("a damaged copy stays")
	}
	// A saved copy is used when the download fails.
	good := newGeo(t, zipOf(t, testCities), 200)
	e.p.geoURL, e.p.dlClient = good.URL, good.Client()
	e.db.nets = []record{rec("10.0.1.0", "10.0.1.255", "US", "OR", "Portland")}
	if _, err = e.p.Export(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(e.p.dir(), geoNamesFile+checkedSuffix)
	os.Chtimes(stamp, testNow.Add(-8*24*time.Hour), testNow.Add(-8*24*time.Hour))
	e.p.geoURL, e.p.dlClient = bad.URL, bad.Client()
	if out, err = e.p.Export(context.Background(), true); err != nil || out.GeoNames != "saved copy" || out.Rows != 1 {
		t.Errorf("saved copy: %+v %v", out, err)
	}
	// No row: the layer file goes too (upload must stop renewing it), the old records are still deleted.
	e.p.geoURL, e.p.dlClient = good.URL, good.Client()
	e.db.nets, e.db.pruned = nil, 2
	os.WriteFile(e.p.LayerPath(), []byte("old"), 0o640)
	if out, err = e.p.Export(context.Background(), true); err != nil || out.Skipped != "no rows" || out.Pruned != 2 {
		t.Errorf("no rows: %+v %v", out, err)
	}
	if _, err := os.Stat(e.p.LayerPath()); !os.IsNotExist(err) {
		t.Errorf("the layer file is still there with no rows left: %v", err)
	}
	e.p.upload = func(context.Context, string, string, publish.PutOptions) (string, publish.PutStatus, error) {
		return "", publish.PutUnchanged, nil
	}
	if up, err := e.p.Upload(context.Background(), false); err != nil || up.Skipped != "no layer file" {
		t.Errorf("upload after no rows: %+v %v", up, err)
	}
	e.noAddress(t, out)
}

func TestUpload(t *testing.T) {
	var calls []publish.PutOptions
	var keys []string
	p := &Pipeline{DataDir: t.TempDir(), upload: func(_ context.Context, key, _ string, opts publish.PutOptions) (string, publish.PutStatus, error) {
		keys, calls = append(keys, key), append(calls, opts)
		return "", publish.PutUnchanged, nil
	}}
	out, err := p.Upload(context.Background(), false)
	if err != nil || out.Skipped != "no layer file" || len(calls) != 0 {
		t.Errorf("no file: %+v %v", out, err)
	}
	os.MkdirAll(p.dir(), 0o750)
	os.WriteFile(p.LayerPath(), []byte("12345"), 0o640)
	out, err = p.Upload(context.Background(), true)
	if err != nil || out.Status != publish.PutUnchanged || out.Bytes != 5 || len(calls) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if keys[0] != "internal/layers/37-whois-arin.csv.gz" || !calls[0].Refresh || !calls[0].Force {
		t.Errorf("put %v %+v", keys, calls)
	}
	if err := (&Pipeline{}).CheckUpload(); err == nil {
		t.Error("an empty R2 setting is accepted")
	}
}
