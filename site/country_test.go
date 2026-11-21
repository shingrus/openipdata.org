package main

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"testing"
)

func TestCountrySlug(t *testing.T) {
	for name, want := range map[string]string{
		"United Kingdom":                   "united-kingdom",
		"Côte d'Ivoire":                    "cote-d-ivoire",
		"Åland Islands":                    "aland-islands",
		"São Tomé and Príncipe":            "sao-tome-and-principe",
		"Curaçao":                          "curacao",
		"Réunion":                          "reunion",
		"Türkiye":                          "turkiye",
		"Saint Barthélemy":                 "saint-barthelemy",
		"Bonaire, Sint Eustatius and Saba": "bonaire-sint-eustatius-and-saba",
		"ÉÀÑ  Øß Æ":                        "ean-oss-ae",
		"  --  ":                           "",
		"":                                 "",
		"中国":                               "",
	} {
		if got := countrySlug(name); got != want {
			t.Errorf("countrySlug(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCountrySlugIndex(t *testing.T) {
	d := &asnData{countries: []countryRow{
		{cc: "AA", name: "Same Name", v4: 100}, {cc: "BB", name: "Same-Name", v4: 50}, {cc: "CC", name: "Other", v4: 10},
		{cc: "ZZ", name: "中国", v4: 5}, {cc: "YY", name: "Same Name", v4: 1},
	}}
	d.indexCountries()
	want := map[string]string{"AA": "same-name", "BB": "same-name-bb", "CC": "other", "ZZ": "zz", "YY": "same-name-yy"}
	for cc, slug := range want {
		if d.cslug[cc] != slug || d.cbySlug[slug] != cc {
			t.Errorf("%s: slug %q (back %q), want %q", cc, d.cslug[cc], d.cbySlug[d.cslug[cc]], slug)
		}
	}
}

func TestCountryRoutes(t *testing.T) {
	s, _ := asnServer(t)
	for _, c := range []struct {
		name, target string
		code         int
		loc          string
		want, not    []string
	}{
		{"canonical", "/germany-asn-list", 200, "", []string{
			"<title>Germany ASN list: networks by IPv4 addresses</title>", "<h1>Germany ASN list</h1>", `<span class="r">520 networks</span>`,
			`<link rel="canonical" href="https://openipdata.org/germany-asn-list">`, `<a href="/as65519">AS65519</a>`, "more networks", `value=""`,
			"<th class=\"n s\">IPv6 /64 networks</th>", "<th class=\"n\">IPv4 addresses</th>", `<td><a href="/asn-huge-net">Huge Net</a></td>`,
		}, []string{`content="noindex"`, "France"}},
		{"upper case", "/Germany-ASN-list", 301, "/germany-asn-list", nil, nil},
		{"all upper", "/GERMANY-ASN-LIST", 301, "/germany-asn-list", nil, nil},
		{"code", "/de-asn-list", 301, "/germany-asn-list", nil, nil},
		{"code upper", "/DE-asn-list", 301, "/germany-asn-list", nil, nil},
		{"code mixed", "/Pt-asn-list", 301, "/portugal-asn-list", nil, nil},
		{"unknown code", "/xx-asn-list", 404, "", []string{"Country not found", `content="noindex"`}, nil},
		{"unknown", "/narnia-asn-list", 404, "", []string{"<h1>Not found</h1>", "Country not found", `content="noindex"`}, nil},
		{"bad characters", "/ger_many-asn-list", 404, "", nil, nil},
		{"no slug", "/-asn-list", 404, "", nil, []string{"Country not found"}},
		{"suffix only", "/asn-list", 404, "", nil, nil},
		{"small country", "/portugal-asn-list", 200, "", []string{"<h1>Portugal ASN list</h1>", `<span class="r">2 networks</span>`, `<a href="/as64500">AS64500</a>`}, []string{"more networks"}},
		{"spain only has networks with addresses", "/spain-asn-list", 200, "", []string{`<span class="r">3 networks</span>`}, nil},
	} {
		w := get(s, "GET", c.target)
		if w.Code != c.code {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.code)
		}
		if got := w.Header().Get("Location"); got != c.loc {
			t.Errorf("%s: Location %q, want %q", c.name, got, c.loc)
		}
		if c.loc != "" && w.Header().Get("Cache-Control") != asnCache {
			t.Errorf("%s: cache %q", c.name, w.Header().Get("Cache-Control"))
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

func TestCountryPageContent(t *testing.T) {
	s, _ := asnServer(t)
	w := get(s, "GET", "/germany-asn-list")
	body := w.Body.String()
	desc := metaOf(body, "description")
	if want := "All 520 networks (autonomous systems) registered in Germany, ranked by announced IPv4 addresses, with their organizations and IPv6 space."; desc != want {
		t.Errorf("description %q", desc)
	}
	// Ranked by IPv4 addresses, biggest first: 10+519 = 529 is AS65519; capped at 500 rows, 20 more.
	rows := regexp.MustCompile(`<td class="a"><a href="/as(\d+)">`).FindAllStringSubmatch(body, -1)
	if len(rows) != 500 || rows[0][1] != "65519" || rows[1][1] != "65518" {
		t.Errorf("%d rows, first %v", len(rows), rows[:2])
	}
	if !strings.Contains(body, "20 more networks") {
		t.Error("no note of the rest")
	}
	// ETag and 304; served with the page headers.
	etag := w.Header().Get("ETag")
	if etag == "" || w.Header().Get("Cache-Control") != edgeCache || w.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("headers %v", w.Header())
	}
	if w2 := get(s, "GET", "/germany-asn-list", "If-None-Match", etag); w2.Code != 304 || w2.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d", w2.Code)
	}
	if w2 := get(s, "HEAD", "/germany-asn-list"); w2.Code != 200 || w2.Body.Len() != 0 {
		t.Errorf("HEAD: %d", w2.Code)
	}
	// The same bytes every time (rendered once).
	if get(s, "GET", "/germany-asn-list").Header().Get("ETag") != etag {
		t.Error("ETag changed")
	}
	// A prefix of the lookup bar page and visitor data are not in it.
	if strings.Contains(body, "X-Real-IP") {
		t.Error("visitor data")
	}
	// Long names keep the title whole.
	if title, _ := countryMeta("Democratic Republic of the Congo", 12); title != "Democratic Republic of the Congo ASN list" {
		t.Errorf("long title %q", title)
	}
	if title, desc := countryMeta("Democratic Republic of the Congo", 123456); len([]rune(title)) > titleLimit || len([]rune(desc)) > 155 {
		t.Errorf("long meta %q %q", title, desc)
	}
}

func TestCountryWithoutData(t *testing.T) {
	s := fixtureServer(t)
	if w := get(s, "GET", "/germany-asn-list"); w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Retry-After") == "" {
		t.Errorf("no data: %d %v", w.Code, w.Header())
	}
	// The case alone needs no data.
	if w := get(s, "GET", "/Germany-asn-list"); w.Code != 301 || w.Header().Get("Location") != "/germany-asn-list" {
		t.Errorf("case without data: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get(s, "GET", "/de-asn-list"); w.Code != 503 {
		t.Errorf("code without data: %d", w.Code)
	}
	if w := get(s, "GET", "/sitemap-countries.xml"); w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("sitemap without data: %d", w.Code)
	}
}

func TestCountrySitemap(t *testing.T) {
	s, _ := asnServer(t)
	w := get(s, "GET", "/sitemap-countries.xml")
	var u urlset
	parseXML(t, w.Body.String(), &u)
	// By the Countries ranking: Germany (most IPv4), then France, Portugal, Spain.
	want := []string{"https://openipdata.org/germany-asn-list", "https://openipdata.org/france-asn-list", "https://openipdata.org/portugal-asn-list", "https://openipdata.org/spain-asn-list"}
	if w.Code != 200 || fmt.Sprint(u.URLs) != fmt.Sprint(want) || w.Header().Get("ETag") == "" {
		t.Errorf("sitemap: %d %v", w.Code, u.URLs)
	}
	if !strings.Contains(get(s, "GET", "/sitemap-index.xml").Body.String(), "/sitemap-countries.xml") {
		t.Error("the index does not list the countries file")
	}
	// Without data the index is not served.
	if w := get(fixtureServer(t), "GET", "/sitemap-index.xml"); w.Code != 503 || strings.Contains(w.Body.String(), "countries") {
		t.Errorf("index without data: %d", w.Code)
	}
}

func TestCountryLinks(t *testing.T) {
	s, _ := asnServer(t)
	s.public = func(a netip.Addr) bool { return true }
	// The ranking and the network page.
	if b := get(s, "GET", "/list-of-countries-by-ipv4-allocation").Body.String(); !strings.Contains(b, `<td><a href="/germany-asn-list">Germany</a></td>`) {
		t.Error("ranking has no link")
	}
	if b := get(s, "GET", "/as64500").Body.String(); !strings.Contains(b, `<span>Country</span><b><a href="/portugal-asn-list">Portugal</a></b>`) {
		t.Error("network page has no link")
	}
	// A network with no country has no link.
	if b := get(s, "GET", "/as64502").Body.String(); strings.Contains(b, "-asn-list") && !strings.Contains(b, `href="/list-of-countries`) {
		t.Error("link on a network without country")
	}
	// The panel: a city links the second line, a country alone the big line.
	b := get(s, "GET", "/?203.0.113.9").Body.String()
	if !strings.Contains(b, `<h1>Lisbon</h1><p><a href="/portugal-asn-list">Portugal</a></p>`) || !strings.Contains(b, `<svg class="map"`) {
		t.Errorf("panel with a city: %s", b[strings.Index(b, `class="place`):][:300])
	}
	b = get(s, "GET", "/?198.51.100.1").Body.String()
	if !strings.Contains(b, `<h1><a href="/spain-asn-list">Spain</a></h1>`) || strings.Contains(b, "<p><a") {
		t.Error("panel with a country only")
	}
	// Location unknown is no link.
	if b := get(s, "GET", "/?192.0.2.1").Body.String(); !strings.Contains(b, "<h1>Location unknown</h1>") {
		t.Error("unknown location")
	}
	// Without data: plain text.
	if b := get(fixtureServer(t), "GET", "/?203.0.113.9").Body.String(); strings.Contains(b, "-asn-list") {
		t.Error("link without data")
	}
	// A country the data does not have: plain text.
	if s.countryHref("QQ") != "" || s.countryHref("") != "" {
		t.Error("link for an unknown country")
	}
}
