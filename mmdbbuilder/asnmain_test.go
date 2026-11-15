package main

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

// rows24 writes one row per /24 for count /24s of 10.<second>.<from>.0, with one city.
func rows24(second, from, count int, country, city string) string {
	var b strings.Builder
	for i := from; i < from+count; i++ {
		fmt.Fprintf(&b, "10.%d.%d.0/24,%s,,%s,x\n", second, i, country, city)
	}
	return b.String()
}

// fillBuild builds an MMDB from layer rows (the CSV body without the header) and
// asn-prefixes rows (asn,prefix), with the ASN main city fill when asnRows is not nil.
func fillBuild(t *testing.T, dir, layerRows string, asnRows *string, index geoNames) (string, []geoLayerStats) {
	t.Helper()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), "prefix,country,region,city,source\n"+layerRows)
	asn := ""
	if asnRows != nil {
		asn = writeFile(t, filepath.Join(dir, "asn-prefixes.csv.gz"), "asn,prefix\n"+*asnRows)
	}
	out := filepath.Join(dir, "out.mmdb")
	stats, err := buildGeo([]string{layer}, "", out, false, nil, nil, nil, index, asn)
	if err != nil {
		t.Fatal(err)
	}
	return out, stats
}

func record(t *testing.T, r *maxminddb.Reader, ip string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := r.Lookup(netip.MustParseAddr(ip)).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// cityOf returns "country/city/source" of an address.
func cityOf(t *testing.T, r *maxminddb.Reader, ip string) string {
	m := record(t, r, ip)
	return fmt.Sprintf("%v/%v/%v", m["country_code"], m["city_name"], m["city_source"])
}

// dump lists every record of the MMDB (not the metadata, which holds the build time).
func dump(t *testing.T, path string) string {
	t.Helper()
	r, err := maxminddb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var b strings.Builder
	for _, within := range []string{"0.0.0.0/0", "2000::/3"} {
		for res := range r.NetworksWithin(netip.MustParsePrefix(within)) {
			var m map[string]any
			if err := res.Decode(&m); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s %v\n", res.Prefix(), m)
		}
	}
	return b.String()
}

func str(s string) *string { return &s }

func TestASNMainShareAndMinimum(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layer  string
		filled bool
	}{
		{"share 0.90 fills", rows24(0, 0, 90, "DE", "Berlin") + rows24(0, 90, 10, "DE", "Munich"), true},
		{"share 0.89 does not", rows24(0, 0, 89, "DE", "Berlin") + rows24(0, 89, 11, "DE", "Munich"), false},
		{"255 addresses do not", func() string {
			var b strings.Builder
			for i := 0; i < 255; i++ {
				fmt.Fprintf(&b, "10.0.0.%d/32,DE,,Berlin,x\n", i)
			}
			return b.String()
		}(), false},
		{"256 addresses do", func() string {
			var b strings.Builder
			for i := 0; i < 256; i++ {
				fmt.Fprintf(&b, "10.0.0.%d/32,DE,,Berlin,x\n", i)
			}
			return b.String()
		}(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// 10.0.200.0/24 has a country and no city, and lies in the ASN's prefix.
			out, stats := fillBuild(t, dir, tc.layer+"10.0.200.0/24,DE,,,x\n", str("100,10.0.0.0/16\n"), nil)
			r, err := maxminddb.Open(out)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			want := "DE/<nil>/<nil>"
			if tc.filled {
				want = "DE/Berlin/80-asn-main"
			}
			if got := cityOf(t, r, "10.0.200.7"); got != want {
				t.Errorf("filled address = %s, want %s", got, want)
			}
			last := stats[len(stats)-1]
			if last.name != asnMainSource {
				t.Fatalf("last layer = %q", last.name)
			}
			if tc.filled != (last.rows == 1 && last.prefixes == 1) {
				t.Errorf("stats = %+v", last)
			}
			wantNote := "asn-main: 0 groups qualified of 1, 0 addresses filled"
			if tc.filled {
				wantNote = "asn-main: 1 groups qualified of 1, 256 addresses filled"
			}
			if last.note != wantNote {
				t.Errorf("note = %q, want %q", last.note, wantNote)
			}
		})
	}
}

// With GeoNames, cities it cannot place are ignored for the counts: a group at 90% only
// when they are left out fills with an index and does not without.
func TestASNMainIgnoresUnplacedCities(t *testing.T) {
	dir := t.TempDir()
	geonames := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		geoNamesLine("2", "Berlin", "", "52.52437", "13.41053", "DE", "3426354")+"\n")
	index, err := loadGeoNames(geonames)
	if err != nil {
		t.Fatal(err)
	}
	// 80 Berlin, 20 Nowhereville (not in GeoNames): 80% of all, 100% of the placed.
	layer := rows24(0, 0, 80, "DE", "Berlin") + rows24(0, 80, 20, "DE", "Nowhereville") + "10.0.200.0/24,DE,,,x\n"
	asn := "100,10.0.0.0/16\n"
	for _, tc := range []struct {
		index geoNames
		want  string
	}{{index, "DE/Berlin/80-asn-main"}, {nil, "DE/<nil>/<nil>"}} {
		out, _ := fillBuild(t, t.TempDir(), layer, &asn, tc.index)
		r, _ := maxminddb.Open(out)
		if got := cityOf(t, r, "10.0.200.1"); got != tc.want {
			t.Errorf("index %v: got %s, want %s", tc.index != nil, got, tc.want)
		}
		if got := cityOf(t, r, "10.0.85.1"); got != "DE/Nowhereville/layer" {
			t.Errorf("unplaced city changed: %s", got)
		}
		r.Close()
	}
}

// The main city is the one with most addresses, whatever its spelling; names compare
// without case, and the most common spelling is written.
func TestASNMainCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	layer := rows24(0, 0, 50, "DE", "Berlin") + rows24(0, 50, 45, "DE", "BERLIN") + rows24(0, 95, 5, "DE", "Munich") + "10.0.200.0/24,DE,,,x\n"
	out, _ := fillBuild(t, dir, layer, str("100,10.0.0.0/16\n"), nil)
	r, _ := maxminddb.Open(out)
	defer r.Close()
	if got := cityOf(t, r, "10.0.200.1"); got != "DE/Berlin/80-asn-main" {
		t.Errorf("got %s", got)
	}
}

// Groups are per ASN and country; an ASN in two countries has two groups; an
// address of another country with no group of its own is not filled; a city is never
// replaced, a country never changed; IPv6 is untouched.
func TestASNMainGroupsAndFillOnly(t *testing.T) {
	dir := t.TempDir()
	layer := rows24(0, 0, 100, "DE", "Berlin") +
		rows24(1, 0, 100, "FR", "Paris") +
		"10.0.150.0/24,DE,,,x\n" + // DE no city: Berlin
		"10.1.150.0/24,FR,,,x\n" + // FR no city: Paris
		"10.0.151.0/24,ES,,,x\n" + // no ES group in this ASN: stays
		"10.0.152.0/24,DE,,Hamburg,x\n" + // has a city, a minority: stays
		"10.0.153.0/24,,,,x\n" + // no country: stays unknown
		"2a00::/32,DE,,,x\n"
	asn := "100,10.0.0.0/15\n100,2a00::/32\n"
	out, _ := fillBuild(t, dir, layer, &asn, nil)
	r, _ := maxminddb.Open(out)
	defer r.Close()
	for ip, want := range map[string]string{
		"10.0.150.1": "DE/Berlin/80-asn-main",
		"10.1.150.1": "FR/Paris/80-asn-main",
		"10.0.151.1": "ES/<nil>/<nil>",
		"10.0.152.1": "DE/Hamburg/layer",
		"10.0.5.1":   "DE/Berlin/layer",
		"10.0.153.1": "<nil>/<nil>/<nil>",
		"2a00::1":    "DE/<nil>/<nil>",
	} {
		if got := cityOf(t, r, ip); got != want {
			t.Errorf("%s = %s, want %s", ip, got, want)
		}
	}
}

// The most specific announced prefix decides the ASN; a prefix announced by two
// ASNs belongs to nobody and also shadows the broader prefix.
func TestASNMainAttribution(t *testing.T) {
	dir := t.TempDir()
	layer := rows24(0, 0, 100, "DE", "Berlin") + // AS100 (10.0.0.0/16): Berlin
		"10.0.200.0/23,DE,,Hamburg,x\n" + // AS200 (10.0.200.0/22): Hamburg
		"10.0.202.0/24,DE,,,x\n" + // no city, AS200's
		"10.0.150.0/24,DE,,,x\n" + // no city, AS100's
		"10.0.160.0/24,DE,,,x\n" // inside the multi-origin /24 below
	asn := "100,10.0.0.0/16\n200,10.0.200.0/22\n300,10.0.160.0/24\n400,10.0.160.0/24\n"
	out, _ := fillBuild(t, dir, layer, &asn, nil)
	r, _ := maxminddb.Open(out)
	defer r.Close()
	for ip, want := range map[string]string{
		"10.0.202.1": "DE/Hamburg/80-asn-main",
		"10.0.150.1": "DE/Berlin/80-asn-main",
		"10.0.160.1": "DE/<nil>/<nil>", // announced by two ASNs: nobody's, not AS100's
	} {
		if got := cityOf(t, r, ip); got != want {
			t.Errorf("%s = %s, want %s", ip, got, want)
		}
	}
}

// A cityless network partly inside an announced prefix is split.
func TestASNMainSplitsNetworks(t *testing.T) {
	dir := t.TempDir()
	layer := rows24(0, 0, 100, "DE", "Berlin") + "10.1.0.0/23,DE,,,x\n"
	asn := "100,10.0.0.0/16\n100,10.1.0.0/24\n"
	out, stats := fillBuild(t, dir, layer, &asn, nil)
	r, _ := maxminddb.Open(out)
	defer r.Close()
	if a, b := cityOf(t, r, "10.1.0.255"), cityOf(t, r, "10.1.1.0"); a != "DE/Berlin/80-asn-main" || b != "DE/<nil>/<nil>" {
		t.Errorf("split: %s, %s", a, b)
	}
	if last := stats[len(stats)-1]; last.rows != 1 || last.prefixes != 1 {
		t.Errorf("stats = %+v", last)
	}
}

// Same inputs, same bytes (apart from the build time); no qualifying group, same as no flag.
func TestASNMainDeterministicAndNoOp(t *testing.T) {
	layer := rows24(0, 0, 100, "DE", "Berlin") + rows24(1, 0, 100, "FR", "Paris") + "10.0.150.0/24,DE,,,x\n10.1.150.0/23,FR,,,x\n"
	asn := "100,10.0.0.0/15\n"
	a, _ := fillBuild(t, t.TempDir(), layer, &asn, nil)
	b, _ := fillBuild(t, t.TempDir(), layer, &asn, nil)
	if dump(t, a) != dump(t, b) {
		t.Error("two builds differ")
	}
	da, _ := os.ReadFile(a)
	db, _ := os.ReadFile(b)
	if len(da) != len(db) {
		t.Errorf("sizes %d, %d", len(da), len(db))
	}

	none, _ := fillBuild(t, t.TempDir(), layer, nil, nil)
	nothing := "100,10.99.0.0/16\n"
	empty, stats := fillBuild(t, t.TempDir(), layer, &nothing, nil)
	if dump(t, none) != dump(t, empty) {
		t.Error("a fill with no qualifying group changed the result")
	}
	dn, _ := os.ReadFile(none)
	de, _ := os.ReadFile(empty)
	if len(dn) != len(de) {
		t.Errorf("sizes %d, %d", len(dn), len(de))
	}
	if last := stats[len(stats)-1]; last.rows != 0 || last.prefixes != 0 || last.note != "asn-main: 0 groups qualified of 0, 0 addresses filled" {
		t.Errorf("stats = %+v", last)
	}
	if strings.Contains(dump(t, none), asnMainSource) || !strings.Contains(dump(t, a), asnMainSource) {
		t.Error("source names")
	}
}

// With GeoNames the filled record has coordinates like any layer's.
func TestASNMainCoordinates(t *testing.T) {
	dir := t.TempDir()
	geonames := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		geoNamesLine("2", "Berlin", "", "52.52437", "13.41053", "DE", "3426354")+"\n")
	index, err := loadGeoNames(geonames)
	if err != nil {
		t.Fatal(err)
	}
	layer := rows24(0, 0, 100, "DE", "Berlin") + "10.0.150.0/24,DE,,,x\n"
	asn := "100,10.0.0.0/16\n"
	out, stats := fillBuild(t, dir, layer, &asn, index)
	r, _ := maxminddb.Open(out)
	defer r.Close()
	m := record(t, r, "10.0.150.9")
	if m["latitude"] != 52.5244 || m["longitude"] != 13.4105 || m["city_source"] != asnMainSource {
		t.Errorf("record = %v", m)
	}
	// Berlin was made by the layer; the fill's record is new, so it is counted in the fill.
	if last := stats[len(stats)-1]; last.placed != 1 {
		t.Errorf("fill stats = %+v", last)
	}
}

func TestASNMainFileErrors(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), "prefix,country,region,city,source\n10.0.0.0/24,DE,,Berlin,x\n")
	for name, path := range map[string]string{
		"missing":    filepath.Join(dir, "missing.csv.gz"),
		"not gzip":   writeFile(t, filepath.Join(dir, "plain.csv.gz.txt"), "asn,prefix\n"),
		"no columns": writeFile(t, filepath.Join(dir, "cols.csv.gz"), "a,b\n1,2\n"),
	} {
		out := filepath.Join(dir, name+".mmdb")
		_, err := buildGeo([]string{layer}, "", out, false, nil, nil, nil, nil, path)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: error %v does not name %s", name, err, path)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s: output written", name)
		}
	}
}

func TestReadASNPrefixes(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "p.csv.gz"), "prefix,asn\n"+ // columns by name
		"10.0.0.0/8,1\n"+
		"10.1.0.0/16,2\n"+
		"10.1.2.0/24,3\n"+
		"10.1.2.0/24,4\n"+ // two ASNs: nobody's, and it shadows /16 and /8 there
		"10.2.0.0/16,5\n"+
		"10.2.0.0/16,5\n"+ // same ASN twice: fine
		"10.3.0.1/16,6\n"+ // host bits cleared
		"2a00::/32,7\n"+ // IPv6: not used
		"::ffff:10.9.0.0/112,8\n"+ // mapped: not used
		"10.4.0.0/16,0\n"+ // ASN 0: not used
		"bad,9\n10.5.0.0/16,x\n")
	segs, err := readASNPrefixes(path)
	if err != nil {
		t.Fatal(err)
	}
	u := func(a string) uint32 { lo, _ := prefixBounds(netip.MustParsePrefix(a + "/32")); return lo }
	// The /16 of AS2 is split around the /24 nobody owns; AS5 and AS6 sit inside AS1's /8.
	want := []asnSeg{
		{u("10.0.0.0"), u("10.0.255.255"), 1},
		{u("10.1.0.0"), u("10.1.1.255"), 2},
		{u("10.1.3.0"), u("10.1.255.255"), 2},
		{u("10.2.0.0"), u("10.2.255.255"), 5},
		{u("10.3.0.0"), u("10.3.255.255"), 6},
		{u("10.4.0.0"), u("10.255.255.255"), 1},
	}
	if !reflect.DeepEqual(segs, want) {
		t.Errorf("segments:\n%v\nwant\n%v", segs, want)
	}
}

func TestRangePrefixes(t *testing.T) {
	u := func(a string) uint32 { lo, _ := prefixBounds(netip.MustParsePrefix(a + "/32")); return lo }
	for _, tc := range []struct {
		lo, hi string
		want   string
	}{
		{"10.0.0.0", "10.0.0.255", "10.0.0.0/24"},
		{"10.0.0.1", "10.0.0.6", "10.0.0.1/32 10.0.0.2/31 10.0.0.4/31 10.0.0.6/32"},
		{"10.0.0.0", "10.0.2.255", "10.0.0.0/23 10.0.2.0/24"},
		{"0.0.0.0", "255.255.255.255", "0.0.0.0/0"},
		{"255.255.255.255", "255.255.255.255", "255.255.255.255/32"},
		{"128.0.0.0", "255.255.255.255", "128.0.0.0/1"},
	} {
		var got []string
		for _, p := range rangePrefixes(u(tc.lo), u(tc.hi)) {
			got = append(got, p.String())
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s-%s = %v, want %s", tc.lo, tc.hi, got, tc.want)
		}
	}
}

// A guessed city is not evidence: gaps keeps its /24 in the list, lookup -in leaves it out,
// and the plain report still shows it.
func TestASNMainIsNotEvidence(t *testing.T) {
	dir := t.TempDir()
	layer := rows24(0, 0, 100, "DE", "Berlin") + "10.0.150.0/24,DE,,,x\n"
	asn := "100,10.0.0.0/16\n"
	out, _ := fillBuild(t, dir, layer, &asn, nil)

	gapsOut := filepath.Join(dir, "gaps.txt")
	if _, err := writeGaps(out, gapsOut); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(gapsOut); string(data) != "10.0.150.0/24 1 DE\n" {
		t.Errorf("gaps = %q", data)
	}

	in := writeFile(t, filepath.Join(dir, "ips.txt"), "10.0.150.9\n10.0.5.5\n")
	citiesOut := filepath.Join(dir, "cities.tsv")
	if _, err := writeCities(out, in, citiesOut); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(citiesOut); string(data) != "10.0.5.5\tDE\tBerlin\n" {
		t.Errorf("cities = %q", data)
	}

	r, _ := maxminddb.Open(out)
	defer r.Close()
	if got := cityOf(t, r, "10.0.150.9"); got != "DE/Berlin/80-asn-main" {
		t.Errorf("the record itself = %s", got)
	}
}
