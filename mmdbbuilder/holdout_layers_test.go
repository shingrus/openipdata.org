package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// The holdout city score by city_source: the answered counts plus the unanswered rows are the rows
// with a city, the right counts are the rows the old score counts as right, and the three old
// holdout numbers are as before.
func TestHoldoutCityBySource(t *testing.T) {
	dir := t.TempDir()
	rir := writeFile(t, filepath.Join(dir, "rir.csv.gz"), "prefix,country,city\n20.0.0.0/16,DE,\n")
	db := sourcesDB(t, dir) // 20.0.1/24 Hamburg (25-bgp), .2 Munich (36-mlab), .3 Berlin (70-geofeeds), .5 Bonn (37-whois-text); 20.0.0/24 has no city
	truth := writeFile(t, filepath.Join(dir, "truth.csv"), "prefix,country,city\n"+
		"20.0.1.0/24,DE,Hamburg\n"+ // 25-bgp, right
		"20.0.2.0/24,DE,Berlin\n"+ // 36-mlab, wrong
		"20.0.3.0/24,DE,berlin\n"+ // 70-geofeeds, right (same comparison as the old score)
		"20.0.5.0/24,DE,Bonn\n"+ // 37-whois-text, right
		"20.0.0.0/24,DE,Paris\n"+ // a record with no city
		"21.0.0.0/24,DE,Essen\n"+ // no record
		"20.0.4.0/24,DE,\n") // a truth row with no city: not counted in the city rows
	m, err := evaluate(evalInputs{db: db, rir: rir, holdoutDB: db, holdoutTruth: truth})
	if err != nil {
		t.Fatal(err)
	}
	if m.HoldoutPrefixes == nil || *m.HoldoutPrefixes != 7 || m.HoldoutCountryPct == nil || m.HoldoutCityPct == nil {
		t.Fatalf("old metrics: %+v", m)
	}
	// Country: 6 of 7 (21.0.0.0 has no record). City: 3 right of 6 rows with a city.
	if *m.HoldoutCountryPct < 85.7 || *m.HoldoutCountryPct > 85.8 || *m.HoldoutCityPct != 50 {
		t.Errorf("country %v city %v", *m.HoldoutCountryPct, *m.HoldoutCityPct)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]float64
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"layer_25_bgp_holdout_city_answered":        1,
		"layer_25_bgp_holdout_city_right":           1,
		"layer_36_mlab_holdout_city_answered":       1,
		"layer_36_mlab_holdout_city_right":          0, // written when answered > 0
		"layer_70_geofeeds_holdout_city_answered":   1,
		"layer_70_geofeeds_holdout_city_right":      1,
		"layer_37_whois_text_holdout_city_answered": 1,
		"layer_37_whois_text_holdout_city_right":    1,
		"holdout_city_unanswered":                   2,
	}
	got := map[string]float64{}
	var answered, right float64
	for k, v := range keys {
		if strings.Contains(k, "holdout_city_") && k != "holdout_city_pct" {
			got[k] = v
			if strings.HasSuffix(k, "_answered") {
				answered += v
			}
			if strings.HasSuffix(k, "_right") {
				right += v
			}
		}
	}
	if len(got) != len(want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (%v), want %v", k, g, ok, v)
		}
	}
	// The invariants: 4 answered + 2 unanswered = 6 rows with a city; 3 right.
	if answered+got["holdout_city_unanswered"] != 6 || right != 3 {
		t.Errorf("answered %v + unanswered %v, right %v", answered, got["holdout_city_unanswered"], right)
	}
	// No holdout, no holdout keys.
	m, err = evaluate(evalInputs{db: db, rir: rir})
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(m)
	if strings.Contains(string(data), "holdout") {
		t.Errorf("holdout keys without -holdout-db: %s", data)
	}
}

// A record with a city and no city_source goes under the "unknown" key, like the Atlas keys.
func TestHoldoutCityUnknownSource(t *testing.T) {
	s := holdoutScore{by: map[string]*layerScore{sourceKey(""): {answered: 2, right: 1}}, cityRows: 3, unanswered: 1}
	got := s.metrics()
	if got["layer_unknown_holdout_city_answered"] != 2 || got["layer_unknown_holdout_city_right"] != 1 || got["holdout_city_unanswered"] != 1 {
		t.Errorf("metrics = %v", got)
	}
	if len((holdoutScore{}).metrics()) != 0 {
		t.Error("no rows, no keys")
	}
}
