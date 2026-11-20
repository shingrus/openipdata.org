package main

// The ASN pages and the JSON of one network. None of them shows visitor data:
// the lookup bar is empty and nothing here reads the client address.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	maxPrefixes   = 500 // prefixes per family on a network page
	maxSameOrg    = 100 // other networks of the organization on a network page
	maxOrgNets    = 500 // networks on an organization page
	asnCache      = "public, max-age=300"
	pagePath      = "/as"
	orgPathPrefix = "/asn-"
)

// edgeCache is for a good answer (200, and its 304) of a page that holds no
// visitor data and changes only when the data is rebuilt: a browser keeps it
// 5 minutes, a shared cache (Cloudflare) 4 hours. Never for errors or
// redirects, and never for an answer with part of its data missing (pageCache).
const edgeCache = "public, max-age=300, s-maxage=14400"

// pageCache is the Cache-Control of a 200 answer: edgeCache when the page is
// complete, else asnCache, so a shared cache never keeps an incomplete page.
func pageCache(complete bool) string {
	if complete {
		return edgeCache
	}
	return asnCache
}

// The four ranking pages.
var rankings = []struct{ path, title, desc string }{
	{"/top-asn-by-ip-address", "Top networks by IPv4 addresses",
		"The largest networks (autonomous systems) ranked by announced IPv4 addresses."},
	{"/top-asn-by-ipv6", "Top networks by IPv6 space",
		"The largest networks ranked by announced IPv6 space, counted in /64 networks."},
	{"/top-organizations-by-ip-address", "Top organizations by IPv4 addresses",
		"The largest organizations ranked by IPv4 addresses announced across all their networks."},
	{"/list-of-countries-by-ipv4-allocation", "Countries by IPv4 addresses",
		"Countries ranked by the IPv4 addresses announced by their networks."},
}

// kv is one row of a head panel. Href links V; A is a second text after V (an AS
// number) that AHref links; Extra are further values of the same row.
type kv struct {
	K, V, Href string
	A, AHref   string
	None       bool   // V is "unknown" or "not loaded": shown grey
	Ext        bool   // Href leaves the site: rel="nofollow noopener"
	Title      string // the full value, shown on hover, when V is a shortened number
	Extra      []kv
}

type cell struct{ Text, Href, Class string }

type column struct{ Name, Class string }

type table struct {
	Title    string
	H1       bool
	Right    string
	Cols     []column
	Rows     [][]cell
	Note     string // "3,995 more networks"
	NoteHref string
	NoteLink string
}

type place struct {
	Title   string
	Big     string
	None    bool
	Mono    bool   // the big line is data (a prefix), set in the mono face
	Ent     bool   // the big line is the page's h1 (prefix, organization); the head label is an h2
	Txt     string // link to the prefixes.txt list, right of the head
	Sub     string
	SubPre  string // text before Sub ("London, ")
	SubHref string // Sub is a link (the country page of a prefix page's place)
	SubNone bool   // Sub is "Location unknown": shown grey
	Rows    []kv
	JSON    string
}

type pfxItem struct{ Text, Href string }

type pfxGroup struct {
	Family string
	List   []pfxItem
}

type pfxPanel struct {
	Title    string // the head; empty = "Prefixes"
	Groups   []pfxGroup
	Note     string
	NoteHref string
	NoteLink string
	TxtHref  string // prefixes.txt list, right of the head
}

// asnView is the data of every ASN page; the template asn.html lays it out.
type asnView struct {
	pageBase
	NoticeHead  string // a page that only says one thing (not found, not loaded)
	Notice      string
	NoticeRight string // right side of the notice panel's head (the search term)
	Place       *place
	Tables      []table
	Pfx         *pfxPanel
	Facts       []fact // panels of label/value rows (the status page)
}

// asnBase is the base of an ASN page; noindex keeps the page out of search
// engines (notices and search results).
func (s *server) asnBase(title string, noindex bool) pageBase {
	return pageBase{HeadJS: template.JS(headJS), ThemeJS: template.JS(themeJS), CSS: template.CSS(styleCSS + asnCSS), Title: title, NoIndex: noindex}
}

func (s *server) renderASN(v *asnView) ([]byte, error) {
	if v.Place != nil {
		v.CSS += template.CSS(placeCSS + netCSS) // the band and the cells of the head (network, organization and prefix pages)
	}
	if len(v.Facts) > 0 {
		v.CSS += template.CSS(rowsCSS) // label/value rows (the status page)
	}
	if v.Pfx != nil {
		v.CSS += template.CSS(pfCSS)
	}
	return render(s.tmpl, "asn.html", v)
}

// fmtV6 prints an amount of IPv6 addresses as a number of /64 networks (each
// 2^64 addresses): a whole number with thousands separators, "<1" for a
// positive amount below one /64, "0" for none.
func fmtV6(v float64) string {
	n := v / (1 << 64)
	switch {
	case !(v > 0):
		return "0"
	case n < 1:
		return "<1"
	case n >= math.MaxInt64:
		return fmtInt(math.MaxInt64)
	}
	return fmtInt(int64(n))
}

func asnText(n uint32) string { return "AS" + strconv.FormatUint(uint64(n), 10) }

func asnHref(n uint32) string { return pagePath + strconv.FormatUint(uint64(n), 10) }

func nameOf(n *network) string { return firstNonEmpty(n.org, n.handle) }

// handleCell is the Handle cell of a table row about a network: the handle,
// linked to the network page like the AS number beside it.
func handleCell(n *network) cell {
	if n.handle == "" {
		return cell{Text: dash, Class: "h"}
	}
	return cell{Text: n.handle, Href: asnHref(n.asn), Class: "h"}
}

// orgCell is the Organization cell of a table row about a network: the name,
// linked to the organization page when the network has one (a handle standing
// in for a missing name is not linked).
func orgCell(n *network) cell {
	c := cell{Text: orElseDash(nameOf(n))}
	if n.org != "" && validSlug(n.slug) {
		c.Href = orgPathPrefix + n.slug
	}
	return c
}

func orElseDash(s string) string { return firstNonEmpty(s, dash) }

func (n *network) countText(c uint32) string {
	if !n.hasPfxes {
		return dash
	}
	return fmtInt(int64(c))
}

// renderASNPages renders the four ranking pages of one load. They hold no
// visitor data, so each is served from memory with an ETag.
func (s *server) renderASNPages(d *asnData) {
	d.pages = map[string]*cachedPage{}
	netTable := func(title string, ids []int32, amount func(*network) string, head string) table {
		t := table{Title: title, H1: true, Right: "Top " + strconv.Itoa(len(ids)),
			Cols: []column{{"#", "r"}, {"AS", ""}, {"Organization", ""}, {head, "n"}}}
		for i, id := range ids {
			n := &d.nets[id]
			t.Rows = append(t.Rows, []cell{
				{Text: strconv.Itoa(i + 1), Class: "r"},
				{Text: asnText(n.asn), Href: asnHref(n.asn), Class: "a"},
				orgCell(n),
				{Text: amount(n), Class: "n"},
			})
		}
		return t
	}
	orgTable := func(title string) table {
		t := table{Title: title, H1: true, Right: "Top " + strconv.Itoa(len(d.topOrgs)),
			Cols: []column{{"#", "r"}, {"Organization", ""}, {"Networks", "n s"}, {"IPv4 addresses", "n"}}}
		for i, o := range d.topOrgs {
			t.Rows = append(t.Rows, []cell{
				{Text: strconv.Itoa(i + 1), Class: "r"},
				{Text: orElseDash(o.name), Href: orgPathPrefix + o.slug},
				{Text: fmtInt(int64(o.n)), Class: "n s"},
				{Text: fmtInt(o.v4), Class: "n"},
			})
		}
		return t
	}
	countryTable := func(title string) table {
		t := table{Title: title, H1: true, Right: fmtInt(int64(len(d.countries))) + " countries",
			Cols: []column{{"#", "r"}, {"Country", ""}, {"IPv4 addresses", "n"}, {"Networks", "n"}}}
		for i, c := range d.countries {
			name := cell{Text: c.name}
			if slug := d.cslug[c.cc]; slug != "" {
				name.Href = countryHrefOf(slug)
			}
			t.Rows = append(t.Rows, []cell{{Text: strconv.Itoa(i + 1), Class: "r"}, name, {Text: fmtInt(c.v4), Class: "n"}, {Text: fmtInt(int64(c.n)), Class: "n"}})
		}
		return t
	}
	for i, r := range rankings {
		var t table
		switch i {
		case 0:
			t = netTable(r.title, d.topV4, func(n *network) string { return fmtInt(n.v4) }, "IPv4 addresses")
		case 1:
			t = netTable(r.title, d.topV6, func(n *network) string { return fmtV6(n.v6) }, "IPv6 /64 networks")
		case 2:
			t = orgTable(r.title)
		default:
			t = countryTable(r.title)
		}
		v := &asnView{pageBase: s.asnBase(r.title+" · openipdata.org", false), Tables: []table{t}}
		v.Desc, v.Canon = r.desc, s.canon(r.path)
		body, err := s.renderASN(v)
		if err != nil {
			continue // the page then answers 500
		}
		sum := sha256.Sum256(body)
		d.pages[r.path] = &cachedPage{body: body, etag: `"` + hex.EncodeToString(sum[:12]) + `"`}
	}
}

// etagMatches reports whether an If-None-Match header names etag (weak
// comparison, a list, or "*").
func etagMatches(header, etag string) bool {
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimPrefix(strings.TrimSpace(p), "W/")
		if p == "*" || p == etag {
			return true
		}
	}
	return false
}

func pageHeaders(h http.Header, csp string) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", csp)
}

func writeBody(w http.ResponseWriter, r *http.Request, code int, ctype string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// asnNotice answers with a page that says one thing, in the site design.
func (s *server) asnNotice(w http.ResponseWriter, r *http.Request, code int, head, text, cache string) {
	pageHeaders(w.Header(), s.csp)
	w.Header().Set("Cache-Control", cache)
	if code == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "60")
	}
	body, err := s.renderASN(&asnView{pageBase: s.asnBase(head+" · openipdata.org", true), NoticeHead: head, Notice: text})
	if err != nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	writeBody(w, r, code, "text/html; charset=utf-8", body)
}

func (s *server) notLoaded(w http.ResponseWriter, r *http.Request) {
	s.asnNotice(w, r, http.StatusServiceUnavailable, "Unavailable", "ASN data is not loaded", "no-store")
}

func jsonError(w http.ResponseWriter, r *http.Request, code int, msg string, cache string) {
	h := w.Header()
	h.Set("Cache-Control", cache)
	h.Set("X-Content-Type-Options", "nosniff")
	if code == http.StatusServiceUnavailable {
		h.Set("Retry-After", "60")
	}
	b, _ := json.Marshal(map[string]string{"error": msg})
	writeBody(w, r, code, "application/json", append(b, '\n'))
}

// asNumber reads the digits of an AS number from a path part. ok is false for
// no digits or any other character. n is 0 when the number is out of range
// (0, or more than 4294967295); canon is the plain decimal text without
// leading zeros.
func asNumber(digits string) (n uint32, canon string, ok bool) {
	if digits == "" {
		return 0, "", false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, "", false
		}
	}
	canon = strings.TrimLeft(digits, "0")
	if canon == "" || len(canon) > 10 {
		return 0, canon, true
	}
	v, err := strconv.ParseUint(canon, 10, 64)
	if err != nil || v > maxASN {
		return 0, canon, true
	}
	return uint32(v), canon, true
}

// serveASN answers the ASN routes and reports whether the path was one.
func (s *server) serveASN(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, prefixPath):
		s.servePrefix(w, r)
		return true
	case strings.HasSuffix(p, prefixesTxt) && s.servePrefixList(w, r, p):
		return true
	case strings.HasPrefix(p, orgPathPrefix):
		s.serveOrg(w, r, p[len(orgPathPrefix):])
		return true
	case len(p) > len("/api/as") && strings.EqualFold(p[:len("/api/as")], "/api/as"):
		n, canon, ok := asNumber(p[len("/api/as"):])
		if !ok {
			return false
		}
		if p != "/api/as"+canon {
			jsonError(w, r, http.StatusNotFound, "not found", asnCache) // only the canonical form is served
			return true
		}
		s.serveJSON(w, r, n)
		return true
	case len(p) > len(pagePath) && strings.EqualFold(p[:len(pagePath)], pagePath):
		n, canon, ok := asNumber(p[len(pagePath):])
		if !ok {
			break
		}
		if n != 0 && p != pagePath+canon {
			h := w.Header()
			h.Set("Cache-Control", asnCache)
			h.Set("Location", pagePath+canon)
			w.WriteHeader(http.StatusMovedPermanently)
			return true
		}
		s.serveNetwork(w, r, n)
		return true
	}
	for _, rk := range rankings {
		if p == rk.path {
			s.serveRanking(w, r, p)
			return true
		}
	}
	return s.serveCountry(w, r)
}

func (s *server) serveRanking(w http.ResponseWriter, r *http.Request, path string) {
	d := s.asnf.data()
	if d == nil {
		s.notLoaded(w, r)
		return
	}
	pg := d.pages[path]
	if pg == nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	h := w.Header()
	pageHeaders(h, s.csp)
	h.Set("Cache-Control", edgeCache)
	h.Set("ETag", pg.etag)
	if etagMatches(r.Header.Get("If-None-Match"), pg.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeBody(w, r, http.StatusOK, "text/html; charset=utf-8", pg.body)
}

type netJSON struct {
	Metadata struct {
		Category    string `json:"category"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		Handle      string `json:"handle"`
	} `json:"metadata"`
	Prefixes struct {
		IPv4 []string `json:"ipv4"`
		IPv6 []string `json:"ipv6"`
	} `json:"prefixes"`
}

// netRow is one network as the database has it.
type netRow struct {
	handle, org, slug, cc, domain string
	v4                            sql.NullInt64
	js                            string
	domainFailed                  bool // the domain read failed (not "no row"): the page is incomplete
}

var errNoNetwork = errors.New("no such network")

// readNetwork reads one network and its domain. errNoNetwork when absent.
func readNetwork(ctx context.Context, db *sql.DB, n uint32) (netRow, error) {
	var r netRow
	var handle, org, slug, cc sql.NullString
	err := db.QueryRowContext(ctx, `SELECT handle, organization, organization_slug, country, ipv4_amount, json FROM asn WHERE asn = ?`, int64(n)).
		Scan(&handle, &org, &slug, &cc, &r.v4, &r.js)
	if errors.Is(err, sql.ErrNoRows) {
		return r, errNoNetwork
	}
	if err != nil {
		return r, err
	}
	r.handle, r.org, r.slug, r.cc = strings.TrimSpace(handle.String), strings.TrimSpace(org.String), slug.String, cc.String
	// A missing domain only means unknown; a failed read leaves it empty and marks the row.
	r.domain, r.domainFailed = asnDomainRead(ctx, db, n)
	return r, nil
}

func (s *server) serveJSON(w http.ResponseWriter, r *http.Request, n uint32) {
	var (
		row  netRow
		rerr error
	)
	if !s.asnf.read(func(db *sql.DB, d *asnData) {
		if n == 0 {
			rerr = errNoNetwork
			return
		}
		row, rerr = readNetwork(r.Context(), db, n)
	}) {
		jsonError(w, r, http.StatusServiceUnavailable, "ASN data is not loaded", "no-store")
		return
	}
	if errors.Is(rerr, errNoNetwork) {
		jsonError(w, r, http.StatusNotFound, "not found", asnCache)
		return
	}
	if rerr != nil {
		jsonError(w, r, http.StatusInternalServerError, "error", "no-store")
		return
	}
	out := struct {
		ASN    uint32          `json:"asn"`
		Domain string          `json:"domain,omitempty"`
		Data   json.RawMessage `json:"data"`
	}{ASN: n, Domain: row.domain, Data: json.RawMessage(row.js)}
	if !json.Valid(out.Data) {
		out.Data = json.RawMessage("null")
	}
	b, err := json.Marshal(out)
	if err != nil {
		jsonError(w, r, http.StatusInternalServerError, "error", "no-store")
		return
	}
	w.Header().Set("Cache-Control", pageCache(!row.domainFailed))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeBody(w, r, http.StatusOK, "application/json", append(b, '\n'))
}

func (s *server) serveNetwork(w http.ResponseWriter, r *http.Request, n uint32) {
	var (
		body     []byte
		complete bool
		rerr     error
	)
	ok := s.asnf.read(func(db *sql.DB, d *asnData) {
		if n == 0 {
			rerr = errNoNetwork
			return
		}
		var row netRow
		if row, rerr = readNetwork(r.Context(), db, n); rerr != nil {
			return
		}
		complete = !row.domainFailed
		body, rerr = s.renderASN(s.networkView(d, n, row))
	})
	switch {
	case !ok:
		s.notLoaded(w, r)
	case errors.Is(rerr, errNoNetwork):
		text := asnText(n) + " was not found"
		if n == 0 {
			text = "AS number out of range"
		}
		s.asnNotice(w, r, http.StatusNotFound, "Not found", text, asnCache)
	case rerr != nil:
		plain(w, http.StatusInternalServerError, "error\n")
	default:
		pageHeaders(w.Header(), s.csp)
		w.Header().Set("Cache-Control", pageCache(complete))
		writeBody(w, r, http.StatusOK, "text/html; charset=utf-8", body)
	}
}

// networkView builds the page of one network from its database row.
func (s *server) networkView(d *asnData, n uint32, row netRow) *asnView {
	var j netJSON
	jsonOK := json.Unmarshal([]byte(row.js), &j) == nil
	handle := firstNonEmpty(row.handle, j.Metadata.Handle)
	cc := row.cc
	country := firstNonEmpty(j.Metadata.Country, d.country[cc])
	if country == "" {
		country = cc
	}
	cat := words(j.Metadata.Category) // as in the Network panel: ISP, Hosting

	big, sub := firstNonEmpty(row.org, handle), ""
	if row.org != "" {
		sub = handle
	}
	pl := &place{Title: asnText(n), Big: big, Sub: sub, JSON: "/api/as" + strconv.FormatUint(uint64(n), 10)}
	if big == "" {
		pl.Big, pl.None = "Name unknown", true
	}
	if r := d.rankV4(n); r > 0 {
		pl.Sub = "#" + fmtInt(int64(r)) + " by IPv4 addresses" // the place in the ranking; the handle has its own row
	}
	org := kv{K: "Organization", V: orElseDash(row.org)}
	if row.org != "" && validSlug(row.slug) {
		org.Href = orgPathPrefix + row.slug
	}
	hnd := kv{K: "Handle", V: orElseDash(handle)}
	if handle != "" && validSlug(row.slug) { // like the handle of the connection panel: to the organization page
		hnd.Href = orgPathPrefix + row.slug
	}
	pfxCount := func(l []string) string {
		if !jsonOK || l == nil { // not JSON, or the list is not in it
			return dash
		}
		return fmtInt(int64(len(l)))
	}
	v4 := dash
	if row.v4.Valid {
		v4 = fmtInt(row.v4.Int64)
	}
	ctry := kv{K: "Country", V: orElseDash(country)}
	if slug := d.cslug[cc]; slug != "" && country != "" {
		ctry.Href = countryHrefOf(slug)
	}
	pl.Rows = []kv{
		hnd,
		org,
		domainKV(row.domain),
		ctry,
		{K: "Type", V: orElseDash(cat)},
		{K: "IPv4 prefixes", V: pfxCount(j.Prefixes.IPv4)},
		{K: "IPv6 prefixes", V: pfxCount(j.Prefixes.IPv6)},
		{K: "IPv4 addresses", V: v4},
	}
	var n4, n6 int64
	if jsonOK {
		n4, n6 = int64(len(j.Prefixes.IPv4)), int64(len(j.Prefixes.IPv6))
	}
	title, desc := networkMeta(n, firstNonEmpty(row.org, handle), country, n4, n6, jsonOK && j.Prefixes.IPv4 != nil && j.Prefixes.IPv6 != nil)
	v := &asnView{pageBase: s.asnBase(title, false), Place: pl}
	v.Desc, v.Canon = desc, s.canon(asnHref(n))
	if validSlug(row.slug) && row.org != "" {
		v.LD = s.crumbs("Networks", rankings[0].path, row.org, orgPathPrefix+row.slug, asnText(n), asnHref(n))
	} else {
		v.LD = s.crumbs("Networks", rankings[0].path, asnText(n), asnHref(n))
	}

	if t, ok := sameOrgTable(d, row.slug, n); ok {
		v.Tables = append(v.Tables, t)
	}

	if len(j.Prefixes.IPv4)+len(j.Prefixes.IPv6) > 0 {
		pp := &pfxPanel{}
		api := pl.JSON
		var capped []string
		for _, g := range []struct {
			fam  string
			list []string
		}{{"IPv4", j.Prefixes.IPv4}, {"IPv6", j.Prefixes.IPv6}} {
			if len(g.list) == 0 {
				continue
			}
			l := g.list[:min(len(g.list), maxPrefixes)]
			pp.Groups = append(pp.Groups, pfxGroup{Family: g.fam, List: pfxItems(l)})
			if len(g.list) > maxPrefixes {
				capped = append(capped, g.fam+" "+fmtInt(int64(len(g.list))))
			}
		}
		if len(capped) > 0 {
			pp.Note = "Showing the first " + strconv.Itoa(maxPrefixes) + " per family. Total: " + strings.Join(capped, ", ") + "."
			pp.NoteHref, pp.NoteLink = api, "All prefixes in JSON"
		}
		pp.TxtHref = prefixesHref(asnHref(n))
		v.Pfx = pp
	}
	return v
}

func (s *server) serveOrg(w http.ResponseWriter, r *http.Request, slug string) {
	d := s.asnf.data()
	if d == nil {
		s.notLoaded(w, r)
		return
	}
	ids := []int32(nil)
	if validSlug(slug) {
		ids = d.org(slug)
	}
	if len(ids) == 0 {
		// An older spelling (capitals, spaces, other characters): one hop to the canonical slug.
		if c := cleanOrgSlug(slug); c != slug && validSlug(c) && len(d.org(c)) > 0 {
			w.Header().Set("Cache-Control", asnCache)
			w.Header().Set("Location", orgPathPrefix+c)
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		s.asnNotice(w, r, http.StatusNotFound, "Not found", "Organization not found", asnCache)
		return
	}
	var total int64
	var npfx uint64
	name := ""
	for _, id := range ids {
		n := &d.nets[id]
		total += n.v4
		npfx += uint64(n.n4) + uint64(n.n6)
		if name == "" {
			name = n.org
		}
	}
	sorted := slices.Clone(ids)
	slices.SortFunc(sorted, func(a, b int32) int {
		x, y := d.nets[a].v4, d.nets[b].v4
		switch {
		case x > y:
			return -1
		case x < y:
			return 1
		}
		return int(d.nets[a].asn) - int(d.nets[b].asn)
	})
	rows, complete := s.orgFacts(r.Context(), d, sorted, total)
	pl := &place{Title: "Organization", Big: name, Ent: true, Rows: rows}
	if name == "" {
		pl.Big, pl.None = "Name unknown", true
	}
	// d.orgs is sorted by IPv4 addresses (most first), then slug: the place is found by that order.
	if i, ok := slices.BinarySearchFunc(d.orgs, slug, func(o orgRow, s string) int {
		if o.v4 != total {
			if o.v4 > total {
				return -1
			}
			return 1
		}
		return strings.Compare(o.slug, s)
	}); ok && total > 0 {
		pl.Sub = "#" + fmtInt(int64(i+1)) + " by IPv4 addresses" // the place in the organization ranking
	}
	if npfx > 0 {
		pl.Txt = prefixesHref(orgPathPrefix + slug)
	}
	t := table{Title: "Networks", Right: fmtInt(int64(len(ids))),
		Cols: []column{{"AS", ""}, {"Handle", ""}, {"Country", "s"}, {"IPv4 addresses", "n"}, {"IPv4 prefixes", "n s"}, {"IPv6 prefixes", "n s"}}}
	for _, id := range sorted[:min(len(sorted), maxOrgNets)] {
		n := &d.nets[id]
		cc := dash
		if n.cc != "" {
			cc = d.countryName(n.cc)
		}
		t.Rows = append(t.Rows, []cell{
			{Text: asnText(n.asn), Href: asnHref(n.asn), Class: "a"},
			handleCell(n),
			{Text: cc, Class: "s"},
			{Text: fmtInt(n.v4), Class: "n"},
			{Text: n.countText(n.n4), Class: "n s"},
			{Text: n.countText(n.n6), Class: "n s"},
		})
	}
	if len(sorted) > maxOrgNets {
		t.Note = fmtInt(int64(len(sorted)-maxOrgNets)) + " more networks"
	}
	title, desc := orgMeta(firstNonEmpty(name, slug), int64(len(ids)), total)
	v := &asnView{pageBase: s.asnBase(title, false), Place: pl, Tables: []table{t}}
	v.Desc, v.Canon = desc, s.canon(orgPathPrefix+slug)
	v.LD = s.crumbs("Organizations", rankings[2].path, firstNonEmpty(name, slug), orgPathPrefix+slug)
	body, err := s.renderASN(v)
	if err != nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	pageHeaders(w.Header(), s.csp)
	w.Header().Set("Cache-Control", pageCache(complete))
	writeBody(w, r, http.StatusOK, "text/html; charset=utf-8", body)
}

// mainCountry is the country code holding most of the IPv4 addresses of these
// networks (ties: more networks, then the code), or "" when none has a country.
func mainCountry(d *asnData, ids []int32) string {
	type sum struct {
		v4 int64
		n  int
	}
	by := map[string]sum{}
	for _, id := range ids {
		if n := &d.nets[id]; n.cc != "" {
			c := by[n.cc]
			by[n.cc] = sum{c.v4 + n.v4, c.n + 1}
		}
	}
	best := ""
	for cc, c := range by {
		if b := by[best]; best == "" || c.v4 > b.v4 || c.v4 == b.v4 && (c.n > b.n || c.n == b.n && cc < best) {
			best = cc
		}
	}
	return best
}

// sameOrgTable is the table "Same organization" of a network or prefix page:
// the other networks of the organization slug (not asn), by number, at most
// maxSameOrg. False when there is none.
func sameOrgTable(d *asnData, slug string, asn uint32) (table, bool) {
	if !validSlug(slug) {
		return table{}, false
	}
	var others []int32
	for _, id := range d.org(slug) {
		if d.nets[id].asn != asn {
			others = append(others, id)
		}
	}
	if len(others) == 0 {
		return table{}, false
	}
	t := table{Title: "Same organization", Right: fmtInt(int64(len(others))) + " others",
		Cols: []column{{"AS", ""}, {"Handle", ""}, {"IPv4 prefixes", "n"}, {"IPv6 prefixes", "n s"}}}
	for _, id := range others[:min(len(others), maxSameOrg)] {
		o := &d.nets[id]
		t.Rows = append(t.Rows, []cell{
			{Text: asnText(o.asn), Href: asnHref(o.asn), Class: "a"},
			handleCell(o),
			{Text: o.countText(o.n4), Class: "n"},
			{Text: o.countText(o.n6), Class: "n s"},
		})
	}
	if len(others) > maxSameOrg {
		t.Note = fmtInt(int64(len(others)-maxSameOrg)) + " more networks"
		t.NoteHref, t.NoteLink = orgPathPrefix+slug, "All networks"
	}
	return t, true
}

// v6KV is the "IPv6 addresses" row of an organization: the number of /64
// networks, short like the Addresses row of an IPv6 prefix, the full number on hover.
func v6KV(v6 float64) kv {
	n := v6 / (1 << 64)
	switch {
	case n >= 1 && n < 2:
		return kv{K: "IPv6 addresses", V: "1 /64 network"}
	case n >= 1000 && n < 1e15: // above that the short form would need more units; the full number stays
		return kv{K: "IPv6 addresses", V: compactCount(int64(n)) + " /64 networks", Title: fmtV6(v6) + " /64 networks"}
	}
	return kv{K: "IPv6 addresses", V: fmtV6(v6) + " /64 networks"}
}

// orgFacts are the rows of an organization page's head, the same kind of facts
// as a network page has: its largest network, how many networks, a domain, the
// country, then the prefix counts and the address space of all its networks.
// sorted is the organization's networks, most IPv4 addresses first.
func (s *server) orgFacts(ctx context.Context, d *asnData, sorted []int32, total int64) (rows []kv, complete bool) {
	top := &d.nets[sorted[0]]
	largest := kv{K: "Largest network", V: firstNonEmpty(top.handle, asnText(top.asn)), Href: asnHref(top.asn)}
	// The domain of the largest network that has one (the first few are tried).
	domain := kv{K: "Domain", V: dash}
	complete = true
	if !s.asnf.read(func(db *sql.DB, _ *asnData) {
		for _, id := range sorted[:min(len(sorted), maxNetworkRows)] {
			dom, failed := asnDomainRead(ctx, db, d.nets[id].asn)
			if failed {
				complete = false
			}
			if dom != "" {
				domain = domainKV(dom)
				return
			}
		}
	}) {
		complete = false
	}
	// The country with most of the IPv4 addresses (mainCountry); "+N" = other countries.
	ccs := map[string]bool{}
	var n4, n6 uint64
	var v6 float64
	pfx := false
	for _, id := range sorted {
		n := &d.nets[id]
		n4, n6, v6 = n4+uint64(n.n4), n6+uint64(n.n6), v6+n.v6
		pfx = pfx || n.hasPfxes
		if n.cc != "" {
			ccs[n.cc] = true
		}
	}
	country, best := kv{K: "Country", V: dash}, mainCountry(d, sorted)
	if best != "" {
		country.V = d.countryName(best)
		if slug := d.cslug[best]; slug != "" {
			country.Href = countryHrefOf(slug)
		}
		if more := len(ccs) - 1; more > 0 {
			country.V += " +" + strconv.Itoa(more)
		}
	}
	count := func(n uint64) string {
		if !pfx { // no network of it has the prefix lists
			return dash
		}
		return fmtInt(int64(n))
	}
	return []kv{
		largest,
		{K: "Networks", V: fmtInt(int64(len(sorted)))},
		domain,
		country,
		{K: "IPv4 prefixes", V: count(n4)},
		{K: "IPv6 prefixes", V: count(n6)},
		{K: "IPv4 addresses", V: fmtInt(total)},
		v6KV(v6),
	}, complete
}

// asnLink is the page of AS number digits (as the ip2asn MMDB writes it) when
// the ASN data is loaded and has the network, else "".
func (s *server) asnLink(digits string) string {
	n, _, ok := asNumber(digits)
	if !ok || n == 0 {
		return ""
	}
	d := s.asnf.data()
	if d == nil {
		return ""
	}
	if _, found := d.find(n); !found {
		return ""
	}
	return asnHref(n)
}
