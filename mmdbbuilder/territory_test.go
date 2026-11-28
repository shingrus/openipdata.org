package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func territoryIndex(t *testing.T, dir string) (geoNames, foldedNames) {
	t.Helper()
	path := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		gnLine("1", "Hong Kong", "22.27832", "114.17469", "HK", "00", "7000000", "Asia/Hong_Kong")+
			gnLine("2", "Macau", "22.20056", "113.54611", "MO", "00", "500000", "Asia/Macau")+
			gnLine("3", "Beijing", "39.9075", "116.39723", "CN", "22", "20000000", "Asia/Shanghai"))
	index, folded, err := loadGeoNamesOpts(path, true, true)
	if err != nil {
		t.Fatal(err)
	}
	return index, folded
}

func TestTerritoryOf(t *testing.T) {
	for _, tc := range []struct {
		code, city, want string
		moved            bool
	}{
		{"CN", "Hong Kong", "HK", true},
		{"CN", "HONG KONG", "HK", true},
		{"CN", " hong kong ", "HK", true},
		{"CN", "Macao", "MO", true},
		{"CN", "Macau", "MO", true},
		{"CN", "Beijing", "CN", false},
		{"CN", "", "CN", false},
		{"US", "Hong Kong", "US", false},
		{"HK", "Hong Kong", "HK", false},
		{"CN", "Hong Kong Island", "CN", false},
		{"US", "Riga", "US", false},
	} {
		got, moved := territoryOf(tc.code, tc.city)
		if got != tc.want || moved != tc.moved {
			t.Errorf("territoryOf(%q, %q) = %q, %v", tc.code, tc.city, got, moved)
		}
	}
}

const territoryHeader = "prefix,country,region,city,source\n"

func TestBuildGeoTerritories(t *testing.T) {
	dir := t.TempDir()
	index, folded := territoryIndex(t, dir)
	layer := writeFile(t, filepath.Join(dir, "70-feeds.csv.gz"), territoryHeader+
		"45.0.1.0/24,CN,,Hong Kong,x\n"+
		"45.0.2.0/24,CN,,HONG KONG,x\n"+
		"45.0.3.0/24,CN,, hong kong ,x\n"+
		"45.0.4.0/24,CN,,Macao,x\n"+
		"45.0.5.0/24,CN,,Macau,x\n"+
		"45.0.6.0/24,CN,,Beijing,x\n"+
		"45.0.7.0/24,CN,,,x\n"+
		"45.0.8.0/24,US,,Hong Kong,x\n"+
		"45.0.9.0/24,HK,,Hong Kong,x\n"+
		"45.0.10.0/24,CN,,Hong Kong Island,x\n"+
		"2001:db8::/48,CN,,Hong Kong,x\n")
	out := filepath.Join(dir, "out.mmdb")
	stats, err := buildGeoNames([]string{layer}, "", out, false, nil, nil, nil, index, folded, "", false, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if stats[0].territory != 6 { // 45.0.1-3, 45.0.4-5 and 2001:db8::/48
		t.Errorf("territory = %d, want 6", stats[0].territory)
	}
	for ip, want := range map[string][3]any{
		"45.0.1.9":    {"HK", "Hong Kong", 22.2783},
		"45.0.2.9":    {"HK", "HONG KONG", 22.2783},
		"45.0.3.9":    {"HK", "hong kong", 22.2783},
		"45.0.4.9":    {"MO", "Macao", nil},
		"45.0.5.9":    {"MO", "Macau", 22.2006},
		"45.0.6.9":    {"CN", "Beijing", 39.9075},
		"45.0.8.9":    {"US", "Hong Kong", nil},
		"45.0.9.9":    {"HK", "Hong Kong", 22.2783},
		"45.0.10.9":   {"CN", "Hong Kong Island", nil},
		"2001:db8::1": {"HK", "Hong Kong", 22.2783},
	} {
		m := lookupGeo(t, out, ip)
		var lat any
		if m["latitude"] != nil {
			lat = m["latitude"]
		}
		if m["country_code"] != want[0] || m["city_name"] != want[1] || lat != want[2] {
			t.Errorf("%s = %v, want %v", ip, m, want)
		}
	}
	if m := lookupGeo(t, out, "45.0.1.9"); m["country_name"] != "Hong Kong" {
		t.Errorf("country name = %v", m["country_name"])
	}
	if m := lookupGeo(t, out, "45.0.7.9"); m["country_code"] != "CN" || m["city_name"] != nil {
		t.Errorf("no city row = %v", m)
	}
}

// Without -geonames the country still changes.
func TestBuildGeoTerritoriesNoGeoNames(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "70-feeds.csv.gz"), territoryHeader+"45.0.1.0/24,CN,,Hong Kong,x\n")
	out := filepath.Join(dir, "out.mmdb")
	stats, err := buildGeo([]string{layer}, "", out, false, nil, nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	m := lookupGeo(t, out, "45.0.1.9")
	if m["country_code"] != "HK" || m["city_name"] != "Hong Kong" || m["latitude"] != nil || stats[0].territory != 1 {
		t.Errorf("record %v, stats %+v", m, stats[0])
	}
}

// A build with no such row has a zero counter and the stats of today.
func TestBuildGeoTerritoriesNone(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "70-feeds.csv.gz"), territoryHeader+"45.0.1.0/24,CN,,Beijing,x\n")
	stats, err := buildGeo([]string{layer}, "", filepath.Join(dir, "out.mmdb"), false, nil, nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := (geoLayerStats{name: "70-feeds.csv.gz", rows: 1, prefixes: 1}); !reflect.DeepEqual(stats[0], want) {
		t.Errorf("stats = %+v, want %+v", stats[0], want)
	}
}

// The layer rules see the new country.
func TestBuildGeoTerritoriesLayerRules(t *testing.T) {
	dir := t.TempDir()
	base := writeFile(t, filepath.Join(dir, "10-rir.csv.gz"), "prefix,country,city\n45.0.0.0/24,HK,\n45.0.1.0/24,CN,\n")
	layer := writeFile(t, filepath.Join(dir, "70-feeds.csv.gz"), territoryHeader+
		"45.0.0.0/23,CN,,Hong Kong,x\n")
	// -same-country: applies where the country so far is HK, not where it is CN.
	out := filepath.Join(dir, "same.mmdb")
	if _, err := buildGeo([]string{base, layer}, "", out, false, map[string]bool{"70-feeds.csv.gz": true}, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if m := lookupGeo(t, out, "45.0.0.9"); m["country_code"] != "HK" || m["city_name"] != "Hong Kong" {
		t.Errorf("same-country, HK part: %v", m)
	}
	if m := lookupGeo(t, out, "45.0.1.9"); m["country_code"] != "CN" || m["city_name"] != nil {
		t.Errorf("same-country, CN part: %v", m)
	}
	// -first-country: the row applies only where the first layer gives HK, split from the /23.
	out = filepath.Join(dir, "first.mmdb")
	if _, err := buildGeo([]string{base, layer}, "", out, false, nil, nil, map[string]bool{"70-feeds.csv.gz": true}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if m := lookupGeo(t, out, "45.0.0.9"); m["country_code"] != "HK" || m["city_name"] != "Hong Kong" {
		t.Errorf("first-country, HK part: %v", m)
	}
	if m := lookupGeo(t, out, "45.0.1.9"); m["country_code"] != "CN" || m["city_name"] != nil {
		t.Errorf("first-country, CN part: %v", m)
	}
	// -within-first: the row is a /24 of the first layer's block and is used.
	small := writeFile(t, filepath.Join(dir, "71-small.csv.gz"), territoryHeader+"45.0.0.0/24,CN,,Macau,x\n")
	out = filepath.Join(dir, "within.mmdb")
	if _, err := buildGeo([]string{base, small}, "", out, false, nil, map[string]bool{"71-small.csv.gz": true}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if m := lookupGeo(t, out, "45.0.0.9"); m["country_code"] != "MO" {
		t.Errorf("within-first: %v", m)
	}
}

// The pre-pass of the full-list fallback sees the same country: CN/Hong Kong is not wanted from the
// full list, because the main list places HK/Hong Kong.
func TestTerritoriesFullListPrePass(t *testing.T) {
	dir := t.TempDir()
	index, folded := territoryIndex(t, dir)
	layer := writeFile(t, filepath.Join(dir, "70-feeds.csv.gz"), territoryHeader+
		"45.0.1.0/24,CN,,Hong Kong,x\n"+
		"45.0.2.0/24,CN,,Macao,x\n"+
		"45.0.3.0/24,CN,,Nowhere,x\n")
	wanted, err := wantedFromLayers([]string{layer}, "", index, &cityCleaner{index: index, folded: folded})
	if err != nil {
		t.Fatal(err)
	}
	// Macao is MO's name for GeoNames' "Macau" here: the pre-pass asks MO, not CN, for it.
	if want := (fullWanted{"CN|nowhere": 1, "MO|macao": 1}); !reflect.DeepEqual(wanted, want) {
		t.Errorf("wanted = %v, want %v", wanted, want)
	}
	full := writeFile(t, filepath.Join(dir, "allCountries.txt"),
		allLine("9", "Hong Kong", "", "1.0", "2.0", "P", "PPL", "CN", "1000"))
	st, err := applyFullList(index, folded, []string{layer}, "", full)
	if err != nil {
		t.Fatal(err)
	}
	if st.rows != 0 || st.placed != 0 {
		t.Errorf("full list stats = %+v", st)
	}
}
