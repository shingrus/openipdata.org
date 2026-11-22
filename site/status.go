package main

// GET /status: a page for the owner with the state of this process and of the
// data it uses. No visitor statistics exist or are collected; nothing from the
// request is used, no address, token or secret is shown.

import (
	"html/template"
	"net/http"
	"path/filepath"
	"strconv"
	"time"
)

// fileStatus describes one data file in use. Built is the build time of an
// MMDB (from its metadata) or the modification time of the SQLite file.
type fileStatus struct {
	Loaded   bool
	Built    time.Time
	LoadedAt time.Time
	Size     int64
}

// fact is a panel of label/value rows.
type fact struct {
	Title string
	Rows  []kv
	Chart template.HTML // a panel with a chart has it instead of rows
}

// ageText says how long ago t was: "just now" under a minute, then minutes,
// hours and days (singular for 1).
func ageText(t, now time.Time) string {
	d := now.Sub(t)
	unit := func(n int64, name string) string {
		if n != 1 {
			name += "s"
		}
		return strconv.FormatInt(n, 10) + " " + name + " ago"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return unit(int64(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return unit(int64(d/time.Hour), "hour")
	}
	return unit(int64(d/(24*time.Hour)), "day")
}

// timeAge is "15 Nov 2026 13:00 UTC · 6 hours ago", or a dash for no time.
func timeAge(t, now time.Time) string {
	if t.IsZero() {
		return dash
	}
	return t.UTC().Format("2 Jan 2006 15:04 UTC") + " · " + ageText(t, now)
}

func fmtMB(n int64) string {
	return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
}

// fileRows are the rows every data file has; a file that is not loaded has the
// one row "not loaded".
func fileRows(st fileStatus, now time.Time) []kv {
	if !st.Loaded {
		return []kv{{K: "Loaded", V: "not loaded", None: true}}
	}
	return []kv{
		{K: "Built", V: timeAge(st.Built, now), None: st.Built.IsZero()},
		{K: "Loaded", V: timeAge(st.LoadedAt, now)},
		{K: "Size", V: fmtMB(st.Size)},
	}
}

// baseName is the file name only, never the directory.
func baseName(path, fallback string) string {
	if path == "" {
		return fallback
	}
	return filepath.Base(path)
}

func (s *server) statusFacts() []fact {
	now := s.now()
	facts := []fact{{Title: "Server", Rows: []kv{
		{K: "Started", V: timeAge(s.started, now)},
		{K: "Version", V: version},
	}}}

	facts = append(facts,
		fact{Title: baseName(s.geo.path, "ip2geo MMDB"), Rows: fileRows(s.geo.status(), now)},
		fact{Title: baseName(s.asn.path, "ip2asn MMDB"), Rows: fileRows(s.asn.status(), now)})
	st, d := s.asnf.status()
	rows := fileRows(st, now)
	if d != nil {
		rows = append(rows,
			kv{K: "Networks", V: fmtInt(int64(len(d.nets)))},
			kv{K: "Organizations", V: fmtInt(int64(len(d.orgs)))},
			kv{K: "Prefixes", V: fmtInt(int64(d.idx.size()))},
			kv{K: "Load time", V: d.loadedIn.Round(time.Millisecond).String()})
	}
	facts = append(facts, fact{Title: baseName(s.asnf.path, "ASN SQLite"), Rows: rows})

	sc := *s.scores.Load()
	pct := func(k string, v float64) kv {
		p, ok := fmtPct(v)
		if ok {
			p += "%"
		}
		return kv{K: k, V: p, None: !ok}
	}
	acc := []kv{
		pct("Right country", sc.Country), pct("Right city", sc.City), pct("Has a city", sc.Coverage),
		{K: "Devices", V: dash, None: true}, {K: "Built", V: dash, None: true},
	}
	if sc.Probes >= 0 {
		acc[3] = kv{K: "Devices", V: fmtInt(sc.Probes)}
	}
	if !sc.Built.IsZero() {
		acc[4] = kv{K: "Built", V: timeAge(sc.Built, now)}
	}
	facts = append(facts, fact{Title: "Accuracy", Rows: acc})
	hist := fact{Title: "Score history", Rows: []kv{{K: "History", V: "no data", None: true}}}
	if d != nil && d.chart != "" {
		hist = fact{Title: "Score history", Chart: d.chart}
	}
	facts = append(facts, hist)

	dns := []kv{{K: "Row", V: "off", None: true}}
	if s.zone != "" {
		red := kv{K: "Redis", V: "unreachable", None: true}
		if s.redis != nil && s.redis.ping() {
			red = kv{K: "Redis", V: "ok"}
		}
		dns = []kv{{K: "Row", V: "on"}, {K: "Zone", V: s.zone}, red}
	}
	facts = append(facts, fact{Title: "DNS beacon", Rows: dns})
	sh := kv{K: "Share", V: "off", None: true}
	if s.shareOn() {
		sh = kv{K: "Share", V: "on"}
	}
	return append(facts, fact{Title: "Share", Rows: []kv{sh}})
}

func (s *server) serveStatus(w http.ResponseWriter, r *http.Request) {
	pageHeaders(w.Header(), s.csp)
	w.Header().Set("Cache-Control", "no-store")
	v := &asnView{pageBase: s.asnBase("Status · openipdata.org", true), Facts: s.statusFacts()}
	body, err := s.renderASN(v) // rendered here, nothing is written under a lock
	if err != nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	writeBody(w, r, http.StatusOK, "text/html; charset=utf-8", body)
}
