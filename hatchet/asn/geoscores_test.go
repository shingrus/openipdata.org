package asn

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestParseGeoScore(t *testing.T) {
	at := time.Date(2026, 11, 21, 9, 41, 9, 500, time.FixedZone("x", 2*3600))
	row := ParseGeoScore(7, at, []byte(`{"city_pct": 97.5, "atlas_country_pct": "97", "atlas_city_pct": null,
		"precision_pct": true, "holdout_city_pct": 1e999, "maxmind_atlas_city_pct": 50, "other": 1}`))
	if row.RunAt != "2026-11-21T07:41:09Z" {
		t.Errorf("RunAt = %q", row.RunAt)
	}
	if row.CityPct == nil || *row.CityPct != 97.5 {
		t.Errorf("CityPct = %v", row.CityPct)
	}
	if row.MaxMindAtlasCity == nil || *row.MaxMindAtlasCity != 50 {
		t.Errorf("MaxMindAtlasCity = %v", row.MaxMindAtlasCity)
	}
	for name, v := range map[string]*float64{"atlas_country": row.AtlasCountryPct, "atlas_city": row.AtlasCityPct,
		"precision": row.PrecisionPct, "holdout": row.HoldoutCityPct, "dbip": row.DBIPAtlasCity} {
		if v != nil {
			t.Errorf("%s = %v, want NULL", name, *v)
		}
	}
	if bad := ParseGeoScore(1, at, []byte(`not json`)); bad.CityPct != nil {
		t.Errorf("bad metrics gave %v", bad.CityPct)
	}
}

func TestExportSQLiteGeoScores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asn.sqlite3")
	at := time.Date(2026, 11, 21, 7, 41, 9, 0, time.UTC)
	scores := []GeoScoreRow{
		ParseGeoScore(3, at.Add(time.Hour), []byte(`{"city_pct": 90}`)),
		ParseGeoScore(1, at, []byte(`{"city_pct": 10, "atlas_city_pct": 5}`)),
		ParseGeoScore(2, at.Add(300*time.Millisecond), []byte(`{"city_pct": 20}`)), // same second, later id wins
	}
	stats, err := ExportSQLite(context.Background(), path, nil, nil, nil, scores)
	if err != nil {
		t.Fatal(err)
	}
	if stats.GeoScores != 2 {
		t.Fatalf("GeoScores = %d, want 2", stats.GeoScores)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT run_at, city_pct, atlas_city_pct FROM geo_scores ORDER BY run_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type got struct {
		runAt string
		city  sql.NullFloat64
		atlas sql.NullFloat64
	}
	var all []got
	for rows.Next() {
		var g got
		if err := rows.Scan(&g.runAt, &g.city, &g.atlas); err != nil {
			t.Fatal(err)
		}
		all = append(all, g)
	}
	if len(all) != 2 || all[0].runAt != "2026-11-21T07:41:09Z" || all[0].city.Float64 != 20 || all[0].atlas.Valid ||
		all[1].runAt != "2026-11-21T08:41:09Z" || all[1].city.Float64 != 90 {
		t.Errorf("rows = %+v", all)
	}
}
