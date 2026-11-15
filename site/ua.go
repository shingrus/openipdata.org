package main

// The Browser row of the visitor's own page: the browser and the system named
// by the User-Agent (and, for Brave, Sec-CH-UA) of the request, each with its
// colour logo. Only "/" shows it (private, no-store); a lookup, report or any
// cached page never has it, nothing is saved and the header is never logged.

import (
	"embed"
	"fmt"
	"html/template"
	"regexp"
	"strings"
)

//go:embed static/icons/*.svg
var iconFS embed.FS

const maxUALen = 512 // longest User-Agent that is read

// browserInfo is what a User-Agent says; an empty field is unknown.
type browserInfo struct {
	Browser, Ver, OS string
}

// uaVersion is the major version after the first of toks found in ua: digits
// only, at most 4. The bool is false when no token is there.
func uaVersion(ua string, toks ...string) (string, bool) {
	for _, t := range toks {
		if i := strings.Index(ua, t); i >= 0 {
			rest := ua[i+len(t):]
			n := 0
			for n < len(rest) && n < 4 && rest[n] >= '0' && rest[n] <= '9' {
				n++
			}
			return rest[:n], true
		}
	}
	return "", false
}

// parseUA reads the User-Agent ua (cut to 512 bytes) and the Sec-CH-UA header
// chUA (only for Brave, whose User-Agent is Chrome's). Nothing is guessed.
func parseUA(ua, chUA string) browserInfo {
	if len(ua) > maxUALen {
		ua = ua[:maxUALen]
	}
	if len(chUA) > maxUALen {
		chUA = chUA[:maxUALen]
	}
	var b browserInfo
	for _, c := range []struct {
		name string
		toks []string
	}{
		{"Edge", []string{"Edg/", "EdgA/", "EdgiOS/"}},
		{"Opera", []string{"OPR/", "OPT/"}},
		{"Samsung Internet", []string{"SamsungBrowser/"}},
		{"Vivaldi", []string{"Vivaldi/"}},
	} {
		if v, ok := uaVersion(ua, c.toks...); ok {
			b.Browser, b.Ver = c.name, v
			break
		}
	}
	if b.Browser == "" {
		if v, ok := uaVersion(ua, "Chrome/"); ok && strings.Contains(chUA, `"Brave"`) {
			b.Browser, b.Ver = "Brave", v
		} else if v, ok := uaVersion(ua, "Firefox/", "FxiOS/"); ok {
			b.Browser, b.Ver = "Firefox", v
		} else if v, ok := uaVersion(ua, "Chrome/", "CriOS/", "Chromium/"); ok {
			b.Browser, b.Ver = "Chrome", v
		} else if strings.Contains(ua, "Safari/") {
			if v, ok := uaVersion(ua, "Version/"); ok {
				b.Browser, b.Ver = "Safari", v
			}
		}
	}
	switch {
	case strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad") || strings.Contains(ua, "iPod"):
		b.OS = "iOS"
	case strings.Contains(ua, "Android"):
		b.OS = "Android"
	case strings.Contains(ua, "Windows"):
		b.OS = "Windows"
	case strings.Contains(ua, "CrOS"):
		b.OS = "ChromeOS"
	case strings.Contains(ua, "Macintosh") || strings.Contains(ua, "Mac OS X"):
		b.OS = "macOS"
	case strings.Contains(ua, "Ubuntu"):
		b.OS = "Ubuntu"
	case strings.Contains(ua, "Linux") || strings.Contains(ua, "X11"):
		b.OS = "Linux"
	}
	return b
}

// browserRow is the Browser row of the template.
type browserRow struct {
	Icon   template.HTML // the browser logo, or empty
	Text   string        // "Chrome 156", or a dash
	OSIcon template.HTML // the system logo, or empty
	OS     string        // its name (accessible name and tooltip only)
}

type iconSet struct {
	browser map[string]template.HTML // by browser display name
	os      map[string]template.HTML // by system name
}

var icons = loadIcons()

const winSVG = `<svg class="ic" aria-hidden="true" focusable="false" viewBox="0 0 24 24" fill="#0078D4"><path d="M2 2h9.5v9.5H2zM12.5 2H22v9.5h-9.5zM2 12.5h9.5V22H2zM12.5 12.5H22V22h-9.5z"/></svg>`

var (
	reProlog = regexp.MustCompile(`(?s)<\?xml.*?\?>|<!--.*?-->|<title>.*?</title>`)
	reGap    = regexp.MustCompile(`>\s+<`)
	reID     = regexp.MustCompile(`(\sid=")([^"]*)"`)
	reURL    = regexp.MustCompile(`url\(#([^)]*)\)`)
	reHref   = regexp.MustCompile(`(href=")#([^"]*)"`)
)

// prepareIcon cleans one icon file once at start: the prolog, title, comments
// and the white space between tags go; the svg gets the class and the
// accessibility attributes; fill (when not empty) is set on the svg; with
// prefix every id and its references are renamed with "b-" so two icons or
// other ids of the page cannot collide.
func prepareIcon(src, fill string, prefix bool) string {
	s := strings.TrimSpace(reGap.ReplaceAllString(reProlog.ReplaceAllString(src, ""), "><"))
	s = strings.Replace(s, ` role="img"`, "", 1)
	attrs := ` class="ic" aria-hidden="true" focusable="false"`
	if fill != "" {
		attrs += ` fill="` + fill + `"`
	}
	s = strings.Replace(s, "<svg", "<svg"+attrs, 1)
	if prefix {
		s = reID.ReplaceAllString(s, `${1}b-${2}"`)
		s = reURL.ReplaceAllString(s, `url(#b-$1)`)
		s = reHref.ReplaceAllString(s, `${1}#b-$2"`)
	}
	return s
}

// loadIcons prepares every icon once; a missing or empty file stops the start.
func loadIcons() iconSet {
	read := func(name string) string {
		b, err := iconFS.ReadFile("static/icons/" + name + ".svg")
		if err != nil || len(b) == 0 {
			panic(fmt.Sprintf("icon %s missing or empty", name))
		}
		return string(b)
	}
	set := iconSet{browser: map[string]template.HTML{}, os: map[string]template.HTML{}}
	for name, file := range map[string]string{"Chrome": "chrome", "Firefox": "firefox", "Safari": "safari", "Edge": "edge",
		"Opera": "opera", "Brave": "brave", "Samsung Internet": "samsung-internet", "Vivaldi": "vivaldi"} {
		set.browser[name] = template.HTML(prepareIcon(read(file), "", true))
	}
	apple := template.HTML(prepareIcon(read("os-apple"), "currentColor", false))
	linux := template.HTML(prepareIcon(read("os-linux"), "#FCC624", false))
	set.os["macOS"], set.os["iOS"] = apple, apple
	set.os["Android"] = template.HTML(prepareIcon(read("os-android"), "#34A853", false))
	set.os["Linux"], set.os["ChromeOS"] = linux, linux
	set.os["Ubuntu"] = template.HTML(prepareIcon(read("os-ubuntu"), "#E95420", false))
	set.os["Windows"] = template.HTML(winSVG)
	return set
}

// browserRowFor is the row for the request headers ua and chUA.
func browserRowFor(ua, chUA string) *browserRow {
	b := parseUA(ua, chUA)
	r := &browserRow{Text: dash}
	if b.Browser != "" {
		r.Icon = icons.browser[b.Browser]
		r.Text = strings.TrimSpace(b.Browser + " " + b.Ver)
	}
	if b.OS != "" {
		r.OSIcon, r.OS = icons.os[b.OS], b.OS
	}
	return r
}
