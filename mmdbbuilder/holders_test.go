package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const holderHeader = "prefix,country,region,city,source\n"

// holderBuild builds an MMDB from layer rows, asn-prefixes rows and holders rows (nil: no
// -holders), with or without -fallback. -asn-main is always on, as -holders needs it.
func holderBuild(t *testing.T, dir, layerRows, asnRows string, holderRows *string, fallback bool) (string, []geoLayerStats) {
	t.Helper()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), holderHeader+layerRows)
	asn := writeFile(t, filepath.Join(dir, "asn-prefixes.csv.gz"), "asn,prefix\n"+asnRows)
	holders := ""
	if holderRows != nil {
		holders = writeFile(t, filepath.Join(dir, "holders.csv.gz"), holderHeader+*holderRows)
	}
	out := filepath.Join(dir, "out.mmdb")
	stats, err := buildGeoGuesses([]string{layer}, "", out, false, nil, nil, nil, nil, asn, fallback, holders, "")
	if err != nil {
		t.Fatal(err)
	}
	return out, stats
}

func TestHolderCityUnannouncedOnly(t *testing.T) {
	// One cityless /16. AS100 announces the first /24, AS200 and AS300 both announce the second.
	// Berlin is the main city of DE, so the country rung gives it to what rung 85 does not take.
	layer := "10.0.0.0/16,DE,,,x\n" + rows24(1, 0, 10, "DE", "Berlin")
	asn := "100,10.0.0.0/24\n200,10.0.1.0/24\n300,10.0.1.0/24\n"
	holders := "10.0.0.0/16,DE,,Bonn,h\n"
	out, stats := holderBuild(t, t.TempDir(), layer, asn, &holders, true)
	checkCities(t, out, map[string]string{
		"10.0.0.9":     "DE/Berlin/86-country-main", // announced by one ASN: as before
		"10.0.1.9":     "DE/Berlin/86-country-main", // announced by two ASNs: as before
		"10.0.2.0":     "DE/Bonn/85-holder-city",
		"10.0.255.255": "DE/Bonn/85-holder-city",
	})
	s := statsOf(t, stats, holderCitySource)
	if s.rows != 1 || s.note != "holder-city: 1 blocks, 65024 addresses filled" {
		t.Errorf("85 stats = %+v", s)
	}
	var names []string
	for _, st := range stats {
		names = append(names, st.name)
	}
	if got := strings.Join(names, " "); !strings.Contains(got, "84-asn-near 85-holder-city 86-country-main") {
		t.Errorf("order = %s", got)
	}
}

func TestHolderCityOtherCountry(t *testing.T) {
	layer := "10.0.0.0/24,DE,,,x\n" + rows24(1, 0, 10, "DE", "Berlin")
	holders := "10.0.0.0/16,FR,,Paris,h\n"
	out, stats := holderBuild(t, t.TempDir(), layer, "100,10.9.0.0/16\n", &holders, true)
	checkCities(t, out, map[string]string{"10.0.0.9": "DE/Berlin/86-country-main"})
	if s := statsOf(t, stats, holderCitySource); s.rows != 0 {
		t.Errorf("85 stats = %+v", s)
	}
}

func TestHolderCityMostSpecificAndLastRow(t *testing.T) {
	layer := "10.0.0.0/16,DE,,,x\n"
	holders := "10.0.0.0/16,DE,,Bonn,h\n" +
		"10.0.1.0/24,DE,,Essen,h\n" +
		"10.0.2.0/24,DE,,Hamburg,h\n" +
		"10.0.2.0/24,DE,,Cologne,h\n" // the same prefix twice: the last row wins
	out, _ := holderBuild(t, t.TempDir(), layer, "100,10.9.0.0/16\n", &holders, false)
	checkCities(t, out, map[string]string{
		"10.0.0.9": "DE/Bonn/85-holder-city",
		"10.0.1.9": "DE/Essen/85-holder-city",
		"10.0.2.9": "DE/Cologne/85-holder-city",
		"10.0.3.9": "DE/Bonn/85-holder-city",
	})
}

func TestHolderCityOnlyFills(t *testing.T) {
	layer := "10.0.0.0/24,DE,,Leipzig,x\n10.0.1.0/24,DE,,,x\n10.1.0.0/24,DE,,,x\n" + rows24(2, 0, 10, "DE", "Berlin")
	holders := "10.0.0.0/23,DE,,Bonn,h\n"
	out, _ := holderBuild(t, t.TempDir(), layer, "100,10.9.0.0/16\n", &holders, true)
	checkCities(t, out, map[string]string{
		"10.0.0.9": "DE/Leipzig/layer",
		"10.0.1.9": "DE/Bonn/85-holder-city",
		"10.1.0.9": "DE/Berlin/86-country-main", // outside every block
	})
}

// With the holder fill counted, Berlin would hold most of DE; it must not count.
func TestHolderCityIsNoEvidence(t *testing.T) {
	layer := rows24(1, 0, 25, "DE", "Berlin") + rows24(2, 0, 25, "DE", "Munich") + rows24(3, 0, 25, "DE", "Hamburg") +
		rows24(4, 0, 25, "DE", "Cologne") + "10.8.0.0/16,DE,,,x\n10.9.0.0/24,DE,,,x\n"
	holders := "10.8.0.0/16,DE,,Berlin,h\n"
	out, stats := holderBuild(t, t.TempDir(), layer, "100,10.200.0.0/16\n", &holders, true)
	checkCities(t, out, map[string]string{
		"10.8.0.9": "DE/Berlin/85-holder-city",
		"10.9.0.9": "DE/<nil>/<nil>",
	})
	if s := statsOf(t, stats, countryMainSource); s.rows != 0 || s.note != "country-main: 0 countries qualified of 1, 0 addresses filled" {
		t.Errorf("86 stats = %+v", s)
	}
}

func TestHolderCityUnusableRows(t *testing.T) {
	layer := "10.0.0.0/24,DE,,,x\n"
	holders := "2001:db8::/32,DE,,Bonn,h\n" + // IPv6
		"10.0.0.0/7,DE,,Bonn,h\n" + // broader than a /8
		"10.0.0.0/16,D1,,Bonn,h\n" + // bad country
		"10.0.0.0/16,DEU,,Bonn,h\n" +
		"10.0.0.0/16,DE,,,h\n" + // no city
		"10.0.0.0/16,DE,,<b>,h\n" + // city cleanCity refuses
		"not-a-prefix,DE,,Bonn,h\n" +
		"10.0.0.0/16\n" + // short row
		" 10.0.0.0/16 , de ,,Bonn,h\n" // usable: trimmed, upper-cased
	out, stats := holderBuild(t, t.TempDir(), layer, "100,10.9.0.0/16\n", &holders, false)
	checkCities(t, out, map[string]string{"10.0.0.9": "DE/Bonn/85-holder-city"})
	s := statsOf(t, stats, holderCitySource)
	if s.skipped != 8 || s.note != "holder-city: 1 blocks, 256 addresses filled" {
		t.Errorf("85 stats = %+v", s)
	}

	for name, body := range map[string]string{"no usable row": "2001:db8::/32,DE,,Bonn,h\n", "header only": ""} {
		t.Run(name, func(t *testing.T) {
			out, stats := holderBuild(t, t.TempDir(), layer, "100,10.9.0.0/16\n", &body, false)
			checkCities(t, out, map[string]string{"10.0.0.9": "DE/<nil>/<nil>"})
			if s := statsOf(t, stats, holderCitySource); s.rows != 0 || s.note != "holder-city: 0 blocks, 0 addresses filled" {
				t.Errorf("85 stats = %+v", s)
			}
		})
	}
}

func TestHolderFileErrors(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), holderHeader+"10.0.0.0/24,DE,,,x\n")
	asn := writeFile(t, filepath.Join(dir, "asn.csv.gz"), "asn,prefix\n100,10.9.0.0/16\n")
	missing := filepath.Join(dir, "missing.csv.gz")
	noHeader := writeFile(t, filepath.Join(dir, "bad.csv.gz"), "a,b\n1,2\n")
	for _, path := range []string{missing, noHeader} {
		_, err := buildGeoGuesses([]string{layer}, "", filepath.Join(dir, "o.mmdb"), false, nil, nil, nil, nil, asn, false, path, "")
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("error for %s = %v", path, err)
		}
	}
	if _, err := buildGeoGuesses([]string{layer}, "", filepath.Join(dir, "o.mmdb"), false, nil, nil, nil, nil, "", false, noHeader, ""); err == nil {
		t.Error("holders without ASN prefixes must fail")
	}
}

// Without -holders the output is what it was; a holders file that gives nothing changes nothing.
func TestHolderCityOffIsIdentical(t *testing.T) {
	layer, asn := ladderLayer()
	for _, fallback := range []bool{false, true} {
		dirA, dirB := t.TempDir(), t.TempDir()
		outA, _ := holderBuild(t, dirA, layer, asn, nil, fallback)
		empty := "10.99.0.0/16,DE,,Bonn,h\n" // covers no cityless address
		outB, _ := holderBuild(t, dirB, layer, asn, &empty, fallback)
		if a, b := dump(t, outA), dump(t, outB); a != b {
			t.Errorf("fallback=%v: output differs", fallback)
		}
		// The same inputs through buildGeo (no fallback, no holders) match the plain build.
		if !fallback {
			plain := filepath.Join(dirA, "plain.mmdb")
			if _, err := buildGeo([]string{filepath.Join(dirA, "layer.csv.gz")}, "", plain, false, nil, nil, nil, nil, filepath.Join(dirA, "asn-prefixes.csv.gz")); err != nil {
				t.Fatal(err)
			}
			if dump(t, plain) != dump(t, outA) {
				t.Error("buildGeo output differs")
			}
		}
	}
}

func TestHolderCityIsAGuess(t *testing.T) {
	dir := t.TempDir()
	layer := rows24(0, 0, 3, "DE", "Berlin") + "10.5.0.0/24,DE,,,x\n"
	holders := "10.5.0.0/24,DE,,Bonn,h\n"
	out, _ := holderBuild(t, dir, layer, "100,10.9.0.0/16\n", &holders, false)
	checkCities(t, out, map[string]string{"10.5.0.9": "DE/Bonn/85-holder-city"})
	gapsOut := filepath.Join(dir, "gaps.txt")
	if _, err := writeGaps(out, gapsOut); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(gapsOut); string(data) != "10.5.0.0/24 1 DE\n" {
		t.Errorf("gaps = %q", data)
	}
	in := writeFile(t, filepath.Join(dir, "ips.txt"), "10.5.0.9\n10.0.1.1\n")
	citiesOut := filepath.Join(dir, "cities.tsv")
	if _, err := writeCities(out, in, citiesOut); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(citiesOut); string(data) != "10.0.1.1\tDE\tBerlin\n" {
		t.Errorf("cities = %q", data)
	}
}

func TestReadASNDataAnnounced(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "asn.csv.gz"), "asn,prefix\n"+
		"1,10.0.0.0/24\n2,10.0.1.0/24\n"+ // neighbours merge
		"3,10.0.4.0/24\n4,10.0.4.0/24\n"+ // announced by two ASNs: no segment, still announced
		"5,10.0.4.0/25\n"+
		"6,10.0.8.0/24\n6,10.0.8.0/25\n"+ // nested: inside the first
		"7,2a00::/32\n")
	segs, announced, err := readASNData(path)
	if err != nil {
		t.Fatal(err)
	}
	u := func(a string) uint32 { lo, _ := prefixBounds(netip.MustParsePrefix(a + "/32")); return lo }
	wantAnnounced := []addrRange{
		{u("10.0.0.0"), u("10.0.1.255")},
		{u("10.0.4.0"), u("10.0.4.255")},
		{u("10.0.8.0"), u("10.0.8.255")},
	}
	if !reflect.DeepEqual(announced, wantAnnounced) {
		t.Errorf("announced = %v, want %v", announced, wantAnnounced)
	}
	for _, s := range segs {
		if s.lo <= u("10.0.4.200") && u("10.0.4.200") <= s.hi {
			t.Errorf("the ambiguous half has a segment: %v", s)
		}
	}
	again, err := readASNPrefixes(path)
	if err != nil || !reflect.DeepEqual(again, segs) {
		t.Errorf("readASNPrefixes = %v, %v", again, err)
	}
}
