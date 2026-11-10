package schema

import (
	"context"
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
