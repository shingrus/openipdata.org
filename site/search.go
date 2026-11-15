package main

// The name search of the lookup box: a word is matched against the slugs of
// the organizations in memory (one pass, no SQL). Nothing here logs the term.

import (
	"net/http"
	"strings"
)

const maxResults = 50 // organizations on the results page

// slugify turns text into slug form: lower case ASCII letters and digits, every
// run of other characters becomes one "-", no "-" at either end, at most
// maxTerm characters. Non-ASCII letters count as "other".
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
			fallthrough
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteByte(c)
		default:
			dash = true
		}
	}
	out := b.String()
	if len(out) > maxTerm {
		out = out[:maxTerm]
	}
	return strings.TrimRight(out, "-")
}

// searchOrgs finds the organizations whose slug has term in it (plus the one
// of a network whose handle is the term) and returns the first maxResults, plus
// how many there are in all. Three groups: (1) the slug equals the term or
// starts with it, or the organization has a network whose handle is the term
// (compared in slug form); (2) the term starts a later word of the slug (after
// a "-"); (3) the slug only contains it. Inside a group the organization with
// more IPv4 addresses first, so a common word ("telecom") lists the big
// "Telecom ..." organizations first, not a small one that happens to own a
// network with that handle. d.orgs is already in that order, so one pass fills
// three buckets and nothing is sorted. An organization that matches twice (by
// slug and by handle) counts once.
func (d *asnData) searchOrgs(term string) (rows []orgRow, total int) {
	handle := int32(-1)
	if i, ok := d.byHandle[term]; ok {
		handle = i
	}
	word := "-" + term
	var group [3][]int32
	for i := range d.orgs {
		slug := d.orgs[i].slug
		at := strings.Index(slug, term)
		byHandle := int32(i) == handle
		if at < 0 && !byHandle {
			continue
		}
		total++
		g := 2
		switch {
		case byHandle, at == 0:
			g = 0
		case strings.Contains(slug, word):
			g = 1
		}
		if len(group[g]) < maxResults {
			group[g] = append(group[g], int32(i))
		}
	}
	for _, g := range group {
		for _, i := range g {
			if len(rows) >= maxResults {
				return rows, total
			}
			rows = append(rows, d.orgs[i])
		}
	}
	return rows, total
}

func (s *server) redirect(w http.ResponseWriter, loc string) {
	h := w.Header()
	h.Set("Cache-Control", asnCache)
	h.Set("Location", loc)
	w.WriteHeader(http.StatusFound)
}

// serveSearch answers a name: the one organization found (redirect), the
// canonical URL of the term, a results page, or "Nothing found". None holds
// visitor data.
func (s *server) serveSearch(w http.ResponseWriter, r *http.Request, q query) {
	d := s.asnf.data()
	if d == nil {
		s.notLoaded(w, r)
		return
	}
	rows, total := d.searchOrgs(q.term)
	switch {
	case total == 1:
		s.redirect(w, orgPathPrefix+rows[0].slug)
		return
	case !q.canon:
		s.redirect(w, "/?q="+q.term)
		return
	}
	v := &asnView{pageBase: s.asnBase(q.term+" · openipdata.org", true)}
	v.Input = q.term
	code := http.StatusOK
	if total == 0 {
		code = http.StatusNotFound
		v.NoticeHead, v.Notice, v.NoticeRight = "Search", "Nothing found", q.term
	} else {
		t := table{Title: "Search", H1: true, Right: q.term,
			Cols: []column{{"Organization", ""}, {"Networks", "n s"}, {"IPv4 addresses", "n"}}}
		for _, o := range rows {
			t.Rows = append(t.Rows, []cell{
				{Text: orElseDash(o.name), Href: orgPathPrefix + o.slug},
				{Text: fmtInt(int64(o.n)), Class: "n s"},
				{Text: fmtInt(o.v4), Class: "n"},
			})
		}
		if total > len(rows) {
			t.Note = fmtInt(int64(total-len(rows))) + " more: type more letters"
		}
		v.Tables = []table{t}
	}
	body, err := s.renderASN(v)
	if err != nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	pageHeaders(w.Header(), s.csp)
	w.Header().Set("Cache-Control", asnCache)
	writeBody(w, r, code, "text/html; charset=utf-8", body)
}
