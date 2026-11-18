package main

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

// berlinLookup answers Berlin in Germany for every address.
func berlinLookup(netip.Addr) (string, string, string, error) {
	return "DE", normCity("Berlin"), "70-geofeeds", nil
}

func feedKeys(m map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range m {
		if strings.HasPrefix(k, "holdout_feed_") {
			out[k] = v
		}
	}
	return out
}

// Per-feed counts, the other bucket for a bad value, and the two sums.
func TestHoldoutByFeed(t *testing.T) {
	dir := t.TempDir()
	truth := writeFile(t, filepath.Join(dir, "truth.csv"), "prefix,country,city,feed\n"+
		"20.0.1.0/24,DE,Berlin,5\n"+ // right
		"20.0.2.0/24,DE,Berlin,5\n"+ // right
		"20.0.3.0/24,DE,Paris,5\n"+ // wrong
		"20.0.4.0/24,DE,Berlin,12\n"+ // right
		"20.0.5.0/24,DE,Rome,12\n"+ // wrong
		"20.0.6.0/24,DE,,12\n"+ // no city: not counted
		"20.0.7.0/24,DE,Berlin,x1\n"+ // bad value: other
		"20.0.8.0/24,DE,Berlin,1234567890123456789\n"+ // 19 digits: other
		"20.0.9.0/24,DE,Rome\n") // short row, no feed: other
	s, err := scoreHoldout(berlinLookup, truth)
	if err != nil {
		t.Fatal(err)
	}
	got := feedKeys(s.metrics())
	want := map[string]float64{
		"holdout_feed_5_city_rows": 3, "holdout_feed_5_city_right": 2,
		"holdout_feed_12_city_rows": 2, "holdout_feed_12_city_right": 1,
		"holdout_feed_other_city_rows": 3, "holdout_feed_other_city_right": 2,
	}
	if len(got) != len(want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	checkFeedSums(t, s)
}

func checkFeedSums(t *testing.T, s holdoutScore) {
	t.Helper()
	var rows, right float64
	for k, v := range feedKeys(s.metrics()) {
		if strings.HasSuffix(k, "_city_rows") {
			rows += v
		}
		if strings.HasSuffix(k, "_city_right") {
			right += v
		}
	}
	if rows != float64(s.cityRows) || right != float64(s.cityOK) {
		t.Errorf("sums: rows %v of %d, right %v of %d", rows, s.cityRows, right, s.cityOK)
	}
}

// Only the 10 feeds with the most rows keep a key; ties go to the smaller id, as a number.
func TestHoldoutFeedCut(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("prefix,country,city,feed\n")
	n := 0
	add := func(feed, rows int) {
		for i := 0; i < rows; i++ {
			n++
			city := "Berlin"
			if i%2 == 1 {
				city = "Rome"
			}
			fmt.Fprintf(&b, "20.%d.%d.0/24,DE,%s,%d\n", n/256, n%256, city, feed)
		}
	}
	// Feeds 100, 20 and 3 have the same rows as feeds 1 and 2: with 12 feeds of 2 rows,
	// the two largest ids (100 and 20 as numbers) are cut, not "100" < "20" as text.
	add(9, 5)
	for _, id := range []int{3, 20, 100, 1, 2, 4, 5, 6, 7, 8, 10} {
		add(id, 2)
	}
	truth := writeFile(t, filepath.Join(dir, "truth.csv"), b.String())
	s, err := scoreHoldout(berlinLookup, truth)
	if err != nil {
		t.Fatal(err)
	}
	m := s.metrics()
	for _, id := range []int{9, 1, 2, 3, 4, 5, 6, 7, 8, 10} {
		if m[fmt.Sprintf("holdout_feed_%d_city_rows", id)] == 0 {
			t.Errorf("feed %d has no key: %v", id, feedKeys(m))
		}
	}
	for _, id := range []int{20, 100} {
		if _, ok := m[fmt.Sprintf("holdout_feed_%d_city_rows", id)]; ok {
			t.Errorf("feed %d has a key", id)
		}
	}
	// Feeds 20 and 100 are 2 rows each, one right each.
	if m["holdout_feed_other_city_rows"] != 4 || m["holdout_feed_other_city_right"] != 2 {
		t.Errorf("other = %v / %v", m["holdout_feed_other_city_rows"], m["holdout_feed_other_city_right"])
	}
	if len(feedKeys(m)) != 22 {
		t.Errorf("%d feed keys, want 22", len(feedKeys(m)))
	}
	checkFeedSums(t, s)
}

// A truth file without the feed column has no per-feed key and the old numbers.
func TestHoldoutWithoutFeedColumn(t *testing.T) {
	dir := t.TempDir()
	rows := "20.0.1.0/24,DE,Berlin\n20.0.2.0/24,DE,Rome\n"
	plain := writeFile(t, filepath.Join(dir, "plain.csv"), "prefix,country,city\n"+rows)
	withFeed := writeFile(t, filepath.Join(dir, "feed.csv"), "prefix,country,city,feed\n20.0.1.0/24,DE,Berlin,4\n20.0.2.0/24,DE,Rome,4\n")
	a, err := scoreHoldout(berlinLookup, plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := scoreHoldout(berlinLookup, withFeed)
	if err != nil {
		t.Fatal(err)
	}
	if len(feedKeys(a.metrics())) != 0 {
		t.Errorf("keys without the column: %v", a.metrics())
	}
	if len(feedKeys(b.metrics())) != 2 {
		t.Errorf("keys with the column: %v", b.metrics())
	}
	if a.rows != b.rows || a.countryOK != b.countryOK || a.cityRows != b.cityRows || a.cityOK != b.cityOK ||
		a.unanswered != b.unanswered || a.cityRows != 2 || a.cityOK != 1 {
		t.Errorf("old numbers differ: %+v vs %+v", a, b)
	}
}
