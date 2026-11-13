package rdns

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// backfillChunk is how many traces one backfill statement fills.
const backfillChunk = 2000

// backfillQuery reads the path of traces that have no candidates yet: the only
// place resolve-ips still reads hops, once for each row written before the column existed.
const backfillQuery = `SELECT prefix::text, target_kind, traced_at, host(target), hops::text
	FROM public.network_traces
	WHERE candidates IS NULL
	ORDER BY traced_at
	LIMIT $1`

// backfillSQL stores the candidates of the rows backfillQuery read, found by their key.
const backfillSQL = `UPDATE public.network_traces t SET candidates = string_to_array(u.cands, ',')::inet[]
	FROM unnest($1::text[], $2::text[], $3::timestamptz[], $4::text[]) AS u(prefix, kind, traced_at, cands)
	WHERE t.prefix = u.prefix::cidr AND t.target_kind = u.kind AND t.traced_at = u.traced_at AND t.candidates IS NULL`

// backfillCandidates fills network_traces.candidates of the rows that have none
// (written before the column was added), in chunks, and returns how many it filled.
// Every row read gets a value (a trace that cannot be read gets an empty list), so
// the loop ends; a chunk that updates nothing (its rows were pruned meanwhile) ends it too.
// trace stores the candidates itself, so after the first run there is nothing to do.
func backfillCandidates(ctx context.Context, conn *pgx.Conn) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		rows, err := conn.Query(ctx, backfillQuery, backfillChunk)
		if err != nil {
			return total, dbError("backfill candidates", err)
		}
		var prefixes, kinds, cands []string
		var tracedAt []time.Time
		for rows.Next() {
			var prefix, kind, target, hops string
			var at time.Time
			if err := rows.Scan(&prefix, &kind, &at, &target, &hops); err != nil {
				rows.Close()
				return total, dbError("backfill candidates", err)
			}
			prefixes, kinds, tracedAt = append(prefixes, prefix), append(kinds, kind), append(tracedAt, at)
			cands = append(cands, joinAddrs(candidatesOfStored(target, hops)))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, dbError("backfill candidates", err)
		}
		if len(prefixes) == 0 {
			return total, nil
		}
		tag, err := conn.Exec(ctx, backfillSQL, prefixes, kinds, tracedAt, cands)
		if err != nil {
			return total, dbError("backfill candidates", err)
		}
		if tag.RowsAffected() == 0 {
			return total, nil
		}
		total += int(tag.RowsAffected())
	}
}

// backfillHopIPsQuery reads the path of traces that have no hop_ips yet, once for each
// row written before the column existed (trace stores hop_ips itself afterwards).
const backfillHopIPsQuery = `SELECT prefix::text, target_kind, traced_at, host(target), hops::text
	FROM public.network_traces
	WHERE hop_ips IS NULL
	ORDER BY traced_at
	LIMIT $1`

// backfillHopIPsSQL stores the hop_ips of the rows backfillHopIPsQuery read, found by their key.
const backfillHopIPsSQL = `UPDATE public.network_traces t SET hop_ips = string_to_array(u.hips, ',')::inet[]
	FROM unnest($1::text[], $2::text[], $3::timestamptz[], $4::text[]) AS u(prefix, kind, traced_at, hips)
	WHERE t.prefix = u.prefix::cidr AND t.target_kind = u.kind AND t.traced_at = u.traced_at AND t.hop_ips IS NULL`

// backfillHopIPs fills network_traces.hop_ips of the rows that have none, in chunks, and
// returns how many it filled. Like backfillCandidates, every row read gets a value (an
// unreadable path gives an empty list), so the loop ends; resolve-names is its only caller.
func backfillHopIPs(ctx context.Context, conn *pgx.Conn) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		rows, err := conn.Query(ctx, backfillHopIPsQuery, backfillChunk)
		if err != nil {
			return total, dbError("backfill hop IPs", err)
		}
		var prefixes, kinds, hips []string
		var tracedAt []time.Time
		for rows.Next() {
			var prefix, kind, target, hops string
			var at time.Time
			if err := rows.Scan(&prefix, &kind, &at, &target, &hops); err != nil {
				rows.Close()
				return total, dbError("backfill hop IPs", err)
			}
			prefixes, kinds, tracedAt = append(prefixes, prefix), append(kinds, kind), append(tracedAt, at)
			hips = append(hips, joinAddrs(hopIPsOfStored(target, hops)))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, dbError("backfill hop IPs", err)
		}
		if len(prefixes) == 0 {
			return total, nil
		}
		tag, err := conn.Exec(ctx, backfillHopIPsSQL, prefixes, kinds, tracedAt, hips)
		if err != nil {
			return total, dbError("backfill hop IPs", err)
		}
		if tag.RowsAffected() == 0 {
			return total, nil
		}
		total += int(tag.RowsAffected())
	}
}
