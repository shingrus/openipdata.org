package main

// The Domain row of the network and prefix pages: the website of the network.
// The value comes from the worker's SQLite file, which anyone could have
// influenced through a registry record, so it is checked again here before it
// becomes a link: a value that is not a plain host name is shown as text.
// Nothing here is logged.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

const domainLinkQ = "/?utm_source=openipdata.org"

func isLowerAlnum(r rune) bool { return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' }

// plainDomain returns the lower-case host name when s is a plain one: dot
// separated labels of letters, digits and "-" (none starting or ending with
// "-"), at least two labels, a last label of two or more letters (or an
// "xn--" label), 253 characters at most. Otherwise "".
func plainDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 253 {
		return ""
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return ""
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return ""
		}
		for _, r := range l {
			if !isLowerAlnum(r) && r != '-' {
				return ""
			}
		}
	}
	last := labels[len(labels)-1]
	if !strings.HasPrefix(last, "xn--") {
		if len(last) < 2 {
			return ""
		}
		for _, r := range last {
			if r < 'a' || r > 'z' {
				return ""
			}
		}
	}
	return s
}

// domainKV is the value of a Domain row: a link to the site when the domain is
// a plain host name, else the text as it is (nothing at all stays a dash).
func domainKV(domain string) kv {
	d := plainDomain(domain)
	if d == "" {
		return kv{K: "Domain", V: orElseDash(strings.TrimSpace(domain))}
	}
	return kv{K: "Domain", V: d, Href: "https://" + d + domainLinkQ, Ext: true}
}

// joinKV makes one row of several values: the first is the row, the others its Extra.
func joinKV(rows []kv) (kv, bool) {
	if len(rows) == 0 {
		return kv{}, false
	}
	r := rows[0]
	r.Extra = rows[1:]
	return r, true
}

// asnDomain reads the domain of one network; a missing value, or a file
// without the table, is "".
func asnDomain(ctx context.Context, db *sql.DB, n uint32) string {
	dom, _ := asnDomainRead(ctx, db, n)
	return dom
}

// asnDomainRead is asnDomain that also says whether the read failed (any error
// but "no row"): the value is then "" and the page is incomplete.
func asnDomainRead(ctx context.Context, db *sql.DB, n uint32) (dom string, failed bool) {
	var v sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT domain FROM asn_domain WHERE asn = ?`, int64(n)).Scan(&v); err != nil {
		return "", !errors.Is(err, sql.ErrNoRows)
	}
	return strings.TrimSpace(v.String), false
}
