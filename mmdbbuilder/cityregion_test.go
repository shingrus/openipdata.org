package main

import (
	"fmt"
	"math"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang/v2"
)

// Two namesakes (one name, two places) with equal counts or equal populations: the winner of
// pickCity is the one with the smaller place key, whatever the map order.
func TestPickCityNamesakeTie(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		gnLine("1", "Springfield", "40.00000", "-90.00000", "US", "IL", "100000", "America/Chicago")+
			gnLine("2", "Springfield", "40.10000", "-90.10000", "US", "MO", "100000", "America/Chicago"))
	index, _, err := loadGeoNamesOpts(path, false, true)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := index.placeOf(place{code: "US", city: "Springfield", region: "IL"})
	b, _ := index.placeOf(place{code: "US", city: "Springfield", region: "MO"})
	want := spelled{"Springfield", "IL"}
	if placeKey(b) < placeKey(a) {
		want = spelled{"Springfield", "MO"}
	}
	disks := []disk{{40.05, -90.05, 100}}
	for _, byCount := range []bool{true, false} {
		for i := 0; i < 100; i++ {
			g := &asnGroup{cities: map[string]*cityCount{}}
			g.add(placeKey(a), "springfield", spelled{"Springfield", "IL"}, 10)
			g.add(placeKey(b), "springfield", spelled{"Springfield", "MO"}, 10)
			got, ok := pickCity(g, "US", index, disks, byCount)
			if !ok || got != want {
				t.Fatalf("byCount %v: got %v %v, want %v", byCount, got, ok, want)
			}
		}
	}
}

func TestValidCoords(t *testing.T) {
	nan := math.NaN()
	for _, tc := range []struct {
		lat, lon float64
		want     bool
	}{
		{0, 0, true}, {90, 180, true}, {-90, -180, true}, {43.66, -70.25, true},
		{90.1, 0, false}, {0, 180.1, false}, {-91, 0, false}, {0, -181, false},
		{nan, 0, false}, {0, nan, false}, {math.Inf(1), 0, false}, {0, math.Inf(-1), false},
	} {
		if got := validCoords(tc.lat, tc.lon); got != tc.want {
			t.Errorf("validCoords(%v, %v) = %v", tc.lat, tc.lon, got)
		}
	}
}

// A record with coordinates out of range is placed by name, as a record with none.
func TestGeoNamesPlacerBadCoordinates(t *testing.T) {
	index, _ := regionIndex(t, t.TempDir(), true)
	path := writeFlatMMDB(t, "ip2geo", map[string]mmdbtype.Map{
		"45.0.0.0/24": {"country_code": mmdbtype.String("US"), "city_name": mmdbtype.String("Portland"),
			"latitude": mmdbtype.Float64(123), "longitude": mmdbtype.Float64(10)},
		"45.0.1.0/24": {"country_code": mmdbtype.String("US"), "city_name": mmdbtype.String("Portland"),
			"latitude": mmdbtype.Float64(43.6615), "longitude": mmdbtype.Float64(-70.2553)},
	})
	r, err := maxminddb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	place := geoNamesPlacer(r, index)
	lat, _, a, _, err := place(netip.MustParseAddr("45.0.0.1"))
	if err != nil || a != placedCity || round4(lat) != 45.5234 {
		t.Errorf("bad coordinates: lat %v answer %v err %v", lat, a, err)
	}
	if lat, _, _, _, _ = place(netip.MustParseAddr("45.0.1.1")); lat != 43.6615 {
		t.Errorf("good coordinates: lat %v", lat)
	}
}

// A layer without a region column, and a row that ends before the region or the city, give an
// empty region and no panic.
func TestReadLayerRegionsMissing(t *testing.T) {
	type row struct{ prefix, code, region, city string }
	read := func(body string) []row {
		path := writeFile(t, filepath.Join(t.TempDir(), "layer.csv.gz"), body)
		var got []row
		if err := readLayerRegions(path, func(_ int, p, c, region, city string) { got = append(got, row{p, c, region, city}) }); err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := read("prefix,country,city\n1.2.3.0/24,US,Portland\n1.2.4.0/24,US\n")
	want := []row{{"1.2.3.0/24", "US", "", "Portland"}, {"1.2.4.0/24", "US", "", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("no region column: %v", got)
	}
	got = read("prefix,country,region,city,source\n1.2.3.0/24,US\n1.2.4.0/24,US,US-ME\n1.2.5.0/24,US,US-ME,Portland\n")
	want = []row{{"1.2.3.0/24", "US", "", ""}, {"1.2.4.0/24", "US", "US-ME", ""}, {"1.2.5.0/24", "US", "US-ME", "Portland"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("short rows: %v", got)
	}
}

// gnLine is a cities1000.txt line with an admin1 code and a time zone.
func gnLine(id, name, lat, lon, country, admin1, pop, tz string) string {
	return strings.Join([]string{id, name, name, "", lat, lon, "P", "PPL", country, "", admin1, "", "", "", pop, "", "10", tz, "2024-01-01"}, "\t") + "\n"
}

// regionIndex loads a small GeoNames file with the namesakes of the problem: Portland in
// Oregon (the bigger) and Maine, Arlington in Texas and Virginia, and the Italian Venezia.
func regionIndex(t *testing.T, dir string, withRegions bool) (geoNames, string) {
	t.Helper()
	path := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		gnLine("1", "Portland", "45.52345", "-122.67621", "US", "OR", "652503", "America/Los_Angeles")+
			gnLine("2", "Portland", "43.66147", "-70.25533", "US", "ME", "66194", "America/New_York")+
			gnLine("3", "Arlington", "32.73569", "-97.10807", "US", "TX", "394266", "America/Chicago")+
			gnLine("4", "Arlington", "38.88101", "-77.10428", "US", "VA", "238643", "America/New_York")+
			gnLine("5", "Boston", "42.35843", "-71.05977", "US", "MA", "675647", "America/New_York")+
			gnLine("10", "Venezia", "45.43713", "12.33265", "IT", "20", "260000", "Europe/Rome")+
			gnLine("11", "Venezia", "40.00000", "10.00000", "IT", "09", "1000", "Europe/Rome"))
	index, _, err := loadGeoNamesOpts(path, false, withRegions)
	if err != nil {
		t.Fatal(err)
	}
	return index, path
}

// regionRows24 is rows24 with a region column.
func regionRows24(second, from, count int, country, region, city string) string {
	var b strings.Builder
	for i := from; i < from+count; i++ {
		fmt.Fprintf(&b, "10.%d.%d.0/24,%s,%s,%s,x\n", second, i, country, region, city)
	}
	return b.String()
}

// The case table is the one of TestFeedState in hatchet/geo/derivedstate_test.go (feedState there
// must equal regionAdmin1): the two tables must stay identical.
func TestRegionAdmin1(t *testing.T) {
	for _, tc := range []struct{ country, region, want string }{
		{"US", "US-ME", "ME"},
		{"us", " us-me ", "ME"},
		{"US", "me", "ME"},
		{"US", "ME", "ME"},
		{"US", "", ""},
		{"US", "US-", ""},
		{"US", "US-MEE", ""},
		{"US", "M1", ""},
		{"US", "Maine", ""},
		{"US", "-ME", ""},
		{"US", "US-É1", ""},
		{"IT", "IT-25", ""}, // another country: never guessed
		{"IT", "MI", ""},
		{"GB", "GB-ENG", ""},
		{"US", "US-CT", "CT"},
		{"US", "CT", "CT"},
		{"US", " us-ct ", "CT"},
		{"us", "ct", "CT"},
		{"US", "US-US", "US"},
		{"US", "US-C", ""},
		{"US", "US-CTX", ""},
		{"US", "US-C1", ""},
		{"US", "1T", ""},
		{"US", "US-CÉ", ""},
		{"US", "CA-ON", ""},
		{"US", "US-US-CT", ""},
		{"CA", "CA-ON", ""},
		{"CA", "ON", ""},
		{"DE", "US-CT", ""},
		{"", "CT", ""},
	} {
		if got := regionAdmin1(tc.country, tc.region); got != tc.want {
			t.Errorf("regionAdmin1(%q, %q) = %q, want %q", tc.country, tc.region, got, tc.want)
		}
	}
}

func TestPlaceIn(t *testing.T) {
	index, _ := regionIndex(t, t.TempDir(), true)
	if p, reg, ok := index.placeIn("US", "US-ME", "portland"); !ok || reg != "ME" || p.id != 2 {
		t.Errorf("Portland US-ME = %+v %q %v", p, reg, ok)
	}
	// The regional place is the default one: no region.
	if p, reg, ok := index.placeIn("US", "US-OR", "Portland"); !ok || reg != "" || p.id != 1 {
		t.Errorf("Portland US-OR = %+v %q %v", p, reg, ok)
	}
	if _, reg, ok := index.placeIn("US", "US-TX", "Portland"); !ok || reg != "" {
		t.Errorf("Portland US-TX: region %q ok %v", reg, ok)
	}
	if _, _, ok := index.placeIn("US", "US-ME", "Nowhere"); ok {
		t.Error("Nowhere was placed")
	}
	// No regional index: only the default.
	plain, _ := regionIndex(t, t.TempDir(), false)
	if p, reg, ok := plain.placeIn("US", "US-ME", "Portland"); !ok || reg != "" || p.id != 1 {
		t.Errorf("without regions: %+v %q %v", p, reg, ok)
	}
	// Without a default place the region changes nothing.
	if _, reg, ok := index.placeIn("US", "US-ME", "Nowhere"); ok || reg != "" {
		t.Errorf("Nowhere: region %q ok %v", reg, ok)
	}
}

// Loops over the index must skip the regional keys: with them in the map, map order used to
// decide whether a place with both keys (Boston) was filed under "US" or under "US:MA".
func TestRegionalKeysAreSkippedByLoops(t *testing.T) {
	with, _ := regionIndex(t, t.TempDir(), true)
	without, _ := regionIndex(t, t.TempDir(), false)
	for key := range with {
		if _, ok := plainKeyCountry(key); !ok && !strings.HasPrefix(key, "US:") {
			t.Errorf("unexpected key %q", key)
		}
	}
	ids := func(ps []gnPlace) string {
		var s []string
		for _, p := range ps {
			s = append(s, fmt.Sprint(p.id))
		}
		return strings.Join(s, ",")
	}
	want := ids(newRTTChecker(without, nil).places("US"))
	if want == "" {
		t.Fatal("no US places")
	}
	wantCells := newPingWorld(without, nil)
	for i := 0; i < 60; i++ {
		c := newRTTChecker(with, nil)
		if got := ids(c.places("US")); got != want {
			t.Fatalf("US places with regions = %s, want %s", got, want)
		}
		for cc := range c.byCountry {
			if len(cc) != 2 {
				t.Fatalf("country key %q", cc)
			}
		}
		w := newPingWorld(with, nil)
		if fmt.Sprint(w.codes, w.cells) != fmt.Sprint(wantCells.codes, wantCells.cells) {
			t.Fatalf("ping cells differ with regions: %v vs %v", w.cells, wantCells.cells)
		}
	}
}

func TestCityRegionPlacing(t *testing.T) {
	dir := t.TempDir()
	index, _ := regionIndex(t, dir, true)
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), "prefix,country,region,city,source\n"+
		"45.0.0.0/24,US,US-ME,Portland,x\n"+
		"45.0.1.0/24,US,US-OR,Portland,x\n"+
		"45.0.2.0/24,US,,Portland,x\n"+
		"45.0.3.0/24,US,US-ZZ,Portland,x\n"+
		"45.0.4.0/24,US,US-TX,Portland,x\n"+ // no Portland in Texas
		"45.0.5.0/24,US,Maine,Portland,x\n"+ // malformed
		"45.0.6.0/24,US,me,Portland,x\n"+ // a bare state, any case
		"45.0.7.0/24,US, US-ME ,portland,x\n"+ // spaces; the city is written as in the row
		"45.0.8.0/24,IT,IT-09,Venezia,x\n"+ // not the US: the region is not used
		"45.0.9.0/24,US,US-VA,Arlington,x\n"+
		"45.0.10.0/24,US,US-ME,,x\n"+ // a region and no city
		"45.0.11.0/24,US,US-ME,Nowhere,x\n")
	out := filepath.Join(dir, "out.mmdb")
	stats, err := buildGeo([]string{layer}, "", out, false, nil, nil, nil, index, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].regioned != 4 { // 45.0.0.0, .6, .7 and the Virginia Arlington
		t.Errorf("stats = %+v, want 4 rows placed by their region", stats)
	}
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	type want struct {
		lat    float64
		region string // "" = no city_region key
	}
	for ip, w := range map[string]want{
		"45.0.0.1":  {43.6615, "ME"},
		"45.0.1.1":  {45.5234, ""},
		"45.0.2.1":  {45.5234, ""},
		"45.0.3.1":  {45.5234, ""},
		"45.0.4.1":  {45.5234, ""},
		"45.0.5.1":  {45.5234, ""},
		"45.0.6.1":  {43.6615, "ME"},
		"45.0.7.1":  {43.6615, "ME"},
		"45.0.8.1":  {45.4371, ""},
		"45.0.9.1":  {38.8810, "VA"},
		"45.0.10.1": {0, ""},
	} {
		m := record(t, r, ip)
		region, has := m["city_region"]
		if w.region == "" && has || w.region != "" && region != w.region {
			t.Errorf("%s: city_region = %v (%v), want %q", ip, region, has, w.region)
		}
		lat, hasLat := m["latitude"]
		if w.lat == 0 {
			if hasLat {
				t.Errorf("%s: unexpected coordinates in %v", ip, m)
			}
			continue
		}
		if lat != w.lat {
			t.Errorf("%s: latitude %v, want %v", ip, lat, w.lat)
		}
	}
	// A city the index lacks: no coordinates, no region.
	if m := record(t, r, "45.0.11.1"); m["city_region"] != nil || m["latitude"] != nil {
		t.Errorf("Nowhere = %v", m)
	}
	// The city name written is the row's own.
	if got := cityOf(t, r, "45.0.7.1"); got != "US/portland/layer" {
		t.Errorf("45.0.7.1 = %s", got)
	}
	// A region never changes the country or the source.
	if got := cityOf(t, r, "45.0.0.1"); got != "US/Portland/layer" {
		t.Errorf("45.0.0.1 = %s", got)
	}
}

// A build in which no row's region changes a placing is the same as a build without the
// regional index; and the regional index does not change a build without regions.
func TestCityRegionNoChangeIsIdentical(t *testing.T) {
	dir := t.TempDir()
	with, _ := regionIndex(t, dir, true)
	without, _ := regionIndex(t, dir, false)
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), "prefix,country,region,city,source\n"+
		"45.0.1.0/24,US,US-OR,Portland,x\n"+
		"45.0.2.0/24,US,,Portland,x\n"+
		"45.0.3.0/24,US,US-ZZ,Portland,x\n"+
		"45.0.4.0/24,US,US-TX,Portland,x\n"+
		"45.0.5.0/24,IT,IT-09,Venezia,x\n"+
		"45.0.6.0/24,US,US-TX,Arlington,x\n"+
		"45.0.7.0/24,US,US-TX,Boston,x\n")
	a, b := filepath.Join(dir, "a.mmdb"), filepath.Join(dir, "b.mmdb")
	if _, err := buildGeo([]string{layer}, "", a, false, nil, nil, nil, with, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := buildGeo([]string{layer}, "", b, false, nil, nil, nil, without, ""); err != nil {
		t.Fatal(err)
	}
	da, db := dump(t, a), dump(t, b)
	if da != db || strings.Contains(da, "city_region") {
		t.Errorf("records differ:\n%s\n%s", da, db)
	}
}

// Guesses learned from regional evidence write the regional place: Portland of Maine must not
// become Portland of Oregon, and two spellings of the one place count together.
func TestCityRegionGuesses(t *testing.T) {
	dir := t.TempDir()
	index, _ := regionIndex(t, dir, true)
	layer := regionRows24(1, 0, 3, "US", "US-ME", "Portland") +
		regionRows24(1, 3, 3, "US", "US-ME", "portland") + // two spellings of one place: 6 /24s
		"10.1.9.0/24,US,,,x\n" + // cityless, announced by AS100: 80-asn-main
		"10.2.0.0/24,US,,,x\n" // cityless, no ASN: 86-country-main
	asn := "100,10.1.0.0/16\n"
	out, stats := guessBuild(t, dir, layer, &asn, index, true)
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for ip, source := range map[string]string{"10.1.9.1": "80-asn-main", "10.2.0.1": "86-country-main"} {
		m := record(t, r, ip)
		if m["city_name"] != "Portland" || m["city_source"] != source || m["city_region"] != "ME" || m["latitude"] != 43.6615 || m["longitude"] != -70.2553 {
			t.Errorf("%s = %v", ip, m)
		}
	}
	if s := statsOf(t, stats, asnMainSource); s.rows != 1 || !strings.HasPrefix(s.note, "asn-main: 1 groups qualified of 1,") {
		t.Errorf("80 stats = %+v", s)
	}
}

// The nearest-piece rung writes the region of the piece it copies.
func TestCityRegionNearPiece(t *testing.T) {
	dir := t.TempDir()
	index, _ := regionIndex(t, dir, true)
	// AS100 has Portland ME and Portland OR pieces; the chunk next to the ME piece takes it.
	layer := "10.1.0.0/24,US,US-ME,Portland,x\n" +
		"10.1.1.0/24,US,,,x\n" +
		"10.1.64.0/24,US,,Portland,x\n" // Oregon, far away
	asn := "100,10.1.0.0/16\n"
	out, _ := guessBuild(t, dir, layer, &asn, index, true)
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	m := record(t, r, "10.1.1.9")
	if m["city_source"] != "84-asn-near" || m["city_region"] != "ME" || m["latitude"] != 43.6615 {
		t.Errorf("10.1.1.9 = %v", m)
	}
	if m := record(t, r, "10.1.64.9"); m["city_region"] != nil || m["latitude"] != 45.5234 {
		t.Errorf("10.1.64.9 = %v", m)
	}
}

// The RTT city check can pick a regional city, and a regional record inside its disks is not
// impossible.
func TestCityRegionRTTCity(t *testing.T) {
	dir := t.TempDir()
	index, _ := regionIndex(t, dir, true)
	main := writeFile(t, filepath.Join(dir, "10-main.csv.gz"), "prefix,country,region,city,source\n"+
		"10.1.1.0/24,US,,Portland,x\n"+ // Oregon: 2 ms from Boston is impossible
		"10.1.2.0/24,US,US-ME,Portland,x\n"+ // Maine: inside the disk, kept
		"10.1.8.0/22,US,US-ME,Portland,x\n")
	asn := writeFile(t, filepath.Join(dir, "asn.csv.gz"), "asn,prefix\n1,10.1.0.0/16\n")
	rtt := writeFile(t, filepath.Join(dir, "rtt.csv.gz"), rttHeader+
		"10.1.1.0/24,US,Boston,2\n10.1.2.0/24,US,Boston,2\n")
	out := filepath.Join(dir, "out.mmdb")
	if _, err := buildGeoNames([]string{main}, "", out, false, nil, nil, nil, index, nil, asn, false, "", "", rtt, "", ""); err != nil {
		t.Fatal(err)
	}
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	m := record(t, r, "10.1.1.9")
	if m["city_name"] != "Portland" || m["city_source"] != rttNetworkSource || m["city_region"] != "ME" || m["latitude"] != 43.6615 {
		t.Errorf("10.1.1.9 = %v", m)
	}
	m = record(t, r, "10.1.2.9")
	if m["city_source"] != "10-main" || m["city_region"] != "ME" {
		t.Errorf("10.1.2.9 = %v", m)
	}
}

// The holder city is placed in the state of its row.
func TestCityRegionHolder(t *testing.T) {
	dir := t.TempDir()
	index, _ := regionIndex(t, dir, true)
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), holderHeader+"10.0.0.0/16,US,,,x\n10.1.0.0/16,US,,,x\n")
	asn := writeFile(t, filepath.Join(dir, "asn.csv.gz"), "asn,prefix\n100,10.9.0.0/24\n")
	holders := writeFile(t, filepath.Join(dir, "holders.csv.gz"), holderHeader+
		"10.0.0.0/16,US,US-ME,Portland,h\n10.1.0.0/16,US,,Portland,h\n")
	out := filepath.Join(dir, "out.mmdb")
	if _, err := buildGeoGuesses([]string{layer}, "", out, false, nil, nil, nil, index, asn, false, holders, ""); err != nil {
		t.Fatal(err)
	}
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	m := record(t, r, "10.0.5.1")
	if m["city_source"] != holderCitySource || m["city_region"] != "ME" || m["latitude"] != 43.6615 {
		t.Errorf("10.0.5.1 = %v", m)
	}
	m = record(t, r, "10.1.5.1")
	if m["city_source"] != holderCitySource || m["city_region"] != nil || m["latitude"] != 45.5234 {
		t.Errorf("10.1.5.1 = %v", m)
	}
}

// convert gives a regional record its own geoname_id and time zone and adds no key.
func TestCityRegionConvert(t *testing.T) {
	dir := t.TempDir()
	index, geonames := regionIndex(t, dir, true)
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), "prefix,country,region,city,source\n"+
		"45.0.0.0/24,US,US-ME,Portland,x\n"+
		"45.0.1.0/24,US,,Portland,x\n"+
		"45.0.2.0/24,US,US-VA,Arlington,x\n")
	flat := filepath.Join(dir, "flat.mmdb")
	if _, err := buildGeo([]string{layer}, "", flat, false, nil, nil, nil, index, ""); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "openipdata-org-ip2geo.mmdb")
	if _, err := convertMMDB(flat, out, geonames); err != nil {
		t.Fatal(err)
	}
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for ip, w := range map[string][3]string{ // geoname id, time zone, latitude
		"45.0.0.1": {"2", "America/New_York", "43.6615"},
		"45.0.1.1": {"1", "America/Los_Angeles", "45.5234"},
		"45.0.2.1": {"4", "America/New_York", "38.881"},
	} {
		var m struct {
			City struct {
				ID uint32 `maxminddb:"geoname_id"`
			} `maxminddb:"city"`
			Location struct {
				TZ  string  `maxminddb:"time_zone"`
				Lat float64 `maxminddb:"latitude"`
			} `maxminddb:"location"`
		}
		if err := r.Lookup(netip.MustParseAddr(ip)).Decode(&m); err != nil {
			t.Fatal(err)
		}
		got := [3]string{fmt.Sprint(m.City.ID), m.Location.TZ, fmt.Sprint(m.Location.Lat)}
		if got != w {
			t.Errorf("%s = %v, want %v", ip, got, w)
		}
		if all := record(t, r, ip); all["city_region"] != nil || all["subdivisions"] != nil {
			t.Errorf("%s has a regional key: %v", ip, all)
		}
	}
}

// The evaluation uses a record's own coordinates, and places by name when it has none.
func TestGeoNamesPlacerUsesCoordinates(t *testing.T) {
	dir := t.TempDir()
	index, _ := regionIndex(t, dir, true)
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), "prefix,country,region,city,source\n"+
		"45.0.0.0/24,US,US-ME,Portland,x\n"+
		"45.0.1.0/24,US,,Nowhere,x\n"+
		"45.0.2.0/24,US,,,x\n")
	withCoords, plain := filepath.Join(dir, "coords.mmdb"), filepath.Join(dir, "plain.mmdb")
	if _, err := buildGeo([]string{layer}, "", withCoords, false, nil, nil, nil, index, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := buildGeo([]string{layer}, "", plain, false, nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	check := func(path, ip string, lat float64, answer cityAnswer) {
		t.Helper()
		r, err := maxminddb.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		got, _, a, _, err := geoNamesPlacer(r, index)(netip.MustParseAddr(ip))
		if err != nil {
			t.Fatal(err)
		}
		if a != answer || a == placedCity && round4(got) != lat {
			t.Errorf("%s %s: lat %v answer %v, want %v %v", filepath.Base(path), ip, got, a, lat, answer)
		}
	}
	check(withCoords, "45.0.0.1", 43.6615, placedCity) // the record's own (Maine) coordinates
	check(plain, "45.0.0.1", 45.5234, placedCity)      // no coordinates: by name, the default Portland
	check(withCoords, "45.0.1.1", 0, unplaced)         // a city GeoNames lacks
	check(withCoords, "45.0.2.1", 0, noCity)
}
