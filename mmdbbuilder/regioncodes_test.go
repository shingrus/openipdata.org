package main

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

// gnFile builds a cities1000.txt with made-up places.
type gnFile struct {
	b  strings.Builder
	id int
}

// place adds one place and returns its id.
func (g *gnFile) place(name, country, admin1 string, pop int) int {
	g.id++
	lat, lon := 40+float64(g.id)*0.05, 10+float64(g.id)*0.05
	g.b.WriteString(gnLine(strconv.Itoa(g.id), name, fmt.Sprintf("%.5f", lat), fmt.Sprintf("%.5f", lon), country, admin1, strconv.Itoa(pop), "Europe/Test"))
	return g.id
}

// cities adds n places with distinct names (prefix + a letter) in one admin1.
func (g *gnFile) cities(country, admin1, prefix string, n int) []string {
	var names []string
	for i := 0; i < n; i++ {
		name := prefix + string(rune('a'+i))
		g.place(name, country, admin1, 1000+i)
		names = append(names, name)
	}
	return names
}

// layerRows builds layer rows, one /24 each.
type layerRows struct {
	b strings.Builder
	n int
}

func (l *layerRows) add(country, region string, cities ...string) {
	for _, c := range cities {
		fmt.Fprintf(&l.b, "10.%d.%d.0/24,%s,%s,%s,x\n", l.n/256, l.n%256, country, region, c)
		l.n++
	}
}

func (l *layerRows) csv() string { return "prefix,country,region,city,source\n" + l.b.String() }

// placing loads the cities file, runs applyCityPlacing over the layer (and the full list when
// fullPath is not "") and returns the index and the counts.
func placing(t *testing.T, dir string, g *gnFile, layer *layerRows, fullPath string) (geoNames, string, string, regionStats, fullStats) {
	t.Helper()
	cities := writeFile(t, filepath.Join(dir, "cities1000.txt"), g.b.String())
	layerPath := writeFile(t, filepath.Join(dir, "10-main.csv.gz"), layer.csv())
	index, folded, err := loadGeoNamesOpts(cities, true, true)
	if err != nil {
		t.Fatal(err)
	}
	rs, fs, err := applyCityPlacing(index, folded, cities, []string{layerPath}, "", fullPath)
	if err != nil {
		t.Fatal(err)
	}
	return index, cities, layerPath, rs, fs
}

// learnedKeys lists the learned mappings of an index (the keys without a "|").
func learnedKeys(index geoNames) map[string]string {
	out := map[string]string{}
	for k, p := range index {
		if !strings.Contains(k, "|") {
			out[k] = p.tz
		}
	}
	return out
}

func TestIsoRegionCode(t *testing.T) {
	for _, tc := range []struct{ country, region, want string }{
		{"DE", "DE-SN", "SN"}, {"de", " de-sn ", "SN"}, {"IT", "IT-25", "25"}, {"CA", "CA-ON", "ON"},
		{"GB", "GB-ENG", "ENG"}, {"DE", "SN", ""}, {"DE", "FR-SN", ""}, {"DE", "DE-SNXX", ""},
		{"DE", "DE-S N", ""}, {"DE", "DE-", ""}, {"DE", "", ""}, {"DE", "DE-É1", ""},
		{"US", "US-ME", ""}, {"US", "ME", ""}, {"", "DE-SN", ""},
	} {
		if got := isoRegionCode(tc.country, tc.region); got != tc.want {
			t.Errorf("isoRegionCode(%q, %q) = %q, want %q", tc.country, tc.region, got, tc.want)
		}
	}
}

func TestLearnRegionCodes(t *testing.T) {
	var g gnFile
	var l layerRows
	// AT-A: five cities, all in admin1 01: learned.
	l.add("AT", "AT-A", g.cities("AT", "01", "ata", 5)...)
	// BE-B: four of five agree (80%): rejected.
	l.add("BE", "BE-B", g.cities("BE", "01", "beb", 4)...)
	l.add("BE", "BE-B", g.cities("BE", "02", "bex", 1)...)
	// CH-C: nine of ten agree (90%): accepted.
	l.add("CH", "CH-C", g.cities("CH", "01", "chc", 9)...)
	l.add("CH", "CH-C", g.cities("CH", "02", "chx", 1)...)
	// DK-D: four cities: too few.
	l.add("DK", "DK-D", g.cities("DK", "01", "dkd", 4)...)
	// ES-E: four unambiguous cities and three names with two places each: the ambiguous ones do
	// not vote, so four voters.
	l.add("ES", "ES-E", g.cities("ES", "01", "ese", 4)...)
	for _, n := range []string{"amb1", "amb2", "amb3"} {
		g.place(n, "ES", "01", 5)
		g.place(n, "ES", "02", 5)
		l.add("ES", "ES-E", n)
	}
	// FI-F: five unambiguous cities and three ambiguous names: the ambiguous ones do not count
	// against it either.
	l.add("FI", "FI-F", g.cities("FI", "01", "fif", 5)...)
	for _, n := range []string{"amb1", "amb2", "amb3"} {
		g.place(n, "FI", "01", 5)
		g.place(n, "FI", "02", 5)
		l.add("FI", "FI-F", n)
	}
	// FR: a bare code and another country's prefix never vote, and are not "seen".
	l.add("FR", "A", g.cities("FR", "01", "fra", 6)...)
	l.add("FR", "GR-A", g.cities("FR", "01", "frb", 6)...)
	// PL-P: four cities, one written three times: still four voters.
	pl := g.cities("PL", "01", "plp", 4)
	l.add("PL", "PL-P", pl...)
	l.add("PL", "PL-P", pl[0], pl[0])
	// IT: two region codes of one country, learned independently, and a mixed one.
	l.add("IT", "IT-M", g.cities("IT", "09", "itm", 5)...)
	l.add("IT", "IT-N", g.cities("IT", "10", "itn", 5)...)
	l.add("IT", "IT-X", g.cities("IT", "09", "itx", 3)...)
	l.add("IT", "IT-X", g.cities("IT", "10", "ity", 2)...)

	index, _, _, rs, _ := placing(t, t.TempDir(), &g, &l, "")
	if rs.learned != 5 || rs.seen != 10 {
		t.Errorf("stats = %+v, want 5 learned of 10 seen", rs)
	}
	for _, tc := range []struct{ cc, r, want string }{
		{"AT", "A", "01"}, {"BE", "B", ""}, {"CH", "C", "01"}, {"DK", "D", ""}, {"ES", "E", ""},
		{"FI", "F", "01"}, {"FR", "A", ""}, {"PL", "P", ""}, {"IT", "M", "09"}, {"IT", "N", "10"}, {"IT", "X", ""},
	} {
		if got := index.learnedAdmin1(tc.cc, tc.r); got != tc.want {
			t.Errorf("%s-%s = %q, want %q", tc.cc, tc.r, got, tc.want)
		}
	}

	// The same rows in the opposite order learn the same.
	lines := strings.Split(strings.TrimRight(l.b.String(), "\n"), "\n")
	var rev layerRows
	for i := len(lines) - 1; i >= 0; i-- {
		rev.b.WriteString(lines[i] + "\n")
	}
	index2, _, _, rs2, _ := placing(t, t.TempDir(), &g, &rev, "")
	if rs2 != rs || !reflect.DeepEqual(learnedKeys(index), learnedKeys(index2)) {
		t.Errorf("row order changed what was learned: %v vs %v", learnedKeys(index), learnedKeys(index2))
	}
}

// The learned keys and the regional places of other countries are skipped by every loop over
// the index (the RTT places and the ping cells take the country with plainKeyCountry).
func TestLearnedKeysAreSkippedByLoops(t *testing.T) {
	var g gnFile
	var l layerRows
	names := g.cities("DE", "13", "sn", 6)
	g.place("Neustadt", "DE", "13", 9000)
	g.place("Neustadt", "DE", "02", 100)
	l.add("DE", "DE-SN", names...)
	l.add("DE", "DE-SN", "Neustadt")
	index, cities, _, rs, _ := placing(t, t.TempDir(), &g, &l, "")
	if rs.learned != 1 {
		t.Fatalf("stats = %+v", rs)
	}
	plain, _, err := loadGeoNamesOpts(cities, false, true)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(ps []gnPlace) string {
		var s []string
		for _, p := range ps {
			s = append(s, fmt.Sprint(p.id))
		}
		return strings.Join(s, ",")
	}
	want := ids(newRTTChecker(plain, nil).places("DE"))
	if want == "" {
		t.Fatal("no DE places")
	}
	for i := 0; i < 30; i++ {
		if got := ids(newRTTChecker(index, nil).places("DE")); got != want {
			t.Fatalf("DE places = %s, want %s", got, want)
		}
	}
	for key := range index {
		if _, ok := plainKeyCountry(key); !ok && strings.Contains(key, "|") && !strings.Contains(key, ":") {
			t.Errorf("unexpected key %q", key)
		}
	}
}

// buildLayer builds an MMDB from a layer with the index and returns the records of the given
// addresses.
func buildLayer(t *testing.T, dir string, index geoNames, layerPath string, extra ...string) *maxminddb.Reader {
	t.Helper()
	out := filepath.Join(dir, "out.mmdb")
	if _, err := buildGeoNames(append([]string{layerPath}, extra...), "", out, false, nil, nil, nil, index, nil, "", false, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// A namesake is placed by a learned region like a US state; without the mapping, or for a row
// without a usable region, the default place stays; the US rule works in the same build.
func TestLearnedRegionPlacing(t *testing.T) {
	var g gnFile
	var l layerRows
	l.add("DE", "DE-SN", g.cities("DE", "13", "sn", 6)...)
	l.add("DE", "DE-BY", g.cities("DE", "02", "by", 6)...)
	l.add("DE", "DE-HH", g.cities("DE", "04", "hh", 4)...) // too few: not learned
	g.place("Neustadt", "DE", "13", 20000)                 // the default: the larger
	byID := g.place("Neustadt", "DE", "02", 5000)
	g.place("Portland", "US", "OR", 650000)
	g.place("Portland", "US", "ME", 66000)
	first := l.n
	l.add("DE", "DE-BY", "Neustadt") // the Bavarian one
	l.add("DE", "DE-SN", "Neustadt") // the default's own region
	l.add("DE", "", "Neustadt")      // no region
	l.add("DE", "DE-XX", "Neustadt") // a region that was never learned
	l.add("DE", "BY", "Neustadt")    // a bare code is not used outside the US
	l.add("DE", "FR-BY", "Neustadt") // another country's prefix
	l.add("DE", "DE-HH", "Neustadt") // below the thresholds
	l.add("US", "US-ME", "Portland") // the US rule, in the same build
	dir := t.TempDir()
	index, cities, layerPath, rs, _ := placing(t, dir, &g, &l, "")
	if rs.learned != 2 || rs.seen != 4 {
		t.Errorf("stats = %+v", rs)
	}
	r := buildLayer(t, dir, index, layerPath)
	ip := func(i int) string { return fmt.Sprintf("10.%d.%d.9", (first+i)/256, (first+i)%256) }
	bavaria, saxony := record(t, r, ip(0)), record(t, r, ip(1))
	if bavaria["city_region"] != "02" || bavaria["latitude"] != round4(40+float64(byID)*0.05) {
		t.Errorf("DE-BY Neustadt = %v", bavaria)
	}
	if saxony["city_region"] != nil || saxony["latitude"] == bavaria["latitude"] {
		t.Errorf("DE-SN Neustadt = %v", saxony)
	}
	for i := 2; i < 7; i++ {
		if m := record(t, r, ip(i)); m["city_region"] != nil || m["latitude"] != saxony["latitude"] {
			t.Errorf("row %d = %v, want the default place", i, m)
		}
	}
	if m := record(t, r, ip(7)); m["city_region"] != "ME" {
		t.Errorf("Portland ME = %v", m)
	}

	// Without the mapping (the index as loaded) the Bavarian row is the default.
	plain, _, err := loadGeoNamesOpts(cities, true, true)
	if err != nil {
		t.Fatal(err)
	}
	r2 := buildLayer(t, t.TempDir(), plain, layerPath)
	if m := record(t, r2, ip(0)); m["city_region"] != nil || m["latitude"] != saxony["latitude"] {
		t.Errorf("without a mapping = %v", m)
	}
	if m := record(t, r2, ip(7)); m["city_region"] != "ME" {
		t.Errorf("the US rule without a mapping = %v", m)
	}
}

// A guess that learns from a region-placed city carries the region.
func TestLearnedRegionGuess(t *testing.T) {
	var g gnFile
	var l layerRows
	l.add("DE", "DE-SN", g.cities("DE", "13", "sn", 6)...)
	l.add("DE", "DE-BY", g.cities("DE", "02", "by", 6)...)
	g.place("Neustadt", "DE", "13", 20000)
	g.place("Neustadt", "DE", "02", 5000)
	fmt.Fprintf(&l.b, "20.1.0.0/24,DE,DE-BY,Neustadt,x\n20.1.1.0/24,DE,,,x\n")
	dir := t.TempDir()
	index, _, layerPath, _, _ := placing(t, dir, &g, &l, "")
	asn := writeFile(t, filepath.Join(dir, "asn.csv.gz"), "asn,prefix\n100,20.1.0.0/16\n")
	out := filepath.Join(dir, "out.mmdb")
	if _, err := buildGeo([]string{layerPath}, "", out, false, nil, nil, nil, index, asn); err != nil {
		t.Fatal(err)
	}
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	m := record(t, r, "20.1.1.9")
	if m["city_source"] != "80-asn-main" || m["city_name"] != "Neustadt" || m["city_region"] != "02" {
		t.Errorf("guess = %v", m)
	}
}

// With no usable non-US region the build is the build without the learning (US rows too).
func TestLearnedRegionsNoChangeIsIdentical(t *testing.T) {
	var g gnFile
	var l layerRows
	l.add("DE", "DE-BY", g.cities("DE", "02", "by", 4)...) // below the thresholds
	g.place("Neustadt", "DE", "13", 20000)
	g.place("Neustadt", "DE", "02", 5000)
	g.place("Portland", "US", "OR", 650000)
	g.place("Portland", "US", "ME", 66000)
	l.add("DE", "DE-BY", "Neustadt")
	l.add("DE", "", "Neustadt")
	l.add("US", "US-ME", "Portland")
	l.add("US", "", "Portland")
	dir := t.TempDir()
	index, cities, layerPath, rs, _ := placing(t, dir, &g, &l, "")
	if rs.learned != 0 {
		t.Fatalf("stats = %+v", rs)
	}
	plain, _, err := loadGeoNamesOpts(cities, true, true)
	if err != nil {
		t.Fatal(err)
	}
	outA, outB := filepath.Join(t.TempDir(), "a.mmdb"), filepath.Join(t.TempDir(), "b.mmdb")
	for out, idx := range map[string]geoNames{outA: index, outB: plain} {
		if _, err := buildGeoNames([]string{layerPath}, "", out, false, nil, nil, nil, idx, nil, "", false, "", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if dump(t, outA) != dump(t, outB) {
		t.Error("the records differ")
	}
}

// allPlace is an allCountries.txt line with a chosen admin1.
func allPlace(id, name, lat, lon, class, code, country, admin1, pop string) string {
	return strings.Join([]string{id, name, name, "", lat, lon, class, code, country, "", admin1, "", "", "", pop, "", "10", "Europe/Test", "2024-01-01"}, "\t") + "\n"
}

// A usable region settles a name the full list leaves ambiguous country-wide, for a US state and
// for a learned region; a name still ambiguous inside the admin1 stays unplaced; a row without a
// region keeps the country-wide result; a region wins over the country-wide place.
func TestFullListSettledByRegion(t *testing.T) {
	var g gnFile
	var l layerRows
	l.add("DE", "DE-SN", g.cities("DE", "13", "sn", 6)...)
	l.add("DE", "DE-BY", g.cities("DE", "02", "by", 6)...)
	dir := t.TempDir()
	full := writeFile(t, filepath.Join(dir, "allCountries.txt"), strings.Join([]string{
		// Zeta Hill: in Maine and Texas, far apart: ambiguous in the US as a whole.
		allPlace("1", "Zeta Hill", "44.0", "-69.0", "P", "PPL", "US", "ME", "10"),
		allPlace("2", "Zeta Hill", "31.0", "-97.0", "P", "PPL", "US", "TX", "5"),
		// Eta Hill: two places 100 km apart in Maine, one in Texas.
		allPlace("3", "Eta Hill", "44.0", "-69.0", "P", "PPL", "US", "ME", "10"),
		allPlace("4", "Eta Hill", "44.9", "-69.0", "P", "PPL", "US", "ME", "9"),
		allPlace("5", "Eta Hill", "31.0", "-97.0", "P", "PPL", "US", "TX", "5"),
		// Theta: Saxony and Bavaria, far apart.
		allPlace("6", "Theta", "51.0", "13.0", "P", "PPL", "DE", "13", "10"),
		allPlace("7", "Theta", "48.0", "11.0", "P", "PPL", "DE", "02", "5"),
		// Iota: a populated place in Saxony and a division in Bavaria: unambiguous country-wide
		// (the place), but a row of Bavaria names the division.
		allPlace("8", "Iota", "51.5", "13.5", "P", "PPL", "DE", "13", "10"),
		allPlace("9", "Iota", "48.5", "11.5", "A", "ADM3", "DE", "02", "0"),
	}, ""))
	l.add("US", "US-ME", "Zeta Hill", "Eta Hill") // 44.0 -69.0 (Zeta); Eta stays ambiguous in Maine
	l.add("US", "US-TX", "Zeta Hill", "Eta Hill")
	l.add("US", "US-OR", "Zeta Hill") // no candidate in Oregon
	l.add("US", "", "Zeta Hill")      // no region
	l.add("DE", "DE-BY", "Theta", "Iota")
	l.add("DE", "DE-SN", "Theta", "Iota")
	l.add("DE", "DE-HH", "Theta", "Iota") // not learned
	l.add("DE", "", "Theta", "Iota")
	index, _, layerPath, rs, fs := placing(t, dir, &g, &l, full)
	if rs.learned != 2 {
		t.Fatalf("regions = %+v", rs)
	}
	// Names asked: Zeta Hill and Eta Hill (US), Theta and Iota (DE). Placed country-wide: Iota.
	if fs.placed != 1 || fs.ambiguous != 3 || fs.settled != 3 {
		t.Errorf("full = %+v", fs)
	}
	r := buildLayer(t, dir, index, layerPath)
	type want struct {
		lat    float64 // 0: no coordinates
		region string
	}
	rows := []want{
		{44.0, "ME"}, {0, ""}, // Zeta ME, Eta ME (ambiguous in Maine)
		{31.0, "TX"}, {31.0, "TX"}, // Zeta TX, Eta TX
		{0, ""},                    // OR
		{0, ""},                    // no region
		{48.0, "02"}, {48.5, "02"}, // DE-BY: Theta Bavaria, Iota the division
		{51.0, "13"}, {51.5, ""}, // DE-SN: Theta Saxony; Iota the country-wide place, which is Saxony's: no region
		{0, ""}, {51.5, ""}, // DE-HH: Theta ambiguous, Iota country-wide
		{0, ""}, {51.5, ""}, // no region
	}
	first := l.n - len(rows)
	for i, w := range rows {
		ip := fmt.Sprintf("10.%d.%d.9", (first+i)/256, (first+i)%256)
		m := record(t, r, ip)
		lat, _ := m["latitude"].(float64)
		reg, _ := m["city_region"].(string)
		if lat != w.lat || reg != w.region {
			t.Errorf("row %d (%s %v): latitude %v region %q, want %v %q", i, ip, m["city_name"], lat, reg, w.lat, w.region)
		}
	}
}

// convert gives a record placed by a region (a main-list place of another country, a full-list
// place) its id and time zone from the flat file alone; without -geonames-all the full-list one
// has none, and nothing else changes.
func TestConvertRegionPlaces(t *testing.T) {
	var g gnFile
	var l layerRows
	l.add("DE", "DE-SN", g.cities("DE", "13", "sn", 6)...)
	l.add("DE", "DE-BY", g.cities("DE", "02", "by", 6)...)
	g.place("Neustadt", "DE", "13", 20000)
	byID := g.place("Neustadt", "DE", "02", 5000)
	dir := t.TempDir()
	full := writeFile(t, filepath.Join(dir, "allCountries.txt"), strings.Join([]string{
		allPlace("6001", "Theta", "51.0", "13.0", "P", "PPL", "DE", "13", "10"),
		allPlace("6002", "Theta", "48.0", "11.0", "P", "PPL", "DE", "02", "5"),
		allPlace("6003", "Zeta Hill", "44.0", "-69.0", "P", "PPL", "US", "ME", "10"),
		allPlace("6004", "Zeta Hill", "31.0", "-97.0", "P", "PPL", "US", "TX", "5"),
	}, ""))
	first := l.n
	l.add("DE", "DE-BY", "Neustadt", "Theta")
	l.add("US", "US-TX", "Zeta Hill")
	index, cities, layerPath, _, _ := placing(t, dir, &g, &l, full)
	flat := filepath.Join(dir, "flat.mmdb")
	if _, err := buildGeoNames([]string{layerPath}, "", flat, false, nil, nil, nil, index, nil, "", false, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	read := func(path string, i int) (id uint, tz string, lat float64) {
		t.Helper()
		r, err := maxminddb.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var rec geoliteRecord
		ip := fmt.Sprintf("10.%d.%d.9", (first+i)/256, (first+i)%256)
		if err := r.Lookup(netip.MustParseAddr(ip)).Decode(&rec); err != nil {
			t.Fatal(err)
		}
		return rec.City.GeoNameID, rec.Location.TimeZone, rec.Location.Latitude
	}
	with := filepath.Join(dir, "with.mmdb")
	if _, err := convertMMDBAll(flat, with, cities, full); err != nil {
		t.Fatal(err)
	}
	if id, tz, _ := read(with, 0); id != uint(byID) || tz != "Europe/Test" {
		t.Errorf("Bavarian Neustadt = %d %q", id, tz)
	}
	if id, tz, lat := read(with, 1); id != 6002 || tz != "Europe/Test" || lat != 48.0 {
		t.Errorf("Theta = %d %q %v", id, tz, lat)
	}
	if id, tz, lat := read(with, 2); id != 6004 || tz != "Europe/Test" || lat != 31.0 {
		t.Errorf("Zeta Hill = %d %q %v", id, tz, lat)
	}
	without := filepath.Join(dir, "without.mmdb")
	if _, err := convertMMDB(flat, without, cities); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := read(without, 0); id != uint(byID) {
		t.Errorf("Bavarian Neustadt without the full list = %d", id)
	}
	if id, tz, lat := read(without, 1); id != 0 || tz != "" || lat != 48.0 {
		t.Errorf("Theta without the full list = %d %q %v", id, tz, lat)
	}
}
