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

// invalidTextRepresentation is Postgres' SQLSTATE for an invalid json value.
const invalidTextRepresentation = "22P02"

// ReadAnnounced calls visit for every current ASN with the IPv4 prefixes of its
// json, as text, in ASN order. A json value that starts like an object but is not
// valid JSON fails AnnouncedQuery (SQLSTATE 22P02); ASNPrefixes ignores such a
// value, so the read then goes through AnnouncedFallbackQuery and one damaged row
// does not stop the caller. visit may be called again from the start after the
// fallback: callers must reset what they collected in a visit that is repeated
// (reset is called first in that case).
func ReadAnnounced(ctx context.Context, conn *pgx.Conn, reset func(), visit func(asn int64, country *string, prefixes []string)) error {
	err := readAnnounced(ctx, conn, AnnouncedQuery, false, visit)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == invalidTextRepresentation {
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
