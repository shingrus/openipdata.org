package main

import (
	"net/http"
	"strings"
)

// serveMoved answers permanent redirects for older paths that no longer
// have a page of their own. It reports whether the path was one; everything
// else stays a 404.
func (s *server) serveMoved(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	to := ""
	// One segment with a trailing slash: the same path without it. Only
	// letters, digits, "-" and "." in the segment, so the target can never be
	// read as another host (a second slash, or a backslash a browser reads as one).
	if len(p) > 2 && p[0] == '/' && p[len(p)-1] == '/' && plainSegment(p[1:len(p)-1]) {
		to = p[:len(p)-1]
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
	}
	if to == "" {
		return false
	}
	w.Header().Set("Cache-Control", asnCache)
	w.Header().Set("Location", to)
	w.WriteHeader(http.StatusMovedPermanently)
	return true
}

// plainSegment reports whether s has only a-z, A-Z, 0-9, "-" and ".".
func plainSegment(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// cleanOrgSlug turns a slug that matched nothing into the form the older
// paths used: lower case, only a-z, 0-9, "-" and space kept, spaces become
// "-", runs of "-" collapse, no "-" at the ends.
func cleanOrgSlug(s string) string {
	var b strings.Builder
	dash := true // true at the start: no leading "-"
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
			dash = false
		case c == '-' || c == ' ':
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}
