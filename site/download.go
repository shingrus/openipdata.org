package main

// GET /download: the data files with their size and date, three copy-ready
// examples, and the licence and sources. GET /llms.txt: the same for AI agents
// in the llms.txt convention. Nothing here holds visitor data.

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed static/download.css
var dlSrc string

var dlCSS = cssText(dlSrc)

const (
	releaseBase  = "https://github.com/shingrus/openipdata.org/releases/latest/download/"
	cc0URL       = "https://creativecommons.org/publicdomain/zero/1.0/"
	geonamesURL  = "https://www.geonames.org/"
	geoFile      = "ip2geo-openipdata.mmdb"
	asnFileName  = "ip2asn-openipdata.mmdb"
	sqliteFile   = "asn-openipdata.sqlite3"
	downloadPath = "/download"

	downloadTitle = "Free GeoIP database download (MMDB) | openipdata.org"
	downloadDesc  = "Download the free IP to city and IP to ASN database in MMDB and SQLite. Public domain (CC0), no signup, no key. With examples for Python, Go and AI agents."

	// The record fields of the two MMDBs, as mmdbbuilder writes them (geo.go,
	// asn.go): all flat, the strings are absent when unknown.
	geoFields = "country_code, country_name, city_name, city_source, latitude, longitude"
	asnFields = "asn, name, org, country_code"
)

const promptText = `Add IP geolocation to this project with the free openipdata.org database (public domain, CC0, no API key). Download ` + releaseBase + geoFile + ` and read it with the MaxMind DB (MMDB) reader library for this language. Each record is a flat map with string fields country_code, country_name, city_name and numbers latitude, longitude; any field can be absent (often the city and the coordinates). Do not use GeoLite2 model classes: decode into a plain map or your own struct. For the network of an address use ` + asnFileName + ` (string fields asn, name, org, country_code). Download the files again once a day.`

const pythonText = `import maxminddb

with maxminddb.open_database("ip2geo-openipdata.mmdb") as db:
    rec = db.get("8.8.8.8") or {}
    print(rec.get("country_code"), rec.get("city_name"))`

const goText = `package main

import (
	"fmt"
	"net/netip"

	"github.com/oschwald/maxminddb-golang/v2"
)

type Place struct {
	CountryCode string   ` + "`maxminddb:\"country_code\"`" + `
	CountryName string   ` + "`maxminddb:\"country_name\"`" + `
	CityName    string   ` + "`maxminddb:\"city_name\"`" + `
	Latitude    *float64 ` + "`maxminddb:\"latitude\"`" + `
	Longitude   *float64 ` + "`maxminddb:\"longitude\"`" + `
}

func main() {
	db, err := maxminddb.Open("ip2geo-openipdata.mmdb")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	var p Place
	if err := db.Lookup(netip.MustParseAddr("8.8.8.8")).Decode(&p); err != nil {
		panic(err)
	}
	fmt.Println(p.CountryCode, p.CityName)
}`

type dlFile struct{ Name, Href, Contains, Size, Updated string }

type dlBlock struct{ Title, Text string }

type dlView struct {
	pageBase
	Files                  []dlFile
	Blocks                 []dlBlock
	Sources                string
	GeoFields, ASNFields   string
	GeoNames, CC0, Release string
}

type ldDist struct {
	Type           string `json:"@type"`
	ContentURL     string `json:"contentUrl"`
	EncodingFormat string `json:"encodingFormat"`
}

type ldDataset struct {
	Context             string   `json:"@context"`
	Type                string   `json:"@type"`
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	URL                 string   `json:"url"`
	License             string   `json:"license"`
	IsAccessibleForFree bool     `json:"isAccessibleForFree"`
	Creator             ldOrg    `json:"creator"`
	DateModified        string   `json:"dateModified,omitempty"`
	Distribution        []ldDist `json:"distribution"`
}

type ldOrg struct {
	Type string `json:"@type"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// dlDate is "2 Jan 2006" in UTC, or a dash for no time.
func dlDate(t time.Time) string {
	if t.IsZero() {
		return dash
	}
	return t.UTC().Format("2 Jan 2006")
}

func dlSize(st fileStatus) string {
	if !st.Loaded {
		return dash
	}
	return fmtMB(st.Size)
}

// downloadCSS is the whole style of the page, joined once.
var downloadCSS = template.CSS(styleCSS + dlCSS)

// dlKey is everything the page shows that changes: the rows of the table, the
// date of the JSON-LD and the origin. The page is rendered again only when it
// differs from the key of the page held.
type dlKey struct {
	files [3]dlFile
	mod   string
	org   string
}

// dlCache holds the one rendered page.
type dlCache struct {
	mu  sync.Mutex
	key dlKey
	pg  *cachedPage
}

// downloadPage returns the rendered page for the files this process has loaded
// (a file that is not loaded has a dash for its size and date).
func (s *server) downloadPage() (*cachedPage, error) {
	geo, asn := s.geo.status(), s.asn.status()
	sq, _ := s.asnf.status()
	key := dlKey{org: s.origin, files: [3]dlFile{
		{geoFile, releaseBase + geoFile, "IP to country and city, coordinates", dlSize(geo), dlDate(geo.Built)},
		{asnFileName, releaseBase + asnFileName, "IP to network", dlSize(asn), dlDate(asn.Built)},
		// Both ASN files come from the same build-asn-data run: the SQLite row shows the
		// build date of the loaded ip2asn MMDB, not the mtime of the local copy (the pull time).
		{sqliteFile, releaseBase + sqliteFile, "Networks, organizations, prefixes", dlSize(sq), dlDate(asn.Built)},
	}}
	if geo.Loaded && !geo.Built.IsZero() {
		key.mod = geo.Built.UTC().Format("2006-01-02")
	}
	c := &s.dl
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pg != nil && c.key == key {
		return c.pg, nil
	}
	ld := ldDataset{
		Context: "https://schema.org", Type: "Dataset",
		Name: "openipdata.org IP geolocation and ASN database", Description: downloadDesc,
		URL: s.canon(downloadPath), License: cc0URL, IsAccessibleForFree: true,
		Creator: ldOrg{"Organization", "openipdata.org", s.canon("/")}, DateModified: key.mod,
	}
	for i, f := range key.files {
		format := "application/vnd.maxmind.db"
		if i == 2 {
			format = "application/vnd.sqlite3"
		}
		ld.Distribution = append(ld.Distribution, ldDist{"DataDownload", f.Href, format})
	}
	// encoding/json writes < > & as \u003c \u003e \u0026, so no "</script>" can
	// end the block. A data block is not executed: script-src needs no hash for
	// it, the page keeps the policy of every page with panel.js.
	b, err := json.Marshal(ld)
	if err != nil {
		return nil, err
	}
	v := &dlView{pageBase: pageBase{
		HeadJS: template.JS(headJS), ThemeJS: template.JS(themeJS), CSS: downloadCSS,
		Title: downloadTitle, Desc: downloadDesc, Canon: s.canon(downloadPath), LD: template.JS(b),
		Pnl: true, PnlJS: template.JS(panelJS),
	}, Files: key.files[:], Blocks: []dlBlock{{"Prompt for a coding agent", promptText}, {"Python", pythonText}, {"Go", goText}},
		Sources: sourcesList, GeoFields: geoFields, ASNFields: asnFields, GeoNames: geonamesURL, CC0: cc0URL}
	body, err := render(s.tmpl, "download.html", v)
	if err != nil {
		return nil, err
	}
	c.key, c.pg = key, newPage(body)
	return c.pg, nil
}

func (s *server) serveDownload(w http.ResponseWriter, r *http.Request) {
	pg, err := s.downloadPage()
	if err != nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	pageHeaders(w.Header(), s.pageCSP(false, false, true, false))
	servePage(w, r, pg, "text/html; charset=utf-8", asnCache)
}

// llmsText is /llms.txt in the llms.txt convention.
func (s *server) llmsText() string {
	o := s.canon("")
	var b strings.Builder
	b.WriteString("# openipdata.org\n\n")
	b.WriteString("> Free IP geolocation and ASN database at city level. Public domain (CC0), no signup, no API key. Accuracy is measured and published on the home page.\n\n")
	b.WriteString("## Download\n\n")
	for _, f := range []struct{ name, what string }{
		{geoFile, "IP to country, city and coordinates (MMDB)"},
		{asnFileName, "IP to network (MMDB)"},
		{sqliteFile, "networks, organizations, prefixes (SQLite)"},
	} {
		b.WriteString("- [" + f.name + "](" + releaseBase + f.name + "): " + f.what + "\n")
	}
	b.WriteString("\n## Record fields\n\n")
	b.WriteString("- " + geoFile + ": " + geoFields + ". Any field can be absent (often the city and the coordinates).\n")
	b.WriteString("- " + asnFileName + ": " + asnFields + " (all strings, any of them can be absent).\n")
	b.WriteString("\n## Use\n\n")
	b.WriteString("Read the MMDB files with any MaxMind DB (MMDB) reader. Records are flat maps: decode into a plain map or your own struct, not into GeoLite2 model classes. The files are rebuilt twice a day; download them again once a day. Examples for Python and Go: [" + o + downloadPath + "](" + o + downloadPath + ").\n")
	b.WriteString("\n## Licence and sources\n\n")
	b.WriteString("[CC0 1.0](" + cc0URL + "). " + sourcesText + "\n")
	b.WriteString("\n## Pages\n\n")
	b.WriteString("- [Download](" + o + downloadPath + ")\n")
	for _, rk := range rankings {
		b.WriteString("- [" + rk.title + "](" + o + rk.path + ")\n")
	}
	b.WriteString("\n## URL patterns\n\n")
	b.WriteString("- " + o + "/?<ip>: one address\n")
	b.WriteString("- " + o + "/as<number>: one network\n")
	b.WriteString("- " + o + "/prefix/<cidr>: one announced prefix\n")
	b.WriteString("- " + o + "/api/as<number>: one network as JSON\n")
	return b.String()
}

// sourcesText names what the published files are built from (docs/pipeline.md);
// GeoNames is where the coordinates come from and needs attribution.
const sourcesList = "RIR delegation files, ipverse (geo-ip-blocks, asn-ip), PeeringDB, whois and operator geofeeds, router names from traceroutes, RIPE RIS"

const sourcesText = "Sources: " + sourcesList + ". Coordinates: GeoNames (CC BY 4.0)."

func (s *server) serveLLMs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", seoCache)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeBody(w, r, http.StatusOK, "text/plain; charset=utf-8", s.static().llms)
}
