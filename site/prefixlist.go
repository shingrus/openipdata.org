package main

// The plain-text prefix lists /as<number>/prefixes.txt and
// /asn-<slug>/prefixes.txt, and the BreadcrumbList data of the indexable pages.
// The lists are cut from the in-memory prefix index with one pass over it and
// built per request (never kept); they are noindex, the HTML page ranks.

import (
	"encoding/json"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const prefixesTxt = "/prefixes.txt"

// prefixesHref is the list of a page path (/as15169 or /asn-<slug>).
func prefixesHref(path string) string { return path + prefixesTxt }

// asnSet is a set of AS numbers: one number, or a sorted list with a small
// bit filter in front of the binary search.
type asnSet struct {
	one  uint32
	list []uint32
	bits [1024]uint64
}

func newASNSet(nums []uint32) *asnSet {
	s := &asnSet{}
	if len(nums) == 1 {
		s.one = nums[0]
		return s
	}
	s.list = slices.Clone(nums)
	slices.Sort(s.list)
	for _, a := range s.list {
		s.bits[(a&0xFFFF)>>6] |= 1 << (a & 63)
	}
	return s
}

func (s *asnSet) has(a uint32) bool {
	if s.list == nil {
		return a == s.one
	}
	if s.bits[(a&0xFFFF)>>6]>>(a&63)&1 == 0 {
		return false
	}
	_, ok := slices.BinarySearch(s.list, a)
	return ok
}

// prefixList writes the prefixes of the networks in set, IPv4 then IPv6, in
// address order, each once, one per line. The index is sorted by address, so
// the same prefix of two networks is adjacent. sizeHint is the buffer size
// (the networks know their prefix counts); append grows it if it is too small.
func (x *pfxIndex) prefixList(set *asnSet, sizeHint int) []byte {
	buf := make([]byte, 0, sizeHint)
	var last4 pfx4
	first := true
	for i := range x.v4 {
		e := x.v4[i]
		if !set.has(e.asn) || (!first && e.addr == last4.addr && e.bits == last4.bits) {
			continue
		}
		first, last4 = false, e
		buf = strconv.AppendUint(buf, uint64(e.addr>>24), 10)
		buf = append(buf, '.')
		buf = strconv.AppendUint(buf, uint64(e.addr>>16&255), 10)
		buf = append(buf, '.')
		buf = strconv.AppendUint(buf, uint64(e.addr>>8&255), 10)
		buf = append(buf, '.')
		buf = strconv.AppendUint(buf, uint64(e.addr&255), 10)
		buf = append(buf, '/')
		buf = strconv.AppendUint(buf, uint64(e.bits), 10)
		buf = append(buf, '\n')
	}
	var last6 pfx6
	first = true
	for i := range x.v6 {
		e := x.v6[i]
		if !set.has(e.asn) || (!first && e.hi == last6.hi && e.lo == last6.lo && e.bits == last6.bits) {
			continue
		}
		first, last6 = false, e
		buf = join6(e.hi, e.lo).AppendTo(buf)
		buf = append(buf, '/')
		buf = strconv.AppendUint(buf, uint64(e.bits), 10)
		buf = append(buf, '\n')
	}
	return buf
}

// servePrefixList answers GET/HEAD <path>/prefixes.txt when path is a network or
// organization page path. It reports whether the path was one of those (a
// path of another kind goes on to the other routes).
func (s *server) servePrefixList(w http.ResponseWriter, r *http.Request, p string) bool {
	head := strings.TrimSuffix(p, prefixesTxt)
	var slug string
	var num uint32
	isOrg := strings.HasPrefix(head, orgPathPrefix)
	switch {
	case isOrg:
		slug = head[len(orgPathPrefix):]
	case len(head) > len(pagePath) && strings.EqualFold(head[:len(pagePath)], pagePath):
		n, canon, ok := asNumber(head[len(pagePath):])
		if !ok {
			return false
		}
		if n == 0 || head != pagePath+canon {
			s.listNotFound(w, r)
			return true
		}
		num = n
	default:
		return false
	}
	d := s.asnf.data()
	if d == nil {
		sitemapNotLoaded(w, r)
		return true
	}
	var nums []uint32
	hint := 0
	if isOrg {
		if validSlug(slug) {
			for _, id := range d.org(slug) {
				nums = append(nums, d.nets[id].asn)
				hint += int(d.nets[id].n4)*19 + int(d.nets[id].n6)*44 // longest lines: 255.255.255.255/32, a full IPv6 address with /128
			}
		}
	} else if n, ok := d.find(num); ok {
		nums, hint = []uint32{num}, int(n.n4)*19+int(n.n6)*44
	}
	if len(nums) == 0 {
		s.listNotFound(w, r)
		return true
	}
	h := w.Header()
	h.Set("X-Robots-Tag", "noindex")
	servePage(w, r, newPage(d.idx.prefixList(newASNSet(nums), hint)), "text/plain; charset=utf-8", asnCache)
	return true
}

func (s *server) listNotFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", asnCache)
	plain(w, http.StatusNotFound, "not found\n")
}

// ldCrumb is one item of a BreadcrumbList.
type ldCrumb struct {
	Type     string `json:"@type"`
	Position int    `json:"position"`
	Name     string `json:"name"`
	Item     string `json:"item"`
}

type ldCrumbs struct {
	Context string    `json:"@context"`
	Type    string    `json:"@type"`
	Items   []ldCrumb `json:"itemListElement"`
}

// crumbs makes the BreadcrumbList block from (name, path) pairs; the paths
// become absolute URLs. encoding/json escapes < > &, so no "</script>" can end
// the block, and a data block needs no CSP hash.
func (s *server) crumbs(pairs ...string) template.JS {
	l := ldCrumbs{Context: "https://schema.org", Type: "BreadcrumbList"}
	for i := 0; i+1 < len(pairs); i += 2 {
		l.Items = append(l.Items, ldCrumb{"ListItem", i/2 + 1, pairs[i], s.canon(pairs[i+1])})
	}
	b, err := json.Marshal(l)
	if err != nil {
		return ""
	}
	return template.JS(b)
}
