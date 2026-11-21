package main

// The prefix pages: /prefix/<cidr>. A prefix has a page when it is announced
// exactly, is covered by an announced prefix, or has announced prefixes inside
// (see pfxindex.go). The place comes from the ip2geo MMDB: the records inside
// the prefix, summed per place. Nothing here logs a prefix, and nothing from
// the request is echoed except the canonical prefix.

import (
	"database/sql"
	"math"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
)

const (
	maxPrefixText    = 64    // longest prefix text that is parsed at all
	maxNetworkRows   = 5     // networks and covering prefixes in the head
	maxInside        = 100   // more specific prefixes in the table
	maxOtherPrefixes = 100   // other prefixes of the network per family on a prefix page
	maxPlaceRows     = 20    // places in the table
	placeRecords     = 20000 // most MMDB records read for the places of one prefix
)

// parseCIDR reads a prefix. An IPv4-mapped IPv6 prefix (at least /96) comes
// back as IPv4. The result is masked: host bits are cleared.
func parseCIDR(s string) (netip.Prefix, bool) {
	if len(s) > maxPrefixText {
		return netip.Prefix{}, false
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || p.Addr().Zone() != "" {
		return netip.Prefix{}, false
	}
	if p.Addr().Is4In6() {
		if p.Bits() < 96 {
			return netip.Prefix{}, false
		}
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p.Masked(), true
}

// prefixOK reports whether a prefix is in the range that has pages: IPv4 /8 to
// /32, IPv6 /16 to /128.
func prefixOK(p netip.Prefix) bool {
	if p.Addr().Is4() {
		return p.Bits() >= 8
	}
	return p.Addr().Is6() && p.Bits() >= 16
}

// lastAddr is the last address of a masked prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// placeAmt is one place (country and city) and how many addresses of the
// prefix it holds. rec is the first record of that place.
type placeAmt struct {
	rec    geoRecord
	amount float64
}

// placeSum is what the MMDB knows about the places inside a prefix. Amounts
// are float64 (2^128 does not fit an integer).
type placeSum struct {
	places  []placeAmt // biggest first
	total   float64    // addresses in the prefix
	unknown float64    // addresses with no location (only when partial)
	partial bool       // some addresses have no location
}

// within sums the records of the MMDB inside p per place. ok is false when
// more than limit records lie inside (nothing is then said about the place) or
// a record cannot be read. A prefix that lies inside one larger record gets
// that record for all of it: the library reports the record's own, larger
// prefix, which is cut to p here. Records without a country, records that
// cannot be decoded and gaps count as unknown. With no file open nothing is
// known.
func (f *mmdbFile) within(p netip.Prefix, limit int) (sum placeSum, ok bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	bitLen := p.Addr().BitLen()
	sum.total = math.Ldexp(1, bitLen-p.Bits())
	if f.r == nil {
		sum.partial, sum.unknown = true, sum.total
		return sum, true
	}
	cursor, end := p.Addr(), lastAddr(p).Next()
	index := map[string]int{}
	known, n := 0.0, 0
	for res := range f.r.NetworksWithin(p) {
		if n++; n > limit || res.Err() != nil {
			return placeSum{}, false
		}
		eff := res.Prefix()
		if eff.Bits() <= p.Bits() {
			eff = p
		}
		if eff.Addr() != cursor {
			sum.partial = true // a gap before this record
		}
		cursor = lastAddr(eff).Next()
		var g geoRecord
		if !res.Found() || res.Decode(&g) != nil {
			sum.partial = true
			continue
		}
		g.norm()
		country := firstNonEmpty(g.CountryName, g.CountryCode)
		if country == "" {
			sum.partial = true
			continue
		}
		amt := math.Ldexp(1, bitLen-eff.Bits())
		known += amt
		key := country + "\x00" + g.CityName
		i, seen := index[key]
		if !seen {
			i = len(sum.places)
			index[key] = i
			sum.places = append(sum.places, placeAmt{rec: g})
		}
		sum.places[i].amount += amt
	}
	if cursor != end {
		sum.partial = true
	}
	if sum.partial {
		sum.unknown = max(sum.total-known, sum.total*1e-9) // never 0 for a real gap
	}
	slices.SortStableFunc(sum.places, func(a, b placeAmt) int {
		switch {
		case a.amount > b.amount:
			return -1
		case a.amount < b.amount:
			return 1
		}
		return 0
	})
	return sum, true
}

func countryText(g *geoRecord) string { return firstNonEmpty(g.CountryName, g.CountryCode) }

// fmtShare is a share of the prefix with one decimal.
func fmtShare(v, total float64) string {
	p := v / total * 100
	if p > 0 && p < 0.05 {
		return "<0.1%"
	}
	return strconv.FormatFloat(min(p, 100), 'f', 1, 64) + "%"
}

// locate fills in the place of the head and, for several places, the table. A
// prefix with too many records shows no place at all.
func (s *server) locate(p netip.Prefix, pl *place, v *asnView) {
	sum, ok := s.geo.within(p, s.placeLimit)
	if !ok {
		return
	}
	switch k := len(sum.places); {
	case k == 0:
		pl.Sub, pl.SubNone = "Location unknown", true
	case k == 1 && !sum.partial:
		g := &sum.places[0].rec
		pl.Sub, pl.SubHref = countryText(g), s.countryHref(g.CountryCode) // the country links to its page, as on the lookup page
		if g.CityName != "" {
			pl.SubPre = g.CityName + ", "
		}
	default:
		if cc := sum.places[0].rec.CountryCode; cc != "" && !slices.ContainsFunc(sum.places, func(a placeAmt) bool { return a.rec.CountryCode != cc }) {
			if !sum.partial { // several cities of one country, nothing unknown: the country is the place
				pl.Sub, pl.SubHref = countryText(&sum.places[0].rec), s.countryHref(cc)
			}
		}
		t := table{Title: "Places", Right: fmtInt(int64(k)),
			Cols: []column{{"City", ""}, {"Country", ""}, {"Share", "n"}}}
		for _, a := range sum.places[:min(k, maxPlaceRows)] {
			t.Rows = append(t.Rows, []cell{
				{Text: orElseDash(a.rec.CityName)},
				{Text: countryText(&a.rec), Href: s.countryHref(a.rec.CountryCode)},
				{Text: fmtShare(a.amount, sum.total), Class: "n"},
			})
		}
		if sum.partial {
			t.Rows = append(t.Rows, []cell{{Text: "Unknown"}, {Text: dash}, {Text: fmtShare(sum.unknown, sum.total), Class: "n"}})
		}
		if k > maxPlaceRows {
			t.Note = fmtInt(int64(k-maxPlaceRows)) + " more places"
		}
		v.Tables = append(v.Tables, t)
	}
}

// orgOf is the Organization cell of a network by number (a dash when the
// network is not in the data).
func orgOf(d *asnData, asn uint32) cell {
	if n, ok := d.find(asn); ok {
		return orgCell(n)
	}
	return cell{Text: dash}
}

// asnItem is the head value of a network: its name and a link to its page.
func asnItem(d *asnData, asn uint32) kv {
	it := kv{A: asnText(asn), AHref: asnHref(asn)}
	if n, ok := d.find(asn); ok {
		it.V = nameOf(n)
	}
	return it
}

// addrKV is the Addresses row of a prefix. IPv4: the number of addresses. IPv6:
// the number of /64 networks, shortened (549.8B) with the full number on hover;
// a prefix longer than /64 has less than one, so its addresses are counted.
func addrKV(p netip.Prefix) kv {
	const k = "Addresses"
	bits := p.Bits()
	switch {
	case p.Addr().Is4():
		return kv{K: k, V: fmtInt(1 << (32 - bits))}
	case bits == 64:
		return kv{K: k, V: "1 /64 network"}
	case bits < 64:
		n := int64(1) << (64 - bits)
		it := kv{K: k, V: compactCount(n) + " /64 networks"}
		if full := fmtInt(n); full != compactCount(n) {
			it.Title = full + " /64 networks"
		}
		return it
	case bits == 65:
		return kv{K: k, V: "9,223,372,036,854,775,808"} // 2^63 does not fit int64
	}
	return kv{K: k, V: fmtInt(1 << (128 - bits))}
}

// prefixView builds the page of p, or nil when p is not announced, not covered
// and has nothing announced inside.
// domainOf gives the domain of a network ("" = unknown).
func (s *server) prefixView(d *asnData, p netip.Prefix, domainOf func(asn uint32) string) *asnView {
	exact := d.idx.exact(p)
	cover := d.idx.covering(p)
	inside, nInside := d.idx.inside(p, maxInside)
	if len(exact) == 0 && len(cover) == 0 && nInside == 0 {
		return nil
	}
	var asn0 uint32
	var name0 string
	pl := &place{Title: "Prefix", Big: p.String(), Mono: true, Ent: true}
	v := &asnView{Place: pl}
	if len(exact) > 0 {
		asn0 = exact[0]
		if n, ok := d.find(asn0); ok {
			name0 = nameOf(n)
		}
		// Organization: each organization once; Network: each announcing network.
		var orgs, nets []kv
		seen := map[string]bool{}
		for _, a := range exact[:min(len(exact), maxNetworkRows)] {
			it := kv{A: asnText(a), AHref: asnHref(a)}
			if n, ok := d.find(a); ok {
				if it.V = n.handle; it.V != "" {
					it.Href = asnHref(a)
				}
				key := firstNonEmpty(n.slug, n.org) // one organization can be spelled two ways; two can share a name
				if o := (kv{V: n.org}); n.org != "" && !seen[key] {
					seen[key] = true
					if validSlug(n.slug) {
						o.Href = orgPathPrefix + n.slug
					}
					orgs = append(orgs, o)
				}
			}
			nets = append(nets, it)
		}
		if row, ok := joinKV(orgs); ok {
			row.K = "Organization"
			pl.Rows = append(pl.Rows, row)
		}
		row, _ := joinKV(nets)
		row.K = "Network"
		pl.Rows = append(pl.Rows, row)
		// Domain: the domain of each of those networks, each once, when known.
		var doms []kv
		for _, a := range exact[:min(len(exact), maxNetworkRows)] {
			if domain := domainOf(a); domain != "" && !seen["."+domain] {
				seen["."+domain] = true // a slug has no dot, so the keys cannot meet
				doms = append(doms, domainKV(domain))
			}
		}
		if row, ok := joinKV(doms); ok {
			pl.Rows = append(pl.Rows, row)
		}
	}
	pl.Rows = append(pl.Rows, addrKV(p))
	var part []kv // the covering prefixes, most specific first, each once
	var last netip.Prefix
	for _, c := range cover {
		if c.Prefix == last {
			continue
		}
		if len(part) == maxNetworkRows {
			break
		}
		last = c.Prefix
		it := asnItem(d, c.ASN)
		it.V = c.Prefix.String()
		if prefixOK(c.Prefix) {
			it.Href = prefixHref(c.Prefix)
		}
		part = append(part, it)
	}
	if len(part) > 0 {
		part[0].K = "Part of"
		part[0].Extra = part[1:]
		pl.Rows = append(pl.Rows, part[0])
	}

	if nInside > 0 {
		t := table{Title: "More specific prefixes", Right: fmtInt(int64(nInside)),
			Cols: []column{{"Prefix", ""}, {"AS", ""}, {"Organization", ""}}}
		for _, r := range inside {
			pc := cell{Text: r.Prefix.String(), Class: "a"}
			if prefixOK(r.Prefix) {
				pc.Href = prefixHref(r.Prefix)
			}
			t.Rows = append(t.Rows, []cell{pc,
				{Text: asnText(r.ASN), Href: asnHref(r.ASN), Class: "a"},
				orgOf(d, r.ASN)})
		}
		if nInside > len(inside) {
			t.Note = fmtInt(int64(nInside-len(inside))) + " more prefixes"
		}
		v.Tables = append(v.Tables, t)
	}
	s.locate(p, pl, v)
	// Announced exactly: the other networks of the organization and the other
	// prefixes of the (first) network, as its own page lists them.
	if n, ok := d.find(asn0); ok && len(exact) > 0 {
		if t, ok := sameOrgTable(d, n.slug, asn0); ok {
			v.Tables = append(v.Tables, t)
		}
		v4, v6 := d.idx.firstOf(asn0, p, maxOtherPrefixes+1) // one more, to know whether the list is cut
		cut := len(v4) > maxOtherPrefixes || len(v6) > maxOtherPrefixes
		v4, v6 = v4[:min(len(v4), maxOtherPrefixes)], v6[:min(len(v6), maxOtherPrefixes)]
		if len(v4)+len(v6) > 0 {
			pp := &pfxPanel{Title: "Other prefixes of " + asnText(asn0), TxtHref: prefixesHref(asnHref(asn0))}
			for _, g := range []struct {
				fam  string
				list []string
			}{{"IPv4", v4}, {"IPv6", v6}} {
				if len(g.list) > 0 {
					pp.Groups = append(pp.Groups, pfxGroup{Family: g.fam, List: pfxItems(g.list)})
				}
			}
			if cut {
				pp.Note = "Showing the first " + strconv.Itoa(maxOtherPrefixes) + " per family."
				pp.NoteHref, pp.NoteLink = asnHref(asn0), "All prefixes of "+asnText(asn0)
			}
			v.Pfx = pp
		}
	}
	place := ""
	if !pl.SubNone {
		place = pl.SubPre + pl.Sub
	}
	title, desc := prefixMeta(p.String(), asn0, name0, place)
	v.pageBase = s.asnBase(title, len(exact) == 0)
	if len(exact) > 0 {
		v.Desc, v.Canon = desc, s.canon(prefixHref(p))
		v.LD = s.crumbs("Networks", rankings[0].path, asnText(asn0), asnHref(asn0), p.String(), prefixHref(p))
	}
	return v
}

// servePrefix answers /prefix/<cidr>.
func (s *server) servePrefix(w http.ResponseWriter, r *http.Request) {
	p, ok := parseCIDR(r.URL.Path[len(prefixPath):])
	if !ok || !prefixOK(p) {
		s.asnNotice(w, r, http.StatusNotFound, "Not found", "Prefix not found", asnCache)
		return
	}
	if canon := prefixHref(p); r.URL.Path != canon || r.URL.RawPath != "" {
		h := w.Header()
		h.Set("Cache-Control", asnCache)
		h.Set("Location", canon)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}
	if !s.public(p.Addr()) {
		s.asnNotice(w, r, http.StatusNotFound, "Not found", "Not a public prefix", asnCache)
		return
	}
	d := s.asnf.data()
	if d == nil {
		s.notLoaded(w, r)
		return
	}
	complete := s.geo.status().Loaded // no geo reader open: every place is unknown, not a complete page
	v := s.prefixView(d, p, func(asn uint32) (domain string) {
		s.asnf.read(func(db *sql.DB, _ *asnData) {
			var failed bool
			if domain, failed = asnDomainRead(r.Context(), db, asn); failed {
				complete = false
			}
		})
		return
	})
	if v == nil {
		s.asnNotice(w, r, http.StatusNotFound, "Not found", "Not announced", asnCache)
		return
	}
	body, err := s.renderASN(v)
	if err != nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	pageHeaders(w.Header(), s.csp)
	w.Header().Set("Cache-Control", pageCache(complete))
	writeBody(w, r, http.StatusOK, "text/html; charset=utf-8", body)
}

// pfxItems makes the list items of the prefixes of a network page: a link to
// the prefix page when the prefix is in its canonical spelling and in range.
func pfxItems(list []string) []pfxItem {
	out := make([]pfxItem, len(list))
	for i, s := range list {
		out[i].Text = s
		if p, err := netip.ParsePrefix(s); err == nil && prefixOK(p) && p == p.Masked() && p.String() == s {
			out[i].Href = prefixPath + s
		}
	}
	return out
}
