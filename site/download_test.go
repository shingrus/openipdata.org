package main

// Tests of /download and /llms.txt.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

func TestDownloadPage(t *testing.T) {
	s, _ := asnServer(t)
	w := get(s, "GET", "/download")
	body := w.Body.String()
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=300" || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	t.Logf("rendered size: %d bytes", len(body))
	for _, want := range []string{
		"<title>" + downloadTitle + "</title>",
		`<meta name="description" content="` + downloadDesc + `">`,
		`<link rel="canonical" href="https://openipdata.org/download">`,
		"<h1>Free IP geolocation database</h1>", "City level. CC0. No signup.",
		`<a href="https://github.com/shingrus/openipdata.org/releases/latest/download/ip2geo-openipdata.mmdb">ip2geo-openipdata.mmdb</a>`,
		`<a href="https://github.com/shingrus/openipdata.org/releases/latest/download/ip2asn-openipdata.mmdb">ip2asn-openipdata.mmdb</a>`,
		`<a href="https://github.com/shingrus/openipdata.org/releases/latest/download/asn-openipdata.sqlite3">asn-openipdata.sqlite3</a>`,
		"<h2>Prompt for a coding agent</h2>", "<h2>Python</h2>", "<h2>Go</h2>",
		`<a href="/">Accuracy</a>`, `<a href="https://www.geonames.org/">GeoNames (CC BY 4.0)</a>`, `<a href="https://creativecommons.org/publicdomain/zero/1.0/">Licence: CC0 1.0</a>`,
		"country_code, country_name, city_name, city_source, latitude, longitude", `value=""`,
		`<footer class="foot"><a href="/download">Download</a><a href="/top-asn-by-ip-address">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, `content="noindex"`) {
		t.Error("noindex")
	}
	// Three copy buttons, hidden until panel.js runs; the script is on the page.
	if n := strings.Count(body, `<button class="cp" type="button" hidden`); n != 3 {
		t.Errorf("%d copy buttons", n)
	}
	if !strings.Contains(body, panelJS[:40]) {
		t.Error("no panel.js")
	}
	// The fixtures have no real build time of the files: the table has the files' sizes or dashes only.
	if !regexp.MustCompile(`<td class="n">[0-9.]+ MB</td>`).MatchString(body) {
		t.Error("no size")
	}
	// Same policy as every page with panel.js; the data block needs no hash.
	if got, want := w.Header().Get("Content-Security-Policy"), s.pageCSP(false, false, true); got != want {
		t.Errorf("CSP %q, want %q", got, want)
	}
	if got := get(s, "GET", "/?203.0.113.9").Header().Get("Content-Security-Policy"); got == w.Header().Get("Content-Security-Policy") {
		t.Log("lookup page has the same policy (no panel script there)")
	}
	if w := get(s, "HEAD", "/download"); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD %d %d", w.Code, w.Body.Len())
	}
	if w := get(s, "POST", "/download"); w.Code != 405 {
		t.Errorf("POST %d", w.Code)
	}
}

func TestDownloadJSONLD(t *testing.T) {
	s, _ := asnServer(t)
	body := get(s, "GET", "/download").Body.String()
	m := regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no JSON-LD")
	}
	var d struct {
		Context, Type, Name, URL, License string
		IsAccessibleForFree               bool
		DateModified                      string
		Creator                           struct{ Type, Name string }
		Distribution                      []struct {
			Type           string `json:"@type"`
			ContentURL     string `json:"contentUrl"`
			EncodingFormat string `json:"encodingFormat"`
		}
	}
	d.Context = ""
	raw := map[string]any{}
	if err := json.Unmarshal([]byte(m[1]), &raw); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	b, _ := json.Marshal(raw)
	json.Unmarshal(b, &d)
	if raw["@type"] != "Dataset" || raw["@context"] != "https://schema.org" || raw["license"] != cc0URL || raw["isAccessibleForFree"] != true || raw["url"] != "https://openipdata.org/download" {
		t.Errorf("fields %v", raw)
	}
	if len(d.Distribution) != 3 || !strings.HasSuffix(d.Distribution[0].ContentURL, "/ip2geo-openipdata.mmdb") || d.Distribution[2].EncodingFormat == "" {
		t.Errorf("distribution %+v", d.Distribution)
	}
	if strings.Count(body, "<script type=\"application/ld+json\">") != 1 || strings.Contains(m[1], "<") {
		t.Error("data block")
	}
	// Other pages have none.
	if strings.Contains(get(s, "GET", "/").Body.String(), "ld+json") {
		t.Error("JSON-LD on the home page")
	}
}

func TestDownloadWithoutFiles(t *testing.T) {
	s, err := newServer("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	w := get(s, "GET", "/download")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	if n := strings.Count(body, `<td class="n">—</td><td class="n">—</td>`); n != 3 {
		t.Errorf("%d dash rows", n)
	}
	var raw map[string]any
	m := regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`).FindStringSubmatch(body)
	if m == nil || json.Unmarshal([]byte(m[1]), &raw) != nil {
		t.Fatal("JSON-LD")
	}
	if _, ok := raw["dateModified"]; ok {
		t.Error("made-up dateModified")
	}
}

func TestDownloadDateOfLoadedFile(t *testing.T) {
	s, err := newServer("testdata/stamped.mmdb", "", "")
	if err != nil {
		t.Fatal(err)
	}
	st := s.geo.status()
	if st.Built.IsZero() {
		t.Skip("fixture has no build time")
	}
	body := get(s, "GET", "/download").Body.String()
	if !strings.Contains(body, st.Built.UTC().Format("2 Jan 2006")) || !strings.Contains(body, `"dateModified":"`+st.Built.UTC().Format("2006-01-02")+`"`) {
		t.Errorf("no build date %v", st.Built)
	}
}

// The code blocks use the field names the builder writes; the Go example must
// read a real record with the reader this module uses.
func TestDownloadExamples(t *testing.T) {
	for _, f := range []string{"country_code", "country_name", "city_name", "latitude", "longitude"} {
		if !strings.Contains(promptText, f) || !strings.Contains(goText, `maxminddb:"`+f+`"`) {
			t.Errorf("field %s missing in the prompt or the Go example", f)
		}
	}
	if !strings.Contains(pythonText, `rec.get("country_code")`) || !strings.Contains(pythonText, `rec.get("city_name")`) {
		t.Error("python fields")
	}
	// The struct of the Go example, as in goText.
	type Place struct {
		CountryCode string   `maxminddb:"country_code"`
		CountryName string   `maxminddb:"country_name"`
		CityName    string   `maxminddb:"city_name"`
		Latitude    *float64 `maxminddb:"latitude"`
		Longitude   *float64 `maxminddb:"longitude"`
	}
	db, err := maxminddb.Open("testdata/lookup-geo.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var p Place
	if err := db.Lookup(netip.MustParseAddr("203.0.113.9")).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.CountryCode == "" || p.CityName == "" || p.Latitude == nil || p.Longitude == nil {
		t.Errorf("record %+v", p)
	}
	for _, line := range []string{
		"db, err := maxminddb.Open(", "defer db.Close()", "db.Lookup(netip.MustParseAddr(", ".Decode(&p)", `"github.com/oschwald/maxminddb-golang/v2"`,
	} {
		if !strings.Contains(goText, line) {
			t.Errorf("example lacks %q", line)
		}
	}
	if !strings.Contains(promptText, "string fields asn, name, org, country_code") || !strings.Contains(promptText, "any field can be absent (often the city and the coordinates)") {
		t.Error("asn fields")
	}
}

func TestLLMsTxt(t *testing.T) {
	s, _ := asnServer(t)
	w := get(s, "GET", "/llms.txt")
	body := w.Body.String()
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Header().Get("Cache-Control") != "public, max-age=3600" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	for _, want := range []string{
		"# openipdata.org\n", "\n> ",
		releaseBase + "ip2geo-openipdata.mmdb", releaseBase + "ip2asn-openipdata.mmdb", releaseBase + "asn-openipdata.sqlite3",
		geoFields, asnFields, "not into GeoLite2 model classes", "Any field can be absent (often the city and the coordinates).", "any of them can be absent", "GeoNames (CC BY 4.0)", cc0URL,
		"https://openipdata.org/download", "https://openipdata.org/top-asn-by-ip-address", "https://openipdata.org/list-of-countries-by-ipv4-allocation",
		"https://openipdata.org/?<ip>", "https://openipdata.org/as<number>", "https://openipdata.org/prefix/<cidr>", "https://openipdata.org/api/as<number>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("llms.txt lacks %q", want)
		}
	}
	if w := get(s, "POST", "/llms.txt"); w.Code != 405 {
		t.Errorf("POST %d", w.Code)
	}
	if w := get(s, "HEAD", "/llms.txt"); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD %d %d", w.Code, w.Body.Len())
	}
}

// The Go example is a complete program: every package it uses is imported and
// every import is used.
func TestGoExampleImports(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "example.go", goText, 0)
	if err != nil {
		t.Fatal(err)
	}
	imported := map[string]bool{}
	for _, im := range f.Imports {
		p := strings.Trim(im.Path.Value, `"`)
		name := path.Base(p)
		if p == "github.com/oschwald/maxminddb-golang/v2" {
			name = "maxminddb" // the package name differs from the last path element
		}
		imported[name] = false
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Obj == nil { // not a local variable
				if _, isImport := imported[id.Name]; isImport {
					imported[id.Name] = true
				} else if id.Name == "fmt" || id.Name == "netip" || id.Name == "maxminddb" {
					t.Errorf("package %s is used but not imported", id.Name)
				}
			}
		}
		return true
	})
	for _, want := range []string{"fmt", "netip", "maxminddb"} {
		if used, ok := imported[want]; !ok || !used {
			t.Errorf("import %s: present %v, used %v", want, ok, used)
		}
	}
	for name, used := range imported {
		if !used {
			t.Errorf("import %s is not used", name)
		}
	}
}
