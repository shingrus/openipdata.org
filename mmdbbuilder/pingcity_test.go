package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// pingCityIndex: the three ping sites, Paris and Lille, and two French cities that the disks of
// the tests can rule out: Marseille (660 km from Paris) and Cayenne (far away).
func pingCityIndex(t *testing.T, dir string) geoNames {
	t.Helper()
	path := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		geoNamesLine("1", "London", "", "51.50853", "-0.12574", "GB", "8961989")+
			geoNamesLine("2", "Paris", "", "48.85341", "2.3488", "FR", "2138551")+
			geoNamesLine("3", "Frankfurt am Main", "", "50.11552", "8.68417", "DE", "650000")+
			geoNamesLine("4", "Marseille", "", "43.29695", "5.38107", "FR", "870000")+
			geoNamesLine("5", "Lille", "", "50.63297", "3.05858", "FR", "800000")+
			geoNamesLine("6", "Cayenne", "", "4.93333", "-52.33", "FR", "60000"))
	index, err := loadGeoNames(path)
	if err != nil {
		t.Fatal(err)
	}
	return index
}

const pingCityLayer = "prefix,country,region,city,source\n" +
	"10.1.1.0/24,FR,,Marseille,x\n" + // 3 ms from Paris: impossible, Paris fits
	"10.1.2.0/24,FR,,Marseille,x\n" + // like the first; the M-Lab row alone allows it
	"10.1.3.0/24,FR,,Cayenne,x\n" + // only times above 30 ms: no disk is used
	"10.1.4.0/24,FR,,Lille,x\n" + // inside every disk: kept
	"10.1.6.0/24,FR,,Paris,x\n" // the most populous city that fits

const pingCityGeofeed = "prefix,country,region,city,source\n10.1.5.0/24,FR,,Marseille,x\n"

const pingCityFile = pingHeader +
	"10.1.1.0/24,6,3,8\n" +
	"10.1.2.0/24,6,3,8\n" +
	"10.1.3.0/24,40,40,40\n" +
	"10.1.4.0/24,6,3,8\n" +
	"10.1.5.0/24,6,3,8\n"

// pingCityBuild builds with the two layers and the given M-Lab and ping files ("" for none).
func pingCityBuild(t *testing.T, dir, rtt, ping string) (string, error) {
	t.Helper()
	main := writeFile(t, filepath.Join(dir, "10-main.csv.gz"), pingCityLayer)
	feeds := writeFile(t, filepath.Join(dir, "70-geofeeds.csv.gz"), pingCityGeofeed)
	var rttPath, pingPath string
	if rtt != "" {
		rttPath = writeFile(t, filepath.Join(dir, "rtt.csv.gz"), rtt)
	}
	if ping != "" {
		pingPath = writeFile(t, filepath.Join(dir, "hosting-rtt.csv.gz"), ping)
	}
	out := filepath.Join(dir, "out.mmdb")
	_, err := buildGeoNames([]string{main, feeds}, "", out, false, nil, nil, nil, pingCityIndex(t, dir), nil, "", false, "", "", rttPath, "", pingPath)
	return out, err
}

func TestPingCityAlone(t *testing.T) {
	out, err := pingCityBuild(t, t.TempDir(), "", pingCityFile)
	if err != nil {
		t.Fatal(err)
	}
	checkCities(t, out, map[string]string{
		"10.1.1.9": "FR/Paris/87-rtt-country",
		"10.1.2.9": "FR/Paris/87-rtt-country",
		"10.1.3.9": "FR/Cayenne/10-main", // 40 ms is not used
		"10.1.4.9": "FR/Lille/10-main",
		"10.1.5.9": "FR/Marseille/70-geofeeds", // the operator's own city
	})
}

// With the ASN prefixes a /24 that only the ping file names gets a city of its network first.
func TestPingCityNetworkCity(t *testing.T) {
	dir := t.TempDir()
	main := writeFile(t, filepath.Join(dir, "10-main.csv.gz"), pingCityLayer)
	asn := writeFile(t, filepath.Join(dir, "asn.csv.gz"), "asn,prefix\n1,10.1.0.0/16\n")
	ping := writeFile(t, filepath.Join(dir, "hosting-rtt.csv.gz"), pingCityFile)
	out := filepath.Join(dir, "out.mmdb")
	if _, err := buildGeoNames([]string{main}, "", out, false, nil, nil, nil, pingCityIndex(t, dir), nil, asn, false, "", "", "", "", ping); err != nil {
		t.Fatal(err)
	}
	// Lille and Paris both fit and have a /24 each in the network: the smaller name wins.
	checkCities(t, out, map[string]string{
		"10.1.1.9": "FR/Lille/87-rtt-network",
		"10.1.4.9": "FR/Lille/10-main",
		"10.1.6.9": "FR/Paris/10-main",
	})
}

func TestPingCityWithRTT(t *testing.T) {
	// The M-Lab row alone (London, 12 ms = 1300 km) allows Marseille.
	rtt := rttHeader + "10.1.2.0/24,GB,London,12\n"
	only, err := pingCityBuild(t, t.TempDir(), rtt, "")
	if err != nil {
		t.Fatal(err)
	}
	checkCities(t, only, map[string]string{"10.1.2.9": "FR/Marseille/10-main"})
	// With the ping disks of the same /24 together the city is impossible.
	both, err := pingCityBuild(t, t.TempDir(), rtt, pingCityFile)
	if err != nil {
		t.Fatal(err)
	}
	checkCities(t, both, map[string]string{
		"10.1.2.9": "FR/Paris/87-rtt-country",
		"10.1.1.9": "FR/Paris/87-rtt-country",
	})
}

// Too few site columns only skips the ping file; the M-Lab rows still work and the output is
// the one without the ping file.
func TestPingCityFewSitesSkips(t *testing.T) {
	rtt := rttHeader + "10.1.1.0/24,GB,London,5\n" // 600 km: Marseille is out, Paris is in
	few := "prefix,GB/London,FR/Paris\n10.1.1.0/24,6,3\n"
	a, err := pingCityBuild(t, t.TempDir(), rtt, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := pingCityBuild(t, t.TempDir(), rtt, few)
	if err != nil {
		t.Fatal(err)
	}
	checkCities(t, b, map[string]string{"10.1.1.9": "FR/Paris/87-rtt-country"})
	if dump(t, a) != dump(t, b) {
		t.Errorf("outputs differ:\n%s\n%s", dump(t, a), dump(t, b))
	}
	var c pingCityCounts
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "few.csv.gz"), few)
	if _, _, err := addPingRows(path, pingCityIndex(t, dir), nil, nil, &c); err != nil || !c.skippedFile {
		t.Errorf("err = %v, counts = %+v", err, c)
	}
	// No prefix column is an error.
	bad := writeFile(t, filepath.Join(dir, "bad.csv.gz"), "GB/London,FR/Paris,DE/Frankfurt am Main\n1,2,3\n")
	if _, _, err := addPingRows(bad, pingCityIndex(t, dir), nil, nil, &c); err == nil {
		t.Error("a file with no prefix column was accepted")
	}
}

// An error names the file it came from, also when the other file is given and good.
func TestPingCityErrorNamesTheFile(t *testing.T) {
	rtt := rttHeader + "10.1.1.0/24,GB,London,5\n"
	_, err := pingCityBuild(t, t.TempDir(), rtt, "GB/London,FR/Paris,DE/Frankfurt am Main\n1,2,3\n")
	if err == nil || !strings.HasPrefix(err.Error(), "hosting-rtt.csv.gz: ") {
		t.Errorf("err = %v", err)
	}
	_, err = pingCityBuild(t, t.TempDir(), "country,city,rtt_ms\nGB,London,5\n", pingCityFile)
	if err == nil || !strings.HasPrefix(err.Error(), "rtt.csv.gz: ") {
		t.Errorf("err = %v", err)
	}
}

func TestPingCityRowCounts(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "p.csv.gz"), pingHeader+
		"10.1.1.0/24,6,3,40\n"+ // two times of at most 30 ms
		"10.1.3.0/24,40,40,40\n"+ // a row, but no time to use
		"10.1.4.0/24,,3\n"+ // too few times: skipped by the reader
		"10.1.5.0/24,31,30,29.5\n") // 30 is in, 31 is out
	var c pingCityCounts
	rows, sites, err := addPingRows(path, pingCityIndex(t, dir), nil, nil, &c)
	if err != nil {
		t.Fatal(err)
	}
	if c.times != 4 || c.clients != 2 || c.skipped != 1 || c.skippedFile || len(rows) != 4 || len(sites) != 3 {
		t.Errorf("counts = %+v, rows = %d, sites = %d", c, len(rows), len(sites))
	}
	for i := 1; i < len(rows); i++ {
		if cmpRTTRow(rows[i-1], rows[i]) > 0 {
			t.Error("rows are not sorted")
		}
	}
}

func TestPingCityNeedsGeoNames(t *testing.T) {
	dir := t.TempDir()
	layer := writeFile(t, filepath.Join(dir, "layer.csv.gz"), pingCityLayer)
	ping := writeFile(t, filepath.Join(dir, "p.csv.gz"), pingCityFile)
	_, err := buildGeoNames([]string{layer}, "", filepath.Join(dir, "o.mmdb"), false, nil, nil, nil, nil, nil, "", false, "", "", "", "", ping)
	if err == nil || !strings.Contains(err.Error(), "-ping-city needs -geonames") {
		t.Errorf("err = %v", err)
	}
}
