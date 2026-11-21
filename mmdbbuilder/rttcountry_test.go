package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

const rttHeader = "prefix,country,city,rtt_ms\n"

// rttIndex is a tiny GeoNames index: London and Paris, New York and Dallas, Sydney.
func rttIndex(t *testing.T, dir string) geoNames {
	t.Helper()
	path := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		geoNamesLine("1", "London", "", "51.50853", "-0.12574", "GB", "8961989")+
			geoNamesLine("2", "Paris", "", "48.85341", "2.3488", "FR", "2138551")+
			geoNamesLine("3", "New York City", "", "40.71427", "-74.00597", "US", "8175133")+
			geoNamesLine("4", "Dallas", "", "32.78306", "-96.80667", "US", "1300000")+
			geoNamesLine("5", "Sydney", "", "-33.86785", "151.20732", "AU", "4627345"))
	index, err := loadGeoNames(path)
	if err != nil {
		t.Fatal(err)
	}
	return index
}

// rttBuild builds an MMDB from layer rows and RTT rows ("" for no -rtt-country).
func rttBuild(t *testing.T, dir, layerRows, rttRows string, fallback bool) (string, error) {
	t.Helper()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), holderHeader+layerRows)
	rtt := ""
	if rttRows != "" {
		rtt = writeFile(t, filepath.Join(dir, "speedtest-rtt.csv.gz"), rttHeader+rttRows)
	}
	out := filepath.Join(dir, "out.mmdb")
	_, err := buildGeoGuesses([]string{layer}, "", out, false, nil, nil, nil, rttIndex(t, dir), "", fallback, "", rtt)
	return out, err
}

func TestRTTCountry(t *testing.T) {
	layer := "10.1.1.0/24,US,,Dallas,x\n" + // London at 8 ms: impossible, changes to GB
		"10.1.2.0/24,GB,,London,x\n" + // London at 8 ms: fine
		"10.1.3.0/24,US,,Dallas,x\n" + // London 8 ms, Sydney 9 ms: the fastest country is impossible too
		"10.1.4.0/24,JP,,Osaka,x\n" + // no place of JP in the index: never impossible
		"10.1.5.0/24,US,,Dallas,x\n" + // a tie between FR and GB: FR (the smaller code)
		"10.1.6.0/24,US,,Dallas,x\n" + // only bad rows
		"10.1.7.0/24,US,,Dallas,x\n" + // no row at all
		"10.1.8.0/24,US,,Dallas,x\n" // New York at 8 ms: US is fine
	rtt := "10.1.1.0/24,GB,London,8\n" +
		"10.1.2.0/24,GB,London,8\n" +
		"10.1.3.0/24,GB,London,8\n10.1.3.0/24,AU,Sydney,9\n" +
		"10.1.4.0/24,GB,London,8\n" +
		"10.1.5.0/24,GB,London,8\n10.1.5.0/24,FR,Paris,8\n" +
		"10.1.6.0/23,GB,London,8\n" + // not a /24
		"10.1.6.0/24,GB,London,31\n" + // above 30 ms
		"10.1.6.0/24,GB,Nowhereville,8\n" + // not placed
		"10.1.6.0/24,ZZ,London,8\n" + // not a country
		"10.1.6.0/24,GB,London,NaN\n" +
		"10.1.6.0/24,GB,London,0\n" +
		"10.1.6.1/24,GB,London,8\n" + // host bits
		"10.1.6.0/24,GB,,8\n" +
		"10.1.8.0/24,US,New York City,8\n"
	dir := t.TempDir()
	out, err := rttBuild(t, dir, layer, rtt, false)
	if err != nil {
		t.Fatal(err)
	}
	checkCities(t, out, map[string]string{
		"10.1.1.9": "GB/<nil>/<nil>",
		"10.1.2.9": "GB/London/layer",
		"10.1.3.9": "US/Dallas/layer",
		"10.1.4.9": "JP/Osaka/layer",
		"10.1.5.9": "FR/<nil>/<nil>",
		"10.1.6.9": "US/Dallas/layer",
		"10.1.7.9": "US/Dallas/layer",
		"10.1.8.9": "US/Dallas/layer",
	})
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if m := record(t, r, "10.1.1.9"); m["country_name"] == nil || m["country_name"] == "" || m["latitude"] != nil {
		t.Errorf("record of the changed /24: %v", m)
	}
	// The changed record is the one a country-only layer row of GB has.
	other, err := rttBuild(t, t.TempDir(), "10.1.1.0/24,GB,,,x\n", "", false)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := maxminddb.Open(other)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	a, b := record(t, r, "10.1.1.9"), record(t, r2, "10.1.1.9")
	if len(a) != len(b) || a["country_code"] != b["country_code"] || a["country_name"] != b["country_name"] {
		t.Errorf("record %v, a country-only row gives %v", a, b)
	}
}

func TestRTTCountryReader(t *testing.T) {
	dir := t.TempDir()
	index := rttIndex(t, dir)
	path := writeFile(t, filepath.Join(dir, "r.csv.gz"), rttHeader+
		"10.1.1.0/24,GB,London,8\n10.1.1.0/24,GB,London,7.5\n"+
		"bad,GB,London,8\n10.1.2.0/24,gb,London,30\n10.1.2.0/24,GB,London,30.1\n")
	rows, sites, skipped, err := readRTT(path, index)
	if err != nil || len(rows) != 3 || len(sites) != 1 || skipped != 2 {
		t.Fatalf("%v rows, %v sites, %d skipped, %v", rows, sites, skipped, err)
	}
	if rows[0].rtt != 7.5 || rows[1].rtt != 8 { // sorted by time inside a /24
		t.Errorf("rows = %v", rows)
	}
	for _, bad := range []string{"prefix,country,city\n", "a,b\n"} {
		p := writeFile(t, filepath.Join(dir, "h.csv.gz"), bad)
		if _, _, _, err := readRTT(p, index); err == nil {
			t.Errorf("header %q accepted", bad)
		}
	}
}

// Without the flag, and with a file that changes nothing, the output is the same.
func TestRTTCountryOffIsIdentical(t *testing.T) {
	layer := "10.1.1.0/24,US,,Dallas,x\n10.1.2.0/24,GB,,London,x\n"
	a, err := rttBuild(t, t.TempDir(), layer, "", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := rttBuild(t, t.TempDir(), layer, "10.1.1.0/23,GB,London,8\n10.1.2.0/24,GB,London,8\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if dump(t, a) != dump(t, b) {
		t.Errorf("outputs differ:\n%s\n%s", dump(t, a), dump(t, b))
	}
}

func TestRTTCountryNeedsGeoNames(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), holderHeader+"10.1.1.0/24,US,,Dallas,x\n")
	rtt := writeFile(t, filepath.Join(dir, "r.csv.gz"), rttHeader+"10.1.1.0/24,GB,London,8\n")
	_, err := buildGeoGuesses([]string{layer}, "", filepath.Join(dir, "o.mmdb"), false, nil, nil, nil, nil, "", false, "", rtt)
	if err == nil || !strings.Contains(err.Error(), "-rtt-country needs -geonames") {
		t.Errorf("err = %v", err)
	}
	if _, err := buildGeoGuesses([]string{layer}, "", filepath.Join(dir, "o.mmdb"), false, nil, nil, nil, rttIndex(t, dir), "", false, "", filepath.Join(dir, "none.csv.gz")); err == nil {
		t.Error("a missing file was accepted")
	}
}

// The guesses run after the check: the country-main city of GB fills the changed /24.
func TestRTTCountryGuessesFollow(t *testing.T) {
	layer := rows24(2, 0, 10, "GB", "London") + "10.1.1.0/24,US,,,x\n"
	out, err := rttBuild(t, t.TempDir(), layer, "10.1.1.0/24,GB,London,8\n", true)
	if err != nil {
		t.Fatal(err)
	}
	checkCities(t, out, map[string]string{"10.1.1.9": "GB/London/86-country-main"})
}
