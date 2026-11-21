package main

// Tests of the SEO basics: titles, descriptions, canonical links, robots.txt
// and the sitemaps.

import (
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func metaOf(body, name string) string {
	m := regexp.MustCompile(`<meta name="` + name + `" content="([^"]*)">`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

func canonOf(body string) string {
	m := regexp.MustCompile(`<link rel="canonical" href="([^"]*)">`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestSEOHead(t *testing.T) {
	s, _ := asnServer(t)
	ps := prefixServer(t)
	for _, c := range []struct {
		srv                *server
		target, title      string
		desc, canon        string
		noDesc, noCanonURL bool
	}{
		{s, "/", homeTitle, homeDesc, "https://openipdata.org/", false, false},
		{s, "/as64500", "AS64500 Example Net: IP ranges and prefixes",
			"AS64500 Example Net is a network in Portugal announcing 2 IPv4 prefixes and 1 IPv6 prefix.", "https://openipdata.org/as64500", false, false},
		{s, "/asn-example-net", "Example Net IP ranges and networks",
			"Example Net operates 2 networks with 768 IPv4 addresses.", "https://openipdata.org/asn-example-net", false, false},
		{s, "/top-asn-by-ip-address", "Top networks by IPv4 addresses · openipdata.org",
			"The largest networks (autonomous systems) ranked by announced IPv4 addresses.", "https://openipdata.org/top-asn-by-ip-address", false, false},
		{s, "/top-asn-by-ipv6", "Top networks by IPv6 space · openipdata.org", "", "https://openipdata.org/top-asn-by-ipv6", false, false},
		{s, "/top-organizations-by-ip-address", "Top organizations by IPv4 addresses · openipdata.org", "", "https://openipdata.org/top-organizations-by-ip-address", false, false},
		{s, "/list-of-countries-by-ipv4-allocation", "Countries by IPv4 addresses · openipdata.org", "", "https://openipdata.org/list-of-countries-by-ipv4-allocation", false, false},
		{ps, "/prefix/203.0.113.0/24", "203.0.113.0/24 Example Net AS64500",
			"203.0.113.0/24 is announced by AS64500 Example Net and located in Lisbon, Portugal.", "https://openipdata.org/prefix/203.0.113.0/24", false, false},
	} {
		body := get(c.srv, "GET", c.target).Body.String()
		if got := regexp.MustCompile(`<title>([^<]*)</title>`).FindStringSubmatch(body); got == nil || got[1] != c.title {
			t.Errorf("%s: title %q, want %q", c.target, got, c.title)
		}
		desc := metaOf(body, "description")
		if desc == "" || (c.desc != "" && desc != c.desc) || len([]rune(desc)) > 155 {
			t.Errorf("%s: description %q", c.target, desc)
		}
		if got := canonOf(body); got != c.canon {
			t.Errorf("%s: canonical %q, want %q", c.target, got, c.canon)
		}
	}
	// No description and no canonical link on pages that are not indexed.
	for _, c := range []struct {
		srv    *server
		target string
	}{
		{s, "/?2001:db8::1"}, {s, "/?1.2.3"}, {s, "/as64999"}, {s, "/asn-nope"}, {s, "/status"},
		{ps, "/prefix/203.0.113.0/25"}, {ps, "/prefix/203.0.113.0/8"},
	} {
		w := get(c.srv, "GET", c.target)
		body := w.Body.String()
		if strings.Contains(body, `rel="canonical"`) || strings.Contains(body, `name="description"`) {
			t.Errorf("%s (%d): canonical or description on a page that is not indexed", c.target, w.Code)
		}
		if w.Code != 200 || c.target == "/status" || strings.Contains(c.target, "/25") {
			if !strings.Contains(body, `<meta name="robots" content="noindex">`) && w.Code != 301 {
				t.Errorf("%s (%d): no noindex", c.target, w.Code)
			}
		}
	}
	// A canonical link is never on a noindex page.
	for _, target := range []string{"/", "/as64500", "/asn-example-net", "/top-asn-by-ip-address", "/prefix/203.0.113.0/25", "/?2001:db8::1", "/status"} {
		body := get(ps, "GET", target).Body.String()
		if strings.Contains(body, `content="noindex"`) && strings.Contains(body, `rel="canonical"`) {
			t.Errorf("%s: noindex page with a canonical link", target)
		}
	}
}

func TestSEOSiteURL(t *testing.T) {
	s, _ := asnServer(t)
	s.origin = "http://127.0.0.1:9000"
	if body := get(s, "GET", "/").Body.String(); canonOf(body) != "http://127.0.0.1:9000/" {
		t.Errorf("home canonical %q", canonOf(body))
	}
	if body := get(s, "GET", "/as64500").Body.String(); canonOf(body) != "http://127.0.0.1:9000/as64500" {
		t.Errorf("network canonical %q", canonOf(body))
	}
	if body := get(s, "GET", "/robots.txt").Body.String(); !strings.Contains(body, "Sitemap: http://127.0.0.1:9000/sitemap.xml\n") {
		t.Errorf("robots %q", body)
	}
	for _, in := range []string{"https://openipdata.org/", "https://openipdata.org"} {
		if o, err := checkOrigin(in); err != nil || o != "https://openipdata.org" {
			t.Errorf("checkOrigin(%q) = %q, %v", in, o, err)
		}
	}
	for _, in := range []string{"openipdata.org", "http://example.org", "https://openipdata.org/x", "https://u@openipdata.org"} {
		if _, err := checkOrigin(in); err == nil {
			t.Errorf("checkOrigin(%q) accepted", in)
		}
	}
}

func TestCutWords(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want string
	}{
		{"Short", 10, "Short"},
		{"Deutsche Telekom AG", 14, "Deutsche"},
		{"Deutsche Telekom AG", 15, "Deutsche"},
		{"Deutsche Telekom AG", 16, "Deutsche Telekom"},
		{"Verylongsinglewordname", 8, "Verylong"},
		{"  spaced   out  ", 20, "spaced out"},
		{"Ünïcödé Nëtwörk Ltd", 14, "Ünïcödé"},
	} {
		if got := cutWords(c.in, c.max); got != c.want {
			t.Errorf("cutWords(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
	long := strings.Repeat("Very Long Organization Name ", 5)
	title, desc := orgMeta(long, 3, 10)
	if n := len([]rune(title)); n > titleLimit || !strings.HasSuffix(title, " IP ranges and networks") {
		t.Errorf("org title %q (%d)", title, n)
	}
	if len([]rune(desc)) > 155 {
		t.Errorf("org description too long: %q", desc)
	}
	title, desc = networkMeta(4294967295, long, "United States of America", 1234567, 7654321, true)
	if n := len([]rune(title)); n > titleLimit || !strings.HasSuffix(title, ": IP ranges and prefixes") {
		t.Errorf("network title %q (%d)", title, n)
	}
	if len([]rune(desc)) > 155 {
		t.Errorf("network description too long (%d): %q", len(desc), desc)
	}
	if title, _ := prefixMeta("2001:db8:1234:5678:9abc:def0:1234:5678/128", 4294967295, "Example Net", ""); title != "2001:db8:1234:5678:9abc:def0:1234:5678/128 AS4294967295" {
		t.Errorf("no room for the name: %q", title)
	}
	if title, _ := prefixMeta("2001:db8:1234:5678:9abc:def0::/96", 4294967295, long, ""); len([]rune(title)) > titleLimit {
		t.Errorf("prefix title %q", title)
	}
}

func TestRobotsTxt(t *testing.T) {
	s, _ := asnServer(t)
	want := "User-agent: *\nDisallow: /api/\nDisallow: /resolver\nDisallow: /ip\nDisallow: /json\nSitemap: https://openipdata.org/sitemap.xml\n"
	w := get(s, "GET", "/robots.txt")
	if w.Code != 200 || w.Body.String() != want {
		t.Errorf("robots.txt: %d %q", w.Code, w.Body.String())
	}
	if h := w.Header(); h.Get("Content-Type") != "text/plain; charset=utf-8" || h.Get("Cache-Control") != "public, max-age=3600" {
		t.Errorf("headers %v", h)
	}
	if w := get(s, "HEAD", "/robots.txt"); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d bytes", w.Code, w.Body.Len())
	}
	if w := get(s, "POST", "/robots.txt"); w.Code != 405 {
		t.Errorf("POST: %d", w.Code)
	}
	// /status and /report/ pages stay open to crawlers but carry noindex.
	if body := get(s, "GET", "/status").Body.String(); !strings.Contains(body, `<meta name="robots" content="noindex">`) {
		t.Error("/status has no noindex")
	}
}

type urlset struct {
	URLs []string `xml:"url>loc"`
}

type sitemapIndex struct {
	Sitemaps []string `xml:"sitemap>loc"`
}

func parseXML(t *testing.T, body string, v any) {
	t.Helper()
	if err := xml.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("not well-formed XML: %v\n%.300s", err, body)
	}
}

func TestSitemaps(t *testing.T) {
	s, _ := asnServer(t)
	check := func(target string) (string, *urlset) {
		t.Helper()
		w := get(s, "GET", target)
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/xml; charset=utf-8" || w.Header().Get("Cache-Control") != "public, max-age=3600" {
			t.Fatalf("%s: %d %v", target, w.Code, w.Header())
		}
		var u urlset
		parseXML(t, w.Body.String(), &u)
		return w.Body.String(), &u
	}

	// Index: 646 networks with prefixes except AS64502 (none), 5 organizations plus the two big ones.
	w := get(s, "GET", "/sitemap.xml")
	var idx sitemapIndex
	parseXML(t, w.Body.String(), &idx)
	want := []string{"https://openipdata.org/sitemap-pages.xml", "https://openipdata.org/sitemap-networks-1.xml", "https://openipdata.org/sitemap-organizations-1.xml", "https://openipdata.org/sitemap-countries.xml"}
	if fmt.Sprint(idx.Sitemaps) != fmt.Sprint(want) || !strings.Contains(w.Body.String(), `xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"`) {
		t.Errorf("index %v", idx.Sitemaps)
	}

	_, pages := check("/sitemap-pages.xml")
	if fmt.Sprint(pages.URLs) != fmt.Sprint([]string{"https://openipdata.org/", "https://openipdata.org/download", "https://openipdata.org/bulk-ip-lookup", "https://openipdata.org/geolite2-alternative", "https://openipdata.org/top-asn-by-ip-address", "https://openipdata.org/top-asn-by-ipv6",
		"https://openipdata.org/top-organizations-by-ip-address", "https://openipdata.org/list-of-countries-by-ipv4-allocation"}) {
		t.Errorf("pages %v", pages.URLs)
	}

	_, nets := check("/sitemap-networks-1.xml")
	if len(nets.URLs) != 645 || nets.URLs[0] != "https://openipdata.org/as64500" || strings.Contains(strings.Join(nets.URLs, " "), "/as64502") {
		t.Errorf("networks: %d, first %q", len(nets.URLs), nets.URLs[0])
	}
	for i := 1; i < len(nets.URLs); i++ {
		var a, b int
		fmt.Sscanf(nets.URLs[i-1], "https://openipdata.org/as%d", &a)
		fmt.Sscanf(nets.URLs[i], "https://openipdata.org/as%d", &b)
		if a >= b {
			t.Fatalf("networks not ordered: %d then %d", a, b)
		}
	}
	_, orgs := check("/sitemap-organizations-1.xml")
	if len(orgs.URLs) != 5 || !strings.Contains(strings.Join(orgs.URLs, " "), "https://openipdata.org/asn-example-net") || strings.Contains(strings.Join(orgs.URLs, " "), "Odd") {
		t.Errorf("organizations %v", orgs.URLs)
	}

	w = get(s, "HEAD", "/sitemap.xml")
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD: %d, %d bytes", w.Code, w.Body.Len())
	}
	if w := get(s, "POST", "/sitemap.xml"); w.Code != 405 {
		t.Errorf("POST: %d", w.Code)
	}
	for _, target := range []string{"/sitemap-networks-0.xml", "/sitemap-networks-2.xml", "/sitemap-networks-01.xml", "/sitemap-networks-x.xml", "/sitemap-networks-.xml",
		"/sitemap-networks-1", "/sitemap-organizations-2.xml", "/sitemap-organizations--1.xml", "/sitemap-other.xml", "/sitemap-networks-99999999999999999999.xml", "/sitemap"} {
		if w := get(s, "GET", target); w.Code != 404 || strings.Contains(w.Body.String(), "<urlset") {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
}

func TestSitemapChunks(t *testing.T) {
	old := sitemapChunk
	sitemapChunk = 100
	defer func() { sitemapChunk = old }()
	s, _ := asnServer(t)
	var idx sitemapIndex
	parseXML(t, get(s, "GET", "/sitemap.xml").Body.String(), &idx)
	// 645 networks = 7 files; 5 organizations = 1 file.
	if len(idx.Sitemaps) != 1+7+1+1 || idx.Sitemaps[7] != "https://openipdata.org/sitemap-networks-7.xml" || idx.Sitemaps[8] != "https://openipdata.org/sitemap-organizations-1.xml" {
		t.Errorf("index %v", idx.Sitemaps)
	}
	seen := map[string]bool{}
	for k := 1; k <= 7; k++ {
		var u urlset
		parseXML(t, get(s, "GET", fmt.Sprintf("/sitemap-networks-%d.xml", k)).Body.String(), &u)
		want := 100
		if k == 7 {
			want = 45
		}
		if len(u.URLs) != want {
			t.Errorf("file %d: %d urls, want %d", k, len(u.URLs), want)
		}
		for _, x := range u.URLs {
			if seen[x] {
				t.Errorf("%s twice", x)
			}
			seen[x] = true
		}
	}
	if len(seen) != 645 {
		t.Errorf("%d networks in all", len(seen))
	}
	if w := get(s, "GET", "/sitemap-networks-8.xml"); w.Code != 404 {
		t.Errorf("past the end: %d", w.Code)
	}
}

func TestSitemapsWithoutData(t *testing.T) {
	s := fixtureServer(t) // no ASN data
	var idx sitemapIndex
	w := get(s, "GET", "/sitemap.xml")
	parseXML(t, w.Body.String(), &idx)
	if w.Code != 200 || len(idx.Sitemaps) != 1 || idx.Sitemaps[0] != "https://openipdata.org/sitemap-pages.xml" {
		t.Errorf("index %d %v", w.Code, idx.Sitemaps)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("incomplete index cache %q", cc)
	}
	if w := get(s, "GET", "/sitemap-pages.xml"); w.Code != 200 {
		t.Errorf("pages: %d", w.Code)
	}
	for _, target := range []string{"/sitemap-networks-1.xml", "/sitemap-organizations-1.xml"} {
		if w := get(s, "GET", target); w.Code != 503 || w.Header().Get("Retry-After") == "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %v", target, w.Code, w.Header())
		}
	}
	if w := get(s, "GET", "/sitemap-networks-x.xml"); w.Code != 404 {
		t.Errorf("malformed k without data: %d", w.Code)
	}
}

func TestSEOHomeVariantsNotIndexed(t *testing.T) {
	s, _ := asnServer(t)
	for _, target := range []string{"/?2001:db8::1", "/?203.0.113.9"} {
		body := get(s, "GET", target).Body.String()
		if strings.Contains(body, `rel="canonical"`) || strings.Contains(body, `name="description"`) {
			t.Errorf("%s: canonical or description", target)
		}
	}
	// No client address (no X-Real-IP): "Address unknown".
	w := get(s, "GET", "/")
	t.Logf("address unknown home: canonical %q, description %q", canonOf(w.Body.String()), metaOf(w.Body.String(), "description"))
}
