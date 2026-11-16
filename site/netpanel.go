package main

// The Network panel of the visitor's page and of a lookup page: the network
// (AS) of the address, as a grid of cells. The panel exists only when the ASN
// data is loaded and has the network. Organisation, country, prefix counts and
// addresses come from the in-memory record; type, role and domain are read
// with one parameterised query per page view under the read lock of asnFile.

import (
	"context"
	"database/sql"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type netPanel struct {
	ASN, Href            string // "AS2856" and its page
	Org, OrgHref         string
	Type                 string
	Country, CountryHref string
	Role                 string
	N4, N6               string
	Addr, AddrTitle      string
	Domain               string
}

// words turns a metadata value such as education_research into "Education
// research"; isp is ISP. Empty stays empty.
func words(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if v == "isp" {
		return "ISP"
	}
	v = strings.ReplaceAll(v, "_", " ")
	r, n := utf8.DecodeRuneInString(v)
	return string(unicode.ToUpper(r)) + v[n:]
}

// roleText is the network role as shown: words, with Tier 1 spelled out.
func roleText(v string) string {
	if strings.TrimSpace(v) == "tier1_transit" {
		return "Tier 1 transit"
	}
	return words(v)
}

// compactCount prints n as 999, 1K, 1.2K, 12.4M, 1.6B (one decimal, a trailing
// .0 dropped, rounded half up).
func compactCount(n int64) string {
	if n < 1000 {
		if n < 0 {
			n = 0
		}
		return strconv.FormatInt(n, 10)
	}
	units := []struct {
		size   int64
		suffix string
	}{{1e3, "K"}, {1e6, "M"}, {1e9, "B"}}
	i := 0
	for i+1 < len(units) && n >= units[i+1].size {
		i++
	}
	tenths := (n*10 + units[i].size/2) / units[i].size
	if tenths >= 10000 && i+1 < len(units) { // 999,950 rounds to 1000.0K: that is 1M
		i++
		tenths = (n*10 + units[i].size/2) / units[i].size
	}
	s := strconv.FormatInt(tenths/10, 10)
	if t := tenths % 10; t != 0 {
		s += "." + strconv.FormatInt(t, 10)
	}
	return s + units[i].suffix
}

func orDash(s string) string {
	if s == "" {
		return dash
	}
	return s
}

// netPanelFor builds the Network panel of a public address, or nil when the ASN
// data is not loaded or does not have the network of the address.
func (s *server) netPanelFor(ctx context.Context, a netip.Addr) *netPanel {
	var rec asnRecord
	s.asn.lookup(a, &rec)
	_, asn := networkOf(rec)
	num, _, ok := asNumber(asn)
	if !ok || num == 0 {
		return nil
	}
	var (
		nw             *network
		found          bool
		cname          string
		cat, role, dom string
	)
	loaded := s.asnf.read(func(db *sql.DB, d *asnData) {
		if nw, found = d.find(num); !found || nw == nil {
			found = false
			return
		}
		cname = d.countryName(nw.cc)
		var c, r, dm sql.NullString
		err := db.QueryRowContext(ctx, `SELECT CASE WHEN json_valid(json) THEN json_extract(json, '$.metadata.category') END,
 CASE WHEN json_valid(json) THEN json_extract(json, '$.metadata.networkRole') END FROM asn WHERE asn = ?`, int64(num)).Scan(&c, &r)
		if err == nil {
			cat, role = clip(c.String, 120), clip(r.String, 120)
		}
		// A missing domain, or a file without that table, only means unknown.
		if err := db.QueryRowContext(ctx, `SELECT domain FROM asn_domain WHERE asn = ?`, int64(num)).Scan(&dm); err == nil {
			dom = clip(dm.String, 120)
		}
	})
	if !loaded || !found {
		return nil
	}
	p := &netPanel{
		ASN:    "AS" + strconv.FormatUint(uint64(num), 10),
		Href:   asnHref(num),
		Org:    orDash(clip(firstNonEmpty(nw.org, nw.handle), 120)),
		Type:   orDash(words(cat)),
		Role:   orDash(roleText(role)),
		N4:     dash,
		N6:     dash,
		Domain: orDash(dom),
	}
	if validSlug(nw.slug) && p.Org != dash {
		p.OrgHref = orgPathPrefix + nw.slug
	}
	p.Country = dash
	if nw.cc != "" {
		p.Country = clip(cname, 120)
		p.CountryHref = s.countryHref(nw.cc)
	}
	if nw.hasPfxes {
		p.N4, p.N6 = fmtInt(int64(nw.n4)), fmtInt(int64(nw.n6))
	}
	p.Addr, p.AddrTitle = compactCount(nw.v4), fmtInt(nw.v4)
	return p
}

// netAddr is the address the page is about: the looked-up one, or the visitor's.
func netAddr(q query, r *http.Request, header string) (netip.Addr, bool) {
	if q.kind == "lookup" {
		return q.addr, true
	}
	return clientAddr(r, header)
}
