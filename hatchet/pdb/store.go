package pdb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/schema"
)

// maxShrink is the largest drop in row count a replace accepts (10%).
const maxShrink = 0.10

// StoreStats compares the replaced table with the new rows.
type StoreStats struct {
	Previous   int `json:"previous"`
	Rows       int `json:"rows"`
	WithCity   int `json:"with_city"`
	WithDomain int `json:"with_domain"`

	IXPrevious int `json:"ix_previous"` // geo_ix_pdb rows before the replace
	IXRows     int `json:"ix_rows"`     // geo_ix_pdb rows now

	// IXKept is why geo_ix_pdb was not replaced (its shrink check failed) and
	// kept its old rows; empty when it was replaced. asn_geo_pdb is replaced anyway.
	IXKept string `json:"ix_kept,omitempty"`
}

// checkShrink refuses an empty result or one much smaller than the current table,
// which points at a broken download rather than PeeringDB losing networks.
func checkShrink(table string, previous, rows int) error {
	if rows == 0 {
		return fmt.Errorf("no %s rows computed", table)
	}
	if float64(rows) < float64(previous)*(1-maxShrink) {
		return fmt.Errorf("refusing to replace %s: %d rows computed, %d stored (more than %.0f%% fewer)",
			table, rows, previous, maxShrink*100)
	}
	return nil
}

// ixKeepReason is why the old geo_ix_pdb rows must stay (the new ones fail the
// shrink check), or "" when they can be replaced.
func ixKeepReason(previous, rows int) string {
	if err := checkShrink("geo_ix_pdb", previous, rows); err != nil {
		return err.Error()
	}
	return ""
}

// Replace swaps the contents of public.asn_geo_pdb for rows and of
// public.geo_ix_pdb for ixRows in one transaction, so readers see either the
// old or the new tables, never a mix.
//
// A failed asn_geo_pdb shrink check refuses everything. A failed geo_ix_pdb
// check (empty or more than 10% smaller) only keeps the old exchange rows,
// so a bad exchange download cannot stop asn_geo_pdb from updating; the
// reason is in StoreStats.IXKept.
func Replace(ctx context.Context, dsn string, rows []GeoRow, ixRows []IXRow) (StoreStats, error) {
	stats := StoreStats{Rows: len(rows), IXRows: len(ixRows)}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return stats, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	defer conn.Close(ctx)

	if err := schema.Ensure(ctx, conn); err != nil {
		return stats, err
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return stats, err
	}
	defer tx.Rollback(ctx)
	// Block other writers until commit; readers keep seeing the old rows.
	if _, err := tx.Exec(ctx, `LOCK TABLE public.asn_geo_pdb IN EXCLUSIVE MODE`); err != nil {
		return stats, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.asn_geo_pdb`).Scan(&stats.Previous); err != nil {
		return stats, err
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE public.geo_ix_pdb IN EXCLUSIVE MODE`); err != nil {
		return stats, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.geo_ix_pdb`).Scan(&stats.IXPrevious); err != nil {
		return stats, err
	}
	if err := checkShrink("asn_geo_pdb", stats.Previous, len(rows)); err != nil {
		return stats, err
	}
	if stats.IXKept = ixKeepReason(stats.IXPrevious, len(ixRows)); stats.IXKept != "" {
		stats.IXRows = stats.IXPrevious
		ixRows = nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.asn_geo_pdb`); err != nil {
		return stats, err
	}
	copyRows := make([][]any, len(rows))
	for i, r := range rows {
		copyRows[i] = []any{r.ASN, r.Country, r.City, r.Dominance, r.Domain, r.InfoScope, r.InfoType}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"public", "asn_geo_pdb"},
		[]string{"asn", "country", "city", "dominance", "domain", "info_scope", "info_type"}, pgx.CopyFromRows(copyRows)); err != nil {
		return stats, fmt.Errorf("copy rows: %w", err)
	}
	if stats.IXKept != "" {
		return stats, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.geo_ix_pdb`); err != nil {
		return stats, err
	}
	ixCopy := make([][]any, len(ixRows))
	for i, r := range ixRows {
		ixCopy[i] = []any{r.Prefix, r.IXID, r.Country, r.City}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"public", "geo_ix_pdb"},
		[]string{"prefix", "ix_id", "country", "city"}, pgx.CopyFromRows(ixCopy)); err != nil {
		return stats, fmt.Errorf("copy exchange prefixes: %w", err)
	}
	return stats, tx.Commit(ctx)
}
