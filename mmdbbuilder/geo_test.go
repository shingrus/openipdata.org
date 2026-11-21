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
	base := writeFile(t, filepath.Join(dir, "rir.csv.gz"), "prefix,country,city\n"+
		"45.0.0.0/8,US,\n"+
		"45.10.0.0/16,CA,\n"+
		"2a00::/12,DE,\n"+
		"45.20.0.0/16,usa,\n"+ // bad code
		"not-a-prefix,US,\n")
	// A later layer replaces everything under its prefixes, even more specific ones.
	pdb := writeFile(t, filepath.Join(dir, "pdb.csv.gz"), "prefix,country,city\n45.10.0.0/15,FR,Paris\n")
	// Geofeed rows: extra columns, a bare address, host bits, a repeated prefix.
	feeds := writeFile(t, filepath.Join(dir, "geofeeds.csv.gz"), "prefix,country,region,city,source\n"+
		"45.10.1.0/24,GB,GB-ENG,London,https://a.example/f.csv\n"+
		"45.10.2.7/24,NL,,Amsterdam,https://a.example/f.csv\n"+
		"45.10.3.4,BE,,,https://b.example/f.csv\n"+
		"45.10.1.0/24,IE,,Dublin,https://b.example/f.csv\n")
	names := writeFile(t, filepath.Join(dir, "names.tsv"), "GB\tUnited Kingdom\nUS\tUnited States\n")

	out := filepath.Join(dir, "ip2geo.mmdb")
	stats, err := buildGeo([]string{base, pdb, feeds}, names, out, false, nil, nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []geoLayerStats{
		{name: "rir.csv.gz", rows: 3, prefixes: 3, skipped: 2},
		{name: "pdb.csv.gz", rows: 1, prefixes: 1},
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
		"45.11.0.1": {"country_code": "FR", "country_name": "France", "city_name": "Paris", "city_source": "pdb"},        // tzdata name
		"45.10.1.9": {"country_code": "IE", "country_name": "Ireland", "city_name": "Dublin", "city_source": "geofeeds"}, // last row wins
		"45.10.2.1": {"country_code": "NL", "country_name": "Netherlands", "city_name": "Amsterdam", "city_source": "geofeeds"},
		"45.10.3.4": {"country_code": "BE", "country_name": "Belgium"},
		"45.10.3.5": {"country_code": "FR", "country_name": "France", "city_name": "Paris", "city_source": "pdb"},
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
		"empty.csv.gz":     "prefix,country,city\n",
		"bad-rows.csv.gz":  "prefix,country,city\nnope,US,\n",
		"no-header.csv.gz": "45.0.0.0/8,US,\n",
		"no-city.csv.gz":   "prefix,country\n45.0.0.0/8,US\n",                       // the layout has a city column
		"plain.csv":        "prefix,country,region,city,source\n45.0.0.0/8,US,,,\n", // layers are gzipped
	} {
		if _, err := buildGeo([]string{writeFile(t, filepath.Join(dir, name), body)}, "", out, false, nil, nil, nil, nil, ""); err == nil {
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
	base := writeFile(t, filepath.Join(dir, "rir.csv.gz"), "prefix,country,city\n45.0.0.0/8,US,\n45.10.0.0/16,CA,\n")
	pdb := writeFile(t, filepath.Join(dir, "pdb.csv.gz"), "prefix,country,city\n45.0.0.0/8,US,Dallas\n46.0.0.0/8,FR,Paris\n")
	out := filepath.Join(dir, "ip2geo.mmdb")
	if _, err := buildGeo([]string{base, pdb}, "", out, false, map[string]bool{"pdb.csv.gz": true}, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]map[string]any{
		"45.1.1.1":  {"country_code": "US", "country_name": "United States", "city_name": "Dallas", "city_source": "pdb"},
		"45.10.0.1": {"country_code": "CA", "country_name": "Canada"},
		"45.11.0.1": {"country_code": "US", "country_name": "United States", "city_name": "Dallas", "city_source": "pdb"},
		"46.0.0.1":  {"country_code": "FR", "country_name": "France", "city_name": "Paris", "city_source": "pdb"}, // nothing was there
	} {
		if got := lookupGeo(t, out, ip); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", ip, got, want)
		}
	}
}

// A retired code (YU, Yugoslavia) or a made-up one is no country: the row is
// skipped and the country of the lower layer stays. XK (Kosovo) is a country.
func TestBuildGeoRetiredCountry(t *testing.T) {
	dir := t.TempDir()
	base := writeFile(t, filepath.Join(dir, "rir.csv.gz"), "prefix,country,city\n45.0.0.0/8,RS,\n46.0.0.0/8,RS,\n")
	whois := writeFile(t, filepath.Join(dir, "whois.csv.gz"), "prefix,country,city\n45.1.0.0/24,YU,\n45.2.0.0/24,ZZ,\n46.1.0.0/24,XK,\n")
	out := filepath.Join(dir, "ip2geo.mmdb")
	stats, err := buildGeo([]string{base, whois}, "", out, false, nil, nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if stats[1].skipped != 2 {
		t.Errorf("skipped = %d, want 2", stats[1].skipped)
	}
	for ip, want := range map[string]string{"45.1.0.1": "RS", "45.2.0.1": "RS", "46.1.0.1": "XK"} {
		if got := lookupGeo(t, out, ip)["country_code"]; got != want {
			t.Errorf("%s = %v, want %s", ip, got, want)
		}
	}
}

// Rows that third parties write: an IPv6 prefix where the tree keeps IPv4
// space, a line that does not parse, and a city that is not plain text.
func TestBuildGeoHostileRows(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "feeds.csv.gz"), "prefix,country,city\n"+
		"45.10.0.0/16,US,Dallas\n"+
		"::800:0/104,RU,Moscow\n"+ // 8.0.0.0/8 in an IPv6 tree
		"::/0,RU,Moscow\n"+
		"45\"x,US,\n"+ // a bare quote in the first field
		"45.20.0.0/16,US,<script>alert(1)</script>\n"+
		"45.30.0.0/16,US,"+strings.Repeat("x", maxCityLen+1)+"\n")
	out := filepath.Join(dir, "ip2geo.mmdb")
	stats, err := buildGeo([]string{layer}, "", out, false, nil, nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if stats[0].rows != 3 || stats[0].skipped != 2 || stats[0].noCity != 2 {
		t.Errorf("stats = %+v", stats[0])
	}
	plain := map[string]any{"country_code": "US", "country_name": "United States"}
	for ip, want := range map[string]map[string]any{
		"45.10.0.1": {"country_code": "US", "country_name": "United States", "city_name": "Dallas", "city_source": "feeds"},
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
	base := writeFile(t, filepath.Join(dir, "rir.csv.gz"), "prefix,country,city\n45.10.0.0/16,US,\n2a00::/32,DE,\n"+
		"47.0.0.0/8,,\n") // delegated, no country: only marks a block
	feeds := writeFile(t, filepath.Join(dir, "feeds.csv.gz"), "prefix,country,city\n"+
		"47.1.0.0/16,SE,Stockholm\n"+ // inside the block without a country
		"45.0.0.0/8,RU,Moscow\n"+ // broader than the block
		"45.10.0.0/16,GB,London\n"+ // the block itself
		"45.10.5.0/24,NL,Amsterdam\n"+ // inside the block
		"2a00::/16,RU,Moscow\n"+ // broader, IPv6
		"2a00:0:1::/48,FR,Paris\n"+ // inside the block
		"46.0.0.0/24,BE,\n") // outside every block: skipped too
	out := filepath.Join(dir, "ip2geo.mmdb")
	stats, err := buildGeo([]string{base, feeds}, "", out, false, nil, map[string]bool{"feeds.csv.gz": true, "rir.csv.gz": true}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := (geoLayerStats{name: "feeds.csv.gz", rows: 4, prefixes: 4, skipped: 3}); stats[1] != want {
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

// A first-country layer applies a row only where the first layer gives the
// row's country; a row that spans first-layer ranges is split.
func TestBuildGeoFirstCountry(t *testing.T) {
	dir := t.TempDir()
	rir := writeFile(t, filepath.Join(dir, "rir.csv.gz"), "prefix,country,city\n"+
		"45.0.0.0/16,FR,\n45.1.0.0/16,DE,\n45.2.0.0/16,US,\n"+
		"45.4.0.0/16,FR,\n45.4.8.0/24,DE,\n"+ // a more specific block of another country
		"47.0.0.0/8,,\n"+ // delegated, no country
		"2a00::/32,FR,\n2a01::/32,DE,\n")
	// Registration data: all of it says Italy (or the US), which the hosting layer may undo.
	blocks := writeFile(t, filepath.Join(dir, "geo-ip-blocks.csv.gz"), "prefix,country,city\n"+
		"45.0.0.0/13,IT,\n47.0.0.0/8,IT,\n48.0.0.0/8,IT,\n2a00::/31,IT,\n")
	hosting := writeFile(t, filepath.Join(dir, "hosting.csv.gz"), "prefix,country,city\n"+
		"45.0.5.0/24,FR,Paris\n"+ // first layer FR: applies
		"45.2.5.0/24,DE,Berlin\n"+ // first layer US: skipped
		"45.0.0.0/15,FR,Lyon\n"+ // spans FR and DE: only the FR half
		"45.4.0.0/16,FR,Nice\n"+ // FR with a DE hole
		"47.1.0.0/16,FR,Lille\n"+ // first layer has no country: skipped
		"48.0.0.0/24,FR,Metz\n"+ // outside the first layer: skipped
		"2a00:0:1::/48,FR,Paris\n"+
		"2a01::/48,FR,Paris\n") // first layer DE: skipped
	out := filepath.Join(dir, "ip2geo.mmdb")
	stats, err := buildGeo([]string{rir, blocks, hosting}, "", out, false, nil, nil, map[string]bool{"hosting.csv.gz": true}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// 4 rows apply; the Nice /16 minus the DE /24 hole is 8 networks, the others 1 each.
	if want := (geoLayerStats{name: "hosting.csv.gz", rows: 4, prefixes: 11, skipped: 4}); stats[2] != want {
		t.Errorf("stats = %+v, want %+v", stats[2], want)
	}
	for ip, want := range map[string][2]string{
		"45.0.5.1":    {"FR", "Paris"},
		"45.0.9.1":    {"FR", "Lyon"},
		"45.1.0.1":    {"IT", ""}, // DE half of the Lyon row
		"45.2.5.1":    {"IT", ""},
		"45.4.1.1":    {"FR", "Nice"},
		"45.4.8.1":    {"IT", ""}, // the DE block inside the Nice row
		"45.4.9.1":    {"FR", "Nice"},
		"47.1.0.1":    {"IT", ""},
		"48.0.0.1":    {"IT", ""},
		"2a00:0:1::1": {"FR", "Paris"},
		"2a00:0:2::1": {"IT", ""},
	} {
		got := lookupGeo(t, out, ip)
		code, _ := got["country_code"].(string)
		city, _ := got["city_name"].(string)
		if code != want[0] || city != want[1] {
			t.Errorf("%s = %v, want %v", ip, got, want)
		}
	}
}

// RFC 8805 allows leaving out trailing fields: a row with just prefix and country is read
// with an empty city; one without a country is passed on as unusable.
func TestReadLayerShortRows(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "layer.csv.gz"), "prefix,country,region,city,source\n"+
		"45.0.0.0/8,US\n"+
		"45.1.0.0/16,CA,,Toronto\n"+
		"45.2.0.0/16\n"+
		"45.3.0.0/16,FR,,Paris,https://feed.example/f.csv\n")
	type row struct{ prefix, code, city string }
	var got []row
	if err := readLayer(path, func(_ int, prefix, code, city string) { got = append(got, row{prefix, code, city}) }); err != nil {
		t.Fatal(err)
	}
	want := []row{{"45.0.0.0/8", "US", ""}, {"45.1.0.0/16", "CA", "Toronto"}, {"", "", ""}, {"45.3.0.0/16", "FR", "Paris"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// An IPv4 row with a country and no city keeps a city of the same country that another layer
// wrote; it still replaces a record of another country, a city of its own layer, and a record with
// no city. An IPv6 row replaces as before.
func TestBuildGeoCountryOnlyRow(t *testing.T) {
	dir := t.TempDir()
	rir := writeFile(t, filepath.Join(dir, "10-rir.csv.gz"), "prefix,country,city\n45.0.0.0/8,US,\n46.0.0.0/8,DE,\n2a00::/16,DE,\n")
	low := writeFile(t, filepath.Join(dir, "30-low.csv.gz"), "prefix,country,city\n"+
		"45.1.0.0/16,US,Dallas\n45.2.0.0/16,US,Reno\n46.1.0.0/16,DE,Berlin\n46.2.0.0/16,DE,Munich\n2a00:1::/32,DE,Berlin\n")
	same := writeFile(t, filepath.Join(dir, "50-same.csv.gz"), "prefix,country,city\n"+
		"46.1.1.0/24,DE,\n"+ // a same-country layer: Berlin stays
		"46.2.0.0/16,FR,\n"+ // another country: the row does not apply, Munich stays
		"45.6.0.0/16,US,\n") // nothing below has a city: the plain record
	top := writeFile(t, filepath.Join(dir, "70-top.csv.gz"), "prefix,country,city\n"+
		"45.1.0.0/24,US,\n"+ // the same country: Dallas stays
		"45.1.1.0/24,US,<b>\n"+ // a city that is refused counts as no city: Dallas stays
		"45.2.0.0/24,CA,\n"+ // another country: replaces Reno
		"45.3.0.0/16,US,Austin\n"+
		"45.3.1.0/24,US,\n"+ // a city of the same layer (the broader row): replaced
		"45.4.0.0/16,US,\n"+ // no city below
		"46.1.0.0/24,DE,\n"+ // Berlin stays
		"2a00:1:1::/48,DE,\n") // IPv6: the rule is IPv4 only, the row replaces Berlin
	out := filepath.Join(dir, "ip2geo.mmdb")
	if _, err := buildGeo([]string{rir, low, same, top}, "", out, false, map[string]bool{"50-same.csv.gz": true}, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	us := map[string]any{"country_code": "US", "country_name": "United States"}
	de := map[string]any{"country_code": "DE", "country_name": "Germany"}
	city := func(code, name, city, source string) map[string]any {
		return map[string]any{"country_code": code, "country_name": name, "city_name": city, "city_source": source}
	}
	for ip, want := range map[string]map[string]any{
		"45.1.0.1":    city("US", "United States", "Dallas", "30-low"),
		"45.1.1.1":    city("US", "United States", "Dallas", "30-low"),
		"45.1.2.1":    city("US", "United States", "Dallas", "30-low"),
		"45.2.0.1":    {"country_code": "CA", "country_name": "Canada"},
		"45.2.1.1":    city("US", "United States", "Reno", "30-low"),
		"45.3.0.1":    city("US", "United States", "Austin", "70-top"),
		"45.3.1.1":    us,
		"45.4.0.1":    us,
		"45.6.0.1":    us,
		"46.1.0.1":    city("DE", "Germany", "Berlin", "30-low"),
		"46.1.1.1":    city("DE", "Germany", "Berlin", "30-low"),
		"46.2.0.1":    city("DE", "Germany", "Munich", "30-low"),
		"2a00:1::1":   city("DE", "Germany", "Berlin", "30-low"),
		"2a00:1:1::1": de,
	} {
		if got := lookupGeo(t, out, ip); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", ip, got, want)
		}
	}
}

// city_source names the layer that wrote the city, in every mode; a record
// without a city has none.
func TestBuildGeoCitySource(t *testing.T) {
	dir := t.TempDir()
	rir := writeFile(t, filepath.Join(dir, "10-rir.csv.gz"), "prefix,country,city\n"+
		"45.0.0.0/16,FR,\n46.0.0.0/16,DE,\n47.0.0.0/16,US,\n48.0.0.0/16,US,\n49.0.0.0/16,US,\n50.0.0.0/16,US,\n")
	plain := writeFile(t, filepath.Join(dir, "30-pdb.csv.gz"), "prefix,country,city\n"+
		"46.0.0.0/24,DE,Berlin\n")
	same := writeFile(t, filepath.Join(dir, "40-hosting.csv.gz"), "prefix,country,city\n"+
		"47.0.0.0/24,US,Dallas\n"+
		"46.0.1.0/24,FR,Paris\n"+ // other country: left alone
		"49.0.0.0/24,US,Reno\n")
	within := writeFile(t, filepath.Join(dir, "60-whois.csv.gz"), "prefix,country,city\n"+
		"48.0.0.0/24,US,Austin\n")
	first := writeFile(t, filepath.Join(dir, "70-geofeeds.csv.gz"), "prefix,country,city\n"+
		"45.0.0.0/24,FR,Lyon\n"+
		"49.0.0.0/24,US,\n") // no city, the same country: Reno of the layer below stays
	for _, debug := range []bool{false, true} {
		out := filepath.Join(dir, "ip2geo.mmdb")
		_, err := buildGeo([]string{rir, plain, same, within, first}, "", out, debug,
			map[string]bool{"40-hosting.csv.gz": true},
			map[string]bool{"60-whois.csv.gz": true},
			map[string]bool{"70-geofeeds.csv.gz": true}, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		for ip, want := range map[string]string{
			"46.0.0.1": "30-pdb",      // plain
			"47.0.0.1": "40-hosting",  // sameCountry
			"48.0.0.1": "60-whois",    // withinFirst
			"45.0.0.1": "70-geofeeds", // firstCountry
			"46.0.1.1": "",            // sameCountry row skipped, RIR has no city
			"49.0.0.1": "40-hosting",  // a later row with the same country and no city keeps it
			"50.0.0.1": "",            // RIR only
		} {
			got := lookupGeo(t, out, ip)
			src, has := got["city_source"].(string)
			if src != want || has != (want != "") {
				t.Errorf("debug=%v %s: city_source = %v, want %q (%v)", debug, ip, got["city_source"], want, got)
			}
			if want == "" && got["city_name"] != nil {
				t.Errorf("%s: unexpected city %v", ip, got["city_name"])
			}
		}
	}
}

// With a GeoNames index a placed city gets latitude and longitude (4 decimals);
// an unplaceable city and a country-only record get neither; two prefixes of
// one place share the values; without the index the file is the same as ever.
func TestBuildGeoCoordinates(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), "prefix,country,region,city,source\n"+
		"45.0.0.0/24,US,,Dallas,x\n"+
		"45.0.1.0/24,US,,Dallas,x\n"+ // the same place again
		"45.0.2.0/24,DE,,Berlin,x\n"+
		"45.0.3.0/24,FR,,Nowhereville,x\n"+ // a city GeoNames lacks
		"45.0.4.0/24,US,,Berlin,x\n"+ // Berlin in another country: not placed
		"45.0.5.0/24,FR,,,x\n") // no city
	geonames := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		geoNamesLine("1", "Dallas", "", "32.78306", "-96.80667", "US", "1300000")+"\n"+
			geoNamesLine("2", "Berlin", "", "52.52437", "13.41053", "DE", "3426354")+"\n")
	index, err := loadGeoNames(geonames)
	if err != nil {
		t.Fatal(err)
	}
	with := filepath.Join(dir, "with.mmdb")
	stats, err := buildGeo([]string{layer}, "", with, false, nil, nil, nil, index, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].placed != 2 || stats[0].unplaced != 2 {
		t.Errorf("stats = %+v, want 2 placed and 2 unplaced records", stats)
	}
	reader, err := maxminddb.Open(with)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	type coords struct{ lat, lon float64 }
	for _, tc := range []struct {
		ip   string
		want *coords // nil: neither field
	}{
		{"45.0.0.1", &coords{32.7831, -96.8067}},
		{"45.0.1.1", &coords{32.7831, -96.8067}}, // shares the values of the first prefix
		{"45.0.2.1", &coords{52.5244, 13.4105}},
		{"45.0.3.1", nil},
		{"45.0.4.1", nil},
		{"45.0.5.1", nil},
	} {
		var record map[string]any
		if err := reader.Lookup(netip.MustParseAddr(tc.ip)).Decode(&record); err != nil {
			t.Fatal(err)
		}
		lat, hasLat := record["latitude"]
		lon, hasLon := record["longitude"]
		if tc.want == nil {
			if hasLat || hasLon {
				t.Errorf("%s: unexpected coordinates in %v", tc.ip, record)
			}
			continue
		}
		if lat != tc.want.lat || lon != tc.want.lon {
			t.Errorf("%s: got %v, %v want %v, %v", tc.ip, lat, lon, tc.want.lat, tc.want.lon)
		}
	}

	// No index: the same bytes as a build that never heard of coordinates.
	a, b := filepath.Join(dir, "a.mmdb"), filepath.Join(dir, "b.mmdb")
	for _, out := range []string{a, b} {
		if _, err := buildGeo([]string{layer}, "", out, false, nil, nil, nil, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	da, _ := os.ReadFile(a)
	db, _ := os.ReadFile(b)
	// The build time in the metadata may differ by a second; compare the records instead.
	if len(da) == 0 || len(da) != len(db) {
		t.Errorf("sizes differ: %d, %d", len(da), len(db))
	}
	plain, err := maxminddb.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	var record map[string]any
	if err := plain.Lookup(netip.MustParseAddr("45.0.0.1")).Decode(&record); err != nil {
		t.Fatal(err)
	}
	if _, ok := record["latitude"]; ok || len(record) != 4 {
		t.Errorf("record without GeoNames = %v", record)
	}
	if len(da) >= 1 {
		if wi, _ := os.Stat(with); wi.Size() <= int64(len(da)) {
			t.Errorf("the build with coordinates (%d bytes) is not larger than without (%d)", wi.Size(), len(da))
		}
	}
}
