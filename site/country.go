package main

// The country pages: /<country-slug>-asn-list, the networks of one country
// ranked by IPv4 addresses. The slug is the country name the data has (see
// countrySlug). Nothing here shows visitor data.

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	countrySuffix = "-asn-list"
	maxCountryNet = 500 // networks on a country page
)

// foldLatin turns the Latin letters with marks that occur in country names into
// ASCII (upper case too).
var foldLatin = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ã", "a", "ä", "a", "å", "a", "Á", "a", "À", "a", "Â", "a", "Ã", "a", "Ä", "a", "Å", "a",
	"ç", "c", "č", "c", "ć", "c", "Ç", "c", "Č", "c", "Ć", "c",
	"é", "e", "è", "e", "ê", "e", "ë", "e", "É", "e", "È", "e", "Ê", "e", "Ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i", "ı", "i", "Í", "i", "Ì", "i", "Î", "i", "Ï", "i", "İ", "i",
	"ñ", "n", "Ñ", "n", "ł", "l", "Ł", "l", "đ", "d", "Đ", "d",
	"ó", "o", "ò", "o", "ô", "o", "õ", "o", "ö", "o", "ø", "o", "Ó", "o", "Ò", "o", "Ô", "o", "Õ", "o", "Ö", "o", "Ø", "o",
	"š", "s", "ş", "s", "Š", "s", "Ş", "s", "ğ", "g", "Ğ", "g",
	"ú", "u", "ù", "u", "û", "u", "ü", "u", "Ú", "u", "Ù", "u", "Û", "u", "Ü", "u",
	"ý", "y", "ÿ", "y", "Ý", "y", "ž", "z", "Ž", "z",
	"æ", "ae", "Æ", "ae", "œ", "oe", "Œ", "oe", "ß", "ss",
)

// countrySlug is a country name as a slug: lower case, marks folded to ASCII,
// every run of other characters one "-", no "-" at the ends. "" when nothing is
// left.
func countrySlug(name string) string { return slugify(foldLatin.Replace(name)) }

// indexCountries gives every country of d.countries (biggest first) its slug.
// A slug that is taken (two codes, one name) goes to the bigger country; the
// other gets "<slug>-<cc>". A name with no letters or digits uses the code.
func (d *asnData) indexCountries() {
	d.cslug = make(map[string]string, len(d.countries))
	d.cbySlug = make(map[string]string, len(d.countries))
	for _, c := range d.countries {
		cc := strings.ToLower(c.cc)
		base := countrySlug(c.name)
		if base == "" {
			base = cc
		}
		for _, slug := range []string{base, base + "-" + cc} {
			if _, taken := d.cbySlug[slug]; !taken {
				d.cslug[c.cc], d.cbySlug[slug] = slug, c.cc
				break
			}
		}
	}
}

func countryHrefOf(slug string) string { return "/" + slug + countrySuffix }

// countryHref is the page of a country code when the ASN data is loaded and has
// the country, else "".
func (s *server) countryHref(cc string) string {
	d := s.asnf.data()
	if d == nil {
		return ""
	}
	if slug := d.cslug[cc]; slug != "" {
		return countryHrefOf(slug)
	}
	return ""
}

// countryPath reports whether a path is /<letters, digits, "-">-asn-list (any
// case, ASCII only) and returns the part before the suffix, lower-cased.
func countryPath(p string) (string, bool) {
	if len(p) < len("/x"+countrySuffix) || p[0] != '/' || strings.ToLower(p[len(p)-len(countrySuffix):]) != countrySuffix {
		return "", false
	}
	mid := p[1 : len(p)-len(countrySuffix)]
	for i := 0; i < len(mid); i++ {
		c := mid[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return "", false
		}
	}
	return strings.ToLower(mid), true
}

// serveCountry answers /<slug>-asn-list and reports whether the path was one.
// The canonical path is lower case with the slug of the country name; another
// case, or a country code (/gb-asn-list), redirects to it. A decision that
// needs the data answers 503 without it.
func (s *server) serveCountry(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	mid, ok := countryPath(p)
	if !ok {
		return false
	}
	redirect := func(to string) {
		h := w.Header()
		h.Set("Cache-Control", asnCache)
		h.Set("Location", to)
		w.WriteHeader(http.StatusMovedPermanently)
	}
	low := "/" + mid + countrySuffix
	d := s.asnf.data()
	if d == nil {
		if p != low {
			redirect(low) // the case alone needs no data
		} else {
			s.notLoaded(w, r)
		}
		return true
	}
	cc, known := d.cbySlug[mid]
	if !known && len(mid) == 2 { // a country code in any case
		if slug := d.cslug[strings.ToUpper(mid)]; slug != "" {
			redirect(countryHrefOf(slug))
			return true
		}
	}
	switch {
	case !known:
		s.asnNotice(w, r, http.StatusNotFound, "Not found", "Country not found", asnCache)
	case p != low:
		redirect(low)
	default:
		pg := s.countryPage(d, cc)
		if pg == nil {
			plain(w, http.StatusInternalServerError, "error\n")
			return true
		}
		pageHeaders(w.Header(), s.csp)
		servePage(w, r, pg, "text/html; charset=utf-8", asnCache)
	}
	return true
}

// countryPage is the page of one country, rendered on its first request after
// a data load and kept with the data.
func (s *server) countryPage(d *asnData, cc string) *cachedPage {
	d.cpMu.Lock()
	defer d.cpMu.Unlock()
	if pg := d.cpg[cc]; pg != nil {
		return pg
	}
	var ids []int32
	for i := range d.nets {
		if n := &d.nets[i]; n.cc == cc && (n.v4 > 0 || n.v6 > 0) {
			ids = append(ids, int32(i))
		}
	}
	slices.SortFunc(ids, func(a, b int32) int {
		x, y := &d.nets[a], &d.nets[b]
		switch {
		case x.v4 != y.v4:
			if x.v4 > y.v4 {
				return -1
			}
			return 1
		case x.v6 != y.v6:
			if x.v6 > y.v6 {
				return -1
			}
			return 1
		}
		return int(x.asn) - int(y.asn)
	})
	name := d.countryName(cc)
	t := table{Title: name + " ASN list", H1: true, Right: plural(int64(len(ids)), "network", "networks"),
		Cols: []column{{"#", "r"}, {"AS", ""}, {"Organization", ""}, {"IPv4 addresses", "n"}, {"IPv6 /64s", "n s"}}}
	for i, id := range ids[:min(len(ids), maxCountryNet)] {
		n := &d.nets[id]
		t.Rows = append(t.Rows, []cell{
			{Text: strconv.Itoa(i + 1), Class: "r"},
			{Text: asnText(n.asn), Href: asnHref(n.asn), Class: "a"},
			{Text: orElseDash(nameOf(n))},
			{Text: fmtInt(n.v4), Class: "n"},
			{Text: fmtV6(n.v6), Class: "n s"},
		})
	}
	if len(ids) > maxCountryNet {
		t.Note = fmtInt(int64(len(ids)-maxCountryNet)) + " more networks"
	}
	title, desc := countryMeta(name, int64(len(ids)))
	v := &asnView{pageBase: s.asnBase(title, false), Tables: []table{t}}
	v.Desc, v.Canon = desc, s.canon(countryHrefOf(d.cslug[cc]))
	body, err := s.renderASN(v)
	if err != nil {
		return nil
	}
	if d.cpg == nil {
		d.cpg = map[string]*cachedPage{}
	}
	pg := newPage(body)
	d.cpg[cc] = pg
	return pg
}

// countryMeta is the title and description of a country page. A long name keeps
// the title shorter ("<Country> ASN list") rather than cut in the name.
func countryMeta(name string, n int64) (title, desc string) {
	title = name + " ASN list: networks by IPv4 addresses"
	if len([]rune(title)) > titleLimit {
		title = fitName("", name, " ASN list")
	}
	for _, limit := range []int{len([]rune(name)), 40, 25} { // never drop the facts, shrink the name
		head := "All " + fmtInt(n) + " networks (autonomous systems) registered in " + cutWords(name, limit) + ", ranked by announced IPv4 addresses"
		for _, tail := range []string{", with their organizations and IPv6 space.", "."} {
			if desc = head + tail; len([]rune(desc)) <= 155 {
				return title, desc
			}
		}
	}
	return title, desc
}
