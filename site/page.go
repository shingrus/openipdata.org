package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"html/template"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The styles, inlined into every page. Each page carries only the parts it
// uses: style.css (all pages), live.css (the copy and Share buttons, the rows a
// script fills in: home, lookup and shared report pages), acc.css (the Accuracy
// panel: home and lookup pages) and asn.css (tables and lists: ASN, prefix,
// search and status pages). cssText drops comments and line breaks once at start.
//
//go:embed static/style.css
var styleSrc string

//go:embed static/live.css
var liveSrc string

//go:embed static/acc.css
var accSrc string

var (
	styleCSS = cssText(styleSrc)
	liveCSS  = cssText(liveSrc)
	accCSS   = cssText(accSrc)
	asnCSS   = cssText(asnSrc)
)

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

// The only scripts of the page: static/head.js (saved theme), static/theme.js
// (theme button), static/fill.js (fills the row of the other address family,
// see home.html; it sits at the end of the body) and static/resolver.js (fills
// the DNS row, after fill.js) and static/panel.js (the copy and share buttons,
// last; only on a page that has them). They are put into the page
// inline, and their sha256 goes into the CSP, so both use the very same string
// from scriptText(): the file without its first comment line and without
// surrounding white space. Never built from request data.
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

var headJS, themeJS, fillJS, resolverJS, panelJS = scriptText("head.js", headSrc), scriptText("theme.js", themeSrc), scriptText("fill.js", fillSrc), scriptText("resolver.js", resolverSrc), scriptText("panel.js", panelSrc)

// scriptText returns the text that goes between <script> and </script>. An empty
// file (or one with only a comment) stops the program at start.
func scriptText(name, src string) string {
	if strings.HasPrefix(src, "//") {
		_, src, _ = strings.Cut(src, "\n")
	}
	if src = strings.TrimSpace(src); src == "" {
		panic("static/" + name + " has no code")
	}
	return src
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
	Pnl             bool // the page has copy or share buttons for panelJS
	PnlJS           template.JS
	CSS             template.CSS
	Title           string      // the page title
	Desc            string      // meta description; empty = no tag
	LD              template.JS // JSON-LD data block of the head (download page), or empty
	Canon           string      // absolute canonical URL of an indexable page; empty = no tag
	Input           string      // lookup bar value
	NoIndex         bool        // lookup, search and error pages: robots noindex
}

type view struct {
	pageBase
	Cards       []card
	Probes      string
	Updated     bool
	UpdatedISO  string
	UpdatedText string
	Panel       *panel
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
		pageBase: pageBase{HeadJS: template.JS(headJS), ThemeJS: template.JS(themeJS), FillJS: template.JS(fillJS), ResJS: template.JS(resolverJS), PnlJS: template.JS(panelJS), CSS: template.CSS(styleCSS + liveCSS + accCSS)},
		Cards: []card{
			makeCard("right country", s.Country),
			makeCard("right city", s.City),
			makeCard("of addresses have a city", s.Coverage),
		},
	}
	if s.Probes > 0 {
		v.Probes = fmtInt(s.Probes)
	}
	if !s.Built.IsZero() {
		v.Updated = true
		v.UpdatedISO = s.Built.UTC().Format(time.RFC3339)
		v.UpdatedText = s.Built.UTC().Format("2 Jan 2006")
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
func cspWith(origins []string, zone string) string { return cspFor(origins, zone, false) }

// cspFor is cspWith for a page that may also have the copy and share buttons
// (pnl): the hash of panel.js, which fetches nothing.
func cspFor(origins []string, zone string, pnl bool) string {
	script := scriptHash(headJS) + " " + scriptHash(themeJS)
	if pnl {
		script += " " + scriptHash(panelJS)
	}
	connect := slices.Clone(origins)
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
		"img-src data:",
		"script-src " + script,
	}
	if len(connect) > 0 {
		d = append(d, "connect-src "+strings.Join(connect, " "))
	}
	return strings.Join(append(d, "base-uri 'none'", "form-action 'self'", "frame-ancestors 'none'"), "; ")
}
