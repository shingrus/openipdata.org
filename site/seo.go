package main

// SEO basics: the canonical URL and description of the indexable pages,
// robots.txt and the sitemaps. Nothing here reads visitor data.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	seoCache   = "public, max-age=3600"
	titleLimit = 60 // titles are cut by runes at a word boundary to about this
	nameInDesc = 60 // longest organization name inside a description
)

// sitemapChunk is the most URLs in one sitemap file (the protocol allows 50,000).
var sitemapChunk = 50000

// canon is the absolute canonical URL of a path of this site.
func (s *server) canon(path string) string { return s.origin + path }

// cutWords shortens s to at most max runes at a word boundary (white space
// folded, no ellipsis).
func cutWords(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if max < 1 || len(r) <= max {
		return s
	}
	if r[max] != ' ' { // the cut is inside a word: back to the last space
		for i := max - 1; i >= max/2; i-- {
			if r[i] == ' ' {
				max = i
				break
			}
		}
	}
	return strings.TrimRight(string(r[:max]), " ,.;:-/(")
}

// fitName cuts name so that head + name + tail stays within titleLimit runes.
func fitName(head, name, tail string) string {
	room := titleLimit - len([]rune(head)) - len([]rune(tail))
	if room < 8 {
		name = ""
	} else {
		name = cutWords(name, room)
	}
	if name == "" {
		return strings.TrimSpace(head) + tail
	}
	return head + name + tail
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmtInt(n) + " " + many
}

// networkMeta is the title and description of a network page.
func networkMeta(n uint32, name, country string, v4, v6 int64, counts bool) (title, desc string) {
	as := asnText(n)
	if name == "" {
		title = as + ": IP ranges and prefixes"
	} else {
		title = fitName(as+" ", name, ": IP ranges and prefixes")
	}
	for _, limit := range []int{nameInDesc, 40, 25} { // a long name and country shrink the name, never the facts
		desc = as
		if name != "" {
			desc += " " + cutWords(name, limit)
		}
		desc += " is a network"
		if country != "" {
			desc += " in " + country
		}
		if counts {
			desc += " announcing " + plural(v4, "IPv4 prefix", "IPv4 prefixes") + " and " + plural(v6, "IPv6 prefix", "IPv6 prefixes")
		}
		if desc += "."; len([]rune(desc)) <= 155 {
			break
		}
	}
	return title, desc
}

// orgMeta is the title and description of an organization page.
func orgMeta(name string, networks, v4 int64) (title, desc string) {
	title = fitName("", name, " IP ranges and networks")
	desc = cutWords(name, nameInDesc) + " operates " + plural(networks, "network", "networks")
	if v4 > 0 {
		desc += " with " + plural(v4, "IPv4 address", "IPv4 addresses")
	}
	return title, desc + "."
}

// prefixMeta is the title of a prefix page and, for one announced exactly,
// its description (as is the AS number 0 = none).
func prefixMeta(prefix string, asn uint32, name, place string) (title, desc string) {
	title = prefix
	if asn == 0 {
		return title, ""
	}
	head := prefix + " "
	tail := " " + asnText(asn)
	if name == "" {
		title = prefix + tail
	} else {
		title = fitName(head, name, tail)
	}
	desc = prefix + " is announced by " + asnText(asn)
	if name != "" {
		desc += " " + cutWords(name, nameInDesc)
	}
	if place != "" {
		desc += " and located in " + place
	}
	return title, desc + "."
}

// seoStatic are the bodies that depend only on the origin, built once for it.
type seoStatic struct {
	origin string
	robots []byte
	llms   []byte
}

const xmlHead = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"
const urlsetOpen = xmlHead + `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n"
const indexOpen = xmlHead + `<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n"

// xmlEscape escapes the origin once; slugs and numbers need no escaping.
func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func appendLoc(b []byte, tag, eo, path string) []byte {
	b = append(b, "<"+tag+"><loc>"...)
	b = append(b, eo...)
	b = append(b, path...)
	return append(b, "</loc></"+tag+">\n"...)
}

// static returns the bodies of the current origin (built again only when the
// origin changes, which in production is never).
func (s *server) static() *seoStatic {
	if p := s.seo.Load(); p != nil && p.origin == s.origin {
		return p
	}
	p := &seoStatic{origin: s.origin}
	p.robots = []byte("User-agent: *\nDisallow: /api/\nDisallow: /resolver\nDisallow: /ip\nDisallow: /json\nSitemap: " + s.canon("/sitemap.xml") + "\nSitemap: " + s.canon("/sitemap-index.xml") + "\n")
	p.llms = []byte(s.llmsText())
	s.seo.Store(p)
	return p
}

func newPage(body []byte) *cachedPage {
	sum := sha256.Sum256(body)
	return &cachedPage{body: body, etag: `"` + hex.EncodeToString(sum[:12]) + `"`}
}

// servePage answers a page held in memory with its ETag (a matching
// If-None-Match gives 304), like the ranking pages.
func servePage(w http.ResponseWriter, r *http.Request, pg *cachedPage, ctype, cache string) {
	h := w.Header()
	h.Set("Cache-Control", cache)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", pg.etag)
	if etagMatches(r.Header.Get("If-None-Match"), pg.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeBody(w, r, http.StatusOK, ctype, pg.body)
}

func (s *server) serveRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", seoCache)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeBody(w, r, http.StatusOK, "text/plain; charset=utf-8", s.static().robots)
}

// sitemapNotLoaded answers a network or organization file without ASN data.
func sitemapNotLoaded(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "60")
	plain(w, http.StatusServiceUnavailable, "ASN data is not loaded\n")
}

// smFile is one sitemap file of one data load, built on its first request.
type smFile struct {
	once sync.Once
	pg   *cachedPage
}

// sitemapSet belongs to one asnData (immutable, replaced on a reload, so a new
// load starts with nothing cached). Nothing is built at load time.
type sitemapSet struct {
	once       sync.Once
	nets       []uint32 // networks that announce something, by number
	netF, orgF []smFile
	idxOnce    sync.Once
	idx        *cachedPage
	ctyF       smFile // /sitemap-countries.xml: every country page, in the order of the Countries ranking
}

// countriesSitemap builds /sitemap-countries.xml on its first request.
func (s *server) countriesSitemap(d *asnData) *cachedPage {
	f := &d.sitemaps().ctyF
	f.once.Do(func() {
		eo := xmlEscape(s.origin)
		b := []byte(urlsetOpen)
		for _, c := range d.countries {
			if slug := d.cslug[c.cc]; slug != "" {
				b = appendLoc(b, "url", eo, countryHrefOf(slug))
			}
		}
		f.pg = newPage(append(b, "</urlset>\n"...))
	})
	return f.pg
}

func chunks(n int) int { return (n + sitemapChunk - 1) / sitemapChunk }

func (d *asnData) sitemaps() *sitemapSet {
	m := &d.sm
	m.once.Do(func() {
		for i := range d.nets {
			n := &d.nets[i]
			if n.v4 > 0 || n.v6 > 0 || n.n4 > 0 || n.n6 > 0 {
				m.nets = append(m.nets, n.asn)
			}
		}
		m.nets = slices.Clip(m.nets)
		m.netF = make([]smFile, chunks(len(m.nets)))
		m.orgF = make([]smFile, chunks(len(d.orgs)))
	})
	return m
}

// sitemapFile builds file k (1-based) of a kind, or nil when k is past the end.
func (s *server) sitemapFile(d *asnData, orgs bool, k int) *cachedPage {
	m := d.sitemaps()
	files, total := m.netF, len(m.nets)
	if orgs {
		files, total = m.orgF, len(d.orgs)
	}
	if k > len(files) {
		return nil
	}
	f := &files[k-1]
	f.once.Do(func() {
		eo := xmlEscape(s.origin)
		lo := (k - 1) * sitemapChunk
		hi := min(total, lo+sitemapChunk)
		b := make([]byte, 0, len(urlsetOpen)+(hi-lo)*(len(eo)+36)+16)
		b = append(b, urlsetOpen...)
		for i := lo; i < hi; i++ {
			b = append(b, "<url><loc>"...)
			b = append(b, eo...)
			if orgs {
				b = append(b, orgPathPrefix...)
				b = append(b, d.orgs[i].slug...)
			} else {
				b = append(b, pagePath...)
				b = strconv.AppendUint(b, uint64(m.nets[i]), 10)
			}
			b = append(b, "</loc></url>\n"...)
		}
		f.pg = newPage(append(b, "</urlset>\n"...))
	})
	return f.pg
}

func (s *server) sitemapIndex(d *asnData) *cachedPage {
	m := d.sitemaps()
	m.idxOnce.Do(func() {
		eo := xmlEscape(s.origin)
		b := []byte(indexOpen)
		for k := 1; k <= len(m.netF); k++ {
			b = appendLoc(b, "sitemap", eo, "/sitemap-networks-"+strconv.Itoa(k)+".xml")
		}
		for k := 1; k <= len(m.orgF); k++ {
			b = appendLoc(b, "sitemap", eo, "/sitemap-organizations-"+strconv.Itoa(k)+".xml")
		}
		b = appendLoc(b, "sitemap", eo, "/sitemap-countries.xml")
		m.idx = newPage(append(b, "</sitemapindex>\n"...))
	})
	return m.idx
}

const xmlType = "application/xml; charset=utf-8"

// smMainCache holds the one rendered /sitemap.xml, with the key of what it shows.
type smMainCache struct {
	mu  sync.Mutex
	key string
	pg  *cachedPage
}

// lastmod is a W3C datetime in UTC, or "" for an unknown time (no tag then).
func lastmod(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

func appendURL(b []byte, eo, path, mod string) []byte {
	b = append(b, "<url><loc>"...)
	b = append(b, eo...)
	b = append(b, path...)
	b = append(b, "</loc>"...)
	if mod != "" {
		b = append(b, "<lastmod>"...)
		b = append(b, mod...)
		b = append(b, "</lastmod>"...)
	}
	return append(b, "</url>\n"...)
}

// mainSitemap returns /sitemap.xml: the main pages with the build time of the
// data each one shows (no date when that file is not loaded; none for the
// static bulk page). It is rendered again only when a date or the origin
// changes, so a data reload is followed.
func (s *server) mainSitemap() *cachedPage {
	var home, dl, rank time.Time
	home = s.scores.Load().Built
	geo, asn := s.geo.status(), s.asn.status()
	if geo.Loaded {
		dl = geo.Built
	}
	if asn.Loaded {
		if asn.Built.After(dl) {
			dl = asn.Built
		}
		// The rankings are built from the SQLite data and answer 503 without it.
		if s.asnf.data() != nil {
			rank = asn.Built
		}
	}
	mods := [3]string{lastmod(home), lastmod(dl), lastmod(rank)}
	key := s.origin + "|" + mods[0] + "|" + mods[1] + "|" + mods[2]
	c := &s.smMain
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pg != nil && c.key == key {
		return c.pg
	}
	eo := xmlEscape(s.origin)
	b := []byte(urlsetOpen)
	b = appendURL(b, eo, "/", mods[0])
	b = appendURL(b, eo, comparePath, mods[0])
	b = appendURL(b, eo, downloadPath, mods[1])
	b = appendURL(b, eo, bulkPath, "")
	for _, rk := range rankings {
		b = appendURL(b, eo, rk.path, mods[2])
	}
	c.key, c.pg = key, newPage(append(b, "</urlset>\n"...))
	return c.pg
}

// serveSitemap answers /sitemap.xml, /sitemap-index.xml, /sitemap-pages.xml
// (a redirect), /sitemap-networks-<k>.xml and /sitemap-organizations-<k>.xml
// and reports whether the path was one. The files are built once per data
// load, on demand.
func (s *server) serveSitemap(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	switch p {
	case "/sitemap-pages.xml":
		w.Header().Set("Cache-Control", seoCache)
		http.Redirect(w, r, "/sitemap.xml", http.StatusMovedPermanently)
		return true
	case "/sitemap.xml":
		servePage(w, r, s.mainSitemap(), xmlType, seoCache)
		return true
	case "/sitemap-index.xml":
		d := s.asnf.data()
		if d == nil {
			sitemapNotLoaded(w, r) // an incomplete index is never cached
			return true
		}
		servePage(w, r, s.sitemapIndex(d), xmlType, seoCache)
		return true
	case "/sitemap-countries.xml":
		d := s.asnf.data()
		if d == nil {
			sitemapNotLoaded(w, r)
			return true
		}
		servePage(w, r, s.countriesSitemap(d), xmlType, seoCache)
		return true
	}
	var orgs bool
	rest, ok := strings.CutSuffix(p, ".xml")
	switch {
	case !ok:
		return false
	case strings.HasPrefix(rest, "/sitemap-networks-"):
		rest = rest[len("/sitemap-networks-"):]
	case strings.HasPrefix(rest, "/sitemap-organizations-"):
		orgs, rest = true, rest[len("/sitemap-organizations-"):]
	default:
		return false
	}
	k, err := strconv.Atoi(rest)
	if err != nil || k < 1 || strconv.Itoa(k) != rest {
		plain(w, http.StatusNotFound, "not found\n")
		return true
	}
	d := s.asnf.data()
	if d == nil {
		sitemapNotLoaded(w, r)
		return true
	}
	pg := s.sitemapFile(d, orgs, k)
	if pg == nil {
		plain(w, http.StatusNotFound, "not found\n")
		return true
	}
	servePage(w, r, pg, xmlType, seoCache)
	return true
}

const (
	homeTitle = "Free IP geolocation database (CC0) | openipdata.org"
	homeDesc  = "Free IP geolocation and ASN database in MMDB format. Public domain (CC0), no signup, no attribution. Look up any IP address, network or prefix."
)
