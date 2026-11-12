package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tiny ip2geo MMDB: a city, a country without a city, and a /24 inside the city's /16.
func lookupDB(t *testing.T, dir string) string {
	t.Helper()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), "prefix,country,region,city,source\n"+
		"45.0.0.0/16,US,,Dallas,test\n"+
		"45.0.5.0/24,DE,,Berlin,test\n"+
		"46.0.0.0/16,FR,,,test\n")
	db := filepath.Join(dir, "geo.mmdb")
	if _, err := buildGeo([]string{layer}, "", db, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestWriteCities(t *testing.T) {
	dir := t.TempDir()
	db := lookupDB(t, dir)
	in := writeFile(t, filepath.Join(dir, "ips.txt"), strings.Join([]string{
		"45.0.0.7",        // Dallas
		"45.0.5.9",        // Berlin: the more specific row
		"46.0.0.1",        // a country but no city: left out
		"47.0.0.1",        // not in the database: left out
		"",                // blank
		"# a comment",     //
		"not-an-ip",       // invalid
		"45.0.0.7/24",     // invalid
		"  45.0.9.9  ",    // Dallas, with spaces
		"::ffff:45.0.0.8", // IPv4 in IPv6 form: written as IPv4
	}, "\n")+"\n")
	out := filepath.Join(dir, "cities.tsv")
	stats, err := writeCities(db, in, out)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "45.0.0.7\tUS\tDallas\n45.0.5.9\tDE\tBerlin\n45.0.9.9\tUS\tDallas\n45.0.0.8\tUS\tDallas\n"
	if string(data) != want {
		t.Errorf("output:\n%s\nwant:\n%s", data, want)
	}
	if stats != (cityStats{lookups: 6, withCity: 4, invalid: 2}) {
		t.Errorf("stats = %+v", stats)
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
}

// A database that cannot be read, or an input that is missing, is an error and writes nothing.
func TestWriteCitiesFailures(t *testing.T) {
	dir := t.TempDir()
	db := lookupDB(t, dir)
	in := writeFile(t, filepath.Join(dir, "ips.txt"), "45.0.0.7\n")
	out := filepath.Join(dir, "cities.tsv")

	if _, err := writeCities(filepath.Join(dir, "missing.mmdb"), in, out); err == nil {
		t.Error("missing database: no error")
	}
	bad := writeFile(t, filepath.Join(dir, "bad.mmdb"), "not a database")
	if _, err := writeCities(bad, in, out); err == nil {
		t.Error("damaged database: no error")
	}
	if _, err := writeCities(db, filepath.Join(dir, "missing.txt"), out); err == nil {
		t.Error("missing input: no error")
	}
	for _, path := range []string{out, out + ".tmp"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s exists after a failure: %v", path, err)
		}
	}
}

// A failed run leaves the previous output as it was.
func TestWriteCitiesKeepsOldOutputOnFailure(t *testing.T) {
	dir := t.TempDir()
	out := writeFile(t, filepath.Join(dir, "cities.tsv"), "old\n")
	in := writeFile(t, filepath.Join(dir, "ips.txt"), "45.0.0.7\n")
	if _, err := writeCities(filepath.Join(dir, "missing.mmdb"), in, out); err == nil {
		t.Fatal("no error")
	}
	if data, _ := os.ReadFile(out); string(data) != "old\n" {
		t.Errorf("output = %q", data)
	}
}

// Records that are not in the "geo" shape (no city_name) give no row.
func TestWriteCitiesOtherRecords(t *testing.T) {
	dir := t.TempDir()
	db := refDB(t, filepath.Join(dir, "ipinfo.mmdb"), "ipinfo", [][3]string{{"45.0.0.0/24", "US", ""}})
	in := writeFile(t, filepath.Join(dir, "ips.txt"), "45.0.0.1\n")
	out := filepath.Join(dir, "cities.tsv")
	stats, err := writeCities(db, in, out)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(out); len(data) != 0 || stats.withCity != 0 || stats.lookups != 1 {
		t.Errorf("rows %q, stats %+v", data, stats)
	}
}
