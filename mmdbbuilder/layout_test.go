package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The same data gives byte-identical gaps and lookup -in/-out output from the flat file and
// from its converted (GeoLite2-City layout) file; any other database_type is an error.
func TestGapsAndLookupReadBothLayouts(t *testing.T) {
	dir := t.TempDir()
	rows := "prefix,country,region,city,source\n" +
		"10.0.0.0/22,DE,,,x\n10.0.4.0/24,DE,,Berlin,x\n10.0.5.0/24,DE,,,x\n" +
		"10.0.6.0/25,FR,,,x\n10.0.6.128/25,DE,,,x\n10.0.7.0/25,DE,,,x\n10.0.7.128/25,DE,,Berlin,x\n" +
		"10.0.8.0/26,DE,,,x\n10.0.9.0/24,FR,,,x\n10.0.10.0/24,FR,,Lyon,x\n" +
		"20.0.0.0/16,DE,,,x\n2001:db8::/32,DE,,Berlin,x\n30.0.0.0/24,,,,\n"
	mix := writeFile(t, filepath.Join(dir, "50-mix.csv.gz"), rows)
	l2 := writeFile(t, filepath.Join(dir, "25-bgp.csv.gz"), "prefix,country,region,city,source\n20.0.1.0/24,DE,,Hamburg,x\n")
	l3 := writeFile(t, filepath.Join(dir, "36-mlab.csv.gz"), "prefix,country,region,city,source\n20.0.2.0/24,DE,,Munich,mlab\n")
	l4 := writeFile(t, filepath.Join(dir, "70-geofeeds.csv.gz"), "prefix,country,region,city,source\n20.0.3.0/24,DE,,Berlin,feed\n")
	names := writeFile(t, filepath.Join(dir, "names.tsv"), "DE\tGermany\nFR\tFrance\n")
	flat := filepath.Join(dir, "flat.mmdb")
	if _, err := buildGeo([]string{mix, l2, l3, l4}, names, flat, false, nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	compat := filepath.Join(dir, "openipdata-org-ip2geo.mmdb")
	if _, err := convertMMDB(flat, compat, ""); err != nil {
		t.Fatal(err)
	}
	ips := writeFile(t, filepath.Join(dir, "ips.txt"),
		"10.0.0.1\n10.0.4.1\n10.0.5.9\n10.0.6.1\n10.0.7.200\n10.0.9.1\n10.0.10.1\n20.0.0.1\n20.0.1.1\n20.0.2.1\n20.0.3.1\n30.0.0.1\n99.9.9.9\n2001:db8::1\n")

	dbs := map[string]string{"flat": flat, "compat": compat}
	gaps := map[string]string{}
	for name, db := range dbs {
		out := filepath.Join(dir, name+"-gaps.txt")
		if _, err := writeGaps(db, out); err != nil {
			t.Fatalf("%s gaps: %v", name, err)
		}
		b, _ := os.ReadFile(out)
		gaps[name] = string(b)
	}
	if gaps["flat"] == "" || gaps["flat"] != gaps["compat"] {
		t.Errorf("gaps differ:\nflat:\n%s\ncompat:\n%s", gaps["flat"], gaps["compat"])
	}

	for _, mode := range []struct{ source, countries bool }{{false, false}, {true, false}, {true, true}} {
		res := map[string]string{}
		for name, db := range dbs {
			out := filepath.Join(dir, name+"-cities.tsv")
			if _, err := writeCitiesSource(db, ips, out, mode.source, mode.countries); err != nil {
				t.Fatalf("%s lookup: %v", name, err)
			}
			b, _ := os.ReadFile(out)
			res[name] = string(b)
		}
		if res["flat"] == "" || res["flat"] != res["compat"] {
			t.Errorf("mode %+v differs:\nflat:\n%s\ncompat:\n%s", mode, res["flat"], res["compat"])
		}
		if mode.source && mode.countries && !strings.Contains(res["compat"], "20.0.0.1\tDE\t\t") {
			t.Errorf("country-only row missing: %q", res["compat"])
		}
	}

	// A database_type that is neither layout is an error, for both commands.
	asDir := filepath.Join(dir, "as")
	writeASN(t, asDir, "1", `{"asn": 1, "metadata": {"handle": "ONE"}, "prefixes": {"ipv4": ["8.8.8.0/24"], "ipv6": []}}`)
	other := filepath.Join(dir, "ip2asn.mmdb")
	if _, err := buildASN(asDir, other, false); err != nil {
		t.Fatal(err)
	}
	if _, err := writeGaps(other, filepath.Join(dir, "g.txt")); err == nil || !strings.Contains(err.Error(), "database_type") {
		t.Errorf("gaps on another type: %v", err)
	}
	if _, err := writeCitiesSource(other, ips, filepath.Join(dir, "c.tsv"), false, false); err == nil || !strings.Contains(err.Error(), "database_type") {
		t.Errorf("lookup on another type: %v", err)
	}
}
