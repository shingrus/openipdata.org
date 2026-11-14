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

// The only scripts of the page. Their sha256 goes into the CSP, so they are
// constants, never built from request data.
const (
	headJS  = `try{var t=localStorage.theme;if(t=="dark"||t=="light")document.documentElement.dataset.theme=t}catch(e){}`
	clickJS = `document.getElementById("theme").onclick=function(){var r=document.documentElement,t=r.dataset.theme||(matchMedia("(prefers-color-scheme: light)").matches?"light":"dark");t=t=="dark"?"light":"dark";r.dataset.theme=t;try{localStorage.theme=t}catch(e){}}`
)

const dash = "—"

type card struct {
	Num, Label, Bar string
	Pct             bool
}

type view struct {
	HeadJS, ClickJS template.JS
	CSS             template.CSS
	Cards           []card
	Probes          string
	Updated         bool
	UpdatedISO      string
	UpdatedText     string
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
		HeadJS: template.JS(headJS), ClickJS: template.JS(clickJS), CSS: template.CSS(styleCSS),
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

// render builds the whole home page once for the given scores.
func render(t *template.Template, s scores) ([]byte, error) {
	var b bytes.Buffer
	if err := t.Execute(&b, makeView(s)); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func scriptHash(js string) string {
	h := sha256.Sum256([]byte(js))
	return "'sha256-" + base64.StdEncoding.EncodeToString(h[:]) + "'"
}

func csp() string {
	return strings.Join([]string{
		"default-src 'none'",
		"style-src 'unsafe-inline'",
		"font-src 'self'",
		"img-src data:",
		"script-src " + scriptHash(headJS) + " " + scriptHash(clickJS),
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}
