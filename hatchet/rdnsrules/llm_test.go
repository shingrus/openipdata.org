package rdnsrules

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fixtures ----

type gnRow struct {
	id              int64
	name, alt       string
	lat, lon        float64
	country, admin1 string
	pop             int64
}

var testGeoRows = []gnRow{
	{1, "Frankfurt am Main", "Frankfurt,Francfort", 50.11, 8.68, "DE", "05", 700000},
	{2, "Frankfurt (Oder)", "Frankfurt", 52.34, 14.55, "DE", "11", 58000},
	{3, "London", "", 51.51, -0.13, "GB", "ENG", 8900000},
	{4, "London", "", 42.98, -81.23, "CA", "08", 380000},
	{5, "Springfield", "", 39.80, -89.64, "US", "IL", 116000},
	{6, "Springfield", "", 37.21, -93.29, "US", "MO", 167000},
	{7, "Istanbul", "Istanbul", 41.01, 28.95, "TR", "34", 15000000},
	{8, "Esenyurt", "", 41.03, 28.67, "TR", "34", 700000},
	{9, "Flint", "", 43.01, -83.69, "US", "MI", 100000},
	{10, "Amsterdam", "", 52.37, 4.89, "NL", "07", 800000},
	{11, "Munich", "Muenchen,Munchen", 48.14, 11.58, "DE", "02", 1500000},
	{12, "Paris", "", 48.85, 2.35, "FR", "11", 2100000},
	{13, "Berlin", "", 52.52, 13.41, "DE", "16", 3600000},
	{14, "Hamburg", "", 53.55, 9.99, "DE", "04", 1800000},
	{15, "Lyon", "", 45.76, 4.84, "FR", "84", 500000},
}

const testAdmin1 = "DE.05\tHesse\tHesse\t1\nDE.11\tBrandenburg\tBrandenburg\t2\nGB.ENG\tEngland\tEngland\t3\nCA.08\tOntario\tOntario\t4\nUS.IL\tIllinois\tIllinois\t5\nUS.MO\tMissouri\tMissouri\t6\n"

func testGeoIndex(t *testing.T, rows ...gnRow) *geoIndex {
	t.Helper()
	if len(rows) == 0 {
		rows = testGeoRows
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("cities1000.txt")
	for _, r := range rows {
		cols := make([]string, 19)
		cols[0], cols[1], cols[2], cols[3] = fmt.Sprint(r.id), r.name, strings.NewReplacer("(", "", ")", "").Replace(r.name), r.alt
		cols[4], cols[5] = fmt.Sprint(r.lat), fmt.Sprint(r.lon)
		cols[8], cols[10], cols[14] = r.country, r.admin1, fmt.Sprint(r.pop)
		fmt.Fprintln(f, strings.Join(cols, "\t"))
	}
	_ = zw.Close()
	g, err := parseGeoIndex(buf.Bytes(), []byte(testAdmin1))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// ---- privacy ----

var ipEncodings = []string{
	"%[1]d.%[2]d.%[3]d.%[4]d.pool.dyn.example.net",
	"%[1]d-%[2]d-%[3]d-%[4]d.dyn.example.net",
	"ip-%[1]d-%[2]d-%[3]d-%[4]d.example.net",
	"pool-%[1]d-%[2]d-%[3]d-%[4]d.nycmny.example.net",
	"%[4]d-%[3]d-%[2]d-%[1]d.in-addr.example.net",
	"syn-%03[1]d-%03[2]d-%03[3]d-%03[4]d.res.example.net",
	"%[1]d-%[2]d-%[3]d-%[4]d.static.fra.example.net",
	"host%[1]d.%[2]d.%[3]d.%[4]d.example.net",
	"p%02[5]x%02[6]x%02[7]x%02[8]x.dip0.example.net", // hex, with digits or letters
	"%02[5]x%02[6]x%02[7]x%02[8]x.addr.example.net",
	"%[5]x%02[6]x%02[7]x%02[8]x.nolead.example.net", // the leading zero of the first octet dropped: 7 hex characters for 10.x.x.x
	"cpe-%02[5]x%02[6]x%02[7]x%02[8]x-fra.example.net",
	"dsl%[1]d-%[2]d-%[3]d-%[4]d.example.net",
}

// No octet of any address survives in a pattern, in decimal or hex, and the regex a model
// writes for the placeholders matches the original names.
func TestPatternsNeverCarryAnAddress(t *testing.T) {
	ips := [][4]int{{71, 183, 45, 12}, {171, 205, 239, 171}, {87, 129, 61, 99}, {203, 113, 7, 250}, {10, 1, 2, 3}, {172, 16, 254, 33}, {192, 0, 2, 200}, {58, 14, 9, 188}, {10, 187, 204, 221}, {12, 171, 205, 239}}
	for _, ip := range ips {
		for i, enc := range ipEncodings {
			name := fmt.Sprintf(enc, ip[0], ip[1], ip[2], ip[3], ip[0], ip[1], ip[2], ip[3])
			name = strings.NewReplacer("%!", "").Replace(name)
			pat := patternOf(name, "example.net")
			if pat == "" {
				t.Fatalf("no pattern for %s", name)
			}
			left := strings.TrimSuffix(pat, ".example.net")
			// Only the placeholder digit 0 may be left of the domain.
			for _, r := range left {
				if r >= '1' && r <= '9' {
					t.Errorf("encoding %d: %q keeps a digit: %q", i, name, pat)
				}
			}
			hex8 := fmt.Sprintf("%02x%02x%02x%02x", ip[0], ip[1], ip[2], ip[3])
			if strings.Contains(left, hex8) {
				t.Errorf("hex address survives: %q -> %q", name, pat)
			}
			for _, o := range ip {
				for _, s := range []string{fmt.Sprintf("%02x", o)} {
					// An octet written in hex with a digit would show up next to other hex runs.
					if strings.ContainsAny(s, "0123456789") && len(s) == 2 && strings.Contains(left, s) && s != "00" {
						t.Errorf("hex octet %s survives: %q -> %q", s, name, pat)
					}
				}
			}
			// The model-side regex of the pattern matches the real name.
			if !patternRegexp(pat, "example.net").MatchString(name) {
				t.Errorf("placeholder regex of %q does not match %q", pat, name)
			}
		}
	}
	// An address written in hex without any digit (171.205.239.171 = abcdefab) is hidden too.
	if got := patternOf("abcdefab.addr.example.net", "example.net"); got != "x.addr.example.net" {
		t.Errorf("letters-only hex: %q", got)
	}
	// 10.187.204.221 is 0abbccdd; written without the leading zero it is 7 letters.
	if got := patternOf("abbccdd.addr.example.net", "example.net"); got != "x.addr.example.net" {
		t.Errorf("7-character hex: %q", got)
	}
}

// patternRegexp is the regex the prompt tells the model to write for a pattern: 0 is [0-9]+ and x is [0-9a-f]+.
func patternRegexp(pattern, domain string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range strings.TrimSuffix(pattern, "."+domain) {
		switch r {
		case '0':
			b.WriteString("[0-9]+")
		case 'x':
			b.WriteString("[0-9a-f]+")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(regexp.QuoteMeta("." + domain))
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func TestRedactLabelExamples(t *testing.T) {
	for in, want := range map[string]string{
		"p57ba8141":           "px",
		"pool-71-183-45-12":   "pool-0-0-0-0",
		"dip0":                "dip0",
		"xe-0-0-0":            "xe-0-0-0",
		"core1":               "core0",
		"decade":              "decade", // 6 hex letters, no digit: a word
		"facade00":            "x",      // exactly 8 hex characters
		"abbccdd":             "x",      // 7 hex letters: 10.187.204.221 with the leading zero dropped
		"defaced":             "x",      // a word that is 7 hex letters goes too (the price of the rule)
		"cafe":                "cafe",
		"ae12":                "ae0",
		"b7c9e1a2f3":          "x",
		"syn-072-129-061-099": "syn-0-0-0-0",
	} {
		if got := redactLabel(in); got != want {
			t.Errorf("redactLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if got := patternOf("p57ba8141.dip0.t-ipconnect.de", "t-ipconnect.de"); got != "px.dip0.t-ipconnect.de" {
		t.Errorf("pattern = %q", got)
	}
}

func TestTopPatternsDedupeAndLimit(t *testing.T) {
	var ptrs []string
	for i := 1; i <= 50; i++ {
		ptrs = append(ptrs, fmt.Sprintf("pool-%d-1-1-1.nyc.example.net", i)) // one pattern, 50 times
	}
	ptrs = append(ptrs, "core1.lnd.example.net", "core2.lnd.example.net", "gw.sjc.example.net")
	got := topPatterns(ptrs, "example.net", 2)
	if len(got) != 2 || got[0] != "pool-0-0-0-0.nyc.example.net" || got[1] != "core0.lnd.example.net" {
		t.Fatalf("patterns = %v", got)
	}
	if patternsHash([]string{"a", "b"}) != patternsHash([]string{"b", "a"}) || patternsHash([]string{"a"}) == patternsHash([]string{"b"}) {
		t.Fatal("hash is not an order-free set hash")
	}
}

// ---- geo ----

func TestGeoResolve(t *testing.T) {
	g := testGeoIndex(t)
	for _, tc := range []struct {
		country, region, city string
		wantID                int64
		wantWhy               string
	}{
		{"DE", "Hesse", "Frankfurt", 1, ""}, // region picks the one in Hesse
		{"DE", "Brandenburg", "Frankfurt", 2, ""},
		{"DE", "", "Frankfurt", 1, ""}, // no region: 700k vs 58k is 10x
		{"DE", "", "Frankfurt am Main", 1, ""},
		{"DE", "", "München", 11, ""},                  // accents and alternate names
		{"US", "", "Springfield", 0, geoAmbiguousCity}, // two namesakes of similar size
		{"US", "Illinois", "Springfield", 5, ""},
		{"US", "IL", "Springfield", 5, ""},                  // the code works too
		{"US", "Texas", "Springfield", 0, geoAmbiguousCity}, // a region that matches nothing falls back to the namesake rule
		{"GB", "", "London", 3, ""},
		{"CA", "", "London", 4, ""},
		{"FR", "", "Atlantis", 0, geoUnknownCity},
		{"ZZ", "", "Paris", 0, geoUnknownCountry},
	} {
		p, why := g.resolve(tc.country, tc.region, tc.city)
		if why != tc.wantWhy || (p != nil && p.ID != tc.wantID) || (p == nil) != (tc.wantWhy != "") {
			t.Errorf("%v: got %v %q", tc, p, why)
		}
	}
	if p, _ := g.resolve("DE", "", "Frankfurt"); p == nil || p.Name != "Frankfurt am Main" {
		t.Fatalf("stored name = %+v", p)
	}
}

func TestHaversineAndSameArea(t *testing.T) {
	g := testGeoIndex(t)
	d := haversineKm(51.51, -0.13, 48.85, 2.35) // London - Paris
	if d < 330 || d > 350 {
		t.Fatalf("London-Paris = %.1f km", d)
	}
	ist, _ := g.resolve("TR", "", "Istanbul")
	esn, _ := g.resolve("TR", "", "Esenyurt")
	fra, _ := g.resolve("DE", "", "Frankfurt am Main")
	muc, _ := g.resolve("DE", "", "Munich")
	if !sameArea(ist, esn, 100) {
		t.Error("Esenyurt is within 100 km of Istanbul")
	}
	if sameArea(fra, muc, 100) {
		t.Error("Frankfurt and Munich are 300 km apart")
	}
	if sameArea(fra, nil, 100) {
		t.Error("nil place")
	}
}

// ---- answer checks ----

func names(t *testing.T, domain string, country string, ptrs ...string) []Name {
	var out []Name
	for i, p := range ptrs {
		out = append(out, Name{IP: fmt.Sprintf("198.51.100.%d", i+1), PTR: p + "." + domain, Country: country})
	}
	return out
}

func cityAnswer(regex string, codes map[string][3]string) llmAnswer {
	a := llmAnswer{Location: true, Regex: regex, Codes: map[string]llmCode{}}
	for t, c := range codes {
		a.Codes[t] = llmCode{Country: c[0], Region: c[1], City: c[2]}
	}
	return a
}

func TestCheckAnswer(t *testing.T) {
	g := testGeoIndex(t)
	dom := "example.net"
	ns := names(t, dom, "", "r1.fra", "r2.fra", "r3.ams", "r4.muc", "r5.ams", "r6.xyz")
	codes := map[string][3]string{"fra": {"DE", "Hesse", "Frankfurt"}, "ams": {"NL", "", "Amsterdam"}, "muc": {"DE", "", "Munich"}}
	good := `^[a-z0-9-]+\.([a-z]+)\.example\.net$`
	rule, why := checkAnswer(dom, cityAnswer(good, codes), ns, false, g)
	if rule == nil || why != "" {
		t.Fatalf("good answer: %q", why)
	}
	if rule.Places["fra"] != (Place{"DE", "Frankfurt am Main"}) || rule.Codes["fra"].GeoNameID != 1 || rule.Codes["fra"].Origin != "llm" {
		t.Fatalf("codes = %+v %+v", rule.Places, rule.Codes)
	}
	if rule.Decoded != 5 || rule.Names != 6 || rule.Distinct != 3 {
		t.Fatalf("evidence = %+v", rule)
	}
	// (?i) in front is fine; a regex without anchoring, domain end or with 2 groups is not.
	if _, why := checkAnswer(dom, cityAnswer("(?i)"+good, codes), ns, false, g); why != "" {
		t.Errorf("(?i) prefix: %q", why)
	}
	for name, tc := range map[string]struct{ regex, why string }{
		"no regex":      {"", rejNoRegex},
		"lookahead":     {`^(?=r)([a-z]+)\.example\.net$`, rejRegexCompile},
		"backreference": {`^([a-z]+)\1\.example\.net$`, rejRegexCompile},
		"two groups":    {`^([a-z0-9-]+)\.([a-z]+)\.example\.net$`, rejRegexGroups},
		"no group":      {`^[a-z0-9-]+\.[a-z]+\.example\.net$`, rejRegexGroups},
		"unanchored":    {`([a-z]+)\.example\.net$`, rejRegexAnchor},
		"no domain end": {`^[a-z0-9-]+\.([a-z]+)\.[a-z]+\.net$`, rejRegexAnchor},
		"digits":        {`^[a-z0-9-]+\.([a-z0-9]+)\.example\.net$`, ""}, // fine here: no name captures a digit
	} {
		_, got := checkAnswer(dom, cityAnswer(tc.regex, codes), ns, false, g)
		if got != tc.why {
			t.Errorf("%s: reason %q, want %q", name, got, tc.why)
		}
	}
	// A capture with a digit on a real name rejects the answer.
	withDigit := append(names(t, dom, "", "r1.fra1", "r2.ams2", "r3.muc3"), ns...)
	if _, got := checkAnswer(dom, cityAnswer(`^[a-z0-9-]+\.([a-z0-9]+)\.example\.net$`, codes), withDigit, false, g); got != rejNonLetter {
		t.Errorf("non-letter capture: %q", got)
	}
	// Too few distinct tokens, too few decoded names.
	if _, got := checkAnswer(dom, cityAnswer(good, map[string][3]string{"fra": codes["fra"]}), ns, false, g); got != rejFewTokens {
		t.Errorf("one token: %q", got)
	}
	many := names(t, dom, "", "r1.fra", "r2.ams", "r3.a", "r4.b", "r5.c", "r6.d", "r7.e", "r8.f", "r9.g", "r10.h")
	if r, got := checkAnswer(dom, cityAnswer(good, codes), many, false, g); r == nil || got != "" {
		t.Errorf("2 of 10 decoded is exactly 20%%: reason %q", got)
	}
	if _, got := checkAnswer(dom, cityAnswer(good, codes), append(many, names(t, dom, "", "r11.i")...), false, g); got != rejLowDecoded {
		t.Errorf("2 of 11 decoded: reason %q", got)
	}
	// An unresolvable (ambiguous) city drops only that code; a bad country rejects.
	amb := map[string][3]string{"fra": codes["fra"], "ams": codes["ams"], "spr": {"US", "", "Springfield"}}
	r2, got := checkAnswer(dom, cityAnswer(good, amb), ns, false, g)
	if r2 == nil || got != "" || r2.Dropped["spr"] != geoAmbiguousCity {
		t.Errorf("ambiguous code: %v %q %v", r2, got, r2)
	}
	bad := map[string][3]string{"fra": codes["fra"], "ams": {"Germany", "", "Berlin"}}
	if _, got := checkAnswer(dom, cityAnswer(good, bad), ns, false, g); got != rejBadCountry {
		t.Errorf("bad country: %q", got)
	}
	if _, got := checkAnswer(dom, cityAnswer(good, map[string][3]string{"zzz": {"FR", "", "Atlantis"}}), ns, false, g); got != rejNoCodes {
		t.Errorf("nothing resolves: %q", got)
	}
	if _, got := checkAnswer(dom, llmAnswer{Location: false}, ns, false, g); got != "no_city" {
		t.Errorf("no location: %q", got)
	}
}

func TestCheckAnswerSmallDomainUsesAnnouncedCountry(t *testing.T) {
	g := testGeoIndex(t)
	dom := "tiny.de"
	ns := names(t, dom, "DE", "gw.muc")
	regex := `^[a-z0-9-]+\.([a-z]+)\.tiny\.de$`
	codes := map[string][3]string{"muc": {"DE", "", "Munich"}}
	if r, why := checkAnswer(dom, cityAnswer(regex, codes), ns, true, g); r == nil || why != "" {
		t.Fatalf("one decoded name of a small domain: %q", why)
	}
	if _, why := checkAnswer(dom, cityAnswer(regex, map[string][3]string{"muc": {"FR", "", "Paris"}}), names(t, dom, "DE", "gw.muc"), true, g); why != rejSmallCountry {
		t.Fatalf("country mismatch: %q", why)
	}
	if _, why := checkAnswer(dom, cityAnswer(regex, codes), names(t, dom, "DE", "gw.zzz"), true, g); why != rejSmallNoDecode {
		t.Fatalf("no name decoded: %q", why)
	}
}

func TestCheckAnswerDomesticGate(t *testing.T) {
	g := testGeoIndex(t)
	dom := "isp.fr"
	// Routers all announced in FR; a rule that places most of them in Germany fails the gate.
	ns := names(t, dom, "FR", "a.par", "b.par", "c.lyo", "d.lyo", "e.xyz", "f.xyz", "g.xyz", "h.xyz")
	regex := `^[a-z0-9-]+\.([a-z]+)\.isp\.fr$`
	if _, why := checkAnswer(dom, cityAnswer(regex, map[string][3]string{"par": {"DE", "", "Berlin"}, "lyo": {"DE", "", "Hamburg"}}), ns, false, g); why != rejDomesticGate {
		t.Fatalf("gate: %q", why)
	}
	if r, why := checkAnswer(dom, cityAnswer(regex, map[string][3]string{"par": {"FR", "", "Paris"}, "lyo": {"FR", "", "Lyon"}}), ns, false, g); r == nil || why != "" {
		t.Fatalf("domestic and agreeing: %q", why)
	}
}

// ---- eligibility and packing ----

func TestNeedsAsking(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	info := &domainInfo{Hash: "h2"}
	mk := func(hash string, ago time.Duration, versions ...string) lastAnswer {
		return lastAnswer{Hash: hash, AskedAt: now.Add(-ago), RuleVersions: versions, HasComplete: true}
	}
	old := mk("h1", 31*24*time.Hour, "v1")
	recent := mk("h1", 5*24*time.Hour, "v1")
	same := mk("h2", 400*24*time.Hour, "v1")
	incompleteNewest := same
	incompleteNewest.NewestIncomplete = true
	onlyIncomplete := lastAnswer{NewestIncomplete: true}
	for name, tc := range map[string]struct {
		last    *lastAnswer
		learned []string
		force   bool
		want    bool
	}{
		"never asked":                   {nil, nil, false, true},
		"changed and old":               {&old, []string{"v1"}, false, true},
		"changed but recent":            {&recent, []string{"v1"}, false, false},
		"same patterns, old":            {&same, []string{"v1"}, false, false},
		"force over same":               {&same, []string{"v1"}, true, true},
		"force over recent":             {&recent, []string{"v1"}, true, true},
		"new learned version, recent":   {&recent, []string{"v2"}, false, true}, // no 30-day wait
		"one of two versions unseen":    {&same, []string{"v1", "v2"}, false, true},
		"learned row appeared":          {&same, []string{"v9"}, false, true},
		"no learned rows, same":         {&same, nil, false, false},
		"newest question is incomplete": {&incompleteNewest, []string{"v1"}, false, true}, // whatever hash and age
		"only incomplete questions":     {&onlyIncomplete, nil, false, true},
	} {
		if got := needsAsking(info, tc.last, tc.learned, now, tc.force); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestPlanRequestsPacksSmallDomainsByCountry(t *testing.T) {
	mk := func(d string, hosts int, country string) *domainInfo {
		return &domainInfo{Domain: d, Hosts: hosts, Country: country, Small: hosts <= llmSmallHosts}
	}
	var infos []*domainInfo
	infos = append(infos, mk("big.net", 50, "US"), mk("mid.net", 6, "DE"), mk("nocountry.net", 2, ""))
	for i := 0; i < 65; i++ {
		infos = append(infos, mk(fmt.Sprintf("small%02d.de", i), 1+i%4, "DE"))
	}
	infos = append(infos, mk("small-fr.fr", 3, "FR"))
	n := 0
	reqs := planRequests(infos, func() string { n++; return fmt.Sprint(n) })
	// big, mid (singles), nocountry (alone), 3 packs of DE (30, 30, 5), 1 of FR
	if len(reqs) != 7 {
		t.Fatalf("%d requests", len(reqs))
	}
	if reqs[0].Domains[0].Domain != "big.net" || reqs[1].Domains[0].Domain != "mid.net" || reqs[0].Kind != kindPropose {
		t.Fatalf("biggest first: %v %v", reqs[0].Domains[0].Domain, reqs[1].Domains[0].Domain)
	}
	sizes := map[string]int{}
	for _, r := range reqs[2:] {
		for _, d := range r.Domains {
			if r.Kind != kindProposeSmall || d.Country != r.Domains[0].Country {
				t.Fatalf("pack mixes countries or kinds: %+v", r)
			}
		}
		sizes[r.Domains[0].Country] += 1
		if len(r.Domains) > packSize {
			t.Fatalf("pack of %d", len(r.Domains))
		}
	}
	if sizes["DE"] != 3 || sizes["FR"] != 1 || sizes[""] != 1 {
		t.Fatalf("packs per country: %v", sizes)
	}
}

func TestBuildDomainInfo(t *testing.T) {
	var ns []Name
	for i := 1; i <= 6; i++ {
		ns = append(ns, Name{IP: fmt.Sprintf("198.51.100.%d", i), PTR: fmt.Sprintf("pool-%d-1-1-1.nyc.example.net", i), Country: "US"})
	}
	ns = append(ns, Name{IP: "198.51.100.9", PTR: "bad_label.example.net", Country: "US"})
	d := buildDomainInfo("example.net", ns)
	if d.Hosts != 6 || d.Small || d.Country != "US" || len(d.Patterns) != 1 || d.Patterns[0] != "pool-0-0-0-0.nyc.example.net" {
		t.Fatalf("%+v", d)
	}
}

// ---- API client ----

func fakeClient(url string) *openRouter {
	return &openRouter{base: url, key: "sk-secret-key", http: http.DefaultClient, backoff: time.Millisecond}
}

func TestClassifyErrors(t *testing.T) {
	body := func(s string) []byte { return []byte(s) }
	h := http.Header{}
	hRA := http.Header{"Retry-After": []string{"3"}}
	for name, tc := range map[string]struct {
		status int
		hdr    http.Header
		body   []byte
		want   apiKind
		ra     time.Duration
	}{
		"key limit":       {403, h, body(`{"error":{"code":403,"message":"x","metadata":{"limit_source":"openrouter_key_limit"}}}`), apiStopCredit, 0},
		"workspace":       {403, h, body(`{"error":{"metadata":{"limit_source":"openrouter_workspace_budget"}}}`), apiStopCredit, 0},
		"guardrail":       {403, h, body(`{"error":{"metadata":{"limit_source":"openrouter_guardrail_budget"}}}`), apiStopCredit, 0},
		"402":             {402, h, body(`{"error":{"message":"credits"}}`), apiStopCredit, 0},
		"in flight, wait": {402, hRA, body(`{"error":{"metadata":{"limit_source":"openrouter_in_flight_budget"}}}`), apiTemporary, 3 * time.Second},
		"in flight, stop": {402, h, body(`{"error":{"metadata":{"limit_source":"openrouter_in_flight_budget"}}}`), apiStopCredit, 0},
		"moderation":      {403, h, body(`{"error":{"metadata":{"reasons":["violence"],"flagged_input":"abc"}}}`), apiBlocked, 0},
		"other 403":       {403, h, body(`{"error":{"message":"guardrail"}}`), apiBlocked, 0},
		"401":             {401, h, body(`{"error":{"message":"no auth"}}`), apiAuth, 0},
		"429":             {429, hRA, nil, apiTemporary, 3 * time.Second},
		"502":             {502, h, nil, apiTemporary, 0},
		"400":             {400, h, body(`{}`), apiOther, 0},
	} {
		kind, ra, reason := classify(tc.status, tc.hdr, tc.body)
		if kind != tc.want || ra != tc.ra || reason == "" {
			t.Errorf("%s: kind %d wait %v reason %q", name, kind, ra, reason)
		}
		if strings.Contains(reason, "abc") || strings.Contains(reason, "credits") {
			t.Errorf("%s: reason echoes the body: %q", name, reason)
		}
	}
}

func completion(content string, cost float64) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]string{"content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 50, "cost": cost},
	})
	return string(b)
}

func TestChatRequestBodyAndRetry(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var body map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(429)
			return
		}
		auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		io.WriteString(w, completion(`{"location": false}`, 0.004))
	}))
	defer srv.Close()
	res, aerr := fakeClient(srv.URL).chat(context.Background(), "m/x", "hello")
	if aerr != nil || calls != 3 || res.Cost != 0.004 || res.TokensIn != 100 || res.TokensOut != 50 {
		t.Fatalf("res %+v err %v calls %d", res, aerr, calls)
	}
	if body["model"] != "m/x" || body["temperature"] != float64(0) || body["max_tokens"] != float64(16000) ||
		body["response_format"].(map[string]any)["type"] != "json_object" || body["usage"].(map[string]any)["include"] != true ||
		body["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("request body: %v", body)
	}
	if auth != "Bearer sk-secret-key" {
		t.Fatalf("auth header: %q", auth)
	}
}

func TestChatGivesUpAfterRetriesWithoutLeakingTheKey(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) }))
	defer srv.Close()
	_, aerr := fakeClient(srv.URL).chat(context.Background(), "m", "p")
	if aerr == nil || aerr.Kind != apiTemporary || calls != maxAttempts {
		t.Fatalf("err %v calls %d", aerr, calls)
	}
	// A dead server: a transport error carries no URL or key text.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	_, aerr = fakeClient(url).chat(context.Background(), "m", "p")
	if aerr == nil || strings.Contains(aerr.Reason, "sk-secret") || strings.Contains(aerr.Reason, url) {
		t.Fatalf("err %v", aerr)
	}
}

func TestNewOpenRouterHTTPIsIPv4Only(t *testing.T) {
	c := newOpenRouterHTTP(180 * time.Second)
	if c.Timeout != 180*time.Second {
		t.Fatalf("timeout %v", c.Timeout)
	}
	tr := c.Transport.(*http.Transport)
	// An IPv6 literal cannot be dialed: the dialer is pinned to tcp4.
	if _, err := tr.DialContext(context.Background(), "tcp", "[::1]:9"); err == nil {
		t.Fatal("dialed an IPv6 address")
	}
}

// ---- full run against a fake OpenRouter and an in-memory store ----

type fakeStore struct {
	mu      sync.Mutex
	last    map[string]lastAnswer
	rules   map[string][]llmRuleRow
	answers []answerRecord
	llmRule map[string]llmRuleWrite
	updates []rowUpdate
	reject  map[string]string // domain -> rejection reason of StoreLLMRule
}

func newFakeStore() *fakeStore {
	return &fakeStore{last: map[string]lastAnswer{}, rules: map[string][]llmRuleRow{}, llmRule: map[string]llmRuleWrite{}, reject: map[string]string{}}
}
func (f *fakeStore) LastAnswers(context.Context) (map[string]lastAnswer, error) { return f.last, nil }
func (f *fakeStore) LearnedVersions(context.Context) (map[string][]string, error) {
	out := map[string][]string{}
	for d, rows := range f.rules {
		for _, r := range rows {
			if r.Rank <= MaxRank && r.Status != "rejected" {
				out[d] = append(out[d], r.Version)
			}
		}
	}
	return out, nil
}
func (f *fakeStore) Rules(_ context.Context, d string) ([]llmRuleRow, error) { return f.rules[d], nil }
func (f *fakeStore) SaveAnswer(_ context.Context, r answerRecord) error {
	f.answers = append(f.answers, r)
	return nil
}
func (f *fakeStore) StoreLLMRule(_ context.Context, d string, w llmRuleWrite) (string, error) {
	if why := f.reject[d]; why != "" {
		return why, nil
	}
	f.llmRule[d] = w
	return "", nil
}
func (f *fakeStore) UpdateLearnedRow(_ context.Context, d string, u rowUpdate) (bool, error) {
	f.updates = append(f.updates, u)
	return true, nil
}
func (f *fakeStore) answer(domain, kind string) *answerRecord {
	for i := range f.answers {
		if f.answers[i].Domain == domain && f.answers[i].Kind == kind {
			return &f.answers[i]
		}
	}
	return nil
}

// fakeAPI answers /key and /chat/completions by model.
type fakeAPI struct {
	mu        sync.Mutex
	remaining string // JSON of limit_remaining
	chat      func(model, prompt string, n int) (int, string)
	prompts   []string
	chats     int
}

func (f *fakeAPI) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/key") {
			rem := f.remaining
			if rem == "" {
				rem = "null"
			}
			io.WriteString(w, `{"data":{"limit_remaining":`+rem+`}}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		f.chats++
		n := f.chats
		prompt := req.Messages[0].Content
		f.prompts = append(f.prompts, prompt)
		f.mu.Unlock()
		status, body := f.chat(req.Model, prompt, n)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
}

func newTestRun(t *testing.T, store *fakeStore, api *fakeAPI) (*proposeRun, func()) {
	srv := api.server()
	g := testGeoIndex(t)
	run := &proposeRun{store: store, api: fakeClient(srv.URL), model: "m/terra", second: "m/luna",
		loadGeo: func(context.Context) (*geoIndex, error) { return g, nil },
		now:     func() time.Time { return time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC) }, parallel: 1}
	return run, srv.Close
}

func domainWith(domain, country string, ptrs ...string) *domainInfo {
	var ns []Name
	for i, p := range ptrs {
		ns = append(ns, Name{IP: fmt.Sprintf("198.51.100.%d", i+1), PTR: p + "." + domain, Country: country})
	}
	return buildDomainInfo(domain, ns)
}

const terraRegex = `^[a-z0-9-]+\.([a-z]+)\.carrier\.net$`

func carrierDomain() *domainInfo {
	return domainWith("carrier.net", "", "r1.fnt", "r2.fnt", "r3.ams", "r4.ams", "r5.muc", "r6.muc")
}

func terraAnswer(regex string) string {
	return `{"location": true, "regex": ` + jsonString(regex) + `, "codes": {
		"fnt": {"country": "DE", "region": "Hesse", "city": "Frankfurt"},
		"ams": {"country": "NL", "region": "North Holland", "city": "Amsterdam"},
		"muc": {"country": "DE", "region": "Bavaria", "city": "Munich"}}, "comment": "ok"}`
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestRunAcceptsAnAnswerAndStoresRankFour(t *testing.T) {
	store := newFakeStore()
	api := &fakeAPI{remaining: "5", chat: func(model, prompt string, n int) (int, string) {
		return 200, completion(terraAnswer(terraRegex), 0.01)
	}}
	run, closeSrv := newTestRun(t, store, api)
	defer closeSrv()
	out, err := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Eligible != 1 || out.Asked != 1 || out.Accepted != 1 || out.Requests != 1 || out.CostUSD != 0.01 || out.Stopped != "" || out.Incomplete != 0 {
		t.Fatalf("%+v", out)
	}
	if out.KeyRemainingUSD == nil || *out.KeyRemainingUSD != 5 {
		t.Fatalf("key remaining %v", out.KeyRemainingUSD)
	}
	w := store.llmRule["carrier.net"]
	if w.Regex != terraRegex || w.Places["fnt"] != (Place{"DE", "Frankfurt am Main"}) || w.Codes["fnt"].GeoNameID != 1 || w.Decoded != 6 {
		t.Fatalf("stored rule %+v", w)
	}
	rec := store.answer("carrier.net", kindPropose)
	if rec == nil || rec.Status != statusComplete || rec.Verdict != verdictAccepted || rec.Hash == "" || len(rec.Patterns) == 0 || rec.TokensIn != 100 {
		t.Fatalf("record %+v", rec)
	}
	// What was sent holds patterns only: no real IP, no real name.
	prompt := api.prompts[0]
	if strings.Contains(prompt, "198.51.100") || strings.Contains(prompt, "r1.fnt.carrier.net") || !strings.Contains(prompt, "r0.fnt.carrier.net") {
		t.Fatalf("prompt:\n%s", prompt)
	}
}

func TestRunStopsOnKeyLimitAndKeepsEverythingStored(t *testing.T) {
	store := newFakeStore()
	api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
		if n == 1 {
			return 200, completion(terraAnswer(terraRegex), 0.01)
		}
		return 403, `{"error":{"code":403,"message":"limit","metadata":{"limit_source":"openrouter_key_limit"}}}`
	}}
	run, closeSrv := newTestRun(t, store, api)
	defer closeSrv()
	infos := map[string]*domainInfo{
		"carrier.net": carrierDomain(),
		"other.net":   domainWith("other.net", "", "a.x", "b.y", "c.z", "d.w", "e.v"),
		"third.net":   domainWith("third.net", "", "a.x", "b.y", "c.z", "d.w", "e.v", "f.u"),
	}
	out, err := run.execute(context.Background(), infos, ProposeOptions{})
	if err != nil {
		t.Fatalf("a credit stop must not fail the task: %v", err)
	}
	if out.Stopped != stopCredit || out.Accepted != 1 {
		t.Fatalf("%+v", out)
	}
	if _, ok := store.llmRule["carrier.net"]; !ok {
		// the biggest domain went first (carrier.net has 6 hosts, third.net 6, other 5)
		if _, ok2 := store.llmRule["third.net"]; ok2 {
			t.Log("third.net answered first")
		}
	}
	incomplete := 0
	for _, a := range store.answers {
		if a.Status == statusIncomplete {
			incomplete++
			if !strings.Contains(a.Reason, "stopped") || a.Verdict != verdictStopped {
				t.Errorf("reason %q verdict %q", a.Reason, a.Verdict)
			}
		}
	}
	if incomplete == 0 || out.Incomplete != incomplete || out.StoppedDomains != incomplete || out.Errors != 0 {
		t.Fatalf("incomplete %d, output %+v", incomplete, out)
	}
}

func TestRunInvalidKeyFails(t *testing.T) {
	store := newFakeStore()
	api := &fakeAPI{chat: func(string, string, int) (int, string) { return 401, `{"error":{"message":"bad key sk-secret-key"}}` }}
	srv := api.server()
	defer srv.Close()
	// The /key call is made first: make it fail with 401 too.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"message":"bad key sk-secret-key"}}`)
	}))
	defer bad.Close()
	for _, base := range []string{srv.URL, bad.URL} {
		run := &proposeRun{store: store, api: fakeClient(base), model: "m", second: "m2", parallel: 1,
			loadGeo: func(context.Context) (*geoIndex, error) { return testGeoIndex(t), nil }}
		_, err := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
		if err != ErrInvalidKey || strings.Contains(err.Error(), "sk-") {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestRunModerationBlockIsAVerdictAndPackIsAskedAlone(t *testing.T) {
	store := newFakeStore()
	var sawSingle int
	api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
		if strings.Count(prompt, "=== domain ") > 1 {
			return 403, `{"error":{"code":403,"metadata":{"reasons":["x"],"flagged_input":"y"}}}`
		}
		sawSingle++
		if strings.Contains(prompt, "badone.de") {
			return 403, `{"error":{"code":403,"metadata":{"reasons":["x"],"flagged_input":"y"}}}`
		}
		return 200, completion(`{"domains": {"fine.de": {"location": false, "regex": "", "codes": {}}}}`, 0.001)
	}}
	run, closeSrv := newTestRun(t, store, api)
	defer closeSrv()
	infos := map[string]*domainInfo{
		"badone.de": domainWith("badone.de", "DE", "a.muc"),
		"fine.de":   domainWith("fine.de", "DE", "a.muc"),
	}
	out, err := run.execute(context.Background(), infos, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Blocked != 1 || out.NoCity != 1 || sawSingle != 2 || out.Asked != 2 || out.Incomplete != 0 {
		t.Fatalf("%+v single=%d", out, sawSingle)
	}
	if r := store.answer("badone.de", kindProposeSmall); r == nil || r.Verdict != verdictBlocked || r.Status != statusComplete {
		t.Fatalf("%+v", r)
	}
}

func TestRunPackedRequestAndSmallCountryRule(t *testing.T) {
	store := newFakeStore()
	api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
		return 200, completion(`{"domains": {
			"a.de": {"location": true, "regex": "^[a-z0-9-]+\\.([a-z]+)\\.a\\.de$", "codes": {"muc": {"country": "DE", "region": "", "city": "Munich"}}},
			"b.de": {"location": true, "regex": "^[a-z0-9-]+\\.([a-z]+)\\.b\\.de$", "codes": {"par": {"country": "FR", "region": "", "city": "Paris"}}},
			"c.de": {"location": false}}}`, 0.006)
	}}
	run, closeSrv := newTestRun(t, store, api)
	defer closeSrv()
	infos := map[string]*domainInfo{
		"a.de": domainWith("a.de", "DE", "gw.muc"),
		"b.de": domainWith("b.de", "DE", "gw.par"), // announced in DE, answer says FR
		"c.de": domainWith("c.de", "DE", "gw.zzz"),
		"d.de": domainWith("d.de", "DE", "gw.xxx"), // missing from the answer
	}
	out, err := run.execute(context.Background(), infos, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Requests != 1 || out.Asked != 4 || out.Accepted != 1 || out.SmallAccepted != 1 || out.Rejected[rejSmallCountry] != 1 || out.NoCity != 1 || out.Errors != 1 || out.Incomplete != 1 {
		t.Fatalf("%+v", out)
	}
	if store.answer("d.de", kindProposeSmall).Status != statusIncomplete {
		t.Fatal("a missing domain must stay incomplete")
	}
	if store.answer("a.de", kindProposeSmall).RequestID != store.answer("c.de", kindProposeSmall).RequestID {
		t.Fatal("a pack shares its request id")
	}
}

func TestRunEligibilityOnlyAndForce(t *testing.T) {
	store := newFakeStore()
	d := carrierDomain()
	store.last["carrier.net"] = lastAnswer{Hash: d.Hash, AskedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), HasComplete: true}
	api := &fakeAPI{chat: func(string, string, int) (int, string) { return 200, completion(terraAnswer(terraRegex), 0) }}
	run, closeSrv := newTestRun(t, store, api)
	defer closeSrv()
	out, _ := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": d}, ProposeOptions{})
	if out.Eligible != 0 || api.chats != 0 {
		t.Fatalf("same patterns must not be asked again: %+v", out)
	}
	run2, close2 := newTestRun(t, store, api)
	defer close2()
	out, _ = run2.execute(context.Background(), map[string]*domainInfo{"carrier.net": d, "x.net": carrierDomain()}, ProposeOptions{Force: true, Domains: []string{"carrier.net"}})
	if out.Eligible != 1 || out.Accepted != 1 {
		t.Fatalf("force + domains: %+v", out)
	}
}

func TestRunRequestCapAndCreditEstimate(t *testing.T) {
	store := newFakeStore()
	api := &fakeAPI{remaining: "0.012", chat: func(string, string, int) (int, string) {
		return 200, completion(`{"location": false}`, 0.005)
	}}
	run, closeSrv := newTestRun(t, store, api)
	defer closeSrv()
	infos := map[string]*domainInfo{}
	for i := 0; i < 5; i++ {
		d := fmt.Sprintf("d%d.net", i)
		infos[d] = domainWith(d, "", "a.x", "b.y", "c.z", "d.w", "e.v")
	}
	out, err := run.execute(context.Background(), infos, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// $0.012 left at $0.005 a request: 2 requests, then the credit stop.
	if out.Requests != 2 || out.Stopped != stopCredit || out.NoCity != 2 {
		t.Fatalf("%+v", out)
	}
	// No credit at all: nothing is asked.
	api2 := &fakeAPI{remaining: "0.001", chat: api.chat}
	run2, close2 := newTestRun(t, newFakeStore(), api2)
	defer close2()
	out, _ = run2.execute(context.Background(), infos, ProposeOptions{})
	if out.Stopped != stopCredit || api2.chats != 0 {
		t.Fatalf("%+v", out)
	}
	// The cap of 400 requests.
	run3, close3 := newTestRun(t, newFakeStore(), &fakeAPI{chat: api.chat})
	defer close3()
	run3.api = fakeClient(run3.api.base)
	big := map[string]*domainInfo{}
	for i := 0; i < maxRequestsPerRun+5; i++ {
		d := fmt.Sprintf("big%d.net", i)
		big[d] = domainWith(d, "", "a.x", "b.y", "c.z", "d.w", "e.v")
	}
	run3.parallel = 4
	out, err = run3.execute(context.Background(), big, ProposeOptions{})
	if err != nil || out.Requests != maxRequestsPerRun || out.Stopped != stopCap {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestRunComparesWithLearnedRulesAndOverridesWithSecondOpinion(t *testing.T) {
	store := newFakeStore()
	learnedPattern := extractionPattern("carrier.net", extraction{label: 1, token: 1, dictionary: "iata"})
	places := map[string][2]string{"fnt": {"US", "Flint"}, "ams": {"NL", "Amsterdam"}, "muc": {"DE", "Munich"}}
	store.rules["carrier.net"] = []llmRuleRow{{Rank: 1, Version: version(learnedPattern, "iata", map[string]Place{"fnt": {"US", "Flint"}, "ams": {"NL", "Amsterdam"}, "muc": {"DE", "Munich"}}),
		Status: "candidate", Origin: "learned", Pattern: learnedPattern, Dictionary: "iata", Codes: map[string]Code{}, Places: places}}
	var secondPrompt string
	api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
		if model == "m/luna" {
			secondPrompt = prompt
			return 200, completion(`{"tokens": {"fnt": {"country": "DE", "region": "Hesse", "city": "Frankfurt am Main"}}}`, 0.002)
		}
		return 200, completion(terraAnswer(terraRegex), 0.01)
	}}
	run, closeSrv := newTestRun(t, store, api)
	defer closeSrv()
	out, err := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.ComparedRules != 1 || out.Disagreements != 1 || out.Overrides != 1 || out.Disputed != 0 || out.Requests != 2 || out.Incomplete != 0 {
		t.Fatalf("%+v", out)
	}
	if len(store.updates) != 1 {
		t.Fatalf("updates %+v", store.updates)
	}
	u := store.updates[0]
	c := u.Codes["fnt"]
	if u.Rank != 1 || c.Origin != "llm" || c.City != "Frankfurt am Main" || c.Country != "DE" || c.GeoNameID != 1 ||
		c.Scope != (Scope{LabelFromRight: 1, TokenFromLeft: 1, Dictionary: "iata"}) || u.Places["fnt"] != (Place{"DE", "Frankfurt am Main"}) || u.Places["ams"] != (Place{"NL", "Amsterdam"}) {
		t.Fatalf("update %+v", u)
	}
	if u.NewVersion == u.OldVersion || u.NewVersion != version(learnedPattern, "iata", u.Places) {
		t.Fatal("version not recomputed")
	}
	// The second model is not told either city, only the token and the learned regex.
	if strings.Contains(secondPrompt, "Flint") || strings.Contains(secondPrompt, "Frankfurt") || !strings.Contains(secondPrompt, `"fnt"`) {
		t.Fatalf("second prompt:\n%s", secondPrompt)
	}
	rec := store.answer("carrier.net", kindPropose)
	if rec.Status != statusComplete || rec.Disagreements != 1 || rec.Overrides != 1 || len(rec.RuleVersions) != 1 {
		t.Fatalf("%+v", rec)
	}
	// The record holds the version the override wrote, so the override does not cause a re-ask.
	if rec.RuleVersions[0] != u.NewVersion {
		t.Fatalf("rule_versions %v, want the version after the override %s", rec.RuleVersions, u.NewVersion)
	}
	if s := store.answer("carrier.net", kindSecond); s == nil || s.Status != statusComplete || s.Verdict != verdictAccepted {
		t.Fatalf("second record %+v", s)
	}
}

func TestRunSecondOpinionDisagreesManualAndFailure(t *testing.T) {
	learnedPattern := extractionPattern("carrier.net", extraction{label: 1, token: 1, dictionary: "iata"})
	mk := func(codes map[string]Code) *fakeStore {
		s := newFakeStore()
		places := map[string][2]string{"fnt": {"US", "Flint"}, "ams": {"NL", "Amsterdam"}, "muc": {"DE", "Munich"}}
		s.rules["carrier.net"] = []llmRuleRow{{Rank: 1, Version: "v1", Status: "candidate", Origin: "learned", Pattern: learnedPattern, Dictionary: "iata", Codes: codes, Places: places}}
		return s
	}
	run := func(store *fakeStore, second func() (int, string)) ProposeOutput {
		api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
			if model == "m/luna" {
				return second()
			}
			return 200, completion(terraAnswer(terraRegex), 0.01)
		}}
		r, closeSrv := newTestRun(t, store, api)
		defer closeSrv()
		out, err := r.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// The second model puts the token in another city: ours stays, the token is disputed.
	store := mk(map[string]Code{})
	out := run(store, func() (int, string) {
		return 200, completion(`{"tokens": {"fnt": {"country": "US", "region": "Michigan", "city": "Flint"}}}`, 0)
	})
	if out.Overrides != 0 || out.Disputed != 1 || len(store.updates) != 0 || out.Incomplete != 0 {
		t.Fatalf("different city: %+v", out)
	}
	// A manual code is never overridden.
	store = mk(map[string]Code{"fnt": {Country: "US", City: "Flint", Origin: "manual"}})
	out = run(store, func() (int, string) {
		return 200, completion(`{"tokens": {"fnt": {"country": "DE", "region": "Hesse", "city": "Frankfurt"}}}`, 0)
	})
	if out.Overrides != 0 || out.Disputed != 1 || len(store.updates) != 0 {
		t.Fatalf("manual: %+v", out)
	}
	// A failed second opinion leaves the answer incomplete (asked again next run).
	store = mk(map[string]Code{})
	out = run(store, func() (int, string) { return 400, `{"error":{"message":"bad"}}` })
	rec := store.answer("carrier.net", kindPropose)
	if out.Incomplete != 2 || rec.Status != statusIncomplete || !strings.Contains(rec.Reason, "second opinion failed") || out.Overrides != 0 {
		t.Fatalf("failure: %+v %+v", out, rec)
	}
}

func TestRunRejectedVersionIsAVerdict(t *testing.T) {
	store := newFakeStore()
	store.reject["carrier.net"] = rejRejectedVersion
	api := &fakeAPI{chat: func(string, string, int) (int, string) { return 200, completion(terraAnswer(terraRegex), 0) }}
	run, closeSrv := newTestRun(t, store, api)
	defer closeSrv()
	out, _ := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
	if out.Accepted != 0 || out.Rejected[rejRejectedVersion] != 1 {
		t.Fatalf("%+v", out)
	}
	if store.answer("carrier.net", kindPropose).Verdict != verdictRejected {
		t.Fatal("verdict")
	}
}

func TestScopeOfRuleRoundTrip(t *testing.T) {
	for _, e := range []extraction{{1, 1, "iata"}, {2, 3, "geonames"}, {4, 2, "locode"}} {
		p := extractionPattern("example.net", e)
		s, ok := scopeOfRule("example.net", p, e.dictionary)
		if !ok || s != (Scope{LabelFromRight: e.label, TokenFromLeft: e.token, Dictionary: e.dictionary}) {
			t.Errorf("%+v -> %+v %v", e, s, ok)
		}
	}
	if _, ok := scopeOfRule("example.net", `^custom$`, "iata"); ok {
		t.Error("a hand-written pattern has no scope")
	}
}

func TestCompareRulesWithin100kmIsAgreement(t *testing.T) {
	g := testGeoIndex(t)
	dom := "tr.net"
	pattern := extractionPattern(dom, extraction{label: 1, token: 1, dictionary: "geonames"})
	learned := compileLearned(dom, []llmRuleRow{{Rank: 1, Status: "candidate", Pattern: pattern, Dictionary: "geonames", Places: map[string][2]string{"ist": {"TR", "Istanbul"}, "fnt": {"US", "Flint"}}}})
	ns := names(t, dom, "", "r1.ist", "r2.ist", "r3.fnt", "r4.fnt")
	rule := &checkedRule{Re: regexp.MustCompile(`(?i)^[a-z0-9-]+\.([a-z]+)\.tr\.net$`)}
	esn, _ := g.resolve("TR", "", "Esenyurt")
	fra, _ := g.resolve("DE", "", "Frankfurt am Main")
	rule.Geo = map[string]*geoPlace{"ist": esn, "fnt": fra}
	got := compareRules(dom, ns, learned, rule, g)
	if len(got) != 1 || got[0].Token != "fnt" || got[0].Terra.ID != 1 || got[0].Rank != 1 {
		t.Fatalf("disputes %+v", got)
	}
}

// The GeoNames index comes from the learner's monthly cache: cities1000 and admin1CodesASCII.
func TestLoadGeoIndexUsesTheMonthlyCache(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("cities1000.txt")
	for _, r := range testGeoRows {
		cols := make([]string, 19)
		cols[0], cols[1], cols[2], cols[3], cols[8], cols[10], cols[14] = fmt.Sprint(r.id), r.name, r.name, r.alt, r.country, r.admin1, fmt.Sprint(r.pop)
		fmt.Fprintln(f, strings.Join(cols, "\t"))
	}
	_ = zw.Close()
	var urls []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		urls = append(urls, r.URL.String())
		body := buf.String()
		if strings.HasSuffix(r.URL.Path, "admin1CodesASCII.txt") {
			body = testAdmin1
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	g, err := loadGeoIndex(context.Background(), dir, client, now)
	if err != nil {
		t.Fatal(err)
	}
	if p, why := g.resolve("DE", "Hesse", "Frankfurt"); p == nil || why != "" || p.ID != 1 {
		t.Fatalf("%v %q", p, why)
	}
	if len(urls) != 2 || urls[0] != geoNamesURL || urls[1] != admin1URL {
		t.Fatalf("urls %v", urls)
	}
	for _, n := range []string{"cities1000.zip.checked", "admin1CodesASCII.txt.checked"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("no %s: %v", n, err)
		}
	}
	if _, err := loadGeoIndex(context.Background(), dir, client, now.Add(7*24*time.Hour)); err != nil || len(urls) != 2 {
		t.Fatalf("a week later the cache is used: %d requests, %v", len(urls), err)
	}
}

// A learned row that this answer checked, an invalid one included, is recorded by version.
func TestRunRecordsEveryCurrentLearnedVersion(t *testing.T) {
	store := newFakeStore()
	store.rules["carrier.net"] = []llmRuleRow{
		{Rank: 1, Version: "good", Status: "candidate", Origin: "learned", Pattern: `^([a-z]+)\.carrier\.net$`, Dictionary: "iata", Places: map[string][2]string{}},
		{Rank: 2, Version: "broken-regex", Status: "candidate", Origin: "learned", Pattern: `([`, Dictionary: "iata", Places: map[string][2]string{}},
		{Rank: 3, Version: "withdrawn", Status: "rejected", Origin: "learned", Pattern: `^x$`, Dictionary: "iata"},
		{Rank: 4, Version: "llm-row", Status: "candidate", Origin: "llm", Pattern: terraRegex, Dictionary: "llm"},
	}
	api := &fakeAPI{chat: func(string, string, int) (int, string) { return 200, completion(terraAnswer(terraRegex), 0) }}
	run, closeSrv := newTestRun(t, store, api)
	defer closeSrv()
	if _, err := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{}); err != nil {
		t.Fatal(err)
	}
	rec := store.answer("carrier.net", kindPropose)
	if len(rec.RuleVersions) != 2 || rec.RuleVersions[0] != "good" || rec.RuleVersions[1] != "broken-regex" {
		t.Fatalf("rule_versions %v", rec.RuleVersions)
	}
	// What LearnedVersions reports for the domain is all in the record: the next run asks nothing.
	learned, _ := store.LearnedVersions(context.Background())
	last := lastAnswer{Hash: carrierDomain().Hash, AskedAt: rec.AskedAt, RuleVersions: rec.RuleVersions, HasComplete: true}
	if needsAsking(carrierDomain(), &last, learned["carrier.net"], run.now(), false) {
		t.Fatal("asked again for versions it had checked")
	}
}

// A domain whose newest question is incomplete is asked again, with the same patterns.
func TestRunRetriesAnIncompleteDomain(t *testing.T) {
	store := newFakeStore()
	d := carrierDomain()
	store.last["carrier.net"] = lastAnswer{Hash: d.Hash, AskedAt: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), HasComplete: true, NewestIncomplete: true}
	api := &fakeAPI{chat: func(string, string, int) (int, string) { return 200, completion(terraAnswer(terraRegex), 0) }}
	run, closeSrv := newTestRun(t, store, api)
	defer closeSrv()
	out, err := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": d}, ProposeOptions{})
	if err != nil || out.Eligible != 1 || out.Accepted != 1 {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestCodeKeysAreTrimmedAndLowerCased(t *testing.T) {
	g := testGeoIndex(t)
	dom := "example.net"
	ns := names(t, dom, "", "r1.fra", "r2.fra", "r3.ams", "r4.ams")
	good := `^[a-z0-9-]+\.([a-z]+)\.example\.net$`
	a := cityAnswer(good, map[string][3]string{" FRA ": {"DE", "Hesse", "Frankfurt"}, "Ams": {"NL", "", "Amsterdam"}})
	rule, why := checkAnswer(dom, a, ns, false, g)
	if rule == nil || why != "" || rule.Places["fra"].City != "Frankfurt am Main" || rule.Places["ams"].City != "Amsterdam" || len(rule.Places) != 2 {
		t.Fatalf("%q %+v", why, rule)
	}
	// Two keys that fold to one token: the first in sorted order stays ("FRA" < "fra").
	a = cityAnswer(good, map[string][3]string{"fra": {"DE", "", "Munich"}, "FRA": {"DE", "Hesse", "Frankfurt"}, "ams": {"NL", "", "Amsterdam"}})
	for i := 0; i < 20; i++ {
		rule, why = checkAnswer(dom, a, ns, false, g)
		if rule == nil || why != "" || rule.Places["fra"].City != "Frankfurt am Main" {
			t.Fatalf("collision: %q %+v", why, rule)
		}
	}
	// The same for the second opinion.
	got := parseSecond(`{"tokens": {"Fnt": {"country": "DE", "city": "Munich"}, " FNT": {"country": "DE", "city": "Frankfurt"}, "x": null}}`)
	if len(got) != 1 || got["fnt"].City != "Frankfurt" { // " FNT" sorts before "Fnt"
		t.Fatalf("second opinion keys %+v", got)
	}
}
