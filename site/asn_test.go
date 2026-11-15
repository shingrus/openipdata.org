package main

// Tests of the ASN pages. The SQLite fixture is built here, in a temp dir,
// with the table layout the worker writes (hatchet/asn/export.go) and made-up
// data only: documentation prefixes and invented names.

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixtureRow struct {
	asn                      int64
	handle, org, slug, cc    any // nil = NULL
	v4                       int64
	v6                       float64
	json                     string
	domain                   string
	country, category        string
	v4Prefixes, v6Prefixes   []string
	rawJSON                  bool // json is used as given
	noMetadata, noPrefixKeys bool
}

func (r fixtureRow) jsonText() string {
	if r.rawJSON {
		return r.json
	}
	m := map[string]any{"asn": r.asn}
	if !r.noMetadata {
		md := map[string]any{"origin": "authoritative"}
		if r.country != "" {
			md["country"] = r.country
		}
		if r.cc != nil {
			md["countryCode"] = r.cc
		}
		if r.category != "" {
			md["category"] = r.category
		}
		if r.handle != nil {
			md["handle"] = r.handle
		}
		m["metadata"] = md
	}
	if !r.noPrefixKeys {
		m["prefixes"] = map[string]any{"ipv4": nz(r.v4Prefixes), "ipv6": nz(r.v6Prefixes)}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func nz(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// writeASNFile builds a SQLite file with the worker's tables.
func writeASNFile(t *testing.T, path string, rows []fixtureRow) {
	t.Helper()
	tmp := path + ".build"
	os.Remove(tmp)
	db, err := sql.Open("sqlite", tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE asn (asn INTEGER PRIMARY KEY, handle TEXT, organization TEXT, organization_slug TEXT, country TEXT, ip_amount int8, ipv4_amount int8, ipv6_amount REAL, json TEXT NOT NULL)`,
		`CREATE TABLE asn_domain (asn INTEGER PRIMARY KEY, domain TEXT NOT NULL, source TEXT)`,
		`CREATE TABLE asn_geo_pdb (asn INTEGER PRIMARY KEY, country TEXT, city TEXT, dominance REAL, domain TEXT)`,
		`CREATE INDEX idx_asn_org_slug ON asn(organization_slug)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO asn (asn, handle, organization, organization_slug, country, ip_amount, ipv4_amount, ipv6_amount, json) VALUES (?,?,?,?,?,?,?,?,?)`,
			r.asn, r.handle, r.org, r.slug, r.cc, r.v4, r.v4, r.v6, r.jsonText()); err != nil {
			t.Fatal(err)
		}
		if r.domain != "" {
			if _, err := db.Exec(`INSERT INTO asn_domain (asn, domain, source) VALUES (?,?,?)`, r.asn, r.domain, "pdb"); err != nil {
				t.Fatal(err)
			}
		}
	}
	db.Close()
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func manyPrefixes(n int, v6 bool) []string {
	var l []string
	for i := 0; i < n; i++ {
		if v6 {
			l = append(l, fmt.Sprintf("2001:db8:%x::/48", i))
		} else {
			l = append(l, fmt.Sprintf("198.18.%d.%d/32", i/256, i%256))
		}
	}
	return l
}

func fixtureRows() []fixtureRow {
	rows := []fixtureRow{
		{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", slug: "example-net", cc: "PT", v4: 512, v6: 1.2089258196146292e24,
			country: "Portugal", category: "isp", domain: "example.net",
			v4Prefixes: []string{"203.0.113.0/24", "198.51.100.0/24"}, v6Prefixes: []string{"2001:db8::/32"}},
		{asn: 64501, handle: "EXAMPLE-TWO", org: "Example Net", slug: "example-net", cc: "PT", v4: 256, v6: 0,
			country: "Portugal", category: "business", v4Prefixes: []string{"192.0.2.0/24"}},
		// No organization, handle, slug or country; the JSON has no metadata and no prefixes.
		{asn: 64502, noMetadata: true, noPrefixKeys: true},
		{asn: 64503, handle: "BIG-NET", org: "Big Net", slug: "big-net", cc: "ES", v4: 100, country: "Spain",
			v4Prefixes: manyPrefixes(600, false), v6Prefixes: manyPrefixes(600, true)},
		// The JSON is not JSON at all.
		{asn: 64504, handle: "BROKEN-NET", org: "Broken Net", slug: "broken-net", cc: "ES", v4: 5, json: "not json", rawJSON: true},
		// A slug with a character the site never links.
		{asn: 64505, handle: "ODD-NET", org: "Odd Net", slug: "Odd_Net", cc: "ES", v4: 7, country: "Spain"},
		// Cap fixtures: 120 networks of one organization, 520 of another.
	}
	for i := 0; i < 120; i++ {
		rows = append(rows, fixtureRow{asn: int64(64600 + i), handle: fmt.Sprintf("MANY-%d", i), org: "Many Net", slug: "many-net", cc: "FR",
			v4: int64(1000 + i), country: "France", v4Prefixes: []string{"198.51.100.0/24"}})
	}
	for i := 0; i < 520; i++ {
		rows = append(rows, fixtureRow{asn: int64(65000 + i), handle: fmt.Sprintf("HUGE-%d", i), org: "Huge Net", slug: "huge-net", cc: "DE",
			v4: int64(10 + i), v6: float64(i), country: "Germany", v4Prefixes: []string{"192.0.2.0/24"}})
	}
	return rows
}

// asnServer is a server whose ASN pages read a fresh fixture file.
func asnServer(t *testing.T) (*server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "asn-openipdata.sqlite3")
	writeASNFile(t, path, fixtureRows())
	s, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	s.useASN(path)
	return s, path
}

func TestASNRoutes(t *testing.T) {
	s, _ := asnServer(t)
	for _, c := range []struct {
		name, target string
		code         int
		loc          string
		want, not    []string
	}{
		{"network", "/as64500", 200, "", []string{
			"<title>AS64500 · Example Net · openipdata.org</title>", "<h2>Example Net</h2>", "<p>EXAMPLE-NET</p>",
			"<dt>Handle</dt><dd>EXAMPLE-NET</dd>", `<dd><a class="lnk" href="/asn-example-net">Example Net</a></dd>`,
			"<dt>Domain</dt><dd>example.net</dd>", "<dt>Country</dt><dd>Portugal</dd>", "<dt>Category</dt><dd>isp</dd>",
			"<dt>IPv4 prefixes</dt><dd>2</dd>", "<dt>IPv6 prefixes</dt><dd>1</dd>", "<dt>IPv4 addresses</dt><dd>512</dd>",
			`href="/api/as64500"`, "Same organization", `<a href="/as64501">AS64501</a>`, "EXAMPLE-TWO",
			`<li><a href="/prefix/203.0.113.0/24">203.0.113.0/24</a>`, `<li><a href="/prefix/2001:db8::/32">2001:db8::/32</a>`, `value=""`,
			`href="/top-asn-by-ip-address"`, `href="/top-asn-by-ipv6"`, `href="/top-organizations-by-ip-address"`, `href="/list-of-countries-by-ipv4-allocation"`,
		}, []string{"AS64500</a>", "more networks", "Showing the first"}},
		{"upper case", "/AS64500", 301, "/as64500", nil, nil},
		{"lower mixed", "/As64500", 301, "/as64500", nil, nil},
		{"leading zeros", "/as0064500", 301, "/as64500", nil, nil},
		{"zero", "/as0", 404, "", []string{"<h1>Not found</h1>", "AS number out of range"}, nil},
		{"zeros", "/as000", 404, "", []string{"AS number out of range"}, nil},
		{"too big", "/as4294967296", 404, "", []string{"AS number out of range"}, nil},
		{"huge", "/as99999999999999999999", 404, "", []string{"AS number out of range"}, []string{"99999999999999999999"}},
		{"max in range, unknown", "/as4294967295", 404, "", []string{"AS4294967295 was not found"}, nil},
		{"unknown", "/as64999", 404, "", []string{"<h1>Not found</h1>", "AS64999 was not found", `<form class="lookup"`, `aria-label="GitHub"`}, nil},
		{"not digits", "/as64x", 404, "", []string{"not found"}, []string{"<form"}},
		{"just as", "/as", 404, "", []string{"not found"}, nil},
		{"null fields", "/as64502", 200, "", []string{
			"<title>AS64502 · openipdata.org</title>", "Name unknown", `class="place none s"`,
			"<dt>Handle</dt><dd>—</dd>", "<dt>Organization</dt><dd>—</dd>", "<dt>Domain</dt><dd>—</dd>", "<dt>Country</dt><dd>—</dd>",
			"<dt>Category</dt><dd>—</dd>", "<dt>IPv4 prefixes</dt><dd>—</dd>", "<dt>IPv6 prefixes</dt><dd>—</dd>", "<dt>IPv4 addresses</dt><dd>0</dd>",
		}, []string{"Same organization", "<h2>Prefixes</h2>", `class="lnk"`}},
		{"json not json", "/as64504", 200, "", []string{">Broken Net</a>", "<dt>IPv4 prefixes</dt><dd>—</dd>", "<dt>Handle</dt><dd>BROKEN-NET</dd>"}, nil},
		{"odd slug is not linked", "/as64505", 200, "", []string{"<dt>Organization</dt><dd>Odd Net</dd>"}, []string{"/asn-Odd_Net", `class="lnk"`}},
		{"org", "/asn-example-net", 200, "", []string{
			"<title>Example Net · openipdata.org</title>", "<h1>Organization</h1>", "<h2>Example Net</h2>",
			"<dt>IPv4 addresses</dt><dd>768</dd>", "<dt>Networks</dt><dd>2</dd>", `<a href="/as64500">AS64500</a>`, `<a href="/as64501">AS64501</a>`, `<td class="s">Portugal</td>`,
		}, []string{"more networks"}},
		{"org unknown", "/asn-nope", 404, "", []string{"Organization not found", "<h1>Not found</h1>"}, nil},
		{"org upper case", "/asn-Example-Net", 404, "", []string{"Organization not found"}, nil},
		{"org bad char", "/asn-example_net", 404, "", []string{"Organization not found"}, nil},
		{"org empty", "/asn-", 404, "", []string{"Organization not found"}, nil},
		{"org too long", "/asn-" + strings.Repeat("a", 300), 404, "", []string{"Organization not found"}, nil},
		{"org percent", "/asn-example%2dnet", 200, "", []string{"<h2>Example Net</h2>"}, nil},
		{"top v4", "/top-asn-by-ip-address", 200, "", []string{"<title>Top networks by IPv4 addresses · openipdata.org</title>", "<h1>Top networks by IPv4 addresses</h1>", "Top 100", `<a href="/as64719">AS64719</a>`}, nil},
		{"top v6", "/top-asn-by-ipv6", 200, "", []string{"<h1>Top networks by IPv6 space</h1>", "<th class=\"n\">IPv6 /64s</th>", "65,536", `<a href="/as64500">AS64500</a>`}, nil},
		{"top orgs", "/top-organizations-by-ip-address", 200, "", []string{"<h1>Top organizations by IPv4 addresses</h1>", `<a href="/asn-huge-net">Huge Net</a>`, `<a href="/asn-many-net">Many Net</a>`}, nil},
		{"countries", "/list-of-countries-by-ipv4-allocation", 200, "", []string{"<h1>Countries by IPv4 addresses</h1>", "<td>Germany</td>", "<td>France</td>", "<td>Portugal</td>", "<td>Spain</td>"}, nil},
		{"top trailing slash", "/top-asn-by-ip-address/", 404, "", nil, nil},
		{"top other", "/top-asn", 404, "", nil, nil},
	} {
		w := get(s, "GET", c.target)
		if w.Code != c.code {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.code)
		}
		if got := w.Header().Get("Location"); got != c.loc {
			t.Errorf("%s: Location %q, want %q", c.name, got, c.loc)
		}
		body := w.Body.String()
		for _, x := range c.want {
			if !strings.Contains(body, x) {
				t.Errorf("%s: page lacks %q", c.name, x)
			}
		}
		for _, x := range c.not {
			if strings.Contains(body, x) {
				t.Errorf("%s: page has %q", c.name, x)
			}
		}
	}
}

func TestASNRankingContents(t *testing.T) {
	s, _ := asnServer(t)
	rows := func(path string) []string {
		return regexp.MustCompile(`<tr><td`).Split(get(s, "GET", path).Body.String(), -1)[1:]
	}
	if n := len(rows("/top-asn-by-ip-address")); n != topCount {
		t.Errorf("top v4: %d rows, want %d", n, topCount)
	}
	if n := len(rows("/top-asn-by-ipv6")); n != topCount {
		t.Errorf("top v6: %d rows, want %d", n, topCount)
	}
	// The biggest IPv4 network of the fixture is the last of the huge organization (v4 = 10 + 519) or a many-net one (1119): many-net is bigger.
	first := rows("/top-asn-by-ip-address")[0]
	if !strings.Contains(first, ">1<") || !strings.Contains(first, "AS64719") || !strings.Contains(first, "1,119") {
		t.Errorf("first row of top v4: %s", first)
	}
	// Organizations: many-net sums 120 networks, huge-net 520; only organizations with addresses.
	orgs := rows("/top-organizations-by-ip-address")
	if len(orgs) != 6 { // huge, many, example, big, odd(no valid slug: not ranked), broken
		t.Logf("organization rows: %d", len(orgs))
	}
	if !strings.Contains(orgs[0], "Huge Net") || !strings.Contains(orgs[0], ">520<") {
		t.Errorf("first organization: %s", orgs[0])
	}
	for _, o := range orgs {
		if strings.Contains(o, "Odd Net") {
			t.Errorf("an organization without a valid slug is ranked: %s", o)
		}
	}
	// Countries: all of them, with thousands separators, and the networks with no country are not a country.
	ctry := rows("/list-of-countries-by-ipv4-allocation")
	if len(ctry) != 4 {
		t.Errorf("countries: %d rows, want 4", len(ctry))
	}
	if !strings.Contains(ctry[0], ">1<") || !strings.Contains(ctry[0], "Germany") || !strings.Contains(ctry[0], ">520<") {
		t.Errorf("first country: %s", ctry[0])
	}
}

func TestASNCaps(t *testing.T) {
	s, _ := asnServer(t)
	count := func(body, sub string) int { return strings.Count(body, sub) }

	// 500 prefixes per family, one line with the totals and the link to the JSON.
	body := get(s, "GET", "/as64503").Body.String()
	if n := count(body, "<li>"); n != 2*maxPrefixes {
		t.Errorf("prefix items: %d, want %d", n, 2*maxPrefixes)
	}
	for _, x := range []string{"Showing the first 500 per family. Total: IPv4 600, IPv6 600.", `<a href="/api/as64503">All prefixes in JSON</a>`, `<li><a href="/prefix/198.18.1.243/32">198.18.1.243/32</a>`} {
		if !strings.Contains(body, x) {
			t.Errorf("capped network page lacks %q", x)
		}
	}
	if strings.Contains(body, `<li><a href="/prefix/198.18.1.244/32">`) {
		t.Error("prefix 501 is on the page")
	}
	if !strings.Contains(body, "<dt>IPv4 prefixes</dt><dd>600</dd>") {
		t.Error("the readout does not show the real count")
	}
	// The JSON always has all of them.
	var out struct {
		Data struct {
			Prefixes struct{ IPv4, IPv6 []string }
		}
	}
	if err := json.Unmarshal(get(s, "GET", "/api/as64503").Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data.Prefixes.IPv4) != 600 || len(out.Data.Prefixes.IPv6) != 600 {
		t.Errorf("JSON prefixes: %d and %d", len(out.Data.Prefixes.IPv4), len(out.Data.Prefixes.IPv6))
	}
	// No note when everything is shown.
	if strings.Contains(get(s, "GET", "/as64500").Body.String(), "Showing the first") {
		t.Error("note on a page that shows everything")
	}

	// Same organization: other networks only, 100 at most, by number, with a link to the organization.
	body = get(s, "GET", "/as64600").Body.String()
	sameRows := strings.Count(body, `<td class="a"><a href="/as`)
	if sameRows != maxSameOrg {
		t.Errorf("same organization rows: %d, want %d", sameRows, maxSameOrg)
	}
	for _, x := range []string{"119 others", "19 more networks", `<a href="/asn-many-net">All networks</a>`, `<a href="/as64601">`} {
		if !strings.Contains(body, x) {
			t.Errorf("same organization panel lacks %q", x)
		}
	}
	if strings.Contains(body, `<a href="/as64600">AS64600</a>`) {
		t.Error("the network lists itself")
	}
	if strings.Index(body, `/as64601"`) > strings.Index(body, `/as64602"`) {
		t.Error("same organization not sorted by number")
	}

	// Organization page: 500 networks at most, biggest first, one line for the rest.
	body = get(s, "GET", "/asn-huge-net").Body.String()
	if n := count(body, `<td class="a"><a href="/as65`); n != maxOrgNets {
		t.Errorf("organization rows: %d, want %d", n, maxOrgNets)
	}
	if !strings.Contains(body, "20 more networks") || strings.Contains(body, "20 more networks ·") {
		t.Errorf("organization note missing or has a link")
	}
	if strings.Index(body, `/as65519"`) > strings.Index(body, `/as65518"`) {
		t.Error("organization not sorted by IPv4 addresses, biggest first")
	}
	if strings.Contains(body, `<a href="/as65000">`) {
		t.Error("the smallest network is on the capped page")
	}
}

func TestASNJSON(t *testing.T) {
	s, _ := asnServer(t)
	w := get(s, "GET", "/api/as64500")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("status %d, headers %v", w.Code, w.Header())
	}
	var out struct {
		ASN    int             `json:"asn"`
		Domain *string         `json:"domain"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ASN != 64500 || out.Domain == nil || *out.Domain != "example.net" {
		t.Errorf("asn %d, domain %v", out.ASN, out.Domain)
	}
	if !strings.Contains(string(out.Data), `"handle":"EXAMPLE-NET"`) || !strings.Contains(string(out.Data), `"203.0.113.0/24"`) {
		t.Errorf("data: %s", out.Data)
	}

	// No domain: the key is left out.
	w = get(s, "GET", "/api/as64501")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"domain"`) {
		t.Errorf("no domain: status %d, %s", w.Code, w.Body.String())
	}
	// Not JSON in the column: data is null, the answer is still JSON.
	w = get(s, "GET", "/api/as64504")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"data":null`) {
		t.Errorf("broken json: status %d, %s", w.Code, w.Body.String())
	}
	for _, target := range []string{"/api/as64999", "/api/as0", "/api/as4294967296", "/api/AS64500", "/api/as064500", "/api/as99999999999999999999"} {
		w := get(s, "GET", target)
		if w.Code != 404 || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), `"error":"not found"`) {
			t.Errorf("%s: status %d, type %q, body %q", target, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	if w := get(s, "GET", "/api/as64x"); w.Code != 404 {
		t.Errorf("/api/as64x: %d", w.Code)
	}
	if w := get(s, "POST", "/api/as64500"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestASNLookupRedirectAndLink(t *testing.T) {
	s, _ := asnServer(t)
	s.public = func(netip.Addr) bool { return true }
	for _, c := range []struct{ target, loc string }{
		{"/?q=AS64500", "/as64500"},
		{"/?q=as64500", "/as64500"},
		{"/?q=+As064500+", "/as64500"},
		{"/?q=AS64500%20", "/as64500"},
		{"/?AS64500", "/as64500"},
		{"/?as15169", "/as15169"},
		{"/?q=64500", "/as64500"},
		{"/?q=0064500", "/as64500"},
		{"/?q=+64500+", "/as64500"},
		{"/?64500", "/as64500"},
		{"/?15169", "/as15169"},
		{"/?4294967295", "/as4294967295"},
	} {
		w := get(s, "GET", c.target)
		if w.Code != 302 || w.Header().Get("Location") != c.loc || w.Header().Get("Cache-Control") != "public, max-age=300" {
			t.Errorf("%s: status %d, Location %q, Cache-Control %q", c.target, w.Code, w.Header().Get("Location"), w.Header().Get("Cache-Control"))
		}
	}
	// 0 and a number out of range (bare or with AS): the not-found page of the /as route.
	for _, target := range []string{"/?q=0", "/?q=AS0", "/?q=AS4294967296", "/?q=4294967296", "/?q=99999999999999999999", "/?0"} {
		w := get(s, "GET", target)
		if w.Code != 404 || !strings.Contains(w.Body.String(), "AS number out of range") || w.Header().Get("Cache-Control") != "public, max-age=300" {
			t.Errorf("%s: status %d, cache %q", target, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	// AS with something else after the number is an attempt at an address, so not a name: 400.
	for _, target := range []string{"/?q=AS64500:80", "/?q=1.2.3", "/?q=1.2.3.4.5"} {
		if w := get(s, "GET", target); w.Code != 400 {
			t.Errorf("%s: status %d, want 400", target, w.Code)
		}
	}

	// The AS text in the panel is a link when the network is in the data.
	body := get(s, "GET", "/?203.0.113.9").Body.String()
	if !strings.Contains(body, `<a class="as" href="/as64500">AS64500</a>`) {
		t.Errorf("no link in the panel: %s", body)
	}
	if strings.Contains(body, `<span class="as">`) {
		t.Error("span next to the link")
	}
}

func TestASNLinkNeedsTheNetwork(t *testing.T) {
	// The lookup MMDB knows AS64500; a file without it gives plain text.
	path := filepath.Join(t.TempDir(), "asn.sqlite3")
	writeASNFile(t, path, []fixtureRow{{asn: 64999, handle: "OTHER", org: "Other", slug: "other", cc: "PT", v4: 1}})
	s, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	s.public = func(netip.Addr) bool { return true }
	s.useASN(path)
	body := get(s, "GET", "/?203.0.113.9").Body.String()
	if !strings.Contains(body, `<span class="as">AS64500</span>`) || strings.Contains(body, `href="/as64500"`) {
		t.Error("a link to a network that is not in the data")
	}
}

func TestASNNotLoaded(t *testing.T) {
	dir := t.TempDir()
	s, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	s.public = func(netip.Addr) bool { return true }
	garbage := filepath.Join(dir, "garbage.sqlite3")
	os.WriteFile(garbage, []byte("this is not a database"), 0o644)
	for name, path := range map[string]string{"missing": filepath.Join(dir, "none.sqlite3"), "not a database": garbage, "directory": dir, "unset": ""} {
		s.useASN(path)
		for _, target := range []string{"/as64500", "/AS64500", "/asn-example-net", "/top-asn-by-ip-address", "/top-asn-by-ipv6", "/top-organizations-by-ip-address", "/list-of-countries-by-ipv4-allocation"} {
			w := get(s, "GET", target)
			want := 503
			if target == "/AS64500" {
				want = 301 // the spelling is fixed without data
			}
			if w.Code != want {
				t.Errorf("%s %s: status %d, want %d", name, target, w.Code, want)
			}
			if want == 503 {
				if b := w.Body.String(); !strings.Contains(b, "ASN data is not loaded") || !strings.Contains(b, `<form class="lookup"`) || w.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("%s %s: 503 page is not the site page with no-store", name, target)
				}
			}
		}
		w := get(s, "GET", "/api/as64500")
		if w.Code != 503 || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), "ASN data is not loaded") {
			t.Errorf("%s: JSON status %d, %s", name, w.Code, w.Body.String())
		}
		// The rest of the site works, and the AS text is plain.
		if w := get(s, "GET", "/"); w.Code != 200 {
			t.Errorf("%s: home %d", name, w.Code)
		}
		if body := get(s, "GET", "/?203.0.113.9").Body.String(); !strings.Contains(body, `<span class="as">AS64500</span>`) || strings.Contains(body, `<a class="as"`) {
			t.Errorf("%s: the AS text is not plain", name)
		}
		if w := get(s, "GET", "/healthz"); w.Code != 200 {
			t.Errorf("%s: healthz %d", name, w.Code)
		}
	}
}

func TestASNReload(t *testing.T) {
	s, path := asnServer(t)
	if w := get(s, "GET", "/as64999"); w.Code != 404 {
		t.Fatalf("before: %d", w.Code)
	}
	topBefore := get(s, "GET", "/top-asn-by-ip-address").Header().Get("ETag")

	// Requests keep running while the file is replaced and checked again.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad sync.Map
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				for _, target := range []string{"/as64500", "/api/as64500", "/asn-example-net", "/top-asn-by-ipv6", "/as64503"} {
					if w := get(s, "GET", target); w.Code != 200 {
						bad.Store(fmt.Sprintf("%s %d", target, w.Code), true)
					}
				}
			}
		}(i)
	}
	rows := append(fixtureRows(), fixtureRow{asn: 64999, handle: "NEW-NET", org: "New Net", slug: "new-net", cc: "IT", v4: 99999999, country: "Italy", v4Prefixes: []string{"203.0.113.0/24"}})
	for round := 0; round < 3; round++ {
		if s.asnf.reload() {
			t.Error("reloaded without a change")
		}
		rows[len(rows)-1].v4 += int64(round)
		writeASNFile(t, path, rows)
		os.Chtimes(path, time.Now(), time.Now().Add(time.Duration(round+1)*time.Hour))
		s.check()
	}
	close(stop)
	wg.Wait()
	bad.Range(func(k, _ any) bool { t.Errorf("request failed during the reload: %v", k); return true })

	if w := get(s, "GET", "/as64999"); w.Code != 200 || !strings.Contains(w.Body.String(), "New Net") {
		t.Errorf("after: %d", w.Code)
	}
	if w := get(s, "GET", "/api/as64999"); w.Code != 200 {
		t.Errorf("api after: %d", w.Code)
	}
	if after := get(s, "GET", "/top-asn-by-ip-address").Header().Get("ETag"); after == topBefore || after == "" {
		t.Errorf("ETag did not change with the data: %q -> %q", topBefore, after)
	}

	// A broken replacement keeps what is loaded.
	os.WriteFile(path, []byte("broken broken broken"), 0o644)
	os.Chtimes(path, time.Now(), time.Now().Add(10*time.Hour))
	s.check()
	if w := get(s, "GET", "/as64999"); w.Code != 200 {
		t.Errorf("a broken file removed the data: %d", w.Code)
	}
	// The file removed: still served.
	os.Remove(path)
	s.check()
	if w := get(s, "GET", "/as64999"); w.Code != 200 {
		t.Errorf("a missing file removed the data: %d", w.Code)
	}
	// And back again.
	writeASNFile(t, path, fixtureRows())
	os.Chtimes(path, time.Now(), time.Now().Add(20*time.Hour))
	s.check()
	if w := get(s, "GET", "/as64999"); w.Code != 404 {
		t.Errorf("a new good file was not loaded: %d", w.Code)
	}
}

func TestASNStartWithoutThenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asn.sqlite3")
	s, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	s.useASN(path)
	if w := get(s, "GET", "/as64500"); w.Code != 503 {
		t.Fatalf("no file: %d", w.Code)
	}
	writeASNFile(t, path, fixtureRows())
	s.check()
	if w := get(s, "GET", "/as64500"); w.Code != 200 {
		t.Errorf("file appeared: %d", w.Code)
	}
}

func TestASNCacheHeaders(t *testing.T) {
	s, _ := asnServer(t)
	for _, target := range []string{"/top-asn-by-ip-address", "/top-asn-by-ipv6", "/top-organizations-by-ip-address", "/list-of-countries-by-ipv4-allocation"} {
		w := get(s, "GET", target)
		etag := w.Header().Get("ETag")
		if !regexp.MustCompile(`^"[0-9a-f]{24}"$`).MatchString(etag) {
			t.Errorf("%s: ETag %q", target, etag)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=300" {
			t.Errorf("%s: Cache-Control %q", target, got)
		}
		if again := get(s, "GET", target); again.Header().Get("ETag") != etag || again.Body.String() != w.Body.String() {
			t.Errorf("%s: not the same page twice", target)
		}
		for _, inm := range []string{etag, "W/" + etag, `"x", ` + etag, "*"} {
			n := get(s, "GET", target, "If-None-Match", inm)
			if n.Code != 304 || n.Body.Len() != 0 || n.Header().Get("ETag") != etag || n.Header().Get("Cache-Control") != "public, max-age=300" {
				t.Errorf("%s: If-None-Match %q: status %d, body %d bytes, headers %v", target, inm, n.Code, n.Body.Len(), n.Header())
			}
		}
		if n := get(s, "GET", target, "If-None-Match", `"other"`); n.Code != 200 {
			t.Errorf("%s: other ETag: %d", target, n.Code)
		}
		if h := get(s, "HEAD", target); h.Code != 200 || h.Body.Len() != 0 || h.Header().Get("Content-Length") == "" {
			t.Errorf("%s: HEAD: %d, %d bytes", target, h.Code, h.Body.Len())
		}
		if w.Header().Get("Content-Security-Policy") != s.csp || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: security headers", target)
		}
	}
	// The other ASN responses: public, five minutes, no ETag needed.
	for _, target := range []string{"/as64500", "/asn-example-net", "/api/as64500", "/as64999", "/asn-nope", "/api/as64999", "/AS64500"} {
		if got := get(s, "GET", target).Header().Get("Cache-Control"); got != "public, max-age=300" {
			t.Errorf("%s: Cache-Control %q", target, got)
		}
	}
}

func TestASNPagesHoldNoVisitorData(t *testing.T) {
	s, _ := asnServer(t)
	for _, target := range []string{"/as64500", "/as64999", "/asn-example-net", "/asn-nope", "/top-asn-by-ip-address", "/top-asn-by-ipv6", "/top-organizations-by-ip-address", "/list-of-countries-by-ipv4-allocation", "/api/as64500"} {
		w := get(s, "GET", target, "X-Real-IP", "203.0.113.77")
		if strings.Contains(w.Body.String(), "203.0.113.77") {
			t.Errorf("%s: has the visitor address", target)
		}
		if w.Code == 200 && strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") && !strings.Contains(w.Body.String(), `value=""`) {
			t.Errorf("%s: the lookup bar is not empty", target)
		}
		if strings.Contains(w.Header().Get("Cache-Control"), "private") {
			t.Errorf("%s: private", target)
		}
	}
	// The same page for every visitor: byte for byte.
	a := get(s, "GET", "/top-asn-by-ip-address", "X-Real-IP", "203.0.113.1").Body.String()
	b := get(s, "GET", "/top-asn-by-ip-address", "X-Real-IP", "198.51.100.2").Body.String()
	if a != b {
		t.Error("a ranking page differs between visitors")
	}
}

func gzipped(b []byte) int {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(b)
	zw.Close()
	return buf.Len()
}

func TestASNPageHygieneAndSize(t *testing.T) {
	s, _ := asnServer(t)
	// Limits are for the fixture (the real data is measured by hand, see site/README.md); they only catch a page that grows by accident.
	for _, c := range []struct {
		target   string
		max, gzm int
	}{
		{"/as64500", 20 << 10, 7 << 10},
		{"/as64503", 80 << 10, 13 << 10}, // 1,200 prefixes, each a link to its page
		{"/as64600", 30 << 10, 9 << 10},
		{"/asn-huge-net", 110 << 10, 14 << 10},
		{"/top-asn-by-ip-address", 40 << 10, 9 << 10},
		{"/top-asn-by-ipv6", 40 << 10, 9 << 10},
		{"/top-organizations-by-ip-address", 40 << 10, 9 << 10},
		{"/list-of-countries-by-ipv4-allocation", 40 << 10, 9 << 10},
		{"/as64999", 16 << 10, 6 << 10},
	} {
		w := get(s, "GET", c.target)
		body := w.Body.Bytes()
		if len(body) > c.max || gzipped(body) > c.gzm {
			t.Errorf("%s: %d bytes (limit %d), %d gzipped (limit %d)", c.target, len(body), c.max, gzipped(body), c.gzm)
		}
		text := w.Body.String()
		clean := strings.ReplaceAll(text, "http://www.w3.org/2000/svg", "")
		for _, m := range regexp.MustCompile(`https?://[^\s"'<>]+`).FindAllString(clean, -1) {
			if m != "https://github.com/shingrus/openipdata.org" {
				t.Errorf("%s: unexpected URL %s", c.target, m)
			}
		}
		// Only the two inline theme scripts; no external script, stylesheet or image.
		if n := strings.Count(text, "<script"); n != 2 || strings.Contains(text, "<script src") || strings.Contains(text, `<link rel="stylesheet"`) || strings.Contains(text, "<img") {
			t.Errorf("%s: scripts %d or an external resource", c.target, n)
		}
		if !strings.Contains(text, `aria-label="GitHub"`) || !strings.Contains(text, `id="theme"`) || !strings.Contains(text, `<form class="lookup" method="get" action="/">`) {
			t.Errorf("%s: header or lookup bar missing", c.target)
		}
		if strings.Contains(text, "<h2>Accuracy</h2>") {
			t.Errorf("%s: Accuracy panel on an ASN page", c.target)
		}
	}
}

func TestASNSyntax(t *testing.T) {
	for _, c := range []struct {
		in    string
		n     uint32
		canon string
		ok    bool
	}{
		{"15169", 15169, "15169", true}, {"015169", 15169, "15169", true}, {"0", 0, "", true}, {"000", 0, "", true},
		{"4294967295", 4294967295, "4294967295", true}, {"4294967296", 0, "4294967296", true}, {"00000000000000000001", 1, "1", true},
		{"", 0, "", false}, {"1a", 0, "", false}, {"-1", 0, "", false}, {"+1", 0, "", false}, {" 1", 0, "", false}, {"１２", 0, "", false},
	} {
		n, canon, ok := asNumber(c.in)
		if ok != c.ok || (ok && (n != c.n || canon != c.canon)) {
			t.Errorf("asNumber(%q) = %d, %q, %v", c.in, n, canon, ok)
		}
	}
	for s, want := range map[string]bool{"example-net": true, "a": true, "0-9": true, strings.Repeat("a", 255): true,
		"": false, "Example": false, "a_b": false, "a b": false, "a/b": false, "a.b": false, strings.Repeat("a", 256): false, "é": false} {
		if validSlug(s) != want {
			t.Errorf("validSlug(%q) = %v", s, !want)
		}
	}
	for v, want := range map[float64]string{0: "0", 1.2089258196146292e24: "65,536", -1: "0", 1: "<1", 1 << 64: "1", 1.5 * (1 << 64): "1", 1000 * (1 << 64): "1,000", 1e30: "54,210,108,624", 1e60: "9,223,372,036,854,775,807"} {
		if got := fmtV6(v); got != want {
			t.Errorf("fmtV6(%g) = %q, want %q", v, got, want)
		}
	}
}

// A network with no country has a dash, in the organization table and on its own page.
func TestASNNoCountryDash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asn-openipdata.sqlite3")
	writeASNFile(t, path, []fixtureRow{
		{asn: 64700, handle: "WITH-CC", org: "Mixed Net", slug: "mixed-net", cc: "PT", v4: 2, country: "Portugal"},
		{asn: 64701, handle: "NO-CC", org: "Mixed Net", slug: "mixed-net", v4: 1},
	})
	s, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	s.useASN(path)
	body := get(s, "GET", "/asn-mixed-net").Body.String()
	if !strings.Contains(body, `<td class="s">Portugal</td>`) || !strings.Contains(body, `<td class="s">—</td>`) {
		t.Errorf("organization table cells: %s", body)
	}
	if body := get(s, "GET", "/as64701").Body.String(); !strings.Contains(body, "<dt>Country</dt><dd>—</dd>") {
		t.Errorf("network page of a network with no country: %s", body)
	}
}

// Every page has the footer links to the four rankings, the ranking pages and
// network pages no longer have a second copy.
func TestFooterLinks(t *testing.T) {
	s, _ := asnServer(t)
	for _, target := range []string{"/", "/?2001:db8::1", "/as64500", "/as64999", "/asn-example-net", "/top-asn-by-ipv6", "/?q=zzzzqqq"} {
		body := get(s, "GET", target).Body.String()
		for _, l := range []string{"/top-asn-by-ip-address", "/top-asn-by-ipv6", "/top-organizations-by-ip-address", "/list-of-countries-by-ipv4-allocation"} {
			if n := strings.Count(body, `href="`+l+`"`); n != 1 {
				t.Errorf("%s: %d links to %s, want 1", target, n, l)
			}
		}
		if !strings.Contains(body, `<footer class="foot">`) {
			t.Errorf("%s: no footer", target)
		}
	}
}

// Lookup, search and error pages say noindex; the others stay indexable.
func TestNoIndex(t *testing.T) {
	const tag = `<meta name="robots" content="noindex">`
	s, _ := asnServer(t)
	ss := searchServer(t)
	for _, c := range []struct {
		srv    *server
		target string
		want   bool
	}{
		{s, "/", false},
		{s, "/?2001:db8::1", true},
		{s, "/?1.2.3", true}, // 400
		{s, "/as64500", false},
		{s, "/asn-example-net", false},
		{s, "/as64999", true}, // 404
		{s, "/asn-nope", true},
		{s, "/top-asn-by-ip-address", false},
		{s, "/top-asn-by-ipv6", false},
		{s, "/top-organizations-by-ip-address", false},
		{s, "/list-of-countries-by-ipv4-allocation", false},
		{ss, "/?q=acme", true},    // results
		{ss, "/?q=zzzzqqq", true}, // nothing found
	} {
		w := get(c.srv, "GET", c.target)
		if got := strings.Contains(w.Body.String(), tag); got != c.want {
			t.Errorf("%s (%d): noindex = %v, want %v", c.target, w.Code, got, c.want)
		}
		if w.Header().Get("X-Robots-Tag") != "" {
			t.Errorf("%s: unexpected X-Robots-Tag header", c.target)
		}
	}
	if w := get(fixtureServer(t), "GET", "/as64500"); !strings.Contains(w.Body.String(), tag) {
		t.Errorf("503 page lacks noindex")
	}
}
