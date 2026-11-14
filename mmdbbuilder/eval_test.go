package main

import (
	"encoding/json"
	"math"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// refDB writes a reference database in the shape of DB-IP and GeoLite2
// (country.iso_code, city.names.en) or, with kind "plain", one with country_code only.
func refDB(t *testing.T, path, kind string, rows [][3]string) string {
	t.Helper()
	writer, err := newWriter(kind, kind)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		var record mmdbtype.Map
		if kind != "plain" {
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
	rir := writeFile(t, filepath.Join(dir, "rir.csv.gz"), "prefix,country,city\n"+
		"45.0.0.0/24,US,\n45.0.1.0/24,DE,\n45.0.2.0/24,FR,\n45.0.3.0/24,,\n46.0.0.0/24,,\n2a00::/12,DE,\n")
	cities := writeFile(t, filepath.Join(dir, "cities.csv.gz"), "prefix,country,city\n"+
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
	maxmind := refDB(t, filepath.Join(dir, "maxmind.mmdb"), "maxmind", [][3]string{
		{"45.0.0.0/24", "US", "Dallas"},
		{"45.0.1.0/25", "DE", "Berlin"},
		{"45.0.1.128/25", "AT", "Vienna"},
		{"45.0.2.0/24", "FR", "Paris"},
	})
	atlas := writeFile(t, filepath.Join(dir, "atlas.csv"), "ip,country,lat,lon\n"+
		"45.0.0.5,US,1,2\n45.0.1.5,DE,1,2\n45.0.2.5,NL,1,2\n45.0.3.5,FR,1,2\nnope,US,1,2\n2a00::1,DE,1,2\n")
	holdoutRIR := writeFile(t, filepath.Join(dir, "holdout-cities.csv.gz"), "prefix,country,city\n"+
		"45.0.0.0/24,US,Dallas\n45.0.1.0/24,DE,\n45.0.2.0/24,NL,\n")
	holdout := filepath.Join(dir, "holdout.mmdb")
	if _, err := buildGeo([]string{holdoutRIR}, "", holdout, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	truth := writeFile(t, filepath.Join(dir, "truth.csv"), "prefix,country,city\n"+
		"45.0.0.0/24,US,Dallas\n45.0.1.0/24,DE,Berlin\n45.0.2.0/24,FR,Paris\n45.0.3.0/24,FR,\n")

	m, err := evaluate(evalInputs{db: ours, rir: rir, dbip: dbip, maxmind: maxmind, atlas: atlas, holdoutDB: holdout, holdoutTruth: truth})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		got  *float64
		want float64
	}{
		// Ours: the same numbers as before the references were scored.
		"country_pct":            {m.CountryPct, 60}, // 768 of 1,280
		"city_pct":               {m.CityPct, 40},    // Dallas 128, Berlin 256, Paris 128
		"atlas_country_pct":      {m.AtlasCountryPct, 50},
		"holdout_country_pct":    {m.HoldoutCountryPct, 50},
		"holdout_city_pct":       {m.HoldoutCityPct, 33.33},
		"dbip_country_agree_pct": {m.DBIPCountryAgreePct, 66.67}, // 512 of 768
		"dbip_city_agree_pct":    {m.DBIPCityAgreePct, 66.67},    // Berlin 256 agrees, Dallas/Austin 128 does not

		"maxmind_country_agree_pct": {m.MaxMindCountryAgreePct, 83.33}, // 640 of 768: 45.0.1.128/25 is AT
		"maxmind_city_agree_pct":    {m.MaxMindCityAgreePct, 100},      // 384 of 384

		// Both references agree on US 256 and DE 128 (we agree with both); 45.0.1.128/25 (DE against AT),
		// 45.0.2.0/24 (NL against FR) have no majority; 45.0.3.0/24 has one vote: 384 of 384.
		"vendor_consensus_country_agree_pct": {m.VendorConsensusCountryAgreePct, 100},

		"dbip_country_pct":       {m.DBIPCountryPct, 80},
		"dbip_city_pct":          {m.DBIPCityPct, 80},
		"dbip_atlas_country_pct": {m.DBIPAtlasCountryPct, 100},

		"maxmind_country_pct":       {m.MaxMindCountryPct, 60},
		"maxmind_city_pct":          {m.MaxMindCityPct, 60},
		"maxmind_atlas_country_pct": {m.MaxMindAtlasCountryPct, 50},
	} {
		if !approx(c.got, c.want) {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
	if m.AtlasProbes == nil || *m.AtlasProbes != 4 || m.HoldoutPrefixes == nil || *m.HoldoutPrefixes != 4 {
		t.Errorf("probes %v, holdout prefixes %v", m.AtlasProbes, m.HoldoutPrefixes)
	}

	// One reference is not enough for a consensus.
	if m, err = evaluate(evalInputs{db: ours, rir: rir, dbip: dbip}); err != nil || m.VendorConsensusCountryAgreePct != nil || m.DBIPCountryAgreePct == nil {
		t.Errorf("one reference: %+v %v", m, err)
	}
}

func TestSweep(t *testing.T) {
	type seg struct {
		lo, hi uint32
		in     [3]bool
	}
	lists := [][]rng{
		{{lo: 0, hi: 9}, {lo: math.MaxUint32 - 1, hi: math.MaxUint32}},
		{{lo: 5, hi: 24}},
		{{lo: 8, hi: 9}, {lo: 30, hi: 30}},
	}
	var got []seg
	sweep(lists, func(lo, hi uint32, active []*rng) {
		s := seg{lo: lo, hi: hi}
		for k, r := range active {
			s.in[k] = r != nil
		}
		got = append(got, s)
	})
	want := []seg{
		{0, 4, [3]bool{true, false, false}}, {5, 7, [3]bool{true, true, false}}, {8, 9, [3]bool{true, true, true}},
		{10, 24, [3]bool{false, true, false}}, {30, 30, [3]bool{false, false, true}},
		{math.MaxUint32 - 1, math.MaxUint32, [3]bool{true, false, false}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("segments = %v\nwant       %v", got, want)
	}
}

// Scores whose reference is missing are left out, not zero.
func TestEvaluateOmitsMissingReferences(t *testing.T) {
	dir := t.TempDir()
	rir := writeFile(t, filepath.Join(dir, "rir.csv.gz"), "prefix,country,city\n45.0.0.0/24,US,\n")
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
	if _, err := evaluate(evalInputs{db: ours, rir: writeFile(t, filepath.Join(dir, "v6.csv.gz"), "prefix,country,city\n2a00::/12,DE,\n")}); err == nil {
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
	path := writeFile(t, filepath.Join(t.TempDir(), "rir.csv.gz"), "prefix,country,city\n"+
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

// refRow is a record of a reference MMDB: no city and no coordinates are left out.
type refRow struct {
	prefix, country, city string
	lat, lon              *float64
}

func f64(v float64) *float64 { return &v }

// refDBLoc writes a reference database with location.latitude/longitude.
func refDBLoc(t *testing.T, path string, rows []refRow) string {
	t.Helper()
	writer, err := newWriter("ref", "ref")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		record := mmdbtype.Map{"country": mmdbtype.Map{"iso_code": mmdbtype.String(r.country)}}
		if r.city != "" {
			record["city"] = mmdbtype.Map{"names": mmdbtype.Map{"en": mmdbtype.String(r.city)}}
		}
		if r.lat != nil {
			record["location"] = mmdbtype.Map{"latitude": mmdbtype.Float64(*r.lat), "longitude": mmdbtype.Float64(*r.lon)}
		}
		if err := writer.Insert(ipNet(netip.MustParsePrefix(r.prefix)), record); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeAtomic(writer, path); err != nil {
		t.Fatal(err)
	}
	return path
}

// geoNamesLine is a cities1000.txt line: 19 tab-separated fields.
func geoNamesLine(id, name, alternates, lat, lon, country, pop string) string {
	return strings.Join([]string{id, name, name, alternates, lat, lon, "P", "PPL", country, "", "00", "", "", "", pop, "", "10", "UTC", "2024-01-01"}, "\t") + "\n"
}

func TestHaversineKm(t *testing.T) {
	for _, c := range []struct{ lat1, lon1, lat2, lon2, want float64 }{
		{0, 0, 0, 0, 0},
		{0, 0, 0, 1, 111.19},                    // one degree of the equator: 6371 * pi / 180
		{52.52, 13.405, 48.8566, 2.3522, 877.4}, // Berlin - Paris
		{0, 0, 0, 180, 20015.09},                // half the circumference
	} {
		if got := haversineKm(c.lat1, c.lon1, c.lat2, c.lon2); math.Abs(got-c.want) > 0.5 {
			t.Errorf("haversineKm(%v,%v,%v,%v) = %.2f, want %.2f", c.lat1, c.lon1, c.lat2, c.lon2, got, c.want)
		}
	}
}

func TestLoadGeoNames(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "cities1000.txt"),
		geoNamesLine("1", "Paris", "", "43", "1", "FR", "120")+ // a hamlet first
			geoNamesLine("2", "Paris", "Parigi, Lutece", "48.85", "2.35", "FR", "2138551")+
			geoNamesLine("3", "Paris", "", "33.66", "-95.55", "US", "25171")+
			geoNamesLine("6", "Big", "Hamlet, \u5df4\u9ece", "10", "10", "FR", "900000")+ // an alternate name
			geoNamesLine("7", "Hamlet", "", "20", "20", "FR", "50")+ // the primary name of a small town wins
			geoNamesLine("4", "Same", "", "1", "1", "DE", "5")+
			geoNamesLine("5", "Same", "", "2", "2", "DE", "5")+ // a tie: the first stays
			"short\tline\n")
	index, err := loadGeoNames(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{
		"FR|paris": 48.85, "FR|parigi": 48.85, "FR|lutece": 48.85, "US|paris": 33.66, "DE|same": 1, "FR|hamlet": 20, "FR|big": 10,
	} {
		if got, ok := index[key]; !ok || got.lat != want {
			t.Errorf("%s = %+v, want lat %v", key, got, want)
		}
	}
	if _, ok := index["FR|\u5df4\u9ece"]; ok {
		t.Error("an alternate name without Latin letters was kept")
	}
	if _, ok := index["DE|paris"]; ok {
		t.Error("a name leaked into another country")
	}
	if _, err := loadGeoNames(writeFile(t, filepath.Join(t.TempDir(), "empty.txt"), "")); err == nil {
		t.Error("an empty dump: no error")
	}
}

// The Atlas city score: ours is placed with GeoNames, a reference uses its own
// coordinates, and only for a record that names a city.
func TestEvaluateAtlasCity(t *testing.T) {
	dir := t.TempDir()
	rir := writeFile(t, filepath.Join(dir, "rir.csv.gz"), "prefix,country,city\n"+
		"45.0.0.0/24,US,\n45.0.1.0/24,DE,\n45.0.2.0/24,FR,\n45.0.3.0/24,FR,\n45.0.4.0/24,US,\n")
	cities := writeFile(t, filepath.Join(dir, "cities.csv.gz"), "prefix,country,city\n"+
		"45.0.0.0/24,US,Dallas\n45.0.1.0/24,DE,Berlin\n45.0.2.0/24,FR,Paris\n45.0.4.0/24,US,Nowhereville\n")
	ours := filepath.Join(dir, "ours.mmdb")
	if _, err := buildGeo([]string{rir, cities}, "", ours, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	geonames := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		geoNamesLine("1", "Paris", "", "43", "1", "FR", "120")+ // the small namesake comes first
			geoNamesLine("2", "Paris", "Parigi", "48.85341", "2.3488", "FR", "2138551")+
			geoNamesLine("3", "Paris", "", "33.66", "-95.55", "US", "25171")+
			geoNamesLine("4", "Dallas City", "Dallas,Big D", "32.78306", "-96.80667", "US", "1197816")+ // found by an alternate name
			geoNamesLine("5", "Berlin", "Berlijn", "52.52437", "13.41053", "DE", "3426354"))
	noBerlin := writeFile(t, filepath.Join(dir, "no-berlin.txt"),
		geoNamesLine("4", "Dallas City", "Dallas,Big D", "32.78306", "-96.80667", "US", "1197816")+
			geoNamesLine("2", "Paris", "Parigi", "48.85341", "2.3488", "FR", "2138551"))
	dbip := refDBLoc(t, filepath.Join(dir, "dbip.mmdb"), []refRow{
		{"45.0.0.0/24", "US", "Austin", f64(30.27), f64(-97.74)}, // 280 km from the probe
		{"45.0.1.0/24", "DE", "Berlin", f64(53.40), f64(13.40)},  // 22 km
		{"45.0.2.0/24", "FR", "Paris", f64(48.86), f64(2.35)},
		{"45.0.3.0/24", "FR", "", f64(45.76), f64(4.84)}, // no city: the coordinates are the country's, a miss
		{"45.0.4.0/24", "US", "Nowhereville", f64(40.0), f64(-100.0)},
	})
	maxmind := refDBLoc(t, filepath.Join(dir, "maxmind.mmdb"), []refRow{
		{"45.0.0.0/24", "US", "Dallas", f64(32.78), f64(-96.80)},
		{"45.0.1.0/24", "DE", "Berlin", f64(52.52), f64(13.40)}, // 120 km from the probe
		{"45.0.2.0/24", "FR", "Paris", nil, nil},                // no coordinates
		{"45.0.3.0/24", "FR", "", f64(45.76), f64(4.84)},
	})
	// P6 has no coordinates and is not counted; P7 is in no database.
	atlas := writeFile(t, filepath.Join(dir, "atlas.csv"), "ip,country,lat,lon\n"+
		"45.0.0.5,US,32.80,-96.80\n"+ // P1: Dallas, found by an alternate name
		"45.0.1.5,DE,53.60,13.40\n"+ // P2: Berlin is 120 km away
		"45.0.2.5,FR,48.70,2.35\n"+ // P3: Paris, the big one of two
		"45.0.3.5,FR,45.76,4.84\n"+ // P4: we name no city
		"45.0.4.5,US,40.0,-100.0\n"+ // P5: a city GeoNames lacks
		"45.0.0.9,US,,\n"+ // P6
		"45.0.5.5,US,10,10\n"+ // P7
		"nope,US,1,2\n2a00::1,DE,1,2\n")

	in := evalInputs{db: ours, rir: rir, dbip: dbip, maxmind: maxmind, atlas: atlas, geonames: geonames}
	m, err := evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	if m.AtlasCityProbes == nil || *m.AtlasCityProbes != 6 || m.AtlasCityUnresolved == nil || *m.AtlasCityUnresolved != 1 {
		t.Errorf("probes %v, unresolved %v", m.AtlasCityProbes, m.AtlasCityUnresolved)
	}
	if m.AtlasProbes == nil || *m.AtlasProbes != 7 {
		t.Errorf("country probes %v, want 7", m.AtlasProbes)
	}
	for name, c := range map[string]struct {
		got  *float64
		want float64
	}{
		"atlas_city_pct":         {m.AtlasCityPct, 33.33},        // P1, P3 of 6
		"dbip_atlas_city_pct":    {m.DBIPAtlasCityPct, 50},       // P2, P3, P5
		"maxmind_atlas_city_pct": {m.MaxMindAtlasCityPct, 16.67}, // P1
	} {
		if !approx(c.got, c.want) {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}

	// Answered = a city with coordinates; precision = hits / answered.
	// Ours: P1, P2, P3 (P5 unplaced, P4 none), 2 hits. dbip: P1, P2, P3, P5, 3 hits.
	// maxmind: P1, P2 (P3 has no coordinates), 1 hit.
	for name, c := range map[string]struct {
		n    *int
		pct  *float64
		wn   int
		wpct float64
	}{
		"ours":    {m.AtlasCityAnswered, m.AtlasCityPrecisionPct, 3, 66.67},
		"dbip":    {m.DBIPAtlasCityAnswered, m.DBIPAtlasCityPrecisionPct, 4, 75},
		"maxmind": {m.MaxMindAtlasCityAnswered, m.MaxMindAtlasCityPrecisionPct, 2, 50},
	} {
		if c.n == nil || *c.n != c.wn || !approx(c.pct, c.wpct) {
			t.Errorf("%s answered %v precision %v, want %d, %v", name, c.n, c.pct, c.wn, c.wpct)
		}
	}

	// Without Berlin in GeoNames, P2 is unresolved instead of too far: still a miss.
	in.geonames = noBerlin
	if m, err = evaluate(in); err != nil || m.AtlasCityUnresolved == nil || *m.AtlasCityUnresolved != 2 || !approx(m.AtlasCityPct, 33.33) ||
		m.AtlasCityAnswered == nil || *m.AtlasCityAnswered != 2 || !approx(m.AtlasCityPrecisionPct, 100) {
		t.Errorf("no Berlin: %+v %v", m, err)
	}

	// Without GeoNames ours is left out, the references and the probe count stay.
	in.geonames = ""
	if m, err = evaluate(in); err != nil || m.AtlasCityPct != nil || m.AtlasCityUnresolved != nil || m.AtlasCityAnswered != nil || m.AtlasCityPrecisionPct != nil ||
		m.AtlasCityProbes == nil || !approx(m.DBIPAtlasCityPct, 50) {
		t.Errorf("no GeoNames: %+v %v", m, err)
	}
}
