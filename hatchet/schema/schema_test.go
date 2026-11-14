package schema

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type recorder struct{ sqls []string }

func (r *recorder) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	r.sqls = append(r.sqls, sql)
	return pgconn.CommandTag{}, nil
}

func TestEnsureGeoEvalRunsOnlyGeoEval(t *testing.T) {
	r := &recorder{}
	if err := EnsureGeoEval(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(r.sqls) != 1 || r.sqls[0] != geoEvalStatement {
		t.Fatalf("want only the geo_eval statement, got %q", r.sqls)
	}
	if !strings.Contains(r.sqls[0], "CREATE TABLE IF NOT EXISTS public.geo_eval") || strings.Contains(r.sqls[0], "ALTER") {
		t.Fatalf("unexpected statement: %s", r.sqls[0])
	}
}

// Ensure is run by the ASN workflows: it must not touch tables they don't write.
func TestEnsureLeavesOutGeoEval(t *testing.T) {
	r := &recorder{}
	if err := Ensure(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(r.sqls, geoEvalStatement) {
		t.Fatal("Ensure runs the geo_eval statement")
	}
}

func TestEnsureAddsFacilityColumns(t *testing.T) {
	r := &recorder{}
	if err := Ensure(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"fac_count INTEGER", "fac_city TEXT", "fac_country TEXT"} {
		want := "ALTER TABLE public.asn_geo_pdb ADD COLUMN IF NOT EXISTS " + col
		if !slices.Contains(r.sqls, want) {
			t.Errorf("Ensure does not run %q", want)
		}
	}
}

func TestEnsureGeofeedURLsRunsOnlyItsStatements(t *testing.T) {
	r := &recorder{}
	if err := EnsureGeofeedURLs(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.sqls, geofeedURLsStatements) || len(r.sqls) != 3 {
		t.Fatalf("want the three geofeed_urls statements, got %q", r.sqls)
	}
	for _, sql := range r.sqls {
		if !strings.Contains(sql, "IF NOT EXISTS") || !strings.Contains(sql, "public.geofeed_urls") || strings.Contains(sql, "ALTER") {
			t.Errorf("unexpected statement: %s", sql)
		}
	}
}

func TestEnsureLeavesOutGeofeedURLs(t *testing.T) {
	r := &recorder{}
	if err := Ensure(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range geofeedURLsStatements {
		if slices.Contains(r.sqls, stmt) {
			t.Errorf("Ensure runs %q", stmt)
		}
	}
}

// visitor_ips is written by external websites: resolve-ips runs one CREATE TABLE IF NOT EXISTS
// and nothing that locks the table (no ALTER, no index).
func TestEnsureVisitorIPsRunsOnlyCreateTable(t *testing.T) {
	r := &recorder{}
	if err := EnsureVisitorIPs(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(r.sqls) != 1 || r.sqls[0] != visitorIPsStatement {
		t.Fatalf("want only the visitor_ips statement, got %q", r.sqls)
	}
	sql := r.sqls[0]
	if !strings.HasPrefix(sql, "CREATE TABLE IF NOT EXISTS public.visitor_ips") || strings.Contains(sql, "ALTER") || strings.Contains(sql, "INDEX") {
		t.Fatalf("unexpected statement: %s", sql)
	}
	for _, col := range []string{"ip inet PRIMARY KEY", "source text NOT NULL", "first_seen_at timestamptz NOT NULL DEFAULT now()", "last_seen_at timestamptz NOT NULL DEFAULT now()"} {
		if !strings.Contains(sql, col) {
			t.Errorf("visitor_ips lacks %q", col)
		}
	}
}

func TestEnsureRDNSRunsOnlyItsTables(t *testing.T) {
	r := &recorder{}
	if err := EnsureRDNS(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.sqls, rdnsStatements) {
		t.Fatalf("got %q", r.sqls)
	}
	tables := map[string]bool{}
	for _, sql := range r.sqls {
		// The ALTERs are on tables that resolve-ips alone writes: network_traces (it also
		// alone reads it) and rdns_geo_layer (build-geo-data reads it).
		alter := strings.Contains(sql, "ALTER") &&
			!strings.HasPrefix(sql, "ALTER TABLE public.network_traces ADD COLUMN IF NOT EXISTS ") &&
			!strings.HasPrefix(sql, "ALTER TABLE public.ip_ptr_names ADD COLUMN IF NOT EXISTS ") &&
			!strings.HasPrefix(sql, "ALTER TABLE public.rdns_geo_layer ADD COLUMN IF NOT EXISTS ")
		if !strings.Contains(sql, "IF NOT EXISTS") || alter || strings.Contains(sql, "visitor_ips") {
			t.Errorf("unexpected statement: %s", sql)
		}
		for _, table := range []string{"network_traces", "traced_networks", "ip_ptr_names", "rdns_geo_layer"} {
			if strings.Contains(sql, "public."+table) {
				tables[table] = true
			}
		}
	}
	if len(tables) != 4 {
		t.Errorf("tables created: %v", tables)
	}
	if !strings.Contains(strings.Join(r.sqls, "\n"), "PRIMARY KEY (prefix, target_kind, traced_at)") {
		t.Error("network_traces has no (prefix, target_kind, traced_at) key")
	}
	if !strings.Contains(strings.Join(r.sqls, "\n"), "ADD COLUMN IF NOT EXISTS candidates inet[]") {
		t.Error("network_traces has no candidates column")
	}
	if !strings.Contains(strings.Join(r.sqls, "\n"), "ADD COLUMN IF NOT EXISTS hop_ips inet[]") {
		t.Error("network_traces has no hop_ips column")
	}
	// traced_networks: one row per traced /24, with its schedule; its next_trace_at index is partial.
	var traced string
	for _, sql := range r.sqls {
		if strings.HasPrefix(sql, "CREATE TABLE IF NOT EXISTS public.traced_networks") {
			traced = sql
		}
	}
	for _, col := range []string{"prefix cidr PRIMARY KEY", "status text NOT NULL", "attempts integer", "last_traced_at timestamptz NOT NULL",
		"next_trace_at timestamptz,", "located_at timestamptz"} {
		if !strings.Contains(traced, col) {
			t.Errorf("traced_networks lacks %q: %s", col, traced)
		}
	}
	if !strings.Contains(strings.Join(r.sqls, "\n"), "ON public.traced_networks (next_trace_at) WHERE next_trace_at IS NOT NULL") {
		t.Error("traced_networks has no partial next_trace_at index")
	}
	if !strings.Contains(strings.Join(r.sqls, "\n"), "ALTER TABLE public.ip_ptr_names ADD COLUMN IF NOT EXISTS tried_at timestamptz") {
		t.Error("ip_ptr_names has no idempotent tried_at column")
	}
	var alters []string
	for _, sql := range r.sqls {
		if strings.HasPrefix(sql, "ALTER TABLE public.rdns_geo_layer ") {
			alters = append(alters, sql)
		}
	}
	if len(alters) != 1 {
		t.Fatalf("ALTER statements of rdns_geo_layer: %q", alters)
	}
	for _, col := range []string{"method text", "rule_votes integer", "mmdb_votes integer", "candidate_votes integer NOT NULL DEFAULT 0"} {
		if !strings.Contains(alters[0], "ADD COLUMN IF NOT EXISTS "+col) {
			t.Errorf("rdns_geo_layer has no idempotent %q column: %s", col, alters[0])
		}
	}
}

// Ensure is run by the ASN workflows: it must not lock the visitor_ips or rDNS tables.
func TestEnsureLeavesOutRDNSTables(t *testing.T) {
	r := &recorder{}
	if err := Ensure(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for _, sql := range r.sqls {
		for _, table := range []string{"visitor_ips", "network_traces", "traced_networks", "ip_ptr_names", "rdns_geo_layer", "rdns_city_rules"} {
			if strings.Contains(sql, table) {
				t.Errorf("Ensure touches %s: %s", table, sql)
			}
		}
	}
}

func TestEnsureRDNSCityRulesRunsOnlyItsTable(t *testing.T) {
	r := &recorder{}
	if err := EnsureRDNSCityRules(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(r.sqls) == 0 {
		t.Fatal("no rule table DDL")
	}
	if all := strings.Join(r.sqls, "\n"); !strings.Contains(all, "PRIMARY KEY (domain, rank)") {
		t.Error("rule table is not keyed by (domain, rank)")
	}
	for _, sql := range r.sqls {
		if strings.HasPrefix(sql, "DO $$") { // the old-layout check
			continue
		}
		if !strings.Contains(sql, "IF NOT EXISTS") || !strings.Contains(sql, "public.rdns_city_rules") {
			t.Errorf("unexpected statement: %s", sql)
		}
		for _, other := range []string{"public.asn", "public.network_traces", "public.ip_ptr_names", "public.rdns_geo_layer"} {
			if strings.Contains(sql, other) {
				t.Errorf("learner DDL touches %s", other)
			}
		}
	}
}

// failingExec returns err for the statements that contain match.
type failingExec struct {
	match string
	err   error
	sqls  []string
}

func (f *failingExec) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.sqls = append(f.sqls, sql)
	if strings.Contains(sql, f.match) {
		return pgconn.CommandTag{}, f.err
	}
	return pgconn.CommandTag{}, nil
}

func TestEnsureRDNSCityRulesOldLayoutIsReported(t *testing.T) {
	f := &failingExec{match: "DO $$", err: &pgconn.PgError{Code: "P0001", Message: oldRuleLayoutMarker}}
	err := EnsureRDNSCityRules(context.Background(), f)
	if !errors.Is(err, ErrOldRuleLayout) || !strings.Contains(err.Error(), "drop it once") {
		t.Fatalf("err = %v", err)
	}
	for _, sql := range f.sqls {
		if strings.Contains(strings.ToUpper(sql), "DROP ") && !strings.Contains(sql, "RAISE") {
			t.Errorf("must never drop: %s", sql)
		}
		if strings.Contains(sql, "CREATE TABLE") {
			t.Error("created the table after detecting the old layout")
		}
	}
	// Any other failure keeps the generic wrapper.
	other := &failingExec{match: "DO $$", err: errors.New("boom")}
	if err := EnsureRDNSCityRules(context.Background(), other); err == nil || errors.Is(err, ErrOldRuleLayout) {
		t.Errorf("other error = %v", err)
	}
}

func TestEnsureRDNSCityRulesAllowsRankFourAndWidensOnlyTheOldCheck(t *testing.T) {
	r := &recorder{}
	if err := EnsureRDNSCityRules(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(r.sqls, "\n")
	if !strings.Contains(all, "CHECK (rank BETWEEN 1 AND 4)") {
		t.Error("a new table does not allow rank 4")
	}
	// The widening only runs when the old constraint exists (no lock otherwise).
	var widen string
	for _, sql := range r.sqls {
		if strings.Contains(sql, "DROP CONSTRAINT") {
			widen = sql
		}
	}
	for _, want := range []string{"IF old_check IS NOT NULL THEN", "pg_get_constraintdef(oid) LIKE '%rank <= 3%'", "ADD CONSTRAINT rdns_city_rules_rank_check CHECK (rank BETWEEN 1 AND 4)"} {
		if !strings.Contains(widen, want) {
			t.Errorf("widening lacks %q: %s", want, widen)
		}
	}
}

func TestEnsureRDNSLLMAnswersRunsOnlyItsTable(t *testing.T) {
	r := &recorder{}
	if err := EnsureRDNSLLMAnswers(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(r.sqls) == 0 {
		t.Fatal("no DDL")
	}
	for _, sql := range r.sqls {
		if !strings.Contains(sql, "IF NOT EXISTS") || !strings.Contains(sql, "public.rdns_llm_answers") {
			t.Errorf("unexpected statement: %s", sql)
		}
		if strings.Contains(sql, "ALTER TABLE") {
			t.Errorf("no ALTER on the answers table: %s", sql)
		}
	}
	for _, col := range []string{"status TEXT NOT NULL", "verdict TEXT NOT NULL", "patterns_hash", "rule_versions", "request_id", "cost_usd"} {
		if !strings.Contains(r.sqls[0], col) {
			t.Errorf("missing column %q", col)
		}
	}
	// Ensure (the ASN workflows) must not touch it.
	ar := &recorder{}
	if err := Ensure(context.Background(), ar); err != nil {
		t.Fatal(err)
	}
	for _, sql := range ar.sqls {
		if strings.Contains(sql, "rdns_llm_answers") {
			t.Errorf("Ensure touches rdns_llm_answers: %s", sql)
		}
	}
}
