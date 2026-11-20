package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

// guessBuild builds an MMDB from layer rows and asn-prefixes rows (nil: no -asn-main),
// with or without -fallback.
func guessBuild(t *testing.T, dir, layerRows string, asnRows *string, index geoNames, fallback bool) (string, []geoLayerStats) {
	t.Helper()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), "prefix,country,region,city,source\n"+layerRows)
	asn := ""
	if asnRows != nil {
		asn = writeFile(t, filepath.Join(dir, "asn-prefixes.csv.gz"), "asn,prefix\n"+*asnRows)
	}
	out := filepath.Join(dir, "out.mmdb")
	stats, err := buildGeoGuesses([]string{layer}, "", out, false, nil, nil, nil, index, asn, fallback, "")
	if err != nil {
		t.Fatal(err)
	}
	return out, stats
}

// checkCities opens the MMDB and compares "country/city/source" per address.
func checkCities(t *testing.T, out string, want map[string]string) {
	t.Helper()
	r, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for ip, w := range want {
		if got := cityOf(t, r, ip); got != w {
			t.Errorf("%s = %s, want %s", ip, got, w)
		}
	}
}

func statsOf(t *testing.T, stats []geoLayerStats, name string) geoLayerStats {
	t.Helper()
	for _, s := range stats {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("no stats for %s in %v", name, stats)
	return geoLayerStats{}
}

// upTo254 is the rows of 10.<second>.<third>.0 to .254 (not .255) with one city, as eight
// networks that merge into one piece of 255 addresses.
func upTo254(second, third int, city string) string {
	var b strings.Builder
	for _, n := range []string{"0/25", "128/26", "192/27", "224/28", "240/29", "248/30", "252/31", "254/32"} {
		fmt.Fprintf(&b, "10.%d.%d.%s,DE,,%s,x\n", second, third, n, city)
	}
	return b.String()
}

// fillerDE is 40 /24s of DE in four cities, none of them in an ASN: with the few /24s
// a test adds, no city holds 30% of DE, so the country rung is silent.
func fillerDE() string {
	return rows24(9, 0, 10, "DE", "Hamburg") + rows24(9, 10, 10, "DE", "Cologne") +
		rows24(9, 20, 10, "DE", "Essen") + rows24(9, 30, 10, "DE", "Bonn")
}

func TestASNLikely(t *testing.T) {
	few := func() string {
		var b strings.Builder
		for i := 0; i < 19; i++ {
			fmt.Fprintf(&b, "10.0.0.%d/32,DE,,Berlin,x\n", i)
		}
		return b.String() + "10.0.0.19/32,DE,,Munich,x\n"
	}()
	for _, tc := range []struct {
		name  string
		layer string
		want  string
	}{
		{"75% gets 82", rows24(0, 0, 75, "DE", "Berlin") + rows24(0, 75, 25, "DE", "Munich"), "DE/Berlin/82-asn-likely"},
		// 65% is below 70%: not 82; far from the evidence (other /20), so the country rung gives it.
		{"65% is not 82", rows24(0, 0, 65, "DE", "Berlin") + rows24(0, 65, 35, "DE", "Munich"), "DE/Berlin/86-country-main"},
		{"95% with 256 addresses is still 80", rows24(0, 0, 95, "DE", "Berlin") + rows24(0, 95, 5, "DE", "Munich"), "DE/Berlin/80-asn-main"},
		{"95% with fewer than 256 addresses is 82", few, "DE/Berlin/82-asn-likely"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, stats := guessBuild(t, t.TempDir(), tc.layer+"10.0.200.0/24,DE,,,x\n", str("100,10.0.0.0/16\n"), nil, true)
			checkCities(t, out, map[string]string{"10.0.200.7": tc.want})
			want80, want82 := 0, 0
			switch {
			case strings.HasSuffix(tc.want, "80-asn-main"):
				want80 = 1
			case strings.HasSuffix(tc.want, "82-asn-likely"):
				want82 = 1
			}
			if s := statsOf(t, stats, asnMainSource); s.rows != want80 {
				t.Errorf("80 rows = %d", s.rows)
			}
			if s := statsOf(t, stats, asnLikelySource); s.rows != want82 {
				t.Errorf("82 rows = %d", s.rows)
			}
		})
	}
}

func TestASNNear(t *testing.T) {
	asn := "100,10.0.0.0/16\n"
	// A group split 50/50 (no 80, no 82); country DE below 30% so the country rung is silent.
	base := rows24(0, 0, 1, "DE", "Berlin") + rows24(0, 32, 1, "DE", "Munich") + fillerDE()
	// DE has no main city, so a chunk in another /20 but the same /16 gets rung 88.
	t.Run("same /20 gets the nearest piece, another /20 only rung 88", func(t *testing.T) {
		out, stats := guessBuild(t, t.TempDir(), base+"10.0.2.0/24,DE,,,x\n10.0.20.0/24,DE,,,x\n", &asn, nil, true)
		checkCities(t, out, map[string]string{
			"10.0.2.9":  "DE/Berlin/84-asn-near",
			"10.0.20.9": "DE/Munich/88-asn-far",
		})
		if s := statsOf(t, stats, asnNearSource); s.rows != 1 || s.note != "asn-near: 256 addresses filled" {
			t.Errorf("84 stats = %+v", s)
		}
	})
	t.Run("the piece after the chunk counts too", func(t *testing.T) {
		out, _ := guessBuild(t, t.TempDir(), rows24(0, 4, 1, "DE", "Berlin")+rows24(0, 32, 1, "DE", "Munich")+fillerDE()+"10.0.2.0/24,DE,,,x\n", &asn, nil, true)
		checkCities(t, out, map[string]string{"10.0.2.9": "DE/Berlin/84-asn-near"})
	})
	// The chunk 10.0.5.0/24 has its middle at .127: the piece before ends at 10.0.4.254 and
	// the one after starts at 10.0.6.0, both 129 away. The bigger piece wins, then the one before.
	munich := upTo254(0, 4, "Munich") + "10.0.4.255/32,FR,,,x\n"
	for _, tc := range []struct {
		name  string
		after string // the piece after, Berlin
		want  string
	}{
		{"bigger piece after wins", rows24(0, 6, 1, "DE", "Berlin"), "DE/Berlin/84-asn-near"},
		{"bigger piece before wins", "10.0.6.0/25,DE,,Berlin,x\n", "DE/Munich/84-asn-near"},
		{"equal pieces: the one before wins", upTo254(0, 6, "Berlin") + "10.0.6.255/32,FR,,,x\n", "DE/Munich/84-asn-near"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layer := munich + tc.after + "10.0.5.0/24,DE,,,x\n" + fillerDE()
			out, _ := guessBuild(t, t.TempDir(), layer, &asn, nil, true)
			checkCities(t, out, map[string]string{"10.0.5.9": tc.want})
		})
	}
	t.Run("a leaf bigger than a /24 is decided per /24", func(t *testing.T) {
		layer := rows24(0, 7, 1, "DE", "Berlin") + rows24(0, 10, 1, "DE", "Munich") + rows24(0, 40, 1, "DE", "Hamburg") +
			"10.0.8.0/23,DE,,,x\n" + fillerDE()
		out, stats := guessBuild(t, t.TempDir(), layer, &asn, nil, true)
		checkCities(t, out, map[string]string{
			"10.0.8.200": "DE/Berlin/84-asn-near",
			"10.0.9.5":   "DE/Munich/84-asn-near",
		})
		if s := statsOf(t, stats, asnNearSource); s.rows != 2 || s.prefixes != 2 {
			t.Errorf("84 stats = %+v", s)
		}
	})
}

func TestCountryMain(t *testing.T) {
	asn := "100,10.5.0.0/16\n"
	t.Run("40% fills everything left, also with no ASN", func(t *testing.T) {
		layer := rows24(1, 0, 40, "DE", "Berlin") + rows24(2, 0, 30, "DE", "Munich") + rows24(3, 0, 30, "DE", "Hamburg") +
			"10.4.0.0/24,DE,,,x\n" + // no ASN
			"10.5.0.0/24,DE,,,x\n" + // an ASN with no city of its own
			"10.6.0.0/24,ES,,,x\n" + // another country, no evidence: stays
			"10.7.0.0/24,DE,,Leipzig,x\n" // has a city: stays
		out, stats := guessBuild(t, t.TempDir(), layer, &asn, nil, true)
		checkCities(t, out, map[string]string{
			"10.4.0.9": "DE/Berlin/86-country-main",
			"10.5.0.9": "DE/Berlin/86-country-main",
			"10.6.0.9": "ES/<nil>/<nil>",
			"10.7.0.9": "DE/Leipzig/layer",
			"10.2.0.9": "DE/Munich/layer",
		})
		s := statsOf(t, stats, countryMainSource)
		if s.rows != 2 || s.prefixes != 2 || s.note != "country-main: 1 countries qualified of 1, 512 addresses filled" {
			t.Errorf("86 stats = %+v", s)
		}
	})
	t.Run("25% fills nothing", func(t *testing.T) {
		layer := rows24(1, 0, 25, "DE", "Berlin") + rows24(2, 0, 25, "DE", "Munich") + rows24(3, 0, 25, "DE", "Hamburg") +
			rows24(4, 0, 25, "DE", "Cologne") + "10.8.0.0/24,DE,,,x\n"
		out, stats := guessBuild(t, t.TempDir(), layer, nil, nil, true)
		checkCities(t, out, map[string]string{"10.8.0.9": "DE/<nil>/<nil>"})
		if s := statsOf(t, stats, countryMainSource); s.rows != 0 || s.note != "country-main: 0 countries qualified of 1, 0 addresses filled" {
			t.Errorf("86 stats = %+v", s)
		}
	})
}

// ladderLayer has one cityless /24 for each rung; the country's main city is Berlin.
func ladderLayer() (layer string, asn string) {
	layer = rows24(0, 0, 90, "DE", "Berlin") + rows24(0, 90, 10, "DE", "Munich") + "10.0.150.0/24,DE,,,x\n" + // 80
		rows24(1, 0, 3, "DE", "Berlin") + rows24(1, 3, 1, "DE", "Munich") + "10.1.100.0/24,DE,,,x\n" + // 82
		rows24(2, 0, 1, "DE", "Berlin") + rows24(2, 32, 1, "DE", "Munich") + "10.2.1.0/24,DE,,,x\n" + // 84
		"10.3.0.0/24,DE,,,x\n" // 86: no ASN
	return layer, "100,10.0.0.0/16\n200,10.1.0.0/16\n300,10.2.0.0/16\n"
}

// The first rung that applies wins, and a later rung does not count the address.
func TestFallbackLadder(t *testing.T) {
	layer, asn := ladderLayer()
	dir := t.TempDir()
	out, stats := guessBuild(t, dir, layer, &asn, nil, true)
	checkCities(t, out, map[string]string{
		"10.0.150.1": "DE/Berlin/80-asn-main",
		"10.1.100.1": "DE/Berlin/82-asn-likely",
		"10.2.1.1":   "DE/Berlin/84-asn-near",
		"10.3.0.1":   "DE/Berlin/86-country-main",
	})
	// Every rung filled exactly its own /24: a later rung does not count an earlier one's.
	for _, name := range []string{asnMainSource, asnLikelySource, asnNearSource, countryMainSource} {
		if s := statsOf(t, stats, name); s.rows != 1 || s.prefixes != 1 || !strings.HasSuffix(s.note, "256 addresses filled") {
			t.Errorf("%s stats = %+v", name, s)
		}
	}
	var names []string
	for _, s := range stats[1:] {
		names = append(names, s.name)
	}
	if got := strings.Join(names, " "); got != "80-asn-main 82-asn-likely 84-asn-near 86-country-main 88-asn-far 89-asn-anywhere" {
		t.Errorf("rungs = %s", got)
	}
	if s := statsOf(t, stats, asnLikelySource); s.note != "asn-likely: 1 groups qualified of 3, 256 addresses filled" {
		t.Errorf("82 note = %q", s.note)
	}
	if s := statsOf(t, stats, countryMainSource); s.note != "country-main: 1 countries qualified of 1, 256 addresses filled" {
		t.Errorf("86 note = %q", s.note)
	}

	// The guesses are no evidence: gaps lists their /24s, lookup -in writes nothing for them.
	gapsOut := filepath.Join(dir, "gaps.txt")
	if _, err := writeGaps(out, gapsOut); err != nil {
		t.Fatal(err)
	}
	wantGaps := "10.0.150.0/24 1 DE\n10.1.100.0/24 1 DE\n10.2.1.0/24 1 DE\n10.3.0.0/24 1 DE\n"
	if data, _ := os.ReadFile(gapsOut); string(data) != wantGaps {
		t.Errorf("gaps = %q", data)
	}
	in := writeFile(t, filepath.Join(dir, "ips.txt"), "10.0.150.9\n10.1.100.9\n10.2.1.9\n10.3.0.9\n10.0.5.5\n")
	citiesOut := filepath.Join(dir, "cities.tsv")
	if _, err := writeCities(out, in, citiesOut); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(citiesOut); string(data) != "10.0.5.5\tDE\tBerlin\n" {
		t.Errorf("cities = %q", data)
	}
	for _, src := range guessSources {
		if !isGuessSource(src) || !isWeakCitySource(src) {
			t.Errorf("%s is not a guess or not weak", src)
		}
	}
	if !isWeakCitySource("37-whois-text") || isGuessSource("37-whois-text") {
		t.Error("37-whois-text must be weak and not a guess (lookup -in keeps its city)")
	}
	if !isWeakCitySource("39-github-geofeeds") || isGuessSource("39-github-geofeeds") {
		t.Error("39-github-geofeeds must be weak (the gap list keeps its /24s) and not a guess")
	}
	if !isWeakCitySource("37-whois-arin") || isGuessSource("37-whois-arin") {
		t.Error("37-whois-arin must be weak (the gap list keeps its /24s) and not a guess")
	}
	if !isWeakCitySource("35-speedtest-site") || isGuessSource("35-speedtest-site") {
		t.Error("35-speedtest-site must be weak (the gap list keeps its /24s) and not a guess")
	}
	if !isWeakCitySource("25-bgp") || !isWeakCitySource("36-mlab") || isGuessSource("25-bgp") || isWeakCitySource("70-geofeeds") {
		t.Error("source lists")
	}
}

// A city written by rung 80 does not count for rung 86: with the guess counted, Berlin
// (35 + 32 /24s) would beat Munich (40) as the country's main city.
func TestFallbackSnapshot(t *testing.T) {
	layer := rows24(5, 0, 40, "DE", "Munich") + rows24(6, 0, 34, "DE", "Berlin") + rows24(7, 0, 25, "DE", "Hamburg") +
		"10.0.0.0/24,DE,,Berlin,x\n" + // the one Berlin /24 of the ASN: group 100% Berlin, 256 addresses
		"10.0.32.0/19,DE,,,x\n" + // 32 /24s that rung 80 gives to Berlin
		"10.9.0.0/24,DE,,,x\n" // no ASN: rung 86
	out, stats := guessBuild(t, t.TempDir(), layer, str("100,10.0.0.0/16\n"), nil, true)
	checkCities(t, out, map[string]string{
		"10.0.40.1": "DE/Berlin/80-asn-main",
		"10.9.0.1":  "DE/Munich/86-country-main",
	})
	if s := statsOf(t, stats, countryMainSource); s.note != "country-main: 1 countries qualified of 1, 256 addresses filled" {
		t.Errorf("86 note = %q", s.note)
	}
}

// With a GeoNames index only cities it places count, for 84 and 86 as for 80.
func TestFallbackIgnoresUnplacedCities(t *testing.T) {
	dir := t.TempDir()
	geonames := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		geoNamesLine("2", "Berlin", "", "52.52437", "13.41053", "DE", "3426354")+
			geoNamesLine("3", "Munich", "", "48.13743", "11.57549", "DE", "1260391"))
	index, err := loadGeoNames(geonames)
	if err != nil {
		t.Fatal(err)
	}
	asn := "100,10.0.0.0/16\n"
	t.Run("84", func(t *testing.T) {
		// Nowhereville (not in GeoNames) lies right before the gap, Berlin a bit further, same /20.
		layer := rows24(0, 1, 1, "DE", "Nowhereville") + "10.0.2.0/24,DE,,,x\n" + rows24(0, 4, 1, "DE", "Berlin") + rows24(0, 40, 1, "DE", "Munich") + fillerDE()
		for _, tc := range []struct {
			index geoNames
			want  string
		}{{index, "DE/Berlin/84-asn-near"}, {nil, "DE/Nowhereville/84-asn-near"}} {
			out, _ := guessBuild(t, t.TempDir(), layer, &asn, tc.index, true)
			checkCities(t, out, map[string]string{"10.0.2.9": tc.want, "10.0.1.9": "DE/Nowhereville/layer"})
		}
	})
	t.Run("86", func(t *testing.T) {
		layer := rows24(1, 0, 60, "DE", "Nowhereville") + rows24(2, 0, 20, "DE", "Berlin") + rows24(3, 0, 20, "DE", "Munich") + "10.4.0.0/24,DE,,,x\n"
		for _, tc := range []struct {
			index geoNames
			want  string
		}{{index, "DE/Berlin/86-country-main"}, {nil, "DE/Nowhereville/86-country-main"}} {
			out, _ := guessBuild(t, t.TempDir(), layer, nil, tc.index, true)
			checkCities(t, out, map[string]string{"10.4.0.9": tc.want, "10.1.0.9": "DE/Nowhereville/layer"})
		}
	})
	t.Run("coordinates", func(t *testing.T) {
		out, _ := guessBuild(t, t.TempDir(), rows24(1, 0, 10, "DE", "Berlin")+"10.4.0.0/24,DE,,,x\n", nil, index, true)
		r, _ := maxminddb.Open(out)
		defer r.Close()
		if m := record(t, r, "10.4.0.9"); m["latitude"] != 52.5244 || m["city_source"] != countryMainSource {
			t.Errorf("record = %v", m)
		}
	})
}

// -fallback without -asn-main runs only the country rung; without -fallback nothing of the
// new rungs is there and the output is what buildGeo gives.
func TestFallbackFlags(t *testing.T) {
	layer, asn := ladderLayer()
	out, stats := guessBuild(t, t.TempDir(), layer, nil, nil, true)
	var names []string
	for _, s := range stats {
		names = append(names, s.name)
	}
	if got := strings.Join(names, " "); got != "layer.csv.gz 86-country-main" {
		t.Errorf("rungs without -asn-main = %s", got)
	}
	checkCities(t, out, map[string]string{
		"10.0.150.1": "DE/Berlin/86-country-main", // the ASN is not known: the country rung only
		"10.3.0.1":   "DE/Berlin/86-country-main",
	})

	off, offStats := guessBuild(t, t.TempDir(), layer, &asn, nil, false)
	for _, s := range offStats {
		if s.name == asnLikelySource || s.name == asnNearSource || s.name == countryMainSource || s.name == asnFarSource || s.name == asnAnywhereSource {
			t.Errorf("rung %s ran without -fallback", s.name)
		}
	}
	for _, src := range []string{asnLikelySource, asnNearSource, countryMainSource, asnFarSource, asnAnywhereSource} {
		if strings.Contains(dump(t, off), src) {
			t.Errorf("%s in a build without -fallback", src)
		}
	}
	old := filepath.Join(t.TempDir(), "old.mmdb")
	layerFile := writeFile(t, filepath.Join(t.TempDir(), "layer.csv.gz"), "prefix,country,region,city,source\n"+layer)
	asnFile := writeFile(t, filepath.Join(t.TempDir(), "asn-prefixes.csv.gz"), "asn,prefix\n"+asn)
	if _, err := buildGeo([]string{layerFile}, "", old, false, nil, nil, nil, nil, asnFile); err != nil {
		t.Fatal(err)
	}
	if dump(t, old) != dump(t, off) {
		t.Error("a build without -fallback differs from buildGeo")
	}
}

func TestFallbackDeterministic(t *testing.T) {
	layer, asn := ladderLayer()
	a, _ := guessBuild(t, t.TempDir(), layer, &asn, nil, true)
	b, _ := guessBuild(t, t.TempDir(), layer, &asn, nil, true)
	if dump(t, a) != dump(t, b) {
		t.Error("two builds differ")
	}
	da, _ := os.ReadFile(a)
	db, _ := os.ReadFile(b)
	if len(da) != len(db) {
		t.Errorf("sizes %d, %d", len(da), len(db))
	}
}

func TestNearPiece(t *testing.T) {
	ps := []piece{{0, 99, 1}, {200, 299, 2}, {5000, 5000, 3}}
	for _, tc := range []struct {
		x    uint32
		want uint32
		ok   bool
	}{
		{120, 1, true},   // the piece before is nearer (21 against 80)
		{190, 2, true},   // the piece after is nearer
		{150, 2, true},   // 51 against 50: after
		{4999, 3, true},  // the piece at 5000 is in the same block of 4096, the one before is not near
		{3000, 0, false}, // nearest is the piece at 5000, in the next block
	} {
		if r, ok := nearPiece(ps, tc.x, asnNearSameBlock); ok != tc.ok || ok && r != tc.want {
			t.Errorf("x=%d: %d %v, want %d %v", tc.x, r, ok, tc.want, tc.ok)
		}
	}
	if _, ok := nearPiece(nil, 5, asnNearSameBlock); ok {
		t.Error("no pieces")
	}
	// The same pieces with the block of rung 88 (2^16): the piece at 5000 is near x=3000.
	if r, ok := nearPiece(ps, 3000, asnFarSameBlock); !ok || r != 3 {
		t.Errorf("block 16: %d %v", r, ok)
	}
	if _, ok := nearPiece(ps, 70000, asnFarSameBlock); ok {
		t.Error("block 16: the nearest piece is in another block")
	}
	// Block 32 is no limit: a shift of a uint32 by 32 gives 0, so every piece matches.
	if r, ok := nearPiece(ps, 70000, asnAnywhereBlock); !ok || r != 3 {
		t.Errorf("block 32: %d %v", r, ok)
	}
	far := []piece{{20 << 24, 20<<24 + 255, 7}}
	if r, ok := nearPiece(far, 200<<24, asnAnywhereBlock); !ok || r != 7 {
		t.Errorf("block 32, far away: %d %v", r, ok)
	}
	if r, ok := nearPiece(far, 4, asnAnywhereBlock); !ok || r != 7 {
		t.Errorf("block 32, piece after: %d %v", r, ok)
	}
	if _, ok := nearPiece(nil, 5, asnAnywhereBlock); ok {
		t.Error("block 32: no pieces")
	}
}

// With a GeoNames index a city is its place: two spellings and an alternate name of one place
// count together, for the country guess and for the ASN guess, and the most common spelling is
// written. Without an index they are separate cities, as before.
func TestGuessesCountAPlaceOnce(t *testing.T) {
	dir := t.TempDir()
	geonames := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		geoNamesLine("1", "São Paulo", "Sao Paulo,Sampa", "-23.5475", "-46.63611", "BR", "10021295")+
			geoNamesLine("2", "Rio de Janeiro", "", "-22.90642", "-43.18223", "BR", "6023699")+
			geoNamesLine("3", "Curitiba", "", "-25.42778", "-49.27306", "BR", "1718421")+
			geoNamesLine("4", "Salvador", "", "-12.97563", "-38.49096", "BR", "2711840"))
	index, err := loadGeoNames(geonames)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("country", func(t *testing.T) {
		// 100 /24s with a city: São Paulo 20, Sao Paulo 15, Rio 28, Salvador 20, Curitiba 17.
		// One place: 35%, the main city. Two cities: Rio leads with 28%, under the 30% bar.
		layer := rows24(1, 0, 20, "BR", "São Paulo") + rows24(2, 0, 15, "BR", "Sao Paulo") + rows24(3, 0, 28, "BR", "Rio de Janeiro") +
			rows24(4, 0, 20, "BR", "Salvador") + rows24(5, 0, 17, "BR", "Curitiba") + "10.9.0.0/24,BR,,,x\n"
		for _, tc := range []struct {
			index geoNames
			want  string
		}{{index, "BR/São Paulo/86-country-main"}, {nil, "BR/<nil>/<nil>"}} {
			out, _ := guessBuild(t, t.TempDir(), layer, nil, tc.index, true)
			checkCities(t, out, map[string]string{"10.9.0.9": tc.want})
		}
	})
	t.Run("asn", func(t *testing.T) {
		// 12 /24s of AS100 with a city: São Paulo 6, Sao Paulo 4, Sampa 1, Rio 1: 11 of 12 in one place.
		layer := rows24(0, 0, 6, "BR", "São Paulo") + rows24(0, 6, 4, "BR", "Sao Paulo") + rows24(0, 10, 1, "BR", "Sampa") +
			rows24(0, 11, 1, "BR", "Rio de Janeiro") + "10.0.20.0/24,BR,,,x\n"
		asn := "100,10.0.0.0/16\n"
		for _, tc := range []struct {
			index geoNames
			want  string
		}{{index, "BR/São Paulo/80-asn-main"}, {nil, "BR/<nil>/<nil>"}} {
			out, _ := guessBuild(t, t.TempDir(), layer, &asn, tc.index, false)
			checkCities(t, out, map[string]string{"10.0.20.9": tc.want})
		}
	})
	t.Run("ties", func(t *testing.T) {
		// Two places with the same count: the smaller name wins, whatever their coordinates
		// (Curitiba before Salvador); two spellings with the same count: the smaller spelling.
		g := &asnGroup{cities: map[string]*cityCount{}}
		for _, city := range []string{"Salvador", "Curitiba"} {
			pl, _ := index.place("BR", city)
			g.add(placeKey(pl), normCity(city), city, 256)
		}
		if main, ok := g.choose(0.5, 0); !ok || main != "Curitiba" {
			t.Errorf("tie between places: %q %v", main, ok)
		}
		g = &asnGroup{cities: map[string]*cityCount{}}
		for _, city := range []string{"São Paulo", "Sao Paulo"} {
			pl, _ := index.place("BR", city)
			g.add(placeKey(pl), normCity(city), city, 256)
		}
		if main, ok := g.choose(0.9, 0); !ok || main != "Sao Paulo" {
			t.Errorf("tie between spellings: %q %v", main, ok)
		}
	})
}

// farBase is ASN 100 = 10.0.0.0/14 and filler in DE with no city at 30%: DE has no main
// city. The ASN's own pieces are added by each test.
func farBase() (layer string, asn *string) {
	return fillerDE(), str("100,10.0.0.0/14\n")
}

func TestASNFar(t *testing.T) {
	layer, asn := farBase()
	layer += "10.0.0.0/24,DE,,Berlin,x\n10.0.200.0/24,DE,,Munich,x\n" + // the pieces
		"10.0.2.0/24,DE,,,x\n" + // same /20 as Berlin: 84
		"10.0.20.0/24,DE,,,x\n" + // Berlin is the nearest, same /16 but not same /20: 88
		"10.1.5.0/24,DE,,,x\n" + // the nearest piece is in another /16: 89
		"10.8.0.0/24,DE,,,x\n" + // no ASN: nothing
		"10.0.30.0/24,FR,,,x\n" // a group with no pieces: nothing
	dir := t.TempDir()
	out, stats := guessBuild(t, dir, layer, asn, nil, true)
	checkCities(t, out, map[string]string{
		"10.0.0.9":  "DE/Berlin/layer",
		"10.0.2.9":  "DE/Berlin/84-asn-near",
		"10.0.20.9": "DE/Berlin/88-asn-far",
		"10.1.5.9":  "DE/Munich/89-asn-anywhere",
		"10.8.0.9":  "DE/<nil>/<nil>",
		"10.0.30.9": "FR/<nil>/<nil>",
	})
	if s := statsOf(t, stats, asnFarSource); s.rows != 1 || s.prefixes != 1 || s.note != "asn-far: 256 addresses filled" {
		t.Errorf("88 stats = %+v", s)
	}
	if s := statsOf(t, stats, asnNearSource); s.note != "asn-near: 256 addresses filled" {
		t.Errorf("84 note = %q", s.note)
	}
	if s := statsOf(t, stats, countryMainSource); s.rows != 0 || s.note != "country-main: 0 countries qualified of 1, 0 addresses filled" {
		t.Errorf("86 stats = %+v", s)
	}
	if s := statsOf(t, stats, asnAnywhereSource); s.rows != 1 || s.prefixes != 1 || s.note != "asn-anywhere: 256 addresses filled" {
		t.Errorf("89 stats = %+v", s)
	}
	if last := stats[len(stats)-1].name; last != asnAnywhereSource {
		t.Errorf("last stats entry = %s", last)
	}
	// The guess is like 84-asn-near for the gap list and lookup -in.
	if !isGuessSource(asnFarSource) || !isWeakCitySource(asnFarSource) {
		t.Error("88-asn-far must be a guess")
	}
	if !isGuessSource("89-asn-anywhere") || !isWeakCitySource("89-asn-anywhere") {
		t.Error("89-asn-anywhere must be a guess")
	}
	gapsOut := filepath.Join(dir, "gaps.txt")
	if _, err := writeGaps(out, gapsOut); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(gapsOut); !strings.Contains(string(data), "10.0.20.0/24 1 DE\n") {
		t.Errorf("gaps = %q", data)
	}
	in := writeFile(t, filepath.Join(dir, "ips.txt"), "10.0.20.9\n10.0.2.9\n10.0.0.9\n10.1.5.9\n")
	citiesOut := filepath.Join(dir, "cities.tsv")
	if _, err := writeCities(out, in, citiesOut); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(citiesOut); string(data) != "10.0.0.9\tDE\tBerlin\n" {
		t.Errorf("cities = %q", data)
	}
}

// The chunk 10.0.100.0/24 has its middle at 10.0.100.127, offset 25727 in the /16.
func TestASNFarNearest(t *testing.T) {
	for _, tc := range []struct {
		name, pieces, want string
	}{
		{"the nearer piece wins (before)", "10.0.88.0/24,DE,,Berlin,x\n10.0.112.0/24,DE,,Munich,x\n", "DE/Berlin/88-asn-far"},
		{"the nearer piece wins (after)", "10.0.80.0/24,DE,,Berlin,x\n10.0.112.0/24,DE,,Munich,x\n", "DE/Munich/88-asn-far"},
		{"equal distance: the bigger piece before", "10.0.88.128/26,DE,,Berlin,x\n10.0.112.63/32,DE,,Munich,x\n", "DE/Berlin/88-asn-far"},
		{"equal distance: the bigger piece after", "10.0.88.190/32,DE,,Berlin,x\n10.0.112.64/26,DE,,Munich,x\n", "DE/Munich/88-asn-far"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layer, asn := farBase()
			// Equal pieces of Berlin and Munich elsewhere keep 80 and 82 away.
			layer += tc.pieces + "10.0.100.0/24,DE,,,x\n" + "10.3.0.0/22,DE,,Berlin,x\n10.3.4.0/22,DE,,Munich,x\n"
			out, _ := guessBuild(t, t.TempDir(), layer, asn, nil, true)
			checkCities(t, out, map[string]string{"10.0.100.9": tc.want})
		})
	}
}

// With a main city in the country, the same layout gets rung 86 and rung 88 does nothing.
func TestASNFarNotWithCountryMain(t *testing.T) {
	layer, asn := farBase()
	layer = rows24(8, 0, 200, "DE", "Hamburg") + layer + "10.0.0.0/24,DE,,Berlin,x\n10.0.200.0/24,DE,,Munich,x\n" +
		"10.0.2.0/24,DE,,,x\n10.0.20.0/24,DE,,,x\n10.1.5.0/24,DE,,,x\n"
	out, stats := guessBuild(t, t.TempDir(), layer, asn, nil, true)
	checkCities(t, out, map[string]string{
		"10.0.2.9":  "DE/Berlin/84-asn-near",
		"10.0.20.9": "DE/Hamburg/86-country-main",
		"10.1.5.9":  "DE/Hamburg/86-country-main",
	})
	if s := statsOf(t, stats, asnFarSource); s.rows != 0 || s.note != "asn-far: 0 addresses filled" {
		t.Errorf("88 stats = %+v", s)
	}
	if strings.Contains(dump(t, out), asnFarSource) {
		t.Error("88-asn-far in a country with a main city")
	}
}

// A chunk filled by rung 88 is not a piece for another chunk, and counts for nothing.
// 10.0.40.0/24 has Munich (at 10.0.60.0) as its nearest real piece; with the filled
// 10.0.20.0/24 counted as a piece, Berlin would be nearer.
func TestASNFarSnapshot(t *testing.T) {
	layer, asn := farBase()
	layer += "10.0.0.0/24,DE,,Berlin,x\n10.0.60.0/24,DE,,Munich,x\n10.0.20.0/24,DE,,,x\n10.0.40.0/24,DE,,,x\n"
	out, stats := guessBuild(t, t.TempDir(), layer, asn, nil, true)
	checkCities(t, out, map[string]string{
		"10.0.20.9": "DE/Berlin/88-asn-far",
		"10.0.40.9": "DE/Munich/88-asn-far",
	})
	if s := statsOf(t, stats, countryMainSource); s.note != "country-main: 0 countries qualified of 1, 0 addresses filled" {
		t.Errorf("86 note = %q", s.note)
	}
}

// Rung 88 needs -fallback and the ASN prefixes.
func TestASNFarFlags(t *testing.T) {
	layer, asn := farBase()
	layer += "10.0.0.0/24,DE,,Berlin,x\n10.0.200.0/24,DE,,Munich,x\n10.0.20.0/24,DE,,,x\n"
	for _, tc := range []struct {
		name     string
		asn      *string
		fallback bool
	}{{"no -fallback", asn, false}, {"no ASN prefixes", nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			out, stats := guessBuild(t, t.TempDir(), layer, tc.asn, nil, tc.fallback)
			for _, s := range stats {
				if s.name == asnFarSource {
					t.Error("rung 88 ran")
				}
			}
			checkCities(t, out, map[string]string{"10.0.20.9": "DE/<nil>/<nil>"})
		})
	}
}

// Rung 89: where the country has no main city and the nearest piece is outside the same
// /16, the chunk gets the nearest piece of its group, however far away.
func TestASNAnywhere(t *testing.T) {
	layer, asn := farBase()
	layer += "10.0.0.0/24,DE,,Berlin,x\n10.0.200.0/24,DE,,Munich,x\n" +
		"10.0.2.0/24,DE,,,x\n" + // same /20: 84
		"10.0.20.0/24,DE,,,x\n" + // same /16: 88
		"10.2.5.0/24,DE,,,x\n" // another /16: 89, the nearest piece is Munich
	out, stats := guessBuild(t, t.TempDir(), layer, asn, nil, true)
	checkCities(t, out, map[string]string{
		"10.0.2.9":  "DE/Berlin/84-asn-near",
		"10.0.20.9": "DE/Berlin/88-asn-far",
		"10.2.5.9":  "DE/Munich/89-asn-anywhere",
	})
	if s := statsOf(t, stats, asnAnywhereSource); s.rows != 1 || s.prefixes != 1 || s.note != "asn-anywhere: 256 addresses filled" {
		t.Errorf("89 stats = %+v", s)
	}
}

// The nearest piece wins, also across a very large distance.
func TestASNAnywhereNearest(t *testing.T) {
	// The chunk 10.2.100.0/24 has its middle at 10.2.100.127.
	for _, tc := range []struct {
		name, pieces, want string
	}{
		{"the nearer piece wins (after)", "10.0.200.0/24,DE,,Berlin,x\n10.3.20.0/24,DE,,Munich,x\n", "DE/Munich/89-asn-anywhere"},
		{"the nearer piece wins (before)", "10.1.250.0/24,DE,,Berlin,x\n10.3.20.0/24,DE,,Munich,x\n", "DE/Berlin/89-asn-anywhere"},
		// Both pieces are 40,000 addresses from the chunk's middle (10.2.100.127), outside its /16.
		{"equal distance: the bigger piece before", "10.1.200.0/26,DE,,Berlin,x\n10.3.0.191/32,DE,,Munich,x\n", "DE/Berlin/89-asn-anywhere"},
		{"equal distance: the bigger piece after", "10.1.200.62/32,DE,,Berlin,x\n10.3.0.192/26,DE,,Munich,x\n", "DE/Munich/89-asn-anywhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layer, asn := farBase()
			// The two far pieces keep rung 82 away (no city holds 70%).
			layer += tc.pieces + "10.0.0.0/24,DE,,Munich,x\n10.0.1.0/24,DE,,Berlin,x\n" + "10.2.100.0/24,DE,,,x\n"
			out, _ := guessBuild(t, t.TempDir(), layer, asn, nil, true)
			checkCities(t, out, map[string]string{"10.2.100.9": tc.want})
		})
	}
	t.Run("across a whole /8", func(t *testing.T) {
		asn := "100,20.0.0.0/8\n200,200.0.0.0/8\n"
		// The piece of AS100 is in 20.0.0.0/8; the chunk is in 20.0.0.0/8 too. A group in
		// 200.0.0.0/8 has a piece far from the chunk.
		layer := fillerDE() + "20.0.0.0/24,DE,,Berlin,x\n20.0.8.0/24,DE,,Munich,x\n200.0.0.0/24,DE,,,x\n20.128.0.0/24,DE,,,x\n"
		out, _ := guessBuild(t, t.TempDir(), layer, &asn, nil, true)
		checkCities(t, out, map[string]string{
			"20.128.0.9": "DE/Munich/89-asn-anywhere",
			"200.0.0.9":  "DE/<nil>/<nil>", // AS200 has no piece
		})
	})
	t.Run("one ASN with pieces 180 /8s apart", func(t *testing.T) {
		asn := "100,20.0.0.0/8\n100,200.0.0.0/8\n"
		layer := fillerDE() + "20.0.0.0/24,DE,,Berlin,x\n20.0.8.0/24,DE,,Munich,x\n200.0.0.0/24,DE,,,x\n"
		out, _ := guessBuild(t, t.TempDir(), layer, &asn, nil, true)
		checkCities(t, out, map[string]string{"200.0.0.9": "DE/Munich/89-asn-anywhere"})
	})
}

// In a country with a main city the chunk gets rung 86, never 89.
func TestASNAnywhereNotWithCountryMain(t *testing.T) {
	layer, asn := farBase()
	layer = rows24(8, 0, 200, "DE", "Hamburg") + layer + "10.0.0.0/24,DE,,Berlin,x\n10.0.200.0/24,DE,,Munich,x\n10.2.5.0/24,DE,,,x\n"
	out, stats := guessBuild(t, t.TempDir(), layer, asn, nil, true)
	checkCities(t, out, map[string]string{"10.2.5.9": "DE/Hamburg/86-country-main"})
	if s := statsOf(t, stats, asnAnywhereSource); s.rows != 0 || s.note != "asn-anywhere: 0 addresses filled" {
		t.Errorf("89 stats = %+v", s)
	}
	if strings.Contains(dump(t, out), asnAnywhereSource) {
		t.Error("89-asn-anywhere in a country with a main city")
	}
}

// A chunk filled by rung 89 is not a piece for another chunk. 10.1.100.0/24 is nearer to
// Berlin (91,008 addresses away) than to Munich and gets Berlin. 10.2.100.0/24 has Munich
// (91,009 away) as its nearest real piece; with the filled 10.1.100.0/24 counted as a piece
// (65,408 away), Berlin would be nearer.
func TestASNAnywhereSnapshot(t *testing.T) {
	layer, asn := farBase()
	layer += "10.0.0.0/24,DE,,Berlin,x\n10.3.200.0/24,DE,,Munich,x\n10.1.100.0/24,DE,,,x\n10.2.100.0/24,DE,,,x\n"
	out, _ := guessBuild(t, t.TempDir(), layer, asn, nil, true)
	checkCities(t, out, map[string]string{
		"10.1.100.9": "DE/Berlin/89-asn-anywhere",
		"10.2.100.9": "DE/Munich/89-asn-anywhere",
	})
}

// Rung 89 needs -fallback and the ASN prefixes.
func TestASNAnywhereFlags(t *testing.T) {
	layer, asn := farBase()
	layer += "10.0.0.0/24,DE,,Berlin,x\n10.0.200.0/24,DE,,Munich,x\n10.2.20.0/24,DE,,,x\n"
	for _, tc := range []struct {
		name     string
		asn      *string
		fallback bool
	}{{"no -fallback", asn, false}, {"no ASN prefixes", nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			out, stats := guessBuild(t, t.TempDir(), layer, tc.asn, nil, tc.fallback)
			for _, s := range stats {
				if s.name == asnAnywhereSource {
					t.Error("rung 89 ran")
				}
			}
			if strings.Contains(dump(t, out), asnAnywhereSource) {
				t.Error("89-asn-anywhere record")
			}
			checkCities(t, out, map[string]string{"10.2.20.9": "DE/<nil>/<nil>"})
		})
	}
}
