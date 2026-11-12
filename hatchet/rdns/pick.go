package rdns

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
)

const (
	// maxNetworksPerRun caps the /24s traced by one run.
	maxNetworksPerRun = 1000
	batchFile         = "batch.jsonl"
)

// batchEntry is one line of the batch file pick-networks writes for trace: a /24
// and its newest visitor IP, if it has one (a recheck of a located /24 may have none).
// The file is on the VM only (the IPs never go through Hatchet) and is rewritten by
// every run's pick-networks; one run of the workflow at a time holds the "rdns" limit,
// so it belongs to the run that reads it.
type batchEntry struct {
	Prefix  string `json:"prefix"`
	Visitor string `json:"visitor,omitempty"`
}

// PickOutput counts the /24s that wait for a trace and those picked for this run.
type PickOutput struct {
	Eligible int `json:"eligible"` // /24s that wait: never traced with a recent visitor, due, or brought back by a visitor
	Picked   int `json:"picked"`
	Limit    int `json:"limit"`    // most /24s one run picks: picked == limit means more wait
	Visitors int `json:"visitors"` // distinct recent visitor IPs in the picked /24s

	PickedNew     int `json:"picked_new"`     // of those picked: never traced
	PickedDue     int `json:"picked_due"`     // next_trace_at is over
	PickedRevived int `json:"picked_revived"` // no next_trace_at, a visitor came again after 60 days
	Backfilled    int `json:"backfilled"`     // traced_networks rows created from older traces (the first run only)
}

// The tiers of pick-networks, in the order they are picked: /24s that were never traced
// first (visitors are waiting for them), then the /24s that are due, then those that a
// visitor brought back.
const (
	tierNew = iota
	tierDue
	tierRevived
)

// The three queries share two parameters: $1 the limit and $2 the non-public blocks to
// leave out (an address in one of them is never traced). Each returns the /24, its newest
// visitor IP (NULL when it has none), the number of its visitor IPs seen in the last 30
// days and the number of /24s that qualify before the limit. Read only.
//
// newestCTE: per /24, its newest visitor IP (last_seen_at, then ip; of any age) and when
// that was, and how many visitor IPs were seen in the last 30 days.
const newestCTE = `WITH newest AS (
		SELECT network(set_masklen(ip, 24)) AS prefix,
		       host((array_agg(ip ORDER BY last_seen_at DESC, ip))[1]) AS visitor,
		       max(last_seen_at) AS seen_at,
		       count(*) FILTER (WHERE last_seen_at > now() - interval '` + visitorWindow + `') AS recent
		FROM public.visitor_ips
		WHERE family(ip) = 4 AND NOT (ip <<= ANY ($2::text[]::inet[]))
		GROUP BY 1
	)
	`

// pickNewQuery: /24s with a visitor seen in the last 30 days and no traced_networks row,
// most visitor IPs first. There is no "already known" check: tracing is cheap, and the
// geofeed layer still wins over the rDNS one.
const pickNewQuery = newestCTE + `SELECT prefix::text, visitor, recent, count(*) OVER ()
	FROM newest n
	WHERE recent > 0 AND NOT EXISTS (SELECT 1 FROM public.traced_networks t WHERE t.prefix = n.prefix)
	ORDER BY recent DESC, prefix
	LIMIT $1`

// pickDueQuery: /24s whose next_trace_at is over (rechecks of located ones included), the
// oldest due first, with their newest visitor IP when there is one.
const pickDueQuery = newestCTE + `SELECT t.prefix::text, n.visitor, coalesce(n.recent, 0), count(*) OVER ()
	FROM public.traced_networks t LEFT JOIN newest n ON n.prefix = t.prefix
	WHERE t.next_trace_at <= now()
	ORDER BY t.next_trace_at, t.prefix
	LIMIT $1`

// pickRevivedQuery: silent or unmatched /24s with no next_trace_at that a visitor came
// from after their last trace, at least 60 days after it; the oldest trace first.
const pickRevivedQuery = newestCTE + `SELECT t.prefix::text, n.visitor, n.recent, count(*) OVER ()
	FROM public.traced_networks t JOIN newest n ON n.prefix = t.prefix
	WHERE t.next_trace_at IS NULL AND t.status <> 'located'
	  AND t.last_traced_at <= now() - interval '` + reviveAfter + `'
	  AND n.seen_at > t.last_traced_at
	ORDER BY t.last_traced_at, t.prefix
	LIMIT $1`

// pickRow is a row of one of the pick queries.
type pickRow struct {
	Prefix   string
	Visitor  string // empty: no visitor IP
	Recent   int
	Eligible int // /24s the query found before its limit
	Tier     int
}

// mergePicks takes the rows of the three tiers (each already in its own order) and
// returns the batch: new /24s first, then due ones, then revived ones, at most limit,
// no /24 twice. eligible is the total of the tiers before the limit.
func mergePicks(limit int, tiers ...[]pickRow) (batch []pickRow, eligible int) {
	seen := map[string]bool{}
	for tier, rows := range tiers {
		for _, r := range rows {
			if r.Eligible > 0 {
				eligible += r.Eligible
				break
			}
		}
		for _, r := range rows {
			if len(batch) >= limit {
				break
			}
			if seen[r.Prefix] {
				continue
			}
			seen[r.Prefix] = true
			r.Tier = tier
			batch = append(batch, r)
		}
	}
	return batch, eligible
}

// readPicks runs one pick query.
func readPicks(ctx context.Context, conn *pgx.Conn, query string, limit int, blocks []string) ([]pickRow, error) {
	rows, err := conn.Query(ctx, query, limit, blocks)
	if err != nil {
		return nil, dbError("pick networks", err)
	}
	defer rows.Close()
	var out []pickRow
	for rows.Next() {
		var r pickRow
		var visitor *string
		if err := rows.Scan(&r.Prefix, &visitor, &r.Recent, &r.Eligible); err != nil {
			return nil, dbError("pick networks", err)
		}
		if visitor != nil {
			r.Visitor = *visitor
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError("pick networks", err)
	}
	return out, nil
}

// PickNetworks chooses the /24s for this run's trace task and writes them to
// the batch file. As the first task of a run it also creates and upgrades the
// tables (connectEnsure) and, once, fills the schedule from older traces
// (backfillSchedule); the later tasks assume they are there.
func (p *Pipeline) PickNetworks(ctx context.Context) (PickOutput, error) {
	var out PickOutput
	conn, err := connectEnsure(ctx, p.PGSQL)
	if err != nil {
		return out, err
	}
	defer conn.Close(ctx)
	if out.Backfilled, err = backfillSchedule(ctx, conn); err != nil {
		return out, err
	}

	blocks := make([]string, len(ipdata.NonPublicV4))
	for i, b := range ipdata.NonPublicV4 {
		blocks[i] = b.String()
	}
	var tiers [][]pickRow
	for _, q := range []string{pickNewQuery, pickDueQuery, pickRevivedQuery} {
		rows, err := readPicks(ctx, conn, q, maxNetworksPerRun, blocks)
		if err != nil {
			return out, err
		}
		tiers = append(tiers, rows)
	}
	picked, eligible := mergePicks(maxNetworksPerRun, tiers...)
	out.Eligible = eligible
	var batch []batchEntry
	for _, r := range picked {
		batch = append(batch, batchEntry{Prefix: r.Prefix, Visitor: r.Visitor})
		out.Visitors += r.Recent
		switch r.Tier {
		case tierNew:
			out.PickedNew++
		case tierDue:
			out.PickedDue++
		default:
			out.PickedRevived++
		}
	}
	out.Picked, out.Limit = len(batch), maxNetworksPerRun
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return out, err
	}
	if err := writeBatch(filepath.Join(p.dir(), batchFile), batch); err != nil {
		return out, err
	}
	p.log("pick-networks: %d /24s wait for a trace, picked %d (%d new, %d due, %d brought back by a visitor; limit %d per run), %d recent visitor IPs; %d schedule rows backfilled",
		out.Eligible, out.Picked, out.PickedNew, out.PickedDue, out.PickedRevived, out.Limit, out.Visitors, out.Backfilled)
	return out, nil
}

// writeBatch writes the batch file through a temporary file.
func writeBatch(path string, batch []batchEntry) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, e := range batch {
		if err := enc.Encode(e); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readBatch reads the batch file. A missing file is an empty batch. A line that
// is not a public /24, or whose visitor IP (optional) is not a public address inside it, is
// dropped and counted; the error text never shows the line.
func readBatch(path string) (entries []batchTarget, dropped int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var e batchEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			dropped++
			continue
		}
		prefix, perr := netip.ParsePrefix(e.Prefix)
		if perr != nil || prefix.Bits() != 24 || !prefix.Addr().Is4() || prefix != prefix.Masked() || !ipdata.PublicV4(prefix.Addr()) {
			dropped++
			continue
		}
		var visitor netip.Addr
		if e.Visitor != "" {
			var verr error
			if visitor, verr = netip.ParseAddr(e.Visitor); verr != nil || !prefix.Contains(visitor) || !ipdata.PublicV4(visitor) {
				dropped++
				continue
			}
		}
		entries = append(entries, batchTarget{Prefix: prefix, Visitor: visitor})
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("read batch file: %w", err)
	}
	return entries, dropped, nil
}

// batchTarget is a parsed batch file line; Visitor is invalid when the /24 has none.
type batchTarget struct {
	Prefix  netip.Prefix
	Visitor netip.Addr
}
