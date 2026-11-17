package main

// The Network panel of the visitor's page and of a lookup page: the network
// (AS) of the address, as a grid of cells. The panel exists only when the ASN
// data is loaded and has the network. Country, prefix counts and addresses
// come from the in-memory record; type and role are read with one
// parameterised query per page view under the read lock of asnFile.

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
	Type                 string
	Country, CountryHref string
	Role                 string
	N4, N6               string
	Addr                 string
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
	}{{1e3, "K"}, {1e6, "M"}, {1e9, "B"}, {1e12, "T"}}
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
		nw        *network
		found     bool
		cname     string
		cat, role string
	)
	loaded := s.asnf.read(func(db *sql.DB, d *asnData) {
		if nw, found = d.find(num); !found || nw == nil {
			found = false
			return
		}
		cname = d.countryName(nw.cc)
		var c, r sql.NullString
		err := db.QueryRowContext(ctx, `SELECT CASE WHEN json_valid(json) THEN json_extract(json, '$.metadata.category') END,
 CASE WHEN json_valid(json) THEN json_extract(json, '$.metadata.networkRole') END FROM asn WHERE asn = ?`, int64(num)).Scan(&c, &r)
		if err == nil {
			cat, role = clip(c.String, 120), clip(r.String, 120)
		}
	})
	if !loaded || !found {
		return nil
	}
	p := &netPanel{
		ASN:  "AS" + strconv.FormatUint(uint64(num), 10),
		Href: asnHref(num),
		Type: orDash(words(cat)),
		Role: orDash(roleText(role)),
		N4:   dash,
		N6:   dash,
	}
	p.Country = dash
	if nw.cc != "" {
		p.Country = clip(cname, 120)
		p.CountryHref = s.countryHref(nw.cc)
	}
	if nw.hasPfxes {
		p.N4, p.N6 = fmtInt(int64(nw.n4)), fmtInt(int64(nw.n6))
	}
	p.Addr = fmtInt(nw.v4) // in full, like every IPv4 count of the site
	return p
}

// netAddr is the address the page is about: the looked-up one, or the visitor's.
func netAddr(q query, r *http.Request, header string) (netip.Addr, bool) {
	if q.kind == "lookup" {
		return q.addr, true
	}
	return clientAddr(r, header)
}
