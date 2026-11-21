package main

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestComparePage(t *testing.T) {
	s := fixtureServer(t)
	w := get(s, "GET", "/geolite2-alternative")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	b := w.Body.String()
	for _, want := range []string{
		`<link rel="canonical" href="https://openipdata.org/geolite2-alternative">`, "<h1>Free GeoLite2 alternative</h1>",
		"Same MMDB layout. Change the file path, keep your code.", "<h2>Why switch</h2>", `<a class="r" href="/download">Download &rarr;</a>`,
		"<h2>Free databases</h2>", "<h2>Accuracy</h2>", "<h2>Switch from GeoLite2</h2>", `reader = geoip2.database.Reader(&#34;openipdata-org-ip2geo.mmdb&#34;)`,
		`<a href="https://github.com/shingrus/openipdata.org/releases/latest/download/openipdata-org-ip2geo.mmdb">`,
		`<a href="https://db-ip.com">IP Geolocation by DB-IP</a> · Includes GeoLite data created by <a href="https://www.maxmind.com">MaxMind</a>`,
		"IP2Location LITE", "IPinfo Lite", "CC BY-SA", `class="cp"`, `<symbol id="i-ok"`, "<span>Layout</span><b>GeoLite2 ", "<span>Updated</span><b>2× a day ",
		`<a href="/geolite2-alternative">vs GeoLite2</a>`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(b, `name="robots"`) {
		t.Error("noindex on an indexable page")
	}
	if n := utf8.RuneCountInString(compareTitle); n > 60 || !strings.Contains(compareTitle, "GeoLite2") {
		t.Errorf("title %q has %d runes", compareTitle, n)
	}
	if !strings.Contains(compareDesc, "MaxMind") {
		t.Error("description does not name MaxMind")
	}
	if l := strings.ToLower(b + compareTitle + compareDesc + s.llmsText()); strings.Contains(l, "drop-in") || strings.Contains(l, "replacement") {
		t.Error("a drop-in claim")
	}
	if !strings.Contains(b, "<code>city_source</code> starts with 8") {
		t.Error("no note on guessed cities")
	}
	if n := len(compareDesc); n >= 155 {
		t.Errorf("description has %d bytes", n)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "public, max-age=300") {
		t.Errorf("Cache-Control %q", cc)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	r := get(s, "GET", "/geolite2-alternative", "If-None-Match", etag)
	if r.Code != 304 || r.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d, %d bytes", r.Code, r.Body.Len())
	}
	if h := get(s, "HEAD", "/geolite2-alternative"); h.Code != 200 || h.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d bytes", h.Code, h.Body.Len())
	}
	if w2 := get(s, "GET", "/geolite2-alternative"); w2.Header().Get("ETag") != etag {
		t.Error("ETag changed without a change")
	}
}

func TestMaxmindRedirect(t *testing.T) {
	w := get(fixtureServer(t), "GET", "/maxmind-alternative")
	if w.Code != 301 || w.Header().Get("Location") != "/geolite2-alternative" || w.Header().Get("Cache-Control") == "" {
		t.Errorf("%d %q %q", w.Code, w.Header().Get("Location"), w.Header().Get("Cache-Control"))
	}
}

func TestMaxmindRedirectQuery(t *testing.T) {
	w := get(fixtureServer(t), "GET", "/maxmind-alternative?utm=x")
	if w.Code != 301 || w.Header().Get("Location") != "/geolite2-alternative?utm=x" {
		t.Errorf("%d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestCompareLinks(t *testing.T) {
	s := fixtureServer(t)
	for _, c := range []struct{ target, want string }{
		{"/sitemap-pages.xml", "<loc>https://openipdata.org/geolite2-alternative</loc>"},
		{"/llms.txt", "https://openipdata.org/geolite2-alternative"},
		{"/download", `<a href="/geolite2-alternative">Compare with GeoLite2</a>`},
		{"/", `<a href="/geolite2-alternative">Compare &rarr;</a>`},
	} {
		if b := get(s, "GET", c.target).Body.String(); !strings.Contains(b, c.want) {
			t.Errorf("%s lacks %q", c.target, c.want)
		}
	}
}

func TestAccRows(t *testing.T) {
	sc := noScores()
	sc.Coverage, sc.Country, sc.City = 70, 98.5, 55
	sc.MM = vendor{65, 99.2, 55.04} // GeoLite2 wins the country; the city ties at 55.0
	sc.DB = vendor{72.34, 97, 40}   // DB-IP wins "have a city"
	rows := accRows(sc)
	want := [3][3]bool{{false, false, true}, {false, true, true}, {true, false, false}}
	for i, r := range rows {
		for c, cell := range r.Cells {
			if cell.Win != want[i][c] {
				t.Errorf("%s column %d: %q win %v", r.Name, c, cell.Text, cell.Win)
			}
		}
	}
	if rows[2].Cells[0].Text != "72.3%" || rows[0].Cells[1].Text != "98.5%" {
		t.Errorf("formatting: %+v", rows)
	}

	// Missing values are dashes and never win; a column with no value has no check.
	sc = noScores()
	sc.Country = 97
	sc.MM.City = math.NaN()
	rows = accRows(sc)
	for i, r := range rows {
		for c, cell := range r.Cells {
			if wantWin := i == 0 && c == 1; cell.Win != wantWin || (!wantWin && cell.Text != dash) {
				t.Errorf("%s column %d: %q win %v", r.Name, c, cell.Text, cell.Win)
			}
		}
	}
}

func TestParseStampVendors(t *testing.T) {
	raw := `{"built_at":"2026-11-20T07:00:00Z","metrics":{"city_pct":71.2,"atlas_country_pct":98,"atlas_city_pct":60.5,"atlas_probes":12345,` +
		`"maxmind_city_pct":65,"maxmind_atlas_country_pct":99,"maxmind_atlas_city_pct":58,"dbip_city_pct":70,"dbip_atlas_country_pct":97,"dbip_atlas_city_pct":50}}`
	sc := parseStamp(raw, time.Time{})
	if sc.MM != (vendor{65, 99, 58}) || sc.DB != (vendor{70, 97, 50}) || sc.Probes != 12345 {
		t.Errorf("%+v", sc)
	}
	if sc := parseStamp("", time.Time{}); !math.IsNaN(sc.MM.City) || !math.IsNaN(sc.DB.Country) {
		t.Errorf("no stamp: %+v", sc)
	}
}

// A page made from a crafted stamp: the checks sit on the right cells.
func TestCompareChecksOnPage(t *testing.T) {
	s := fixtureServer(t)
	sc := noScores()
	sc.Built = time.Date(2026, 11, 20, 7, 0, 0, 0, time.UTC)
	sc.Coverage, sc.Country, sc.City, sc.Probes = 71.2, 98, 60.5, 12345
	sc.MM = vendor{65, 99, 58}
	sc.DB = vendor{70, 97, 50}
	s.scores.Store(&sc)
	b := get(s, "GET", "/geolite2-alternative").Body.String()
	chk := ` <svg class="ok" viewBox="0 0 16 16" aria-hidden="true"><use href="#i-ok"/></svg>`
	for _, want := range []string{
		`<th scope="row">openipdata</th><td class="n">71.2%` + chk + `</td><td class="n m">98.0%</td><td class="n">60.5%` + chk + `</td>`,
		`<th scope="row">GeoLite2</th><td class="n m">65.0%</td><td class="n">99.0%` + chk + `</td><td class="n m">58.0%</td>`,
		`<span>Have a city</span><b>71.2%` + chk + ` <small>GeoLite2 65.0%</small></b>`, "build of 20 Nov 2026", `<span class="mono">12,345</span> RIPE Atlas devices`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// A new stamp renders the page again; the same one keeps it.
	sc.City = 61
	s.scores.Store(&sc)
	if b2 := get(s, "GET", "/geolite2-alternative").Body.String(); !strings.Contains(b2, "61.0%") {
		t.Error("page not rendered again after a new stamp")
	}

	// No stamp: dashes, no check, no note, the short cache.
	s2 := fixtureServer(t)
	none := noScores()
	s2.scores.Store(&none)
	w := get(s2, "GET", "/geolite2-alternative")
	nb := w.Body.String()
	if !strings.Contains(nb, "<span>Have a city</b>") && !strings.Contains(nb, "<span>Have a city</span><b>"+dash+"</b>") {
		t.Error("no dash for the missing coverage")
	}
	acc := nb[strings.Index(nb, "<h2>Accuracy</h2>"):strings.Index(nb, "<h2>Switch")]
	if strings.Contains(acc, `class="ok"`) || strings.Contains(acc, "build of") || strings.Contains(acc, "RIPE Atlas") {
		t.Error("check, build date or probe note without a stamp")
	}
	if w.Header().Get("Cache-Control") != asnCache {
		t.Errorf("Cache-Control %q", w.Header().Get("Cache-Control"))
	}
}

func TestWhyHaveACity(t *testing.T) {
	chk := `<svg class="ok"`
	cell := func(ours, mm float64) string {
		s := fixtureServer(t)
		sc := noScores()
		sc.Built = time.Date(2026, 11, 20, 7, 0, 0, 0, time.UTC)
		sc.Coverage, sc.MM.Coverage = ours, mm
		s.scores.Store(&sc)
		b := get(s, "GET", "/geolite2-alternative").Body.String()
		i := strings.Index(b, "<span>Have a city</span>")
		return b[i : i+strings.Index(b[i:], "</div>")]
	}
	if c := cell(97.8, 45.3); !strings.Contains(c, chk) || !strings.Contains(c, "<small>GeoLite2 45.3%</small>") || !strings.Contains(c, "97.8%") {
		t.Errorf("ours > GeoLite2: %s", c)
	}
	for _, mm := range []float64{97.8, 99} {
		if c := cell(97.8, mm); strings.Contains(c, chk) || !strings.Contains(c, "<small>GeoLite2 ") {
			t.Errorf("ours <= GeoLite2 (%v): %s", mm, c)
		}
	}
	if c := cell(97.8, math.NaN()); strings.Contains(c, chk) || strings.Contains(c, "<small>") || !strings.Contains(c, "97.8%") {
		t.Errorf("no GeoLite2 value: %s", c)
	}
	if c := cell(math.NaN(), 45.3); strings.Contains(c, chk) || strings.Contains(c, "<small>") || !strings.Contains(c, dash) {
		t.Errorf("no own value: %s", c)
	}
}
