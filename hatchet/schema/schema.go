// Package schema creates and upgrades the Postgres tables the workflows share:
// public.asn, public.asn_domain, and public.asn_geo_pdb. Each table has one
// writer (see AGENTS.md); every workflow runs Ensure before it touches them.
package schema

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Execer is satisfied by *pgx.Conn, *pgxpool.Pool, and pgx.Tx.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

var statements = []string{
	`CREATE TABLE IF NOT EXISTS public.asn (
		asn BIGINT PRIMARY KEY,
		handle TEXT,
		organization TEXT,
		organization_slug TEXT,
		country TEXT,
		ip_amount NUMERIC,
		ipv4_amount NUMERIC,
		ipv6_amount NUMERIC,
		json TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_asn_ipv4_amount ON public.asn (ipv4_amount)`,
	`CREATE INDEX IF NOT EXISTS idx_asn_ipv6_amount ON public.asn (ipv6_amount)`,
	`CREATE INDEX IF NOT EXISTS idx_asn_org_trim ON public.asn (TRIM(organization))`,
	`CREATE INDEX IF NOT EXISTS idx_asn_org_slug ON public.asn (organization_slug)`,
	`CREATE INDEX IF NOT EXISTS idx_asn_country_trim ON public.asn (TRIM(country))`,
	`CREATE TABLE IF NOT EXISTS public.asn_domain (
		asn BIGINT PRIMARY KEY,
		domain TEXT
	)`,
	// source: 'pdb' or 'rdap'; NULL for rows written before sources were tracked.
	`ALTER TABLE public.asn_domain ADD COLUMN IF NOT EXISTS source TEXT`,
	`ALTER TABLE public.asn_domain ADD COLUMN IF NOT EXISTS checked_at TIMESTAMPTZ`,
	// failed_at: last RDAP lookup that failed temporarily; retried after a pause.
	`ALTER TABLE public.asn_domain ADD COLUMN IF NOT EXISTS failed_at TIMESTAMPTZ`,
	`CREATE TABLE IF NOT EXISTS public.asn_geo_pdb (
		asn BIGINT PRIMARY KEY,
		country TEXT,
		city TEXT,
		dominance DOUBLE PRECISION,
		domain TEXT
	)`,
}

// Ensure creates missing tables, indexes, and columns. It is safe to run every time.
func Ensure(ctx context.Context, db Execer) error {
	for _, stmt := range statements {
		if _, err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	return nil
}
