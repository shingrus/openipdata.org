package main

import (
	"archive/zip"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang/v2"
)

// allLine is an allCountries.txt line: the class and code of the feature, the alternate names
// and the population matter here.
func allLine(id, name, alts, lat, lon, class, code, country, pop string) string {
	return strings.Join([]string{id, name, name, alts, lat, lon, class, code, country, "", "01", "", "", "", pop, "", "10", "Europe/Test", "2024-01-01"}, "\t") + "\n"
}

// fullMainList is the main list of the tests: one place, Lima, that the full list also has.
func fullMainIndex(t *testing.T, dir string) (geoNames, foldedNames) {
	t.Helper()
	path := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		gnLine("1", "Lima", "-12.04318", "-77.02824", "PE", "15", "9000000", "America/Lima")+
			gnLine("2", "Edinburgh", "55.95206", "-3.19648", "GB", "SCT", "464990", "Europe/London"))
	index, folded, err := loadGeoNamesOpts(path, true, true)
	if err != nil {
		t.Fatal(err)
	}
	return index, folded
}

// fullListLines covers every rule of the choice.
func fullListLines() []string {
	return []string{
		allLine("100", "Alpha", "", "10.0", "20.0", "P", "PPL", "GB", "10"),
		allLine("101", "Bravo", "", "11.0", "21.0", "A", "ADM3", "GB", "0"),
		allLine("102", "Charlie", "", "12.0", "22.0", "A", "ADM1", "GB", "999"), // not a level of 2 to 5
		allLine("103", "Delta", "", "13.0", "23.0", "H", "LK", "GB", "999"),     // another class
		allLine("104", "Echo", "", "14.0", "24.0", "A", "ADM2", "GB", "0"),
		allLine("105", "Foxtrot", "", "15.0", "25.0", "A", "ADM4", "GB", "5000"), // a division, more people ...
		allLine("106", "Foxtrot", "", "15.1", "25.1", "P", "PPLX", "GB", "10"),   // ... but a place wins
		allLine("107", "Golf Town", "", "16.0", "26.0", "P", "PPL", "GB", "10"),  // primary
		allLine("108", "Other", "Golf Town,Moscow", "16.2", "26.2", "P", "PPL", "GB", "9000"),
		allLine("109", "Hotel", "", "17.0", "27.0", "P", "PPL", "GB", "10"),
		allLine("110", "Hotel", "", "17.1", "27.1", "P", "PPL", "GB", "500"), // 14 km away: the larger
		allLine("111", "India", "", "18.0", "28.0", "P", "PPL", "GB", "10"),
		allLine("112", "India", "", "19.0", "28.0", "P", "PPL", "GB", "500"), // 111 km away: ambiguous
		allLine("113", "Juliet", "", "20.0", "29.0", "P", "PPL", "GB", "10"),
		allLine("114", "Juliet", "", "20.2", "29.0", "P", "PPL", "GB", "10"), // same people: the smaller id
		allLine("115", "Lima", "", "50.0", "5.0", "P", "PPL", "PE", "100"),   // the main list has Lima
		allLine("116", "Kilo", "Kilo Alt,Žilina", "21.0", "30.0", "P", "PPL", "GB", "10"),
		allLine("117", "Mike", "", "22.0", "31.0", "P", "PPL", "FR", "10"), // another country
		"too\tfew\tfields\n",
		"118\tNovember\tNovember\t\tnot-a-number\t5\tP\tPPL\tGB\t\t01\t\t\t\t10\t\t10\tEurope/Test\t2024\n",
		"abc\tOscar\tOscar\t\t23.0\t32.0\tP\tPPL\tGB\t\t01\t\t\t\t10\t\t10\tEurope/Test\t2024\n",
	}
}

func fullWantedNames() fullWanted {
	w := fullWanted{}
	for _, c := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf Town", "Hotel", "India", "Juliet", "Kilo", "Zilina", "Mike", "November", "Oscar", "Papa"} {
		w["GB|"+strings.ToLower(c)]++
	}
	w["FR|mike"] = 1
	delete(w, "GB|mike")
	w["PE|lima"] = 1 // the build never asks for it; the full list must not matter if it did
	return w
}

func TestAddFullPlacesRules(t *testing.T) {
	dir := t.TempDir()
	lines := fullListLines()
	txt := writeFile(t, filepath.Join(dir, "allCountries.txt"), strings.Join(lines, ""))
	index, _ := fullMainIndex(t, dir)
	st, err := addFullPlaces(index, fullWantedNames(), txt)
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		id      uint32
		primary bool
	}
	for name, w := range map[string]want{
		"GB|alpha": {100, true}, "GB|bravo": {101, true}, "GB|echo": {104, true},
		"GB|foxtrot": {106, true}, "GB|golf town": {107, true}, "GB|hotel": {110, true},
		"GB|juliet": {113, true}, "GB|kilo": {116, true}, "FR|mike": {117, true}, "PE|lima": {115, true},
	} {
		p, ok := index[fullKeyOf(name)]
		if !ok || p.id != w.id || p.primary != w.primary {
			t.Errorf("%s = %+v %v, want id %d", name, p, ok, w.id)
		}
	}
	for _, name := range []string{"GB|charlie", "GB|delta", "GB|india", "GB|zilina", "GB|november", "GB|oscar", "GB|papa"} {
		if _, ok := index[fullKeyOf(name)]; ok {
			t.Errorf("%s was placed", name)
		}
	}
	// An alternate nobody asked for is not added, and an accented alternate (Zilina against Žilina) is a different key.
	if _, ok := index["GB+|kilo alt"]; ok {
		t.Error("an alternate nobody asked for was added")
	}
	// P beats A, and the time zone comes with the place.
	if p := index["GB+|foxtrot"]; p.tz != "Europe/Test" || p.lat != 15.1 {
		t.Errorf("foxtrot = %+v", p)
	}
	// The main list is untouched: no plain key changed or appeared.
	if p, _ := index.place("PE", "Lima"); p.id != 1 {
		t.Errorf("Lima = %+v", p)
	}
	if p, ok := index.placeFull("PE", "Lima"); !ok || p.id != 1 {
		t.Errorf("Lima from placeFull = %+v", p) // the main list wins over the full list
	}
	if p, ok := index.placeOf(place{code: "GB", city: "ALPHA "}); !ok || p.id != 100 {
		t.Errorf("Alpha from placeOf = %+v", p)
	}
	if _, ok := index.place("GB", "Alpha"); ok {
		t.Error("place (the cleaner and the RTT sites) sees the fallback")
	}
	for key := range index {
		if cc, ok := plainKeyCountry(key); ok && strings.Contains(key, "+") {
			t.Errorf("plain key %q has the mark (%s)", key, cc)
		}
	}
	// 17 distinct wanted names: 10 placed, India ambiguous, the other six have no candidate.
	if st.placed != 10 || st.ambiguous != 1 || st.unplaced != 6 || st.rows != 10 {
		t.Errorf("stats = %+v", st)
	}
}

// The result does not depend on the order of the lines, and a zip gives the same as the text.
func TestAddFullPlacesOrderAndZip(t *testing.T) {
	dir := t.TempDir()
	lines := fullListLines()
	run := func(path string) map[string]gnPlace {
		index, _ := fullMainIndex(t, t.TempDir())
		if _, err := addFullPlaces(index, fullWantedNames(), path); err != nil {
			t.Fatal(err)
		}
		out := map[string]gnPlace{}
		for k, p := range index {
			if strings.Contains(k, "+|") {
				out[k] = p
			}
		}
		return out
	}
	base := run(writeFile(t, filepath.Join(dir, "a.txt"), strings.Join(lines, "")))
	if len(base) == 0 {
		t.Fatal("nothing placed")
	}
	slices.Reverse(lines)
	if got := run(writeFile(t, filepath.Join(dir, "b.txt"), strings.Join(lines, ""))); !reflect.DeepEqual(got, base) {
		t.Errorf("reversed lines: %v != %v", got, base)
	}
	rotated := append(slices.Clone(lines[5:]), lines[:5]...)
	if got := run(writeFile(t, filepath.Join(dir, "c.txt"), strings.Join(rotated, ""))); !reflect.DeepEqual(got, base) {
		t.Errorf("rotated lines differ")
	}
	zipPath := filepath.Join(dir, "allCountries.zip")
	writeZip(t, zipPath, "allCountries.txt", strings.Join(lines, ""), "readme.txt", "hello")
	if got := run(zipPath); !reflect.DeepEqual(got, base) {
		t.Errorf("zip differs from the text file")
	}
	// A zip with one other name is its only file; two files and none of the right name is an error.
	one := filepath.Join(dir, "one.zip")
	writeZip(t, one, "other.txt", strings.Join(lines, ""))
	if got := run(one); !reflect.DeepEqual(got, base) {
		t.Errorf("single-file zip differs")
	}
	two := filepath.Join(dir, "two.zip")
	writeZip(t, two, "a.txt", "x", "b.txt", "y")
	index, _ := fullMainIndex(t, t.TempDir())
	if _, err := addFullPlaces(index, fullWantedNames(), two); err == nil {
		t.Error("a zip without allCountries.txt was accepted")
	}
}

func writeZip(t *testing.T, path string, nameBody ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for i := 0; i < len(nameBody); i += 2 {
		w, err := zw.Create(nameBody[i])
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(nameBody[i+1]))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestAddFullPlacesBadFile(t *testing.T) {
	dir := t.TempDir()
	index, _ := fullMainIndex(t, dir)
	if _, err := addFullPlaces(index, fullWantedNames(), filepath.Join(dir, "missing.zip")); err == nil {
		t.Error("a missing file was accepted")
	}
	bad := writeFile(t, filepath.Join(dir, "bad.zip"), "PK\x03\x04 not really a zip")
	if _, err := addFullPlaces(index, fullWantedNames(), bad); err == nil {
		t.Error("a broken zip was accepted")
	}
	// Only broken lines, an over-long line and no final newline: nothing placed, no error.
	long := "1\t" + strings.Repeat("x", fullMaxLine+10) + "\n"
	last := strings.TrimSuffix(allLine("100", "Alpha", "", "10.0", "20.0", "P", "PPL", "GB", "10"), "\n")
	path := writeFile(t, filepath.Join(dir, "odd.txt"), "\n\r\nfoo\n"+long+last)
	st, err := addFullPlaces(index, fullWanted{"GB|alpha": 1, "GB|beta": 1}, path)
	if err != nil || st.placed != 1 || st.unplaced != 1 {
		t.Errorf("odd file: %+v %v", st, err)
	}
}

// buildFull runs what `geo -geonames-all` does: the full list, then the build.
func buildFull(t *testing.T, dir string, layers []string, mainIndex func() (geoNames, foldedNames), fullPath string, asn string) (string, fullStats, []geoLayerStats) {
	t.Helper()
	index, folded := mainIndex()
	var st fullStats
	if fullPath != "" {
		var err error
		if st, err = applyFullList(index, folded, layers, "", fullPath); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "out.mmdb")
	stats, err := buildGeoNames(layers, "", out, false, nil, nil, nil, index, folded, asn, asn != "", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return out, st, stats
}

func TestGeoNamesAllBuild(t *testing.T) {
	dir := t.TempDir()
	index0, folded0 := fullMainIndex(t, dir)
	mainIndex := func() (geoNames, foldedNames) {
		// A copy: addFullPlaces adds to the map.
		idx := geoNames{}
		for k, v := range index0 {
			idx[k] = v
		}
		return idx, folded0
	}
	full := writeFile(t, filepath.Join(dir, "allCountries.txt"), strings.Join(fullListLines(), ""))
	layer := writeFile(t, filepath.Join(dir, "10-main.csv.gz"), "prefix,country,region,city,source\n"+
		"10.1.1.0/24,GB,,Alpha,x\n"+
		"10.1.2.0/24,GB,,Bravo,x\n"+
		"10.1.3.0/24,GB,,Charlie,x\n"+
		"10.1.4.0/24,GB,,Edinburgh,x\n"+ // the main list places it
		"10.1.5.0/24,PE,,Lima,x\n"+ // the full list has another Lima: never used
		"10.1.6.0/24,GB,,Golf Town (UK),x\n"+ // the cleaner repairs it, and the cleaned name is looked up
		"10.1.7.0/24,GB,,India,x\n")
	out, st, stats := buildFull(t, dir, []string{layer}, mainIndex, full, "")
	if st != (fullStats{rows: 2, placed: 2, ambiguous: 1, unplaced: 2}) { // Alpha and Bravo; India; Charlie and the unclean name
		t.Errorf("stats = %+v", st)
	}
	_ = stats
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for ip, want := range map[string][3]any{
		"10.1.1.9": {"Alpha", 10.0, 20.0},
		"10.1.2.9": {"Bravo", 11.0, 21.0},
		"10.1.4.9": {"Edinburgh", 55.9521, -3.1965},
		"10.1.5.9": {"Lima", -12.0432, -77.0282},
	} {
		m := record(t, r, ip)
		if m["city_name"] != want[0] || m["latitude"] != want[1] || m["longitude"] != want[2] || m["city_region"] != nil {
			t.Errorf("%s = %v", ip, m)
		}
	}
	for _, ip := range []string{"10.1.3.9", "10.1.7.9"} { // ADM1 and the ambiguous name stay unplaced
		if m := record(t, r, ip); m["latitude"] != nil || m["city_name"] == nil {
			t.Errorf("%s = %v", ip, m)
		}
	}
	// "Golf Town (UK)" is cleaned to "Golf Town" only when the main list has it; it does not, so the
	// row keeps its name and is not placed (the cleaner is unchanged and the fallback never renames).
	if m := record(t, r, "10.1.6.9"); m["city_name"] != "Golf Town (UK)" || m["latitude"] != nil {
		t.Errorf("10.1.6.9 = %v", m)
	}
	// A full list that wants nothing changes nothing: the build equals one without the flag.
	quiet := writeFile(t, filepath.Join(dir, "quiet.csv.gz"), "prefix,country,region,city,source\n10.1.4.0/24,GB,,Edinburgh,x\n10.1.5.0/24,PE,,Lima,x\n")
	plain, _, _ := buildFull(t, t.TempDir(), []string{quiet}, mainIndex, "", "")
	same, st2, _ := buildFull(t, t.TempDir(), []string{quiet}, mainIndex, full, "")
	if dump(t, plain) != dump(t, same) || st2 != (fullStats{}) {
		t.Errorf("a list that wants nothing changed the build: %+v", st2)
	}
	if m := record2(t, plain, "10.1.4.9"); m["latitude"] != 55.9521 {
		t.Errorf("plain build: %v", m)
	}
}

func record2(t *testing.T, path, ip string) map[string]any {
	t.Helper()
	r, err := maxminddb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	return record(t, r, ip)
}

// A name the cleaner repairs is repaired as before, and the repaired name is the one looked up.
func TestGeoNamesAllAfterCleaning(t *testing.T) {
	dir := t.TempDir()
	index, folded := fullMainIndex(t, dir)
	full := writeFile(t, filepath.Join(dir, "allCountries.txt"),
		allLine("300", "Dunedin Small", "", "-45.9", "170.5", "P", "PPL", "NZ", "10"))
	layer := writeFile(t, filepath.Join(dir, "10-main.csv.gz"), "prefix,country,region,city,source\n"+
		"10.1.1.0/24,GB,,EDINBURGH (Scotland),x\n"+ // cleaned to Edinburgh by the main list
		"10.1.2.0/24,NZ,,Dunedin Small,x\n")
	st, err := applyFullList(index, folded, []string{layer}, "", full)
	if err != nil {
		t.Fatal(err)
	}
	if st.placed != 1 || st.unplaced != 0 || st.rows != 1 {
		t.Errorf("stats = %+v (the cleaned name must not be wanted)", st)
	}
	out := filepath.Join(dir, "out.mmdb")
	if _, err := buildGeoNames([]string{layer}, "", out, false, nil, nil, nil, index, folded, "", false, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if m := record(t, r, "10.1.1.9"); m["city_name"] != "Edinburgh" || m["latitude"] != 55.9521 {
		t.Errorf("cleaned = %v", m)
	}
	if m := record(t, r, "10.1.2.9"); m["latitude"] != -45.9 || m["longitude"] != 170.5 {
		t.Errorf("fallback = %v", m)
	}
}

func TestGeoNamesAllNeedsGeoNames(t *testing.T) {
	dir := t.TempDir()
	full := writeFile(t, filepath.Join(dir, "allCountries.txt"), "")
	if _, err := applyFullList(nil, nil, nil, "", full); err == nil || !strings.Contains(err.Error(), "-geonames-all needs -geonames") {
		t.Errorf("err = %v", err)
	}
	if _, err := convertMMDBAll("in.mmdb", filepath.Join(dir, "o.mmdb"), "", full); err == nil || !strings.Contains(err.Error(), "-geonames-all needs -geonames") {
		t.Errorf("convert err = %v", err)
	}
}

func TestGeoNamesAllHoldersAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	index, folded := fullMainIndex(t, dir)
	holders := writeFile(t, filepath.Join(dir, "holders.csv.gz"), "prefix,country,region,city,source\n"+
		"10.0.0.0/12,GB,,Alpha,x\n10.0.0.0/12,XX1,,Bravo,x\n")
	layer := writeFile(t, filepath.Join(dir, "10-main.csv.gz"), "prefix,country,region,city,source\n10.1.1.0/24,GB,,Edinburgh,x\n")
	full := writeFile(t, filepath.Join(dir, "allCountries.txt"), strings.Join(fullListLines(), ""))
	st, err := applyFullList(index, folded, []string{layer}, holders, full)
	if err != nil || st.placed != 1 || st.rows != 0 {
		t.Errorf("holders: %+v %v", st, err)
	}
	if _, ok := index["GB+|alpha"]; !ok {
		t.Error("the holder's city was not placed")
	}
	_, err = applyFullList(index, folded, []string{layer}, "", filepath.Join(dir, "missing.zip"))
	if err == nil || !strings.Contains(err.Error(), "missing.zip") {
		t.Errorf("missing file: %v", err)
	}
}

// A guess that learns from fallback-placed evidence carries the coordinates, and the RTT city
// check treats a fallback-placed record as placed.
func TestGeoNamesAllGuessesAndRTTCity(t *testing.T) {
	dir := t.TempDir()
	index, folded := fullMainIndex(t, dir)
	full := writeFile(t, filepath.Join(dir, "allCountries.txt"),
		allLine("100", "Alpha", "", "51.0", "-0.5", "P", "PPL", "GB", "10"))
	// AS100 has one /24 in Alpha and a cityless neighbour: 80-asn-main copies Alpha (256+ addresses).
	layer := writeFile(t, filepath.Join(dir, "10-main.csv.gz"), "prefix,country,region,city,source\n"+
		"10.1.0.0/24,GB,,Alpha,x\n10.1.1.0/24,GB,,,x\n")
	asn := writeFile(t, filepath.Join(dir, "asn.csv.gz"), "asn,prefix\n100,10.1.0.0/16\n")
	if _, err := applyFullList(index, folded, []string{layer}, "", full); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mmdb")
	if _, err := buildGeoNames([]string{layer}, "", out, false, nil, nil, nil, index, folded, asn, false, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	m := record2(t, out, "10.1.1.9")
	if m["city_source"] != "80-asn-main" || m["city_name"] != "Alpha" || m["latitude"] != 51.0 {
		t.Errorf("guess = %v", m)
	}

	// The RTT city check: 2 ms from London (a site of the main list) allows 300 km; Alpha at
	// 51.0, -0.5 is inside, so the record is possible and kept (it would be impossible if unplaced
	// records were skipped as "no coordinates"); Edinburgh is 530 km away and is replaced.
	main := writeFile(t, filepath.Join(dir, "20-main.csv.gz"), "prefix,country,region,city,source\n"+
		"10.2.1.0/24,GB,,Alpha,x\n10.2.2.0/24,GB,,Edinburgh,x\n")
	full2 := writeFile(t, filepath.Join(dir, "all2.txt"),
		allLine("100", "Alpha", "", "51.0", "-0.5", "P", "PPL", "GB", "10")+
			gnLine("200", "London", "51.50853", "-0.12574", "GB", "ENG", "8961989", "Europe/London"))
	idx2, f2 := fullMainIndex(t, t.TempDir())
	idx2["GB|london"] = gnPlace{lat: 51.50853, lon: -0.12574, pop: 8961989, primary: true, id: 200}
	if _, err := applyFullList(idx2, f2, []string{main}, "", full2); err != nil {
		t.Fatal(err)
	}
	rtt := writeFile(t, filepath.Join(dir, "rtt.csv.gz"), rttHeader+"10.2.1.0/24,GB,London,2\n10.2.2.0/24,GB,London,2\n")
	out2 := filepath.Join(dir, "out2.mmdb")
	if _, err := buildGeoNames([]string{main}, "", out2, false, nil, nil, nil, idx2, f2, "", false, "", "", rtt, "", ""); err != nil {
		t.Fatal(err)
	}
	checkCities(t, out2, map[string]string{
		"10.2.1.9": "GB/Alpha/20-main",
		"10.2.2.9": "GB/Alpha/87-rtt-country",
	})
}

// convert gives a record placed from the full list its geoname_id and time zone; without the
// flag it gets neither.
func TestConvertGeoNamesAll(t *testing.T) {
	dir := t.TempDir()
	index, folded := fullMainIndex(t, dir)
	full := writeFile(t, filepath.Join(dir, "allCountries.txt"),
		allLine("100", "Alpha", "", "10.0", "20.0", "P", "PPL", "GB", "10"))
	cities := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		gnLine("1", "Lima", "-12.04318", "-77.02824", "PE", "15", "9000000", "America/Lima"))
	layer := writeFile(t, filepath.Join(dir, "10-main.csv.gz"), "prefix,country,region,city,source\n10.1.1.0/24,GB,,Alpha,x\n10.1.2.0/24,PE,,Lima,x\n10.1.3.0/24,GB,,Nowhere,x\n")
	if _, err := applyFullList(index, folded, []string{layer}, "", full); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(dir, "flat.mmdb")
	if _, err := buildGeoNames([]string{layer}, "", flat, false, nil, nil, nil, index, folded, "", false, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	read := func(path, ip string) (id uint, tz string, lat float64) {
		t.Helper()
		r, err := maxminddb.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var rec geoliteRecord
		if err := r.Lookup(netip.MustParseAddr(ip)).Decode(&rec); err != nil {
			t.Fatal(err)
		}
		return rec.City.GeoNameID, rec.Location.TimeZone, rec.Location.Latitude
	}
	with := filepath.Join(dir, "with.mmdb")
	if _, err := convertMMDBAll(flat, with, cities, full); err != nil {
		t.Fatal(err)
	}
	if id, tz, lat := read(with, "10.1.1.9"); id != 100 || tz != "Europe/Test" || lat != 10.0 {
		t.Errorf("Alpha = %d %q %v", id, tz, lat)
	}
	if id, tz, _ := read(with, "10.1.2.9"); id != 1 || tz != "America/Lima" {
		t.Errorf("Lima = %d %q", id, tz)
	}
	if id, tz, _ := read(with, "10.1.3.9"); id != 0 || tz != "" {
		t.Errorf("Nowhere = %d %q", id, tz)
	}
	without := filepath.Join(dir, "without.mmdb")
	if _, err := convertMMDB(flat, without, cities); err != nil {
		t.Fatal(err)
	}
	if id, tz, lat := read(without, "10.1.1.9"); id != 0 || tz != "" || lat != 10.0 {
		t.Errorf("Alpha without the flag = %d %q %v", id, tz, lat)
	}
	if _, err := convertMMDBAll(flat, with, cities, filepath.Join(dir, "missing.zip")); err == nil || !strings.Contains(err.Error(), "missing.zip") {
		t.Errorf("missing list: %v", err)
	}
	// An ip2asn input has nothing to place.
	asn := writeFlatMMDB(t, "ip2asn", map[string]mmdbtype.Map{"1.0.0.0/24": {"asn": mmdbtype.String("1")}})
	if _, err := convertMMDBAll(asn, with, cities, full); err == nil {
		t.Error("ip2asn with -geonames-all: no error")
	}
}

// failAfter gives its data, then fails with err.
type failAfter struct {
	data []byte
	err  error
}

func (f *failAfter) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

// A read error inside an over-long line is returned, not swallowed; an over-long line that ends
// at the end of the file is fine, and so is a good line before it.
func TestScanFullListReadErrors(t *testing.T) {
	wanted := fullWanted{"GB|alpha": 1}
	good := allLine("100", "Alpha", "", "10.0", "20.0", "P", "PPL", "GB", "10")
	long := []byte(good + "1\t" + strings.Repeat("x", fullMaxLine+10))
	boom := errors.New("flate: corrupt input")
	if _, err := scanFullList(&failAfter{long, boom}, wanted); !errors.Is(err, boom) {
		t.Errorf("error in a long line: %v", err)
	}
	if _, err := scanFullList(&failAfter{[]byte(good), boom}, wanted); !errors.Is(err, boom) {
		t.Errorf("error after a line: %v", err)
	}
	cands, err := scanFullList(&failAfter{long, io.EOF}, wanted)
	if err != nil || len(cands["GB|alpha"]) != 1 {
		t.Errorf("long line at EOF: %v %v", cands, err)
	}
}
