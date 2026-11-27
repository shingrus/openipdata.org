package main

import (
	"bytes"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

// gnRow is a cities1000 line with a main name, an ascii name and alternates.
func gnRow(id, name, ascii, alternates, lat, lon, country, pop string) string {
	return strings.Join([]string{id, name, ascii, alternates, lat, lon, "P", "PPL", country, "", "00", "", "", "", pop, "", "10", "UTC", "2024-01-01"}, "\t") + "\n"
}

func cleanerFixture(t *testing.T) (*cityCleaner, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cities1000.txt")
	body := gnRow("1", "São Paulo", "Sao Paulo", "", "-23.5475", "-46.6361", "BR", "10000000") +
		gnRow("2", "Düsseldorf", "Duesseldorf", "", "51.2217", "6.77616", "DE", "600000") +
		gnRow("3", "Saint Petersburg", "Saint Petersburg", "Sankt-Peterburg", "59.9386", "30.3141", "RU", "5000000") +
		gnRow("4", "Wetzikon", "Wetzikon", "", "47.3236", "8.7985", "CH", "20000") +
		gnRow("5", "Belgrade", "Belgrade", "Beograd", "44.804", "20.4651", "RS", "1200000") +
		gnRow("6", "Taipei", "Taipei", "", "25.0478", "121.5319", "TW", "2600000") +
		gnRow("7", "Mount Olive", "Mount Olive", "", "35.19", "-78.06", "US", "4600") +
		gnRow("8", "Washington", "Washington", "", "38.89511", "-77.03637", "US", "600000") +
		gnRow("9", "Serbia", "Serbia", "", "44", "21", "RS", "100") +
		gnRow("10", "Zürich", "Zurich", "", "47.3667", "8.55", "CH", "400000")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	index, folded, err := loadGeoNamesFolded(path, true)
	if err != nil {
		t.Fatal(err)
	}
	return &cityCleaner{index: index, folded: folded}, path
}

func TestFoldName(t *testing.T) {
	for in, want := range map[string]string{
		"São Paulo":      "sao paulo",
		"ST. PETERSBURG": "st petersburg",
		"Straße":         "strasse",
		"Łódź - Øst":     "lodz ost",
		"Đà Nẵng":        "da nang",
		"áb":            "ab", // combining acute
		"  --x--  ":      "x",
		"":               "",
		"Москва":         "",
	} {
		if got := foldName(in); got != want {
			t.Errorf("foldName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepairEncoding(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"D\xfcsseldorf", "Düsseldorf"},           // Latin-1 byte
		{"Z\xfcrich \x96 X", "Zürich – X"},        // Windows-1252 dash
		{"D\xc3\xbcsseldorf\xff", "DÃ¼sseldorfÿ"}, // the whole string is read as Windows-1252
		{"A\x81B", "A\x81B"},                      // undefined Windows-1252 byte: no repair
		{"DÃ¼sseldorf", "Düsseldorf"},             // mojibake
		{"Düsseldorf", "Düsseldorf"},              // valid text is untouched
		{"Москва", "Москва"},                      // beyond Latin-1
		{"Plain", "Plain"},
	} {
		if got := repairEncoding(c.in); got != c.want {
			t.Errorf("repairEncoding(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCityClean(t *testing.T) {
	c, _ := cleanerFixture(t)
	for _, tc := range []struct {
		code, city, want string
		renamed          bool
	}{
		{"BR", "São Paulo", "São Paulo", false},           // places exactly: untouched
		{"BR", "SAO PAULO", "SAO PAULO", false},           // the ascii name places it exactly: untouched
		{"BR", "Sao  Paulo", "São Paulo", true},           // folded
		{"BR", "São Paulo/SP", "São Paulo", true},         // slash part
		{"BR", "Sao Paulo, SP", "São Paulo", true},        // comma part
		{"RU", "ST PETERSBURG", "Saint Petersburg", true}, // st -> saint
		{"RU", "St. Petersburg", "Saint Petersburg", true},
		{"CH", "Wetzikon (ZH)", "Wetzikon", true},           // group dropped
		{"RS", "Belgrade/Serbia", "Belgrade/Serbia", false}, // Serbia is a place of RS in the fixture: a list
		{"RS", "Belgrade/Srbija", "Belgrade", true},
		{"DE", "D\xfcsseldorf", "Düsseldorf", true}, // invalid UTF-8
		{"DE", "DÃ¼sseldorf", "Düsseldorf", true},   // mojibake
		{"DE", "Duesseldorf", "Duesseldorf", false}, // ascii name places exactly
		{"US", "MT OLIVE", "Mount Olive", true},     // mt -> mount
		{"TW", "Taipei City", "Taipei", true},       // trailing word
		{"US", "Washington DC", "Washington", true},
		{"US", "Washington D.C.", "Washington", true},
		{"US", "Washington 12", "Washington", true},         // a number
		{"CH", "Zurich - Zürich", "Zurich - Zürich", false}, // exact? no: a list of two places is left alone
		{"CH", "Nowhereville", "Nowhereville", false},       // no hit keeps the city
		{"DE", "Bad\x81Name", "Bad\x81Name", false},         // invalid and not repairable
		{"", "Taipei City", "Taipei City", false},           // no country: nothing to look up
	} {
		got, renamed := c.clean(tc.code, tc.city)
		if got != tc.want || renamed != tc.renamed {
			t.Errorf("clean(%s, %q) = %q, %v; want %q, %v", tc.code, tc.city, got, renamed, tc.want, tc.renamed)
		}
	}
}

// A list of places is never cut: "Belgrade/Serbia" would be cut only when the second part is
// not a place of the country.
func TestCityCleanList(t *testing.T) {
	c, _ := cleanerFixture(t)
	if got, ok := c.clean("CH", "Wetzikon; Zürich"); ok || got != "Wetzikon; Zürich" {
		t.Errorf("list of two places: %q, %v", got, ok)
	}
	if got, ok := c.clean("CH", "Wetzikon, Canton"); !ok || got != "Wetzikon" {
		t.Errorf("place and a word: %q, %v", got, ok)
	}
}

func TestFoldedIndexTieRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cities1000.txt")
	body := gnRow("1", "Alpha", "Alpha", "Cafe", "1", "1", "XX", "100") +
		gnRow("2", "Café", "Cafe", "", "2", "2", "XX", "10") + // a primary name beats an alternate
		gnRow("3", "Bravo", "Bravo", "", "3", "3", "XX", "50") +
		gnRow("4", "Brávo", "Bravo", "", "4", "4", "XX", "70") // more people wins
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, folded, err := loadGeoNamesFolded(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := folded["XX|cafe"].name; got != "Café" {
		t.Errorf("cafe = %q", got)
	}
	if got := folded["XX|bravo"].name; got != "Brávo" {
		t.Errorf("bravo = %q", got)
	}
	if _, plain, _ := loadGeoNamesFolded(path, false); plain != nil {
		t.Error("the folded index is built without being asked for")
	}
}

func TestBuildGeoCityNames(t *testing.T) {
	_, gn := cleanerFixture(t)
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), "prefix,country,region,city,source\n"+
		"45.0.0.0/24,BR,,São Paulo/SP,x\n"+
		"45.0.1.0/24,BR,,São Paulo,x\n"+
		"45.0.2.0/24,CH,,Nowhereville,x\n"+
		"45.0.3.0/24,DE,,D\xfcsseldorf,x\n")
	index, folded, err := loadGeoNamesFolded(gn, true)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "with.mmdb")
	stats, err := buildGeoNames([]string{layer}, "", out, false, nil, nil, nil, index, folded, "", false, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].renamed != 2 || stats[0].placed != 2 || stats[0].unplaced != 1 {
		t.Fatalf("stats = %+v, want 2 renamed, 2 placed, 1 unplaced", stats)
	}
	reader, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for ip, want := range map[string]string{"45.0.0.1": "São Paulo", "45.0.1.1": "São Paulo", "45.0.2.1": "Nowhereville", "45.0.3.1": "Düsseldorf"} {
		var r map[string]any
		if err := reader.Lookup(netip.MustParseAddr(ip)).Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r["city_name"] != want {
			t.Errorf("%s: city %v, want %q", ip, r["city_name"], want)
		}
		if want == "São Paulo" && (r["latitude"] != -23.5475 || r["longitude"] != -46.6361) {
			t.Errorf("%s: coordinates %v %v", ip, r["latitude"], r["longitude"])
		}
		if want == "Nowhereville" && r["latitude"] != nil {
			t.Errorf("%s: unexpected coordinates", ip)
		}
	}

	// The same rows with the exact index only (or none): nothing is cleaned, the bytes are the
	// old builder's.
	plainIndex, _, _ := loadGeoNamesFolded(gn, false)
	a, b := filepath.Join(dir, "a.mmdb"), filepath.Join(dir, "b.mmdb")
	if _, err := buildGeoGuesses([]string{layer}, "", a, false, nil, nil, nil, plainIndex, "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := buildGeoNames([]string{layer}, "", b, false, nil, nil, nil, plainIndex, nil, "", false, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	c, d := filepath.Join(dir, "c.mmdb"), filepath.Join(dir, "d.mmdb")
	if _, err := buildGeo([]string{layer}, "", c, false, nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := buildGeoNames([]string{layer}, "", d, false, nil, nil, nil, nil, folded, "", false, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	same := func(x, y string) bool {
		bx, _ := os.ReadFile(x)
		by, _ := os.ReadFile(y)
		// the build epoch is in the metadata: compare the data and tree, not the metadata
		i, j := bytes.Index(bx, []byte("\xab\xcd\xefMaxMind.com")), bytes.Index(by, []byte("\xab\xcd\xefMaxMind.com"))
		return i >= 0 && j >= 0 && bytes.Equal(bx[:i], by[:j])
	}
	if !same(a, b) || !same(c, d) {
		t.Error("output differs without a folded index")
	}
	if same(a, filepath.Join(dir, "with.mmdb")) {
		t.Error("the cleanup changed nothing")
	}
}
