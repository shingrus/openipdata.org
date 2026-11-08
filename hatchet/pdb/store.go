package pdb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// maxShrink is the largest drop in row count a replace accepts (10%).
const maxShrink = 0.10

// StoreStats compares the replaced table with the new rows.
type StoreStats struct {
	Previous int `json:"previous"`
	Rows     int `json:"rows"`
}

// checkShrink refuses an empty result or one much smaller than the current table,
// which points at a broken download rather than PeeringDB losing networks.
func checkShrink(previous, rows int) error {
	if rows == 0 {
		return fmt.Errorf("no rows computed")
	}
	if float64(rows) < float64(previous)*(1-maxShrink) {
		return fmt.Errorf("refusing to replace asn_geo_pdb: %d rows computed, %d stored (more than %.0f%% fewer)",
			rows, previous, maxShrink*100)
	}
	return nil
}

// Replace swaps the contents of public.asn_geo_pdb for rows in one transaction,
// so readers see either the old or the new table, never a mix.
func Replace(ctx context.Context, dsn string, rows []GeoRow) (StoreStats, error) {
	stats := StoreStats{Rows: len(rows)}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return stats, fmt.Errorf("connect to Postgres: %w", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.asn_geo_pdb (
		asn BIGINT PRIMARY KEY,
		country TEXT,
		city TEXT,
		dominance DOUBLE PRECISION,
		domain TEXT
	)`); err != nil {
		return stats, fmt.Errorf("ensure schema: %w", err)
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
	if err := checkShrink(stats.Previous, len(rows)); err != nil {
		return stats, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.asn_geo_pdb`); err != nil {
		return stats, err
	}
	copyRows := make([][]any, len(rows))
	for i, r := range rows {
		copyRows[i] = []any{r.ASN, r.Country, r.City, r.Dominance, r.Domain}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"public", "asn_geo_pdb"},
		[]string{"asn", "country", "city", "dominance", "domain"}, pgx.CopyFromRows(copyRows)); err != nil {
		return stats, fmt.Errorf("copy rows: %w", err)
	}
	return stats, tx.Commit(ctx)
}
