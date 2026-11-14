package main

// Reading the scores that the geo build stamps into the metadata of the ip2geo
// MMDB (description.openipdata, a compact JSON object).

import (
	"encoding/json"
	"math"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

const stampKey = "openipdata"

// scores holds what the Accuracy panel shows. A missing value is NaN (the
// percentages), -1 (probes) or the zero time (built). Nothing is ever filled
// with a default.
type scores struct {
	Country  float64 // metrics.atlas_country_pct
	City     float64 // metrics.atlas_city_pct
	Coverage float64 // metrics.city_pct
	Probes   int64   // metrics.atlas_probes
	Built    time.Time
}

func noScores() scores {
	return scores{Country: math.NaN(), City: math.NaN(), Coverage: math.NaN(), Probes: -1}
}

// readScores opens the MMDB, reads its metadata and closes it. On an error it
// returns empty scores and the error.
func readScores(path string) (scores, error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return noScores(), err
	}
	defer r.Close()
	var epoch time.Time
	if r.Metadata.BuildEpoch > 0 {
		epoch = r.Metadata.BuildTime()
	}
	return parseStamp(r.Metadata.Description[stampKey], epoch), nil
}

// parseStamp reads the stamp text (may be empty or garbage). epoch is the
// MMDB's own build time (zero if none), used when the stamp has no valid
// built_at.
func parseStamp(raw string, epoch time.Time) scores {
	s := noScores()
	var st struct {
		BuiltAt string         `json:"built_at"`
		Metrics map[string]any `json:"metrics"`
	}
	if raw != "" && json.Unmarshal([]byte(raw), &st) == nil {
		num := func(k string) float64 {
			if v, ok := st.Metrics[k].(float64); ok {
				return v
			}
			return math.NaN()
		}
		s.Country, s.City, s.Coverage = num("atlas_country_pct"), num("atlas_city_pct"), num("city_pct")
		if p := num("atlas_probes"); p >= 0 && p < 1e12 && p == math.Floor(p) {
			s.Probes = int64(p)
		}
		if t, err := time.Parse(time.RFC3339, st.BuiltAt); err == nil {
			s.Built = t.UTC()
		}
	}
	if s.Built.IsZero() && !epoch.IsZero() {
		s.Built = epoch.UTC()
	}
	return s
}
