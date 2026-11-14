package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"
)

//go:embed static/style.css
var styleCSS string

// The only scripts of the page: static/head.js (saved theme), static/theme.js
// (theme button) and static/fill.js (fills the row of the other address family,
// see home.html; it sits at the end of the body). They are put into the page
// inline, and their sha256 goes into the CSP, so both use the very same string
// from scriptText(): the file without its first comment line and without
// surrounding white space. Never built from request data.
//
//go:embed static/head.js
var headSrc string

//go:embed static/theme.js
var themeSrc string

//go:embed static/fill.js
var fillSrc string

var headJS, themeJS, fillJS = scriptText("head.js", headSrc), scriptText("theme.js", themeSrc), scriptText("fill.js", fillSrc)

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

type view struct {
	HeadJS, ThemeJS template.JS
	Fill            bool // the page has a row for fillJS to fill
	FillJS          template.JS
	CSS             template.CSS
	Cards           []card
	Probes          string
	Updated         bool
	UpdatedISO      string
	UpdatedText     string
	Panel           *panel
	Input           string // lookup bar value
	Title           string // the page title
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
		HeadJS: template.JS(headJS), ThemeJS: template.JS(themeJS), FillJS: template.JS(fillJS), CSS: template.CSS(styleCSS),
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

// render executes the page template for v.
func render(t *template.Template, v view) ([]byte, error) {
	var b bytes.Buffer
	if err := t.Execute(&b, v); err != nil {
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
func csp(origins ...string) string {
	script := scriptHash(headJS) + " " + scriptHash(themeJS)
	d := []string{
		"default-src 'none'",
		"style-src 'unsafe-inline'",
		"font-src 'self'",
		"img-src data:",
	}
	if len(origins) > 0 {
		script += " " + scriptHash(fillJS)
		d = append(d, "script-src "+script, "connect-src "+strings.Join(origins, " "))
	} else {
		d = append(d, "script-src "+script)
	}
	return strings.Join(append(d, "base-uri 'none'", "form-action 'self'", "frame-ancestors 'none'"), "; ")
}
