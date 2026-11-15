package main

// What the connection panel says about a network: its short name (handle),
// the organisation it links to and the announced prefix, from the loaded ASN
// data (the in-memory records; no database read per page view).

import (
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"
)

// netSnap is what is known about the network of an address; it is also the
// network part of a saved report. Handle and Slug are only ever read from the
// ASN data. Category and Domain are no longer shown or filled; they stay so
// that older saved reports still read.
type netSnap struct {
	ASN      uint32 `json:"asn,omitempty"`
	Handle   string `json:"handle,omitempty"`
	Slug     string `json:"slug,omitempty"`   // the organisation's slug: the handle links to /asn-<slug>
	Prefix   string `json:"prefix,omitempty"` // the announced prefix of the address
	Category string `json:"category,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Org      string `json:"org,omitempty"` // the organisation (or name) text of the ip2asn MMDB
}

// clip cuts s to at most n runes.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// networkInfo reads what the panel shows about the network of an MMDB record.
// Without the ASN data (or without this network in it) only the MMDB text and
// the number are known.
func (s *server) networkInfo(n asnRecord) netSnap {
	text, digits := networkOf(n)
	ns := netSnap{Org: clip(text, 200)}
	num, _, ok := asNumber(digits)
	if !ok || num == 0 {
		return ns
	}
	ns.ASN = num
	if d := s.asnf.data(); d != nil {
		if nw, found := d.find(num); found {
			ns.Handle, ns.Slug = clip(nw.handle, 120), nw.slug
		}
	}
	return ns
}

// parts are the pieces of the Network row: the short name (handle) as a link
// to the organisation page (to the network page when the organisation has no
// page), then the announced prefix as a link to its page; a part that is
// unknown is left out. With no handle the MMDB text, else the AS number, stands
// in its place; with nothing at all a dash.
func (ns netSnap) parts() []part {
	var l []part
	switch {
	case ns.Handle != "" && validSlug(ns.Slug):
		l = append(l, part{Text: ns.Handle, Href: orgPathPrefix + ns.Slug})
	case ns.Handle != "" && ns.ASN != 0:
		l = append(l, part{Text: ns.Handle, Href: asnHref(ns.ASN)})
	case ns.Handle != "":
		l = append(l, part{Text: ns.Handle})
	case ns.Org != "":
		l = append(l, part{Text: ns.Org})
	case ns.ASN != 0:
		l = append(l, part{Text: "AS" + strconv.FormatUint(uint64(ns.ASN), 10)})
	}
	if p, err := netip.ParsePrefix(ns.Prefix); err == nil && p == p.Masked() {
		pp := part{Text: p.String()}
		if prefixOK(p) {
			pp.Href = prefixHref(p)
		}
		l = append(l, pp)
	}
	if len(l) == 0 {
		return []part{{Text: dash}}
	}
	return l
}
