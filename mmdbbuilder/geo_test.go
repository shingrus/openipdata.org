package main

import (
	"compress/gzip"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if filepath.Ext(path) == ".gz" {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		gz.Write([]byte(body))
		gz.Close()
		f.Close()
		return path
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildGeo(t *testing.T) {
	dir := t.TempDir()
	base := writeFile(t, filepath.Join(dir, "rir.csv"), "prefix,country,city\n"+
		"45.0.0.0/8,US,\n"+
		"45.10.0.0/16,CA,\n"+
		"2a00::/12,DE,\n"+
		"45.20.0.0/16,usa,\n"+ // bad code
		"not-a-prefix,US,\n")
	// A later layer replaces everything under its prefixes, even more specific ones.
	pdb := writeFile(t, filepath.Join(dir, "pdb.csv"), "prefix,country,city\n45.10.0.0/15,FR,Paris\n")
	// Geofeed rows: extra columns, a bare address, host bits, a repeated prefix.
	feeds := writeFile(t, filepath.Join(dir, "geofeeds.csv.gz"), "prefix,country,region,city,feed_url\n"+
		"45.10.1.0/24,GB,GB-ENG,London,https://a.example/f.csv\n"+
		"45.10.2.7/24,NL,,Amsterdam,https://a.example/f.csv\n"+
		"45.10.3.4,BE,,,https://b.example/f.csv\n"+
		"45.10.1.0/24,IE,,Dublin,https://b.example/f.csv\n")
	names := writeFile(t, filepath.Join(dir, "names.tsv"), "GB\tUnited Kingdom\nUS\tUnited States\n")

	out := filepath.Join(dir, "ip2geo.mmdb")
	stats, err := buildGeo([]string{base, pdb, feeds}, names, out, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []geoLayerStats{
		{name: "rir.csv", rows: 3, prefixes: 3, skipped: 2},
		{name: "pdb.csv", rows: 1, prefixes: 1},
		{name: "geofeeds.csv.gz", rows: 4, prefixes: 3},
	}
	if !reflect.DeepEqual(stats, want) {
		t.Errorf("stats = %+v\nwant    %+v", stats, want)
	}

	reader, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if reader.Metadata.DatabaseType != "ip2geo" || reader.Metadata.Description["en"] != "IP2GEO" {
		t.Errorf("metadata = %s %v", reader.Metadata.DatabaseType, reader.Metadata.Description)
	}
	for ip, want := range map[string]map[string]any{
		"45.1.1.1":  {"country_code": "US", "country_name": "United States"},
		"45.11.0.1": {"country_code": "FR", "country_name": "France", "city_name": "Paris"},   // tzdata name
		"45.10.1.9": {"country_code": "IE", "country_name": "Ireland", "city_name": "Dublin"}, // last row wins
		"45.10.2.1": {"country_code": "NL", "country_name": "Netherlands", "city_name": "Amsterdam"},
		"45.10.3.4": {"country_code": "BE", "country_name": "Belgium"},
		"45.10.3.5": {"country_code": "FR", "country_name": "France", "city_name": "Paris"},
		"2a00::1":   {"country_code": "DE", "country_name": "Germany"},
		"8.8.8.8":   nil,
	} {
		result := reader.Lookup(netip.MustParseAddr(ip))
		var got map[string]any
		if result.Found() {
			if err := result.Decode(&got); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", ip, got, want)
		}
	}
}

func TestBuildGeoFailures(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "ip2geo.mmdb")
	writeFile(t, out, "previous database")
	for name, body := range map[string]string{
		"empty.csv":     "prefix,country,city\n",
		"bad-rows.csv":  "prefix,country,city\nnope,US,\n",
		"no-header.csv": "45.0.0.0/8,US,\n",
	} {
		if _, err := buildGeo([]string{writeFile(t, filepath.Join(dir, name), body)}, "", out, false, nil, nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if data, _ := os.ReadFile(out); string(data) != "previous database" {
		t.Error("a failed build replaced the previous file")
	}
}

func lookupGeo(t *testing.T, path, ip string) map[string]any {
	t.Helper()
	reader, err := maxminddb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var got map[string]any
	if result := reader.Lookup(netip.MustParseAddr(ip)); result.Found() {
		if err := result.Decode(&got); err != nil {
			t.Fatal(err)
		}
	}
	return got
}

// A same-country layer refines: it leaves networks of another country alone.
func TestBuildGeoSameCountry(t *testing.T) {
	dir := t.TempDir()
	base := writeFile(t, filepath.Join(dir, "rir.csv"), "prefix,country,city\n45.0.0.0/8,US,\n45.10.0.0/16,CA,\n")
	pdb := writeFile(t, filepath.Join(dir, "pdb.csv"), "prefix,country,city\n45.0.0.0/8,US,Dallas\n46.0.0.0/8,FR,Paris\n")
	out := filepath.Join(dir, "ip2geo.mmdb")
	if _, err := buildGeo([]string{base, pdb}, "", out, false, map[string]bool{"pdb.csv": true}, nil); err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]map[string]any{
		"45.1.1.1":  {"country_code": "US", "country_name": "United States", "city_name": "Dallas"},
		"45.10.0.1": {"country_code": "CA", "country_name": "Canada"},
		"45.11.0.1": {"country_code": "US", "country_name": "United States", "city_name": "Dallas"},
		"46.0.0.1":  {"country_code": "FR", "country_name": "France", "city_name": "Paris"}, // nothing was there
	} {
		if got := lookupGeo(t, out, ip); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", ip, got, want)
		}
	}
}

// Rows that third parties write: an IPv6 prefix where the tree keeps IPv4
// space, a line that does not parse, and a city that is not plain text.
func TestBuildGeoHostileRows(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "feeds.csv"), "prefix,country,city\n"+
		"45.10.0.0/16,US,Dallas\n"+
		"::800:0/104,RU,Moscow\n"+ // 8.0.0.0/8 in an IPv6 tree
		"::/0,RU,Moscow\n"+
		"45\"x,US,\n"+ // a bare quote in the first field
		"45.20.0.0/16,US,<script>alert(1)</script>\n"+
		"45.30.0.0/16,US,"+strings.Repeat("x", maxCityLen+1)+"\n")
	out := filepath.Join(dir, "ip2geo.mmdb")
	stats, err := buildGeo([]string{layer}, "", out, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats[0].rows != 3 || stats[0].skipped != 2 || stats[0].noCity != 2 {
		t.Errorf("stats = %+v", stats[0])
	}
	plain := map[string]any{"country_code": "US", "country_name": "United States"}
	for ip, want := range map[string]map[string]any{
		"45.10.0.1": {"country_code": "US", "country_name": "United States", "city_name": "Dallas"},
		"8.8.8.8":   nil,
		"45.20.0.1": plain,
		"45.30.0.1": plain,
	} {
		if got := lookupGeo(t, out, ip); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", ip, got, want)
		}
	}
}

// A within-first layer skips rows broader than every block of the first layer.
func TestBuildGeoWithinFirst(t *testing.T) {
	dir := t.TempDir()
	base := writeFile(t, filepath.Join(dir, "rir.csv"), "prefix,country,city\n45.10.0.0/16,US,\n2a00::/32,DE,\n"+
		"47.0.0.0/8,,\n") // delegated, no country: only marks a block
	feeds := writeFile(t, filepath.Join(dir, "feeds.csv"), "prefix,country,city\n"+
		"47.1.0.0/16,SE,Stockholm\n"+ // inside the block without a country
		"45.0.0.0/8,RU,Moscow\n"+ // broader than the block
		"45.10.0.0/16,GB,London\n"+ // the block itself
		"45.10.5.0/24,NL,Amsterdam\n"+ // inside the block
		"2a00::/16,RU,Moscow\n"+ // broader, IPv6
		"2a00:0:1::/48,FR,Paris\n"+ // inside the block
		"46.0.0.0/24,BE,\n") // outside every block: skipped too
	out := filepath.Join(dir, "ip2geo.mmdb")
	stats, err := buildGeo([]string{base, feeds}, "", out, false, nil, map[string]bool{"feeds.csv": true, "rir.csv": true})
	if err != nil {
		t.Fatal(err)
	}
	if want := (geoLayerStats{name: "feeds.csv", rows: 4, prefixes: 4, skipped: 3}); stats[1] != want {
		t.Errorf("stats = %+v, want %+v", stats[1], want)
	}
	for ip, want := range map[string]string{
		"45.1.1.1":    "",
		"45.10.0.1":   "GB",
		"45.10.5.1":   "NL",
		"2a00:2::1":   "",
		"2a00:0:1::1": "FR",
		"46.0.0.1":    "",
		"47.1.0.1":    "SE",
		"47.2.0.1":    "",
	} {
		if got := lookupGeo(t, out, ip)["country_code"]; got != nil && got != want || got == nil && want != "" {
			t.Errorf("%s = %v, want %q", ip, got, want)
		}
	}
}
