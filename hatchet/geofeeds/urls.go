package geofeeds

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/schema"
)

// urlRow is one row of public.geofeed_urls: a geofeed URL, what points to it,
// and what the last checks found. A zero time is NULL. sync-geofeeds is the
// table's only writer. Nothing is ever deleted: the table lists every URL found.
type urlRow struct {
	URL         string
	Sources     []string  // sorted, distinct; a source is never removed
	FirstSeen   time.Time // first run whose discovery listed the URL
	LastChecked time.Time // last check that ended with a result about the feed
	LastSuccess time.Time // last check that found a working feed
	Status      string    // last_fetch_status: one of the feed* statuses
	Result      string    // result_text, see resultText
}

// discoveredURL is a URL that discover found, with the sources that point to it.
type discoveredURL struct {
	URL     string
	Sources []string // sorted, distinct
}

// urlStore is the access to public.geofeed_urls.
type urlStore interface {
	// Load returns every row.
	Load(ctx context.Context) ([]urlRow, error)
	// Upsert adds new URLs, and for known ones sets last_seen_at to now and
	// merges the sources (sorted, distinct union with the stored ones).
	Upsert(ctx context.Context, found []discoveredURL, now time.Time) error
	// SaveChecks stores the check results (last_checked_at, last_success_at,
	// last_fetch_status, result_text) of the given rows, matched by URL.
	SaveChecks(ctx context.Context, rows []urlRow) error
}

// CheckPGSQL fails when PGSQL is missing, so a task can stop before it starts.
// discover, check-feeds, export-rows and probe-domains all need it.
func (p *Pipeline) CheckPGSQL() error {
	if p.PGSQL == "" && p.store == nil && p.domainRows == nil { // the test hooks stand in for Postgres
		return errors.New("PGSQL is not set")
	}
	return nil
}

func (p *Pipeline) openStore() (urlStore, error) {
	if p.store != nil {
		return p.store, nil
	}
	if p.PGSQL == "" {
		return nil, errors.New("PGSQL is not set")
	}
	return &pgStore{dsn: p.PGSQL}, nil
}

// Batch sizes: statements per round trip.
const (
	upsertBatch = 1000
	saveBatch   = 500
)

// pgStore is the Postgres implementation of urlStore. Each method opens its
// own connection: check-feeds runs for up to an hour between saves.
type pgStore struct{ dsn string }

func (s *pgStore) connect(ctx context.Context, ensure bool) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, s.dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	// Only this table's statements, never schema.Ensure: its ALTER TABLE
	// statements lock tables other workflows write.
	if ensure {
		if err := schema.EnsureGeofeedURLs(ctx, conn); err != nil {
			conn.Close(ctx)
			return nil, err
		}
	}
	return conn, nil
}

const loadQuery = `SELECT url, sources, first_seen_at, last_checked_at, last_success_at, last_fetch_status, result_text
	FROM public.geofeed_urls`

func (s *pgStore) Load(ctx context.Context) ([]urlRow, error) {
	conn, err := s.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, loadQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []urlRow
	for rows.Next() {
		var (
			r                urlRow
			checked, success *time.Time
			status, result   *string
		)
		if err := rows.Scan(&r.URL, &r.Sources, &r.FirstSeen, &checked, &success, &status, &result); err != nil {
			return nil, err
		}
		if checked != nil {
			r.LastChecked = *checked
		}
		if success != nil {
			r.LastSuccess = *success
		}
		if status != nil {
			r.Status = *status
		}
		if result != nil {
			r.Result = *result
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// upsertQuery: a new URL is inserted with its sources; a known one gets
// last_seen_at = now and the sorted distinct union of the old and new sources.
// The "C" collation makes the order the same as Go's.
const upsertQuery = `INSERT INTO public.geofeed_urls AS t (url, sources, first_seen_at, last_seen_at)
	VALUES ($1, $2, $3, $3)
	ON CONFLICT (url) DO UPDATE SET
		last_seen_at = EXCLUDED.last_seen_at,
		sources = ARRAY(SELECT s FROM unnest(t.sources || EXCLUDED.sources) AS s GROUP BY s ORDER BY s COLLATE "C")`

func (s *pgStore) Upsert(ctx context.Context, found []discoveredURL, now time.Time) error {
	conn, err := s.connect(ctx, true)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	for start := 0; start < len(found); start += upsertBatch {
		var batch pgx.Batch
		for _, f := range found[start:min(start+upsertBatch, len(found))] {
			batch.Queue(upsertQuery, f.URL, nonNil(f.Sources), now)
		}
		if err := runBatch(ctx, conn, &batch); err != nil {
			return err
		}
	}
	return nil
}

const saveQuery = `UPDATE public.geofeed_urls
	SET last_checked_at = $2, last_success_at = $3, last_fetch_status = $4, result_text = $5
	WHERE url = $1`

func (s *pgStore) SaveChecks(ctx context.Context, rows []urlRow) error {
	conn, err := s.connect(ctx, false)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	for start := 0; start < len(rows); start += saveBatch {
		var batch pgx.Batch
		for _, r := range rows[start:min(start+saveBatch, len(rows))] {
			batch.Queue(saveQuery, r.URL, nullTime(r.LastChecked), nullTime(r.LastSuccess), nullText(r.Status), nullText(r.Result))
		}
		if err := runBatch(ctx, conn, &batch); err != nil {
			return err
		}
	}
	return nil
}

func runBatch(ctx context.Context, conn *pgx.Conn, batch *pgx.Batch) error {
	results := conn.SendBatch(ctx, batch)
	for range batch.Len() {
		if _, err := results.Exec(); err != nil {
			results.Close()
			return err
		}
	}
	return results.Close()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// mergeSources is the sorted, distinct union of two source lists.
func mergeSources(old, add []string) []string {
	out := append(slices.Clone(old), add...)
	slices.Sort(out)
	return slices.Compact(out)
}

// resultText is the short text stored in result_text:
//
//	HTTP 200, 3412 rows        a working feed (HTTP 304 if the stored copy was still current)
//	HTTP 404                   an HTTP answer that is not a feed
//	HTTP 200, web page         an answer that is not a feed, with what was wrong
//	timeout / lookup ...       no HTTP answer: the error, cut short
//
// It holds no secrets: a feed URL is public, and the text never contains one.
func resultText(status string, httpStatus, rows int, detail string) string {
	var parts []string
	if httpStatus != 0 {
		parts = append(parts, "HTTP "+strconv.Itoa(httpStatus))
	}
	switch {
	case status == feedOK:
		parts = append(parts, strconv.Itoa(rows)+" rows")
	case detail != "":
		parts = append(parts, detail)
	}
	return strings.Join(parts, ", ")
}

// resultRows reads the row count back from a result_text that resultText wrote
// for a working feed. Anything else has none.
func resultRows(result string) int {
	i := strings.LastIndex(result, ", ")
	if i < 0 || !strings.HasSuffix(result, " rows") {
		return 0
	}
	n, _ := strconv.Atoi(result[i+2 : len(result)-len(" rows")])
	return n
}

// errDetail is an error as short text for result_text: the transport error
// without the URL it carries, on one line, cut at 200 characters.
func errDetail(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "..."
	}
	return msg
}
