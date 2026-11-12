// Package schema creates and upgrades the Postgres tables the workflows share:
// public.asn, public.asn_domain, public.asn_geo_pdb, public.geo_ix_pdb, public.geo_eval,
// public.geofeed_urls, public.visitor_ips, public.network_traces, public.ip_ptr_names, and
// public.rdns_geo_layer. Each table has one writer (see AGENTS.md); a workflow runs DDL only for
// tables it writes (Ensure, EnsureGeoEval for the read-only build-geo-data, EnsureGeofeedURLs
// for sync-geofeeds, or EnsureVisitorIPs and EnsureRDNS for resolve-ips).
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

// geofeedURLsStatements create public.geofeed_urls: every geofeed URL ever found, with
// when it was seen and checked. sync-geofeeds is its only writer and runs only these
// statements (EnsureGeofeedURLs), never Ensure. The table already exists in production
// (made by an older tool), so the definition must stay exactly as it is there.
var geofeedURLsStatements = []string{
	`CREATE TABLE IF NOT EXISTS public.geofeed_urls (
		id BIGINT GENERATED ALWAYS AS IDENTITY NOT NULL,
		url TEXT NOT NULL,
		first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		last_checked_at TIMESTAMPTZ NULL,
		last_success_at TIMESTAMPTZ NULL,
		last_fetch_status TEXT NULL,
		sources TEXT[] NOT NULL DEFAULT '{}'::text[],
		result_text TEXT NULL,
		CONSTRAINT geofeed_urls_pkey PRIMARY KEY (id),
		CONSTRAINT geofeed_urls_url_key UNIQUE (url)
	)`,
	`CREATE INDEX IF NOT EXISTS geofeed_urls_last_seen_at_idx ON public.geofeed_urls USING btree (last_seen_at)`,
	`CREATE INDEX IF NOT EXISTS geofeed_urls_last_checked_at_idx ON public.geofeed_urls USING btree (last_checked_at)`,
}

// visitorIPsStatement creates public.visitor_ips: the IPs of active visitors, inserted
// by external websites (resolve-ips only reads it). resolve-ips runs this one statement
// (EnsureVisitorIPs) so it never locks the table the websites write to: no ALTER, no extra index.
const visitorIPsStatement = `CREATE TABLE IF NOT EXISTS public.visitor_ips (
		ip inet PRIMARY KEY,
		source text NOT NULL,
		first_seen_at timestamptz NOT NULL DEFAULT now(),
		last_seen_at timestamptz NOT NULL DEFAULT now()
	)`

// rdnsStatements create the tables resolve-ips writes (EnsureRDNS).
var rdnsStatements = []string{
	// One traceroute of one host in a /24. target_kind: visitor, gateway (.1) or random.
	// hops is the full path, [{"ttl": 1, "ip": "192.0.2.1", "rtt_ms": 0.4}, ...], with a
	// null ip for a hop that did not answer. The two newest per (prefix, target_kind) are kept.
	`CREATE TABLE IF NOT EXISTS public.network_traces (
		prefix cidr NOT NULL,
		target_kind text NOT NULL,
		target inet NOT NULL,
		traced_at timestamptz NOT NULL DEFAULT now(),
		hops jsonb NOT NULL,
		PRIMARY KEY (prefix, target_kind, traced_at)
	)`,
	`CREATE INDEX IF NOT EXISTS network_traces_traced_at_idx ON public.network_traces (traced_at)`,
	// candidates: the IPs of the trace that resolve-names and locate look at (the target,
	// then the last quarter of the public hops, closest first), so they never read hops.
	// NULL for rows written before the column was added, until resolve-ips fills them.
	// network_traces has one writer and no other reader, so this ALTER locks nobody.
	`ALTER TABLE public.network_traces ADD COLUMN IF NOT EXISTS candidates inet[]`,
	// The rows still to fill, for the one-time backfill; empty afterwards.
	`CREATE INDEX IF NOT EXISTS network_traces_no_candidates_idx ON public.network_traces (traced_at) WHERE candidates IS NULL`,
	// The PTR name of one IP (a hop or a target). status: ok, not_found, or error;
	// ptr is NULL unless the status is ok.
	`CREATE TABLE IF NOT EXISTS public.ip_ptr_names (
		ip inet PRIMARY KEY,
		ptr text,
		status text NOT NULL,
		resolved_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS ip_ptr_names_resolved_at_idx ON public.ip_ptr_names (resolved_at)`,
	// One located prefix. scope: traced (a /24 a trace located) or wide (an announced
	// prefix, never wider than /16, that the traced /24s voted for). A /24 can be
	// both, hence the key. router_*, rule_name and same_asn describe a traced
	// row's winning router (same_asn is NULL when the target's ASN was unknown) and are NULL
	// for a wide row.
	`CREATE TABLE IF NOT EXISTS public.rdns_geo_layer (
		prefix cidr NOT NULL,
		scope text NOT NULL,
		country text NOT NULL,
		city text NOT NULL,
		router_ip inet,
		router_ptr text,
		rule_name text,
		same_asn boolean,
		located_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (scope, prefix)
	)`,
}

// statements are the tables the ASN workflows write (Ensure). geo_eval,
// geofeed_urls and the resolve-ips tables are not here: only their own writers create them
// (EnsureGeoEval, EnsureGeofeedURLs, EnsureVisitorIPs, EnsureRDNS), so no other workflow takes locks on them.
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
	// Facilities (netfac) only, no exchanges: fac_count is how many the network
	// has; fac_city and fac_country are set only when all of them are in one city.
	// NULL for rows written before the columns were added.
	`ALTER TABLE public.asn_geo_pdb ADD COLUMN IF NOT EXISTS fac_count INTEGER`,
	`ALTER TABLE public.asn_geo_pdb ADD COLUMN IF NOT EXISTS fac_city TEXT`,
	`ALTER TABLE public.asn_geo_pdb ADD COLUMN IF NOT EXISTS fac_country TEXT`,
	// Peering LAN prefixes of internet exchanges with the exchange's country and
	// city; sync-peeringdb replaces them together with asn_geo_pdb.
	`CREATE TABLE IF NOT EXISTS public.geo_ix_pdb (
		prefix TEXT PRIMARY KEY,
		ix_id BIGINT,
		country TEXT,
		city TEXT
	)`,
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

// EnsureGeofeedURLs creates public.geofeed_urls only. sync-geofeeds uses it
// instead of Ensure: it writes no other table.
func EnsureGeofeedURLs(ctx context.Context, db Execer) error {
	for _, stmt := range geofeedURLsStatements {
		if _, err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	return nil
}

// EnsureVisitorIPs creates public.visitor_ips only (see visitorIPsStatement).
func EnsureVisitorIPs(ctx context.Context, db Execer) error {
	if _, err := db.Exec(ctx, visitorIPsStatement); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}
	return nil
}

// EnsureRDNS creates public.network_traces, public.ip_ptr_names and
// public.rdns_geo_layer, the tables resolve-ips writes, and adds the
// network_traces.candidates column. resolve-ips uses it (with EnsureVisitorIPs)
// instead of Ensure.
func EnsureRDNS(ctx context.Context, db Execer) error {
	for _, stmt := range rdnsStatements {
		if _, err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	return nil
}

// Ensure creates the missing tables, indexes, and columns of the ASN tables
// (asn, asn_domain, asn_geo_pdb, geo_ix_pdb). It is safe to run every time.
// Workflows call it only when they write one of these tables.
func Ensure(ctx context.Context, db Execer) error {
	for _, stmt := range statements {
		if _, err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	return nil
}
