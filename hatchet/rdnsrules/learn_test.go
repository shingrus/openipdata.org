package rdnsrules

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLearnCogentStyleWithDigitAndHyphenBoundaries(t *testing.T) {
	dict := Dictionaries{"iata": {"fra": {Country: "DE", City: "Frankfurt"}, "lon": {Country: "GB", City: "London"}}}
	names := []Name{
		{IP: "192.0.2.1", PTR: "be2433.ccr41.fra03.atlas.cogentco.com."},
		{IP: "192.0.2.2", PTR: "be3042.ccr22.lon13.atlas.cogentco.com."},
		{IP: "192.0.2.3", PTR: "xe-4.ccr40.fra99.atlas.cogentco.com."},
	}
	c, ok := learnDomain("cogentco.com", names, dict)
	if !ok {
		t.Fatal("expected supported candidate")
	}
	if c.Scope.LabelFromRight != 2 || c.Scope.TokenFromLeft != 1 || c.Dictionary != "iata" || c.Decoded != 3 || c.Distinct != 2 {
		t.Fatalf("unexpected candidate: %+v", c)
	}
	if !patternAgrees(c.Pattern, names, c.Domain, extraction{label: 2, token: 1, dictionary: "iata"}) {
		t.Fatalf("pattern does not reproduce learner: %s", c.Pattern)
	}
}

func TestLearnLeavesAmbiguousTokensUnresolved(t *testing.T) {
	csvText := "type,iata_code,iso_country,municipality,name\nlarge_airport,FRA,DE,Frankfurt,Frankfurt Airport\nlarge_airport,FRA,FR,Nice,Nice Airport\nlarge_airport,LHR,GB,London,Heathrow\n"
	d, err := parseAirports([]byte(csvText))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d["fra"]; ok {
		t.Fatal("ambiguous IATA code must remain unresolved")
	}
	if _, ok := d["lhr"]; !ok {
		t.Fatal("unambiguous IATA code must resolve")
	}
}

func TestExtractionPatternRequiresWholeToken(t *testing.T) {
	names := []Name{{IP: "a", PTR: "xfrankfurt-de1.dc.example.net"}, {IP: "b", PTR: "frankfurt-de1.dc.example.net"}}
	e := extraction{label: 2, token: 1}
	if !patternAgrees(extractionPattern("example.net", e), names, "example.net", e) {
		r, _ := regexp.Compile(extractionPattern("example.net", e))
		var details []string
		for _, n := range names {
			want, ok := extract(n.PTR, "example.net", e)
			details = append(details, fmt.Sprintf("%s => %q %v", n.PTR, want, ok))
			details = append(details, fmt.Sprintf("match=%q", r.FindStringSubmatch(n.PTR)))
		}
		t.Fatal("generated regex must match the learner: " + strings.TrimSpace(extractionPattern("example.net", e)) + " " + strings.Join(details, "; "))
	}
}

func TestExtractionPatternTokenTwoAcrossHyphensAndDigits(t *testing.T) {
	names := []Name{{IP: "1", PTR: "msk-m9-p1.ip.example.net"}, {IP: "2", PTR: "ams-12-e3.ip.example.net"}, {IP: "3", PTR: "xe--fra2bb3.ip.example.net"}, {IP: "4", PTR: "r1fra2bb3.ip.example.net"}}
	e := extraction{label: 2, token: 2}
	if !patternAgrees(extractionPattern("example.net", e), names, "example.net", e) {
		t.Fatalf("token two regex differs from extractor: %s", extractionPattern("example.net", e))
	}
	if got, _ := extract(names[0].PTR, "example.net", e); got != "m" {
		t.Fatalf("token=%q", got)
	}
}

func TestExtractionPatternTokenOneAdjacentLetterDigitRuns(t *testing.T) {
	names := []Name{{IP: "1", PTR: "xe1fra2.atlas.example.net"}, {IP: "2", PTR: "fra1bb2.atlas.example.net"}, {IP: "3", PTR: "xe--fra2.atlas.example.net"}}
	e := extraction{label: 2, token: 1}
	if !patternAgrees(extractionPattern("example.net", e), names, "example.net", e) {
		t.Fatalf("token one regex differs from extractor: %s", extractionPattern("example.net", e))
	}
}

func TestManualCodeOverridesOnlyMatchingScope(t *testing.T) {
	names := []Name{{IP: "1", PTR: "msk-m9-p1.ip.example.net"}, {IP: "2", PTR: "msk-m8-p2.ip.example.net"}, {IP: "3", PTR: "kzn-m7-p3.ip.example.net"}}
	sc := Scope{LabelFromRight: 2, TokenFromLeft: 1, Dictionary: "iata"}
	codes := map[string]Code{"msk": {Country: "RU", City: "Moscow", Origin: "manual", Scope: sc}, "kzn": {Country: "RU", City: "Kazan", Origin: "manual", Scope: sc}}
	c, ok := learnDomainCodes("example.net", names, Dictionaries{}, codes)
	if !ok {
		t.Fatal("manual override should support the observed token")
	}
	if c.Dictionary != "iata" || c.Places["msk"] != (Place{Country: "RU", City: "Moscow"}) {
		t.Fatalf("override not applied: %+v", c)
	}
	wsc := Scope{LabelFromRight: 1, TokenFromLeft: 1, Dictionary: "iata"}
	wrong := map[string]Code{"msk": {Country: "RU", City: "Moscow", Origin: "manual", Scope: wsc}, "kzn": {Country: "RU", City: "Kazan", Origin: "manual", Scope: wsc}}
	if _, ok := learnDomainCodes("example.net", names, Dictionaries{}, wrong); ok {
		t.Fatal("override at a different extraction scope must not be reused")
	}
}

func TestParseLOCODEOfficialColumnNames(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("CodeListPart1.csv")
	w := csv.NewWriter(f)
	_ = w.Write([]string{"Country", "Location", "Name", "NameWoDiacritics"})
	_ = w.Write([]string{"DE", "FRA", "Frankfurt am Main", "Frankfurt am Main"})
	w.Flush()
	_ = zw.Close()
	m, err := parseLOCODE(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := m["defra"]; got != (Place{Country: "DE", City: "Frankfurt am Main"}) {
		t.Fatalf("place=%+v", got)
	}
}

func TestParseLOCODEMergesAllPublicationParts(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("CodeListPart1.csv")
	w := csv.NewWriter(f)
	_ = w.Write([]string{"Change", "Country", "Location", "Name", "NameWoDiacritics", "SubDiv", "Function", "Status", "Date", "IATA", "Coordinates", "Remarks"})
	_ = w.Write([]string{"", "DE", "FRA", "Frankfurt am Main", "Frankfurt am Main", "", "1-------", "AI", "2501", "", "", ""})
	w.Flush()
	f2, _ := zw.Create("CodeListPart2.csv")
	w2 := csv.NewWriter(f2)
	_ = w2.Write([]string{"", "US", "NYC", "New York", "New York", "", "1-------", "AI", "2501", "", "", ""})
	w2.Flush()
	_ = zw.Close()
	m, err := parseLOCODE(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if m["defra"].City != "Frankfurt am Main" || m["usnyc"].City != "New York" {
		t.Fatalf("did not merge LOCODE files: %+v", m)
	}
}

func TestParseOfficialLOCODEArchiveWhenProvided(t *testing.T) {
	path := os.Getenv("OPENIPDATA_TEST_UNLOCODE_ZIP")
	if path == "" {
		t.Skip("set OPENIPDATA_TEST_UNLOCODE_ZIP to smoke-test the official release archive")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseLOCODE(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) < 1000 {
		t.Fatalf("official UN/LOCODE dictionary has only %d unambiguous codes", len(m))
	}
	if _, ok := m["defra"]; !ok {
		t.Fatal("Frankfurt code missing from official archive")
	}
}

func TestDictionaryCacheMonthlyConditionalRefresh(t *testing.T) {
	var requests atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			return &http.Response{StatusCode: http.StatusNotModified, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		h := make(http.Header)
		h.Set("ETag", `"v1"`)
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("dictionary")), Request: r}, nil
	})
	client := &http.Client{Transport: transport}
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	data, fresh, err := fetchSource(context.Background(), dir, "source.csv", "https://example.test/source.csv", client, now)
	if err != nil || !fresh || string(data) != "dictionary" {
		t.Fatalf("first fetch: %q %v %v", data, fresh, err)
	}
	// A recent checked marker is written by loadDictionaries only after parsing.
	path := filepath.Join(dir, "source.csv.checked")
	if err = os.WriteFile(path, []byte(now.Format(time.RFC3339Nano)), 0o640); err != nil {
		t.Fatal(err)
	}
	_, fresh, err = fetchSource(context.Background(), dir, "source.csv", "https://example.test/source.csv", client, now.Add(7*24*time.Hour))
	if err != nil || fresh {
		t.Fatalf("weekly run should use cache: fresh=%v err=%v", fresh, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("weekly run made %d requests", requests.Load())
	}
	_, fresh, err = fetchSource(context.Background(), dir, "source.csv", "https://example.test/source.csv", client, now.Add(30*24*time.Hour))
	if err != nil || !fresh {
		t.Fatalf("monthly run should revalidate: fresh=%v err=%v", fresh, err)
	}
	if requests.Load() != 2 {
		t.Fatalf("monthly run made %d total requests", requests.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRoleTokenDoesNotBeatLocationToken(t *testing.T) {
	// `ccr` is in the (fixture) airport dictionary and is in every name.
	dict := Dictionaries{"iata": {
		"ccr": {Country: "US", City: "Concord"},
		"fra": {Country: "DE", City: "Frankfurt"},
		"lon": {Country: "GB", City: "London"},
		"ams": {Country: "NL", City: "Amsterdam"},
	}}
	names := []Name{
		{IP: "192.0.2.1", PTR: "be2433.ccr41.fra03.atlas.cogentco.com"},
		{IP: "192.0.2.2", PTR: "be3042.ccr22.lon13.atlas.cogentco.com"},
		{IP: "192.0.2.3", PTR: "be1.ccr21.ams01.atlas.cogentco.com"},
		{IP: "192.0.2.4", PTR: "be1.ccr31.ams02.atlas.cogentco.com"},
	}
	c, ok := learnDomain("cogentco.com", names, dict)
	if !ok || c.Scope.LabelFromRight != 2 || len(c.Places) != 3 || c.Places["ccr"] != (Place{}) {
		t.Fatalf("role token won: %+v ok=%v", c, ok)
	}
	// Even with no stop list entry, a single-valued combination loses to a multi-valued one.
	delete(roleTokens, "ccr")
	defer func() { roleTokens["ccr"] = true }()
	c, ok = learnDomain("cogentco.com", names, dict)
	if !ok || c.Scope.LabelFromRight != 2 {
		t.Fatalf("constant token won the ranking: %+v ok=%v", c, ok)
	}
}

func TestSingleValuedExtractionGetsNoRule(t *testing.T) {
	// A one-token column is a role, even a one-city operator gets no rule.
	dict := Dictionaries{"iata": {"fra": {Country: "DE", City: "Frankfurt"}}}
	names := []Name{
		{IP: "192.0.2.1", PTR: "r1.fra1.example.net"},
		{IP: "192.0.2.2", PTR: "r2.fra1.example.net"},
		{IP: "192.0.2.3", PTR: "r3.fra2.example.net"},
	}
	if c, ok := learnDomain("example.net", names, dict); ok {
		t.Fatalf("single-valued rule learned: %+v", c)
	}
}

func TestMyvzwConstantSubGetsNoRule(t *testing.T) {
	dict := Dictionaries{"geonames": {"sub": {Country: "ID", City: "Surabaya"}}}
	var names []Name
	for i := 1; i <= 6; i++ {
		names = append(names, Name{IP: fmt.Sprintf("192.0.2.%d", i), PTR: fmt.Sprintf("sub%d.sub.myvzw.com", i), Country: "US"})
	}
	if c, ok := learnDomain("myvzw.com", names, dict); ok {
		t.Fatalf("rule for a constant word: %+v", c)
	}
	delete(roleTokens, "sub") // not even without the stop list
	defer func() { roleTokens["sub"] = true }()
	if c, ok := learnDomain("myvzw.com", names, dict); ok {
		t.Fatalf("single-valued rule without stop list: %+v", c)
	}
}

func TestStopListCoversWordsAndRegions(t *testing.T) {
	for _, w := range strings.Fields("ip ips pool sub isp net static dsl ppp cable mobile res biz bus inf cust host home ftth lte red rev dns gw cloud cdn dedi colo dc al il ma mi la dc ny tx ca on qc bc ab") {
		if !roleTokens[w] {
			t.Errorf("%q is not in the stop list", w)
		}
	}
	for _, w := range []string{"boston", "denver", "fra", "lhr", "orem"} {
		if roleTokens[w] {
			t.Errorf("%q must not be a stop word", w)
		}
	}
}

func TestComcastStateCodesAreSkippedCityPositionLearned(t *testing.T) {
	dict := Dictionaries{"geonames": {
		"boston": {Country: "US", City: "Boston"}, "denver": {Country: "US", City: "Denver"},
		"orem": {Country: "US", City: "Orem"}, "ogden": {Country: "US", City: "Ogden"},
		// what the real dictionary used to say about the state codes
		"ma": {Country: "TH", City: "Wiang Pa Pao"}, "co": {Country: "CO", City: "Co"}, "ut": {Country: "TH", City: "Ut"},
		"il": {Country: "KE", City: "Il"}, "la": {Country: "XX", City: "La"}, "mi": {Country: "CN", City: "Qingpingjie"},
	}}
	pairs := [][2]string{{"boston", "ma"}, {"denver", "co"}, {"orem", "ut"}, {"ogden", "ut"}, {"boston", "ma"}, {"denver", "co"}}
	var names []Name
	for i, p := range pairs {
		names = append(names, Name{IP: fmt.Sprintf("192.0.2.%d", i+1), PTR: fmt.Sprintf("ae%d-ar01.%s.%s.comcast.net", i, p[0], p[1]), Country: "US"})
	}
	c, ok := learnDomain("comcast.net", names, dict)
	if !ok || c.Dictionary != "geonames" || c.Scope.LabelFromRight != 2 || len(c.Places) != 4 || c.Places["boston"].City != "Boston" {
		t.Fatalf("city position not learned: %+v ok=%v", c, ok)
	}
	if _, bad := c.Places["ma"]; bad {
		t.Fatal("state code decoded")
	}
}

func TestDomesticOperatorNeedsCountryAgreement(t *testing.T) {
	// Spectrum-like: every router is in the US but the tokens decode abroad.
	dict := Dictionaries{"iata": {"biz": {Country: "DE", City: "Bizcity"}, "inf": {Country: "FR", City: "Infville"}, "xyz": {Country: "DE", City: "Xyzburg"}}}
	var names []Name
	for i, tok := range []string{"xyz", "xyz", "abc", "xyz", "xyz"} {
		names = append(names, Name{IP: fmt.Sprintf("192.0.2.%d", i+1), PTR: fmt.Sprintf("r%d.%s.spectrum.com", i, tok), Country: "US"})
	}
	dict["iata"]["abc"] = Place{Country: "FR", City: "Abcville"}
	if !isDomestic(names) {
		t.Fatal("domestic not detected")
	}
	if c, ok := learnDomain("spectrum.com", names, dict); ok {
		t.Fatalf("domestic rule with country_agree 0: %+v", c)
	}
	// The same decode in a domestic operator that agrees passes.
	dict["iata"]["xyz"], dict["iata"]["abc"] = Place{Country: "US", City: "Xyz"}, Place{Country: "US", City: "Abc"}
	if c, ok := learnDomain("spectrum.com", names, dict); !ok || c.CountryAgree == nil || *c.CountryAgree != 1 {
		t.Fatalf("agreeing domestic rule dropped: %+v ok=%v", c, ok)
	}
}

func TestGlobalCarrierStaysUngated(t *testing.T) {
	dict := Dictionaries{"iata": {"fra": {Country: "DE", City: "Frankfurt"}, "lon": {Country: "GB", City: "London"}, "ams": {Country: "NL", City: "Amsterdam"}, "jfk": {Country: "US", City: "New York"}}}
	// Router countries are the carrier's: half US, the rest scattered; agreement is 0.5.
	ctry := []string{"US", "US", "US", "DE", "NL", "GB"}
	toks := []string{"fra", "lon", "ams", "fra", "jfk", "lon"}
	var names []Name
	for i := range toks {
		names = append(names, Name{IP: fmt.Sprintf("192.0.2.%d", i+1), PTR: fmt.Sprintf("be%d.ccr41.%s03.atlas.cogentco.com", i, toks[i]), Country: ctry[i]})
	}
	if isDomestic(names) {
		t.Fatal("global carrier called domestic")
	}
	if c, ok := learnDomain("cogentco.com", names, dict); !ok || c.Scope.LabelFromRight != 2 {
		t.Fatalf("Cogent-like rule lost: %+v ok=%v", c, ok)
	}
}

func TestGeoNamesMinimumTokenAndPopulation(t *testing.T) {
	b := citiesZip(t, [][3]string{
		{"Ip", "RO", "20000"}, {"Pool", "GB", "30000"}, {"Sub", "ID", "50000"},
		{"Orem", "US", "98000"}, {"Boston", "US", "650000"}, {"Smallville", "US", "14999"}, {"Exactville", "US", "15000"},
		{"Frankfurt am Main", "DE", "750000"},
	})
	m, aliases, err := parseGeoNamesWithAliases(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"ip", "sub", "smallville"} {
		if _, ok := m[w]; ok {
			t.Errorf("%q resolves", w)
		}
		if _, ok := aliases[cityAlias{"US", w}]; ok {
			t.Errorf("alias %q resolves", w)
		}
	}
	// "pool" (4 letters, pop 30000) passes the size rules; the stop list is what blocks it.
	for _, w := range []string{"pool", "orem", "boston", "exactville", "frankfurtammain"} {
		if _, ok := m[w]; !ok {
			t.Errorf("%q lost", w)
		}
	}
}

func TestGeoNamesPopulationAfterNamesakeRule(t *testing.T) {
	// Two small namesakes stay unresolved; a dominant one below the floor is dropped, not promoted.
	b := citiesZip(t, [][3]string{{"Lakeview", "US", "12000"}, {"Lakeview", "CA", "500"}, {"Lakeside", "US", "40000"}, {"Lakeside", "CA", "40000"}, {"Bigtown", "US", "20000"}})
	m, _, err := parseGeoNamesWithAliases(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["lakeview"]; ok {
		t.Error("dominant namesake below the population floor resolved")
	}
	if _, ok := m["lakeside"]; ok {
		t.Error("ambiguous namesake resolved")
	}
	if _, ok := m["bigtown"]; !ok {
		t.Error("plain place lost")
	}
}

func TestOddNameDoesNotBlockDomain(t *testing.T) {
	dict := Dictionaries{"iata": {"fra": {Country: "DE", City: "Frankfurt"}, "lon": {Country: "GB", City: "London"}}}
	names := []Name{
		{IP: "192.0.2.1", PTR: "r1.fra1.example.net"},
		{IP: "192.0.2.2", PTR: "r2.lon1.example.net"},
		{IP: "192.0.2.3", PTR: "r3.fra2.example.net"},
		{IP: "192.0.2.4", PTR: "r_4.fra3.example.net"},
		{IP: "192.0.2.5", PTR: "r5.fra_9.example.net"},
	}
	if cleanLabels(names[3].PTR, "example.net") || !cleanLabels(names[0].PTR, "example.net") {
		t.Fatal("cleanLabels")
	}
	c, ok := learnDomain("example.net", names, dict)
	if !ok || c.Scope.LabelFromRight != 1 || c.Decoded != 3 || c.Names != 3 {
		t.Fatalf("odd names blocked or leaked: %+v ok=%v", c, ok)
	}
}

func citiesZip(t *testing.T, rows [][3]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("cities1000.txt")
	for i, r := range rows { // name, country, population
		cols := make([]string, 19)
		cols[0], cols[1], cols[2], cols[8], cols[14] = fmt.Sprint(i), r[0], r[0], r[1], r[2]
		fmt.Fprintln(f, strings.Join(cols, "\t"))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestGeoNamesDominantNamesake(t *testing.T) {
	b := citiesZip(t, [][3]string{
		{"London", "GB", "8900000"}, {"London", "CA", "400000"},
		{"Springfield", "US", "150000"}, {"Springfield", "GB", "120000"},
		{"Paris", "FR", "2100000"}, {"Paris", "FR", "2100000"},
		{"Oslo", "NO", "0"}, {"Oslo", "US", "0"},
	})
	m, aliases, err := parseGeoNamesWithAliases(b)
	if err != nil {
		t.Fatal(err)
	}
	if m["london"] != (Place{"GB", "London"}) {
		t.Fatalf("dominant namesake: %+v", m["london"])
	}
	if _, ok := m["springfield"]; ok {
		t.Fatal("similar-sized namesakes must stay unresolved")
	}
	if _, ok := m["oslo"]; ok {
		t.Fatal("unknown populations must stay unresolved")
	}
	if m["paris"] != (Place{"FR", "Paris"}) {
		t.Fatal("identical places must dedupe")
	}
	if aliases[cityAlias{"CA", "london"}] != (Place{"CA", "London"}) || aliases[cityAlias{"GB", "london"}] != (Place{"GB", "London"}) {
		t.Fatalf("per-country aliases: %+v", aliases)
	}
}

func TestLearnUpToThreeExtractionsPerDomain(t *testing.T) {
	cities := map[string]string{"fra": "Frankfurt", "muc": "Munich", "ber": "Berlin", "ham": "Hamburg"}
	dict := Dictionaries{"iata": {}, "geonames": {}}
	for code, city := range cities {
		dict["iata"][code] = Place{Country: "DE", City: city}
		dict["geonames"][strings.ToLower(city)] = Place{Country: "DE", City: city}
	}
	var names []Name
	add := func(ptr string) { names = append(names, Name{IP: fmt.Sprintf("192.0.2.%d", len(names)+1), PTR: ptr}) }
	for i, c := range []string{"fra", "muc", "ber", "ham"} { // convention A: router names
		add(fmt.Sprintf("r%d.%s1.example.net", i+1, c))
	}
	for _, c := range []string{"fra", "muc", "ber"} { // B: pool names, the city is the second token
		add("pool-" + c + ".example.net")
	}
	for _, c := range []string{"frankfurt", "munich", "berlin"} { // C: full city names
		add(c + ".example.net")
	}
	for _, c := range []string{"fra", "muc", "ber"} { // D: a fourth convention, beyond the cap
		add("x." + c + "1.y.example.net")
	}
	ranked := learnDomainRanks("example.net", names, dict, nil)
	if len(ranked) != MaxRank {
		t.Fatalf("got %d ranks, want %d: %+v", len(ranked), MaxRank, ranked)
	}
	wantNames, wantDecoded := []int{13, 9, 6}, []int{4, 3, 3}
	seen := map[string]bool{}
	for i, c := range ranked {
		if c.Rank != i+1 || c.Names != wantNames[i] || c.Decoded != wantDecoded[i] || c.Scope.LabelFromRight != 1 || c.Version == "" {
			t.Errorf("rank %d: %+v", i+1, c)
		}
		if seen[c.Version] {
			t.Errorf("rank %d repeats an earlier version", i+1)
		}
		seen[c.Version] = true
	}
	if ranked[0].Dictionary != "iata" || ranked[0].Scope.TokenFromLeft != 1 || ranked[1].Dictionary != "geonames" || ranked[2].Scope.TokenFromLeft != 2 {
		t.Errorf("unexpected extractions: %+v", ranked)
	}
	// Every regex applies to every name of the domain: rank 3 decodes pool names only.
	re := regexp.MustCompile(ranked[2].Pattern)
	if m := re.FindStringSubmatch("pool-muc.example.net"); len(m) != 2 || m[1] != "muc" {
		t.Errorf("rank 3 pattern %s on a pool name: %v", ranked[2].Pattern, m)
	}
	// The first rank equals the single-rank learner.
	if one, ok := learnDomain("example.net", names, dict); !ok || one.Version != ranked[0].Version {
		t.Errorf("rank 1 differs from learnDomain: %+v", one)
	}
	// Below the floor on the remaining names there is no further rank.
	if r := learnDomainRanks("example.net", names[:9], dict, nil); len(r) != 2 {
		t.Errorf("two conventions gave %d ranks", len(r))
	}
}

func TestLearnerInputIncludesTargetsAndHops(t *testing.T) {
	if strings.Contains(inputQuery, "<> t.target") || !strings.Contains(inputQuery, "t.hop_ips") || !strings.Contains(inputQuery, "t.target") {
		t.Errorf("inputQuery: %s", inputQuery)
	}
	if strings.Contains(inputQueryNoHops, "hop_ips") || strings.Contains(inputQueryNoHops, "<> t.target") || !strings.Contains(inputQueryNoHops, "t.target") {
		t.Errorf("inputQueryNoHops: %s", inputQueryNoHops)
	}
	if !strings.Contains(inputQuery, "SELECT DISTINCT host(n.ip)") {
		t.Error("an IP must count once")
	}
}

// A sampled customer name keeps the country of its /24 (our MMDB's), and each IP counts once.
func TestCollectNameAndCountries(t *testing.T) {
	byDomain := map[string][]Name{}
	seen := map[string]bool{}
	skipped := 0
	collectName(byDomain, seen, &skipped, "5.6.7.1", "h1.nyc.cust.example.net", "")      // traced name: country later
	collectName(byDomain, seen, &skipped, "5.6.7.1", "h1.nyc.cust.example.net", "de")    // same IP again: ignored
	collectName(byDomain, seen, &skipped, "5.6.7.50", "h2.nyc.cust.example.net", " de ") // customer name
	collectName(byDomain, seen, &skipped, "10.0.0.1", "h3.nyc.cust.example.net", "DE")   // not public
	collectName(byDomain, seen, &skipped, "5.6.7.60", "h_4.nyc.cust.example.net", "DE")  // label outside [a-z0-9-]
	collectName(byDomain, seen, &skipped, "5.6.7.70", "localhost", "DE")                 // no registered domain
	names := byDomain["example.net"]
	if len(byDomain) != 1 || len(names) != 2 || skipped != 1 {
		t.Fatalf("names = %+v, skipped %d", byDomain, skipped)
	}
	if names[0].Country != "" || names[1].Country != "DE" {
		t.Errorf("countries = %q, %q", names[0].Country, names[1].Country)
	}
	var byBits [33]map[uint32]string
	byBits[16] = map[uint32]string{5<<24 | 6<<16: "US"}
	applyCountries(byDomain, byBits)
	names = byDomain["example.net"]
	if names[0].Country != "US" || names[1].Country != "DE" {
		t.Errorf("after the announced prefixes: %q, %q (the customer name must keep DE)", names[0].Country, names[1].Country)
	}
}

// The customer names count in the domestic gate with the country they carry.
func TestCustomerNamesCountInDomesticGate(t *testing.T) {
	var names []Name
	for i := 0; i < 8; i++ {
		names = append(names, Name{IP: "5.6.7.1", PTR: "x", Country: "DE"})
	}
	names = append(names, Name{IP: "5.6.7.2", PTR: "y", Country: "US"}, Name{IP: "5.6.7.3", PTR: "z", Country: "US"})
	if !isDomestic(names) {
		t.Error("80% of the known countries in one country is domestic")
	}
}

// At most customerNamesPerDomain customer names stay per domain: the smallest hashes of the IP,
// whatever the order the rows come in; a router name already seen for the IP wins.
func TestCollectCustomerKeepsSmallestHashes(t *testing.T) {
	var ips []string
	for i := 1; i <= 200; i++ {
		ips = append(ips, fmt.Sprintf("5.6.%d.%d", i/200+7, i%200+1))
	}
	collect := func(order []string) []string {
		heaps := map[string]*customerHeap{}
		seen := map[string]bool{ips[0]: true} // a router name has the first IP
		skipped := 0
		for _, ip := range order {
			collectCustomer(heaps, seen, &skipped, 50, ip, "h.nyc.cust.example.net", "de")
		}
		var kept []string
		for _, c := range *heaps["example.net"] {
			if c.Country != "DE" || c.hash != ipHash(c.IP) {
				t.Fatalf("name %+v", c)
			}
			kept = append(kept, c.IP)
		}
		sort.Strings(kept)
		return kept
	}
	forward := collect(ips)
	reversed := make([]string, len(ips))
	for i, ip := range ips {
		reversed[len(ips)-1-i] = ip
	}
	if len(forward) != 50 || !slices.Equal(forward, collect(reversed)) {
		t.Fatalf("kept %d, order dependent", len(forward))
	}
	if slices.Contains(forward, ips[0]) {
		t.Error("an IP seen as a router name was taken as a customer")
	}
	// They are the 50 smallest hashes of the others.
	rest := slices.Clone(ips[1:])
	sort.Slice(rest, func(i, j int) bool { return ipHash(rest[i]) < ipHash(rest[j]) })
	want := slices.Clone(rest[:50])
	sort.Strings(want)
	if !slices.Equal(forward, want) {
		t.Errorf("kept the wrong names")
	}
	if ipHash("a") != 0xe40c292c || ipHash("") != 2166136261 {
		t.Error("not FNV-1a")
	}
}
