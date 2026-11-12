package rdns

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// The schedule of the traced /24s (public.traced_networks, one row per /24; resolve-ips
// is its only writer). A /24 is in one of three states:
//
//   - silent: no hop answered in any of its newest traces. Traced again after 7 days,
//     at most 3 attempts in a row; after the 3rd there is no next_trace_at.
//   - unmatched: hops answered but no city was found. Traced again after 7, 20 and 60
//     days (attempts 1, 2, 3); after that there is no next_trace_at.
//   - located: a rule (or the MMDB vote) gave it a city. Rechecked 150 days after its
//     last trace, whatever the visitors do. A recheck that finds nothing keeps the old
//     city (locate carries the row over).
//
// A silent or unmatched /24 with no next_trace_at is traced again only when a visitor
// of the /24 was seen after its last trace and that trace is at least 60 days old; its
// attempts then start again. A /24 that has no row is traced when a visitor (seen in the
// last 30 days) comes from it.
const (
	netSilent    = "silent"
	netUnmatched = "unmatched"
	netLocated   = "located"

	day = 24 * time.Hour

	silentRetry       = 7 * day
	silentMaxAttempts = 3
	locatedRecheck    = 150 * day

	// reviveAfter, visitorWindow and traceExpire are SQL intervals (the queries are the
	// authority on them; see pick.go and cleanupSQL).
	reviveAfter   = "60 days"
	visitorWindow = "30 days"
	traceExpire   = "200 days"
)

// unmatchedBackoff is the wait after the 1st, 2nd and 3rd trace of an unmatched /24.
var unmatchedBackoff = [...]time.Duration{7 * day, 20 * day, 60 * day}

// tracedNet is one row of public.traced_networks.
type tracedNet struct {
	Prefix       string
	Status       string
	Attempts     int
	LastTracedAt time.Time
	NextTraceAt  *time.Time // nil: only when a visitor comes again
	LocatedAt    *time.Time // nil until the /24 was located
}

// nextTraceAt is when a /24 with this status is traced again, after its attempts-th
// trace at tracedAt; nil means only when a visitor comes again.
func nextTraceAt(status string, attempts int, tracedAt time.Time) *time.Time {
	var wait time.Duration
	switch status {
	case netLocated:
		wait = locatedRecheck
	case netSilent:
		if attempts < 1 || attempts >= silentMaxAttempts {
			return nil
		}
		wait = silentRetry
	case netUnmatched:
		if attempts < 1 || attempts > len(unmatchedBackoff) {
			return nil
		}
		wait = unmatchedBackoff[attempts-1]
	default:
		return nil
	}
	next := tracedAt.Add(wait)
	return &next
}

// afterTrace is the row of a /24 after a trace at time at in which answered says whether
// any hop of any of its new traces replied; prev is its row before (nil: never traced).
// Located stays located (the trace was a recheck: locate keeps the old city when it finds
// none, and replaces it when it finds one). Otherwise the status is silent or the
// provisional unmatched (locate makes it located when a rule matches); the attempts
// count the consecutive traces with the same status, and start again at 1 when the status
// changed or when the /24 had no next_trace_at (a visitor brought it back).
func afterTrace(prev *tracedNet, prefix string, answered bool, at time.Time) tracedNet {
	n := tracedNet{Prefix: prefix, LastTracedAt: at}
	if prev != nil && prev.Status == netLocated {
		n.Status, n.Attempts, n.LocatedAt = netLocated, 0, prev.LocatedAt
		n.NextTraceAt = nextTraceAt(netLocated, 0, at)
		return n
	}
	n.Status = netSilent
	if answered {
		n.Status = netUnmatched
	}
	n.Attempts = 1
	if prev != nil && prev.Status == n.Status && prev.NextTraceAt != nil {
		n.Attempts = prev.Attempts + 1
	}
	n.NextTraceAt = nextTraceAt(n.Status, n.Attempts, at)
	return n
}

// readScheduleSQL reads the rows of the /24s a trace task is about to store, locked
// until the transaction ends.
const readScheduleSQL = `SELECT prefix::text, status, attempts, last_traced_at, next_trace_at, located_at
	FROM public.traced_networks WHERE prefix = ANY ($1::text[]::cidr[]) FOR UPDATE`

// writeScheduleSQL upserts rows computed by afterTrace.
const writeScheduleSQL = `INSERT INTO public.traced_networks (prefix, status, attempts, last_traced_at, next_trace_at, located_at)
	SELECT u.prefix::cidr, u.status, u.attempts, u.last_traced_at, u.next_trace_at, u.located_at
	FROM unnest($1::text[], $2::text[], $3::integer[], $4::timestamptz[], $5::timestamptz[], $6::timestamptz[])
		AS u(prefix, status, attempts, last_traced_at, next_trace_at, located_at)
	ON CONFLICT (prefix) DO UPDATE SET status = excluded.status, attempts = excluded.attempts,
		last_traced_at = excluded.last_traced_at, next_trace_at = excluded.next_trace_at, located_at = excluded.located_at`

// updateSchedule stores the schedule of the prefixes of a trace batch (answered says for
// each whether any of its new traces got a reply), in the transaction that stores the
// traces. It returns how many /24s are silent after it.
func updateSchedule(ctx context.Context, tx pgx.Tx, answered map[string]bool, at time.Time) (silent int, err error) {
	prefixes := make([]string, 0, len(answered))
	for p := range answered {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	prev := map[string]*tracedNet{}
	rows, err := tx.Query(ctx, readScheduleSQL, prefixes)
	if err != nil {
		return 0, dbError("read schedule", err)
	}
	for rows.Next() {
		var n tracedNet
		if err := rows.Scan(&n.Prefix, &n.Status, &n.Attempts, &n.LastTracedAt, &n.NextTraceAt, &n.LocatedAt); err != nil {
			rows.Close()
			return 0, dbError("read schedule", err)
		}
		prev[n.Prefix] = &n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, dbError("read schedule", err)
	}

	var statuses []string
	var attempts []int32
	var lasts, nexts, locateds []*time.Time
	for _, p := range prefixes {
		n := afterTrace(prev[p], p, answered[p], at)
		if n.Status == netSilent {
			silent++
		}
		statuses, attempts = append(statuses, n.Status), append(attempts, int32(n.Attempts))
		last := n.LastTracedAt
		lasts, nexts, locateds = append(lasts, &last), append(nexts, n.NextTraceAt), append(locateds, n.LocatedAt)
	}
	if _, err := tx.Exec(ctx, writeScheduleSQL, prefixes, statuses, attempts, lasts, nexts, locateds); err != nil {
		return 0, dbError("store schedule", err)
	}
	return silent, nil
}

// backfillScheduleSQL creates the rows of /24s that were traced before the schedule
// existed: located when rdns_geo_layer has a traced row (its located_at kept), else
// unmatched when a hop answered in the newest traces (those within an hour of the newest one;
// a run stores all its traces of a /24 at one time) and silent when none did, with
// attempts 1; next_trace_at follows the rules from the newest trace. A located row of
// the layer with no trace left gets one too.
const backfillScheduleSQL = `INSERT INTO public.traced_networks (prefix, status, attempts, last_traced_at, next_trace_at, located_at)
	SELECT s.prefix,
		CASE WHEN g.prefix IS NOT NULL THEN 'located' WHEN s.answered THEN 'unmatched' ELSE 'silent' END,
		CASE WHEN g.prefix IS NOT NULL THEN 0 ELSE 1 END,
		s.last_traced_at,
		s.last_traced_at + CASE WHEN g.prefix IS NOT NULL THEN interval '150 days' ELSE interval '7 days' END,
		g.located_at
	FROM (
		SELECT prefix, last AS last_traced_at, bool_or(jsonb_path_exists(hops, '$[*] ? (@.ip != null)')) AS answered
		FROM (SELECT prefix, traced_at, hops, max(traced_at) OVER (PARTITION BY prefix) AS last FROM public.network_traces) n
		WHERE traced_at > last - interval '1 hour'
		GROUP BY prefix, last
	) s
	LEFT JOIN public.rdns_geo_layer g ON g.prefix = s.prefix AND g.scope = 'traced'
	ON CONFLICT (prefix) DO NOTHING`

const backfillLayerSQL = `INSERT INTO public.traced_networks (prefix, status, attempts, last_traced_at, next_trace_at, located_at)
	SELECT g.prefix, 'located', 0, g.located_at, g.located_at + interval '150 days', g.located_at
	FROM public.rdns_geo_layer g WHERE g.scope = 'traced'
	ON CONFLICT (prefix) DO NOTHING`

// backfillSchedule fills public.traced_networks from network_traces and rdns_geo_layer
// when the table is empty (the first run after it was added), so /24s traced before
// the schedule existed are not traced again as new ones. It returns the rows created.
// Later runs find the table filled (trace writes a row with every trace it stores), so
// this is a single cheap EXISTS check.
func backfillSchedule(ctx context.Context, conn *pgx.Conn) (int, error) {
	var filled bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.traced_networks)`).Scan(&filled); err != nil {
		return 0, dbError("backfill schedule", err)
	}
	if filled {
		return 0, nil
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, dbError("backfill schedule", err)
	}
	defer tx.Rollback(ctx)
	// One run of the workflow at a time holds the "rdns" limit; the lock is for a manual
	// run-rdns that overlaps one.
	if _, err := tx.Exec(ctx, `LOCK TABLE public.traced_networks IN EXCLUSIVE MODE`); err != nil {
		return 0, dbError("backfill schedule", err)
	}
	// Checked again under the lock: a run that overlapped may have filled it.
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.traced_networks)`).Scan(&filled); err != nil {
		return 0, dbError("backfill schedule", err)
	}
	if filled {
		return 0, nil
	}
	total := 0
	for _, sql := range []string{backfillScheduleSQL, backfillLayerSQL} {
		tag, err := tx.Exec(ctx, sql)
		if err != nil {
			return 0, dbError("backfill schedule", err)
		}
		total += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, dbError("backfill schedule", err)
	}
	return total, nil
}

// cleanupTracesSQL and cleanupNetworksSQL remove what no schedule or locate uses any
// more: traces older than 200 days (a located /24 never depends on them: locate keeps
// its row), and the rows of /24s that are not located, have no next_trace_at and were
// last traced more than 200 days ago.
const (
	cleanupTracesSQL   = `DELETE FROM public.network_traces WHERE traced_at < now() - interval '` + traceExpire + `'`
	cleanupNetworksSQL = `DELETE FROM public.traced_networks
		WHERE status <> 'located' AND next_trace_at IS NULL AND last_traced_at < now() - interval '` + traceExpire + `'`
)

// cleanupSchedule runs both cleanups and returns the rows removed from each.
func cleanupSchedule(ctx context.Context, conn *pgx.Conn) (traces, networks int, err error) {
	tag, err := conn.Exec(ctx, cleanupTracesSQL)
	if err != nil {
		return 0, 0, dbError("clean up traces", err)
	}
	traces = int(tag.RowsAffected())
	tag, err = conn.Exec(ctx, cleanupNetworksSQL)
	if err != nil {
		return traces, 0, dbError("clean up traced networks", err)
	}
	return traces, int(tag.RowsAffected()), nil
}

// keepNewestVisitorSQL deletes the visitor IPs of the given /24s ($1, as text) except the
// newest of each (last_seen_at, then ip): once a /24 is traced, one IP is enough to find it
// again. resolve-ips is the deleter of public.visitor_ips; the websites insert.
const keepNewestVisitorSQL = `DELETE FROM public.visitor_ips v USING (
		SELECT ip, row_number() OVER (PARTITION BY network(set_masklen(ip, 24)) ORDER BY last_seen_at DESC, ip) AS n
		FROM public.visitor_ips
		WHERE family(ip) = 4 AND network(set_masklen(ip, 24)) = ANY ($1::text[]::cidr[])
	) r
	WHERE v.ip = r.ip AND r.n > 1`

// deleteLocatedVisitorsSQL deletes every visitor IP of a located /24: it needs no visitor
// to be found again (rechecks trace .1 and a random host).
const deleteLocatedVisitorsSQL = `DELETE FROM public.visitor_ips
	WHERE family(ip) = 4 AND network(set_masklen(ip, 24)) IN (SELECT prefix FROM public.traced_networks WHERE status = 'located')`
