package main

// What the connection panel says about a network: its short name (handle),
// the organisation it links to and the announced prefix, from the loaded ASN
// data (the in-memory records; no database read per page view).

import (
	"net/netip"
	"strings"
	"unicode/utf8"
)

// netSnap is what is known about the network of an address; it is also the
// network part of a saved report. Handle and Slug are only ever read from the
// ASN data. Category is no longer shown or filled; it stays so that older
// saved reports still read.
type netSnap struct {
	ASN      uint32 `json:"asn,omitempty"`
	Handle   string `json:"handle,omitempty"`
	Slug     string `json:"slug,omitempty"`   // the organisation's slug: the handle links to /asn-<slug>
	Prefix   string `json:"prefix,omitempty"` // the announced prefix of the address
	Category string `json:"category,omitempty"`
	Domain   string `json:"domain,omitempty"` // the network's domain (asn_domain), shown as the Domain row
	Org      string `json:"org,omitempty"`    // the organisation (or name) text of the ip2asn MMDB
	Name     string `json:"name,omitempty"`   // the organization as the ASN data has it: the Organization row
	InData   bool   `json:"-"`                // the network is in the loaded ASN data (not stored)
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
			ns.Handle, ns.Slug, ns.Name = clip(nw.handle, 120), nw.slug, clip(nw.org, 200)
			ns.InData = true
		}
	}
	return ns
}

// rows are the network rows of the connection panel, the same rows in the same
// order as the head of a prefix page: Organization (linked to its page when it
// has one; left out when unknown), Network (the handle and the AS number, both
// to the network page; a dash when nothing is known), Prefix (the announced
// prefix, to its page; left out when unknown) and Domain (the site; left out
// when unknown). Without the ASN data the MMDB text stands for the organization.
// orgRow is the Organization row alone (none when unknown).
func (ns netSnap) orgRow() []row {
	var l []row
	switch name := firstNonEmpty(ns.Name, ns.Org); {
	case ns.Name != "" && validSlug(ns.Slug):
		l = append(l, row{K: "Organization", Parts: []part{{Text: name, Href: orgPathPrefix + ns.Slug}}})
	case name != "":
		l = append(l, row{K: "Organization", Parts: []part{{Text: name}}})
	}
	return l
}

func (ns netSnap) rows() []row {
	l := ns.orgRow()
	nw := row{K: "Network"}
	if ns.ASN != 0 {
		as := part{Text: asnText(ns.ASN), Sp: true}
		if ns.Handle != "" { // the network is in the ASN data, so it has a page
			as.Href = asnHref(ns.ASN)
			nw.Parts = append(nw.Parts, part{Text: ns.Handle, Href: as.Href})
		}
		nw.Parts = append(nw.Parts, as)
	} else {
		nw.Parts = []part{{Text: dash}}
	}
	l = append(l, nw)
	if p, err := netip.ParsePrefix(ns.Prefix); err == nil && p == p.Masked() {
		pp := part{Text: p.String()}
		if prefixOK(p) {
			pp.Href = prefixHref(p)
		}
		l = append(l, row{K: "Prefix", Parts: []part{pp}})
	}
	if ns.Domain != "" {
		dk := domainKV(ns.Domain)
		l = append(l, row{K: "Domain", Parts: []part{{Text: dk.V, Href: dk.Href, Ext: dk.Ext}}})
	}
	return l
}
