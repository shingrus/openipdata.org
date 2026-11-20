package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"html/template"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The styles, inlined into every page. Each page carries only the parts it
// uses: style.css (all pages), map.css (a page with a map), live.css (the copy
// buttons and the rows with links: a home, lookup or shared report page whose
// panel has an address), visitor.css (the Share and Headers buttons, the
// Browser, other-family and DNS rows: the visitor's own page), acc.css (the
// Accuracy panel: home and lookup pages), net.css and api.css (the Network and
// API panels, where they are) and asn.css (tables and lists: ASN, prefix,
// search and status pages). cssText drops comments and line breaks once at start.
//
//go:embed static/style.css
var styleSrc string

//go:embed static/live.css
var liveSrc string

//go:embed static/acc.css
var accSrc string

//go:embed static/map.css
var mapSrc string

//go:embed static/visitor.css
var visitorSrc string

//go:embed static/net.css
var netSrc string

//go:embed static/api.css
var apiSrc string

//go:embed static/db.css
var dbSrc string

//go:embed static/place.css
var placeSrc string

//go:embed static/rows.css
var rowsSrc string

//go:embed static/pf.css
var pfSrc string

var (
	styleCSS   = cssText(styleSrc)
	liveCSS    = cssText(liveSrc)
	accCSS     = cssText(accSrc)
	asnCSS     = cssText(asnSrc)
	mapCSS     = cssText(mapSrc)
	visitorCSS = cssText(visitorSrc)
	netCSS     = cssText(netSrc)
	apiCSS     = cssText(apiSrc)
	dbCSS      = cssText(dbSrc)
	placeCSS   = cssText(placeSrc)
	rowsCSS    = cssText(rowsSrc)
	pfCSS      = cssText(pfSrc)
)

// homeCSS is the style of a home or lookup page: only the parts this view
// shows. Call it when the view is complete.
func (v *view) homeCSS() template.CSS {
	css := styleCSS
	if p := v.Panel; p != nil {
		if p.Msg == "" { // a full panel: the place block and the rows
			css += placeCSS + rowsCSS
		}
		if p.Map != "" {
			css += mapCSS
		}
		if p.Copy {
			css += liveCSS
		}
		if p.Share || p.Br != nil || v.Fill || v.Res {
			css += visitorCSS
		}
	}
	css += accCSS
	if v.Net != nil || v.Home {
		css += netCSS // .cells: the Network panel and the Database panel
	}
	if v.API != nil {
		css += apiCSS
	}
	if v.Home {
		css += dbCSS
	}
	return template.CSS(css)
}

var cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// cssText is a stylesheet without its comments, line breaks and the white
// space around them (the files are written one rule a line, so nothing else
// needs a space).
func cssText(src string) string {
	var b strings.Builder
	for _, l := range strings.Split(cssComment.ReplaceAllString(src, ""), "\n") {
		b.WriteString(strings.TrimSpace(l))
	}
	return b.String()
}

// The only scripts of the page: static/head.js (saved theme, browser bar
// colour), static/theme.js (theme button, the / key), static/fill.js (fills the
// row of the other address family, see home.html; it sits at the end of the
// body), static/resolver.js (fills the DNS row, after fill.js), static/panel.js
// (the copy buttons, last but one; only on a page that has them) and
// static/share.js (the Share button, last; only on the visitor's own page).
// They are put into the page inline, and their sha256 goes into the CSP, so
// both use the very same string from scriptText(). Never built from request
// data.
//
//go:embed static/asn.css
var asnSrc string

//go:embed static/head.js
var headSrc string

//go:embed static/theme.js
var themeSrc string

//go:embed static/fill.js
var fillSrc string

//go:embed static/resolver.js
var resolverSrc string

//go:embed static/panel.js
var panelSrc string

//go:embed static/share.js
var shareSrc string

var headJS, themeJS, fillJS, resolverJS, panelJS, shareJS = scriptText("head.js", headSrc), scriptText("theme.js", themeSrc), scriptText("fill.js", fillSrc), scriptText("resolver.js", resolverSrc), scriptText("panel.js", panelSrc), scriptText("share.js", shareSrc)

// scriptText returns the text that goes between <script> and </script>: the file
// without its first comment line, without every line that is only a // comment,
// without the white space around each line and without empty lines. The line
// breaks that stay keep automatic semicolon insertion as it is, which is why a
// comment never follows code on its line (a // inside a string or a regular
// expression is not at the start of a line, so it is safe). An empty file (or
// one with only comments) stops the program at start.
func scriptText(name, src string) string {
	if strings.HasPrefix(src, "//") {
		_, src, _ = strings.Cut(src, "\n")
	}
	var b strings.Builder
	for _, l := range strings.Split(src, "\n") {
		if l = strings.TrimSpace(l); l == "" || strings.HasPrefix(l, "//") {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	if b.Len() == 0 {
		panic("static/" + name + " has no code")
	}
	return b.String()
}

const dash = "—"

type card struct {
	Num, Label, Bar string
	Pct             bool
}

// pageBase is what every page has: the header, the lookup bar and the scripts.
type pageBase struct {
	HeadJS, ThemeJS template.JS
	Fill            bool // the page has a row for fillJS to fill
	FillJS          template.JS
	Res             bool // the page has the DNS row for resolverJS to fill
	ResJS           template.JS
	Pnl             bool // the page has copy buttons for panelJS
	PnlJS           template.JS
	Share           bool // the page has the Share button for shareJS (after panelJS)
	ShareJS         template.JS
	CSS             template.CSS
	Title           string      // the page title
	Desc            string      // meta description; empty = no tag
	LD              template.JS // JSON-LD data block of the head (download page), or empty
	Canon           string      // absolute canonical URL of an indexable page; empty = no tag
	Input           string      // lookup bar value
	NoIndex         bool        // lookup, search and error pages: robots noindex
	Home            bool        // the visitor's own page: the heading in the header and the Database panel
}

type view struct {
	pageBase
	Cards       []card
	Probes      string
	Updated     bool
	UpdatedISO  string
	UpdatedText string
	Rebuilt     string // age of the loaded geo database ("5 hours ago") or a dash: the Database panel
	Panel       *panel
	Net         *netPanel // the Network panel (visitor and lookup pages with the network in the ASN data)
	API         *apiPanel // the API panel below the Network panel; nil = none
}

// fmtPct returns the number with one decimal, or false when v is missing,
// NaN or outside 0..100.
func fmtPct(v float64) (string, bool) {
	if math.IsNaN(v) || v < 0 || v > 100 {
		return dash, false
	}
	v = math.Abs(v) // turns -0 into 0
	return strconv.FormatFloat(v, 'f', 1, 64), true
}

// fmtInt prints n with commas between thousands.
func fmtInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func makeCard(label string, v float64) card {
	num, ok := fmtPct(v)
	if !ok {
		return card{Num: dash, Label: label, Bar: "0%"}
	}
	return card{Num: num, Label: label, Bar: num + "%", Pct: true}
}

func makeView(s scores) view {
	v := view{
		pageBase: pageBase{HeadJS: template.JS(headJS), ThemeJS: template.JS(themeJS), FillJS: template.JS(fillJS), ResJS: template.JS(resolverJS), PnlJS: template.JS(panelJS), ShareJS: template.JS(shareJS)},
		Cards: []card{
			makeCard("right country", s.Country),
			makeCard("right city", s.City),
			makeCard("of addresses have a city", s.Coverage),
		},
	}
	v.Rebuilt = dash
	if s.Probes > 0 {
		v.Probes = fmtInt(s.Probes)
	}
	if !s.Built.IsZero() {
		v.Updated = true
		v.UpdatedISO = s.Built.UTC().Format(time.RFC3339)
		v.UpdatedText = s.Built.UTC().Format("2 Jan 2006")
		v.Rebuilt = ageText(s.Built, time.Now())
	}
	return v
}

// render executes the named page template for v.
func render(t *template.Template, name string, v any) ([]byte, error) {
	var b bytes.Buffer
	if err := t.ExecuteTemplate(&b, name, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func scriptHash(js string) string {
	h := sha256.Sum256([]byte(js))
	return "'sha256-" + base64.StdEncoding.EncodeToString(h[:]) + "'"
}

// csp is the policy of every page. With origins (the origins of V4_URL and
// V6_URL) it is the policy of a page that has the fill script: that script's
// hash and connect-src for exactly those origins. Without, nothing is added.
func csp(origins ...string) string { return cspWith(origins, "") }

// cspWith is csp for a page that may also have the DNS row: with a zone the
// hash of resolver.js and connect-src for the names under the zone.
func cspWith(origins []string, zone string) string { return cspFor(origins, zone, false, false) }

// cspFor is cspWith for a page that may also have the copy and share buttons
// (pnl): the hash of panel.js. With share (the Share button, whose script
// share.js posts to POST /share) the hash of share.js and connect-src also
// allows the page's own origin.
func cspFor(origins []string, zone string, pnl, share bool) string {
	script := scriptHash(headJS) + " " + scriptHash(themeJS)
	if pnl {
		script += " " + scriptHash(panelJS)
	}
	var connect []string
	if share {
		script += " " + scriptHash(shareJS)
		connect = append(connect, "'self'")
	}
	connect = append(connect, origins...)
	if len(origins) > 0 {
		script += " " + scriptHash(fillJS)
	}
	if zone != "" {
		script += " " + scriptHash(resolverJS)
		connect = append(connect, "https://*."+zone)
	}
	d := []string{
		"default-src 'none'",
		"style-src 'unsafe-inline'",
		"font-src 'self'",
		"img-src 'self' data:",
		"script-src " + script,
	}
	if len(connect) > 0 {
		d = append(d, "connect-src "+strings.Join(connect, " "))
	}
	return strings.Join(append(d, "base-uri 'none'", "form-action 'self'", "frame-ancestors 'none'"), "; ")
}
