package geo

import (
	"encoding/csv"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The case table is the one of TestRegionAdmin1 in mmdbbuilder/cityregion_test.go (feedState must
// equal regionAdmin1 there): the two tables must stay identical.
func TestFeedState(t *testing.T) {
	for _, c := range []struct{ country, region, want string }{
		{"US", "US-ME", "ME"},
		{"us", " us-me ", "ME"},
		{"US", "me", "ME"},
		{"US", "ME", "ME"},
		{"US", "", ""},
		{"US", "US-", ""},
		{"US", "US-MEE", ""},
		{"US", "M1", ""},
		{"US", "Maine", ""},
		{"US", "-ME", ""},
		{"US", "US-É1", ""},
		{"IT", "IT-25", ""}, // another country: never guessed
		{"IT", "MI", ""},
		{"GB", "GB-ENG", ""},
		{"US", "US-CT", "CT"},
		{"US", "CT", "CT"},
		{"US", " us-ct ", "CT"},
		{"us", "ct", "CT"},
		{"US", "US-US", "US"},
		{"US", "US-C", ""},
		{"US", "US-CTX", ""},
		{"US", "US-C1", ""},
		{"US", "1T", ""},
		{"US", "US-CÉ", ""},
		{"US", "CA-ON", ""},
		{"US", "US-US-CT", ""},
		{"CA", "CA-ON", ""},
		{"CA", "ON", ""},
		{"DE", "US-CT", ""},
		{"", "CT", ""},
	} {
		if got := feedState(c.country, c.region); got != c.want {
			t.Errorf("feedState(%q, %q) = %q, want %q", c.country, c.region, got, c.want)
		}
	}
}

func TestTopState(t *testing.T) {
	ct, ny := [2]byte{'C', 'T'}, [2]byte{'N', 'Y'}
	for _, c := range []struct {
		states map[[2]byte]uint64
		whole  uint64
		want   string
	}{
		{map[[2]byte]uint64{ct: 80}, 100, "CT"},
		{map[[2]byte]uint64{ct: 79}, 100, ""},
		{map[[2]byte]uint64{ct: 40, ny: 40}, 50, "CT"}, // a tie is the smaller code
		{map[[2]byte]uint64{ct: 40, ny: 40}, 100, ""},
		{nil, 100, ""},
		{map[[2]byte]uint64{ct: 5}, 0, ""},
	} {
		if got := topState(c.states, c.whole); got != c.want {
			t.Errorf("topState(%v, %d) = %q, want %q", c.states, c.whole, got, c.want)
		}
	}
}

func TestASNFeedState(t *testing.T) {
	a := ann(64500, "10.0.0.0/19")
	a4 := ann(64500, "10.0.0.0/22") // four /24s cover it
	// rowsOf is one /24 row per index of one city, all with the same region.
	rowsOf := func(city, region string, from, to int) string {
		var b strings.Builder
		for i := from; i < to; i++ {
			fmt.Fprintf(&b, "10.0.%d.0/24,US,%s,%s,f\n", i, region, city)
		}
		return b.String()
	}
	// All rows name the state: every row of the ASN has it, and the counter says one decision.
	got, asns, n := runASNFeed(t, rowsOf("Madison", "US-CT", 0, 4), a4)
	if got != "10.0.0.0/22,US,US-CT,Madison,asn:64500\n" || asns != 1 || n != 1 {
		t.Errorf("all rows: %q %d %d", got, asns, n)
	}
	// The same rows with no region give the same row with an empty region column.
	got, _, _ = runASNFeed(t, rowsOf("Madison", "", 0, 4), a4)
	if got != "10.0.0.0/22,US,,Madison,asn:64500\n" {
		t.Errorf("no region: %q", got)
	}
	// A bare code, any case, is read like US-XX.
	got, _, _ = runASNFeed(t, rowsOf("Madison", "ct", 0, 4), a4)
	if got != "10.0.0.0/22,US,US-CT,Madison,asn:64500\n" {
		t.Errorf("bare code: %q", got)
	}
	// 16 of 20 /24s (80%) name the state; 15 of 19 (78.9%) do not. The rows without a state count.
	got, _, _ = runASNFeed(t, rowsOf("Madison", "US-CT", 0, 16)+rowsOf("Madison", "", 16, 20), a)
	if got != "10.0.0.0/19,US,US-CT,Madison,asn:64500\n" {
		t.Errorf("80%%: %q", got)
	}
	got, _, _ = runASNFeed(t, rowsOf("Madison", "US-CT", 0, 15)+rowsOf("Madison", "", 15, 19), a)
	if got != "10.0.0.0/19,US,,Madison,asn:64500\n" {
		t.Errorf("78.9%%: %q", got)
	}
	// Two states split the city: no state.
	got, _, _ = runASNFeed(t, rowsOf("Madison", "US-CT", 0, 8)+rowsOf("Madison", "US-WI", 8, 16), a)
	if got != "10.0.0.0/19,US,,Madison,asn:64500\n" {
		t.Errorf("split: %q", got)
	}
	// The spellings of one city add up (the city identity is unchanged), and a region that is not a
	// state is no state.
	got, _, _ = runASNFeed(t, rowsOf("Madison", "US-CT", 0, 8)+rowsOf("MADISON", "US-CT", 8, 16)+rowsOf("Madison", "US-12", 16, 18), a)
	if got != "10.0.0.0/19,US,US-CT,Madison,asn:64500\n" {
		t.Errorf("spellings: %q", got)
	}
	// A state on a city that is not the dominant one is ignored, and so is one on a row with no city.
	rows := rowsOf("Madison", "", 0, 16) + rowsOf("Boston", "US-MA", 16, 18) + "10.0.20.0/24,US,US-NY,,f\n"
	got, _, _ = runASNFeed(t, rows, a)
	if got != "10.0.0.0/19,US,,Madison,asn:64500\n" {
		t.Errorf("other city: %q", got)
	}
	// Outside the US a region is never read.
	got, _, _ = runASNFeed(t, "10.0.0.0/24,DE,US-CT,Berlin,f\n10.0.1.0/24,DE,US-CT,Berlin,f\n", ann(64500, "10.0.0.0/22"))
	if got != "10.0.0.0/22,DE,,Berlin,asn:64500\n" {
		t.Errorf("non-US: %q", got)
	}
	// The counter counts decisions with a state: two ASNs, one with a state.
	two := rowsOf("Madison", "US-CT", 0, 4) + "10.1.0.0/24,US,,Austin,f\n10.1.1.0/24,US,,Austin,f\n"
	got, asns, _ = runASNFeed(t, two, append(ann(64500, "10.0.0.0/22"), ann(64501, "10.1.0.0/23")...))
	if got != "10.0.0.0/22,US,US-CT,Madison,asn:64500\n10.1.0.0/23,US,,Austin,asn:64501\n" || asns != 2 {
		t.Errorf("two ASNs: %q %d", got, asns)
	}
}

func TestASNFeedStateCounter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.csv.gz")
	writeGzip(t, path, feedHeader+
		"10.0.0.0/24,US,US-CT,Madison,f\n10.0.1.0/24,US,US-CT,Madison,f\n"+
		"10.1.0.0/24,US,,Austin,f\n10.1.1.0/24,US,,Austin,f\n")
	all := append(ann(1, "10.0.0.0/23"), ann(2, "10.1.0.0/23")...)
	var asns, n, states int
	layerRows(t, func(w *csv.Writer) error {
		var err error
		asns, n, states, err = asnFeedRows(path, all, w)
		return err
	})
	if asns != 2 || n != 2 || states != 1 {
		t.Errorf("asns %d rows %d states %d", asns, n, states)
	}
}

func TestASNFeedStateIPv6(t *testing.T) {
	a := ann(64500, "2001:db8::/43") // 32 /48s
	rowsOf := func(region string, from, to int) string {
		var b strings.Builder
		for i := from; i < to; i++ {
			fmt.Fprintf(&b, "2001:db8:%x::/48,US,%s,Madison,f\n", i, region)
		}
		return b.String()
	}
	got, _, _ := runASNFeed(t, rowsOf("US-CT", 0, 16)+rowsOf("", 16, 20), a)
	if got != "2001:db8::/43,US,US-CT,Madison,asn:64500\n" {
		t.Errorf("80%%: %q", got)
	}
	got, _, _ = runASNFeed(t, rowsOf("US-CT", 0, 15)+rowsOf("", 15, 19), a)
	if got != "2001:db8::/43,US,,Madison,asn:64500\n" {
		t.Errorf("78.9%%: %q", got)
	}
	// The two families are decided on their own.
	both := append(ann(64500, "10.0.0.0/22"), a...)
	got, _, _ = runASNFeed(t, "10.0.0.0/24,US,,Madison,f\n10.0.1.0/24,US,,Madison,f\n"+rowsOf("US-CT", 0, 20), both)
	if got != "10.0.0.0/22,US,,Madison,asn:64500\n2001:db8::/43,US,US-CT,Madison,asn:64500\n" {
		t.Errorf("families: %q", got)
	}
}

// feedFileRegion writes a geofeed layer file from rows "prefix,country,region,city".
func feedFileRegion(t *testing.T, rows ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(feedHeader)
	for _, r := range rows {
		parts := strings.SplitN(r, ",", 4)
		fmt.Fprintf(&b, "%s,%s,%s,%s,https://feed0.example/geo.csv\n", parts[0], parts[1], parts[2], parts[3])
	}
	path := filepath.Join(t.TempDir(), "feeds.csv.gz")
	writeGzip(t, path, b.String())
	return path
}

func TestCloudRegionsState(t *testing.T) {
	data := regionJSON(
		[2]string{"52.0.0.0/16", "us-west-2"},
		[2]string{"54.0.0.0/16", "us-west-2"},
		[2]string{"13.0.0.0/16", "us-east-1"},
	)
	// Every row of the main city names the state: every range of the label has it.
	feeds := feedFileRegion(t,
		"52.0.0.0/20,US,US-OR,Boardman",
		"54.0.0.0/20,US,or,Boardman",
		"13.0.0.0/20,US,US-VA,Ashburn",
	)
	rows, st := cloudLayer(t, data, feeds)
	want := "13.0.0.0/16,US,US-VA,Ashburn,cloud-regions:aws:us-east-1\n" +
		"52.0.0.0/16,US,US-OR,Boardman,cloud-regions:aws:us-west-2\n" +
		"54.0.0.0/16,US,US-OR,Boardman,cloud-regions:aws:us-west-2\n"
	if rows != want || st.Qualified != 2 || st.States != 2 || st.Rows != 3 {
		t.Errorf("rows:\n%s\nstats %+v", rows, st)
	}

	// No region at all: the same rows with an empty region column.
	rows, st = cloudLayer(t, data, feedFile(t, "52.0.0.0/20,US,Boardman", "54.0.0.0/20,US,Boardman", "13.0.0.0/20,US,Ashburn"))
	want = "13.0.0.0/16,US,,Ashburn,cloud-regions:aws:us-east-1\n" +
		"52.0.0.0/16,US,,Boardman,cloud-regions:aws:us-west-2\n" +
		"54.0.0.0/16,US,,Boardman,cloud-regions:aws:us-west-2\n"
	if rows != want || st.States != 0 {
		t.Errorf("no region:\n%s\nstats %+v", rows, st)
	}

	one := regionJSON([2]string{"52.0.0.0/16", "us-west-2"})
	// The share is counted over the city's whole weight in the label: 4 of 5 /20s name the state
	// (80%), and a second city does not enter into it.
	rows, _ = cloudLayer(t, one, feedFileRegion(t,
		"52.0.0.0/20,US,US-OR,Boardman", "52.0.16.0/20,US,US-OR,Boardman", "52.0.32.0/20,US,US-OR,Boardman",
		"52.0.48.0/20,US,US-OR,Boardman", "52.0.64.0/20,US,,Boardman", "52.0.80.0/20,US,US-WA,Reston"))
	if rows != "52.0.0.0/16,US,US-OR,Boardman,cloud-regions:aws:us-west-2\n" {
		t.Errorf("80%% of the city:\n%s", rows)
	}
	// 3 of 4 (75%) is not enough, though the label's own main-city share (4 of 5) qualifies it.
	rows, _ = cloudLayer(t, one, feedFileRegion(t,
		"52.0.0.0/20,US,US-OR,Boardman", "52.0.16.0/20,US,US-OR,Boardman", "52.0.32.0/20,US,US-OR,Boardman",
		"52.0.48.0/20,US,,Boardman", "52.0.64.0/20,US,US-OR,Reston"))
	if rows != "52.0.0.0/16,US,,Boardman,cloud-regions:aws:us-west-2\n" {
		t.Errorf("75%% of the city:\n%s", rows)
	}
	// Two states split the city: no state.
	rows, _ = cloudLayer(t, one, feedFileRegion(t, "52.0.0.0/20,US,US-OR,Boardman", "52.0.16.0/20,US,US-WA,Boardman"))
	if rows != "52.0.0.0/16,US,,Boardman,cloud-regions:aws:us-west-2\n" {
		t.Errorf("split:\n%s", rows)
	}
	// A state on another city, or on a row with no city, or outside the US, is ignored.
	rows, _ = cloudLayer(t, one, feedFileRegion(t,
		"52.0.0.0/20,US,,Boardman", "52.0.16.0/20,US,,Boardman", "52.0.32.0/20,US,,Boardman", "52.0.48.0/20,US,,Boardman",
		"52.0.64.0/20,US,US-WA,Reston", "52.0.80.0/20,US,US-OR,"))
	if rows != "52.0.0.0/16,US,,Boardman,cloud-regions:aws:us-west-2\n" {
		t.Errorf("other rows:\n%s", rows)
	}
	rows, _ = cloudLayer(t, one, feedFileRegion(t, "52.0.0.0/20,DE,US-OR,Berlin"))
	if rows != "52.0.0.0/16,DE,,Berlin,cloud-regions:aws:us-west-2\n" {
		t.Errorf("non-US:\n%s", rows)
	}
}

// The 25-bgp layer reads the same feed rows and must not change when they carry a region.
func TestBGPRowsIgnoreRegion(t *testing.T) {
	comm := "10.0.0.0/24,100:1\n10.0.1.0/24,100:1\n10.0.2.0/24,100:1\n10.0.9.0/24,100:1 5:5\n"
	plain := "10.0.0.0/24,US,,Madison,f\n10.0.1.0/24,US,,Madison,f\n10.0.2.0/24,US,,Madison,f\n"
	regional := "10.0.0.0/24,US,US-CT,Madison,f\n10.0.1.0/24,US,US-CT,Madison,f\n10.0.2.0/24,US,US-CT,Madison,f\n"
	want, st := bgpLayerOf(t, comm, plain)
	got, st2 := bgpLayerOf(t, comm, regional)
	if got != want || st != st2 || !strings.Contains(got, "10.0.9.0/24,US,,Madison,bgp\n") {
		t.Errorf("with regions:\n%s\nwithout:\n%s", got, want)
	}
}
