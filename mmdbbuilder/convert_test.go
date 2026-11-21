package main

import (
	"bytes"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang/v2"
)

func TestCountriesTable(t *testing.T) {
	m, err := parseCountries(countriesTSV)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) < 250 {
		t.Errorf("%d rows, want 250 or more", len(m))
	}
	for code, want := range map[string]countryInfo{
		"US": {6252001, "NA"}, "DE": {2921044, "EU"}, "XK": {831053, "EU"},
	} {
		if m[code] != want {
			t.Errorf("%s = %+v, want %+v", code, m[code], want)
		}
	}
	for _, bad := range []string{"US\t1\n", "US\tx\tNA\n", "US\t1\tZZ\n", "usa\t1\tNA\n", "US\t1\tNA\nUS\t2\tNA\n"} {
		if _, err := parseCountries([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// geoliteRecord has the shape of geoip2's City model.
type geoliteRecord struct {
	Continent struct {
		Code      string            `maxminddb:"code"`
		GeoNameID uint              `maxminddb:"geoname_id"`
		Names     map[string]string `maxminddb:"names"`
	} `maxminddb:"continent"`
	Country struct {
		ISOCode   string            `maxminddb:"iso_code"`
		GeoNameID uint              `maxminddb:"geoname_id"`
		IsInEU    bool              `maxminddb:"is_in_european_union"`
		Names     map[string]string `maxminddb:"names"`
	} `maxminddb:"country"`
	City struct {
		GeoNameID uint              `maxminddb:"geoname_id"`
		Names     map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Location struct {
		Latitude  float64 `maxminddb:"latitude"`
		Longitude float64 `maxminddb:"longitude"`
		TimeZone  string  `maxminddb:"time_zone"`
	} `maxminddb:"location"`
	CitySource string `maxminddb:"city_source"`
}

// hasKey reports whether the nested keys exist in a record decoded as a map.
func hasKey(m map[string]any, path ...string) bool {
	for _, k := range path {
		v, ok := m[k]
		if !ok {
			return false
		}
		if next, isMap := v.(map[string]any); isMap {
			m = next
		}
	}
	return true
}

func TestConvert(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), "prefix,country,region,city,source\n"+
		"45.0.0.0/24,DE,,Berlin,x\n"+ // placed, EU
		"45.0.1.0/24,US,,Dallas,x\n"+ // placed, not EU
		"45.0.2.0/24,FR,,Nowhereville,x\n"+ // unplaced city
		"45.0.3.0/24,FR,,,x\n"+ // country only
		"45.0.5.0/24,DE,,Dallas,x\n"+ // Dallas is not in DE: unplaced
		"2001:db8::/32,DE,,Berlin,x\n"+
		"2001:db9::/32,US,,,x\n")
	names := writeFile(t, filepath.Join(dir, "names.tsv"), "DE\tGermany\nUS\tUnited States\nFR\tFrance\nZZ\tZedland\n")
	geonames := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		"2950159\tBerlin\tBerlin\t\t52.52437\t13.41053\tP\tPPLC\tDE\t\t16\t\t\t\t3426354\t\t74\tEurope/Berlin\t2024-01-01\n"+
			"4684888\tDallas\tDallas\t\t32.78306\t-96.80667\tP\tPPL\tUS\t\t48\t\t\t\t1300000\t\t131\tAmerica/Chicago\t2024-01-01\n")
	index, err := loadGeoNames(geonames)
	if err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(dir, "flat.mmdb")
	if _, err := buildGeo([]string{layer}, names, flat, false, nil, nil, nil, index, ""); err != nil {
		t.Fatal(err)
	}
	// Stamp it, so the description carries the score stamp.
	meta := writeFile(t, filepath.Join(dir, "meta.json"), `{"atlas_country_pct": 97.5, "built_at": "2026-11-20T07:00:00Z"}`)
	stamped := filepath.Join(dir, "stamped.mmdb")
	if err := stampMMDB(flat, meta, stamped); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "openipdata-org-ip2geo.mmdb")
	stats, err := convertMMDB(stamped, out, geonames)
	if err != nil {
		t.Fatal(err)
	}
	if stats.networks != 7 || stats.withCity != 5 || stats.placed != 3 || stats.withTZ != 3 {
		t.Errorf("stats = %+v", stats)
	}
	if _, err := os.Stat(out + ".tmp"); err == nil {
		t.Error("tmp file left")
	}

	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Verify(); err != nil {
		t.Fatal(err)
	}
	get := func(ip string) (geoliteRecord, map[string]any) {
		var rec geoliteRecord
		res := r.Lookup(netip.MustParseAddr(ip))
		if !res.Found() {
			t.Fatalf("%s not found", ip)
		}
		if err := res.Decode(&rec); err != nil {
			t.Fatal(err)
		}
		return rec, lookupGeo(t, out, ip)
	}

	rec, raw := get("45.0.0.9")
	if rec.Continent.Code != "EU" || rec.Continent.GeoNameID != 6255148 || rec.Continent.Names["en"] != "Europe" ||
		rec.Country.ISOCode != "DE" || rec.Country.GeoNameID != 2921044 || !rec.Country.IsInEU || rec.Country.Names["en"] != "Germany" ||
		rec.City.GeoNameID != 2950159 || rec.City.Names["en"] != "Berlin" ||
		rec.Location.Latitude != 52.5244 || rec.Location.Longitude != 13.4105 || rec.Location.TimeZone != "Europe/Berlin" ||
		rec.CitySource != "layer" {
		t.Errorf("Berlin: %+v", rec)
	}
	if len(raw) != 5 {
		t.Errorf("Berlin keys: %v", raw)
	}
	rec, raw = get("45.0.1.9")
	if rec.Continent.Code != "NA" || rec.Country.IsInEU || hasKey(raw, "country", "is_in_european_union") ||
		rec.City.GeoNameID != 4684888 || rec.Location.TimeZone != "America/Chicago" {
		t.Errorf("Dallas: %+v", rec)
	}
	rec, raw = get("45.0.2.9") // unplaced: a name only
	if rec.City.Names["en"] != "Nowhereville" || hasKey(raw, "city", "geoname_id") || hasKey(raw, "location") || !rec.Country.IsInEU {
		t.Errorf("Nowhereville: %v", raw)
	}
	_, raw = get("45.0.3.9") // country only
	if hasKey(raw, "city") || hasKey(raw, "location") || !hasKey(raw, "country", "iso_code") {
		t.Errorf("country only: %v", raw)
	}
	// A code not in the table (the builder refuses it now, but an older flat file
	// still has YU): iso_code and names only, no continent.
	countries, err := parseCountries(countriesTSV)
	if err != nil {
		t.Fatal(err)
	}
	conv := &converter{countries: countries, cache: map[convertKey]mmdbtype.Map{}}
	zz := conv.build(convertKey{code: "YU", name: "Yugoslavia"})
	zzCountry, _ := zz["country"].(mmdbtype.Map)
	if _, ok := zz["continent"]; ok || zzCountry == nil || zzCountry["iso_code"] != mmdbtype.String("YU") ||
		zzCountry["names"] == nil || zzCountry["geoname_id"] != nil || zzCountry["is_in_european_union"] != nil {
		t.Errorf("YU: %v", zz)
	}
	rec, raw = get("45.0.5.9") // the city is not in that country: no id, no time zone, no coordinates
	if rec.City.Names["en"] != "Dallas" || hasKey(raw, "city", "geoname_id") || hasKey(raw, "location") {
		t.Errorf("Dallas in DE: %v", raw)
	}
	rec, _ = get("2001:db8::1")
	if rec.City.GeoNameID != 2950159 || rec.Country.ISOCode != "DE" {
		t.Errorf("IPv6 Berlin: %+v", rec)
	}
	_, raw = get("2001:db9::1")
	if hasKey(raw, "city") || hasKey(raw, "location") {
		t.Errorf("IPv6 country only: %v", raw)
	}

	// Every network of the input is in the output with the same place; nothing else is.
	src, err := maxminddb.Open(stamped)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	count := 0
	for res := range src.Networks() {
		if err := res.Err(); err != nil {
			t.Fatal(err)
		}
		count++
		var want flatRecord
		if err := res.Decode(&want); err != nil {
			t.Fatal(err)
		}
		var got geoliteRecord
		other := r.Lookup(res.Prefix().Addr())
		if !other.Found() || other.Prefix() != res.Prefix() {
			t.Fatalf("network %s: found %v, prefix %s", res.Prefix(), other.Found(), other.Prefix())
		}
		if err := other.Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Country.ISOCode != want.CountryCode || got.Country.Names["en"] != want.CountryName ||
			got.City.Names["en"] != want.CityName || got.CitySource != want.CitySource {
			t.Errorf("network %s differs: %+v vs %+v", res.Prefix(), got, want)
		}
		if want.Latitude != nil && (got.Location.Latitude != *want.Latitude || got.Location.Longitude != *want.Longitude) {
			t.Errorf("network %s: coordinates differ", res.Prefix())
		}
		if want.Latitude == nil && got.Location.Latitude != 0 {
			t.Errorf("network %s: coordinates appeared", res.Prefix())
		}
	}
	n := 0
	for res := range r.Networks() {
		if res.Err() != nil {
			t.Fatal(res.Err())
		}
		n++
	}
	if count != 7 || n != count {
		t.Errorf("networks: input %d, output %d", count, n)
	}

	// Metadata.
	m, sm := r.Metadata, src.Metadata
	if m.DatabaseType != "OpenIPData-City" || len(m.Languages) != 1 || m.Languages[0] != "en" ||
		m.BuildEpoch != sm.BuildEpoch || m.IPVersion != 6 || m.RecordSize != 28 ||
		len(m.Description) != len(sm.Description) || m.Description["openipdata"] == "" {
		t.Errorf("metadata: %+v", m)
	}
	for k, v := range sm.Description {
		if m.Description[k] != v {
			t.Errorf("description %q differs", k)
		}
	}

	// The same input gives the same bytes.
	again := filepath.Join(dir, "again.mmdb")
	if _, err := convertMMDB(stamped, again, geonames); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(out)
	b, _ := os.ReadFile(again)
	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Error("two runs gave different bytes")
	}

	// Without GeoNames: no ids and no time zones, the rest as before.
	plain := filepath.Join(dir, "plain.mmdb")
	stats, err = convertMMDB(stamped, plain, "")
	if err != nil {
		t.Fatal(err)
	}
	if stats.placed != 0 || stats.withTZ != 0 || stats.networks != 7 {
		t.Errorf("stats without GeoNames = %+v", stats)
	}
	praw := lookupGeo(t, plain, "45.0.0.9")
	if hasKey(praw, "city", "geoname_id") || hasKey(praw, "location", "time_zone") || !hasKey(praw, "location", "latitude") {
		t.Errorf("without GeoNames: %v", praw)
	}
}

func TestConvertFailures(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mmdb")
	os.WriteFile(out, []byte("previous"), 0o644)
	if _, err := convertMMDB(filepath.Join(dir, "missing.mmdb"), out, ""); err == nil {
		t.Error("missing input: no error")
	}
	if data, _ := os.ReadFile(out); string(data) != "previous" {
		t.Error("a failed convert replaced the output")
	}
}
