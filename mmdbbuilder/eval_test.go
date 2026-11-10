package main

import (
	"encoding/json"
	"math"
	"net/netip"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// refDB writes a reference database in the shape of DB-IP (country.iso_code,
// city.names.en) or, with city empty, IPinfo (country_code).
func refDB(t *testing.T, path, kind string, rows [][3]string) string {
	t.Helper()
	writer, err := newWriter(kind, kind)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		var record mmdbtype.Map
		if kind == "dbip" {
			record = mmdbtype.Map{
				"country": mmdbtype.Map{"iso_code": mmdbtype.String(r[1])},
				"city":    mmdbtype.Map{"names": mmdbtype.Map{"en": mmdbtype.String(r[2])}},
			}
		} else {
			record = mmdbtype.Map{"country_code": mmdbtype.String(r[1])}
		}
		if err := writer.Insert(ipNet(netip.MustParsePrefix(r[0])), record); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeAtomic(writer, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func approx(got *float64, want float64) bool {
	return got != nil && math.Abs(*got-want) < 0.005
}

func TestEvaluate(t *testing.T) {
	dir := t.TempDir()
	// 1,280 addresses of delegated space; the IPv6 row and 47.0.0.0/24 do not count.
	rir := writeFile(t, filepath.Join(dir, "rir.csv"), "prefix,country,city\n"+
		"45.0.0.0/24,US,\n45.0.1.0/24,DE,\n45.0.2.0/24,FR,\n45.0.3.0/24,,\n46.0.0.0/24,,\n2a00::/12,DE,\n")
	cities := writeFile(t, filepath.Join(dir, "cities.csv"), "prefix,country,city\n"+
		"45.0.0.0/25,US,Dallas\n45.0.1.0/24,DE,Berlin\n45.0.2.0/25,FR,Paris\n47.0.0.0/24,US,Elsewhere\n")
	ours := filepath.Join(dir, "ours.mmdb")
	if _, err := buildGeo([]string{rir, cities}, "", ours, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	dbip := refDB(t, filepath.Join(dir, "dbip.mmdb"), "dbip", [][3]string{
		{"45.0.0.0/24", "US", "Austin"},
		{"45.0.1.0/24", "DE", " berlin "}, // same city once trimmed and lower-cased
		{"45.0.2.0/24", "NL", "Paris"},
		{"45.0.3.0/24", "FR", "Lyon"}, // we have nothing here
	})
	ipinfo := refDB(t, filepath.Join(dir, "ipinfo.mmdb"), "ipinfo", [][3]string{
		{"45.0.0.0/25", "US", ""},
		{"45.0.0.128/25", "CA", ""},
		{"45.0.1.0/24", "DE", ""},
		{"45.0.2.0/24", "NL", ""},
	})
	atlas := writeFile(t, filepath.Join(dir, "atlas.csv"), "ip,country,lat,lon\n"+
		"45.0.0.5,US,1,2\n45.0.1.5,DE,1,2\n45.0.2.5,NL,1,2\n45.0.3.5,FR,1,2\nnope,US,1,2\n2a00::1,DE,1,2\n")
	holdoutRIR := writeFile(t, filepath.Join(dir, "holdout-cities.csv"), "prefix,country,city\n"+
		"45.0.0.0/24,US,Dallas\n45.0.1.0/24,DE,\n45.0.2.0/24,NL,\n")
	holdout := filepath.Join(dir, "holdout.mmdb")
	if _, err := buildGeo([]string{holdoutRIR}, "", holdout, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	truth := writeFile(t, filepath.Join(dir, "truth.csv"), "prefix,country,city\n"+
		"45.0.0.0/24,US,Dallas\n45.0.1.0/24,DE,Berlin\n45.0.2.0/24,FR,Paris\n45.0.3.0/24,FR,\n")

	m, err := evaluate(evalInputs{db: ours, rir: rir, dbip: dbip, ipinfo: ipinfo, atlas: atlas, holdoutDB: holdout, holdoutTruth: truth})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		got  *float64
		want float64
	}{
		"country_pct":                 {m.CountryPct, 60},               // 768 of 1,280
		"city_pct":                    {m.CityPct, 40},                  // Dallas 128, Berlin 256, Paris 128
		"dbip_country_agree_pct":      {m.DBIPCountryAgreePct, 66.67},   // 512 of 768
		"dbip_city_agree_pct":         {m.DBIPCityAgreePct, 66.67},      // Berlin 256 agrees, Dallas/Austin 128 does not
		"ipinfo_country_agree_pct":    {m.IPinfoCountryAgreePct, 50},    // 384 of 768
		"consensus_country_agree_pct": {m.ConsensusCountryAgreePct, 60}, // US 128, DE 256 and NL 256 agreed; we have FR for NL
		"atlas_country_pct":           {m.AtlasCountryPct, 50},
		"holdout_country_pct":         {m.HoldoutCountryPct, 50},
		"holdout_city_pct":            {m.HoldoutCityPct, 33.33},
	} {
		if !approx(c.got, c.want) {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
	if m.AtlasProbes == nil || *m.AtlasProbes != 4 || m.HoldoutPrefixes == nil || *m.HoldoutPrefixes != 4 {
		t.Errorf("probes %v, holdout prefixes %v", m.AtlasProbes, m.HoldoutPrefixes)
	}
}

// Scores whose reference is missing are left out, not zero.
func TestEvaluateOmitsMissingReferences(t *testing.T) {
	dir := t.TempDir()
	rir := writeFile(t, filepath.Join(dir, "rir.csv"), "prefix,country,city\n45.0.0.0/24,US,\n")
	ours := filepath.Join(dir, "ours.mmdb")
	if _, err := buildGeo([]string{rir}, "", ours, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := evaluate(evalInputs{db: ours, rir: rir})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m)
	var got map[string]float64
	json.Unmarshal(data, &got)
	if !reflect.DeepEqual(got, map[string]float64{"country_pct": 100, "city_pct": 0}) {
		t.Errorf("metrics = %s", data)
	}

	// A reference that shares no addresses with us has no score either.
	dbip := refDB(t, filepath.Join(dir, "dbip.mmdb"), "dbip", [][3]string{{"60.0.0.0/24", "JP", "Tokyo"}})
	if m, err = evaluate(evalInputs{db: ours, rir: rir, dbip: dbip}); err != nil || m.DBIPCountryAgreePct != nil || m.DBIPCityAgreePct != nil {
		t.Errorf("no overlap: %+v %v", m, err)
	}
	if _, err := evaluate(evalInputs{db: ours, rir: rir, holdoutDB: ours}); err == nil {
		t.Error("-holdout-db without -holdout-truth: no error")
	}
	if _, err := evaluate(evalInputs{db: ours, rir: writeFile(t, filepath.Join(dir, "v6.csv"), "prefix,country,city\n2a00::/12,DE,\n")}); err == nil {
		t.Error("an RIR layer without IPv4 space: no error")
	}
}

func TestJoin(t *testing.T) {
	type seg struct {
		lo, hi uint32
		x, y   bool
	}
	a := []rng{{lo: 0, hi: 9}, {lo: 20, hi: 29}, {lo: math.MaxUint32 - 1, hi: math.MaxUint32}}
	b := []rng{{lo: 5, hi: 24}, {lo: math.MaxUint32, hi: math.MaxUint32}}
	var got []seg
	join(a, b, func(lo, hi uint32, x, y *rng) { got = append(got, seg{lo, hi, x != nil, y != nil}) })
	want := []seg{
		{0, 4, true, false}, {5, 9, true, true}, {10, 19, false, true}, {20, 24, true, true}, {25, 29, true, false},
		{math.MaxUint32 - 1, math.MaxUint32 - 1, true, false}, {math.MaxUint32, math.MaxUint32, true, true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("segments = %v\nwant       %v", got, want)
	}
}

func TestLoadSpaceMerges(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "rir.csv"), "prefix,country,city\n"+
		"45.0.1.0/24,,\n45.0.0.0/24,US,\n45.0.0.0/16,US,\n46.0.0.0/24,,\n46.0.1.0/24,,\n")
	got, err := loadSpace(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []rng{{lo: 45 << 24, hi: 45<<24 | 0xffff}, {lo: 46 << 24, hi: 46<<24 | 0x1ff}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("space = %v, want %v", got, want)
	}
}
