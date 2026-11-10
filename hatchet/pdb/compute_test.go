package pdb

import (
	"math"
	"os"
	"strings"
	"testing"
)

func writeDataset(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(datasetPath(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestCompute(t *testing.T) {
	dir := t.TempDir()
	writeDataset(t, dir, "org",
		`{"id": 1, "city": "San Francisco", "country": "US", "website": "https://www.Cloudflare.com/"}`,
		`{"id": 2, "city": "Berlin", "country": "DE", "website": "example.de"}`,
		`{"id": 3, "city": null, "country": "NL", "website": ""}`,
	)
	writeDataset(t, dir, "fac",
		`{"id": 10, "city": "Frankfurt", "country": "DE"}`,
		`{"id": 11, "city": " Amsterdam  ", "country": "NL"}`,
		`{"id": 12, "city": "London", "country": "GB"}`,
		`{"id": 13, "city": "Paris", "country": "FR"}`,
	)
	writeDataset(t, dir, "ix",
		`{"id": 20, "city": "Frankfurt", "country": "DE"}`,
		`{"id": 21, "city": "Amsterdam", "country": "NL"}`,
	)
	writeDataset(t, dir, "net",
		`{"id": 100, "asn": 13335, "org_id": 1, "website": "https://one.one.one.one/"}`,
		`{"id": 101, "asn": 64500, "org_id": 2, "website": ""}`,
		`{"id": 102, "asn": 64501, "org_id": 3, "website": null}`,
		`{"id": 103, "asn": 64502, "org_id": 2, "website": ""}`,
		`{"id": 104, "asn": 64503, "org_id": 1, "website": ""}`,
	)
	writeDataset(t, dir, "netfac",
		`{"id": 1, "net_id": 100, "fac_id": 10}`,
		`{"id": 2, "net_id": 103, "fac_id": 11}`,
		`{"id": 3, "net_id": 103, "fac_id": 10}`,
		`{"id": 4, "net_id": 104, "fac_id": 10}`,
		`{"id": 5, "net_id": 104, "fac_id": 11}`,
		`{"id": 6, "net_id": 104, "fac_id": 12}`,
		`{"id": 7, "net_id": 104, "fac_id": 13}`,
	)
	writeDataset(t, dir, "netixlan",
		`{"id": 1, "net_id": 100, "ix_id": 20}`,
		`{"id": 2, "net_id": 100, "ix_id": 21}`,
		`{"id": 3, "net_id": 101, "ix_id": 999}`,
	)

	rows, err := Compute(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		asn                   int64
		country, city, domain string
		dominance             float64
	}{
		// Most points in Frankfurt; the network website wins over the org website.
		{13335, "DE", "Frankfurt", "one.one.one.one", 2.0 / 3},
		// Only an unknown exchange: HQ country, no city, org website.
		{64500, "DE", "<nil>", "example.de", 0},
		// No presence points at all.
		{64501, "NL", "<nil>", "<nil>", 0},
		// Ties go to the first point seen; whitespace in city names is normalized.
		{64502, "NL", "Amsterdam", "example.de", 0.5},
		// Four cities at 25% each: below the threshold, so no city.
		{64503, "DE", "<nil>", "www.cloudflare.com", 0.25},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i, w := range want {
		r := rows[i]
		if r.ASN != w.asn || str(r.Country) != w.country || str(r.City) != w.city ||
			str(r.Domain) != w.domain || math.Abs(r.Dominance-w.dominance) > 1e-9 {
			t.Errorf("row %d = {%d %s %s %s %v}, want %+v",
				i, r.ASN, str(r.Country), str(r.City), str(r.Domain), r.Dominance, w)
		}
	}
}

func TestBestPlace(t *testing.T) {
	repeat := func(p place, n int) []place {
		out := make([]place, n)
		for i := range out {
			out[i] = p
		}
		return out
	}
	cases := []struct {
		name          string
		points        []place
		country, city string
		dominance     float64
	}{
		{
			// London is 1 of 10 located points, not 100% of the points that have a city.
			name:    "share counts every located point",
			points:  append([]place{{"GB", "London"}}, repeat(place{"GB", ""}, 9)...),
			country: "GB", dominance: 0.1,
		},
		{
			// Frankfurt has the most points overall, but the country is US: no "Frankfurt, US".
			name: "city comes from the winning country",
			points: []place{{"US", "Ashburn"}, {"US", "Dallas"}, {"US", "New York"},
				{"DE", "Frankfurt"}, {"DE", "Frankfurt"}},
			country: "US", dominance: 0.2,
		},
		{
			name:    "city above the threshold",
			points:  []place{{"US", "Ashburn"}, {"US", "Ashburn"}, {"US", "Dallas"}, {"DE", "Frankfurt"}},
			country: "US", city: "Ashburn", dominance: 0.5,
		},
		{
			name:    "points without a location fall back to HQ",
			points:  []place{{"", ""}, {"", "Somewhere"}},
			country: "NL",
		},
	}
	for _, tc := range cases {
		country, city, dominance := bestPlace(tc.points, "NL")
		if country != tc.country || city != tc.city || math.Abs(dominance-tc.dominance) > 1e-9 {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q, %v)",
				tc.name, country, city, dominance, tc.country, tc.city, tc.dominance)
		}
	}
}

func TestDomainFromWebsite(t *testing.T) {
	cases := map[string]string{
		"":                               "",
		"  ":                             "",
		"example.com":                    "example.com",
		"HTTPS://WWW.Example.COM:8443/x": "www.example.com",
		"http://bücher.example/":         "xn--bcher-kva.example",
		"https://example.org.":           "example.org",
		"http://bad host.example":        "",
		"https://under_score.example":    "",
	}
	for in, want := range cases {
		if got := domainFromWebsite(in); got != want {
			t.Errorf("domainFromWebsite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckShrink(t *testing.T) {
	cases := []struct {
		previous, rows int
		ok             bool
	}{
		{0, 0, false},
		{0, 100, true},
		{35212, 40000, true},
		{1000, 900, true},
		{1000, 899, false},
	}
	for _, c := range cases {
		if err := checkShrink("asn_geo_pdb", c.previous, c.rows); (err == nil) != c.ok {
			t.Errorf("checkShrink(%d, %d) = %v, want ok=%v", c.previous, c.rows, err, c.ok)
		}
	}
}

// A failed exchange check keeps the old geo_ix_pdb rows with a reason; it never refuses asn_geo_pdb.
func TestIXKeepReason(t *testing.T) {
	cases := []struct {
		previous, rows int
		keep           bool
	}{
		{0, 0, true},
		{500, 0, true},
		{1000, 899, true},
		{1000, 900, false},
		{0, 300, false},
	}
	for _, c := range cases {
		reason := ixKeepReason(c.previous, c.rows)
		if (reason != "") != c.keep || (c.keep && !strings.Contains(reason, "geo_ix_pdb")) {
			t.Errorf("ixKeepReason(%d, %d) = %q, want keep=%v", c.previous, c.rows, reason, c.keep)
		}
	}
	// The asn_geo_pdb check is unchanged.
	if checkShrink("asn_geo_pdb", 1000, 899) == nil {
		t.Error("asn_geo_pdb shrink accepted")
	}
}

func TestSingleCity(t *testing.T) {
	facs := map[int64]place{
		1: {"FR", "Roubaix"},
		2: {"fr", "roubaix"}, // same city, other case
		3: {"FR", "Paris"},
		4: {"US", "Paris"},
		5: {"FR", ""},
		6: {"France", "Roubaix"},
		7: {"FR", "Roubaix"},
	}
	cases := []struct {
		name          string
		ids           []int64
		count         int
		city, country string
	}{
		{"one facility", []int64{1}, 1, "Roubaix", "FR"},
		{"same city, case differs", []int64{1, 2}, 2, "Roubaix", "FR"},
		{"two facilities in one city", []int64{1, 7}, 2, "Roubaix", "FR"},
		{"a repeated link counts once", []int64{1, 1, 7}, 2, "Roubaix", "FR"},
		{"two cities", []int64{1, 3}, 2, "", ""},
		{"same name, other country", []int64{3, 4}, 2, "", ""},
		{"one facility without a city", []int64{1, 5}, 2, "", ""},
		{"country is not 2 letters", []int64{6}, 1, "", ""},
		{"unknown facility", []int64{1, 99}, 2, "", ""},
		{"no facilities", nil, 0, "", ""},
	}
	for _, c := range cases {
		count, city, country := singleCity(c.ids, facs)
		if count != c.count || city != c.city || country != c.country {
			t.Errorf("%s: got (%d, %q, %q), want (%d, %q, %q)", c.name, count, city, country, c.count, c.city, c.country)
		}
	}
}

// Compute stores the facility count and the single city; exchanges do not count.
func TestComputeFacilityCity(t *testing.T) {
	dir := t.TempDir()
	writeDataset(t, dir, "org", `{"id": 1, "country": "DE"}`)
	writeDataset(t, dir, "fac",
		`{"id": 10, "city": "Roubaix", "country": "FR"}`,
		`{"id": 11, "city": " ROUBAIX ", "country": "FR"}`,
		`{"id": 12, "city": "Paris", "country": "FR"}`,
	)
	writeDataset(t, dir, "ix", `{"id": 20, "city": "Frankfurt", "country": "DE"}`)
	writeDataset(t, dir, "net",
		`{"id": 100, "asn": 64500, "org_id": 1, "info_type": "Content", "info_scope": "Europe"}`,
		`{"id": 101, "asn": 64501, "org_id": 1}`,
		`{"id": 102, "asn": 64502, "org_id": 1}`,
	)
	writeDataset(t, dir, "netfac",
		`{"net_id": 100, "fac_id": 10}`, `{"net_id": 100, "fac_id": 11}`,
		`{"net_id": 101, "fac_id": 10}`, `{"net_id": 101, "fac_id": 12}`,
	)
	writeDataset(t, dir, "netixlan", `{"net_id": 100, "ix_id": 20}`, `{"net_id": 102, "ix_id": 20}`)

	rows, err := Compute(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		count         int
		city, country string
	}{{2, "Roubaix", "FR"}, {2, "<nil>", "<nil>"}, {0, "<nil>", "<nil>"}}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows", len(rows))
	}
	for i, w := range want {
		r := rows[i]
		if r.FacCount != w.count || str(r.FacCity) != w.city || str(r.FacCountry) != w.country {
			t.Errorf("row %d = (%d, %s, %s), want %+v", i, r.FacCount, str(r.FacCity), str(r.FacCountry), w)
		}
	}
	// The exchange in Frankfurt still counts for the dominant city: unchanged.
	if str(rows[0].InfoType) != "Content" {
		t.Errorf("info_type = %s", str(rows[0].InfoType))
	}
}
