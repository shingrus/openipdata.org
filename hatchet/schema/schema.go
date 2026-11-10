// Package schema creates and upgrades the Postgres tables the workflows share:
// public.asn, public.asn_domain, public.asn_geo_pdb, public.geo_ix_pdb, and public.geo_eval. Each table has one
// writer (see AGENTS.md); a workflow runs DDL only for tables it writes (Ensure, or
// EnsureGeoEval for the read-only build-geo-data).
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

// geoEvalStatement creates public.geo_eval. build-geo-data runs it alone
// (EnsureGeoEval), so it must stay a single self-contained statement.
// One row per build-geo-data run: the scores of the MMDB it built. published
// is set once that MMDB has been published; the latest such row is the
// baseline the next publish is compared with.
const geoEvalStatement = `CREATE TABLE IF NOT EXISTS public.geo_eval (
		id BIGSERIAL PRIMARY KEY,
		run_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		date_tag TEXT,
		metrics JSONB NOT NULL,
		published BOOLEAN NOT NULL DEFAULT false
	)`

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
	// seen_at: last build-asn-data run that found the ASN in the source. Rows
	// are never deleted, so this tells current ASNs from old ones; NULL for
	// rows written before the column was added.
	`ALTER TABLE public.asn ADD COLUMN IF NOT EXISTS seen_at TIMESTAMPTZ`,
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
	// The network's own "scope" and "type" in PeeringDB; NULL for rows written
	// before the columns were added, until the next sync-peeringdb run.
	`ALTER TABLE public.asn_geo_pdb ADD COLUMN IF NOT EXISTS info_scope TEXT`,
	`ALTER TABLE public.asn_geo_pdb ADD COLUMN IF NOT EXISTS info_type TEXT`,
	// Peering LAN prefixes of internet exchanges with the exchange's country and
	// city; sync-peeringdb replaces them together with asn_geo_pdb.
	`CREATE TABLE IF NOT EXISTS public.geo_ix_pdb (
		prefix TEXT PRIMARY KEY,
		ix_id BIGINT,
		country TEXT,
		city TEXT
	)`,
	geoEvalStatement,
}

// EnsureGeoEval creates public.geo_eval only. build-geo-data uses it instead of
// Ensure: it reads the other tables, and the ALTER TABLE statements in Ensure
// take an ACCESS EXCLUSIVE lock on them even when the column exists, which
// would make a reader block (or be blocked by) the writers.
func EnsureGeoEval(ctx context.Context, db Execer) error {
	if _, err := db.Exec(ctx, geoEvalStatement); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}
	return nil
}

// Ensure creates missing tables, indexes, and columns. It is safe to run every time.
// Workflows call it only when they write one of these tables.
func Ensure(ctx context.Context, db Execer) error {
	for _, stmt := range statements {
		if _, err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	return nil
}
