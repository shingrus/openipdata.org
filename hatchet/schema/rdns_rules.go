package schema

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// rdnsCityRulesStatement creates the table written only by learn-rdns-rules.
// rejected_versions remembers withdrawn content so a later identical proposal
// cannot silently revive it. A domain has up to 3 rows, one per rank: rank 1 is its
// best extraction, rank 2 the best on names rank 1 does not decode, rank 3 likewise.
const rdnsCityRulesStatement = `CREATE TABLE IF NOT EXISTS public.rdns_city_rules (
	domain TEXT NOT NULL,
	rank SMALLINT NOT NULL DEFAULT 1 CHECK (rank BETWEEN 1 AND 3),
	pattern TEXT NOT NULL,
	dictionary TEXT NOT NULL,
	codes JSONB NOT NULL DEFAULT '{}'::jsonb,
	places JSONB NOT NULL DEFAULT '{}'::jsonb,
	version TEXT NOT NULL,
	names BIGINT NOT NULL DEFAULT 0,
	decoded BIGINT NOT NULL DEFAULT 0,
	distinct_places BIGINT NOT NULL DEFAULT 0,
	country_agree DOUBLE PRECISION,
	truth_hits BIGINT,
	truth_misses BIGINT,
	precision DOUBLE PRECISION,
	status TEXT NOT NULL DEFAULT 'candidate',
	origin TEXT NOT NULL DEFAULT 'learned',
	learned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	measured_at TIMESTAMPTZ,
	rejected_versions TEXT[] NOT NULL DEFAULT '{}',
	PRIMARY KEY (domain, rank)
)`

// ErrOldRuleLayout is returned when public.rdns_city_rules exists without the rank
// column (created before the table was keyed by (domain, rank)). It is never fixed
// automatically: the table holds only learned rules, so dropping it once is safe.
var ErrOldRuleLayout = errors.New("public.rdns_city_rules has the old per-domain layout; drop it once (it holds only learned rules) and rerun")

// oldRuleLayoutMarker is the message of the exception rdnsCityRulesCheck raises.
const oldRuleLayoutMarker = "rdns_city_rules old per-domain layout"

// rdnsCityRulesCheck raises when the table exists without the rank column.
// CREATE TABLE IF NOT EXISTS would keep such a table and later inserts would fail.
const rdnsCityRulesCheck = `DO $$ BEGIN
	IF to_regclass('public.rdns_city_rules') IS NOT NULL AND NOT EXISTS (
		SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('public.rdns_city_rules') AND attname = 'rank' AND NOT attisdropped
	) THEN RAISE EXCEPTION '` + oldRuleLayoutMarker + `'; END IF;
END $$`

// EnsureRDNSCityRules creates only the table owned by learn-rdns-rules. A table with the
// old per-domain layout is reported (ErrOldRuleLayout), never dropped or altered.
func EnsureRDNSCityRules(ctx context.Context, db Execer) error {
	if _, err := db.Exec(ctx, rdnsCityRulesCheck); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Message == oldRuleLayoutMarker {
			return ErrOldRuleLayout
		}
		return fmt.Errorf("ensure schema: %w", err)
	}
	if _, err := db.Exec(ctx, rdnsCityRulesStatement); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}
	return nil
}
