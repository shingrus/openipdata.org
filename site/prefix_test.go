package main

// Tests of the prefix index and the prefix pages. The SQLite file is built in
// a temp dir (documentation and benchmark prefixes, made-up names); the places
// come from testdata/lookup-geo.mmdb (see README.md for what is in it).

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func prefixRows() []fixtureRow {
	rows := []fixtureRow{
		{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", slug: "example-net", cc: "PT", v4: 512, country: "Portugal",
			v4Prefixes: []string{"203.0.113.0/24", "198.51.100.0/24", "198.18.0.0/24"}, v6Prefixes: []string{"2001:db8::/32"}},
		// The same prefix as AS64500, a piece of it, and a bigger one that covers 198.18.0.0/24.
		{asn: 64501, handle: "EXAMPLE-TWO", org: "Example Two", slug: "example-two", cc: "PT", v4: 256, country: "Portugal",
			v4Prefixes: []string{"203.0.113.0/24", "203.0.113.128/25", "198.18.0.0/22"}},
		{asn: 64503, handle: "BIG-NET", org: "Big Net", slug: "big-net", cc: "ES", v4: 100, country: "Spain",
			v4Prefixes: []string{"198.18.0.0/25", "198.18.1.0/24"}, v6Prefixes: []string{"2001:db8:1::/48", "2001:db8:2::/48"}},
		// Not JSON; and JSON with prefixes the index must skip (not masked, not a prefix, a number, wrong family, IPv4-mapped).
		{asn: 64541, handle: "BROKEN-NET", org: "Broken Net", slug: "broken-net", cc: "ES", v4: 5, json: "not json", rawJSON: true},
		{asn: 64542, handle: "ODD-NET", org: "Odd Net", slug: "odd-net", cc: "ES", v4: 5, rawJSON: true,
			json: `{"prefixes":{"ipv4":["10.0.0.1/8","bad",5,"2001:db8::/32","::ffff:1.2.3.0/120","9.9.9.0/24","9.9.9.0/24"],"ipv6":["2001:db8:ffff::/48","1.2.3.0/24","2001:db8:ffff::/48"]}}`},
		// Out of range for a page (/7), and a spelling that is not canonical.
		{asn: 64540, handle: "LINKS-NET", org: "Links Net", slug: "links-net", cc: "ES", v4: 5, country: "Spain",
			v4Prefixes: []string{"2.0.0.0/7", "8.8.8.0/24"}, v6Prefixes: []string{"2001:DB8:3::/48"}},
		// More networks than the head shows, all with the same prefix.
		{asn: 64550, handle: "ONE", org: "Net One", slug: "net-one", v4: 1, v4Prefixes: []string{"198.18.8.0/24"}},
	}
	for i := 1; i <= 6; i++ {
		rows = append(rows, fixtureRow{asn: int64(64550 + i), handle: fmt.Sprintf("SHARE-%d", i), v4: 1, v4Prefixes: []string{"198.18.8.0/24"}})
	}
	// Seven nested prefixes: a page inside all of them shows the five most specific.
	rows = append(rows, fixtureRow{asn: 64570, handle: "NEST", org: "Nest Net", slug: "nest-net", v4: 1,
		v4Prefixes: []string{"198.20.0.0/16", "198.20.0.0/17", "198.20.0.0/18", "198.20.0.0/19", "198.20.0.0/20", "198.20.0.0/21", "198.20.0.0/22"}})
	// 120 prefixes inside 203.0.114.0/24, which is announced as well.
	var small []string
	for i := 0; i < 120; i++ {
		small = append(small, fmt.Sprintf("203.0.114.%d/32", i))
	}
	rows = append(rows,
		fixtureRow{asn: 64560, handle: "SMALL-NET", org: "Small Net", slug: "small-net", v4: 120, v4Prefixes: small},
		fixtureRow{asn: 64561, handle: "WIDE-NET", org: "Wide Net", slug: "wide-net", v4: 256, v4Prefixes: []string{"203.0.114.0/24"}})
	return rows
}

func prefixServer(t *testing.T) *server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "asn-openipdata.sqlite3")
	writeASNFile(t, path, prefixRows())
	s, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	s.public = func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsPrivate() }
	s.useASN(path)
	return s
}

func asns(l []uint32) string {
	var out []string
	for _, a := range l {
		out = append(out, fmt.Sprint(a))
	}
	return strings.Join(out, ",")
}

func entries(l []annPrefix) string {
	var out []string
	for _, e := range l {
		out = append(out, fmt.Sprintf("%s=%d", e.Prefix, e.ASN))
	}
	return strings.Join(out, " ")
}

func TestPrefixIndex(t *testing.T) {
	s := prefixServer(t)
	d := s.asnf.data()
	if d == nil {
		t.Fatal("no data")
	}
	x := &d.idx
	// Bad, unmasked, wrong-family and mapped entries are out, duplicates are one.
	// IPv4: 64500:3, 64501:3, 64503:2, 64542:1 (9.9.9.0/24), 64540:2, 64550-64556:7, 64570:7, 64560:120, 64561:1.
	// IPv6: 64500:1, 64503:2, 64542:1 (2001:db8:ffff::/48), 64540:1 (upper case parses and is masked).
	wantV4, wantV6 := 3+3+2+1+2+7+7+120+1, 1+2+1+1
	if len(x.v4) != wantV4 || len(x.v6) != wantV6 {
		t.Errorf("index sizes %d, %d; want %d, %d", len(x.v4), len(x.v6), wantV4, wantV6)
	}
	if !slices.IsSortedFunc(x.v4, func(a, b pfx4) int {
		if c := cmp4(a, b.addr, b.bits); c != 0 {
			return c
		}
		return cmpU32(a.asn, b.asn)
	}) {
		t.Error("IPv4 index not sorted")
	}

	mp := netip.MustParsePrefix
	for _, c := range []struct {
		p    string
		want string
	}{
		{"203.0.113.0/24", "64500,64501"}, // the same prefix from two networks
		{"203.0.113.128/25", "64501"},
		{"203.0.113.0/25", ""},
		{"203.0.113.0/23", ""},
		{"198.18.0.0/22", "64501"},
		{"9.9.9.0/24", "64542"},
		{"10.0.0.0/8", ""},
		{"2001:db8::/32", "64500"},
		{"2001:db8:1::/48", "64503"},
		{"2001:db8:ffff::/48", "64542"},
		{"2001:db8:3::/48", "64540"},
		{"2001:db8::/33", ""},
		{"2001:db8:1::/64", ""},
	} {
		if got := asns(x.exact(mp(c.p))); got != c.want {
			t.Errorf("exact(%s) = %q, want %q", c.p, got, c.want)
		}
	}

	for _, c := range []struct{ p, want string }{
		{"203.0.113.0/25", "203.0.113.0/24=64500 203.0.113.0/24=64501"},
		{"203.0.113.128/25", "203.0.113.0/24=64500 203.0.113.0/24=64501"}, // not itself
		{"198.18.0.0/25", "198.18.0.0/24=64500 198.18.0.0/22=64501"},      // most specific first
		{"198.18.1.0/24", "198.18.0.0/22=64501"},
		{"2001:db8:1::/48", "2001:db8::/32=64500"},
		{"2001:db8:1::/128", "2001:db8:1::/48=64503 2001:db8::/32=64500"},
		{"8.8.8.0/24", ""},
		{"198.18.0.0/22", ""},
	} {
		if got := entries(x.covering(mp(c.p))); got != c.want {
			t.Errorf("covering(%s) = %q, want %q", c.p, got, c.want)
		}
	}

	rows, total := x.inside(mp("198.18.0.0/22"), 100)
	if got, want := entries(rows), "198.18.0.0/24=64500 198.18.0.0/25=64503 198.18.1.0/24=64503 198.18.8.0/24=64550"; total != 3 || strings.Contains(got, "198.18.8.") || !strings.HasPrefix(want, got) {
		t.Errorf("inside(198.18.0.0/22) = %q (%d)", got, total)
	}
	rows, total = x.inside(mp("203.0.113.0/24"), 100)
	if entries(rows) != "203.0.113.128/25=64501" || total != 1 {
		t.Errorf("inside(203.0.113.0/24) = %q (%d)", entries(rows), total)
	}
	rows, total = x.inside(mp("203.0.114.0/24"), 5)
	if len(rows) != 5 || total != 120 || rows[0].Prefix.String() != "203.0.114.0/32" || rows[4].Prefix.String() != "203.0.114.4/32" {
		t.Errorf("inside(203.0.114.0/24, 5): %d rows, %d in all", len(rows), total)
	}
	rows, total = x.inside(mp("2001:db8::/32"), 100)
	if entries(rows) != "2001:db8:1::/48=64503 2001:db8:2::/48=64503 2001:db8:3::/48=64540 2001:db8:ffff::/48=64542" || total != 4 {
		t.Errorf("inside(2001:db8::/32) = %q (%d)", entries(rows), total)
	}
	if rows, total = x.inside(mp("2001:db8:1::/48"), 100); len(rows) != 0 || total != 0 {
		t.Errorf("inside(an announced leaf) = %q (%d)", entries(rows), total)
	}
	if rows, total = x.inside(mp("2001:db8:ffff::/64"), 100); len(rows) != 0 || total != 0 {
		t.Errorf("inside(a piece of a leaf) = %q (%d)", entries(rows), total)
	}

	for _, c := range []struct{ a, want string }{
		{"203.0.113.9", "203.0.113.0/24"},
		{"203.0.113.200", "203.0.113.128/25"},
		{"198.18.0.5", "198.18.0.0/25"},
		{"198.18.0.200", "198.18.0.0/24"},
		{"198.18.3.1", "198.18.0.0/22"},
		{"2001:db8:1::5", "2001:db8:1::/48"},
		{"2001:db8:9::5", "2001:db8::/32"},
		{"8.8.4.4", ""},
		{"2600::1", ""},
	} {
		p, _, ok := x.longest(netip.MustParseAddr(c.a))
		if got := ""; ok {
			got = p.String()
			if got != c.want {
				t.Errorf("longest(%s) = %s, want %s", c.a, got, c.want)
			}
		} else if c.want != "" {
			t.Errorf("longest(%s) found nothing, want %s", c.a, c.want)
		}
	}
	if _, asn, _ := x.longest(netip.MustParseAddr("203.0.113.9")); asns(asn) != "64500,64501" {
		t.Errorf("longest networks: %v", asn)
	}

	// The extreme prefixes of both families do not wrap around.
	var edge pfxIndex
	edge.v4 = []pfx4{{addr: 0xffffff00, asn: 1, bits: 24}}
	edge.v6 = []pfx6{{hi: ^uint64(0), lo: 0, asn: 2, bits: 64}}
	if r, n := edge.inside(mp("255.0.0.0/8"), 10); n != 1 || len(r) != 1 {
		t.Errorf("inside at the top of IPv4: %d", n)
	}
	if r, n := edge.inside(mp("ff00::/8"), 10); n != 1 || len(r) != 1 {
		t.Errorf("inside at the top of IPv6: %d", n)
	}
	if got := edge.exact(mp("ffff:ffff:ffff:ffff::/64")); asns(got) != "2" {
		t.Errorf("exact at the top of IPv6: %v", got)
	}
}

func TestPrefixLoadIsTolerant(t *testing.T) {
	// A file whose rows are all unreadable still loads, with an empty index.
	path := filepath.Join(t.TempDir(), "x.sqlite3")
	writeASNFile(t, path, []fixtureRow{{asn: 1, rawJSON: true, json: "not json"}, {asn: 2, rawJSON: true, json: `{"prefixes":"x"}`}, {asn: 3, rawJSON: true, json: `{"prefixes":{"ipv4":"1.0.0.0/8"}}`}})
	f := &asnFile{path: path}
	if !f.reload() {
		t.Fatal("load failed")
	}
	if d := f.data(); d == nil || d.idx.size() != 0 {
		t.Errorf("index of unreadable rows: %v", d)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if x, err := loadPrefixes(context.Background(), db); err != nil || x.size() != 0 {
		t.Errorf("loadPrefixes: %d, %v", x.size(), err)
	}
}

func TestPrefixParse(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok, rng  bool
	}{
		{"8.8.8.0/24", "8.8.8.0/24", true, true},
		{"8.8.8.9/24", "8.8.8.0/24", true, true},
		{"8.0.0.0/8", "8.0.0.0/8", true, true},
		{"8.0.0.0/7", "8.0.0.0/7", true, false},
		{"8.8.8.8/32", "8.8.8.8/32", true, true},
		{"2001:4860::/32", "2001:4860::/32", true, true},
		{"2001:DB8::1/32", "2001:db8::/32", true, true},
		{"2001::/16", "2001::/16", true, true},
		{"2001::/15", "2000::/15", true, false},
		{"::1/128", "::1/128", true, true},
		{"::ffff:8.8.8.0/120", "8.8.8.0/24", true, true},
		{"::ffff:8.8.8.0/95", "", false, false},
		{"8.8.8.0/33", "", false, false},
		{"8.8.8.0/-1", "", false, false},
		{"8.8.8.0", "", false, false},
		{"08.8.8.0/24", "", false, false},
		{"fe80::1%eth0/64", "", false, false},
		{" 8.8.8.0/24", "", false, false},
		{"", "", false, false},
		{strings.Repeat("1", 80) + "/24", "", false, false},
	} {
		p, ok := parseCIDR(c.in)
		if ok != c.ok || (ok && (p.String() != c.want || prefixOK(p) != c.rng)) {
			t.Errorf("parseCIDR(%q) = %s, %v (in range %v); want %s, %v (%v)", c.in, p, ok, ok && prefixOK(p), c.want, c.ok, c.rng)
		}
	}
	for _, c := range []struct{ p, want string }{
		{"8.8.8.0/24", "8.8.8.255"}, {"8.8.8.8/32", "8.8.8.8"}, {"255.0.0.0/8", "255.255.255.255"},
		{"2001:db8::/32", "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff"}, {"ff00::/8", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"}, {"::1/128", "::1"},
	} {
		if got := lastAddr(netip.MustParsePrefix(c.p)).String(); got != c.want {
			t.Errorf("lastAddr(%s) = %s, want %s", c.p, got, c.want)
		}
	}
	for v, want := range map[float64]string{0: "0.0%", 50: "50.0%", 25.04: "25.0%", 100: "100.0%", 0.01: "<0.1%", 0.05: "0.1%"} {
		if got := fmtShare(v, 100); got != want {
			t.Errorf("fmtShare(%g, 100) = %q, want %q", v, got, want)
		}
	}
	if got := fmtShare(1, 1<<100); got != "<0.1%" {
		t.Errorf("a tiny share: %q", got)
	}
}

func TestPrefixRedirectsAndErrors(t *testing.T) {
	s := prefixServer(t)
	for _, c := range []struct {
		target string
		code   int
		loc    string
		body   string
	}{
		{"/prefix/203.0.113.7/24", 301, "/prefix/203.0.113.0/24", ""},
		{"/prefix/2001:DB8::/32", 301, "/prefix/2001:db8::/32", ""},
		{"/prefix/2001:db8:0::/32", 301, "/prefix/2001:db8::/32", ""},
		{"/prefix/::ffff:203.0.113.0/120", 301, "/prefix/203.0.113.0/24", ""},
		{"/prefix/203.0.113.0%2F24", 301, "/prefix/203.0.113.0/24", ""},
		{"/prefix/2001%3Adb8%3A%3A/32", 301, "/prefix/2001:db8::/32", ""},
		{"/prefix/", 404, "", "Prefix not found"},
		{"/prefix/foo", 404, "", "Prefix not found"},
		{"/prefix/203.0.113.0", 404, "", "Prefix not found"},
		{"/prefix/203.0.113.0/7", 404, "", "Prefix not found"},
		{"/prefix/203.0.113.0/33", 404, "", "Prefix not found"},
		{"/prefix/2001:db8::/15", 404, "", "Prefix not found"},
		{"/prefix/2001:db8::/129", 404, "", "Prefix not found"},
		{"/prefix/::ffff:1.2.3.0/64", 404, "", "Prefix not found"},
		{"/prefix/%3Cscript%3E", 404, "", "Prefix not found"},
		{"/prefix/10.0.0.0/8", 404, "", "Not a public prefix"},
		{"/prefix/10.1.2.3/24", 301, "/prefix/10.1.2.0/24", ""}, // the redirect needs no check of the data
		{"/prefix/9.9.10.0/24", 404, "", "Not announced"},
		{"/prefix/2600::/16", 404, "", "Not announced"},
		{"/prefix/203.0.113.0/24/", 404, "", "Prefix not found"},
	} {
		w := get(s, "GET", c.target)
		if w.Code != c.code || w.Header().Get("Location") != c.loc {
			t.Errorf("%s: status %d, Location %q; want %d, %q", c.target, w.Code, w.Header().Get("Location"), c.code, c.loc)
		}
		if !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s: no %q in %s", c.target, c.body, w.Body.String())
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=300" {
			t.Errorf("%s: Cache-Control %q", c.target, got)
		}
		if strings.Contains(w.Body.String(), "script%3E") || strings.Contains(w.Body.String(), "<script>alert") {
			t.Errorf("%s: echoes the request", c.target)
		}
		if w.Code == 404 && !strings.Contains(w.Body.String(), `<meta name="robots" content="noindex">`) {
			t.Errorf("%s: a notice without noindex", c.target)
		}
	}

	// Without ASN data: the redirect and the public check still work, the rest is 503.
	bare := fixtureServer(t)
	for _, c := range []struct {
		target string
		code   int
	}{
		{"/prefix/203.0.113.7/24", 301}, {"/prefix/203.0.113.0/24", 503}, {"/prefix/10.0.0.0/8", 404}, {"/prefix/foo", 404},
	} {
		w := get(bare, "GET", c.target)
		if w.Code != c.code {
			t.Errorf("no data %s: status %d, want %d", c.target, w.Code, c.code)
		}
		if c.code == 503 && (w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "ASN data is not loaded")) {
			t.Errorf("no data: %v %s", w.Header(), w.Body.String())
		}
	}
	if w := get(s, "HEAD", "/prefix/203.0.113.0/24"); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d bytes", w.Code, w.Body.Len())
	}
	if w := get(s, "POST", "/prefix/203.0.113.0/24"); w.Code != 405 {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestPrefixPages(t *testing.T) {
	s := prefixServer(t)
	noindex := `<meta name="robots" content="noindex">`
	for _, c := range []struct {
		name, target string
		indexed      bool
		want, not    []string
	}{
		{"exact, two networks", "/prefix/203.0.113.0/24", true, []string{
			"<title>203.0.113.0/24 Example Net AS64500</title>", "<h2>Prefix</h2>", `<div class="w band"><div class="place s m"><h1>203.0.113.0/24</h1>`, `<div class="cells">`, `<p>Lisbon, <a href="/portugal-asn-list">Portugal</a></p>`,
			`<span>Organization</span><b><a href="/asn-example-net">Example Net</a><br><a href="/asn-example-two">Example Two</a></b>`,
			`<span>Network</span><b><a href="/as64500">EXAMPLE-NET</a> <a href="/as64500">AS64500</a><br><a href="/as64501">EXAMPLE-TWO</a> <a href="/as64501">AS64501</a></b>`,
			"<span>Addresses</span><b>256</b>",
			"More specific prefixes", `<td class="a"><a href="/prefix/203.0.113.128/25">203.0.113.128/25</a></td><td class="a"><a href="/as64501">AS64501</a></td><td><a href="/asn-example-two">Example Two</a></td>`,
			`value=""`, `href="/top-asn-by-ip-address"`,
		}, []string{"Part of", "Places", "Location unknown", "more prefixes", `<svg class="map"`, `class="aim"`, "mapped", "<dl>"}},
		{"covered only, inside one record", "/prefix/203.0.113.0/25", false, []string{
			"<title>203.0.113.0/25</title>", `<p>Lisbon, <a href="/portugal-asn-list">Portugal</a></p>`,
			`<span>Part of</span><b><a href="/prefix/203.0.113.0/24">203.0.113.0/24</a> <a href="/as64500">AS64500</a></b>`,
			"<span>Addresses</span><b>128</b>",
		}, []string{"<span>Network</span>", "More specific", "Places", `<svg class="map"`, `class="aim"`}},
		{"IPv6 exact", "/prefix/2001:db8::/32", true, []string{
			"<h1>2001:db8::/32</h1>", `<span>Addresses</span><b title="4,294,967,296 /64 networks">4.3B /64 networks</b>`, `<p>Lisbon, <a href="/portugal-asn-list">Portugal</a></p>`,
			`<a href="/prefix/2001:db8:1::/48">2001:db8:1::/48</a>`, `<a href="/prefix/2001:db8:ffff::/48">`, "<span>Network</span>",
		}, []string{"<span>/64s</span>", "Part of", `<svg class="map"`, `class="aim"`}},
		{"IPv6 leaf with a parent", "/prefix/2001:db8:1::/48", true, []string{
			`<span>Addresses</span><b title="65,536 /64 networks">65.5K /64 networks</b>`, `<span>Part of</span><b><a href="/prefix/2001:db8::/32">2001:db8::/32</a> <a href="/as64500">AS64500</a></b>`, `<span>Organization</span><b><a href="/asn-big-net">Big Net</a></b>`, `<span>Network</span><b><a href="/as64503">BIG-NET</a> <a href="/as64503">AS64503</a></b>`,
		}, []string{"More specific"}},
		{"IPv6 longer than /64", "/prefix/2001:db8:1::/96", false, []string{"<span>Addresses</span><b>4,294,967,296</b>", "Part of"}, []string{"/64"}},
		{"IPv6 /128", "/prefix/2001:db8:1::5/128", false, []string{"<h1>2001:db8:1::5/128</h1>", "<span>Addresses</span><b>1</b>", `<a href="/prefix/2001:db8:1::/48"`}, nil},
		{"announced only inside", "/prefix/198.18.0.0/21", false, []string{"<span>Addresses</span><b>2,048</b>", "More specific prefixes"}, []string{"Part of", "<span>Network</span>"}},
		{"a piece of a prefix with a smaller piece announced", "/prefix/198.18.0.0/23", false, []string{"198.18.0.0/22", `href="/prefix/198.18.0.0/25"`}, nil},
		{"many networks, one prefix", "/prefix/198.18.8.0/24", true, []string{
			`href="/as64550">AS64550</a>`, `href="/as64554">AS64554</a>`, `<span>Organization</span><b><a href="/asn-net-one">Net One</a></b>`, `<b><a href="/as64550">ONE</a> <a href="/as64550">AS64550</a><br><a href="/as64551">SHARE-1</a> <a`,
		}, []string{`href="/as64555"`, `href="/as64556"`}},
		{"more specific prefixes are capped", "/prefix/203.0.114.0/24", true, []string{
			">120</span>", "20 more prefixes", `<td class="a"><a href="/prefix/203.0.114.99/32">`, `<td><a href="/asn-small-net">Small Net</a></td>`, `<b><a href="/asn-wide-net">Wide Net</a></b>`,
		}, []string{`href="/prefix/203.0.114.100/32"`}},
		{"odd data is skipped", "/prefix/9.9.9.0/24", true, []string{"Odd Net"}, nil},
	} {
		w := get(s, "GET", c.target)
		body := w.Body.String()
		if w.Code != 200 {
			t.Errorf("%s: status %d", c.name, w.Code)
			continue
		}
		if got := strings.Contains(body, noindex); got == c.indexed {
			t.Errorf("%s: noindex present %v, indexed %v", c.name, got, c.indexed)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=300" {
			t.Errorf("%s: Cache-Control %q", c.name, got)
		}
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
	body := get(s, "GET", "/prefix/198.20.0.0/24").Body.String()
	if n := strings.Count(body, `href="/prefix/198.20.`); n != maxNetworkRows || !strings.Contains(body, `href="/prefix/198.20.0.0/22"`) || strings.Contains(body, `href="/prefix/198.20.0.0/17"`) {
		t.Errorf("part of: %d prefixes", n)
	}
	// "More specific" has exactly 100 rows.
	body = get(s, "GET", "/prefix/203.0.114.0/24").Body.String()
	if n := strings.Count(body, `<td class="a"><a href="/prefix/203.0.114.`); n != maxInside {
		t.Errorf("more specific rows: %d, want %d", n, maxInside)
	}
	// Five networks at most in the head.
	got := get(s, "GET", "/prefix/198.18.8.0/24").Body.String()
	head, _, _ := strings.Cut(got, "</section>") // the tables below have links of their own
	if n := strings.Count(head, `">AS645`); n != maxNetworkRows {
		t.Errorf("networks in the head: %d", n)
	}
}

func TestPrefixPlaces(t *testing.T) {
	s := prefixServer(t)
	for _, c := range []struct {
		name, target string
		want, not    []string
	}{
		{"one place, whole prefix", "/prefix/198.18.0.0/25", []string{`<p>Lisbon, <a href="/portugal-asn-list">Portugal</a></p>`, `<div class="w band"><div class="place s m">`}, []string{"Places", `<svg class="map"`, `class="aim"`, "mapped"}},
		{"country only", "/prefix/198.51.100.0/24", []string{`<p><a href="/spain-asn-list">Spain</a></p>`}, []string{`<svg class="map"`, `class="here"`, `class="aim"`, "Places", "Unknown"}},
		{"nothing known", "/prefix/203.0.114.0/24", []string{`<p class="none">Location unknown</p>`}, []string{"Places", `<svg class="map"`, `class="here"`}},
		{"two places and a gap", "/prefix/198.18.0.0/24", []string{
			"<h2>Places</h2>", `<span class="r">2</span>`,
			`<td>Lisbon</td><td><a href="/portugal-asn-list">Portugal</a></td><td class="n">50.0%</td>`, `<td>—</td><td><a href="/spain-asn-list">Spain</a></td><td class="n">25.0%</td>`, `<td>Unknown</td><td>—</td><td class="n">25.0%</td>`,
		}, []string{"Lisbon, Portugal", `<svg class="map"`, "more places"}},
		{"two cities of one country", "/prefix/198.18.1.0/24", []string{
			`<td>Lisbon</td><td><a href="/portugal-asn-list">Portugal</a></td><td class="n">50.0%</td>`, `<td>Porto</td><td><a href="/portugal-asn-list">Portugal</a></td><td class="n">50.0%</td>`, `<p><a href="/portugal-asn-list">Portugal</a></p>`,
		}, []string{`<svg class="map"`, `class="here"`, `class="aim"`, "Unknown"}},
		{"part of a prefix, a gap and one place", "/prefix/198.18.0.0/23", []string{
			`<td>Lisbon</td><td><a href="/portugal-asn-list">Portugal</a></td><td class="n">`, `<td>Unknown</td>`,
		}, nil},
	} {
		w := get(s, "GET", c.target)
		body := w.Body.String()
		if w.Code != 200 {
			t.Errorf("%s: status %d", c.name, w.Code)
			continue
		}
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
	// The order: biggest first.
	body := get(s, "GET", "/prefix/198.18.0.0/24").Body.String()
	if strings.Index(body, "<td>Lisbon</td>") > strings.Index(body, ">Spain</a></td>") {
		t.Error("places are not biggest first")
	}

	// Over the record limit nothing is said about the place: not a partial answer, not "unknown".
	s.placeLimit = 1
	for _, target := range []string{"/prefix/198.18.0.0/24", "/prefix/198.18.1.0/24"} {
		body = get(s, "GET", target).Body.String()
		for _, x := range []string{"Places", "Location unknown", "<p>", `<svg class="map"`, "Unknown"} {
			if strings.Contains(body, x) {
				t.Errorf("%s over the limit has %q", target, x)
			}
		}
	}
	// Exactly at the limit (2 records) it still shows.
	s.placeLimit = 2
	if body = get(s, "GET", "/prefix/198.18.0.0/24").Body.String(); !strings.Contains(body, "<h2>Places</h2>") {
		t.Error("exactly at the limit: no places")
	}
	s.placeLimit = placeRecords
	if s.placeLimit != 20000 {
		t.Errorf("limit %d", s.placeLimit)
	}

	// No MMDB open: nothing is known.
	empty := prefixServer(t)
	empty.geo = &mmdbFile{}
	if body = get(empty, "GET", "/prefix/203.0.113.0/24").Body.String(); !strings.Contains(body, "Location unknown") {
		t.Error("no MMDB: not unknown")
	}
}

func TestPrefixWithinInsideOneRecord(t *testing.T) {
	s := prefixServer(t)
	// A prefix inside one larger record: the library gives the record's own
	// prefix, which within cuts to the asked prefix.
	for _, c := range []struct {
		p       string
		places  int
		partial bool
		total   float64
	}{
		{"203.0.113.64/26", 1, false, 64},
		{"203.0.113.0/24", 1, false, 256},
		{"203.0.0.0/16", 1, true, 65536},
		{"192.0.2.0/24", 0, true, 256}, // no record
		{"198.18.0.0/24", 2, true, 256},
		{"2001:db8:1::/48", 1, false, 1 << 80},
	} {
		sum, ok := s.geo.within(netip.MustParsePrefix(c.p), 100)
		if !ok || len(sum.places) != c.places || sum.partial != c.partial || sum.total != c.total {
			t.Errorf("within(%s) = %d places, partial %v, total %g, ok %v", c.p, len(sum.places), sum.partial, sum.total, ok)
		}
	}
	sum, _ := s.geo.within(netip.MustParsePrefix("203.0.113.64/26"), 100)
	if sum.places[0].amount != 64 || sum.places[0].rec.CityName != "Lisbon" {
		t.Errorf("a piece of one record: %+v", sum.places)
	}
	if _, ok := s.geo.within(netip.MustParsePrefix("198.18.0.0/24"), 1); ok {
		t.Error("two records with a limit of 1")
	}
}

func TestPrefixLinksOnOtherPages(t *testing.T) {
	s := prefixServer(t)
	// A network page links the canonical in-range prefixes and leaves the others as text.
	body := get(s, "GET", "/as64540").Body.String()
	for _, x := range []string{`<li><a href="/prefix/8.8.8.0/24">8.8.8.0/24</a>`, "<li>2.0.0.0/7", "<li>2001:DB8:3::/48"} {
		if !strings.Contains(body, x) {
			t.Errorf("network page lacks %q", x)
		}
	}
	if strings.Contains(body, `/prefix/2.0.0.0/7`) || strings.Contains(body, `/prefix/2001:DB8`) {
		t.Error("a prefix with no page is linked")
	}
	if b := get(s, "GET", "/as64500").Body.String(); !strings.Contains(b, `<li><a href="/prefix/2001:db8::/32">2001:db8::/32</a>`) {
		t.Error("IPv6 prefix not linked")
	}

	// The Network panel shows the longest announced prefix in its Prefix cell and links it.
	for _, c := range []struct{ target, want string }{
		{"/?203.0.113.9", `<div><span>Prefix</span><b><a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></b></div>`},
		{"/?203.0.113.200", `<div><span>Prefix</span><b><a href="/prefix/203.0.113.128/25">203.0.113.128/25</a></b></div>`}, // the MMDB says /24
		{"/?2001:db8::5", `<a href="/prefix/2001:db8::/32">2001:db8::/32</a>`},
	} {
		if b := get(s, "GET", c.target).Body.String(); !strings.Contains(b, c.want) {
			t.Errorf("%s: no %q", c.target, c.want)
		}
	}
	// No announced prefix: no prefix part, a dash. (198.51.100.1 is announced by 64500; 192.0.2.1 is in no file.)
	if b := get(s, "GET", "/?192.0.2.1").Body.String(); !strings.Contains(b, "<dt>Network</dt><dd>—</dd>") || strings.Contains(b, "<dt>Prefix") {
		t.Error("an address with no prefix")
	}
	// The visitor's page stays private; its Network panel links the prefix.
	w := fromPeer(s, "/", "127.0.0.1:5000", "X-Real-IP", "203.0.113.9")
	if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("ETag") != "" || !strings.Contains(w.Body.String(), `<div><span>Prefix</span><b><a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></b></div>`) {
		t.Errorf("visitor page: %v", w.Header())
	}
	// Without the ASN data the MMDB tree prefix is linked in the Prefix row.
	if b := get(fixtureServer(t), "GET", "/?203.0.113.9").Body.String(); !strings.Contains(b, `<dt>Prefix</dt><dd><a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></dd>`) {
		t.Errorf("no ASN data: %s", b)
	}
}

func TestPrefixLookupBox(t *testing.T) {
	s := prefixServer(t)
	bare := fixtureServer(t)
	for _, c := range []struct{ target, loc string }{
		{"/?8.8.8.0/24", "/prefix/8.8.8.0/24"},
		{"/?q=8.8.8.0/24", "/prefix/8.8.8.0/24"},
		{"/?q=8.8.8.0%2F24", "/prefix/8.8.8.0/24"},
		{"/?q=+8.8.8.9/24+", "/prefix/8.8.8.0/24"},
		{"/?8.8.8.0%2F24", "/prefix/8.8.8.0/24"},
		{"/?2001:4860::/32", "/prefix/2001:4860::/32"},
		{"/?q=2001%3A4860%3A%3A%2F32", "/prefix/2001:4860::/32"},
		{"/?q=2001:DB8::1/32", "/prefix/2001:db8::/32"},
		{"/?q=::ffff:8.8.8.0/120", "/prefix/8.8.8.0/24"},
	} {
		for name, srv := range map[string]*server{"data": s, "no data": bare} {
			w := get(srv, "GET", c.target)
			if w.Code != 302 || w.Header().Get("Location") != c.loc || w.Header().Get("Cache-Control") != "public, max-age=300" {
				t.Errorf("%s (%s): status %d, Location %q, Cache-Control %q", c.target, name, w.Code, w.Header().Get("Location"), w.Header().Get("Cache-Control"))
			}
		}
	}
	for _, target := range []string{"/?8.8.8.0/7", "/?q=8.8.8.0/33", "/?q=2001:db8::/129", "/?q=2001:db8::/15", "/?q=8.8.8/24", "/?q=8.8.8.0/24/1", "/?1.2.3.4/"} {
		w := get(s, "GET", target)
		if w.Code != 400 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), badMsg) {
			t.Errorf("%s: status %d, Cache-Control %q", target, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	// Other input keeps its order: an address, an AS number, a name.
	for target, loc := range map[string]string{"/?8.8.8.8": "", "/?AS64500": "/as64500", "/?q=64500": "/as64500"} {
		w := get(s, "GET", target)
		if loc == "" && w.Code != 200 || loc != "" && w.Header().Get("Location") != loc {
			t.Errorf("%s: %d %q", target, w.Code, w.Header().Get("Location"))
		}
	}
	// The redirect itself holds nothing from the request but the canonical prefix.
	if w := get(s, "GET", "/?q=8.8.8.9/24"); strings.Contains(w.Body.String(), "8.8.8.9") || strings.Contains(fmt.Sprint(w.Header()), "8.8.8.9") {
		t.Error("the redirect echoes the input")
	}
}

func TestPrefixPagesHaveNoVisitorData(t *testing.T) {
	s := prefixServer(t)
	for _, target := range []string{"/prefix/203.0.113.0/24", "/prefix/9.9.10.0/24", "/prefix/foo"} {
		w := get(s, "GET", target, "X-Real-IP", "203.0.113.77")
		if strings.Contains(w.Body.String(), "203.0.113.77") || strings.Contains(w.Header().Get("Cache-Control"), "private") {
			t.Errorf("%s: visitor data or private cache", target)
		}
		if w.Code == 200 && (!strings.Contains(w.Body.String(), `value=""`) || strings.Count(w.Body.String(), "<script") != 3) {
			t.Errorf("%s: lookup bar or scripts", target)
		}
	}
}

// The head of the network, organization and prefix pages is a name band with
// the facts as cells: no two halves, no map and no definition list; a prefix
// that is not announced exactly has only the cells Addresses and Part of.
func TestNameBandAndCells(t *testing.T) {
	s := prefixServer(t)
	for _, target := range []string{"/as64500", "/asn-example-net", "/prefix/203.0.113.0/24", "/prefix/203.0.113.0/25"} {
		b := get(s, "GET", target).Body.String()
		for _, x := range []string{`<div class="w band">`, `<div class="cells">`} {
			if !strings.Contains(b, x) {
				t.Errorf("%s: page lacks %q", target, x)
			}
		}
		for _, x := range []string{`<svg class="map"`, "<dl>", "<dt>", `class="conn w"`, "mapped"} {
			if strings.Contains(b, x) {
				t.Errorf("%s: page has %q", target, x)
			}
		}
	}
	b := get(s, "GET", "/prefix/203.0.113.0/25").Body.String()
	_, cells, _ := strings.Cut(b, `<div class="cells">`)
	cells, _, _ = strings.Cut(cells, "</section>")
	if n := strings.Count(cells, "<div><span>"); n != 2 || !strings.Contains(cells, "<span>Addresses</span>") || !strings.Contains(cells, "<span>Part of</span>") {
		t.Errorf("a prefix that is not announced exactly: %d cells, want Addresses and Part of: %s", n, cells)
	}
}
