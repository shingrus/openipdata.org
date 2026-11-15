package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tiny ip2geo MMDB with every kind of gap: whole networks, a network with a city, networks
// smaller than a /24 that agree, split between countries, or hold a city.
func gapsDB(t *testing.T, dir string) string {
	t.Helper()
	layer := writeFile(t, filepath.Join(dir, "gaps-layer.csv.gz"), "prefix,country,region,city,source\n"+
		"10.0.0.0/22,DE,,,test\n"+ // four /24s
		"10.0.4.0/24,DE,,Berlin,test\n"+ // a city: not a gap
		"10.0.5.0/24,DE,,,test\n"+ // a gap, not next to the first run
		"10.0.6.0/25,FR,,,test\n"+ // split between countries: skipped
		"10.0.6.128/25,DE,,,test\n"+
		"10.0.7.0/25,DE,,,test\n"+ // a part has a city: skipped
		"10.0.7.128/25,DE,,Berlin,test\n"+
		"10.0.8.0/26,DE,,,test\n"+ // a part only: the /24 is a gap
		"10.0.9.0/24,FR,,,test\n"+ // two FR /24s in one run
		"10.0.10.0/24,FR,,,test\n"+
		"10.0.11.0/25,DE,,,test\n"+ // two parts of one country
		"10.0.11.128/26,DE,,,test\n"+
		"10.0.12.0/24,DE,,,test\n"+ // next to the one before, same country: one run
		"2001:db8::/32,DE,,,test\n") // IPv6 is not listed
	db := filepath.Join(dir, "gaps.mmdb")
	if _, err := buildGeo([]string{layer}, "", db, false, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestWriteGaps(t *testing.T) {
	dir := t.TempDir()
	db := gapsDB(t, dir)
	out := filepath.Join(dir, "gaps.txt")
	stats, err := writeGaps(db, out)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"10.0.0.0/24 4 DE",
		"10.0.5.0/24 1 DE",
		"10.0.8.0/24 1 DE",
		"10.0.9.0/24 2 FR",
		"10.0.11.0/24 2 DE",
	}, "\n") + "\n"
	if string(data) != want {
		t.Errorf("output:\n%s\nwant:\n%s", data, want)
	}
	if stats != (gapStats{networks: 10, runs: 5, split: 1, hasCity: 1}) {
		t.Errorf("stats = %+v", stats)
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
}

// A database that cannot be read writes nothing.
func TestWriteGapsFailures(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "gaps.txt")
	if _, err := writeGaps(filepath.Join(dir, "missing.mmdb"), out); err == nil {
		t.Error("missing database: no error")
	}
	bad := writeFile(t, filepath.Join(dir, "bad.mmdb"), "not a database")
	if _, err := writeGaps(bad, out); err == nil {
		t.Error("damaged database: no error")
	}
	for _, path := range []string{out, out + ".tmp"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s exists after a failure: %v", path, err)
		}
	}
}
