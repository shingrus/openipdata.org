package main

// Shared reports (decided 2026-11-15, on demand). A report is made only when
// the visitor presses Share (POST /share): the server builds the snapshot of the
// visitor's own page itself and keeps it in Redis for 30 days. A page view
// stores nothing. This is the one place the site keeps a visitor address. Nothing
// here is ever logged: not an address, not a report id, not a beacon token (the
// Redis client logs fixed error kinds only).
//
//	report:<id>  JSON snapshot, expires after 30 days   (id: 32 hex characters)
//
// The other family's address reaches the snapshot through this server only:
// /ip?r=<id> (the request the page makes to the v4 or v6 name after a share).
// The DNS resolver is read from beacon:<token> (written by openipdata-beacon)
// when the share request names the page's token.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	reportTTL      = 30 * 24 * time.Hour
	maxShareBody   = 1024 // the biggest body of POST /share
	maxReportBytes = 4096 // the biggest snapshot written or read
	defaultOrigin  = "https://openipdata.org"
)

var hrefRE = regexp.MustCompile(`^/as(n-[a-z0-9-]{1,255}|[0-9]{1,10})$`)

type placeSnap struct {
	City    string   `json:"city,omitempty"`
	Country string   `json:"country,omitempty"`
	CC      string   `json:"cc,omitempty"`
	Lat     *float64 `json:"lat,omitempty"` // only for a placed city
	Lon     *float64 `json:"lon,omitempty"`
}

type resolverSnap struct {
	IP   string `json:"ip"`
	Name string `json:"name,omitempty"`
	Org  string `json:"org,omitempty"` // a page of this site
}

// snapshot is a saved visit: nothing from the request but the address.
type snapshot struct {
	Created  string        `json:"created"` // RFC 3339
	IP       string        `json:"ip"`
	Family   int           `json:"family"` // 4 or 6
	Place    placeSnap     `json:"place"`
	Network  netSnap       `json:"network"`
	Other    string        `json:"other_ip,omitempty"` // the address of the other family, when the page asked for it
	Resolver *resolverSnap `json:"resolver,omitempty"`
}

// marshalSnap is the JSON of a snapshot, without HTML escaping (it is kept in
// Redis and shown only through html/template).
func marshalSnap(sn snapshot) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(sn); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b.Bytes()), nil
}

// randHex is 32 hex characters from crypto/rand.
func randHex() (string, bool) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", false
	}
	return hex.EncodeToString(b[:]), true
}

// checkOrigin accepts an absolute https URL (http only for 127.0.0.1 and
// localhost) with no path, query or fragment, and returns it without a trailing slash.
func checkOrigin(v string) (string, error) {
	if err := checkURL(v); err != nil || v == "" {
		return "", errNotOrigin
	}
	u, _ := url.Parse(v)
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery {
		return "", errNotOrigin
	}
	return u.Scheme + "://" + u.Host, nil
}

var errNotOrigin = errString("not an origin (https://host)")

// shareOn reports whether reports are written and served.
func (s *server) shareOn() bool { return s.share && s.redis != nil }

// useShare sets the SHARE setting and the origin of the links.
func (s *server) useShare(on bool, origin string) {
	s.share = on
	if origin != "" {
		s.origin = origin
	}
}

// serveShare answers POST /share: it saves the snapshot of the visitor's own
// page (the address by the usual client rules, the place and the network as the
// page shows them, and the DNS resolver when the page's beacon token names one)
// and answers its link. Nothing from the request is stored. The custom header
// X-Share keeps another site from calling it (no CORS header is ever sent).
func (s *server) serveShare(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if !s.share {
		plain(w, http.StatusNotFound, "not found\n")
		return
	}
	if r.Method != http.MethodPost {
		h.Set("Allow", "POST")
		plain(w, http.StatusMethodNotAllowed, "method not allowed\n")
		return
	}
	reply := func(code int, v any) {
		b, _ := json.Marshal(v)
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(code)
		w.Write(b)
	}
	if r.Header.Get("X-Share") != "1" {
		reply(http.StatusForbidden, struct{}{})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxShareBody))
	if err != nil {
		reply(http.StatusRequestEntityTooLarge, struct{}{})
		return
	}
	a, ok := clientAddr(r, s.realIP)
	if !ok || !s.public(a) {
		reply(http.StatusBadRequest, struct{}{})
		return
	}
	if s.redis == nil {
		reply(http.StatusServiceUnavailable, struct{}{})
		return
	}
	// The page's beacon token, from the form body or the query; anything else is ignored.
	token := ""
	form, _ := url.ParseQuery(string(body))
	for _, v := range []url.Values{form, r.URL.Query()} {
		if t := v["t"]; len(t) == 1 && tokenRE.MatchString(t[0]) {
			token = t[0]
			break
		}
	}
	p := s.panelFor("", a)
	if p.snap == nil {
		reply(http.StatusBadRequest, struct{}{})
		return
	}
	sn := *p.snap
	if token != "" && s.zone != "" {
		if val, found, err := s.redis.hget("beacon:"+token, "resolver_ip"); err == nil && found {
			if ra, ok := parseAddr(val); ok && s.public(ra) {
				info := s.resolverNetwork(ra)
				sn.Resolver = &info
			}
		}
	}
	id, ok := randHex()
	if !ok {
		reply(http.StatusServiceUnavailable, struct{}{})
		return
	}
	b, err := marshalSnap(sn)
	if err != nil || len(b) > maxReportBytes || s.redis.setEX("report:"+id, string(b), reportTTL) != nil {
		reply(http.StatusServiceUnavailable, struct{}{})
		return
	}
	reply(http.StatusOK, struct {
		URL string `json:"url"`
		ID  string `json:"id"`
	}{s.origin + "/report/" + id, id})
}

// updateReport reads a report, lets change edit it, and writes it back with
// the remaining lifetime. A report that is gone (or was never there) is left
// alone. Updates of one report run one after another in this process.
func (s *server) updateReport(id string, change func(*snapshot) bool) {
	mu := &s.shareMu[id[0]&63]
	mu.Lock()
	defer mu.Unlock()
	val, found, err := s.redis.get("report:"+id, maxReportBytes)
	if err != nil || !found {
		return
	}
	var sn snapshot
	if json.Unmarshal([]byte(val), &sn) != nil || !change(&sn) {
		return
	}
	if b, err := marshalSnap(sn); err == nil && len(b) <= maxReportBytes {
		s.redis.replaceKeepTTL("report:"+id, string(b))
	}
}

// noteOther adds the address a /ip request saw to the report named by its r
// parameter, when it is a valid existing report, a is public and of the family
// the report does not have yet.
func (s *server) noteOther(r *http.Request, a netip.Addr) {
	if !s.shareOn() || !s.public(a) {
		return
	}
	v := r.URL.Query()["r"]
	if len(v) != 1 || !tokenRE.MatchString(v[0]) {
		return
	}
	fam := 6
	if a.Is4() {
		fam = 4
	}
	s.updateReport(v[0], func(sn *snapshot) bool {
		if sn.Family == fam || sn.Other != "" {
			return false
		}
		sn.Other = a.String()
		return true
	})
}

// reportPanel is the panel of a saved report: the stored values as they were
// at the time of the visit, nothing looked up again except the map.
func (s *server) reportPanel(sn *snapshot, created time.Time) panel {
	p := panel{Title: "Shared report", Right: "Shared " + created.UTC().Format("2 Jan 2006 15:04 UTC") + " · expires " + created.Add(reportTTL).UTC().Format("2 Jan 2006")}
	g := geoRecord{CityName: sn.Place.City, CountryName: sn.Place.Country, CountryCode: sn.Place.CC, Latitude: sn.Place.Lat, Longitude: sn.Place.Lon}
	p.Big, p.Sub, p.None, p.Map = s.placeBlock(&g)
	if href := s.countryHref(g.CountryCode); href != "" {
		if g.CityName != "" {
			p.SubHref = href
		} else {
			p.BigHref = href
		}
	}
	addr := func(k string, v string) row {
		if a, ok := parseAddr(v); ok && a.String() == v {
			p.Copy = true
			return row{K: k, V: v, Copy: true}
		}
		return row{K: k, V: dash}
	}
	v4, v6 := "", ""
	switch sn.Family {
	case 4:
		v4, v6 = sn.IP, sn.Other
	case 6:
		v4, v6 = sn.Other, sn.IP
	}
	p.Rows = []row{addr("IPv4", v4), addr("IPv6", v6)}
	dns := row{K: "DNS resolver", V: dash}
	if sn.Resolver != nil {
		if a, ok := parseAddr(sn.Resolver.IP); ok && a.String() == sn.Resolver.IP {
			dns = row{K: "DNS resolver", V: a.String(), VHref: "/?" + a.String()}
			if n := sn.Resolver.Name; n != "" && len(n) <= 120 {
				dns.Parts = []part{{Text: n}}
				if hrefRE.MatchString(sn.Resolver.Org) {
					dns.Parts[0].Href = sn.Resolver.Org
				}
			}
		}
	}
	p.Rows = append(p.Rows, dns, row{K: "Network", Parts: sn.Network.parts()})
	return p
}

type reportView struct {
	pageBase
	Panel *panel
}

// serveReport answers GET /report/<id>: the saved report, 410 when there is
// none (unknown, malformed or expired id), 503 when Redis cannot be asked.
func (s *server) serveReport(w http.ResponseWriter, r *http.Request) {
	if !s.share {
		plain(w, http.StatusNotFound, "not found\n")
		return
	}
	const gone = "This shared report has expired"
	id := strings.TrimPrefix(r.URL.Path, "/report/")
	if !tokenRE.MatchString(id) {
		s.asnNotice(w, r, http.StatusGone, "Shared report", gone, "private, no-store")
		return
	}
	if s.redis == nil {
		s.asnNotice(w, r, http.StatusServiceUnavailable, "Unavailable", "Shared reports are not available right now", "no-store")
		return
	}
	val, found, err := s.redis.get("report:"+id, maxReportBytes)
	if err != nil {
		s.asnNotice(w, r, http.StatusServiceUnavailable, "Unavailable", "Shared reports are not available right now", "no-store")
		return
	}
	var sn snapshot
	created, terr := time.Parse(time.RFC3339, "")
	if found && json.Unmarshal([]byte(val), &sn) == nil {
		created, terr = time.Parse(time.RFC3339, sn.Created)
	}
	if !found || terr != nil {
		s.asnNotice(w, r, http.StatusGone, "Shared report", gone, "private, no-store")
		return
	}
	p := s.reportPanel(&sn, created)
	base := s.asnBase("Shared report · openipdata.org", true)
	base.CSS = template.CSS(styleCSS + liveCSS)
	base.Pnl, base.PnlJS = p.Copy, template.JS(panelJS)
	body, err := render(s.tmpl, "report.html", &reportView{pageBase: base, Panel: &p})
	if err != nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	pageHeaders(w.Header(), s.pageCSP(false, false, p.Copy, false))
	w.Header().Set("Cache-Control", "private, no-store")
	writeBody(w, r, http.StatusOK, "text/html; charset=utf-8", body)
}
