package main

// The "Your connection" / "Lookup" panel: which address is looked up, what the
// two MMDBs know about it, and how that is shown. Nothing here logs an address.

import (
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/oschwald/maxminddb-golang/v2"
)

const (
	maxQueryLen = 100 // longest raw query that is looked at at all
	maxInputLen = 64  // longest text put back into the lookup input (runes)
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
	if len(v) != 1 {
		return netip.Addr{}, false
	}
	return parseAddr(v[0])
}

// query is what the raw query string of "/" asks for.
type query struct {
	kind  string     // "visitor" (the home page), "lookup" (the canonical address), "redirect" (to "/" or to the canonical URL) or "bad"
	addr  netip.Addr // lookup, and redirect to the canonical URL (invalid: redirect to "/")
	input string     // bad: the text to put back into the input, already cut
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
	if a, ok := parseAddr(text); ok {
		if kind == "lookup" && raw != a.String() {
			kind = "redirect" // another spelling of the address
		}
		return query{kind: kind, addr: a}
	}
	return query{kind: "bad", input: cut(text)}
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
	f.mu.Lock()
	old := f.r
	f.r = r
	if old != nil {
		old.Close() // no lookup is inside: they hold the read lock
	}
	f.mu.Unlock()
	f.sig, f.lastErr = sig, ""
	log.Printf("opened %s", f.path)
	return true
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
	return res.Prefix(), true
}

type geoRecord struct {
	CountryCode string `maxminddb:"country_code"`
	CountryName string `maxminddb:"country_name"`
	CityName    string `maxminddb:"city_name"`
}

type asnRecord struct {
	ASN  string `maxminddb:"asn"`
	Name string `maxminddb:"name"`
	Org  string `maxminddb:"org"`
}

type row struct {
	K, V, A string // label, value, accent text after the value
}

// panel is the "Your connection" / "Lookup" panel.
type panel struct {
	Title string
	Msg   string // when set, the panel shows only this (and Addr)
	Addr  string
	Big   string
	Sub   string
	None  bool // no location: Big is shown grey
	Rows  []row
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

// panelFor builds the panel for a public or non-public address.
func (s *server) panelFor(title string, a netip.Addr) panel {
	p := panel{Title: title}
	if !s.public(a) {
		p.Addr, p.Msg = a.String(), "Not a public address"
		return p
	}
	var g geoRecord
	s.geo.lookup(a, &g)
	country := g.CountryName
	if country == "" {
		country = g.CountryCode
	}
	switch {
	case g.CityName != "":
		p.Big, p.Sub = g.CityName, country
	case country != "":
		p.Big, p.Sub = country, "city unknown"
	default:
		p.Big, p.None = "Location unknown", true
	}

	var n asnRecord
	prefix, found := s.asn.lookup(a, &n)
	network, as := firstNonEmpty(n.Org, n.Name), ""
	if isDigits(n.ASN) {
		as = "AS" + n.ASN
	}
	switch {
	case network == "" && as == "":
		network = dash
	case network == "":
		network, as = as, ""
	}
	pfx := dash
	if found && prefix.IsValid() {
		pfx = prefix.String()
	}
	family := "IPv6"
	if a.Is4() {
		family = "IPv4"
	}
	p.Rows = []row{{"Network", network, as}, {family, a.String(), ""}, {"Prefix", pfx, ""}}
	return p
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
