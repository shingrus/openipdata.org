package main

// What the connection panel says about a network: its short name (handle),
// category and domain, from the loaded ASN data. Read per page view, one row
// by number with parameters only, under the read lock of asnFile like the
// network page (nothing is written to the client under it).

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// netSnap is what is known about the network of an address; it is also the
// network part of a saved report. Handle is only ever read from the ASN data.
type netSnap struct {
	ASN      uint32 `json:"asn,omitempty"`
	Handle   string `json:"handle,omitempty"`
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.asnf.read(func(db *sql.DB, d *asnData) {
		nw, found := d.find(num)
		if !found {
			return
		}
		ns.Handle = nw.handle
		var cat, jhandle sql.NullString
		// One row by number; the JSON is read only for two small values, and only when valid.
		err := db.QueryRowContext(ctx, `SELECT CASE WHEN json_valid(json) THEN json_extract(json, '$.metadata.category') END,
 CASE WHEN json_valid(json) THEN json_extract(json, '$.metadata.handle') END FROM asn WHERE asn = ?`, int64(num)).Scan(&cat, &jhandle)
		if err == nil {
			ns.Category = clip(cat.String, 40)
			if ns.Handle == "" {
				ns.Handle = jhandle.String
			}
		}
		ns.Handle = clip(ns.Handle, 120)
		var dom sql.NullString
		// A missing domain, or a file without that table, only means unknown.
		if err := db.QueryRowContext(ctx, `SELECT domain FROM asn_domain WHERE asn = ?`, int64(num)).Scan(&dom); err == nil {
			ns.Domain = clip(dom.String, 100)
		}
	})
	return ns
}

// categoryText is the category as shown: "isp" is "ISP", every other value
// has a capital first letter and spaces for underscores.
func categoryText(c string) string {
	words := strings.Fields(strings.ReplaceAll(c, "_", " "))
	for i, w := range words {
		switch l := strings.ToLower(w); {
		case l == "isp" || l == "dsl" || l == "cdn" || l == "ix" || l == "ixp" || l == "nsp":
			words[i] = strings.ToUpper(l) // short forms are written in capitals
		case i == 0:
			r, size := utf8.DecodeRuneInString(w)
			words[i] = string(unicode.ToUpper(r)) + w[size:]
		}
	}
	return strings.Join(words, " ")
}

// parts are the pieces of the Network row: the handle (a link to the network
// page), the category and the domain; a part that is unknown is left out. With
// none of them the MMDB text, else the AS number, else a dash.
func (ns netSnap) parts() []part {
	var l []part
	if ns.Handle != "" && ns.ASN != 0 {
		l = append(l, part{Text: ns.Handle, Href: asnHref(ns.ASN)})
	} else if ns.Handle != "" {
		l = append(l, part{Text: ns.Handle})
	}
	if c := categoryText(ns.Category); c != "" {
		l = append(l, part{Text: c})
	}
	if ns.Domain != "" {
		l = append(l, part{Text: ns.Domain})
	}
	if len(l) > 0 {
		return l
	}
	text := ns.Org
	if text == "" && ns.ASN != 0 {
		text = "AS" + strconv.FormatUint(uint64(ns.ASN), 10)
	}
	return []part{{Text: orElseDash(text)}}
}
