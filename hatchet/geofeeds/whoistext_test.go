package geofeeds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

func TestNormWords(t *testing.T) {
	tests := []struct {
		in    string
		want  []string
		other bool
	}{
		{"Amsterdam", []string{"amsterdam"}, false},
		{"Zürich-Altstetten", []string{"zurich", "altstetten"}, false},
		{"Málaga, Spain", []string{"malaga", "spain"}, false},
		{"Łódź", []string{"lodz"}, false},
		{"Straße 12", []string{"strasse"}, false},
		{"St. Gallen", []string{"saint", "gallen"}, false},
		{"Ste-Foy / ST", []string{"sainte", "foy", "saint"}, false},
		{"NET_ACME-ROUTER", []string{"net", "acme", "router"}, false},
		{"a\u0301bc", []string{"abc"}, false}, // a combining mark is dropped
		{"Москва Moscow", []string{"moscow"}, true},
		{"Αθήνα", nil, true},
		{"  ... 123 ", nil, false},
		{"", nil, false},
	}
	for _, tt := range tests {
		got, other := normWords(tt.in)
		if !reflect.DeepEqual(got, tt.want) || other != tt.other {
			t.Errorf("normWords(%q) = %q, %v; want %q, %v", tt.in, got, other, tt.want, tt.other)
		}
	}
	// Latin-1 bytes (not valid UTF-8) are read as Latin-1; valid UTF-8 is left alone.
	if got, _ := normWords(latin1("Gen\xe8ve K\xf6ln")); !reflect.DeepEqual(got, []string{"geneve", "koln"}) {
		t.Errorf("latin-1: %q", got)
	}
	if got := latin1("Genève"); got != "Genève" {
		t.Errorf("valid UTF-8 changed: %q", got)
	}
}

// The ignore list file has entries, each of which survives normalisation, so a typo
// that would make an entry match nothing is caught.
func TestWhoisIgnoreList(t *testing.T) {
	entries := ignoreEntries(whoisIgnoreList)
	if len(entries) < 100 {
		t.Fatalf("%d entries", len(entries))
	}
	for _, e := range entries {
		words, other := normWords(e)
		if len(words) == 0 || other {
			t.Errorf("entry %q has no usable words", e)
			continue
		}
		if !whoisIgnore[strings.Join(words, " ")] {
			t.Errorf("entry %q is not in the ignore set", e)
		}
	}
	for _, want := range []string{"luxembourg", "united kingdom", "cote d ivoire", "centre", "ulus", "kolo"} {
		if !whoisIgnore[want] {
			t.Errorf("%q is not ignored", want)
		}
	}
	// A lone "saint" is a hard rule in code, not an entry.
	if whoisIgnore["saint"] || whoisIgnore["sainte"] {
		t.Error("saint is in the file")
	}
}

// testNames is a small name index: (country, name, ascii, alternates, population).
func testNames() *nameIndex {
	x := newNameIndex()
	for _, r := range []struct {
		cc, name, ascii, alt string
		pop                  int
	}{
		{"NL", "Amsterdam", "Amsterdam", "", 800000},
		{"NL", "Den Haag", "Den Haag", "The Hague,'s-Gravenhage,Haag", 500000},
		{"NL", "Haag", "Haag", "", 9000000}, // bigger, but a shorter name
		{"NL", "Utrecht", "Utrecht", "", 350000},
		{"NL", "Zeist", "Zeist", "", 60000},
		{"NL", "Ede", "Ede", "", 110000}, // 3 letters
		{"NL", "Luxembourg", "Luxembourg", "", 90000},
		{"NL", "Centre", "Centre", "", 90000},
		{"NL", "Te Ko", "Te Ko", "", 1000}, // two words of two letters
		{"NL", "Saint", "Saint", "", 1000},
		{"NL", "Springfield", "Springfield", "", 1000},
		{"NL", "Springfield", "Springfield", "", 5000}, // the same name, more people
		{"NL", "Москва", "Moskva", "Москва", 1000},     // main name in another script, asciiname usable
		{"DE", "Berlin", "Berlin", "Berlino", 3400000},
		{"DE", "Amsterdam", "Amsterdam", "", 10}, // namesake in another country
		{"CH", "Genève", "Geneve", "Genf,Geneva", 200000},
		{"CH", "Sankt Gallen", "Sankt Gallen", "St. Gallen", 75000},
		{"CH", "Zürich", "Zurich", "", 400000},
		{"US", "Spring", "Spring", "", 50000},
	} {
		x.add(r.cc, r.name, r.ascii, r.alt, r.pop)
	}
	return x
}

func linesOf(texts ...string) [][]string {
	var lines [][]string
	for _, s := range texts {
		if w, other := normWords(s); !other && len(w) > 0 {
			lines = append(lines, w)
		}
	}
	return lines
}

func TestTextCity(t *testing.T) {
	x := testNames()
	tests := []struct {
		name    string
		country string
		texts   []string
		want    string // main name, "" for none
	}{
		{"plain", "NL", []string{"Customers in Amsterdam"}, "Amsterdam"},
		{"longest group wins over a bigger place", "NL", []string{"Office Den Haag"}, "Den Haag"},
		{"alternate name", "NL", []string{"The Hague office"}, "Den Haag"},
		{"alternate name with punctuation", "NL", []string{"'s-Gravenhage"}, "Den Haag"},
		{"the bigger place of two groups", "NL", []string{"Zeist Utrecht"}, "Utrecht"},
		{"the bigger place, other order", "NL", []string{"Utrecht Zeist"}, "Utrecht"},
		{"the same name twice: the bigger one", "NL", []string{"Springfield"}, "Springfield"},
		{"only in its own country", "DE", []string{"Amsterdam"}, "Amsterdam"}, // the German namesake
		{"not in another country", "FR", []string{"Amsterdam"}, ""},
		{"country name", "NL", []string{"Luxembourg"}, ""},
		{"ignore list word", "NL", []string{"Centre"}, ""},
		{"three letters", "NL", []string{"Ede"}, ""},
		{"two short words", "NL", []string{"Te Ko"}, ""},
		{"lone saint", "NL", []string{"Saint"}, ""},
		{"st. is saint", "CH", []string{"St. Gallen"}, "Sankt Gallen"},
		{"accent folded", "CH", []string{"Geneve", "Zürich"}, "Zürich"},
		{"main name in another script", "NL", []string{"Moskva"}, "Москва"},
		{"a line of another script is skipped", "NL", []string{"Москва Amsterdam"}, ""},
		{"groups do not cross lines", "NL", []string{"Den", "Haag Zeist"}, "Haag"},
		{"no text", "NL", nil, ""},
		{"numbers separate", "NL", []string{"AMSTERDAM2-NET"}, "Amsterdam"},
	}
	for _, tt := range tests {
		id, ok := x.textCity(tt.country, linesOf(tt.texts...))
		got := ""
		if ok {
			got = x.places[id].name
		}
		if got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
	// The ignore list compares the normalised group: "United Kingdom" is two words.
	y := newNameIndex()
	y.add("GB", "United Kingdom", "United Kingdom", "", 1000)
	if _, ok := y.textCity("GB", linesOf("United Kingdom")); ok {
		t.Error("a country name was a city")
	}
}

// The index keeps the most populous place per key (ties: the smaller main name) and
// is built from the places readPlaces keeps.
func TestNameIndexFromGeoNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), geonamesFile)
	os.WriteFile(path, citiesZip(t, [][]string{
		{"Zaandam", "52.44", "4.83", "NL", "75000"},
		{"Zaandam-Noord", "52.45", "4.83", "NL", "90000", "PPLX"}, // a district: never a name
		{"Twin", "52", "4", "NL", "5000"},
		{"Twinn", "52", "4", "NL", "5000"},
	}), 0o640)
	_, x, err := readPlaces(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := x.textCity("NL", linesOf("Zaandam Noord")); !ok {
		t.Error("Zaandam not found")
	}
	if id, _ := x.textCity("NL", linesOf("Zaandam Noord")); x.places[id].name != "Zaandam" {
		t.Errorf("a district named the city: %q", x.places[id].name)
	}
	if _, ok := x.ids["NL|zaandam noord"]; ok {
		t.Error("the district is indexed")
	}
	// A small place is matched by its name and asciiname, never by an alternate name:
	// those are mostly old or foreign spellings that an ISP or a product also carries.
	z := newNameIndex()
	z.add("MT", "Imdina", "Imdina", "Mdina,Melita", altNameMinPop-1)
	z.add("MT", "Valletta", "Valletta", "Il-Belt", altNameMinPop)
	if _, ok := z.textCity("MT", linesOf("Melita cable customers")); ok {
		t.Error("an alternate name of a small place named a city")
	}
	if id, ok := z.textCity("MT", linesOf("office Imdina")); !ok || z.places[id].name != "Imdina" {
		t.Error("a small place is not found by its own name")
	}
	if id, ok := z.textCity("MT", linesOf("Il-Belt office")); !ok || z.places[id].name != "Valletta" {
		t.Error("an alternate name of a big place is not found")
	}
	// Ties go to the smaller name.
	y := newNameIndex()
	y.add("NL", "Twinn", "Twin", "", 5000)
	y.add("NL", "Twin", "Twin", "", 5000)
	if id := y.ids["NL|twin"]; y.places[id].name != "Twin" {
		t.Errorf("tie: %q", y.places[id].name)
	}
}

func obj(attrs ...string) []attr {
	var out []attr
	for i := 0; i+1 < len(attrs); i += 2 {
		out = append(out, attr{attrs[i], attrs[i+1]})
	}
	return out
}

func TestTextReaderRules(t *testing.T) {
	x := testNames()
	var r textReader
	r.read(obj("inetnum", "192.0.2.0 - 192.0.2.255", "netname", "NL-AMSTERDAM_NET", "country", "nl"), x, 0)                                            // netname split
	r.read(obj("inetnum", "198.51.100.0 - 198.51.100.255", "descr", "Office", "descr", "Utrecht data centre", "country", "NL", "country", "DE"), x, 0) // first country, any descr
	r.read(obj("inetnum", "10.0.0.0 - 10.0.255.255", "descr", "Amsterdam", "country", "NL"), x, 0)                                                     // a /16 is the largest
	r.read(obj("inetnum", "10.4.0.0 - 10.5.255.255", "descr", "Amsterdam", "country", "NL"), x, 0)                                                     // a /15
	r.read(obj("inetnum", "10.6.0.0 - 10.6.0.255", "descr", "Amsterdam", "country", "EU"), x, 0)                                                       // not a country
	r.read(obj("inetnum", "10.7.0.0 - 10.7.0.255", "descr", "Amsterdam"), x, 0)                                                                        // no country
	r.read(obj("inetnum", "10.8.0.0 - 10.8.0.255", "descr", "Nothing here", "country", "NL"), x, 0)                                                    // no city
	r.read(obj("inetnum", "10.9.0.0 - 10.9.0.255", "descr", "Gen\xe8ve", "country", "CH # moved"), x, 0)                                               // Latin-1 and a comment
	r.read(obj("inetnum", "10.10.0.0 - 10.10.0.255", "descr", "Москва Amsterdam", "country", "NL"), x, 0)                                              // other script
	r.read(obj("inetnum", "not a range", "descr", "Amsterdam", "country", "NL"), x, 0)                                                                 // bad range
	r.read(obj("inet6num", "2001:db8::/32", "descr", "Amsterdam", "country", "NL"), x, 0)                                                              // other class
	r.read(obj("descr", "Amsterdam", "inetnum", "10.11.0.0 - 10.11.0.255", "country", "NL"), x, 0)                                                     // class not first
	r.read(nil, x, 0)

	if r.objects != 10 || !reflect.DeepEqual(r.dropped, map[string]int{dropTooLarge: 1, dropBadCountry: 2}) {
		t.Errorf("objects %d, dropped %v", r.objects, r.dropped)
	}
	var got []string
	for _, b := range r.blocks {
		got = append(got, string(b.country[:])+" "+x.places[b.place].name)
	}
	want := []string{"NL Amsterdam", "NL Utrecht", "NL Amsterdam", "CH Genève"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blocks %q, want %q", got, want)
	}
	if b := r.blocks[2]; b.to-b.from != 65535 {
		t.Errorf("the /16: %+v", b)
	}
}

func TestWriteWhoisTextRows(t *testing.T) {
	x := testNames()
	var r textReader
	// Given in no particular order; a nested block, and two blocks with the same start.
	r.read(obj("inetnum", "192.0.2.128 - 192.0.2.191", "descr", "Utrecht", "country", "NL"), x, 0)
	r.read(obj("inetnum", "192.0.2.0 - 192.0.2.255", "descr", "Amsterdam", "country", "NL"), x, 0)
	r.read(obj("inetnum", "10.0.0.0 - 10.0.0.255", "descr", "Berlin", "country", "DE"), x, 1)
	r.read(obj("inetnum", "192.0.2.0 - 192.0.2.127", "descr", "Zeist", "country", "NL"), x, 0)
	r.read(obj("inetnum", "198.51.100.64 - 198.51.100.191", "descr", "Zürich", "country", "CH"), x, 0) // two /26
	path := filepath.Join(t.TempDir(), WhoisTextFile)
	registries := []string{"ripe", "afrinic"}
	rows, err := writeWhoisTextRows(path, r.blocks, x, registries)
	if err != nil {
		t.Fatal(err)
	}
	want := "prefix,country,region,city,source\n" +
		"10.0.0.0/24,DE,,Berlin,whois-text:afrinic\n" +
		"192.0.2.0/24,NL,,Amsterdam,whois-text:ripe\n" +
		"192.0.2.0/25,NL,,Zeist,whois-text:ripe\n" +
		"192.0.2.128/26,NL,,Utrecht,whois-text:ripe\n" +
		"198.51.100.64/26,CH,,Zürich,whois-text:ripe\n" +
		"198.51.100.128/26,CH,,Zürich,whois-text:ripe\n"
	if got := readGzip(t, path); rows != 6 || got != want {
		t.Errorf("rows %d:\n%s\nwant:\n%s", rows, got, want)
	}
	// The same input in another order gives the same file.
	rev := append([]textBlock(nil), r.blocks...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	other := filepath.Join(t.TempDir(), WhoisTextFile)
	if _, err := writeWhoisTextRows(other, rev, x, registries); err != nil || readGzip(t, other) != want {
		t.Errorf("order changes the file: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left: %v", err)
	}
	// No rows: an error, and the file stays.
	if _, err := writeWhoisTextRows(path, nil, x, registries); err == nil || readGzip(t, path) != want {
		t.Errorf("no rows: %v", err)
	}
}

// discover matches the text while it reads the dump, writes the layer file and keeps it
// when GeoNames cannot be had; neither whois layer then blocks the geofeed discovery.
func TestDiscoverWhoisText(t *testing.T) {
	ripe := gzipped(t, "inetnum: 192.0.2.0 - 192.0.2.255\nnetname: EXAMPLE-NET\ndescr: Acme in Amsterdam\ncountry: NL\n"+
		"remarks: Geofeed https://isp.example/geofeed.csv\n\n"+
		"inetnum: 198.51.100.0 - 198.51.100.255\ndescr: Acme in Berlin\ncountry: NL\n\n"+ // Berlin is not in NL
		"inetnum: 203.0.113.0 - 203.0.113.255\ndescr: Acme in Berlin\ncountry: DE\n\n"+
		"inetnum: 10.0.0.0 - 10.1.255.255\ndescr: Amsterdam\ncountry: NL\n")
	mux := http.NewServeMux()
	mux.HandleFunc("/ripe.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(ripe) })
	mux.HandleFunc("/cities.zip", func(w http.ResponseWriter, r *http.Request) { w.Write(citiesZip(t, testCities)) })
	server := httptest.NewServer(mux)
	defer server.Close()
	p := &Pipeline{
		store:     newFakeStore(),
		DataDir:   t.TempDir(),
		dumps:     []dump{{"ripe-inetnum", "ripe", server.URL + "/ripe.gz"}},
		caidaBase: server.URL + "/caida", listPages: []string{},
		geonames:     server.URL + "/cities.zip",
		now:          func() time.Time { return time.Date(2026, 11, 16, 5, 0, 0, 0, time.UTC) },
		allowPrivate: true,
	}
	out, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := WhoisTextOutput{Objects: 4, Blocks: 2, Dropped: map[string]int{dropTooLarge: 1}, Rows: 2, GeoNames: statusDownloaded, Bytes: out.WhoisText.Bytes}
	if !reflect.DeepEqual(out.WhoisText, want) || out.WhoisText.Bytes == 0 || out.Sources["ripe-inetnum"].WhoisText != 2 {
		t.Errorf("whois text = %+v, want %+v; sources %+v", out.WhoisText, want, out.Sources)
	}
	path := filepath.Join(p.dir(), OutDir, WhoisTextFile)
	wantFile := "prefix,country,region,city,source\n192.0.2.0/24,NL,,Amsterdam,whois-text:ripe\n203.0.113.0/24,DE,,Berlin,whois-text:ripe\n"
	if got := readGzip(t, path); got != wantFile {
		t.Errorf("%s:\n%s\nwant:\n%s", WhoisTextFile, got, wantFile)
	}

	// GeoNames gone: both whois layers are skipped with the reason, discover still succeeds
	// with the feed it found, and the files stay.
	os.Remove(filepath.Join(p.dir(), "dumps", geonamesFile))
	p.geonames = server.URL + "/no-cities.zip"
	out, err = p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.WhoisText.Skipped, "GeoNames") || out.WhoisText.Rows != 0 || out.Whois.Skipped == "" {
		t.Errorf("without GeoNames: %+v / %+v", out.WhoisText, out.Whois)
	}
	if got := readGzip(t, path); got != wantFile {
		t.Errorf("the previous file changed:\n%s", got)
	}
	if out.Sources["ripe-inetnum"].URLs != 1 || out.Sources["ripe-inetnum"].Blocks != 1 {
		t.Errorf("the geofeed was not found: %+v", out)
	}
}

func TestUploadWhoisTextGate(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	os.MkdirAll(p.dir(), 0o750)
	var keys []string
	upload := func(_ context.Context, key, _ string, _ publish.PutOptions) (string, publish.PutStatus, error) {
		keys = append(keys, key)
		return "r2://x", publish.PutUploaded, nil
	}
	try := func(rows int, force bool) WhoisUploadOutput {
		writeLayerRows(t, p, WhoisTextFile, rows)
		return p.uploadWhoisText(context.Background(), upload, force)
	}
	if out := p.uploadWhoisText(context.Background(), upload, true); out.Skipped == "" || len(keys) != 0 {
		t.Errorf("no file: %+v", out)
	}
	// The geoloc minimum (20,000) is not this file's: it needs 200,000 on the first upload.
	if out := try(minWhoisTextFirstUpload-1, false); out.Uploaded || !strings.Contains(out.Skipped, "fewer than 200000") {
		t.Errorf("first upload below the minimum: %+v", out)
	}
	if out := try(minWhoisTextFirstUpload, false); !out.Uploaded || !reflect.DeepEqual(keys, []string{"internal/layers/37-whois-text.csv.gz"}) {
		t.Errorf("first upload at the minimum: %+v %v", out, keys)
	}
	// Its own state file, not the geoloc one.
	if _, err := os.Stat(filepath.Join(p.dir(), whoisTextUploadFile)); err != nil {
		t.Errorf("state file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.dir(), whoisUploadFile)); err == nil {
		t.Errorf("%s written", whoisUploadFile)
	}
	if out := try(minWhoisTextFirstUpload*91/100, false); !out.Uploaded {
		t.Errorf("a 9%% drop: %+v", out)
	}
	if out := try(minWhoisTextFirstUpload*80/100, false); out.Uploaded || !strings.Contains(out.Skipped, "down from") {
		t.Errorf("a drop of more than 10%%: %+v", out)
	}
	if out := try(minWhoisTextFirstUpload*80/100, true); !out.Uploaded {
		t.Errorf("forced drop: %+v", out)
	}
	// A file discover did not rebuild is not sent again.
	path := filepath.Join(p.dir(), OutDir, WhoisTextFile)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(path, past, past)
	if out := p.uploadWhoisText(context.Background(), upload, false); out.Uploaded || !strings.Contains(out.Skipped, "not rebuilt") {
		t.Errorf("an old file: %+v", out)
	}
}
