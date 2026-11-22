package asn

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// GeoScoreRow is one published build-geo-data score row for the geo_scores table.
// A nil value is NULL: the key is missing or its value is not a finite number.
type GeoScoreRow struct {
	ID               int64 // public.geo_eval.id; the later id wins when two rows share a second
	RunAt            string
	CityPct          *float64
	AtlasCountryPct  *float64
	AtlasCityPct     *float64
	PrecisionPct     *float64
	HoldoutCityPct   *float64
	MaxMindAtlasCity *float64
	DBIPAtlasCity    *float64
}

// ParseGeoScore picks the seven scores out of a geo_eval metrics document. A metrics
// document that is not a JSON object gives all NULLs.
func ParseGeoScore(id int64, runAt time.Time, metrics []byte) GeoScoreRow {
	row := GeoScoreRow{ID: id, RunAt: runAt.UTC().Format(time.RFC3339)}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(metrics, &m); err != nil {
		return row
	}
	row.CityPct = metricNumber(m, "city_pct")
	row.AtlasCountryPct = metricNumber(m, "atlas_country_pct")
	row.AtlasCityPct = metricNumber(m, "atlas_city_pct")
	row.PrecisionPct = metricNumber(m, "precision_pct")
	row.HoldoutCityPct = metricNumber(m, "holdout_city_pct")
	row.MaxMindAtlasCity = metricNumber(m, "maxmind_atlas_city_pct")
	row.DBIPAtlasCity = metricNumber(m, "dbip_atlas_city_pct")
	return row
}

// metricNumber returns the value of key when it is a finite JSON number, else nil.
func metricNumber(m map[string]json.RawMessage, key string) *float64 {
	raw, ok := m[key]
	if !ok || string(raw) == "null" {
		return nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil { // a string, bool, null or object fails
		return nil
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// cleanGeoScores sorts by run_at (then id) and keeps the later row of two with the same run_at.
func cleanGeoScores(rows []GeoScoreRow) []GeoScoreRow {
	sorted := append([]GeoScoreRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].RunAt != sorted[j].RunAt {
			return sorted[i].RunAt < sorted[j].RunAt
		}
		return sorted[i].ID < sorted[j].ID
	})
	out := sorted[:0]
	for _, r := range sorted {
		if n := len(out); n > 0 && out[n-1].RunAt == r.RunAt {
			out[n-1] = r
			continue
		}
		out = append(out, r)
	}
	return out
}

// GeoScores returns the published public.geo_eval rows of the last 60 days. build-asn-data
// does not own that table and runs no DDL for it: a missing table means no rows.
func (s *Store) GeoScores(ctx context.Context) ([]GeoScoreRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, run_at, metrics FROM public.geo_eval
		WHERE published AND run_at >= now() - interval '60 days' ORDER BY run_at, id`)
	if err != nil {
		return nil, noRowsIfMissing(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (GeoScoreRow, error) {
		var id int64
		var runAt time.Time
		var metrics []byte
		if err := row.Scan(&id, &runAt, &metrics); err != nil {
			return GeoScoreRow{}, err
		}
		return ParseGeoScore(id, runAt, metrics), nil
	})
	if err != nil {
		return nil, noRowsIfMissing(err)
	}
	return out, nil
}

// noRowsIfMissing turns "undefined_table" (42P01) into no error.
func noRowsIfMissing(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return nil
	}
	return err
}
