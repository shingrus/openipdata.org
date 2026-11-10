package geo

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/schema"
)

// pdbQuery picks the ASNs whose PeeringDB city is known (sync-peeringdb sets
// it only when it holds at least 30% of the ASN's presence) and whose
// PeeringDB country matches the whois country of public.asn. Read only.
//
// Global-scope networks are kept: leaving them out (2026-11-10) halved the
// holdout city score (6.48% to 3.41%), so their dominant city is mostly right.
//
// public.asn never loses rows, so an ASN that left the source is left out
// after 7 days without a build-asn-data run seeing it. A NULL seen_at counts
// only until the first run has stamped some row: after that, NULL means the
// ASN was already gone when the column was added.
const pdbQuery = `SELECT a.json, upper(btrim(g.country)), btrim(g.city)
	FROM public.asn a JOIN public.asn_geo_pdb g USING (asn)
	WHERE btrim(coalesce(g.city, '')) <> ''
	  AND upper(btrim(a.country)) = upper(btrim(g.country))
	  AND (a.seen_at > now() - interval '7 days'
	       OR (a.seen_at IS NULL AND NOT EXISTS (SELECT 1 FROM public.asn WHERE seen_at IS NOT NULL)))
	ORDER BY a.asn`

// writePDBRows writes every announced prefix of those ASNs as a layer row
// with the PeeringDB country and city.
func writePDBRows(ctx context.Context, dsn string, w *csv.Writer) (asns, prefixes int, err error) {
	if dsn == "" {
		return 0, 0, fmt.Errorf("PGSQL is not set")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, 0, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	defer conn.Close(ctx)
	// The query needs seen_at, which may be newer than the last build-asn-data run.
	if err := schema.Ensure(ctx, conn); err != nil {
		return 0, 0, err
	}
	rows, err := conn.Query(ctx, pdbQuery)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw, code, city string
		if err := rows.Scan(&raw, &code, &city); err != nil {
			return 0, 0, err
		}
		n := 0
		for _, p := range asnPrefixes(raw) {
			w.Write([]string{p.String(), code, city})
			n++
		}
		if n > 0 {
			asns++
			prefixes += n
		}
	}
	return asns, prefixes, rows.Err()
}

// asnPrefixes reads the announced prefixes from a public.asn json value;
// unparsable ones are left out.
func asnPrefixes(raw string) []netip.Prefix {
	var data struct {
		Prefixes struct {
			IPv4 []string `json:"ipv4"`
			IPv6 []string `json:"ipv6"`
		} `json:"prefixes"`
	}
	if json.Unmarshal([]byte(raw), &data) != nil {
		return nil
	}
	var out []netip.Prefix
	for _, text := range append(data.Prefixes.IPv4, data.Prefixes.IPv6...) {
		if p, err := netip.ParsePrefix(strings.TrimSpace(text)); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

// ixQuery lists the internet exchange peering LAN prefixes with a 2-letter
// country (sync-peeringdb already drops the rest) and a city. An exchange
// without a city stays in the table but not in the layer: as a later layer, a
// country-only row would erase a city that the lower layers set. Read only.
const ixQuery = `SELECT prefix, upper(btrim(country)), btrim(city)
	FROM public.geo_ix_pdb
	WHERE length(btrim(country)) = 2
	  AND btrim(coalesce(city, '')) <> ''
	ORDER BY prefix`

// ErrNoIXRows means public.geo_ix_pdb does not exist yet or has no rows, which
// is the state before sync-peeringdb has run once with the exchange prefixes.
var ErrNoIXRows = errors.New("public.geo_ix_pdb has no rows")

// writeIXRows writes every exchange prefix that has a city as a layer row and
// returns how many. It only reads: no DDL, as ALTER TABLE would take a lock
// that can block on sync-peeringdb's replace. It returns ErrNoIXRows when the
// table is missing or empty.
func writeIXRows(ctx context.Context, dsn string, w *csv.Writer) (int, error) {
	if dsn == "" {
		return 0, fmt.Errorf("PGSQL is not set")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	defer conn.Close(ctx)
	// to_regclass is NULL for a table that does not exist yet.
	var table *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.geo_ix_pdb')::text`).Scan(&table); err != nil {
		return 0, err
	}
	if table == nil {
		return 0, ErrNoIXRows
	}
	rows, err := conn.Query(ctx, ixQuery)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var prefix, code, city string
		if err := rows.Scan(&prefix, &code, &city); err != nil {
			return 0, err
		}
		w.Write([]string{prefix, code, city})
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if n > 0 {
		return n, nil
	}
	// Nothing written: an empty table is the rollout state, but a table
	// with rows that all lack a city or country is a data problem.
	var total int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM public.geo_ix_pdb`).Scan(&total); err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, ErrNoIXRows
	}
	return 0, nil
}
