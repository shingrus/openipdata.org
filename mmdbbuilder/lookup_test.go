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
	if _, err := buildGeo([]string{layer}, "", db, false, nil, nil, nil, nil, ""); err != nil {
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
	db := refDB(t, filepath.Join(dir, "plain.mmdb"), "plain", [][3]string{{"45.0.0.0/24", "US", ""}})
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

// sourcesDB is a tiny MMDB whose cities come from different layers (city_source is the layer file name).
func sourcesDB(t *testing.T, dir string) string {
	t.Helper()
	l1 := writeFile(t, filepath.Join(dir, "10-rir.csv.gz"), "prefix,country,region,city,source\n"+
		"20.0.0.0/16,DE,,,rir\n")
	l2 := writeFile(t, filepath.Join(dir, "25-bgp.csv.gz"), "prefix,country,region,city,source\n"+
		"20.0.1.0/24,DE,,Hamburg,x\n")
	l3 := writeFile(t, filepath.Join(dir, "36-mlab.csv.gz"), "prefix,country,region,city,source\n"+
		"20.0.2.0/24,DE,,Munich,mlab\n")
	l4 := writeFile(t, filepath.Join(dir, "70-geofeeds.csv.gz"), "prefix,country,region,city,source\n"+
		"20.0.3.0/24,DE,,Berlin,feed\n")
	l5 := writeFile(t, filepath.Join(dir, "37-whois-text.csv.gz"), "prefix,country,region,city,source\n"+
		"20.0.5.0/24,DE,,Bonn,whois-text:ripe\n")
	l6 := writeFile(t, filepath.Join(dir, "37-whois-arin.csv.gz"), "prefix,country,region,city,source\n"+
		"20.0.6.0/24,DE,,Cologne,whois-city:arin\n")
	db := filepath.Join(dir, "sources.mmdb")
	if _, err := buildGeo([]string{l1, l2, l3, l6, l5, l4}, "", db, false, nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	return db
}

// -with-source adds the city_source as a fourth column; without it the rows are as before.
func TestWriteCitiesSource(t *testing.T) {
	dir := t.TempDir()
	db := sourcesDB(t, dir)
	in := writeFile(t, filepath.Join(dir, "ips.txt"), "20.0.1.1\n20.0.2.1\n20.0.3.1\n20.0.0.1\n")
	plain, with := filepath.Join(dir, "plain.tsv"), filepath.Join(dir, "with.tsv")
	if _, err := writeCities(db, in, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := writeCitiesSource(db, in, plain+"2", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := writeCitiesSource(db, in, with, true, false); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(plain)
	b, _ := os.ReadFile(plain + "2")
	if string(a) != string(b) || string(a) != "20.0.1.1\tDE\tHamburg\n20.0.2.1\tDE\tMunich\n20.0.3.1\tDE\tBerlin\n" {
		t.Errorf("plain output %q (flag off %q)", a, b)
	}
	c, _ := os.ReadFile(with)
	want := "20.0.1.1\tDE\tHamburg\t25-bgp\n20.0.2.1\tDE\tMunich\t36-mlab\n20.0.3.1\tDE\tBerlin\t70-geofeeds\n"
	if string(c) != want {
		t.Errorf("with source %q, want %q", c, want)
	}
}

// lookup -in leaves out only the guesses: the city of 37-whois-text stays.
func TestWriteCitiesKeepsWhoisText(t *testing.T) {
	dir := t.TempDir()
	db := sourcesDB(t, dir)
	in := writeFile(t, filepath.Join(dir, "ips.txt"), "20.0.5.1\n")
	out := filepath.Join(dir, "c.tsv")
	if _, err := writeCitiesSource(db, in, out, true, false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(out); string(data) != "20.0.5.1\tDE\tBonn\t37-whois-text\n" {
		t.Errorf("rows %q", data)
	}
}

// A /24 whose city comes from 25-bgp, 36-mlab, 37-whois-arin or 37-whois-text is still a gap (20.0.5.0/24 and 20.0.6.0/24 are inside the last run); one from a geofeed is not.
func TestGapsWeakSources(t *testing.T) {
	dir := t.TempDir()
	db := sourcesDB(t, dir)
	out := filepath.Join(dir, "gaps.txt")
	if _, err := writeGaps(db, out); err != nil {
		t.Fatal(err)
	}
	want := "20.0.0.0/24 3 DE\n20.0.4.0/24 252 DE\n" // 20.0.3.0/24 (geofeed city) is not in it
	if data, _ := os.ReadFile(out); string(data) != want {
		t.Errorf("gaps = %q, want %q", data, want)
	}
}

// -countries (with -with-source) also writes the IPs that have a country and no city, with an
// empty city and no source; a guessed city is shown as no city; IPs with no country stay out.
func TestWriteCitiesCountries(t *testing.T) {
	dir := t.TempDir()
	db := sourcesDB(t, dir)
	in := writeFile(t, filepath.Join(dir, "ips.txt"), "20.0.0.1\n20.0.3.1\n21.0.0.1\n")
	out := filepath.Join(dir, "c.tsv")
	if _, err := writeCitiesSource(db, in, out, true, true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if want := "20.0.0.1\tDE\t\t\n20.0.3.1\tDE\tBerlin\t70-geofeeds\n"; string(data) != want {
		t.Errorf("rows %q, want %q", data, want)
	}
}
