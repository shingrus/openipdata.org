package main

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parseDay(d string) (time.Time, error) { return time.Parse("2006-01-02", d) }

func histFixture() []scoreFixture {
	return []scoreFixture{
		{"2026-11-20T07:00:00Z", [7]any{70.0, 97.0, 59.5, 60.0, 55.0, 58.0, 57.0}},
		{"2026-11-18T07:00:00Z", [7]any{69.0, 96.5, 58.0, 59.0, nil, 57.5, 56.0}},
		{"garbage", [7]any{1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0}},
		{"2026-11-21T07:00:00Z", [7]any{71.0, 97.2, 60.1, 60.5, 150.0, nil, math.Inf(1)}},
	}
}

func openFixture(t *testing.T, scores ...scoreFixture) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.sqlite3")
	writeASNFile(t, path, fixtureRows(), scores...)
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func historyServer(t testing.TB) *server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "asn-openipdata.sqlite3")
	writeASNFile(t, path, fixtureRows(), histFixture()...)
	s, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	s.useASN(path)
	return s
}

func TestLoadScores(t *testing.T) {
	pts, err := loadScores(context.Background(), openFixture(t))
	if err != nil || pts != nil {
		t.Fatalf("no table: %v, %v", pts, err)
	}
	pts, err = loadScores(context.Background(), openFixture(t, histFixture()...))
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 { // the row with a bad time is skipped
		t.Fatalf("%d points", len(pts))
	}
	for i := 1; i < len(pts); i++ {
		if !pts[i-1].t.Before(pts[i].t) {
			t.Error("not oldest first")
		}
	}
	if pts[0].v[0] != 69 || !math.IsNaN(pts[0].v[4]) {
		t.Errorf("first point %v", pts[0].v)
	}
	last := pts[2]
	if !math.IsNaN(last.v[4]) || !math.IsNaN(last.v[5]) || !math.IsNaN(last.v[6]) { // 150, NULL and +Inf are unknown
		t.Errorf("last point %v", last.v)
	}
}

func TestScoreValue(t *testing.T) {
	for _, x := range []any{nil, "x", -1.0, 100.5, math.NaN(), math.Inf(-1)} {
		if !math.IsNaN(scoreValue(x)) {
			t.Errorf("%v is known", x)
		}
	}
	if scoreValue(int64(7)) != 7 || scoreValue(0.0) != 0 || scoreValue(100.0) != 100 {
		t.Error("valid values changed")
	}
}

func nan7() [7]float64 {
	var v [7]float64
	for i := range v {
		v[i] = math.NaN()
	}
	return v
}

func pt(day string, set map[int]float64) scorePoint {
	v := nan7()
	for i, x := range set {
		v[i] = x
	}
	t, _ := parseDay(day)
	return scorePoint{t: t, v: v}
}

func TestDrawScoreChart(t *testing.T) {
	if drawScoreChart(nil) != "" || drawScoreChart([]scorePoint{pt("2026-11-20", nil)}) != "" {
		t.Error("a chart without values")
	}
	// A NULL splits the line into two paths; a lone value is a dot.
	h := string(drawScoreChart([]scorePoint{
		pt("2026-11-10", map[int]float64{2: 60, 1: 90}),
		pt("2026-11-12", map[int]float64{2: 61, 1: 91}),
		pt("2026-11-14", map[int]float64{1: 92}),
		pt("2026-11-16", map[int]float64{2: 62, 1: 93}),
		pt("2026-11-18", map[int]float64{2: 63, 1: 94}),
		pt("2026-11-20", map[int]float64{2: 64, 1: 95, 0: 70}),
	}))
	if n := strings.Count(h, `<path class="c3"`); n != 2 {
		t.Errorf("%d paths for the gap line", n)
	}
	if n := strings.Count(h, `<path class="c2"`); n != 1 {
		t.Errorf("%d paths for the full line", n)
	}
	if n := strings.Count(h, `<path class="c1 pt" d="M1000 `); n != 1 {
		t.Errorf("%d dots", n)
	}
	for _, left := range []string{"Precision", "Holdout city", "Right city (MaxMind)", "Right city (DB-IP)"} {
		if strings.Contains(h, left) {
			t.Errorf("legend has %s without values", left)
		}
	}
	for _, want := range []string{"Has a city <b>70.0%</b>", "Right city <b>64.0%</b>", `aria-label="Geo scores, last 60 days"`, ">10 Nov<", ">20 Nov<", `>60</span>`, `class="x f" style="left:0%"`, `class="x l" style="left:100%"`, `preserveAspectRatio="none"`, `M0 0H1000`} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(h, "MaxMind and DB-IP") || strings.Contains(h, "dash") {
		t.Error("vendor credit or dash without vendor lines")
	}
	// The floor is the nearest 10 below the smallest value: 60 -> labels 60..100 every 10; no 50.
	if strings.Contains(h, `>50</span>`) || strings.Contains(h, "<text") || strings.Contains(h, "<circle") {
		t.Error("floor below the smallest value")
	}
	// A wide range uses steps of 20 and never goes below 0.
	w := string(drawScoreChart([]scorePoint{pt("2026-11-10", map[int]float64{0: 5}), pt("2026-11-11", map[int]float64{0: 90})}))
	if !strings.Contains(w, `>0</span>`) || !strings.Contains(w, `>20</span>`) || !strings.Contains(w, `>100</span>`) || strings.Contains(w, `>10</span>`) {
		t.Errorf("wide range labels: %s", w)
	}
	// A single point is a dot, vendors are dashed and credited.
	one := string(drawScoreChart([]scorePoint{pt("2026-11-10", map[int]float64{2: 60, 5: 58})}))
	if strings.Count(one, ` pt" d="`) != 2 || strings.Contains(one, "L") && strings.Contains(one, `class="c3" d="M`) && strings.Contains(one, "L5") || !strings.Contains(one, `left:50%`) {
		t.Errorf("single point: %s", one)
	}
	two := string(drawScoreChart([]scorePoint{pt("2026-11-10", map[int]float64{5: 58}), pt("2026-11-12", map[int]float64{5: 59})}))
	if !strings.Contains(two, `class="c6 dash"`) || !strings.Contains(two, `<i class="sw c6 dash">`) || !strings.Contains(two, "IP Geolocation by") {
		t.Errorf("vendor line: %s", two)
	}
	// Spacing follows time, not the index: the middle point (day 2 of 10) sits at 20%.
	sp := string(drawScoreChart([]scorePoint{pt("2026-11-10", map[int]float64{2: 60}), pt("2026-11-12", map[int]float64{2: 61}), pt("2026-11-20", map[int]float64{2: 62})}))
	if !strings.Contains(sp, "L200 ") {
		t.Errorf("time spacing: %s", sp)
	}
}

func TestStatusScoreHistory(t *testing.T) {
	// With ASN data but no history.
	s, _ := asnServer(t)
	b := get(s, "GET", "/status").Body.String()
	if !strings.Contains(b, "<h2>Score history</h2>") || !strings.Contains(b, `<dd class="none">no data</dd>`) || strings.Contains(b, `class="chart"`) {
		t.Error("status without history")
	}
	// Without ASN data at all.
	n, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	if b := get(n, "GET", "/status").Body.String(); !strings.Contains(b, `<dd class="none">no data</dd>`) {
		t.Error("status without ASN data")
	}
	// With history, right after the Accuracy panel.
	b = get(historyServer(t), "GET", "/status").Body.String()
	for _, want := range []string{`<div class="pw" role="img"`, "Right city (MaxMind)", "includes GeoLite data created by MaxMind"} {
		if !strings.Contains(b, want) {
			t.Errorf("status with history lacks %q", want)
		}
	}
	a, h := strings.Index(b, "<h2>Accuracy</h2>"), strings.Index(b, "<h2>Score history</h2>")
	if a < 0 || h < a || strings.Index(b, "<h2>DNS beacon</h2>") < h {
		t.Error("panel order")
	}
	if strings.Contains(b, "<script") && strings.Contains(b, `src="http`) {
		t.Error("external script")
	}
}

// A short history shows a date twice; the right-aligned last label is kept.
func TestScoreChartShortSpanLabels(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	var v [nSeries]float64
	for i := range v {
		v[i] = math.NaN()
	}
	v[2] = 60
	h := string(drawScoreChart([]scorePoint{{t: t0, v: v}, {t: t0.Add(12 * time.Hour), v: v}}))
	if !strings.Contains(h, `<span class="x l" style="left:100%">21 Nov</span>`) {
		t.Errorf("last label missing: %s", h)
	}
	if strings.Count(h, ">21 Nov<") != 1 {
		t.Error("a date is shown twice")
	}
	if strings.Contains(h, `class="chart" role=`) || !strings.Contains(h, `<div class="pw" role="img"`) {
		t.Error("role=img must be on the plot only, not on the legend and credit line")
	}
}
