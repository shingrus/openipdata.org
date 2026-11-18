package geo

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The prefixes below are real-looking public ones (the public-address check is not replaced);
// the cities are made up where it matters.

// regionJSON makes a region file of the given (prefix, region) entries plus 1000 filler /24s of
// the region "filler-1" (3.0.0.0 to 3.3.231.0), so that it has enough valid entries.
func regionJSON(entries ...[2]string) []byte {
	var three [][3]string
	for _, e := range entries {
		three = append(three, [3]string{e[0], e[1], ""})
	}
	return regionJSON3(three...)
}

// regionJSON3 is regionJSON with a network_border_group per entry (empty: the field is absent).
func regionJSON3(entries ...[3]string) []byte {
	var b strings.Builder
	b.WriteString(`{"syncToken":"1","createDate":"2026-11-18-00-00-00","prefixes":[`)
	first := true
	add := func(prefix, region, border string) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		if border == "" {
			fmt.Fprintf(&b, `{"ip_prefix":%q,"region":%q,"service":"AMAZON"}`, prefix, region)
			return
		}
		fmt.Fprintf(&b, `{"ip_prefix":%q,"region":%q,"service":"AMAZON","network_border_group":%q}`, prefix, region, border)
	}
	for _, e := range entries {
		add(e[0], e[1], e[2])
	}
	for i := 0; i < cloudRegionsMinPrefixes; i++ {
		add(fmt.Sprintf("3.%d.%d.0/24", i/256, i%256), "filler-1", "filler-1")
	}
	b.WriteString(`],"ipv6_prefixes":[{"ipv6_prefix":"2600:1f00::/40","region":"us-east-1","service":"AMAZON"}]}`)
	return []byte(b.String())
}

// feedFile writes a geofeed layer file from rows "prefix,country,city" and returns its path.
func feedFile(t *testing.T, rows ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(feedHeader)
	for _, r := range rows {
		parts := strings.SplitN(r, ",", 3)
		fmt.Fprintf(&b, "%s,%s,,%s,https://feed0.example/geo.csv\n", parts[0], parts[1], parts[2])
	}
	path := filepath.Join(t.TempDir(), "feeds.csv.gz")
	writeGzip(t, path, b.String())
	return path
}

// cloudLayer runs the layer and returns the rows without the header.
func cloudLayer(t *testing.T, data []byte, feeds string) (string, cloudStats) {
	t.Helper()
	var st cloudStats
	out := layerRows(t, func(w *csv.Writer) error {
		var err error
		st, err = cloudRegionRows(data, feeds, w)
		return err
	})
	return strings.TrimPrefix(out, feedHeader), st
}

func TestParseCloudRegions(t *testing.T) {
	data := regionJSON(
		[2]string{"52.0.0.0/16", "EU-West-1"},             // valid, case folded
		[2]string{"54.0.0.0/16", "GLOBAL"},                // valid, the pseudo region
		[2]string{"52.1.0.0/16", "eu_west_1"},             // bad region
		[2]string{"52.2.0.0/16", ""},                      // bad region
		[2]string{"52.3.0.0/16", strings.Repeat("a", 49)}, // bad region (too long)
		[2]string{"52.4.0.1/16", "x"},                     // host bits
		[2]string{"52.0.0.0/7", "x"},                      // broader than /8
		[2]string{"10.0.0.0/8", "x"},                      // not public
		[2]string{"192.0.2.0/24", "x"},                    // documentation
		[2]string{"2600:1f00::/40", "x"},                  // IPv6 in ip_prefix
		[2]string{"nonsense", "x"},                        // no prefix
		[2]string{"52.5.0.0/32", strings.Repeat("a", 48)}, // valid: /32, 48 characters
	)
	got, st, err := parseCloudRegions(data)
	if err != nil {
		t.Fatal(err)
	}
	if st.Entries != 12+cloudRegionsMinPrefixes || st.Valid != 3+cloudRegionsMinPrefixes || st.Bad != 9 {
		t.Errorf("stats %+v", st)
	}
	if got[0].region != "eu-west-1" || got[1].region != "global" || got[0].bits != 16 || got[0].addr != 52<<24 {
		t.Errorf("first entries %+v", got[:2])
	}

	// Too few valid entries: not the file.
	few := `{"prefixes":[{"ip_prefix":"52.0.0.0/16","region":"a"}]}`
	if _, _, err := parseCloudRegions([]byte(few)); err == nil {
		t.Error("no error for too few entries")
	} else if _, ok := err.(*cloudInputError); !ok {
		t.Errorf("error type %T", err)
	}
	for _, bad := range []string{"", "not json", `{"prefixes":"x"}`, `{}`, `[]`} {
		if _, _, err := parseCloudRegions([]byte(bad)); err == nil {
			t.Errorf("no error for %q", bad)
		}
	}
	// More than the limit of entries.
	entry := `{"ip_prefix":"52.0.0.0/16","region":"a"},`
	huge := `{"prefixes":[` + strings.Repeat(entry, cloudRegionsMaxEntries+1) + `{"ip_prefix":"52.1.0.0/16","region":"a"}]}`
	if _, _, err := parseCloudRegions([]byte(huge)); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("huge: %v", err)
	}
	// Exactly the limit is accepted.
	atLimit := `{"prefixes":[` + strings.Repeat(entry, cloudRegionsMaxEntries-1) + `{"ip_prefix":"52.1.0.0/16","region":"a"}]}`
	if _, st, err := parseCloudRegions([]byte(atLimit)); err != nil || st.Entries != cloudRegionsMaxEntries {
		t.Errorf("at the limit: %+v %v", st, err)
	}
}

func TestCloudSegments(t *testing.T) {
	r := func(prefix, region string) cloudRange {
		a, b := parseTestPrefix(prefix)
		return cloudRange{addr: a, bits: b, region: region}
	}
	ranges := []cloudRange{
		r("13.0.0.0/16", "a"),
		r("13.0.5.0/24", "b"),      // nested: the most specific decides
		r("13.0.6.0/24", "global"), // a hole in a
		r("18.0.0.0/16", "c"),      // the same prefix twice: the first one
		r("18.0.0.0/16", "d"),
		r("18.1.0.0/16", "global"), // global never wins over a real region
		r("18.1.0.0/16", "e"),
		r("18.2.0.0/16", "global"), // global only: no row
		r("20.0.0.0/17", "a"),      // adjacent ranges of one region are NOT merged: a row
		r("20.0.128.0/17", "a"),    // never gets broader than an entry of the file
		r("20.1.0.0/16", "b"),      // adjacent but another region
	}
	segs, names := cloudSegments(ranges)
	if want := []string{"a", "b", "c", "e"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v", names)
	}
	type seg struct {
		first, last string
		region      string
	}
	var got []seg
	for _, s := range segs {
		got = append(got, seg{addrFrom32(s.start).String(), addrFrom32(s.end).String(), names[s.region]})
	}
	want := []seg{
		{"13.0.0.0", "13.0.4.255", "a"},
		{"13.0.5.0", "13.0.5.255", "b"},
		{"13.0.7.0", "13.0.255.255", "a"},
		{"18.0.0.0", "18.0.255.255", "c"},
		{"18.1.0.0", "18.1.255.255", "e"},
		{"20.0.0.0", "20.0.127.255", "a"},
		{"20.0.128.0", "20.0.255.255", "a"},
		{"20.1.0.0", "20.1.255.255", "b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("segments:\n got %v\nwant %v", got, want)
	}
	// The same input in another order gives the same result. The permutation keeps the relative
	// order of equal prefixes (equal prefixes keep the order of the file).
	order := []int{10, 8, 0, 9, 3, 4, 1, 5, 6, 2, 7}
	shuffled := make([]cloudRange, 0, len(ranges))
	for _, i := range order {
		shuffled = append(shuffled, ranges[i])
	}
	s2, n2 := cloudSegments(shuffled)
	if !reflect.DeepEqual(s2, segs) || !reflect.DeepEqual(n2, names) {
		t.Errorf("another order gives another result: %v", s2)
	}
}

func parseTestPrefix(s string) (uint32, uint8) {
	var a, b, c, d, n int
	fmt.Sscanf(s, "%d.%d.%d.%d/%d", &a, &b, &c, &d, &n)
	return uint32(a)<<24 | uint32(b)<<16 | uint32(c)<<8 | uint32(d), uint8(n)
}

func TestCloudRegionsLearnAndRows(t *testing.T) {
	data := regionJSON(
		[2]string{"52.0.0.0/16", "eu-west-1"},
		[2]string{"54.0.0.0/16", "eu-west-1"},
		[2]string{"13.0.0.0/16", "us-east-1"},
		[2]string{"18.0.0.0/16", "xx-nothing-1"}, // no geofeed row: no city
	)
	feeds := feedFile(t,
		"52.0.0.0/20,IE,Dublin",
		"13.0.0.0/20,US,Ashburn",
	)
	rows, st := cloudLayer(t, data, feeds)
	want := "13.0.0.0/16,US,,Ashburn,cloud-regions:aws:us-east-1\n" +
		"52.0.0.0/16,IE,,Dublin,cloud-regions:aws:eu-west-1\n" +
		"54.0.0.0/16,IE,,Dublin,cloud-regions:aws:eu-west-1\n"
	if rows != want {
		t.Errorf("rows:\n%s", rows)
	}
	// 4 real regions plus filler-1; xx-nothing-1 and filler-1 do not qualify.
	if st.Seen != 4 || st.Qualified != 2 || st.Rows != 3 {
		t.Errorf("stats %+v", st)
	}
}

// A hole of the pseudo region gets no row and its addresses do not count for the region.
func TestCloudRegionsGlobalHole(t *testing.T) {
	data := regionJSON([2]string{"13.0.0.0/16", "us-east-1"}, [2]string{"13.0.4.0/22", "GLOBAL"})
	rows, st := cloudLayer(t, data, feedFile(t, "13.0.0.0/16,US,Ashburn"))
	want := "13.0.0.0/22,US,,Ashburn,cloud-regions:aws:us-east-1\n" +
		"13.0.8.0/21,US,,Ashburn,cloud-regions:aws:us-east-1\n" +
		"13.0.16.0/20,US,,Ashburn,cloud-regions:aws:us-east-1\n" +
		"13.0.32.0/19,US,,Ashburn,cloud-regions:aws:us-east-1\n" +
		"13.0.64.0/18,US,,Ashburn,cloud-regions:aws:us-east-1\n" +
		"13.0.128.0/17,US,,Ashburn,cloud-regions:aws:us-east-1\n"
	if rows != want || st.Rows != 6 {
		t.Errorf("rows:\n%s", rows)
	}
	// Only the hole carries the feed city: nothing is left to learn from.
	rows, _ = cloudLayer(t, data, feedFile(t, "13.0.4.0/22,US,Ashburn"))
	if rows != "" {
		t.Errorf("hole counted: %s", rows)
	}
}

// The addresses that a geofeed row owns are its prefix minus the more specific rows inside it.
func TestCloudRegionsOwnedAddresses(t *testing.T) {
	data := regionJSON([2]string{"13.0.0.0/16", "us-east-1"})
	// A /16 of Ashburn with a /18 of Reston inside: Ashburn owns 75%, below the share.
	rows, _ := cloudLayer(t, data, feedFile(t, "13.0.0.0/16,US,Ashburn", "13.0.0.0/18,US,Reston"))
	if rows != "" {
		t.Errorf("75%% qualified: %s", rows)
	}
	// With a /19 inside it owns 87.5%.
	rows, _ = cloudLayer(t, data, feedFile(t, "13.0.0.0/16,US,Ashburn", "13.0.0.0/19,US,Reston"))
	if !strings.Contains(rows, ",US,,Ashburn,") {
		t.Errorf("87.5%% did not qualify: %s", rows)
	}
	// A repeated prefix: the last row counts, the first is ignored.
	rows, _ = cloudLayer(t, data, feedFile(t, "13.0.0.0/16,US,Reston", "13.0.0.0/16,US,Ashburn"))
	if !strings.Contains(rows, ",US,,Ashburn,") || strings.Contains(rows, "Reston") {
		t.Errorf("repeated prefix: %s", rows)
	}
	// A row without a city owns its addresses too (here the /19 hides nothing of the /20 that
	// follows it), but it counts for no city: the share is taken over the addresses that have a
	// city, so the /20 of Ashburn alone is 100% and the region qualifies.
	rows, _ = cloudLayer(t, data, feedFile(t, "13.0.0.0/19,US,", "13.0.32.0/20,US,Ashburn"))
	if !strings.Contains(rows, ",US,,Ashburn,") {
		t.Errorf("city-less row: %s", rows)
	}
}

// A geofeed row that spans two regions counts for each region with the part inside it.
func TestCloudRegionsRowSpansRegions(t *testing.T) {
	data := regionJSON([2]string{"13.0.0.0/16", "r-one"}, [2]string{"13.1.0.0/16", "r-two"}, [2]string{"13.2.0.0/16", "r-three"})
	rows, st := cloudLayer(t, data, feedFile(t, "13.0.0.0/15,IE,Dublin"))
	want := "13.0.0.0/16,IE,,Dublin,cloud-regions:aws:r-one\n13.1.0.0/16,IE,,Dublin,cloud-regions:aws:r-two\n"
	if rows != want || st.Qualified != 2 {
		t.Errorf("rows:\n%s%+v", rows, st)
	}
}

func TestCloudRegionsSpelling(t *testing.T) {
	data := regionJSON([2]string{"13.0.0.0/16", "r-one"})
	// The most common spelling wins (by rows); case and spacing do not split the city.
	rows, _ := cloudLayer(t, data, feedFile(t,
		"13.0.0.0/20,IE,dublin", "13.0.16.0/20,IE,Dublin", "13.0.32.0/20,IE, Dublin ", "13.0.48.0/20,IE,DUBLIN  "))
	if !strings.Contains(rows, ",IE,,Dublin,") {
		t.Errorf("rows: %s", rows)
	}
	// A tie: the smaller string.
	rows, _ = cloudLayer(t, data, feedFile(t, "13.0.0.0/20,IE,dublin", "13.0.16.0/20,IE,Dublin"))
	if !strings.Contains(rows, ",IE,,Dublin,") {
		t.Errorf("tie: %s", rows)
	}
	// A form in capitals does not decide the spelling, even when it is the most common one;
	// when every form is in capitals it is used.
	rows, _ = cloudLayer(t, data, feedFile(t, "13.0.0.0/20,IE,DUBLIN", "13.0.16.0/20,IE,DUBLIN", "13.0.32.0/20,IE,DUBLIN", "13.0.48.0/20,IE,Dublin"))
	if !strings.Contains(rows, ",IE,,Dublin,") {
		t.Errorf("capitals: %s", rows)
	}
	rows, _ = cloudLayer(t, data, feedFile(t, "13.0.0.0/20,IE,DUBLIN", "13.0.16.0/20,IE,DUBLIN"))
	if !strings.Contains(rows, ",IE,,DUBLIN,") {
		t.Errorf("only capitals: %s", rows)
	}
	// The same name in another country is another city.
	rows, _ = cloudLayer(t, data, feedFile(t, "13.0.0.0/20,US,Dublin", "13.0.16.0/20,IE,Dublin", "13.0.32.0/20,IE,Dublin", "13.0.48.0/20,IE,Dublin", "13.0.64.0/20,IE,Dublin"))
	if !strings.Contains(rows, ",IE,,Dublin,") || strings.Contains(rows, ",US,") {
		t.Errorf("namesake: %s", rows)
	}
}

func TestCloudRegionsThresholds(t *testing.T) {
	data := regionJSON([2]string{"13.0.0.0/16", "us-east-1"})
	// 4095 addresses: one short. The last two prefixes of the list end at 13.0.15.254.
	small := []string{"13.0.0.0/21", "13.0.8.0/22", "13.0.12.0/23", "13.0.14.0/24", "13.0.15.0/25", "13.0.15.128/26",
		"13.0.15.192/27", "13.0.15.224/28", "13.0.15.240/29", "13.0.15.248/30", "13.0.15.252/31", "13.0.15.254/32"}
	var rows []string
	for _, p := range small {
		rows = append(rows, p+",US,Ashburn")
	}
	if out, _ := cloudLayer(t, data, feedFile(t, rows...)); out != "" {
		t.Errorf("4095 addresses qualified: %s", out)
	}
	rows = append(rows, "13.0.15.255/32,US,Ashburn")
	if out, _ := cloudLayer(t, data, feedFile(t, rows...)); out == "" {
		t.Error("4096 addresses did not qualify")
	}

	// The share: five /20s, four in one city is exactly 80%; three is 60%.
	four := feedFile(t, "13.0.0.0/20,US,Ashburn", "13.0.16.0/20,US,Ashburn", "13.0.32.0/20,US,Ashburn", "13.0.48.0/20,US,Ashburn", "13.0.64.0/20,US,Reston")
	if out, _ := cloudLayer(t, data, four); !strings.Contains(out, "Ashburn") {
		t.Errorf("80%% did not qualify: %s", out)
	}
	three := feedFile(t, "13.0.0.0/20,US,Ashburn", "13.0.16.0/20,US,Ashburn", "13.0.32.0/20,US,Ashburn", "13.0.48.0/20,US,Reston", "13.0.64.0/20,US,Reston")
	if out, _ := cloudLayer(t, data, three); out != "" {
		t.Errorf("60%% qualified: %s", out)
	}
	// Just under: 79 of 100 /24-blocks... 4 of 5 is the line; 16 of 20 (80%) passes, 15 of 19 (78.9%) fails.
	var r80, r79 []string
	for i := 0; i < 16; i++ {
		r80 = append(r80, fmt.Sprintf("13.0.%d.0/24,US,Ashburn", i))
	}
	for i := 16; i < 20; i++ {
		r80 = append(r80, fmt.Sprintf("13.0.%d.0/24,US,Reston", i))
	}
	for i := 0; i < 15; i++ {
		r79 = append(r79, fmt.Sprintf("13.0.%d.0/24,US,Ashburn", i))
	}
	for i := 15; i < 19; i++ {
		r79 = append(r79, fmt.Sprintf("13.0.%d.0/24,US,Reston", i))
	}
	// 20 /24s are 5120 addresses and 19 are 4864: both are above the minimum.
	if out, _ := cloudLayer(t, data, feedFile(t, r80...)); !strings.Contains(out, "Ashburn") {
		t.Errorf("16 of 20: %s", out)
	}
	if out, _ := cloudLayer(t, data, feedFile(t, r79...)); out != "" {
		t.Errorf("15 of 19: %s", out)
	}
}

func TestCloudRegionRowsErrors(t *testing.T) {
	feeds := feedFile(t, "13.0.0.0/20,US,Ashburn")
	err := func(data []byte, feeds string) error {
		_, e := layerRowsErr(t, func(w *csv.Writer) error { _, e := cloudRegionRows(data, feeds, w); return e })
		return e
	}
	if e := err([]byte("nope"), feeds); e == nil {
		t.Error("no error for a bad region file")
	} else if _, ok := e.(*cloudInputError); !ok {
		t.Errorf("type %T", e)
	}
	// A broken geofeed layer file is a local error, not an input error of the region file.
	e := err(regionJSON(), filepath.Join(t.TempDir(), "missing.csv.gz"))
	if e == nil {
		t.Fatal("no error for a missing feed file")
	}
	if _, ok := e.(*cloudInputError); ok {
		t.Error("a feed file problem is a region file problem")
	}
}

func layerRowsErr(t *testing.T, fill func(w *csv.Writer) error) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "layer.csv.gz")
	_, err := writeLayer(path, fill)
	return path, err
}

// regionServer serves a region file and can be switched to fail.
type regionServer struct {
	*httptest.Server
	body []byte
	fail bool
}

func newRegionServer(body []byte) *regionServer {
	s := &regionServer{body: body}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Write(s.body)
	}))
	return s
}

func cloudPipeline(t *testing.T, url string, feedRows ...string) (*Pipeline, string, string) {
	t.Helper()
	p := &Pipeline{DataDir: t.TempDir(), cloudRegionsURL: url}
	layers, downloads := p.layersDir(), filepath.Join(p.dir(), "downloads")
	os.MkdirAll(layers, 0o750)
	os.MkdirAll(downloads, 0o750)
	if feedRows != nil {
		var b strings.Builder
		b.WriteString(feedHeader)
		for _, r := range feedRows {
			parts := strings.SplitN(r, ",", 3)
			fmt.Fprintf(&b, "%s,%s,,%s,f\n", parts[0], parts[1], parts[2])
		}
		writeGzip(t, filepath.Join(layers, FeedsLayer), b.String())
	}
	return p, layers, downloads
}

func newCloudSource() *Source {
	return &Source{Rows: map[string]int{}, R2Uploaded: map[string]string{}}
}

func skippedOnly(t *testing.T, src *Source, why string) {
	t.Helper()
	if !src.skipped(CloudRegionsLayer) || len(src.StaleLayers) != 0 {
		t.Errorf("%s: skipped %v stale %v", why, src.SkippedLayers, src.StaleLayers)
	}
	if src.Rows[CloudRegionsLayer] != 0 || src.CloudRegions != 0 {
		t.Errorf("%s: rows %v regions %d", why, src.Rows, src.CloudRegions)
	}
}

func TestDownloadCloudRegions(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	srv := newRegionServer(regionJSON([2]string{"52.0.0.0/16", "eu-west-1"}, [2]string{"13.0.0.0/16", "us-east-1"}))
	defer srv.Close()
	client := newHTTPClient()

	p, layers, downloads := cloudPipeline(t, srv.URL, "52.0.0.0/20,IE,Dublin")
	src := newCloudSource()
	if err := p.downloadCloudRegions(ctx, client, downloads, layers, now, src); err != nil {
		t.Fatal(err)
	}
	if src.skipped(CloudRegionsLayer) || len(src.StaleLayers) != 0 || src.CloudRegions != 1 || src.CloudRegionsSeen != 3 || src.Rows[CloudRegionsLayer] != 1 {
		t.Fatalf("source %+v", src)
	}
	if got := readGzip(t, filepath.Join(layers, CloudRegionsLayer)); got != feedHeader+"52.0.0.0/16,IE,,Dublin,cloud-regions:aws:eu-west-1\n" {
		t.Errorf("layer:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(downloads, cloudRegionsFile)); err != nil {
		t.Error("no saved copy", err)
	}

	// The fetch fails: the saved copy, confirmed 13 days ago, is used.
	srv.fail = true
	os.Remove(filepath.Join(layers, CloudRegionsLayer))
	src = newCloudSource()
	if err := p.downloadCloudRegions(ctx, client, downloads, layers, now.Add(13*24*time.Hour), src); err != nil {
		t.Fatal(err)
	}
	if src.skipped(CloudRegionsLayer) || src.CloudRegions != 1 {
		t.Errorf("saved copy not used: %+v", src)
	}
	// Confirmed 15 days ago: too old, the layer is skipped and nothing is stale.
	os.Remove(filepath.Join(layers, CloudRegionsLayer))
	src = newCloudSource()
	if err := p.downloadCloudRegions(ctx, client, downloads, layers, now.Add(15*24*time.Hour), src); err != nil {
		t.Fatal(err)
	}
	skippedOnly(t, src, "old copy")
	if _, err := os.Stat(filepath.Join(layers, CloudRegionsLayer)); err == nil {
		t.Error("a layer file for a skipped layer")
	}

	// A failed fetch with no copy at all.
	p2, layers2, downloads2 := cloudPipeline(t, srv.URL, "52.0.0.0/20,IE,Dublin")
	src = newCloudSource()
	if err := p2.downloadCloudRegions(ctx, client, downloads2, layers2, now, src); err != nil {
		t.Fatal(err)
	}
	skippedOnly(t, src, "no copy")
	srv.fail = false

	// No geofeed layer.
	p3, layers3, downloads3 := cloudPipeline(t, srv.URL)
	src = newCloudSource()
	if err := p3.downloadCloudRegions(ctx, client, downloads3, layers3, now, src); err != nil {
		t.Fatal(err)
	}
	skippedOnly(t, src, "no geofeed layer")

	// No region qualifies.
	p4, layers4, downloads4 := cloudPipeline(t, srv.URL, "52.0.0.0/24,IE,Dublin")
	src = newCloudSource()
	if err := p4.downloadCloudRegions(ctx, client, downloads4, layers4, now, src); err != nil {
		t.Fatal(err)
	}
	skippedOnly(t, src, "no region")
	if _, err := os.Stat(filepath.Join(layers4, CloudRegionsLayer)); err == nil {
		t.Error("a layer file with no rows")
	}

	// An unusable file is skipped and the bad copy is not kept.
	bad := newRegionServer([]byte(`{"prefixes":[]}`))
	defer bad.Close()
	p5, layers5, downloads5 := cloudPipeline(t, bad.URL, "52.0.0.0/20,IE,Dublin")
	src = newCloudSource()
	if err := p5.downloadCloudRegions(ctx, client, downloads5, layers5, now, src); err != nil {
		t.Fatal(err)
	}
	skippedOnly(t, src, "unusable file")
	if _, err := os.Stat(filepath.Join(downloads5, cloudRegionsFile)); err == nil {
		t.Error("the unusable copy was kept")
	}

	// A body that announces more than the cap fails at once: skipped.
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(cloudRegionsMaxFile+1))
		w.Write([]byte("{"))
	}))
	defer big.Close()
	p6, layers6, downloads6 := cloudPipeline(t, big.URL, "52.0.0.0/20,IE,Dublin")
	src = newCloudSource()
	if err := p6.downloadCloudRegions(ctx, client, downloads6, layers6, now, src); err != nil {
		t.Fatal(err)
	}
	skippedOnly(t, src, "too large")

	// A cancelled context is an error.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.downloadCloudRegions(cctx, client, downloads, layers, now, newCloudSource()); err == nil {
		t.Error("no error for a cancelled context")
	}
}

func TestCloudRegionsLayerRules(t *testing.T) {
	if layerRules[CloudRegionsLayer] != "-within-first" || !ValidSkip(CloudRegionsLayer) {
		t.Error("rule or skip_layers missing")
	}
	p := &Pipeline{DataDir: t.TempDir()}
	touchLayers(t, p, RIRLayer, CloudRegionsLayer, FeedsLayer)
	got, err := p.geoArgs("o.mmdb", nil)
	if err != nil {
		t.Fatal(err)
	}
	if line := strings.Join(got, " "); !strings.Contains(line, "-within-first "+CloudRegionsLayer+","+FeedsLayer+" ") {
		t.Errorf("args: %s", line)
	}
}

// The holdout build recomputes the layer from the kept feeds: the cities of a held-out feed must
// not reach it, and the layer is left out when the held-out feed carries the cities of a region.
func TestPrepareHoldoutCloudRegions(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"), 0o755)
	p := &Pipeline{DataDir: dir, MMDBBuilder: script, heldOutURLs: heldOutSet(testHeldOut)}
	layers := p.layersDir()
	downloads := filepath.Join(p.dir(), "downloads")
	os.MkdirAll(layers, 0o750)
	os.MkdirAll(downloads, 0o750)
	writeGzip(t, filepath.Join(layers, RIRLayer), feedHeader)
	// Kept feed0 carries eu-west-1 (Dublin). Held-out feed1 carries af-south-1 (Cape Town).
	feeds := feedHeader +
		"52.0.0.0/20,IE,,Dublin,https://feed0.example/geo.csv\n" +
		"13.0.0.0/20,ZA,,Cape Town,https://feed1.example/geo.csv\n"
	writeGzip(t, filepath.Join(layers, FeedsLayer), feeds)
	os.WriteFile(filepath.Join(downloads, cloudRegionsFile),
		regionJSON([2]string{"52.0.0.0/16", "eu-west-1"}, [2]string{"13.0.0.0/16", "af-south-1"}), 0o640)
	// The main layer, as Download wrote it from all the feeds: both regions.
	writeGzip(t, filepath.Join(layers, CloudRegionsLayer), feedHeader+
		"13.0.0.0/16,ZA,,Cape Town,cloud-regions:aws:af-south-1\n52.0.0.0/16,IE,,Dublin,cloud-regions:aws:eu-west-1\n")

	eval := p.evalDir()
	os.MkdirAll(eval, 0o750)
	mmdb, _, status, err := p.prepareHoldout(context.Background(), eval)
	if err != nil || mmdb == "" {
		t.Fatalf("%q %q %v", mmdb, status, err)
	}
	own := filepath.Join(eval, "holdout", CloudRegionsLayer)
	got := readGzip(t, own)
	if got != feedHeader+"52.0.0.0/16,IE,,Dublin,cloud-regions:aws:eu-west-1\n" {
		t.Errorf("holdout layer = %q", got)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), " "+own+" ") || strings.Contains(string(args), filepath.Join(layers, CloudRegionsLayer)) ||
		!strings.Contains(string(args), "-within-first "+CloudRegionsLayer) || strings.Contains(status, "left out") {
		t.Errorf("args = %s status = %q", args, status)
	}

	// Only the held-out feed carries a region's cities: no region qualifies from the kept feeds.
	writeGzip(t, filepath.Join(layers, FeedsLayer), feedHeader+
		"52.0.0.0/24,IE,,Dublin,https://feed0.example/geo.csv\n"+
		"52.0.16.0/20,IE,,Dublin,https://feed1.example/geo.csv\n")
	os.Remove(argsFile)
	_, _, status, err = p.prepareHoldout(context.Background(), eval)
	args, _ = os.ReadFile(argsFile)
	if err != nil || strings.Contains(string(args), "68-cloud-regions") || !strings.Contains(status, CloudRegionsLayer+" left out: no rows") {
		t.Errorf("held out: %v args = %s status = %q", err, args, status)
	}

	// No saved region file: left out.
	writeGzip(t, filepath.Join(layers, FeedsLayer), feeds)
	os.Remove(filepath.Join(downloads, cloudRegionsFile))
	os.Remove(argsFile)
	_, _, status, err = p.prepareHoldout(context.Background(), eval)
	args, _ = os.ReadFile(argsFile)
	if err != nil || strings.Contains(string(args), "68-cloud-regions") || !strings.Contains(status, CloudRegionsLayer+" left out: no saved region file") {
		t.Errorf("no saved file: %v args = %s status = %q", err, args, status)
	}
	// An unusable saved file: left out too.
	os.WriteFile(filepath.Join(downloads, cloudRegionsFile), []byte("nope"), 0o640)
	_, _, status, err = p.prepareHoldout(context.Background(), eval)
	if err != nil || !strings.Contains(status, "saved region file unusable") {
		t.Errorf("unusable: %v status = %q", err, status)
	}
	// A run whose build skipped the layer has no layer file: the holdout does not add one.
	os.Remove(filepath.Join(layers, CloudRegionsLayer))
	_, _, status, err = p.prepareHoldout(context.Background(), eval)
	if err != nil || strings.Contains(status, CloudRegionsLayer) {
		t.Errorf("skipped layer: %v status = %q", err, status)
	}
}

// The label of an entry is its network_border_group when that is a non-empty string, else its region.
func TestParseCloudRegionsLabel(t *testing.T) {
	data := regionJSON3(
		[3]string{"13.0.0.0/16", "us-east-1", ""},                // no border group: the region
		[3]string{"13.1.0.0/24", "us-east-1", "US-East-1-LIM-1"}, // a zone: its own label, lower-cased
		[3]string{"13.2.0.0/24", "us-east-1", "us-east-1"},       // the border group of a normal range
		[3]string{"13.3.0.0/24", "us-east-1", "bad_group"},       // an invalid border group is bad, not the region
		[3]string{"13.4.0.0/24", "us-east-1", "GLOBAL"},          // the pseudo label
	)
	got, st, err := parseCloudRegions(data)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, r := range got[:4] {
		labels = append(labels, r.region)
	}
	if want := []string{"us-east-1", "us-east-1-lim-1", "us-east-1", "global"}; !reflect.DeepEqual(labels, want) || st.Bad != 1 {
		t.Errorf("labels %v bad %d", labels, st.Bad)
	}
	// An empty or null border group falls back to the region; any other type makes the file unusable.
	for body, ok := range map[string]bool{
		`"network_border_group":""`:    true,
		`"network_border_group":null`:  true,
		`"network_border_group":7`:     false,
		`"network_border_group":["x"]`: false,
	} {
		file := `{"prefixes":[{"ip_prefix":"13.0.0.0/16","region":"us-east-1",` + body + `},` +
			strings.Repeat(`{"ip_prefix":"13.1.0.0/16","region":"a"},`, cloudRegionsMinPrefixes) + `{"ip_prefix":"13.2.0.0/16","region":"a"}]}`
		got, _, err := parseCloudRegions([]byte(file))
		if (err == nil) != ok || (ok && got[0].region != "us-east-1") {
			t.Errorf("%s: %v", body, err)
		}
	}
	// Keys other than prefixes are skipped whatever they hold.
	skip := `{"a":{"b":[1,2,{"c":null}]},"prefixes":[` + strings.Repeat(`{"ip_prefix":"13.1.0.0/16","region":"a"},`, cloudRegionsMinPrefixes) + `{"ip_prefix":"13.2.0.0/16","region":"a"}],"z":[[[]]]}`
	if _, st, err := parseCloudRegions([]byte(skip)); err != nil || st.Valid != cloudRegionsMinPrefixes+1 {
		t.Errorf("skip: %+v %v", st, err)
	}
	// A body cut short, or with text after a key that is not a value, is not a file.
	for _, bad := range []string{`{"prefixes":[`, `{"a":`, `{"prefixes":[{"ip_prefix":"1.0.0.0/8"`, `{"prefixes":[] `} {
		if _, _, err := parseCloudRegions([]byte(bad)); err == nil {
			t.Errorf("no error for %q", bad)
		}
	}
}

// A body with more entries than the limit is rejected as soon as the limit is passed: the rest is
// not decoded (here it is not even valid JSON).
func TestParseCloudRegionsStopsAtLimit(t *testing.T) {
	entry := `{"ip_prefix":"52.0.0.0/16","region":"a"},`
	body := `{"prefixes":[` + strings.Repeat(entry, 3*cloudRegionsMaxEntries) + `}}}}garbage that is not JSON`
	_, st, err := parseCloudRegions([]byte(body))
	var in *cloudInputError
	if !errors.As(err, &in) || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("err = %v", err)
	}
	if st.Entries != cloudRegionsMaxEntries+1 {
		t.Errorf("read %d entries before stopping", st.Entries)
	}
}

// A zone has the parent region in "region" and its own place in the border group: it is its own
// group, gets a city only from the geofeed addresses inside its ranges and only with enough of them.
func TestCloudRegionsZones(t *testing.T) {
	data := regionJSON3(
		[3]string{"13.0.0.0/16", "us-east-1", "us-east-1"},
		[3]string{"13.0.5.0/24", "us-east-1", "us-east-1-lim-1"}, // a small zone inside the region
		[3]string{"13.2.0.0/20", "us-east-1", "us-east-1-bog-1"}, // a zone with enough addresses
		[3]string{"18.0.0.0/16", "eu-west-1", ""},                // no border group
	)
	feeds := feedFile(t,
		"13.0.0.0/16,US,Ashburn",
		"13.0.5.0/24,PE,Lima", // 100% one city, but only 256 addresses
		"13.2.0.0/20,CO,Bogota",
		"18.0.0.0/20,IE,Dublin",
	)
	rows, st := cloudLayer(t, data, feeds)
	if strings.Contains(rows, "Lima") || strings.Contains(rows, "lim-1") || strings.Contains(rows, "13.0.5.") {
		t.Errorf("the small zone got a row or the parent's: %s", rows)
	}
	for _, want := range []string{
		"13.0.0.0/22,US,,Ashburn,cloud-regions:aws:us-east-1\n",
		"13.0.4.0/24,US,,Ashburn,cloud-regions:aws:us-east-1\n", // the zone is taken out of the region
		"13.0.6.0/23,US,,Ashburn,cloud-regions:aws:us-east-1\n",
		"13.2.0.0/20,CO,,Bogota,cloud-regions:aws:us-east-1-bog-1\n",
		"18.0.0.0/16,IE,,Dublin,cloud-regions:aws:eu-west-1\n",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("missing %q in:\n%s", want, rows)
		}
	}
	// The zone's addresses are not counted for the parent: the Ashburn feed row owns the
	// region's addresses minus the Lima and Bogota rows, and 100% of them are Ashburn.
	// Labels: us-east-1, -lim-1, -bog-1, eu-west-1, filler-1.
	if st.Seen != 5 || st.Qualified != 3 {
		t.Errorf("stats %+v", st)
	}
	// A zone that is the only holder of the feed city does not give it to the parent: the parent
	// has no city of its own, so it gets no row.
	data = regionJSON3([3]string{"13.0.0.0/16", "us-east-1", "us-east-1"}, [3]string{"13.0.0.0/20", "us-east-1", "us-east-1-lim-1"})
	rows, _ = cloudLayer(t, data, feedFile(t, "13.0.0.0/20,PE,Lima"))
	if strings.Contains(rows, "us-east-1\n") || !strings.Contains(rows, "13.0.0.0/20,PE,,Lima,cloud-regions:aws:us-east-1-lim-1\n") {
		t.Errorf("rows:\n%s", rows)
	}
}

// A saved copy that is used after a failed fetch but turns out unusable is deleted with its ETag
// and stamp, and the layer is skipped.
func TestDownloadCloudRegionsUnusableSavedCopy(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	srv := newRegionServer(nil)
	srv.fail = true
	defer srv.Close()
	p, layers, downloads := cloudPipeline(t, srv.URL, "52.0.0.0/20,IE,Dublin")
	saved := filepath.Join(downloads, cloudRegionsFile)
	os.WriteFile(saved, []byte("not a region file"), 0o640)
	os.WriteFile(saved+".etag", []byte(`"x"`), 0o640)
	os.WriteFile(saved+".checked", nil, 0o640)
	os.Chtimes(saved+".checked", now.Add(-24*time.Hour), now.Add(-24*time.Hour))
	src := newCloudSource()
	if err := p.downloadCloudRegions(ctx, newHTTPClient(), downloads, layers, now, src); err != nil {
		t.Fatal(err)
	}
	skippedOnly(t, src, "unusable saved copy")
	for _, f := range []string{saved, saved + ".etag", saved + ".checked"} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was kept", filepath.Base(f))
		}
	}
}
