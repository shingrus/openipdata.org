package ipdata

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AnnouncedQuery reads the announced IPv4 prefixes (prefixes.ipv4 of the json) of
// every current ASN with its country, so the rest of each document is never sent.
// public.asn.json is TEXT: a value that is not a JSON object (the column defaults
// to ”) has no prefixes, as for ASNPrefixes; the CASE keeps the cast to jsonb away
// from it. It works on Postgres 15 (no pg_input_is_valid). Read only.
const AnnouncedQuery = `SELECT a.asn, a.country,
		ARRAY(SELECT t.e FROM jsonb_array_elements_text(l.list) AS t(e) WHERE t.e IS NOT NULL)
	FROM public.asn a
	CROSS JOIN LATERAL (SELECT CASE WHEN a.json LIKE '{%' THEN a.json::jsonb #> '{prefixes,ipv4}' END AS list) l
	WHERE jsonb_typeof(l.list) = 'array' AND ` + SeenAtFilter + ` ORDER BY a.asn`

// AnnouncedFallbackQuery reads every current ASN's whole json, for the case that
// AnnouncedQuery cannot cast one of them to jsonb. Read only.
const AnnouncedFallbackQuery = `SELECT a.asn, a.country, a.json FROM public.asn a WHERE ` + SeenAtFilter + ` ORDER BY a.asn`

// PrefixesJSON is a SQL expression for public.asn a (alias a): the json column reduced to
// its prefixes object, as text, so ASNPrefixes reads the result like the whole document and
// the rest of it (about 1.3 KB per ASN on average) is never sent. A document that is not a
// JSON object (the column defaults to an empty string), or is NULL, gives an empty string (no prefixes, as for
// ASNPrefixes). So does one with white space before the "{": build-asn-data writes none, and
// AnnouncedQuery has always had the same test; without prefixes the result is {"prefixes": null}, also no prefixes. A value
// that starts with { but that Postgres cannot cast to jsonb (see badJSON) fails the whole
// query: use WithJSONFallback. Postgres 15 works (no pg_input_is_valid).
const PrefixesJSON = `(CASE WHEN a.json LIKE '{%' THEN jsonb_build_object('prefixes', a.json::jsonb -> 'prefixes')::text ELSE '' END)`

// WithJSONFallback runs pass with the reduced query; when Postgres cannot cast a document to
// jsonb (badJSON) it runs pass again with the whole query, which sends a.json as it is: Go
// reads such a document, or ignores it. A pass must start from nothing and keep its results to
// itself until it returns nil, so a failed first pass leaves nothing behind.
func WithJSONFallback(reduced, whole string, pass func(query string) error) error {
	err := pass(reduced)
	if badJSON(err) {
		err = pass(whole)
	}
	return err
}

// badJSON reports whether err is Postgres refusing to cast a document to jsonb: a data
// exception (SQLSTATE class 22: 22P02 for invalid JSON, 22P05 for valid JSON with a \u0000
// escape, which jsonb cannot store, 22003 for a number out of range) or a limit that was
// passed (class 54, for example nesting too deep). Go reads or ignores such a document.
func badJSON(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) != 5 {
		return false
	}
	return pgErr.Code[:2] == "22" || pgErr.Code[:2] == "54"
}

// ReadAnnounced calls visit for every current ASN with the IPv4 prefixes of its
// json, as text, in ASN order. A json value that starts like an object but cannot be
// cast to jsonb (not valid JSON, or a \u0000 escape: badJSON) fails AnnouncedQuery; Go
// ignores or reads such a value, so the read then goes through AnnouncedFallbackQuery and
// one such row does not stop the caller. visit may be called again from the start after the
// fallback: callers must reset what they collected in a visit that is repeated
// (reset is called first in that case).
func ReadAnnounced(ctx context.Context, conn *pgx.Conn, reset func(), visit func(asn int64, country *string, prefixes []string)) error {
	err := readAnnounced(ctx, conn, AnnouncedQuery, false, visit)
	if badJSON(err) {
		if reset != nil {
			reset()
		}
		err = readAnnounced(ctx, conn, AnnouncedFallbackQuery, true, visit)
	}
	return err
}

func readAnnounced(ctx context.Context, conn *pgx.Conn, query string, whole bool, visit func(int64, *string, []string)) error {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var asn int64
		var country *string
		var texts []string
		if whole {
			var raw string
			if err := rows.Scan(&asn, &country, &raw); err != nil {
				return err
			}
			for _, p := range ASNPrefixes(raw) {
				if p.Addr().Is4() {
					texts = append(texts, p.String())
				}
			}
		} else if err := rows.Scan(&asn, &country, &texts); err != nil {
			return err
		}
		visit(asn, country, texts)
	}
	return rows.Err()
}
