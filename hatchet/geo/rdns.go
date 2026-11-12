package geo

import (
	"context"
	"encoding/csv"
	"fmt"
)

// The scopes of public.rdns_geo_layer (written by resolve-ips), one per layer.
const (
	rdnsTraced = "traced" // 65-rdns: /24s whose traceroute passed a router with a city in its name
	rdnsWide   = "wide"   // 35-rdns-wide: announced prefixes (at most /16) that such /24s voted for
)

// rdnsQuery lists the rows of one scope. Read only.
const rdnsQuery = `SELECT prefix::text, upper(btrim(country)), btrim(city)
	FROM public.rdns_geo_layer
	WHERE scope = $1
	  AND length(btrim(country)) = 2
	  AND btrim(city) <> ''
	ORDER BY prefix`

// writeRDNSRows writes the rows of one scope of public.rdns_geo_layer as a
// layer and returns how many. It only reads: no DDL, resolve-ips owns the table.
// It returns ErrNoRows when the table does not exist yet or has no row of the
// scope, which skips the layer for this run.
func writeRDNSRows(ctx context.Context, dsn, scope string, w *csv.Writer) (int, error) {
	if scope != rdnsTraced && scope != rdnsWide {
		return 0, fmt.Errorf("unknown rDNS scope %q", scope)
	}
	conn, err := connect(ctx, dsn)
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)
	// to_regclass is NULL for a table that does not exist yet.
	var table *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.rdns_geo_layer')::text`).Scan(&table); err != nil {
		return 0, err
	}
	if table == nil {
		return 0, ErrNoRows
	}
	rows, err := conn.Query(ctx, rdnsQuery, scope)
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
		layerRow(w, prefix, code, city)
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrNoRows
	}
	return n, nil
}
