package schema

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// rdnsCityRulesStatement creates the table written only by learn-rdns-rules.
// rejected_versions remembers withdrawn content so a later identical proposal
// cannot silently revive it. A domain has up to 4 rows, one per rank: rank 1 is its
// best learned extraction, rank 2 the best on names rank 1 does not decode, rank 3 likewise;
// rank 4 is the LLM rule of propose-codes (origin llm), which the learner never touches.
const rdnsCityRulesStatement = `CREATE TABLE IF NOT EXISTS public.rdns_city_rules (
	domain TEXT NOT NULL,
	rank SMALLINT NOT NULL DEFAULT 1 CHECK (rank BETWEEN 1 AND 4),
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

// rdnsCityRulesWidenRank replaces the old rank check (1 to 3) with 1 to 4. It does nothing,
// and so takes no lock, when no constraint with the old bound exists. Postgres stores
// BETWEEN 1 AND 3 as (rank >= 1) AND (rank <= 3).
const rdnsCityRulesWidenRank = `DO $$ DECLARE old_check TEXT; BEGIN
	SELECT conname INTO old_check FROM pg_constraint
	WHERE conrelid = to_regclass('public.rdns_city_rules') AND contype = 'c'
	  AND pg_get_constraintdef(oid) LIKE '%rank <= 3%' LIMIT 1;
	IF old_check IS NOT NULL THEN
		EXECUTE format('ALTER TABLE public.rdns_city_rules DROP CONSTRAINT %I', old_check);
		ALTER TABLE public.rdns_city_rules ADD CONSTRAINT rdns_city_rules_rank_check CHECK (rank BETWEEN 1 AND 4);
	END IF;
END $$`

// rdnsLLMAnswersStatements creates the record of the LLM questions of propose-codes (one row
// per domain and question), written only by learn-rdns-rules. Only status = 'complete' rows
// count as answered. It holds name patterns (digits and hex runs removed), never IPs or names.
var rdnsLLMAnswersStatements = []string{
	`CREATE TABLE IF NOT EXISTS public.rdns_llm_answers (
	id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	asked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	kind TEXT NOT NULL,
	domain TEXT NOT NULL,
	request_id TEXT NOT NULL,
	model TEXT NOT NULL,
	hosts INTEGER NOT NULL DEFAULT 0,
	patterns_sent TEXT[] NOT NULL DEFAULT '{}',
	patterns_hash TEXT NOT NULL DEFAULT '',
	rule_versions TEXT[] NOT NULL DEFAULT '{}',
	answer JSONB,
	status TEXT NOT NULL DEFAULT 'incomplete',
	verdict TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	disagreements INTEGER NOT NULL DEFAULT 0,
	overrides INTEGER NOT NULL DEFAULT 0,
	disputed INTEGER NOT NULL DEFAULT 0,
	tokens_in INTEGER,
	tokens_out INTEGER,
	cost_usd DOUBLE PRECISION
)`,
	`CREATE INDEX IF NOT EXISTS rdns_llm_answers_domain_asked_idx ON public.rdns_llm_answers (domain, asked_at DESC)`,
}

// EnsureRDNSLLMAnswers creates public.rdns_llm_answers only (written by learn-rdns-rules).
func EnsureRDNSLLMAnswers(ctx context.Context, db Execer) error {
	for _, stmt := range rdnsLLMAnswersStatements {
		if _, err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	return nil
}

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
	if _, err := db.Exec(ctx, rdnsCityRulesWidenRank); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}
	return nil
}
