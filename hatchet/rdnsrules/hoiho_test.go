package rdnsrules

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A small made-up GeoNames index. Ids are only tie-breakers.
func hoihoTestGeo(t *testing.T) *geoIndex {
	t.Helper()
	return testGeoIndex(t,
		gnRow{1, "Chicago", "", 41.85, -87.65, "US", "IL", 2700000},
		gnRow{2, "Chicago Heights", "", 41.51, -87.64, "US", "IL", 30000},
		gnRow{3, "Dallas", "", 32.78, -96.80, "US", "TX", 1300000},
		gnRow{4, "Adrian", "", 41.90, -84.04, "US", "MI", 20000},
		gnRow{5, "Adrienne", "", 43.00, -84.50, "US", "MI", 50000},
		gnRow{6, "Adrian", "", 40.70, -84.60, "US", "OH", 90000},
		gnRow{7, "New York City", "", 40.71, -74.01, "US", "NY", 8300000},
		gnRow{8, "Albany", "", 42.65, -73.75, "US", "NY", 97000},
		gnRow{9, "Nyack", "", 41.09, -73.92, "US", "NY", 7000},
		gnRow{10, "Toronto", "", 43.65, -79.38, "CA", "08", 2700000},
		gnRow{11, "Montreal", "", 45.50, -73.57, "CA", "10", 1700000},
		gnRow{12, "St. John's", "", 47.56, -52.71, "CA", "05", 100000},
		gnRow{13, "London", "", 51.51, -0.13, "GB", "ENG", 8900000},
		gnRow{14, "Manchester", "", 53.48, -2.24, "GB", "ENG", 550000},
		gnRow{15, "London", "", 42.98, -81.23, "CA", "08", 380000},
		gnRow{16, "Londrina", "", -23.31, -51.16, "BR", "18", 500000},
		gnRow{17, "Londonderry", "", 55.0, -7.32, "IE", "07", 80000},
		gnRow{18, "San Jose", "", 37.34, -121.89, "US", "CA", 1000000},
		gnRow{19, "Chicago", "", 41.9, -88.3, "US", "IL", 1000}, // a namesake, smaller, same distance class
	)
}

type testHint struct {
	code, typ string
	lat, lng  string
	tp        int
	loc       string // JSON of "location" ("" = none)
	st        string // top-level st
}

func (h testHint) json() string {
	s := fmt.Sprintf(`{"code":%q,"type":%q,"tp":%d,"fp":0,"learned":0`, h.code, h.typ, h.tp)
	if h.lat != "" {
		s += fmt.Sprintf(`,"lat":%q,"lng":%q`, h.lat, h.lng)
	}
	if h.st != "" {
		s += fmt.Sprintf(`,"st":%q`, h.st)
	}
	if h.loc != "" {
		s += `,"location":` + h.loc
	}
	return s + "}"
}

// recordLine makes one JSON line; res and plans are parallel.
func recordLine(domain string, res []string, plans [][]string, hints ...testHint) string {
	var re, plan, hs []string
	for _, r := range res {
		re = append(re, fmt.Sprintf("%q", r))
	}
	for _, p := range plans {
		var q []string
		for _, x := range p {
			q = append(q, fmt.Sprintf("%q", x))
		}
		plan = append(plan, "["+strings.Join(q, ",")+"]")
	}
	for _, h := range hints {
		hs = append(hs, h.json())
	}
	return fmt.Sprintf(`{"domain":%q,"re":[%s],"plan":[%s],"geohints":[%s],"score":{"class":"good"}}`, domain, strings.Join(re, ","), strings.Join(plan, ","), strings.Join(hs, ","))
}

func chicago(code, typ string, tp int) testHint {
	return testHint{code: code, typ: typ, lat: "41.978600", lng: "-87.904800", tp: tp, loc: `{"cc":"US","place":"Chicago","st":"IL"}`}
}

func convertLines(t *testing.T, lines ...string) ([]HoihoRow, HoihoOutput) {
	t.Helper()
	var out HoihoOutput
	recs := parseHoiho([]byte(strings.Join(lines, "\n")), &out)
	rows, tooMany := convertHoiho(recs, hoihoTestGeo(t), &out)
	if tooMany {
		t.Fatal("too many keys")
	}
	return rows, out
}

func onlyRow(t *testing.T, rows []HoihoRow) HoihoRow {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	return rows[0]
}

func wantPlaces(t *testing.T, r HoihoRow, want map[string][2]string) {
	t.Helper()
	if !reflect.DeepEqual(r.Places, want) {
		t.Errorf("places = %v, want %v", r.Places, want)
	}
}

func TestHoihoPlanShapes(t *testing.T) {
	chi := [2]string{"US", "Chicago"}
	type tc struct {
		name  string
		re    string
		plan  []string
		hints []testHint
		want  map[string][2]string
	}
	lhr := testHint{code: "lhr", typ: "iata", lat: "51.47", lng: "-0.45", tp: 3, loc: `{"cc":"GB","place":"London"}`}
	sjc := testHint{code: "sjc", typ: "facility", lat: "37.36", lng: "-121.9", tp: 2, st: "ca", loc: `{"cc":"US","place":"San Jose","st":"XX"}`}
	for _, c := range []tc{
		{"iata", `^([a-z]{3})\.example\.net$`, []string{"iata"}, []testHint{chicago("chi", "iata", 1)}, map[string][2]string{"chi": chi}},
		{"place", `^([a-z]+)\.example\.net$`, []string{"place"}, []testHint{chicago("chicago", "place", 1)}, map[string][2]string{"chicago": chi}},
		{"iata,cc", `^([a-z]{3})\.([a-z]{2})\.example\.net$`, []string{"iata", "cc"}, []testHint{chicago("chi", "iata", 1), lhr},
			map[string][2]string{"chi,us": chi, "lhr,gb": {"GB", "London"}, "lhr,uk": {"GB", "London"}}},
		{"cc,iata", `^([a-z]{2})\.([a-z]{3})\.example\.net$`, []string{"cc", "iata"}, []testHint{chicago("chi", "iata", 1)}, map[string][2]string{"us,chi": chi}},
		{"place,st", `^([a-z]+)\.([a-z]{2})\.example\.net$`, []string{"place", "st"}, []testHint{chicago("chicago", "place", 1)}, map[string][2]string{"chicago,il": chi}},
		{"st,place", `^([a-z]{2})\.([a-z]+)\.example\.net$`, []string{"st", "place"}, []testHint{chicago("chicago", "place", 1)}, map[string][2]string{"il,chicago": chi}},
		{"facility,st", `^([a-z]+)\.([a-z]{2})\.example\.net$`, []string{"facility", "st"}, []testHint{sjc}, map[string][2]string{"sjc,ca": {"US", "San Jose"}}},
		{"locode", `^([a-z]{5})\.example\.net$`, []string{"locode"}, []testHint{chicago("uschi", "locode", 1)}, map[string][2]string{"uschi": chi}},
		{"clli", `^([a-z]{6})\.example\.net$`, []string{"clli"}, []testHint{{code: "adrnmi", typ: "clli", tp: 2}}, map[string][2]string{"adrnmi": {"US", "Adrienne"}}},
		{"clli,st", `^([a-z]{6})\.([a-z]{2})\.example\.net$`, []string{"clli", "st"}, []testHint{{code: "adrnmi", typ: "clli", tp: 2}}, map[string][2]string{"adrnmi,mi": {"US", "Adrienne"}}},
		{"clli,clli", `^([a-z]{3})([a-z]{3})\.example\.net$`, []string{"clli", "clli"}, []testHint{{code: "adrnmi", typ: "clli", tp: 2}}, map[string][2]string{
			"a,drnmi": {"US", "Adrienne"}, "ad,rnmi": {"US", "Adrienne"}, "adr,nmi": {"US", "Adrienne"}, "adrn,mi": {"US", "Adrienne"}, "adrnm,i": {"US", "Adrienne"}}},
		{"clli,cc", `^([a-z]{6})\.([a-z]{2})\.example\.net$`, []string{"clli", "cc"}, []testHint{{code: "londxx", typ: "clli", tp: 1}}, map[string][2]string{
			"londxx,gb": {"GB", "London"}, "londxx,uk": {"GB", "London"}, "londxx,ca": {"CA", "London"}, "londxx,br": {"BR", "Londrina"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows, out := convertLines(t, recordLine("example.net", []string{c.re}, [][]string{c.plan}, c.hints...))
			r := onlyRow(t, rows)
			wantPlaces(t, r, c.want)
			if r.Domain != "example.net" || r.Seq != 1 || r.Pattern != c.re || r.Plan != strings.Join(c.plan, ",") || r.Class != "good" || len(r.Version) != 64 {
				t.Errorf("row = %+v", r)
			}
			if out.Rows != 1 || out.Keys != len(c.want) {
				t.Errorf("output = %+v", out)
			}
		})
	}
}

func TestHoihoSeqKeepsPositionWhenAnEarlierRegexIsSkipped(t *testing.T) {
	rows, out := convertLines(t, recordLine("example.net",
		[]string{`^([a-z]+(\.example\.net$`, `^([a-z]{3})\.example\.net$`},
		[][]string{{"iata"}, {"iata"}}, chicago("chi", "iata", 1)))
	r := onlyRow(t, rows)
	if r.Seq != 2 || out.RegexesSkipped != 1 || out.SkipReasons[skipRegexCompile] != 1 {
		t.Fatalf("row %+v output %+v", r, out)
	}
}

func TestHoihoPlacing(t *testing.T) {
	re, plan := []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}
	byName := chicago("chi", "iata", 1)
	// The name is not a GeoNames name, but Chicago is within 30 km of the coordinates.
	nearby := testHint{code: "ord", typ: "iata", lat: "41.97", lng: "-87.90", tp: 1, loc: `{"cc":"US","place":"O'Hare"}`}
	// Dallas by name, but the coordinates are far from it, and no place is within 30 km.
	far := testHint{code: "dfw", typ: "iata", lat: "35.0", lng: "-97.0", tp: 1, loc: `{"cc":"US","place":"Dallas"}`}
	badCoord := testHint{code: "bad", typ: "iata", lat: "north", lng: "-87.9", tp: 1, loc: `{"cc":"US","place":"Chicago"}`}
	badRange := testHint{code: "rng", typ: "iata", lat: "91", lng: "-87.9", tp: 1, loc: `{"cc":"US","place":"Chicago"}`}
	badCountry := testHint{code: "xxx", typ: "iata", lat: "41.9", lng: "-87.6", tp: 1, loc: `{"cc":"ZZ","place":"Chicago"}`}
	noLoc := testHint{code: "nol", typ: "iata", tp: 1}
	rows, out := convertLines(t, recordLine("example.net", re, plan, byName, nearby, far, badCoord, badRange, badCountry, noLoc))
	wantPlaces(t, onlyRow(t, rows), map[string][2]string{"chi": {"US", "Chicago"}, "ord": {"US", "Chicago"}})
	if out.Hints != 7 || out.HintsPlacedByName != 1 || out.HintsPlacedNearby != 1 || out.HintsUnplaced != 5 {
		t.Errorf("output = %+v", out)
	}
}

func TestHoihoPlacingPrefersTheMostPopulousWithinRadius(t *testing.T) {
	// "Chicago" exists twice within 50 km of the hint (2.7M and 1,000 people): the bigger wins.
	hint := testHint{code: "chi", typ: "iata", lat: "41.88", lng: "-87.8", tp: 1, loc: `{"cc":"US","place":"Chicago"}`}
	rows, _ := convertLines(t, recordLine("example.net", []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}, hint))
	wantPlaces(t, onlyRow(t, rows), map[string][2]string{"chi": {"US", "Chicago"}})
	// Close to the small namesake only (41.9,-88.3 is 33 km from the big one, 3 km from the small one):
	// both are within 50 km, the bigger still wins.
	hint.lng = "-88.28"
	rows, _ = convertLines(t, recordLine("example.net", []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}, hint))
	wantPlaces(t, onlyRow(t, rows), map[string][2]string{"chi": {"US", "Chicago"}})
}

func TestHoihoKeyCollisionLargerTPWins(t *testing.T) {
	re, plan := []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}
	dallas := func(tp int) testHint {
		return testHint{code: "dup", typ: "iata", lat: "32.8", lng: "-96.8", tp: tp, loc: `{"cc":"US","place":"Dallas"}`}
	}
	chi := func(tp int) testHint { return chicago("dup", "iata", tp) }
	for _, tc := range []struct {
		name  string
		hints []testHint
		city  string
	}{
		{"larger tp", []testHint{chi(1), dallas(5)}, "Dallas"},
		{"larger tp, other order", []testHint{dallas(5), chi(1)}, "Dallas"},
		{"tie: smaller name", []testHint{dallas(2), chi(2)}, "Chicago"},
		{"tie: smaller name, other order", []testHint{chi(2), dallas(2)}, "Chicago"},
	} {
		rows, _ := convertLines(t, recordLine("example.net", re, plan, tc.hints...))
		r := onlyRow(t, rows)
		if len(r.Places) != 1 || r.Places["dup"][1] != tc.city {
			t.Errorf("%s: places %v", tc.name, r.Places)
		}
	}
}

func TestHoihoSkipReasons(t *testing.T) {
	hint := chicago("chi", "iata", 1)
	for _, tc := range []struct {
		name   string
		re     string
		plan   []string
		reason string
	}{
		{"compile", `^([a-z]+(\.example\.net$`, []string{"iata"}, skipRegexCompile},
		{"plan length", `^([a-z]{3})\.([a-z]{2})\.example\.net$`, []string{"iata"}, skipPlanMismatch},
		{"no location type", `^([a-z]{2})\.example\.net$`, []string{"cc"}, skipNoLocation},
		{"two location types", `^([a-z]{3})\.([a-z]+)\.example\.net$`, []string{"iata", "place"}, skipTwoLocations},
		{"location type twice", `^([a-z]{3})\.([a-z]{3})\.example\.net$`, []string{"iata", "iata"}, skipLocationTwice},
		{"clli twice but not adjacent", `^([a-z]{3})\.([a-z]{2})\.([a-z]{3})\.example\.net$`, []string{"clli", "cc", "clli"}, skipLocationTwice},
		{"two cc", `^([a-z]{3})\.([a-z]{2})\.([a-z]{2})\.example\.net$`, []string{"iata", "cc", "cc"}, skipTwoCC},
		{"two st", `^([a-z]{3})\.([a-z]{2})\.([a-z]{2})\.example\.net$`, []string{"iata", "st", "st"}, skipTwoST},
		{"unknown type", `^([a-z]{3})\.example\.net$`, []string{"icao"}, skipUnknownType},
		{"no keys", `^([a-z]{3})\.example\.net$`, []string{"place"}, skipNoKeys}, // only an iata hint exists
	} {
		_, out := convertLines(t, recordLine("example.net", []string{tc.re}, [][]string{tc.plan}, hint))
		if out.Rows != 0 || out.RegexesSkipped != 1 || out.SkipReasons[tc.reason] != 1 {
			t.Errorf("%s: output %+v", tc.name, out)
		}
	}
	// A regex too long, a missing plan.
	long := `^(` + strings.Repeat("a", hoihoMaxRegexBytes) + `)\.example\.net$`
	_, out := convertLines(t, recordLine("example.net", []string{long, `^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}, hint))
	if out.SkipReasons[skipRegexLong] != 1 || out.SkipReasons[skipPlanMismatch] != 1 || out.Rows != 0 {
		t.Errorf("long regex / missing plan: %+v", out)
	}
	// A hint without the state a plan needs is skipped for that regex only.
	noSt := testHint{code: "dal", typ: "place", lat: "32.8", lng: "-96.8", tp: 1, loc: `{"cc":"US","place":"Dallas"}`}
	_, out = convertLines(t, recordLine("example.net", []string{`^([a-z]+)\.([a-z]{2})\.example\.net$`}, [][]string{{"place", "st"}}, noSt))
	if out.SkipReasons[skipHintNoState] != 1 || out.SkipReasons[skipNoKeys] != 1 {
		t.Errorf("missing state: %+v", out)
	}
	// A hint with no state is counted once, however many regexes need it.
	_, out = convertLines(t, recordLine("example.net", []string{`^([a-z]+)\.([a-z]{2})\.example\.net$`, `^([a-z]+)-([a-z]{2})\.example\.net$`}, [][]string{{"place", "st"}, {"place", "st"}}, noSt))
	if out.SkipReasons[skipHintNoState] != 1 || out.SkipReasons[skipNoKeys] != 2 {
		t.Errorf("missing state counted per regex: %+v", out)
	}
	// A bad hint code.
	_, out = convertLines(t, recordLine("example.net", []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}, chicago("c h i", "iata", 1)))
	if out.SkipReasons[skipHintBadCode] != 1 || out.Rows != 0 {
		t.Errorf("bad code: %+v", out)
	}
}

func TestHoihoFileLimits(t *testing.T) {
	good := recordLine("example.net", []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}, chicago("chi", "iata", 1))
	re33 := make([]string, 33)
	plan33 := make([][]string, 33)
	for i := range re33 {
		re33[i], plan33[i] = `^([a-z]{3})\.example\.net$`, []string{"iata"}
	}
	hugeLine := `{"domain":"huge.example.net","x":"` + strings.Repeat("a", hoihoMaxLine) + `"}`
	manyHints := recordLine("hints.example.net", nil, nil)
	manyHints = strings.Replace(manyHints, `"geohints":[]`, `"geohints":[`+strings.Repeat(`{"code":"a","type":"iata"},`, hoihoMaxHints)+`{"code":"a","type":"iata"}]`, 1)
	lines := []string{
		good,
		"not json",
		"",
		recordLine("Bad_Domain.example.net", []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}),
		recordLine("nodot", nil, nil),
		recordLine(strings.Repeat("a", 250)+".net", nil, nil),
		good, // the same domain twice
		recordLine("many.example.net", re33, plan33),
		manyHints,
		hugeLine,
	}
	var out HoihoOutput
	recs := parseHoiho([]byte(strings.Join(lines, "\r\n")), &out)
	if len(recs) != 1 || recs[0].Domain != "example.net" {
		t.Fatalf("records = %d", len(recs))
	}
	want := map[string]int{skipBadJSON: 1, skipBadDomain: 3, skipDuplicateDom: 1, skipTooManyRegexes: 1, skipTooManyHints: 1, skipLongLine: 1}
	if !reflect.DeepEqual(out.SkipReasons, want) || out.RecordsSkipped != 8 {
		t.Errorf("reasons %v, skipped %d", out.SkipReasons, out.RecordsSkipped)
	}
	// At most hoihoMaxRecords records.
	var many []string
	for i := 0; i < hoihoMaxRecords+3; i++ {
		many = append(many, fmt.Sprintf(`{"domain":"d%d.example.net","re":[],"plan":[],"geohints":[]}`, i))
	}
	out = HoihoOutput{}
	if recs = parseHoiho([]byte(strings.Join(many, "\n")), &out); len(recs) != hoihoMaxRecords || out.SkipReasons[skipTooManyRecords] != 3 {
		t.Errorf("records %d, reasons %v", len(recs), out.SkipReasons)
	}
}

func TestHoihoCLLIGuess(t *testing.T) {
	re := func(plan ...string) ([]string, [][]string) {
		return []string{`^([a-z]{6})\.example\.net$`}, [][]string{plan}
	}
	guess := func(code string) (HoihoRow, HoihoOutput) {
		res, plans := re("clli")
		rows, out := convertLines(t, recordLine("example.net", res, plans, testHint{code: code, typ: "clli", tp: 1}))
		if len(rows) == 0 {
			return HoihoRow{}, out
		}
		return rows[0], out
	}
	for _, tc := range []struct {
		code, country, city string
	}{
		{"adrnmi", "US", "Adrienne"}, // the most populous of the abbreviated places in Michigan
		{"adrnoh", "US", "Adrian"},   // the state filter
		{"albyny", "US", "Albany"},
		{"nycmny", "US", "New York City"}, // the known codes
		{"nyckny", "US", "New York City"},
		{"nycrny", "US", "New York City"},
		{"torton", "CA", "Toronto"},
		{"mtrlpq", "CA", "Montreal"}, // PQ is Quebec
		{"mtrlqc", "CA", "Montreal"},
		{"stjonf", "CA", "St. John's"}, // NF is Newfoundland and Labrador
		{"stjonl", "CA", "St. John's"},
	} {
		r, _ := guess(tc.code)
		got, ok := r.Places[tc.code]
		if !ok || got != [2]string{tc.country, tc.city} {
			t.Errorf("%s = %v ok=%v, want %s %s", tc.code, got, ok, tc.country, tc.city)
		}
	}
	for _, code := range []string{
		"rienmi",  // the first letter of the place is not the first letter of the code
		"nycmtx",  // New York City codes only count in NY
		"nyaxny",  // no place of New York abbreviates nyax
		"adrnzz",  // not a state or province
		"adrnmin", // not six letters
		"adrn1i",
	} {
		if r, _ := guess(code); len(r.Places) != 0 {
			t.Errorf("%s gave %v", code, r.Places)
		}
	}
	// A subsequence, not a prefix: nyck would need the letters in order (Nyack: n y a c k).
	if r, _ := guess("nyckmi"); len(r.Places) != 0 {
		t.Errorf("nyckmi gave %v", r.Places)
	}
	_, out := guess("nycmny")
	if out.CLLIHints != 1 || out.CLLIGuessed != 1 || out.CLLIKnownNYC != 1 {
		t.Errorf("nyc output %+v", out)
	}
	_, out = guess("adrnzz")
	if out.CLLINoRegion != 1 || out.CLLIGuessed != 0 {
		t.Errorf("no region output %+v", out)
	}
	_, out = guess("nyaxny")
	if out.CLLINoCandidate != 1 {
		t.Errorf("no candidate output %+v", out)
	}
}

func TestHoihoCLLIWithCountryPlan(t *testing.T) {
	res, plans := []string{`^([a-z]{6})\.([a-z]{2})\.example\.net$`}, [][]string{{"clli", "cc"}}
	rows, _ := convertLines(t, recordLine("example.net", res, plans, testHint{code: "adrnmi", typ: "clli", tp: 1}))
	// The state decides "us"; no other country has a place of 100,000 that adrn abbreviates.
	wantPlaces(t, onlyRow(t, rows), map[string][2]string{"adrnmi,us": {"US", "Adrienne"}})
	// A US code also gets the other countries: London (Canada) is not in Michigan, but "lond" with a US
	// state is read as US first and Canada second.
	rows, _ = convertLines(t, recordLine("example.net", res, plans, testHint{code: "londmi", typ: "clli", tp: 1}))
	wantPlaces(t, onlyRow(t, rows), map[string][2]string{"londmi,gb": {"GB", "London"}, "londmi,uk": {"GB", "London"}, "londmi,ca": {"CA", "London"}, "londmi,br": {"BR", "Londrina"}})
	// The 100,000 floor: Londonderry (80,000) never appears, and a Canadian province code decides "ca".
	rows, _ = convertLines(t, recordLine("example.net", res, plans, testHint{code: "london", typ: "clli", tp: 1}))
	wantPlaces(t, onlyRow(t, rows), map[string][2]string{"london,ca": {"CA", "London"}, "london,gb": {"GB", "London"}, "london,uk": {"GB", "London"}, "london,br": {"BR", "Londrina"}})
	// A plan with cc and st: the st part is the region code.
	res, plans = []string{`^([a-z]{6})\.([a-z]{2})\.([a-z]{2})\.example\.net$`}, [][]string{{"clli", "cc", "st"}}
	rows, _ = convertLines(t, recordLine("example.net", res, plans, testHint{code: "adrnmi", typ: "clli", tp: 1}))
	wantPlaces(t, onlyRow(t, rows), map[string][2]string{"adrnmi,us,mi": {"US", "Adrienne"}})
}

func TestHoihoConversionIsDeterministic(t *testing.T) {
	lines := []string{
		recordLine("b.example.net", []string{`^([a-z]{3})\.([a-z]{2})\.b\.example\.net$`}, [][]string{{"iata", "cc"}}, chicago("chi", "iata", 1), chicago("ord", "iata", 2)),
		recordLine("a.example.net", []string{`^([a-z]{6})\.([a-z]{2})\.a\.example\.net$`}, [][]string{{"clli", "cc"}},
			testHint{code: "londxx", typ: "clli", tp: 1}, testHint{code: "adrnmi", typ: "clli", tp: 1}),
	}
	first, _ := convertLines(t, lines...)
	second, _ := convertLines(t, lines...)
	reversed, _ := convertLines(t, lines[1], lines[0])
	if len(first) != 2 || first[0].Domain != "a.example.net" || !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first, reversed) {
		t.Fatalf("rows differ:\n%+v\n%+v\n%+v", first, second, reversed)
	}
	// The version follows the content.
	changed, _ := convertLines(t, strings.Replace(lines[0], "ord", "orx", 1), lines[1])
	if changed[0].Version != first[0].Version || changed[1].Version == first[1].Version {
		t.Errorf("versions: %s %s / %s %s", first[0].Version, first[1].Version, changed[0].Version, changed[1].Version)
	}
}

func TestHoihoClassIsStored(t *testing.T) {
	line := strings.Replace(recordLine("example.net", []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}, chicago("chi", "iata", 1)), `"good"`, `"Promising"`, 1)
	if r := onlyRow(t, mustRows(t, line)); r.Class != "promising" {
		t.Errorf("class %q", r.Class)
	}
}

func mustRows(t *testing.T, lines ...string) []HoihoRow {
	rows, _ := convertLines(t, lines...)
	return rows
}

// ---- the import without a database ----

func TestImportHoihoSkipsWithFixedReasons(t *testing.T) {
	ctx := context.Background()
	g := hoihoTestGeo(t)
	small := []byte(recordLine("example.net", []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}, chicago("chi", "iata", 1)))
	p := &Pipeline{DataDir: t.TempDir(), PGSQL: "postgres://unused.invalid/db", testHoiho: small, testGeo: g}
	if out, err := p.ImportHoiho(ctx); err != nil || out.Skipped != "too few records" || out.Written {
		t.Fatalf("too few records: %+v %v", out, err)
	}
	if _, err := (&Pipeline{}).ImportHoiho(ctx); err == nil {
		t.Error("no PGSQL is not an error")
	}
	// An unreachable file and unreachable GeoNames only skip.
	fail := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })}
	p = &Pipeline{DataDir: t.TempDir(), PGSQL: "postgres://unused.invalid/db", client: fail, now: time.Now}
	if out, err := p.ImportHoiho(ctx); err != nil || out.Skipped != "file unavailable" {
		t.Fatalf("file: %+v %v", out, err)
	}
	p = &Pipeline{DataDir: t.TempDir(), PGSQL: "postgres://unused.invalid/db", client: fail, now: time.Now, testHoiho: []byte(hoihoRecords(hoihoMinRecords, true))}
	if out, err := p.ImportHoiho(ctx); err != nil || out.Skipped != "GeoNames unavailable" {
		t.Fatalf("geonames: %+v %v", out, err)
	}
}

// manyKeys makes a record whose one regex (clli,clli) gets 11 keys for each of n hints.
func manyKeysLine(domain string, n int) string {
	var hs []testHint
	for i := 0; i < n; i++ {
		hs = append(hs, testHint{code: fmt.Sprintf("%012d", i), typ: "clli", lat: "41.9", lng: "-87.6", tp: 1, loc: `{"cc":"US","place":"Chicago"}`})
	}
	return recordLine(domain, []string{`^([a-z]{6})([a-z]{6})\.example\.net$`}, [][]string{{"clli", "clli"}}, hs...)
}

func TestHoihoKeyCaps(t *testing.T) {
	// A regex over hoihoMaxRowKeys is skipped; the others of the record stay.
	line := manyKeysLine("big.example.net", 4600) // 4600 * 11 = 50,600 keys
	small := recordLine("small.example.net", []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}, chicago("chi", "iata", 1))
	rows, out := convertLines(t, line, small)
	if len(rows) != 1 || rows[0].Domain != "small.example.net" || out.SkipReasons[skipTooManyKeys] != 1 || out.RegexesSkipped != 1 {
		t.Fatalf("row cap: %d rows, %+v", len(rows), out)
	}
	if rows, out = convertLines(t, manyKeysLine("ok.example.net", 4500)); len(rows) != 1 || out.Keys != 49500 { // just under the cap
		t.Fatalf("under the cap: %d rows, %+v", len(rows), out)
	}
	// The whole conversion stops above hoihoMaxKeys and returns no rows.
	var lines []string
	for i := 0; i < 11; i++ {
		lines = append(lines, manyKeysLine(fmt.Sprintf("d%d.example.net", i), 4500))
	}
	var o HoihoOutput
	recs := parseHoiho([]byte(strings.Join(lines, "\n")), &o)
	if rows, tooMany := convertHoiho(recs, hoihoTestGeo(t), &o); !tooMany || rows != nil {
		t.Fatalf("total cap: %d rows, tooMany=%v", len(rows), tooMany)
	}
}

// A file that only changes score.class is not unchanged.
func TestHoihoVersionIncludesClass(t *testing.T) {
	line := recordLine("example.net", []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}, chicago("chi", "iata", 1))
	a := onlyRow(t, mustRows(t, line))
	b := onlyRow(t, mustRows(t, strings.Replace(line, `"good"`, `"promising"`, 1)))
	if a.Version == b.Version || a.Class == b.Class {
		t.Errorf("class not in the version: %s %s", a.Version, b.Version)
	}
}

// fileClient serves body for every request.
func fileClient(body string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, ContentLength: int64(len(body))}, nil
	})}
}

// hoihoRecords makes n records with a hint no GeoNames place matches (so they give no rows).
func hoihoRecords(n int, placed bool) string {
	var lines []string
	for i := 0; i < n; i++ {
		h := chicago("chi", "iata", 1)
		if !placed {
			h.loc = `{"cc":"ZZ","place":"Nowhere"}`
		}
		lines = append(lines, recordLine(fmt.Sprintf("d%d.example.net", i), []string{`^([a-z]{3})\.example\.net$`}, [][]string{{"iata"}}, h))
	}
	return strings.Join(lines, "\n")
}

// The cached file is removed only when the file is the problem.
func TestImportHoihoDropsTheFileOnlyWhenItIsTheProblem(t *testing.T) {
	ctx := context.Background()
	g := hoihoTestGeo(t)
	run := func(body string, geo *geoIndex) (string, string, HoihoOutput) {
		dir := t.TempDir()
		p := &Pipeline{DataDir: dir, PGSQL: "postgres://unused.invalid/db", client: fileClient(body), now: time.Now, testGeo: geo}
		out, err := p.ImportHoiho(ctx)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "rdnsrules", "dictionaries", hoihoCacheName)
		_, errFile := os.Stat(path)
		_, errStamp := os.Stat(path + ".checked")
		return map[bool]string{true: "kept", false: "removed"}[errFile == nil], map[bool]string{true: "stamped", false: "unstamped"}[errStamp == nil], out
	}
	// Too few records: the file is the problem.
	if file, _, out := run(hoihoRecords(hoihoMinRecords-1, true), g); out.Skipped != "too few records" || file != "removed" {
		t.Errorf("too few records: %s %+v", file, out)
	}
	// Enough records but no rows (the GeoNames index does not know the places): the file stays, no stamp.
	if file, stamp, out := run(hoihoRecords(hoihoMinRecords, false), g); out.Skipped != "too few rows" || file != "kept" || stamp != "unstamped" {
		t.Errorf("too few rows: %s %s %+v", file, stamp, out)
	}
	// Too large: the file is the problem.
	if file, _, out := run(strings.Repeat("x", hoihoMaxFile+1), g); out.Skipped != "file unavailable" && out.Skipped != "file too large" || file != "removed" {
		t.Errorf("too large: %s %+v", file, out)
	}
	// Too many keys: the file is the problem.
	var lines []string
	for i := 0; i < 11; i++ {
		lines = append(lines, manyKeysLine(fmt.Sprintf("d%d.example.net", i), 4500))
	}
	for i := 0; i < hoihoMinRecords; i++ {
		lines = append(lines, fmt.Sprintf(`{"domain":"f%d.example.net","re":[],"plan":[],"geohints":[]}`, i))
	}
	if file, _, out := run(strings.Join(lines, "\n"), g); out.Skipped != "too many keys" || file != "removed" {
		t.Errorf("too many keys: %s %+v", file, out)
	}
}
