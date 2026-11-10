package geofeeds

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseGeoloc(t *testing.T) {
	tests := []struct {
		in       string
		lat, lon float64
		ok       bool
	}{
		{"52.3676 4.9041", 52.3676, 4.9041, true},
		{"  -33.86   151.21  ", -33.86, 151.21, true},
		{"90 180", 90, 180, true},
		{"-90 -180", -90, -180, true},
		{"48.1 11.5 extra fields", 48.1, 11.5, true},
		{"0 0", 0, 0, false},
		{"0.0 0.00", 0, 0, false},
		{"0 12.5", 0, 12.5, true}, // only 0 0 is a placeholder
		{"90.1 10", 0, 0, false},
		{"-90.5 10", 0, 0, false},
		{"10 180.5", 0, 0, false},
		{"10 -181", 0, 0, false},
		{"NaN 10", 0, 0, false},
		{"10 Inf", 0, 0, false},
		{"52.3676", 0, 0, false},
		{"", 0, 0, false},
		{"52.3676,4.9041", 0, 0, false},
		{"north east", 0, 0, false},
		{"52.3N 4.9E", 0, 0, false},
	}
	for _, tt := range tests {
		lat, lon, ok := parseGeoloc(tt.in)
		if ok != tt.ok || ok && (lat != tt.lat || lon != tt.lon) {
			t.Errorf("parseGeoloc(%q) = %v, %v, %v; want %v, %v, %v", tt.in, lat, lon, ok, tt.lat, tt.lon, tt.ok)
		}
	}
}

func TestGeolocBlockOf(t *testing.T) {
	obj := []attr{{"inetnum", "192.0.2.0 - 192.0.2.255"}, {"netname", "X"}, {"country", "nl"}, {"country", "DE"}, {"geoloc", "52.37 4.9"}, {"geoloc", "1 1"}}
	b, ok := geolocBlockOf(obj, "ripe")
	want := geolocBlock{"ripe", netip.MustParseAddr("192.0.2.0"), netip.MustParseAddr("192.0.2.255"), "NL", 52.37, 4.9}
	if !ok || b != want {
		t.Errorf("got %+v, %v; want %+v", b, ok, want)
	}
	for name, obj := range map[string][]attr{
		"no geoloc":      {{"inetnum", "192.0.2.0 - 192.0.2.255"}, {"country", "NL"}},
		"bad geoloc":     {{"inetnum", "192.0.2.0 - 192.0.2.255"}, {"country", "NL"}, {"geoloc", "0 0"}},
		"other class":    {{"inet6num", "2001:db8::/32"}, {"country", "NL"}, {"geoloc", "52 4"}},
		"route":          {{"route", "192.0.2.0/24"}, {"geoloc", "52 4"}},
		"bad range":      {{"inetnum", "192.0.2.255 - 192.0.2.0"}, {"geoloc", "52 4"}},
		"not a range":    {{"inetnum", "192.0.2.0/24"}, {"geoloc", "52 4"}},
		"geoloc in head": {{"geoloc", "52 4"}, {"inetnum", "192.0.2.0 - 192.0.2.255"}},
		"empty":          nil,
	} {
		if b, ok := geolocBlockOf(obj, "ripe"); ok {
			t.Errorf("%s: got %+v", name, b)
		}
	}
	// A block without a country is kept here; reject drops it.
	b, ok = geolocBlockOf([]attr{{"inetnum", "192.0.2.0 - 192.0.2.255"}, {"geoloc", "52 4"}}, "apnic")
	if !ok || b.country != "" || b.reject() != dropBadCountry {
		t.Errorf("no country: %+v, %v", b, ok)
	}
}

func TestGeolocBlockReject(t *testing.T) {
	block := func(from, to, country string) geolocBlock {
		return geolocBlock{from: netip.MustParseAddr(from), to: netip.MustParseAddr(to), country: country}
	}
	tests := []struct {
		b    geolocBlock
		want string
	}{
		{block("10.0.0.0", "10.0.0.255", "DE"), ""},
		{block("10.0.0.7", "10.0.0.7", "DE"), ""},
		{block("10.0.0.0", "10.0.255.255", "XK"), ""},             // a /16 is the largest allowed
		{block("10.0.0.1", "10.1.0.0", "DE"), ""},                 // 65,536 addresses, not aligned
		{block("10.0.0.0", "10.1.0.0", "DE"), dropTooLarge},       // 65,537
		{block("10.0.0.0", "10.1.255.255", "DE"), dropTooLarge},   // a /15
		{block("10.0.0.0", "10.255.255.255", "DE"), dropTooLarge}, // a /8
		{block("0.0.0.0", "255.255.255.255", "DE"), dropTooLarge}, // no overflow
		{block("10.0.0.0", "10.0.0.255", "EU"), dropBadCountry},
		{block("10.0.0.0", "10.0.0.255", "ZZ"), dropBadCountry},
		{block("10.0.0.0", "10.0.0.255", "AP"), dropBadCountry},
		{block("10.0.0.0", "10.0.0.255", ""), dropBadCountry},
		{block("10.0.0.0", "10.0.0.255", "XX"), dropBadCountry},
		{block("10.0.0.0", "10.0.0.255", "GERMANY"), dropBadCountry},
	}
	for _, tt := range tests {
		if got := tt.b.reject(); got != tt.want {
			t.Errorf("%v-%v %q: reject = %q, want %q", tt.b.from, tt.b.to, tt.b.country, got, tt.want)
		}
	}
}

func TestBiggestWithin(t *testing.T) {
	x := placeIndex{}
	add := func(country, name string, lat, lon float64, pop int) { x.add(country, place{name, lat, lon, pop}) }
	// Berlin and a few places around the point 52.52 N, 13.49 E, just west of the 13.5 cell boundary.
	add("DE", "Berlin", 52.52437, 13.41053, 3426354) // 6 km west
	add("DE", "Eastville", 52.52, 13.52, 5000)       // next cell to the east, 2 km
	add("DE", "Westville", 52.52, 13.45, 3000)       // same cell, 3 km
	add("DE", "Hamlet", 52.52, 13.49, 1500)          // on the point
	add("DE", "Potsdam", 52.3906, 13.0645, 178000)   // 31 km: too far
	add("PL", "Slubice", 52.35, 14.56, 16000)        // another country
	add("PL", "Polish Hamlet", 52.52, 13.5, 99999999)

	tests := []struct {
		name         string
		country      string
		lat, lon, km float64
		want         string
	}{
		{"most populous, not the closest", "DE", 52.52, 13.49, 25, "Berlin"},
		{"a neighbouring cell", "DE", 52.52, 13.49, 3, "Eastville"},
		{"closer but smaller is not chosen", "DE", 52.52, 13.49, 25, "Berlin"},
		{"a small one when it is all there is", "DE", 52.52, 13.49, 1, "Hamlet"},
		{"too far", "DE", 52.0, 13.49, 25, ""},
		{"other country only", "PL", 52.35, 13.0, 25, ""},
		{"no such country", "FR", 52.52, 13.49, 25, ""},
		{"Potsdam within reach", "DE", 52.39, 13.1, 25, "Potsdam"},
	}
	for _, tt := range tests {
		got, ok := x.biggestWithin(tt.country, tt.lat, tt.lon, tt.km)
		if tt.want == "" && ok || tt.want != "" && (!ok || got.name != tt.want) {
			t.Errorf("%s: got %q, %v; want %q", tt.name, got.name, ok, tt.want)
		}
	}

	// Equal populations: the closer one wins.
	y := placeIndex{}
	y.add("DE", place{"Far", 0.2, 10, 10})
	y.add("DE", place{"Near", 0.1, 10, 10})
	if got, _ := y.biggestWithin("DE", 0, 10, 25); got.name != "Near" {
		t.Errorf("tie: %q", got.name)
	}
}

// The search follows the circle: at high latitude a degree of longitude is
// short, so the neighbours can be several cells away; and it wraps at 180.
func TestBiggestWithinFarNorthAndAntimeridian(t *testing.T) {
	x := placeIndex{}
	// At 70 N, 0.6 degrees of longitude are 22.8 km.
	x.add("NO", place{"Far East", 70, 0.6, 2000})
	x.add("NO", place{"Far West", 70, -0.6, 1000})
	if got, ok := x.biggestWithin("NO", 70, 0, 25); !ok || got.name != "Far East" {
		t.Errorf("at 70 N: %q, %v", got.name, ok)
	}
	if got, ok := x.biggestWithin("NO", 70, 0, 22); ok {
		t.Errorf("at 70 N, 22 km: %q", got.name)
	}
	// 0.15 degrees across the antimeridian at 17 S are about 16 km.
	x.add("FJ", place{"Taveuni", -16.9, -179.95, 4000})
	x.add("FJ", place{"Vanua", -16.9, 179.9, 100})
	if got, ok := x.biggestWithin("FJ", -16.9, 179.95, 25); !ok || got.name != "Taveuni" {
		t.Errorf("east of the antimeridian: %q, %v", got.name, ok)
	}
	if got, ok := x.biggestWithin("FJ", -16.9, -179.99, 25); !ok || got.name != "Taveuni" {
		t.Errorf("west of the antimeridian: %q, %v", got.name, ok)
	}
	if got, ok := x.biggestWithin("FJ", -16.9, 180, 25); !ok || got.name != "Taveuni" {
		t.Errorf("on 180: %q, %v", got.name, ok)
	}
	// At the pole every longitude is near.
	x.add("AQ", place{"Pole", -89.95, 120, 10})
	if _, ok := x.biggestWithin("AQ", -89.99, -60, 25); !ok {
		t.Error("near the pole")
	}
}

func TestWriteWhoisRows(t *testing.T) {
	places := placeIndex{}
	places.add("NL", place{"Amsterdam", 52.37, 4.89, 800000})
	places.add("DE", place{"Berlin", 52.52, 13.4, 3400000})
	addr := netip.MustParseAddr
	blocks := []geolocBlock{
		{"ripe", addr("192.0.2.0"), addr("192.0.2.255"), "NL", 52.38, 4.9},
		{"apnic", addr("198.51.100.64"), addr("198.51.100.191"), "DE", 52.5, 13.4}, // two /26
		{"ripe", addr("203.0.113.0"), addr("203.0.113.255"), "NL", 48.0, 2.0},      // nothing near
		{"afrinic", addr("10.1.0.0"), addr("10.1.0.255"), "NL", 52.37, 4.89},
	}
	path := filepath.Join(t.TempDir(), WhoisRowsFile)
	dropped := map[string]int{}
	rows, err := writeWhoisRows(path, blocks, places, dropped)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 4 || !reflect.DeepEqual(dropped, map[string]int{dropNoPlace: 1}) {
		t.Errorf("rows %d, dropped %v", rows, dropped)
	}
	want := "prefix,country,region,city,source\n" +
		"192.0.2.0/24,NL,,Amsterdam,whois:ripe\n" +
		"198.51.100.64/26,DE,,Berlin,whois:apnic\n" +
		"198.51.100.128/26,DE,,Berlin,whois:apnic\n" +
		"10.1.0.0/24,NL,,Amsterdam,whois:afrinic\n"
	if got := readGzip(t, path); got != want {
		t.Errorf("rows file:\n%s\nwant:\n%s", got, want)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left: %v", err)
	}
	// No rows: an error, and the file stays.
	if _, err := writeWhoisRows(path, blocks[2:3], places, map[string]int{}); err == nil || readGzip(t, path) != want {
		t.Errorf("no rows: %v", err)
	}
}

func readGzip(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// citiesZip builds a GeoNames cities1000.zip from rows of name, latitude,
// longitude, country code, population and, optionally, feature code (default PPL).
func citiesZip(t *testing.T, cities [][]string) []byte {
	t.Helper()
	var text strings.Builder
	for i, c := range cities {
		f := make([]string, 19)
		feature := "PPL"
		if len(c) > 5 {
			feature = c[5]
		}
		f[0], f[1], f[2], f[4], f[5], f[6], f[7], f[8], f[14] = strconv.Itoa(i+1), c[0], c[0], c[1], c[2], "P", feature, c[3], c[4]
		text.WriteString(strings.Join(f, "\t") + "\n")
	}
	text.WriteString("short\tline\n")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("readme.txt")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("not the data"))
	if w, err = zw.Create(geonamesText); err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(text.String()))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var testCities = [][]string{
	{"Amsterdam", "52.37", "4.89", "NL", "800000"},
	{"Zaandam", "52.44", "4.83", "NL", "75000"},
	{"Berlin", "52.52", "13.4", "DE", "3400000"},
}

func TestReadPlaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), geonamesFile)
	os.WriteFile(path, citiesZip(t, testCities), 0o640)
	x, err := readPlaces(path)
	if err != nil {
		t.Fatal(err)
	}
	// Zaandam is 8 km from the point, Amsterdam 9 km: the bigger wins.
	if got, ok := x.biggestWithin("NL", 52.43, 4.9, 25); !ok || got.name != "Amsterdam" || got.pop != 800000 {
		t.Errorf("got %+v, %v", got, ok)
	}
	// A populous district, and places that are gone, never name a city.
	os.WriteFile(path, citiesZip(t, [][]string{
		{"Zaandam", "52.44", "4.83", "NL", "75000"},
		{"Amsterdam-Noord", "52.40", "4.92", "NL", "90000", "PPLX"},
		{"Old Town", "52.43", "4.9", "NL", "500000", "PPLH"},
		{"Abandoned", "52.43", "4.9", "NL", "400000", "PPLQ"},
		{"Destroyed", "52.43", "4.9", "NL", "300000", "PPLW"},
		{"Former Capital", "52.43", "4.9", "NL", "200000", "PPLCH"},
		{"Capital", "52.5", "4.9", "NL", "100000", "PPLC"},
	}), 0o640)
	if x, err = readPlaces(path); err != nil {
		t.Fatal(err)
	}
	if got, ok := x.biggestWithin("NL", 52.43, 4.9, 25); !ok || got.name != "Capital" {
		t.Errorf("with districts and ghosts: %+v, %v", got, ok)
	}
	if got, ok := x.biggestWithin("NL", 52.35, 4.83, 12); !ok || got.name != "Zaandam" {
		t.Errorf("a district is bigger but not a city: %+v, %v", got, ok)
	}
	os.WriteFile(path, []byte("not a zip"), 0o640)
	if _, err := readPlaces(path); err == nil {
		t.Error("a broken zip was read")
	}
}

// A copy younger than geonamesMaxAge is used without any request; an older
// one is asked about; a failing server leaves the old copy in use.
func TestLoadPlacesAge(t *testing.T) {
	var requests atomic.Int32
	status := http.StatusOK
	body := citiesZip(t, testCities)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if status != http.StatusOK {
			http.Error(w, "down", status)
			return
		}
		w.Write(body)
	}))
	defer server.Close()
	now := time.Date(2026, 11, 10, 5, 0, 0, 0, time.UTC)
	var logs []string
	p := &Pipeline{DataDir: t.TempDir(), geonames: server.URL, allowPrivate: true, now: func() time.Time { return now },
		Progress: func(s string) { logs = append(logs, s) }}
	path := filepath.Join(p.dir(), "dumps", geonamesFile)

	// No copy: downloaded.
	if _, st, err := p.loadPlaces(context.Background()); err != nil || st != statusDownloaded || requests.Load() != 1 {
		t.Fatalf("first load: %q, %v, %d requests", st, err, requests.Load())
	}
	// Fresh copy: no request at all, even though the server is down.
	status = http.StatusInternalServerError
	os.Chtimes(path, now.Add(-geonamesMaxAge+time.Hour), now.Add(-geonamesMaxAge+time.Hour))
	if x, st, err := p.loadPlaces(context.Background()); err != nil || st != statusFresh || requests.Load() != 1 || len(x) == 0 {
		t.Fatalf("fresh copy: %q, %v, %d requests", st, err, requests.Load())
	}
	// Old copy, server down: the copy is used, with a warning.
	os.Chtimes(path, now.Add(-geonamesMaxAge), now.Add(-geonamesMaxAge))
	if x, st, err := p.loadPlaces(context.Background()); err != nil || st != statusStale || requests.Load() != 2 || len(x) == 0 {
		t.Fatalf("old copy, server down: %q, %v, %d requests", st, err, requests.Load())
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "WARNING") {
		t.Errorf("logs = %q", logs)
	}
	// Old copy, server up: replaced.
	status = http.StatusOK
	if _, st, err := p.loadPlaces(context.Background()); err != nil || st != statusDownloaded || requests.Load() != 3 {
		t.Fatalf("old copy, server up: %q, %v, %d requests", st, err, requests.Load())
	}
	// No copy, server down: an error.
	os.Remove(path)
	status = http.StatusInternalServerError
	if _, st, err := p.loadPlaces(context.Background()); err == nil || st != statusFailed {
		t.Errorf("no copy, server down: %q, %v", st, err)
	}
	// A damaged copy is removed so the next run downloads it again.
	os.WriteFile(path, []byte("not a zip"), 0o640)
	if _, _, err := p.loadPlaces(context.Background()); err == nil {
		t.Error("a damaged copy was read")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("damaged copy kept: %v", err)
	}
}

var testGeoloc = func() []geolocBlock {
	addr := netip.MustParseAddr
	return []geolocBlock{
		{"ripe", addr("192.0.2.0"), addr("192.0.2.255"), "NL", 52.38, 4.9},        // Amsterdam
		{"apnic", addr("198.51.100.0"), addr("198.51.100.255"), "DE", 52.5, 13.4}, // Berlin
		{"ripe", addr("203.0.113.0"), addr("203.0.113.255"), "NL", 48, 2},         // no place
		{"ripe", addr("10.0.0.0"), addr("10.1.0.0"), "NL", 52.38, 4.9},            // too large
		{"ripe", addr("10.2.0.0"), addr("10.2.0.255"), "EU", 52.38, 4.9},          // not a country
		{"afrinic", addr("10.3.0.0"), addr("10.3.0.255"), "", 52.38, 4.9},         // no country
	}
}()

const testWhoisFile = "prefix,country,region,city,source\n192.0.2.0/24,NL,,Amsterdam,whois:ripe\n198.51.100.0/24,DE,,Berlin,whois:apnic\n"

// citiesServer serves the GeoNames zip and counts the requests.
func citiesServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write(citiesZip(t, testCities))
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func TestBuildWhois(t *testing.T) {
	server, requests := citiesServer(t)
	p := &Pipeline{DataDir: t.TempDir(), geonames: server.URL, allowPrivate: true}
	rowsPath := filepath.Join(p.dir(), OutDir, WhoisRowsFile)

	// Nothing usable: GeoNames is not even asked for.
	if out := p.buildWhois(context.Background(), testGeoloc[3:]); out.Skipped == "" || requests.Load() != 0 {
		t.Errorf("no usable blocks: %+v, %d requests", out, requests.Load())
	}

	out := p.buildWhois(context.Background(), testGeoloc)
	wantDropped := map[string]int{dropTooLarge: 1, dropBadCountry: 2, dropNoPlace: 1}
	if out.Blocks != 6 || out.Rows != 2 || !reflect.DeepEqual(out.Dropped, wantDropped) || out.Skipped != "" ||
		out.GeoNames != statusDownloaded || out.Bytes == 0 {
		t.Errorf("out = %+v", out)
	}
	if got := readGzip(t, rowsPath); got != testWhoisFile {
		t.Errorf("rows:\n%s\nwant:\n%s", got, testWhoisFile)
	}

	// GeoNames cannot be had (no copy of it either): skipped with the reason, and the previous
	// file stays. It never fails.
	broken := &Pipeline{DataDir: p.DataDir, geonames: server.URL + "-unreachable", allowPrivate: true}
	os.Remove(filepath.Join(p.dir(), "dumps", geonamesFile))
	if out = broken.buildWhois(context.Background(), testGeoloc); !strings.Contains(out.Skipped, "GeoNames") || out.Blocks != 6 {
		t.Errorf("no GeoNames: %+v", out)
	}
	if got := readGzip(t, rowsPath); got != testWhoisFile {
		t.Errorf("the previous file changed:\n%s", got)
	}

	// Blocks that all lack a place leave the previous file alone as well.
	if out = p.buildWhois(context.Background(), testGeoloc[2:3]); out.Skipped == "" {
		t.Errorf("no rows: %+v", out)
	}
	if got := readGzip(t, rowsPath); got != testWhoisFile {
		t.Errorf("the previous file changed:\n%s", got)
	}
	if _, err := os.Stat(rowsPath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left: %v", err)
	}
}

// writeRowsFile writes a whois rows file with n rows, as if discover had just built it.
func writeRowsFile(t *testing.T, p *Pipeline, n int) string {
	t.Helper()
	dir := filepath.Join(p.dir(), OutDir)
	os.MkdirAll(dir, 0o750)
	path := filepath.Join(dir, WhoisRowsFile)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte("prefix,country,region,city,source\n"))
	for i := 0; i < n; i++ {
		gz.Write([]byte("10." + strconv.Itoa(i/256%256) + "." + strconv.Itoa(i%256) + ".0/24,NL,,Amsterdam,whois:ripe\n"))
	}
	gz.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	// Newer than any upload recorded by now.
	later := time.Now().Add(time.Hour)
	os.Chtimes(path, later, later)
	return path
}

func TestUploadWhois(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	os.MkdirAll(p.dir(), 0o750)
	var keys []string
	upload := func(ctx context.Context, key, path string) (string, error) {
		keys = append(keys, key)
		return "r2://bucket/" + key, nil
	}
	statePath := filepath.Join(p.dir(), whoisUploadFile)

	// No file: skipped with a reason.
	if out := p.uploadWhois(context.Background(), upload, true); out.Skipped == "" || out.Uploaded || len(keys) != 0 {
		t.Errorf("no file: %+v", out)
	}

	// skip_upload: nothing is sent, the rows are counted.
	writeRowsFile(t, p, 3)
	if out := p.uploadWhois(context.Background(), nil, false); out.Rows != 3 || out.Uploaded || out.Skipped != "" {
		t.Errorf("without upload: %+v", out)
	}
	// Below the first-upload minimum: skipped with a reason.
	if out := p.uploadWhois(context.Background(), upload, false); out.Uploaded || !strings.Contains(out.Skipped, "fewer than") || len(keys) != 0 {
		t.Errorf("small first upload: %+v", out)
	}
	// force_upload skips the gate.
	if out := p.uploadWhois(context.Background(), upload, true); !out.Uploaded || out.Skipped != "" || !reflect.DeepEqual(keys, []string{"internal/layers/60-whois.csv.gz"}) {
		t.Errorf("forced upload: %+v, keys %v", out, keys)
	}
	// A file that is older than the last upload (discover did not rebuild it) is not sent again.
	path := filepath.Join(p.dir(), OutDir, WhoisRowsFile)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(path, past, past)
	if out := p.uploadWhois(context.Background(), upload, false); out.Uploaded || !strings.Contains(out.Skipped, "not rebuilt") {
		t.Errorf("an old file: %+v", out)
	}
	if out := p.uploadWhois(context.Background(), upload, true); !out.Uploaded {
		t.Errorf("forced upload of an old file: %+v", out)
	}
	// An upload error is a skip too.
	writeRowsFile(t, p, 3)
	failing := func(context.Context, string, string) (string, error) { return "", errors.New("R2 is down") }
	if out := p.uploadWhois(context.Background(), failing, true); out.Uploaded || out.Skipped != "R2 is down" {
		t.Errorf("failing upload: %+v", out)
	}
	if data, _ := os.ReadFile(statePath); !strings.Contains(string(data), `"rows":3`) {
		t.Errorf("%s = %s", whoisUploadFile, data)
	}
	if _, err := os.Stat(filepath.Join(p.dir(), uploadFile)); err == nil {
		t.Errorf("%s written", uploadFile)
	}
}

func TestUploadWhoisGate(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	os.MkdirAll(p.dir(), 0o750)
	var uploads int
	upload := func(context.Context, string, string) (string, error) { uploads++; return "r2://x", nil }
	try := func(rows int, force bool) WhoisUploadOutput {
		writeRowsFile(t, p, rows)
		return p.uploadWhois(context.Background(), upload, force)
	}
	if out := try(minWhoisFirstUpload-1, false); out.Uploaded || out.Skipped == "" || uploads != 0 {
		t.Errorf("first upload below the minimum: %+v", out)
	}
	if out := try(minWhoisFirstUpload, false); !out.Uploaded || uploads != 1 {
		t.Errorf("first upload at the minimum: %+v", out)
	}
	if out := try(18_001, false); !out.Uploaded {
		t.Errorf("a 9.995%% drop: %+v", out)
	}
	// The record now says 18,001.
	if out := try(16_200, false); out.Uploaded || out.PreviousRows != 18_001 || !strings.Contains(out.Skipped, "down from") {
		t.Errorf("a drop of more than 10%%: %+v", out)
	}
	if out := try(16_200, true); !out.Uploaded {
		t.Errorf("forced drop: %+v", out)
	}
	os.WriteFile(filepath.Join(p.dir(), whoisUploadFile), []byte(`{"rows":`), 0o640)
	if out := try(30_000, false); out.Uploaded || !strings.Contains(out.Skipped, "damaged") {
		t.Errorf("damaged record: %+v", out)
	}
}

// discover names the whois cities itself and writes the layer file; a GeoNames problem
// does not fail it and keeps the previous file.
func TestDiscoverWhois(t *testing.T) {
	ripe := gzipped(t, "inetnum: 192.0.2.0 - 192.0.2.255\ncountry: NL\ngeoloc: 52.37 4.89\nremarks: Geofeed https://isp.example/geofeed.csv\n\n"+
		"inetnum: 198.51.100.0 - 198.51.100.255\ncountry: NL\ngeoloc: 0 0\n\n"+
		"inetnum: 203.0.113.0 - 203.0.113.255\ncountry: NL\n\n"+
		"inetnum: 10.0.0.0 - 10.1.255.255\ncountry: NL\ngeoloc: 52.37 4.89\n")
	other := gzipped(t, "inetnum: 192.0.2.0 - 192.0.2.255\ncountry: BR\ngeoloc: -23.5 -46.6\n")
	mux := http.NewServeMux()
	mux.HandleFunc("/ripe.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(ripe) })
	mux.HandleFunc("/lacnic.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(other) })
	mux.HandleFunc("/cities.zip", func(w http.ResponseWriter, r *http.Request) { w.Write(citiesZip(t, testCities)) })
	server := httptest.NewServer(mux)
	defer server.Close()

	p := &Pipeline{
		store:   newFakeStore(),
		DataDir: t.TempDir(),
		dumps: []dump{
			{"ripe-inetnum", "ripe", server.URL + "/ripe.gz"},
			{"lacnic", "lacnic", server.URL + "/lacnic.gz"}, // not read for geoloc
		},
		caidaBase:    server.URL + "/caida",
		geonames:     server.URL + "/cities.zip",
		now:          func() time.Time { return time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC) },
		allowPrivate: true,
	}
	out, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Sources["ripe-inetnum"].Geoloc != 2 || out.Sources["lacnic"].Geoloc != 0 {
		t.Errorf("sources = %+v", out.Sources)
	}
	// The block of a /15 is too large; the other one is named after Amsterdam.
	wantWhois := WhoisOutput{Blocks: 2, Dropped: map[string]int{dropTooLarge: 1}, Rows: 1, GeoNames: statusDownloaded, Bytes: out.Whois.Bytes}
	if !reflect.DeepEqual(out.Whois, wantWhois) || out.Whois.Bytes == 0 {
		t.Errorf("whois = %+v, want %+v", out.Whois, wantWhois)
	}
	path := filepath.Join(p.dir(), OutDir, WhoisRowsFile)
	want := "prefix,country,region,city,source\n192.0.2.0/24,NL,,Amsterdam,whois:ripe\n"
	if got := readGzip(t, path); got != want {
		t.Errorf("%s:\n%s\nwant:\n%s", WhoisRowsFile, got, want)
	}
	// The feed in the first object is still found.
	if s := out.Sources["ripe-inetnum"]; s.Blocks != 1 || s.URLs != 1 {
		t.Errorf("ripe-inetnum stats = %+v", s)
	}

	// GeoNames gone: discover still works, says why, and the file stays.
	os.Remove(filepath.Join(p.dir(), "dumps", geonamesFile))
	p.geonames = server.URL + "/no-cities.zip"
	if out, err = p.Discover(context.Background()); err != nil || !strings.Contains(out.Whois.Skipped, "GeoNames") || out.Whois.Rows != 0 {
		t.Errorf("without GeoNames: %+v, %v", out.Whois, err)
	}
	if got := readGzip(t, path); got != want {
		t.Errorf("the previous file changed:\n%s", got)
	}
	if out.URLs == 0 {
		t.Error("discover found no URLs")
	}
}
