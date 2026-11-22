package main

// GET /geolite2-alternative: why switch from GeoLite2, a table of the free
// databases, the accuracy of ours against GeoLite2 and DB-IP Lite (from the
// stamp in the ip2geo MMDB) and the one line that changes in a GeoLite2
// program. Rendered once and kept; it holds no visitor data.

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"html/template"
	"math"
	"net/http"
	"strings"
	"sync"
)

//go:embed static/compare.css
var cmpSrc string

// compareCSS is the whole style of the page, joined once.
var compareCSS = template.CSS(styleCSS + cssText(cmpSrc))

const (
	comparePath  = "/geolite2-alternative"
	compareTitle = "Free GeoLite2 alternative, same MMDB layout | openipdata.org"
	compareDesc  = "Free GeoLite2 (MaxMind) alternative in the same MMDB layout. CC0, no sign-up, rebuilt twice a day, accuracy compared with GeoLite2 and DB-IP."
	dbipURL      = "https://db-ip.com"
	maxmindURL   = "https://www.maxmind.com"
)

const compareCode = `# only the file changes
reader = geoip2.database.Reader("` + geoFile + `")
r = reader.city("8.8.8.8")
r.country.iso_code, r.city.names["en"]`

// cmpCell is a table or grid cell; Win puts the check mark next to it and
// keeps its text in the normal colour (the others are muted).
type cmpCell struct {
	Text string
	Win  bool
	Note string // muted small text after the value (the GeoLite2 figure of Why switch)
}

type cmpRow struct {
	Name  string
	Ours  bool
	Cells []cmpCell
}

type cmpView struct {
	pageBase
	Why      []cmpRow // label in Name, the value in Cells[0]
	Free     []cmpRow
	Acc      []cmpRow
	Chart    template.HTML // the Score history chart of /status, or empty (then the block is left out)
	Built    string        // "2 Jan 2006" or empty
	Probes   string        // thousands separated, or empty
	Download string        // the geo file URL
	File     string
	Code     string
	DBIP, MM string
}

// freeRows is the table of the free databases (static facts).
func freeRows() []cmpRow {
	y := func(t string) cmpCell { return cmpCell{Text: t, Win: true} }
	n := func(t string) cmpCell { return cmpCell{Text: t} }
	return []cmpRow{
		{"openipdata", true, []cmpCell{y("CC0"), y("no"), y("2× day"), y("yes")}},
		{"GeoLite2", false, []cmpCell{n("EULA"), n("key"), n("2× week"), y("yes")}},
		{"DB-IP Lite", false, []cmpCell{n("CC BY"), y("no"), n("monthly"), y("yes")}},
		{"IP2Location LITE", false, []cmpCell{n("attribution"), n("account"), n("monthly"), y("yes")}},
		{"IPinfo Lite", false, []cmpCell{n("CC BY-SA"), n("token"), n("daily"), n("no")}},
	}
}

// pctTenths is the value as tenths of a percent for comparing what the page
// shows (so two values that print the same tie), or false when missing.
func pctTenths(v float64) (int, bool) {
	if _, ok := fmtPct(v); !ok {
		return 0, false
	}
	return int(math.Round(v * 10)), true
}

// accRows is the Accuracy table: the highest value of each column (all of a
// tie) gets the check; a column with no value has none.
func accRows(s scores) []cmpRow {
	src := []struct {
		name string
		ours bool
		v    [3]float64
	}{
		{"openipdata", true, [3]float64{s.Coverage, s.Country, s.City}},
		{"GeoLite2", false, [3]float64{s.MM.Coverage, s.MM.Country, s.MM.City}},
		{"DB-IP Lite", false, [3]float64{s.DB.Coverage, s.DB.Country, s.DB.City}},
	}
	var best [3]int
	var has [3]bool
	for _, r := range src {
		for c, v := range r.v {
			if t, ok := pctTenths(v); ok && (!has[c] || t > best[c]) {
				best[c], has[c] = t, true
			}
		}
	}
	rows := make([]cmpRow, len(src))
	for i, r := range src {
		rows[i] = cmpRow{Name: r.name, Ours: r.ours}
		for c, v := range r.v {
			txt, _ := fmtPct(v)
			if t, ok := pctTenths(v); ok {
				txt += "%"
				rows[i].Cells = append(rows[i].Cells, cmpCell{Text: txt, Win: t == best[c]})
			} else {
				rows[i].Cells = append(rows[i].Cells, cmpCell{Text: txt})
			}
		}
	}
	return rows
}

// cmpCache holds the one rendered page, with the key of what it shows.
type cmpCache struct {
	mu  sync.Mutex
	key string
	pg  *cachedPage
}

// comparePage returns the rendered page for the scores now loaded, and whether
// the stamp was there (else the page has dashes and keeps the short cache).
func (s *server) comparePage() (*cachedPage, bool, error) {
	sc := *s.scores.Load()
	v := &cmpView{pageBase: pageBase{HeadJS: template.JS(headJS), ThemeJS: template.JS(themeJS), CSS: compareCSS,
		Title: compareTitle, Desc: compareDesc, Canon: s.canon(comparePath), Pnl: true, PnlJS: template.JS(panelJS)},
		Free: freeRows(), Acc: accRows(sc), Download: releaseBase + geoFile, File: geoFile, Code: compareCode, DBIP: dbipURL, MM: maxmindURL}
	have := cmpCell{Text: dash}
	if t, ok := fmtPct(sc.Coverage); ok {
		have = cmpCell{Text: t + "%"}
		if g, ok := fmtPct(sc.MM.Coverage); ok {
			have.Note = "GeoLite2 " + g + "%"
			a, _ := pctTenths(sc.Coverage)
			b, _ := pctTenths(sc.MM.Coverage)
			have.Win = a > b
		}
	}
	v.Why = []cmpRow{
		{"Licence", false, []cmpCell{{Text: "CC0", Win: true}}},
		{"Sign-up", false, []cmpCell{{Text: "none", Win: true}}},
		{"Updated", false, []cmpCell{{Text: "2× a day", Win: true}}},
		{"Have a city", false, []cmpCell{have}},
		{"Layout", false, []cmpCell{{Text: "GeoLite2", Win: true}}},
	}
	if !sc.Built.IsZero() {
		v.Built = dlDate(sc.Built)
	}
	if sc.Probes > 0 {
		v.Probes = fmtInt(sc.Probes)
	}
	asnLoaded := false
	if d := s.asnf.data(); d != nil {
		asnLoaded = true
		if d.chart != "" {
			v.Chart = d.chart
			v.CSS += template.CSS(chartCSS)
		}
	}
	h := sha256.Sum256([]byte(v.Chart))
	var k strings.Builder
	k.WriteString(s.origin + "|" + v.Built + "|" + v.Probes + "|" + hex.EncodeToString(h[:8]))
	for _, rows := range [][]cmpRow{v.Why, v.Acc} {
		for _, r := range rows {
			for _, c := range r.Cells {
				k.WriteString("|" + c.Text + c.Note)
				if c.Win {
					k.WriteByte('*')
				}
			}
		}
	}
	c := &s.cmp
	c.mu.Lock()
	defer c.mu.Unlock()
	complete := asnLoaded && !sc.Built.IsZero() && have.Text != dash
	if c.pg != nil && c.key == k.String() {
		return c.pg, complete, nil
	}
	body, err := render(s.tmpl, "compare.html", v)
	if err != nil {
		return nil, false, err
	}
	c.key, c.pg = k.String(), newPage(body)
	return c.pg, complete, nil
}

func (s *server) serveCompare(w http.ResponseWriter, r *http.Request) {
	pg, complete, err := s.comparePage()
	if err != nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	pageHeaders(w.Header(), s.pageCSP(false, false, true, false))
	servePage(w, r, pg, "text/html; charset=utf-8", pageCache(complete))
}
