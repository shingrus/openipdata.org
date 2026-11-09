package geo

import (
	"context"
	"encoding/csv"
	"encoding/json"
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
