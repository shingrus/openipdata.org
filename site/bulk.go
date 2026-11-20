package main

// Bulk lookup: GET /bulk-ip-lookup (the form and the API examples), POST
// /bulk-ip-lookup and POST / with several addresses (the results as TSV, or a
// CSV download). Addresses and pasted text are never logged or stored.

import (
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

//go:embed static/bulk.css
var bulkSrc string

// bulkCSS is the whole style of the page, joined once.
var bulkCSS = template.CSS(styleCSS + cssText(bulkSrc))

const (
	bulkPath   = "/bulk-ip-lookup"
	bulkMax    = 1000      // most addresses looked up at once
	bulkBody   = 256 << 10 // most bytes of a form post
	bulkTitle  = "Bulk IP geolocation lookup (free, CSV) | openipdata.org"
	bulkDesc   = "Look up the country, city, coordinates and network of up to 1000 IP addresses at once. Free, no signup, copy or download as CSV, with a JSON API."
	bulkHeader = "ip\tcountry_code\tcountry\tcity\tlat\tlon\tasn\tas_name"
)

// addrChar is a character an address can be made of; everything else separates.
func addrChar(c rune) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == ':' || c == '.' || c == '%'
}

// attempt reports a token that looks like an address that did not parse: four
// dotted numbers, or hex and colons with at least two colons and either "::" or
// a letter. Words, times (12:30:01) and plain numbers are not attempts.
func attempt(t string) bool {
	digits, dots, colons, letters := true, 0, 0, false
	for i := 0; i < len(t); i++ {
		switch c := t[i]; {
		case c >= '0' && c <= '9':
		case c == '.':
			dots++
		case c == ':':
			colons++
			digits = false
		case c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F':
			letters, digits = true, false
		default: // '%'
			return false
		}
	}
	if digits {
		return dots == 3
	}
	return colons >= 2 && (strings.Contains(t, "::") || letters)
}

// extractAddrs finds the addresses in any text: the distinct public ones in
// order of first appearance (at most max; more reports another one beyond),
// how many distinct non-public ones, and how many tokens look like an address
// that is not valid.
func (s *server) extractAddrs(text string, max int) (addrs []netip.Addr, nonPublic, invalid int, more bool) {
	seen := map[netip.Addr]struct{}{}
	for _, tok := range strings.FieldsFunc(text, func(c rune) bool { return !addrChar(c) }) {
		tok = strings.TrimRight(tok, ".")
		if tok == "" {
			continue
		}
		a, ok := parseAddr(tok)
		if !ok {
			if i := strings.IndexByte(tok, ':'); i > 0 && strings.Count(tok, ":") == 1 && i+1 < len(tok) && allDigits(tok[i+1:]) {
				if a, ok = parseAddr(tok[:i]); !ok && attempt(tok[:i]) {
					invalid++
					continue
				}
			}
			if !ok {
				if attempt(tok) {
					invalid++
				}
				continue
			}
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		if !s.public(a) {
			nonPublic++
			continue
		}
		if len(addrs) == max {
			more = true
			break
		}
		addrs = append(addrs, a)
	}
	return
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// bulkRows is the header and one row per address, from the same answer as the
// JSON API.
func bulkRows(res []ipJSON) [][]string {
	rows := [][]string{strings.Split(bulkHeader, "\t")}
	for _, j := range res {
		var lat, lon, asn string
		if j.Latitude != nil && j.Longitude != nil {
			lat = strconv.FormatFloat(*j.Latitude, 'f', -1, 64)
			lon = strconv.FormatFloat(*j.Longitude, 'f', -1, 64)
		}
		if j.ASN != 0 {
			asn = strconv.FormatUint(uint64(j.ASN), 10)
		}
		rows = append(rows, []string{j.IP, cellText(j.CountryCode), cellText(j.CountryName), cellText(j.CityName), lat, lon, asn, cellText(j.ASName)})
	}
	return rows
}

// bulkRow is one line of the results table on the page.
type bulkRow struct{ IP, Country, City, Coords, Net string }

// lookupAll looks every address up once, in order.
func (s *server) lookupAll(addrs []netip.Addr) []ipJSON {
	res := make([]ipJSON, len(addrs))
	for i, a := range addrs {
		res[i] = s.ipJSONFor(a)
	}
	return res
}

// tableRows is the results table as the page shows it: the country name, the
// coordinates as one value, the network as AS number and name.
func tableRows(res []ipJSON) []bulkRow {
	out := make([]bulkRow, len(res))
	for i, j := range res {
		r := bulkRow{IP: j.IP, Country: j.CountryName, City: j.CityName, Net: j.ASName}
		if j.Latitude != nil && j.Longitude != nil {
			r.Coords = strconv.FormatFloat(*j.Latitude, 'f', -1, 64) + ", " + strconv.FormatFloat(*j.Longitude, 'f', -1, 64)
		}
		if j.ASN != 0 {
			r.Net = strings.TrimSpace("AS" + strconv.FormatUint(uint64(j.ASN), 10) + " " + j.ASName)
		}
		out[i] = r
	}
	return out
}

// cellText keeps a text value from the data from being read as a formula when
// the table is pasted or opened in a spreadsheet: a leading = + - @, tab or CR gets a '.
func cellText(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

var tabSpace = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ")

func tsv(rows [][]string) string {
	var b strings.Builder
	for _, r := range rows {
		for i, v := range r {
			if i > 0 {
				b.WriteByte('\t')
			}
			b.WriteString(tabSpace.Replace(v))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

type bulkView struct {
	pageBase
	Text    string    // the textarea: the found addresses, one per line
	Summary string    // the counts line, or "No public addresses found"
	TSV     string    // the results as the copy button copies them; empty = no Results panel
	Rows    []bulkRow // the same results as the page shows them
	Cmds    []string
}

type ldWebApp struct {
	Context             string  `json:"@context"`
	Type                string  `json:"@type"`
	Name                string  `json:"name"`
	URL                 string  `json:"url"`
	Description         string  `json:"description"`
	ApplicationCategory string  `json:"applicationCategory"`
	OperatingSystem     string  `json:"operatingSystem"`
	IsAccessibleForFree bool    `json:"isAccessibleForFree"`
	Offers              ldOffer `json:"offers"`
}

type ldOffer struct {
	Type          string `json:"@type"`
	Price         string `json:"price"`
	PriceCurrency string `json:"priceCurrency"`
}

// bulkCache holds the one rendered GET page (it holds no data, only the origin).
type bulkCache struct {
	mu  sync.Mutex
	org string
	pg  *cachedPage
}

func (s *server) bulkBase(title string) pageBase {
	return pageBase{HeadJS: template.JS(headJS), ThemeJS: template.JS(themeJS), CSS: bulkCSS, Title: title,
		Pnl: true, PnlJS: template.JS(panelJS)}
}

func (s *server) bulkCmds() []string {
	return []string{"curl " + s.origin + "/json/1.1.1.1,8.8.8.8", "curl --data-binary @ips.txt " + s.origin + "/json"}
}

func (s *server) bulkPage() (*cachedPage, error) {
	c := &s.bulk
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pg != nil && c.org == s.origin {
		return c.pg, nil
	}
	b, err := json.Marshal(ldWebApp{Context: "https://schema.org", Type: "WebApplication", Name: "Bulk IP lookup",
		URL: s.canon(bulkPath), Description: bulkDesc, ApplicationCategory: "DeveloperApplication", OperatingSystem: "Any",
		IsAccessibleForFree: true, Offers: ldOffer{"Offer", "0", "USD"}})
	if err != nil {
		return nil, err
	}
	v := &bulkView{pageBase: s.bulkBase(bulkTitle), Cmds: s.bulkCmds()}
	v.Desc, v.Canon, v.LD = bulkDesc, s.canon(bulkPath), template.JS(b)
	body, err := render(s.tmpl, "bulk.html", v)
	if err != nil {
		return nil, err
	}
	c.org, c.pg = s.origin, newPage(body)
	return c.pg, nil
}

// serveBulk answers /bulk-ip-lookup.
func (s *server) serveBulk(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		pg, err := s.bulkPage()
		if err != nil {
			plain(w, http.StatusInternalServerError, "error\n")
			return
		}
		pageHeaders(w.Header(), s.pageCSP(false, false, true, false))
		servePage(w, r, pg, "text/html; charset=utf-8", edgeCache)
	case http.MethodPost:
		text, ok := readForm(w, r, "ips")
		if !ok {
			return
		}
		s.bulkResults(w, r, text, r.PostForm.Get("format") == "csv")
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		plain(w, http.StatusMethodNotAllowed, "method not allowed\n")
	}
}

// readForm reads the post body (at most bulkBody bytes) and returns the field.
// On failure it has answered.
func readForm(w http.ResponseWriter, r *http.Request, field string) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, bulkBody)
	if err := r.ParseForm(); err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			plain(w, http.StatusRequestEntityTooLarge, "request too large\n")
		} else {
			plain(w, http.StatusBadRequest, "bad request\n")
		}
		return "", false
	}
	return r.PostForm.Get(field), true
}

func counts(found, np, inv int, more bool) string {
	parts := []string{}
	if found > 0 {
		parts = append(parts, strconv.Itoa(found)+" found")
	}
	if np > 0 {
		parts = append(parts, strconv.Itoa(np)+" not public")
	}
	if inv > 0 {
		parts = append(parts, strconv.Itoa(inv)+" invalid")
	}
	if more {
		parts = append(parts, "only the first "+strconv.Itoa(bulkMax)+" looked up")
	}
	return strings.Join(parts, " · ")
}

// bulkResults answers with the results page (or the CSV file) for the text.
func (s *server) bulkResults(w http.ResponseWriter, r *http.Request, text string, asCSV bool) {
	addrs, np, inv, more := s.extractAddrs(text, bulkMax)
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if asCSV && len(addrs) > 0 {
		var b strings.Builder
		cw := csv.NewWriter(&b)
		cw.WriteAll(bulkRows(s.lookupAll(addrs)))
		h.Set("Content-Disposition", `attachment; filename="ip-geolocation.csv"`)
		writeBody(w, r, http.StatusOK, "text/csv; charset=utf-8", []byte(b.String()))
		return
	}
	v := &bulkView{pageBase: s.bulkBase("Bulk IP lookup · openipdata.org"), Cmds: s.bulkCmds()}
	v.NoIndex = true
	var lines []string
	for _, a := range addrs {
		lines = append(lines, a.String())
	}
	v.Text = strings.Join(lines, "\n")
	if len(addrs) == 0 {
		v.Summary = "No public addresses found"
		if c := counts(0, np, inv, false); c != "" {
			v.Summary += " · " + c
		}
	} else {
		v.Summary = counts(len(addrs), np, inv, more)
		res := s.lookupAll(addrs)
		v.TSV, v.Rows = tsv(bulkRows(res)), tableRows(res)
	}
	body, err := render(s.tmpl, "bulk.html", v)
	if err != nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	pageHeaders(h, s.pageCSP(false, false, true, false))
	writeBody(w, r, http.StatusOK, "text/html; charset=utf-8", body)
}

// servePostHome answers POST /, the lookup box: several addresses give the bulk
// results, anything else one 303 to the page GET /?q=<text> would reach.
func (s *server) servePostHome(w http.ResponseWriter, r *http.Request) {
	q, ok := readForm(w, r, "q")
	if !ok {
		return
	}
	// Redirect only what the old GET box could hold: short text with at most one
	// address-like token. Anything else is a pasted list and must not reach a
	// URL (the log, the history, a Location header too long for nginx).
	addrs, np, inv, _ := s.extractAddrs(q, bulkMax)
	if len(strings.TrimSpace(q)) > 100 || len(addrs)+np+inv >= 2 {
		s.bulkResults(w, r, q, false)
		return
	}
	tq := strings.TrimSpace(q)
	esc := "/?q=" + url.QueryEscape(tq)
	loc := esc
	pq := parseQuery("q=" + url.QueryEscape(tq))
	switch pq.kind {
	case "redirect":
		loc = "/"
		if pq.addr.IsValid() {
			loc = "/?" + pq.addr.String()
		}
	case "prefix":
		loc = prefixHref(pq.pfx)
	case "asn":
		if pq.asn != "" {
			loc = pagePath + pq.asn
		}
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Location", loc)
	w.WriteHeader(http.StatusSeeOther)
}
