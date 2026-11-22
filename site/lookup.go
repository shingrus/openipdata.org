package main

// The "Your connection" / "Lookup" panel: which address is looked up, what the
// two MMDBs know about it, and how that is shown. Nothing here logs an address.

import (
	"html/template"
	"log"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

const (
	maxQueryLen = 100 // longest raw query that is looked at at all
	maxInputLen = 64  // longest text put back into the lookup input (runes)
	minTerm     = 2   // shortest name search term (after slugify)
	maxTerm     = 64  // longest name search term (after slugify)
	badMsg      = "Not an IP address, AS number or name"
)

// Ranges that are never public addresses, besides what netip already says
// (unspecified, loopback, link-local, multicast, broadcast). IPv6 is also
// required to be in 2000::/3, the only block handed out for global use.
var notPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"2001::/23", "2001:db8::/32", "3fff::/20",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var globalV6 = netip.MustParsePrefix("2000::/3")

// isPublic reports whether a is an address of the public internet. An
// IPv4-mapped IPv6 address counts as the IPv4 address inside it.
func isPublic(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.Zone() != "" || !a.IsGlobalUnicast() {
		return false
	}
	if a.Is6() && !globalV6.Contains(a) {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// parseAddr reads one IP address (no zone, no port, no spaces around it). An
// IPv4-mapped IPv6 address comes back as IPv4.
func parseAddr(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// clientAddr is the address of the visitor, or unknown (ok false). With a
// header configured: a loopback peer (our nginx) means the one value of that
// header, and a missing, repeated or invalid value means unknown, never the
// proxy's address; a private or link-local peer (most likely a proxy moved off
// loopback) means unknown; any other peer is the visitor itself. With no header
// the peer is the visitor.
func clientAddr(r *http.Request, header string) (netip.Addr, bool) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	ip := peer.Addr().Unmap()
	if header == "" {
		return ip, true
	}
	if !ip.IsLoopback() {
		// Not our nginx. A private or link-local peer is most likely a proxy
		// that was moved off loopback: unknown, never the proxy's address.
		if ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return netip.Addr{}, false
		}
		return ip, true
	}
	v := r.Header.Values(header)
	if len(v) == 0 && testAddr.IsValid() {
		return testAddr, true // local work only: a direct loopback request with no header (nginx always sets it)
	}
	if len(v) != 1 {
		return netip.Addr{}, false
	}
	return parseAddr(v[0])
}

// testAddr is TEST_IP: the visitor address of a request that comes straight
// from loopback without the real-IP header, for local work (a browser on the
// developer's machine). Never set on a server; behind nginx it changes nothing.
var testAddr netip.Addr

// query is what the raw query string of "/" asks for.
type query struct {
	kind  string       // "visitor" (the home page), "lookup" (the canonical address), "redirect" (to "/" or to the canonical URL), "asn" (the page of an AS number), "prefix" (the page of a CIDR prefix), "search" (a name) or "bad"
	addr  netip.Addr   // lookup, and redirect to the canonical URL (invalid: redirect to "/")
	pfx   netip.Prefix // prefix: the canonical (masked) prefix
	asn   string       // asn: the number without leading zeros; empty when it is 0 or out of range
	term  string       // search: the normalised term (see slugify)
	canon bool         // search: the raw query is exactly the canonical URL "q=<term>"
	input string       // bad: the text to put back into the input, already cut
}

// parseQuery reads the raw query. Empty = the visitor. Parameters without q =
// the visitor (the home page). q empty = a redirect to "/". q=<address> (the
// lookup form), or an address in any spelling other than the canonical one
// (netip text of the unmapped address; percent encoding, upper case, spaces,
// IPv4-mapped form), = a redirect to the canonical URL. Exactly the canonical
// address = a lookup. Anything else is bad.
func parseQuery(raw string) query {
	if raw == "" {
		return query{kind: "visitor"}
	}
	if len(raw) > maxQueryLen {
		return query{kind: "bad", input: cut(raw)}
	}
	text, kind := "", "lookup"
	if strings.Contains(raw, "=") {
		vals, err := url.ParseQuery(raw)
		if err != nil {
			return query{kind: "bad", input: cut(raw)}
		}
		if !vals.Has("q") {
			// Parameters that are not ours (e.g. a tracking tag added by
			// another site's link): the home page, not an error.
			return query{kind: "visitor"}
		}
		text, kind = vals.Get("q"), "redirect"
		if strings.TrimSpace(text) == "" {
			return query{kind: "redirect"} // the empty lookup box: back to the home page
		}
	} else if t, err := url.PathUnescape(raw); err == nil {
		text = t
	}
	// Order: an IP address, "AS<number>", a bare number, a CIDR prefix, then a name. Input
	// that looks like an attempt at an address never becomes a name search.
	if a, ok := parseAddr(text); ok {
		if kind == "lookup" && raw != a.String() {
			kind = "redirect" // another spelling of the address
		}
		return query{kind: kind, addr: a}
	}
	if n, ok := asnInput(text); ok {
		return query{kind: "asn", asn: n}
	}
	if strings.Contains(text, "/") { // a CIDR prefix, in any spelling: the canonical prefix page
		if p, ok := parseCIDR(strings.TrimSpace(text)); ok && prefixOK(p) {
			return query{kind: "prefix", pfx: p}
		}
	}
	if looksLikeIP(text) {
		return query{kind: "bad", input: cut(text)}
	}
	term := slugify(text)
	if len(term) < minTerm {
		return query{kind: "bad", input: cut(text)}
	}
	return query{kind: "search", term: term, canon: raw == "q="+term}
}

// looksLikeIP reports input that is an attempt at an address, valid or not:
// it has a colon, or it is only digits, dots and slashes with at least one dot
// ("1.2.3", "1.1.1.1/24"). Such input is "bad", never a name.
func looksLikeIP(s string) bool {
	if strings.Contains(s, ":") {
		return true
	}
	dot := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '.':
			dot = true
		case c >= '0' && c <= '9', c == '/':
		default:
			return false
		}
	}
	return dot
}

// asnInput reads "AS15169" or "as15169" (no space, any case) or a bare
// number "15169": the number is 1 to 4294967295, leading zeros are ignored. It
// returns the number without leading zeros, or "" with ok true for a number that
// is 0 or too large.
func asnInput(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) >= 3 && strings.EqualFold(s[:2], "as") {
		s = s[2:]
	}
	n, canon, ok := asNumber(s)
	if !ok {
		return "", false
	}
	if n == 0 {
		return "", true
	}
	return canon, true
}

// cut shortens s for the lookup input.
func cut(s string) string {
	if r := []rune(s); len(r) > maxInputLen {
		return string(r[:maxInputLen])
	}
	return s
}

// mmdbFile is an MMDB kept open for lookups. A changed file is opened again
// and swapped in under the write lock, so a reader is never closed while a
// lookup uses it.
type mmdbFile struct {
	path string

	mu sync.RWMutex
	r  *maxminddb.Reader // nil until a file opened
	st fileStatus        // about the open file; set with r

	// Only reload (the watch goroutine, or a test) touches these.
	sig     fileSig
	lastErr string
}

// reload opens the file when it changed since the last good open and reports
// whether a new reader is now in use. A missing or unreadable file keeps the
// reader in use (or none) and logs once per error.
func (f *mmdbFile) reload() bool {
	if f.path == "" {
		return false
	}
	info, err := os.Stat(f.path)
	if err != nil {
		f.fail(err)
		return false
	}
	sig := fileSig{info.ModTime(), info.Size()}
	if sig == f.sig {
		return false
	}
	r, err := maxminddb.Open(f.path)
	if err != nil {
		f.fail(err)
		return false
	}
	st := fileStatus{Loaded: true, LoadedAt: time.Now(), Size: info.Size()}
	if r.Metadata.BuildEpoch > 0 {
		st.Built = r.Metadata.BuildTime().UTC()
	}
	f.mu.Lock()
	old := f.r
	f.r, f.st = r, st
	if old != nil {
		old.Close() // no lookup is inside: they hold the read lock
	}
	f.mu.Unlock()
	f.sig, f.lastErr = sig, ""
	log.Printf("opened %s", f.path)
	return true
}

// status describes the open file (Loaded is false when none is open).
func (f *mmdbFile) status() fileStatus {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.st
}

// stamp returns the scores in the metadata of the open file, or empty scores
// when no file is open.
func (f *mmdbFile) stamp() scores {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.r == nil {
		return noScores()
	}
	return metaScores(f.r)
}

func (f *mmdbFile) fail(err error) {
	if msg := err.Error(); msg != f.lastErr {
		f.lastErr = msg
		log.Printf("mmdb %s: %v (keeping what is open, if anything)", f.path, err)
	}
}

// lookup decodes the record of a into v and returns the network that matched.
// ok is false when there is no reader, no record or the record cannot be read.
func (f *mmdbFile) lookup(a netip.Addr, v any) (prefix netip.Prefix, ok bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.r == nil {
		return netip.Prefix{}, false
	}
	res := f.r.Lookup(a)
	if res.Err() != nil || !res.Found() || res.Decode(v) != nil {
		return netip.Prefix{}, false
	}
	if n, ok := v.(interface{ norm() }); ok {
		n.norm()
	}
	return res.Prefix(), true
}

// geoRecord is a record of the ip2geo MMDB in either layout: the GeoLite2-City
// layout (nested fields, preferred) or the old flat one. norm() puts what was
// found into the flat fields, which the rest of the code reads.
type geoRecord struct {
	CountryCode string `maxminddb:"country_code"`
	CountryName string `maxminddb:"country_name"`
	CityName    string `maxminddb:"city_name"`
	// Where the city was placed (a double each); a record without a placed city
	// has neither, so they are optional: (0, 0) is a real place, absent is not.
	Latitude  *float64 `maxminddb:"latitude"`
	Longitude *float64 `maxminddb:"longitude"`
	// Same key in both layouts: where the city came from (a layer file or a guess).
	CitySource string `maxminddb:"city_source"`

	// The GeoLite2-City layout. The flat tags above are for the switch and go once
	// the server reads only the new files.
	Continent struct {
		Code  string `maxminddb:"code"`
		Names struct {
			EN string `maxminddb:"en"`
		} `maxminddb:"names"`
	} `maxminddb:"continent"`
	Country struct {
		ISO string `maxminddb:"iso_code"`
		// The builder writes this only as true (27 members), never false.
		EU    bool `maxminddb:"is_in_european_union"`
		Names struct {
			EN string `maxminddb:"en"`
		} `maxminddb:"names"`
	} `maxminddb:"country"`
	City struct {
		Names struct {
			EN string `maxminddb:"en"`
		} `maxminddb:"names"`
	} `maxminddb:"city"`
	Location struct {
		Latitude  *float64 `maxminddb:"latitude"`
		Longitude *float64 `maxminddb:"longitude"`
		TimeZone  string   `maxminddb:"time_zone"`
	} `maxminddb:"location"`

	// Set by norm(): the record had country.iso_code, so it is in the new layout
	// and a missing is_in_european_union means "not in the EU".
	newLayout bool
}

// norm makes the flat fields hold the value of the new layout when it has one.
func (g *geoRecord) norm() {
	g.newLayout = g.Country.ISO != ""
	if g.Country.ISO != "" {
		g.CountryCode = g.Country.ISO
	}
	if g.Country.Names.EN != "" {
		g.CountryName = g.Country.Names.EN
	}
	if g.City.Names.EN != "" {
		g.CityName = g.City.Names.EN
	}
	if g.Location.Latitude != nil && g.Location.Longitude != nil {
		g.Latitude, g.Longitude = g.Location.Latitude, g.Location.Longitude
	}
}

// coords returns the place of the city when the record has both numbers and
// they are on the globe.
func (g *geoRecord) coords() (lat, lon float64, ok bool) {
	if g.Latitude == nil || g.Longitude == nil {
		return 0, 0, false
	}
	lat, lon = *g.Latitude, *g.Longitude
	if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, false
	}
	return lat, lon, true
}

// asnRecord is a record of the ip2asn MMDB in either layout: the GeoLite2-ASN
// layout (number and organisation, preferred) or the old flat one.
type asnRecord struct {
	ASN  string `maxminddb:"asn"`
	Name string `maxminddb:"name"`
	Org  string `maxminddb:"org"`

	// The GeoLite2-ASN layout. The flat tags above are for the switch and go once
	// the server reads only the new files.
	Number uint32 `maxminddb:"autonomous_system_number"`
	Holder string `maxminddb:"autonomous_system_organization"`
}

func (n *asnRecord) norm() {
	if n.Number != 0 {
		n.ASN = strconv.FormatUint(uint64(n.Number), 10)
	}
	if n.Holder != "" {
		n.Org = n.Holder
	}
}

// part is one piece of a row value: text, or a link when Href is set.
// part is one piece of the Network row; Ext marks a link to another site.
type part struct {
	Text, Href string
	Ext        bool
	Sp         bool // joined to the part before it by a space, not by " · "
}

type row struct {
	K, V  string
	Small string // muted word after V (plain rows only, joined by a space)
	Parts []part // the value is these parts, joined by " · " (the Network row)
	VHref string // V is a link to this page
	Fam   string // "4" or "6": a row the page script fills in with the address of that family, found at From
	From  string
	Zone  string // the DNS row: the page script fills it in with the address and company of the visitor's DNS resolver, using names under this zone
	Copy  bool   // V is an address with a copy button (hidden until the page script runs)
	Own   bool   // the row of the address family that is looked up (addFill places the other family's row by it)
	Look  string // the visitor's own page: V links to its lookup page (the other family's row gets its link from fill.js)
}

// panel is the "Your connection" / "Lookup" panel.
type panel struct {
	Title string
	Msg   string // when set, the panel shows only this (and Addr)
	Addr  string
	Big   string
	Sub   string
	// BigHref or SubHref: the country name (Big when there is no city, else Sub) links to its country page.
	BigHref, SubHref string
	None             bool          // no location: Big is shown grey
	H2               bool          // the place name is an h2: the visitor's own page has its h1 in the header
	Map              template.HTML // the outline map behind the place (see worldmap.go), or empty
	Rows             []row
	Right            string      // right side of the head (the time of a shared report)
	Share            bool        // the visitor's own page has the (hidden) Share button, which makes a report on demand (POST /share)
	Copy             bool        // a row has a copy button: the page has panel.js
	Hdr              template.JS // the visitor's own page only: the request headers as JSON for the Headers button (never on a lookup or report page)

	Br *browserRow // the visitor's own page only: the Browser row (never on a lookup or report page)

	snap *snapshot // what a visit of the home page saves (see share.go); nil on other pages
}

func isDigits(s string) bool {
	if s == "" || len(s) > 10 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// placeMap is the map behind the place of a record: with a city that has
// coordinates, with the city at the point of the frame and the aim; with a country and no city, that
// country's outline in the accent colour, its label point at the point of the frame, no aim
// (a country too big for the frame is zoomed out to fit, centred);
// otherwise none (a city without coordinates has no map; neither does an
// unknown country code).
func (w *world) placeMap(g *geoRecord) template.HTML {
	if g.CityName != "" {
		lat, lon, ok := g.coords()
		if !ok {
			return ""
		}
		return w.cached(mapKey{lat: lat, lon: lon, city: true}, func() template.HTML {
			m, _ := w.drawMap(lat, lon, "", true)
			return m
		})
	}
	if codeSlot(g.CountryCode) < 0 {
		return "" // not a country code: nothing to draw, nothing to keep
	}
	return w.cached(mapKey{code: g.CountryCode}, func() template.HTML {
		m, _ := w.countryMap(g.CountryCode)
		return m
	})
}

// cached draws through the map cache (see mapcache.go); a world without one draws directly.
func (w *world) cached(k mapKey, draw func() template.HTML) template.HTML {
	if w == nil || w.cache == nil {
		return draw()
	}
	return w.cache.get(k, draw)
}

// placeBlock is what the place half of the panel shows for a record, and its
// map.
func (s *server) placeBlock(g *geoRecord) (big, sub string, none bool, m template.HTML) {
	country := g.CountryName
	if country == "" {
		country = g.CountryCode
	}
	switch {
	case g.CityName != "":
		return g.CityName, country, false, s.world.placeMap(g)
	case country != "":
		return country, "", false, s.world.placeMap(g) // no city: the country alone, no second line
	}
	return "Location unknown", "", true, ""
}

// addrData is what the two MMDBs and the loaded ASN data know about a public
// address: the geo record, the ip2asn record and the network as the panel shows
// it (with the announced prefix). The panel and the JSON API both read it here.
func (s *server) addrData(a netip.Addr) (g geoRecord, n asnRecord, info netSnap) {
	s.geo.lookup(a, &g)
	prefix, found := s.asn.lookup(a, &n)
	info = s.networkInfo(n)
	if found && prefix.IsValid() {
		info.Prefix = prefix.String()
	}
	// With the ASN data loaded the prefix is the longest announced one: the
	// MMDB tree may hold only a piece of it (a more specific prefix splits it).
	if d := s.asnf.data(); d != nil {
		if lp, _, ok := d.idx.longest(a); ok {
			info.Prefix = lp.String()
		}
	}
	return g, n, info
}

// panelFor builds the panel for a public or non-public address: the family
// row, the Network row and the Prefix row, in this order (the visitor's own
// page rearranges them, see visitorRows).
func (s *server) panelFor(title string, a netip.Addr) panel {
	p := panel{Title: title}
	if !s.public(a) {
		p.Addr, p.Msg = a.String(), "Not a public address"
		return p
	}
	g, _, info := s.addrData(a)
	p.Big, p.Sub, p.None, p.Map = s.placeBlock(&g)
	if href := s.countryHref(g.CountryCode); href != "" {
		if g.CityName != "" {
			p.SubHref = href
		} else {
			p.BigHref = href
		}
	}
	family, fam := "IPv6", 6
	if a.Is4() {
		family, fam = "IPv4", 4
	}
	// With the network in the ASN data the Network panel below has the handle, the
	// AS number, the prefix and the domain, so this panel keeps only the
	// organization; without it everything known is a row here.
	net := info.rows()
	if info.InData {
		net = info.orgRow()
	}
	p.Rows = append([]row{{K: family, V: a.String(), Own: true, Copy: true}}, net...)
	if title == "Lookup" {
		p.Rows = append(p.Rows, s.jsonRows(a)...)
	}
	p.Copy = true
	p.snap = &snapshot{Created: s.now().UTC().Format(time.RFC3339), IP: a.String(), Family: fam, Network: info}
	if g.CityName != "" || g.CountryCode != "" || g.CountryName != "" {
		p.snap.Place = placeSnap{City: clip(g.CityName, 120), Country: clip(g.CountryName, 120), CC: clip(g.CountryCode, 2)}
		if lat, lon, ok := g.coords(); ok && g.CityName != "" {
			p.snap.Place.Lat, p.snap.Place.Lon = &lat, &lon
		}
	}
	return p
}

// orgLink is the organisation page of the network of this record, from the
// loaded ASN data (never from the MMDB text), or "" with no data, no such
// network or no slug.
func (s *server) orgLink(n asnRecord) string {
	if !isDigits(n.ASN) {
		return ""
	}
	num, _, valid := asNumber(n.ASN)
	if !valid || num == 0 {
		return ""
	}
	d := s.asnf.data()
	if d == nil {
		return ""
	}
	if nw, found := d.find(num); found && nw.slug != "" {
		return orgPathPrefix + nw.slug
	}
	return ""
}

// addFill puts the row of the other address family next to the family row of
// p (the visitor's own panel): IPv4 first, then IPv6, a dash for the family
// this connection did not use. It reports whether a row was added, which means
// the page needs the fill script. With no URL for that family nothing changes.
func (s *server) addFill(p *panel, a netip.Addr) bool {
	if p.Msg != "" {
		return false
	}
	own := slices.IndexFunc(p.Rows, func(r row) bool { return r.Own })
	if own < 0 {
		return false
	}
	fam, from := "6", s.v6
	if !a.Is4() {
		fam, from = "4", s.v4
	}
	if from == "" {
		return false
	}
	if a.Is4() {
		own++ // IPv4 first, then IPv6: after the family row for an IPv4 address, before it for IPv6
	}
	p.Rows = slices.Insert(p.Rows, own, row{K: "IPv" + fam, V: dash, Fam: fam, From: from, Copy: true})
	return true
}

// addDNS puts the DNS row (right after the Network row) into the visitor's panel p: a dash that the page
// script fills in with the address and company of the visitor's DNS resolver. It reports
// whether a row was added, which means the page needs resolver.js. Only a
// panel of a public address (no message) with the feature on gets it.
func (s *server) addDNS(p *panel) bool {
	if s.zone == "" || p.Msg != "" {
		return false
	}
	// After the network rows (netSnap.rows): the last of Organization, Network, Prefix, Domain.
	i := 0
	for j, r := range p.Rows {
		if r.K == "Organization" || r.K == "Network" || r.K == "Prefix" || r.K == "Domain" {
			i = j + 1
		}
	}
	if i == 0 {
		i = len(p.Rows)
	}
	p.Rows = slices.Insert(p.Rows, i, row{K: "DNS resolver", V: dash, Zone: s.zone})
	return true
}

// networkOf is the name (organisation, else name) and the AS number (digits
// only) of a record of the ip2asn MMDB; each is empty when unknown.
func networkOf(n asnRecord) (name, asn string) {
	if isDigits(n.ASN) {
		asn = n.ASN
	}
	return firstNonEmpty(n.Org, n.Name), asn
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// jsonRows are the rows of a lookup page that come from the JSON API answer
// (ipJSONFor, so page and API cannot differ), each only when known.
func (s *server) jsonRows(a netip.Addr) []row {
	j := s.ipJSONFor(a)
	var l []row
	if j.ContinentName != "" {
		r := row{K: "Continent", V: j.ContinentName}
		if j.InEU != nil && *j.InEU {
			r.Small = "EU" // only a member says something; "EU: No" next to a US city said nothing
		}
		l = append(l, r)
	}
	if j.Latitude != nil && j.Longitude != nil {
		f := func(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }
		l = append(l, row{K: "Coordinates", V: f(*j.Latitude) + ", " + f(*j.Longitude)})
	}
	if j.TimeZone != "" {
		l = append(l, row{K: "Time zone", V: j.TimeZone})
	}
	return l
}
