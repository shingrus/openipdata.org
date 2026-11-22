package main

// The geo score history on /status: the worker writes one row per published
// geo build into the table geo_scores of the ASN SQLite file; it is read while
// the data loads and drawn once per load as one inline SVG (no script, no
// request). A file without the table has no history.

import (
	"context"
	"database/sql"
	_ "embed"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed static/chart.css
var chartSrc string

var chartCSS = cssText(chartSrc)

const maxScoreRows = 200

// scoreSeries are the lines of the chart in column order of the table.
var scoreSeries = []struct {
	name   string
	vendor bool // dashed, and the vendor credit line
}{
	{"Has a city", false}, {"Right country", false}, {"Right city", false}, {"Precision", false}, {"Holdout city", false},
	{"Right city (MaxMind)", true}, {"Right city (DB-IP)", true},
}

const nSeries = 7

// scorePoint is one published build; an unknown value is NaN.
type scorePoint struct {
	t time.Time
	v [nSeries]float64
}

// scoreValue turns a column into a percentage; NULL, anything that is not a
// finite number and anything outside 0..100 is unknown.
func scoreValue(x any) float64 {
	var f float64
	switch n := x.(type) {
	case float64:
		f = n
	case int64:
		f = float64(n)
	default:
		return math.NaN()
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 100 {
		return math.NaN()
	}
	return f
}

// loadScores reads the newest rows of geo_scores, oldest first. A file without
// the table has none; a row whose time does not parse is skipped.
func loadScores(ctx context.Context, db *sql.DB) ([]scorePoint, error) {
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, "geo_scores").Scan(&one)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT run_at, city_pct, atlas_country_pct, atlas_city_pct, precision_pct, holdout_city_pct, maxmind_atlas_city_pct, dbip_atlas_city_pct
 FROM geo_scores ORDER BY run_at DESC LIMIT ?`, maxScoreRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pts []scorePoint
	for rows.Next() {
		var at sql.NullString
		var c [nSeries]any
		if err := rows.Scan(&at, &c[0], &c[1], &c[2], &c[3], &c[4], &c[5], &c[6]); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(at.String))
		if err != nil {
			continue
		}
		p := scorePoint{t: t.UTC()}
		for i := range c {
			p.v[i] = scoreValue(c[i])
		}
		pts = append(pts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].t.Before(pts[j].t) })
	return pts, nil
}

func chartNum(v float64) string {
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
}

// drawScoreChart is the whole body of the panel: the SVG, the legend and the
// credit line. It is empty when no series has a value.
func drawScoreChart(pts []scorePoint) template.HTML {
	var shown []int
	lo, vendors := 100.0, false
	for i := 0; i < nSeries; i++ {
		has := false
		for _, p := range pts {
			if !math.IsNaN(p.v[i]) {
				has = true
				lo = math.Min(lo, p.v[i])
			}
		}
		if has {
			shown = append(shown, i)
			vendors = vendors || scoreSeries[i].vendor
		}
	}
	if len(shown) == 0 {
		return ""
	}
	// The plot is an SVG in data units (1000 x 1000, stretched to its CSS box);
	// the labels are HTML around it, so text and strokes keep their size.
	step := 10.0
	if 100-lo > 60 {
		step = 20
	}
	ymin := math.Min(math.Floor(lo/step)*step, 100-step)
	y := func(v float64) float64 { return (100 - v) / (100 - ymin) * 1000 }
	t0, t1 := pts[0].t, pts[len(pts)-1].t
	span := t1.Sub(t0)
	x := func(t time.Time) float64 {
		if span <= 0 {
			return 500
		}
		return float64(t.Sub(t0)) / float64(span) * 1000
	}

	var svg, ylab, xlab strings.Builder
	svg.WriteString(`<g class="gr">`)
	for v := ymin; v <= 100.001; v += step {
		svg.WriteString(`<path d="M0 ` + chartNum(y(v)) + `H1000"/>`)
		ylab.WriteString(`<span class="y" style="top:` + chartNum(y(v)/10) + `%">` + strconv.Itoa(int(v)) + `</span>`)
	}
	svg.WriteString(`</g>`)
	nl := 5
	if span <= 0 {
		nl = 1
	}
	// A date shown twice (a short history) keeps its later label, so the
	// right-aligned last one is never the one left out.
	type dateLabel struct {
		k   int
		t   time.Time
		lbl string
	}
	var dates []dateLabel
	seen := map[string]bool{}
	for k := nl - 1; k >= 0; k-- {
		t := t0
		if nl > 1 {
			t = t0.Add(time.Duration(float64(span) * float64(k) / float64(nl-1)))
		}
		lbl := t.Format("2 Jan")
		if seen[lbl] {
			continue
		}
		seen[lbl] = true
		dates = append([]dateLabel{{k, t, lbl}}, dates...)
	}
	for _, d := range dates {
		cls := "x"
		switch {
		case nl > 1 && d.k == nl-1:
			cls += " l"
		case nl > 1 && d.k == 0:
			cls += " f"
		case d.k%2 == 1:
			cls += " m" // hidden on a phone, where five dates do not fit
		}
		xlab.WriteString(`<span class="` + cls + `" style="left:` + chartNum(x(d.t)/10) + `%">` + d.lbl + `</span>`)
	}
	for _, i := range shown {
		cls := "c" + strconv.Itoa(i+1)
		if scoreSeries[i].vendor {
			cls += " dash"
		}
		flush := func(run []scorePoint) {
			switch len(run) {
			case 0:
			case 1: // a zero-length segment with round caps is a dot that does not stretch
				svg.WriteString(`<path class="` + strings.TrimSuffix(cls, " dash") + ` pt" d="M` + chartNum(x(run[0].t)) + " " + chartNum(y(run[0].v[i])) + `h0"/>`)
			default:
				svg.WriteString(`<path class="` + cls + `" d="`)
				for j, p := range run {
					c := "L"
					if j == 0 {
						c = "M"
					}
					svg.WriteString(c + chartNum(x(p.t)) + " " + chartNum(y(p.v[i])))
				}
				svg.WriteString(`"/>`)
			}
		}
		var run []scorePoint
		for _, p := range pts {
			if math.IsNaN(p.v[i]) {
				flush(run)
				run = run[:0]
				continue
			}
			run = append(run, p)
		}
		flush(run)
	}

	var b strings.Builder
	b.WriteString(`<div class="chart"><div class="pw" role="img" aria-label="Geo scores, last 60 days"><svg viewBox="0 0 1000 1000" preserveAspectRatio="none" aria-hidden="true">`)
	b.WriteString(svg.String())
	b.WriteString(`</svg><div class="yl" aria-hidden="true">` + ylab.String() + `</div></div><div class="xl" aria-hidden="true">` + xlab.String() + `</div><ul class="lg">`)
	last := pts[len(pts)-1]
	for _, i := range shown {
		val := `<b class="none">` + dash + `</b>`
		if s, ok := fmtPct(last.v[i]); ok {
			val = "<b>" + s + "%</b>"
		}
		sw := "sw c" + strconv.Itoa(i+1)
		if scoreSeries[i].vendor {
			sw += " dash"
		}
		b.WriteString(`<li><i class="` + sw + `"></i>` + template.HTMLEscapeString(scoreSeries[i].name) + " " + val + "</li>")
	}
	b.WriteString(`</ul>`)
	if vendors {
		b.WriteString(`<p class="note">MaxMind and DB-IP measured on the same RIPE Atlas probes. IP Geolocation by <a href="` + dbipURL + `">DB-IP</a> · includes GeoLite data created by MaxMind.</p>`)
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}
