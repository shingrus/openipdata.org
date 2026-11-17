package main

// Tests of the h1 of the entity pages, the prefixes.txt lists, the breadcrumb
// data and the added links.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

var ldRe = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)

func crumbsOf(t *testing.T, body string) []ldCrumb {
	t.Helper()
	m := ldRe.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	var l ldCrumbs
	if err := json.Unmarshal([]byte(m[1]), &l); err != nil {
		t.Fatalf("JSON-LD: %v", err)
	}
	if l.Context != "https://schema.org" || l.Type != "BreadcrumbList" || strings.Contains(m[1], "<") {
		t.Errorf("JSON-LD head %+v", l)
	}
	return l.Items
}

func crumbText(l []ldCrumb) string {
	var o []string
	for _, c := range l {
		o = append(o, fmt.Sprintf("%d %s %s", c.Position, c.Name, c.Item))
	}
	return strings.Join(o, " | ")
}

func TestOneH1(t *testing.T) {
	s, _ := asnServer(t)
	p := prefixServer(t)
	for _, c := range []struct {
		srv        *server
		path, h1   string
		h2Contains string
	}{
		{s, "/as64500", "<h1>AS64500</h1>", `<h2>Example Net</h2>`},
		{s, "/asn-example-net", "<h1>Example Net</h1>", `<h2>Organization</h2>`},
		{p, "/prefix/203.0.113.0/24", "<h1>203.0.113.0/24</h1>", `<h2>Prefix</h2>`},
		{p, "/prefix/203.0.113.0/26", "<h1>203.0.113.0/26</h1>", `<h2>Prefix</h2>`}, // covered, not announced
		{s, "/as1", "<h1>Not found</h1>", ""},
		{s, "/asn-nothing", "<h1>Not found</h1>", ""},
		{s, "/top-asn-by-ip-address", "<h1>Top networks by IPv4 addresses</h1>", ""},
	} {
		w := get(c.srv, "GET", c.path)
		body := w.Body.String()
		if n := strings.Count(body, "<h1"); n != 1 || !strings.Contains(body, c.h1) {
			t.Errorf("%s: %d h1, want %s", c.path, n, c.h1)
		}
		if c.h2Contains != "" && !strings.Contains(body, c.h2Contains) {
			t.Errorf("%s: no %s", c.path, c.h2Contains)
		}
	}
	if b := get(s, "GET", "/asn-nothing").Body.String(); strings.Contains(b, "ld+json") {
		t.Error("JSON-LD on a 404")
	}
}

func TestPrefixListRoutes(t *testing.T) {
	s := prefixServer(t)
	w := get(s, "GET", "/as64500/prefixes.txt")
	want := "198.18.0.0/24\n198.51.100.0/24\n203.0.113.0/24\n2001:db8::/32\n"
	if w.Code != 200 || w.Body.String() != want {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	h := w.Header()
	if h.Get("Content-Type") != "text/plain; charset=utf-8" || h.Get("Cache-Control") != asnCache || h.Get("X-Robots-Tag") != "noindex" || h.Get("ETag") == "" {
		t.Errorf("headers %v", h)
	}
	if n := get(s, "GET", "/as64500/prefixes.txt", "If-None-Match", h.Get("ETag")); n.Code != 304 || n.Body.Len() != 0 {
		t.Errorf("304: %d", n.Code)
	}
	if hd := get(s, "HEAD", "/as64500/prefixes.txt"); hd.Code != 200 || hd.Body.Len() != 0 || hd.Header().Get("Content-Length") != fmt.Sprint(len(want)) {
		t.Errorf("HEAD %d %d", hd.Code, hd.Body.Len())
	}
	if m := get(s, "POST", "/as64500/prefixes.txt"); m.Code != 405 {
		t.Errorf("POST %d", m.Code)
	}
	// IPv4 in address order, then IPv6; a prefix of two networks of one organization is written once.
	rows := append(prefixRows(), fixtureRow{asn: 64502, handle: "EXAMPLE-3", org: "Example Net", slug: "example-net", v4Prefixes: []string{"203.0.113.0/24", "10.1.0.0/16"}, v6Prefixes: []string{"2001:db8::/32", "2001:db7::/32"}})
	rewrite(t, s, rows)
	org := get(s, "GET", "/asn-example-net/prefixes.txt")
	if org.Code != 200 || org.Body.String() != "10.1.0.0/16\n198.18.0.0/24\n198.51.100.0/24\n203.0.113.0/24\n2001:db7::/32\n2001:db8::/32\n" {
		t.Errorf("org list %d %q", org.Code, org.Body.String())
	}
	if org.Header().Get("X-Robots-Tag") != "noindex" {
		t.Error("org list indexable")
	}
	// A known network with nothing announced: 200, empty. Odd Net has JSON the index skips only partly; use 64541 (not JSON).
	if e := get(s, "GET", "/as64541/prefixes.txt"); e.Code != 200 || e.Body.Len() != 0 {
		t.Errorf("empty: %d %q", e.Code, e.Body.String())
	}
	for _, p := range []string{"/AS64500/prefixes.txt", "/as064500/prefixes.txt", "/as0/prefixes.txt", "/as99999/prefixes.txt", "/asn-nothing/prefixes.txt",
		"/asn-Example-Net/prefixes.txt", "/asn-example-net/x/prefixes.txt", "/as64500/prefixes.txt/", "/as64500/Prefixes.txt", "/as64500/prefixes.txt2"} {
		if w := get(s, "GET", p); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		} else if strings.Contains(w.Body.String(), "<") || w.Header().Get("Location") != "" {
			t.Errorf("%s: not plain: %q", p, w.Body.String())
		}
	}
	// Existing paths are unchanged.
	for p, code := range map[string]int{"/as64500": 200, "/asn-example-net": 200, "/AS64500": 301, "/prefix/203.0.113.0/24": 200} {
		if w := get(s, "GET", p); w.Code != code {
			t.Errorf("%s: %d, want %d", p, w.Code, code)
		}
	}
	// No data: 503, no-store.
	n, err := newServer("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/as64500/prefixes.txt", "/asn-example-net/prefixes.txt"} {
		w := get(n, "GET", p)
		if w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Retry-After") == "" {
			t.Errorf("%s no data: %d %v", p, w.Code, w.Header())
		}
	}
}

func rewrite(t *testing.T, s *server, rows []fixtureRow) {
	t.Helper()
	path := s.asnf.path
	writeASNFile(t, path, rows)
	os.Chtimes(path, time.Now(), time.Now().Add(time.Hour))
	s.check()
}

func TestPrefixListLinks(t *testing.T) {
	s := prefixServer(t)
	net := get(s, "GET", "/as64500").Body.String()
	if !strings.Contains(net, `<h2>Prefixes</h2><a class="r" href="/as64500/prefixes.txt">prefixes.txt</a>`) {
		t.Error("network page has no list link")
	}
	if org := get(s, "GET", "/asn-example-net").Body.String(); !strings.Contains(org, `<a class="r" href="/asn-example-net/prefixes.txt">prefixes.txt</a>`) {
		t.Error("organization page has no list link")
	}
	if strings.Contains(get(s, "GET", "/as64541").Body.String(), "prefixes.txt") {
		t.Error("link on a network with no prefixes")
	}
	// A prefix page links the list of its network, in the head of "Other prefixes".
	if b := get(s, "GET", "/prefix/203.0.113.0/24").Body.String(); strings.Count(b, `href="/as64500/prefixes.txt"`) != 1 || !strings.Contains(b, `<h2>Other prefixes of AS64500</h2><a class="r" href="/as64500/prefixes.txt">prefixes.txt</a>`) {
		t.Error("prefix page: the list link of its network is missing or repeated")
	}
	if !strings.Contains(get(s, "GET", "/llms.txt").Body.String(), "/as<number>/prefixes.txt") || !strings.Contains(get(s, "GET", "/llms.txt").Body.String(), "/asn-<slug>/prefixes.txt") {
		t.Error("llms.txt lacks the patterns")
	}
}

func TestBreadcrumbs(t *testing.T) {
	s := prefixServer(t)
	for _, c := range []struct{ path, want string }{
		{"/as64500", "1 Networks https://openipdata.org/top-asn-by-ip-address | 2 Example Net https://openipdata.org/asn-example-net | 3 AS64500 https://openipdata.org/as64500"},
		{"/as64550", "1 Networks https://openipdata.org/top-asn-by-ip-address | 2 Net One https://openipdata.org/asn-net-one | 3 AS64550 https://openipdata.org/as64550"},
		{"/as64551", "1 Networks https://openipdata.org/top-asn-by-ip-address | 2 AS64551 https://openipdata.org/as64551"}, // no slug
		{"/asn-example-net", "1 Organizations https://openipdata.org/top-organizations-by-ip-address | 2 Example Net https://openipdata.org/asn-example-net"},
		{"/prefix/203.0.113.0/24", "1 Networks https://openipdata.org/top-asn-by-ip-address | 2 AS64500 https://openipdata.org/as64500 | 3 203.0.113.0/24 https://openipdata.org/prefix/203.0.113.0/24"},
	} {
		w := get(s, "GET", c.path)
		if w.Code != 200 {
			t.Fatalf("%s: %d", c.path, w.Code)
		}
		if got := crumbText(crumbsOf(t, w.Body.String())); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.path, got, c.want)
		}
	}
	// None on noindex pages, errors, searches, lookup pages and the other pages.
	for _, p := range []string{"/prefix/198.18.0.0/23", "/prefix/203.0.113.0/26", "/prefix/1.2.3.0/24", "/as1", "/asn-nothing", "/as0", "/?q=net", "/status", "/", "/top-asn-by-ip-address"} {
		w := get(s, "GET", p)
		if strings.Contains(w.Body.String(), "BreadcrumbList") {
			t.Errorf("%s: breadcrumb (status %d)", p, w.Code)
		}
	}
	// The data block needs no script hash.
	csp := get(s, "GET", "/as64500").Header().Get("Content-Security-Policy")
	if strings.Count(csp, "sha256-") != 2 || strings.Contains(csp, "ld+json") {
		t.Errorf("CSP has a hash for the data block: %s", csp) // only the two theme scripts
	}
}

func TestPrefixLinksAdded(t *testing.T) {
	s := prefixServer(t)
	b := get(s, "GET", "/prefix/203.0.113.0/24").Body.String()
	if !strings.Contains(b, `<span>Organization</span><b><a href="/asn-example-net">Example Net</a>`) {
		t.Error("network name does not link to the organization")
	}
	// A network with no organization page (no slug): the name, if any, stays plain.
	if b := get(s, "GET", "/prefix/198.18.8.0/24").Body.String(); strings.Contains(b, `<a href="/asn-">`) || strings.Contains(b, `<a href="/asn-"></a>`) {
		t.Error("empty organization link")
	}
	// Organization table: the country is plain text (hundreds of rows repeat it).
	// The head has the one link (the Country cell).
	if org := get(s, "GET", "/asn-example-net").Body.String(); strings.Count(org, `portugal-asn-list`) != 1 || !strings.Contains(org, `<td class="s">Portugal</td>`) {
		t.Errorf("organization table country is linked")
	}
	// The network page already linked the country.
	if !strings.Contains(get(s, "GET", "/as64500").Body.String(), `<a href="/portugal-asn-list">Portugal</a>`) {
		t.Error("network country is not linked")
	}
}

// A synthetic index of about 500,000 prefixes: one pass for one network.
func BenchmarkPrefixList(b *testing.B) {
	var x pfxIndex
	for i := 0; i < 400000; i++ {
		x.v4 = append(x.v4, pfx4{addr: uint32(i) << 8, asn: uint32(1 + i%5000), bits: 24})
	}
	for i := 0; i < 126000; i++ {
		x.v6 = append(x.v6, pfx6{hi: 0x2001<<48 | uint64(i)<<16, asn: uint32(1 + i%5000), bits: 48})
	}
	one := newASNSet([]uint32{77})
	many := make([]uint32, 300)
	for i := range many {
		many[i] = uint32(10 + i*13)
	}
	set := newASNSet(many)
	b.Run("one", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			x.prefixList(one, 4000)
		}
	})
	b.Run("org300", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			x.prefixList(set, 1<<20)
		}
	})
}

// The network page head: the place in the IPv4 ranking is the second line,
// the name band has no map and the rows are cells (like the organization and
// the prefix page); a network with no IPv4 addresses keeps its handle on the
// second line.
func TestNetworkHeadRank(t *testing.T) {
	s, _ := asnServer(t)
	d := s.asnf.data()
	var noV4 uint32
	for i := range d.nets {
		if n := &d.nets[i]; noV4 == 0 && n.v4 == 0 && n.org != "" && n.handle != "" {
			noV4 = n.asn
		}
	}
	if top := d.nets[d.topV4[0]].asn; d.rankV4(top) != 1 {
		t.Errorf("rank of the biggest network = %d", d.rankV4(top))
	}
	if d.rankV4(4294967295) != 0 {
		t.Error("rank of an unknown network")
	}
	b := get(s, "GET", "/as64500").Body.String()
	if !strings.Contains(b, "<p>#138 by IPv4 addresses</p>") {
		t.Error("network page: no rank line")
	}
	if !strings.Contains(b, `<div class="w band"><div class="place s"><h2>Example Net</h2>`) || !strings.Contains(b, `<div class="cells">`) || !strings.Contains(b, ".cells{display:grid") || !strings.Contains(b, ".band+.cells{grid-template-columns:repeat(4,") {
		t.Error("network page: not the name band and the cells, or no cells styles")
	}
	if strings.Contains(b, `<svg class="map"`) || strings.Contains(b, "mapped") || strings.Contains(b, `class="conn w"`) || strings.Contains(b, "<dl>") || strings.Contains(b, ".place svg.map{position:absolute") {
		t.Error("network page: a map, the two halves or the map styles")
	}
	if o := get(s, "GET", "/asn-example-net").Body.String(); !strings.Contains(o, `<div class="w band"><div class="place s"><h1>Example Net</h1>`) || !strings.Contains(o, `<div class="cells">`) || strings.Contains(o, `<svg class="map"`) || strings.Contains(o, "mapped") || strings.Contains(o, "<dl>") || strings.Contains(o, ".place svg.map{position:absolute") {
		t.Error("organization page: not the name band and the cells, or a map")
	}
	if noV4 != 0 {
		n, _ := d.find(noV4)
		if b := get(s, "GET", asnHref(noV4)).Body.String(); strings.Contains(b, "by IPv4 addresses</p>") || !strings.Contains(b, "<p>"+n.handle+"</p>") {
			t.Errorf("AS%d has no IPv4 addresses: second line is not its handle", noV4)
		}
	}
}
