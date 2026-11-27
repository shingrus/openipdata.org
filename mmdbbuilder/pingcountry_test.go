package main

import (
	"fmt"
	"math"
	"math/rand"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang/v2"
)

const pingHeader = "prefix,GB/London,FR/Paris,DE/Frankfurt am Main\n"

// pingIndex is a small GeoNames index: three sites (London, Paris, Frankfurt), the cities
// between them (Brussels, Amsterdam, Lille) and Dallas.
func pingIndex(t *testing.T, dir string) geoNames {
	t.Helper()
	path := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		geoNamesLine("1", "London", "", "51.50853", "-0.12574", "GB", "8961989")+
			geoNamesLine("2", "Paris", "", "48.85341", "2.3488", "FR", "2138551")+
			geoNamesLine("3", "Frankfurt am Main", "", "50.11552", "8.68417", "DE", "650000")+
			geoNamesLine("4", "Dallas", "", "32.78306", "-96.80667", "US", "1300000")+
			geoNamesLine("5", "Brussels", "", "50.85045", "4.34878", "BE", "1000000")+
			geoNamesLine("6", "Amsterdam", "", "52.37403", "4.88969", "NL", "900000")+
			geoNamesLine("7", "Lille", "", "50.63297", "3.05858", "FR", "800000"))
	index, err := loadGeoNames(path)
	if err != nil {
		t.Fatal(err)
	}
	return index
}

// pingBuild builds an MMDB from layer rows and a ping file body ("" for no -ping-country).
func pingBuild(t *testing.T, dir, layerRows, ping string, debug bool) (string, error) {
	t.Helper()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), holderHeader+layerRows)
	pingPath := ""
	if ping != "" {
		pingPath = writeFile(t, filepath.Join(dir, "hosting-rtt.csv.gz"), ping)
	}
	out := filepath.Join(dir, "out.mmdb")
	_, err := buildGeoNames([]string{layer}, "", out, debug, nil, nil, nil, pingIndex(t, dir), nil, "", false, "", "", "", pingPath, "")
	return out, err
}

func TestPingCountry(t *testing.T) {
	var layer, ping strings.Builder
	add := func(n int, country, city, row string) {
		fmt.Fprintf(&layer, "10.2.%d.0/24,%s,,%s,x\n", n, country, city)
		if row != "" {
			fmt.Fprintf(&ping, "10.2.%d.0/24,%s\n", n, row)
		}
	}
	london := "1,4,7"                     // only London is inside all three disks
	add(1, "US", "Dallas", london)        // impossible: GB holds all the weight
	add(2, "GB", "London", london)        // possible: kept
	add(3, "US", "Dallas", "1,,7")        // two usable cells: the row is skipped
	add(4, "US", "Dallas", "3.5,3.5,3.5") // Brussels, Amsterdam and Lille: no country holds half
	add(5, "JP", "Osaka", london)         // no cell of JP: never impossible
	add(6, "US", "Dallas", "0.1,0.1,0.1") // no possible cell at all
	dir := t.TempDir()
	out, err := pingBuild(t, dir, layer.String(), pingHeader+ping.String(), false)
	if err != nil {
		t.Fatal(err)
	}
	checkCities(t, out, map[string]string{
		"10.2.1.9": "GB/<nil>/<nil>",
		"10.2.2.9": "GB/London/layer",
		"10.2.3.9": "US/Dallas/layer",
		"10.2.4.9": "US/Dallas/layer",
		"10.2.5.9": "JP/Osaka/layer",
		"10.2.6.9": "US/Dallas/layer",
	})
}

func TestPingCountryOffIsIdentical(t *testing.T) {
	layer := "10.2.1.0/24,US,,Dallas,x\n10.2.2.0/24,GB,,London,x\n"
	a, err := pingBuild(t, t.TempDir(), layer, "", false)
	if err != nil {
		t.Fatal(err)
	}
	// A file that changes nothing: the GB /24 is possible, the other row has two cells.
	b, err := pingBuild(t, t.TempDir(), layer, pingHeader+"10.2.1.0/24,1,,7\n10.2.2.0/24,1,4,7\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if dump(t, a) != dump(t, b) {
		t.Errorf("outputs differ:\n%s\n%s", dump(t, a), dump(t, b))
	}
}

func TestPingCountryDebugRecord(t *testing.T) {
	out, err := pingBuild(t, t.TempDir(), "10.2.1.0/24,US,,Dallas,x\n", pingHeader+"10.2.1.0/24,1,4,7\n", true)
	if err != nil {
		t.Fatal(err)
	}
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	m := record(t, r, "10.2.1.9")
	if m["country_code"] != "GB" || m["network"] != "10.2.1.0/24" || m["city_name"] != nil {
		t.Errorf("record = %v", m)
	}
}

func TestPingCountryNeedsGeoNames(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), holderHeader+"10.2.1.0/24,US,,Dallas,x\n")
	ping := writeFile(t, filepath.Join(dir, "p.csv.gz"), pingHeader+"10.2.1.0/24,1,4,7\n")
	_, err := buildGeoNames([]string{layer}, "", filepath.Join(dir, "o.mmdb"), false, nil, nil, nil, nil, nil, "", false, "", "", "", ping, "")
	if err == nil || !strings.Contains(err.Error(), "-ping-country needs -geonames") {
		t.Errorf("err = %v", err)
	}
	_, err = buildGeoNames([]string{layer}, "", filepath.Join(dir, "o.mmdb"), false, nil, nil, nil, pingIndex(t, dir), nil, "", false, "", "", "", filepath.Join(dir, "none.csv.gz"), "")
	if err == nil {
		t.Error("a missing file was accepted")
	}
}

// Fill the guesses after the check: the country-main city of GB fills the changed /24.
func TestPingCountryGuessesFollow(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), holderHeader+rows24(2, 0, 10, "GB", "London")+"10.2.50.0/24,US,,,x\n")
	ping := writeFile(t, filepath.Join(dir, "p.csv.gz"), pingHeader+"10.2.50.0/24,1,4,7\n")
	out := filepath.Join(dir, "out.mmdb")
	if _, err := buildGeoNames([]string{layer}, "", out, false, nil, nil, nil, pingIndex(t, dir), nil, "", true, "", "", "", ping, ""); err != nil {
		t.Fatal(err)
	}
	checkCities(t, out, map[string]string{"10.2.50.9": "GB/London/86-country-main"})
}

func TestPingCountryHeader(t *testing.T) {
	dir := t.TempDir()
	index := pingIndex(t, dir)
	open := func(body string) (*pingReader, error) {
		return openPing(writeFile(t, filepath.Join(dir, "h.csv.gz"), body), index)
	}
	for _, bad := range []string{
		"GB/London,FR/Paris,DE/Frankfurt am Main\n", // no prefix
		"prefix,GB/London,FR/Paris\n",               // two sites
		"prefix,GB/London,FR/Paris,GB/Nowhere,XX/London,Frankfurt\n",
		"",
	} {
		if p, err := open(bad); err == nil {
			p.Close()
			t.Errorf("header %q accepted", bad)
		}
	}
	// Unplaced, malformed, unknown country and duplicate columns are ignored and counted.
	p, err := open("prefix,GB/London,FR/Paris,DE/Frankfurt am Main,GB/Nowhere,Dallas,XX/London,gb/London,US/Dallas,/x,\n")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if len(p.sites) != 4 || p.ignored != 6 {
		t.Errorf("%d sites, %d ignored", len(p.sites), p.ignored)
	}
}

func TestPingCountryRows(t *testing.T) {
	dir := t.TempDir()
	index := pingIndex(t, dir)
	path := writeFile(t, filepath.Join(dir, "r.csv.gz"), pingHeader+
		"10.2.1.0/24,1,4,7\n"+ // good
		"10.2.2.0/24,1,4\n"+ // wrong field count
		"10.2.3.0/23,1,4,7\n"+ // not a /24
		"10.2.4.1/24,1,4,7\n"+ // host bits
		"2001:db8::/24,1,4,7\n"+ // IPv6
		"bad,1,4,7\n"+
		"10.2.5.0/24,1,x,7,\n"+ // wrong field count again
		"10.2.6.0/24,1,x,7\n"+ // a bad cell: two usable cells, skipped
		"10.2.7.0/24,1,0,7\n"+ // zero is not usable
		"10.2.8.0/24,1,-4,7\n"+
		"10.2.9.0/24,1,2001,7\n"+ // above 2000 ms
		"10.2.10.0/24,1,NaN,7\n"+
		"10.2.11.0/24,1,Inf,7\n"+
		"10.2.13.0/24,2.5,4,7,\n"+
		"10.2.14.0/24,2000,4,7\n"+ // 2000 is in
		"10.2.12.0/24,\"1,4,7\n") // a csv parse error (an open quote), last: it swallows the rest
	p, err := openPing(path, index)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rows := 0
	for {
		_, disks, ok, err := p.next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		rows++
		if len(disks) != 3 || disks[0].theta > disks[1].theta || disks[1].theta > disks[2].theta {
			t.Errorf("disks = %v", disks)
		}
	}
	if rows != 2 || p.skipped < 11 {
		t.Errorf("%d rows, %d skipped", rows, p.skipped)
	}
}

// A file whose site columns GeoNames does not place (for example after a rename) skips the
// check without an error, so it never fails the build.
func TestPingCountryFewSitesSkips(t *testing.T) {
	dir := t.TempDir()
	index := pingIndex(t, dir)
	tree, err := newWriter("test", "test")
	if err != nil {
		t.Fatal(err)
	}
	path := writeFile(t, filepath.Join(dir, "f.csv.gz"), "prefix,GB/London,GB/Nowhere,FR/Nowhere\n10.2.1.0/24,1,2,3\n")
	counts, err := applyPingCountry(tree, path, index, false, func(place, *geoLayerStats) int { return 0 }, func(int) mmdbtype.Map { return nil })
	if err != nil || !counts.skippedFile || counts.rows != 0 {
		t.Errorf("err %v, counts %+v", err, counts)
	}
	// A file without a prefix column is still an error.
	path = writeFile(t, filepath.Join(dir, "g.csv.gz"), "GB/London,FR/Paris,DE/Frankfurt am Main\n")
	if _, err := applyPingCountry(tree, path, index, false, func(place, *geoLayerStats) int { return 0 }, func(int) mmdbtype.Map { return nil }); err == nil {
		t.Error("no prefix column accepted")
	}
}

// pingTree makes a tree of /24s with one country each and runs the check on it.
func pingTree(t *testing.T, parts map[string]string, ping string, debug bool) (pingCounts, string) {
	t.Helper()
	dir := t.TempDir()
	index := pingIndex(t, dir)
	tree, err := newWriter("test", "test")
	if err != nil {
		t.Fatal(err)
	}
	for p, cc := range parts {
		if err := tree.Insert(ipNet(netip.MustParsePrefix(p)), mmdbtype.Map{"country_code": mmdbtype.String(cc), "city_name": mmdbtype.String("X")}); err != nil {
			t.Fatal(err)
		}
	}
	var recs []mmdbtype.Map
	recordFor := func(pl place, _ *geoLayerStats) int {
		recs = append(recs, mmdbtype.Map{"country_code": mmdbtype.String(pl.code)})
		return len(recs) - 1
	}
	path := writeFile(t, filepath.Join(dir, "p.csv.gz"), pingHeader+ping)
	counts, err := applyPingCountry(tree, path, index, debug, recordFor, func(i int) mmdbtype.Map { return recs[i] })
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "o.mmdb")
	if err := writeAtomic(tree, out); err != nil {
		t.Fatal(err)
	}
	return counts, out
}

// A /24 with two parts of one country and one of another: the check is made once per country
// and the counts are of rows, /24s and parts.
func TestPingCountryCounts(t *testing.T) {
	counts, out := pingTree(t, map[string]string{
		"10.2.1.0/26":   "US",
		"10.2.1.128/25": "US",
		"10.2.1.64/26":  "GB",
		"10.2.2.0/24":   "GB", // possible
		"10.2.3.0/24":   "US",
		"10.2.3.128/25": "FR",
		"10.2.200.0/24": "US", // not in the file
	}, "10.2.1.0/24,1,4,7\n10.2.2.0/24,1,4,7\nbad,1,4,7\n10.2.3.0/24,1,,7\n", false)
	if counts.rows != 2 || counts.skipped != 2 || counts.changed24 != 1 || counts.partsChanged != 2 {
		t.Errorf("counts = %+v", counts)
	}
	checkCities(t, out, map[string]string{
		"10.2.1.9":   "GB/<nil>/<nil>",
		"10.2.1.70":  "GB/X/<nil>",
		"10.2.1.200": "GB/<nil>/<nil>",
		"10.2.2.9":   "GB/X/<nil>",
		"10.2.3.9":   "US/X/<nil>",
		"10.2.200.9": "US/X/<nil>",
	})
	want := "ping-country: 2 rows, 2 skipped, 2 /24s, 1 /24s changed, 2 parts changed"
	if counts.String() != want {
		t.Errorf("line = %q", counts.String())
	}
	if !regexp.MustCompile(`^ping-country: \d+ rows, \d+ skipped, \d+ /24s, \d+ /24s changed, \d+ parts changed$`).MatchString(counts.String()) {
		t.Errorf("line %q does not match", counts.String())
	}
}

// A brute-force check of the fast search over random places, sites and times, and its speed.
func TestPingCheckerRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const countries, perCountry, nSites = 200, 750, 27
	index := geoNames{}
	var codes []string
	id := uint32(0)
	type pl struct {
		cc       string
		lat, lon float64
	}
	var all []pl
	for c := 0; c < countries; c++ {
		cc := string(rune('A'+c/26)) + string(rune('A'+c%26))
		codes = append(codes, cc)
		clat, clon := rng.Float64()*140-70, rng.Float64()*360-180
		for i := 0; i < perCountry; i++ {
			id++
			lat := math.Max(-89, math.Min(89, clat+rng.NormFloat64()*3))
			lon := clon + rng.NormFloat64()*3
			if lon > 180 {
				lon -= 360
			}
			if lon < -180 {
				lon += 360
			}
			index[fmt.Sprintf("%s|p%d", cc, id)] = gnPlace{lat: lat, lon: lon, pop: int64(rng.Intn(1000000)), id: id}
			all = append(all, pl{cc, lat, lon})
		}
	}
	var sites []pingSite
	for i := 0; i < nSites; i++ {
		p := all[rng.Intn(len(all))]
		x, y, z := unitVec(p.lat, p.lon)
		sites = append(sites, pingSite{p.lat, p.lon, x, y, z})
	}
	w := newPingWorld(index, sites)
	checker := &pingChecker{w: w}
	makeDisks := func() ([]pingDisk, []float64) {
		truth := all[rng.Intn(len(all))]
		var disks []pingDisk
		var radius []float64
		for s := range sites {
			if rng.Intn(10) == 0 {
				continue
			}
			d := haversineKm(truth.lat, truth.lon, sites[s].lat, sites[s].lon)
			rtt := d/100*(1+rng.Float64()) + 1 + rng.Float64()*3
			radius = append(radius, 0)
			disks = append(disks, newDisk(int32(s), rtt))
			radius[len(radius)-1] = rtt*pingKmPerMs + pingMarginKm
		}
		sortDisks(disks)
		return disks, radius
	}
	// Brute force with haversine over the same cells.
	naive := func(x string, disks []pingDisk) string {
		in := func(c pingCell) bool {
			for _, d := range disks {
				s := sites[d.site]
				// the cell's point from its unit vector
				lat := math.Asin(c.z) * 180 / math.Pi
				lon := math.Atan2(c.y, c.x) * 180 / math.Pi
				if haversineKm(s.lat, s.lon, lat, lon) > d.theta*pingEarthKm {
					return false
				}
			}
			return true
		}
		sums := map[string]float64{}
		total := 0.0
		for _, c := range w.cells {
			if in(c) {
				sums[w.codes[c.cc]] += c.w
				total += c.w
				if w.codes[c.cc] == x {
					return ""
				}
			}
		}
		top := ""
		for _, cc := range w.codes {
			if v := sums[cc]; v > 0 && (top == "" || v > sums[top]) {
				top = cc
			}
		}
		if top == "" || sums[top] < pingMinShare*total {
			return ""
		}
		return top
	}
	for i := 0; i < 600; i++ {
		disks, _ := makeDisks()
		checker.reset(disks)
		x := codes[rng.Intn(len(codes))]
		if got, want := checker.correct(x), naive(x, disks); got != want {
			t.Fatalf("row %d country %s: got %q, brute force %q", i, x, got, want)
		}
	}
	// Speed: 20,000 rows (200,000 took 27 s in the check, run by hand) over 150,000 places, half of them with a wrong country.
	const rows = 20000
	changed := 0
	var spent time.Duration
	for i := 0; i < rows; i++ {
		disks, _ := makeDisks()
		x := codes[rng.Intn(len(codes))]
		start := time.Now()
		checker.reset(disks)
		if checker.correct(x) != "" {
			changed++
		}
		spent += time.Since(start)
	}
	t.Logf("%d rows, %d cells, %d changed: %v in the check", rows, len(w.cells), changed, spent)
}
