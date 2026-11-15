package geo

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/bgp"
	"openipdata.org/hatchet/publish"
)

const bgpHeader = "prefix,communities\n"

// bgpLayerOf runs the layer on a communities file and a geofeed file given as text.
func bgpLayerOf(t *testing.T, communities, feeds string) (string, bgpStats) {
	t.Helper()
	dir := t.TempDir()
	c, f := filepath.Join(dir, "c.csv.gz"), filepath.Join(dir, "f.csv.gz")
	writeGzip(t, c, bgpHeader+communities)
	writeGzip(t, f, feedHeader+feeds)
	var st bgpStats
	out := layerRows(t, func(w *csv.Writer) error {
		var err error
		st, err = bgpRows(c, f, w)
		return err
	})
	return strings.TrimPrefix(out, feedHeader), st
}

// feedRows is one geofeed row per prefix, all in the one city.
func feedRows(city string, prefixes ...string) string {
	var b strings.Builder
	for _, p := range prefixes {
		fmt.Fprintf(&b, "%s,DE,,%s,f\n", p, city)
	}
	return b.String()
}

func TestBGPLearnsAndApplies(t *testing.T) {
	// Three /24s that a geofeed puts in Berlin carry 100:1; so does a fourth /24 that no feed covers.
	comm := "10.0.0.0/24,100:1\n10.0.1.0/24,100:1\n10.0.2.0/24,100:1\n10.0.9.0/24,100:1 5:5\n10.0.20.0/24,\n10.0.30.0/24,5:5\n"
	rows, st := bgpLayerOf(t, comm, feedRows("Berlin", "10.0.0.0/24", "10.0.1.0/24", "10.0.2.0/24"))
	want := "10.0.0.0/24,DE,,Berlin,bgp\n10.0.1.0/24,DE,,Berlin,bgp\n10.0.2.0/24,DE,,Berlin,bgp\n10.0.9.0/24,DE,,Berlin,bgp\n"
	if rows != want {
		t.Errorf("rows:\n%s", rows)
	}
	if st.Prefixes != 6 || st.Known != 3 || st.Rules != 1 || st.Located != 4 || st.Rows != 4 {
		t.Errorf("stats %+v", st)
	}
}

// The prefix of the rows is its owned part only: a more specific BGP prefix is not given the
// city of the broader one, and the rest is split into the fewest prefixes.
func TestBGPOwnedPartsOnly(t *testing.T) {
	comm := "10.0.0.0/22,100:1\n10.0.1.0/24,\n10.1.0.0/24,100:1\n10.2.0.0/24,100:1\n10.3.0.0/24,100:1\n"
	rows, _ := bgpLayerOf(t, comm, feedRows("Berlin", "10.1.0.0/24", "10.2.0.0/24", "10.3.0.0/24"))
	// 10.0.0.0/22 minus 10.0.1.0/24: 10.0.0.0/24 and 10.0.2.0/23.
	want := "10.0.0.0/24,DE,,Berlin,bgp\n10.0.2.0/23,DE,,Berlin,bgp\n10.1.0.0/24,DE,,Berlin,bgp\n10.2.0.0/24,DE,,Berlin,bgp\n10.3.0.0/24,DE,,Berlin,bgp\n"
	if rows != want {
		t.Errorf("rows:\n%s", rows)
	}
	// An owned part that is not one block: 10.0.0.0/21 with a /24 hole at 10.0.2.0.
	comm = "10.0.0.0/21,100:1\n10.0.2.0/24,\n10.1.0.0/24,100:1\n10.2.0.0/24,100:1\n10.3.0.0/24,100:1\n"
	rows, _ = bgpLayerOf(t, comm, feedRows("Berlin", "10.1.0.0/24", "10.2.0.0/24", "10.3.0.0/24"))
	want = "10.0.0.0/23,DE,,Berlin,bgp\n10.0.3.0/24,DE,,Berlin,bgp\n10.0.4.0/22,DE,,Berlin,bgp\n10.1.0.0/24,DE,,Berlin,bgp\n10.2.0.0/24,DE,,Berlin,bgp\n10.3.0.0/24,DE,,Berlin,bgp\n"
	if rows != want {
		t.Errorf("rows:\n%s", rows)
	}
	// The whole address space edge: a /8 at the top and the bottom.
	rows, _ = bgpLayerOf(t, "0.0.0.0/8,100:1\n255.0.0.0/8,100:1\n240.0.0.0/8,100:1\n", feedRows("Berlin", "0.0.0.0/8", "255.0.0.0/8", "240.0.0.0/8"))
	if rows != "0.0.0.0/8,DE,,Berlin,bgp\n240.0.0.0/8,DE,,Berlin,bgp\n255.0.0.0/8,DE,,Berlin,bgp\n" {
		t.Errorf("edges:\n%s", rows)
	}
}

// In the geofeed file a more specific row wins over a broader one, and of a repeated prefix the last row wins.
func TestBGPTruthFromFeeds(t *testing.T) {
	known := func(feeds string, owned string) int {
		_, st := bgpLayerOf(t, owned+",\n", feeds)
		return st.Known
	}
	// The /24 is Berlin through a broad row, except a more specific Munich /25: 50% each, no city has 80% of the covered.
	if n := known(feedRows("Berlin", "10.0.0.0/16")+feedRows("Munich", "10.0.0.0/25"), "10.0.0.0/24"); n != 0 {
		t.Errorf("more specific row: %d", n)
	}
	// A more specific row with no city covers its part without a city: 50% covered, all in Berlin.
	if n := known(feedRows("Berlin", "10.0.0.0/16")+"10.0.0.0/25,DE,,,f\n", "10.0.0.0/24"); n != 1 {
		t.Errorf("cityless specific row: %d", n)
	}
	// The same prefix twice: the last row wins.
	if n := known(feedRows("Munich", "10.0.0.0/24")+feedRows("Berlin", "10.0.0.0/24"), "10.0.0.0/24"); n != 1 {
		t.Errorf("repeated prefix: %d", n)
	}
	// A last row with a bad country removes the prefix.
	if n := known(feedRows("Berlin", "10.0.0.0/24")+"10.0.0.0/24,Germany,,Berlin,f\n", "10.0.0.0/24"); n != 0 {
		t.Errorf("bad country: %d", n)
	}
	// Case and spacing of the city do not matter; IPv6 and broken rows are skipped.
	if n := known("10.0.0.0/25,DE,,berlin,f\n10.0.0.128/25,DE,,  Berlin ,f\n2001:db8::/32,DE,,Berlin,f\nnonsense\n", "10.0.0.0/24"); n != 1 {
		t.Errorf("spelling: %d", n)
	}
	// The same name in another country is another city.
	if n := known("10.0.0.0/25,DE,,Berlin,f\n10.0.0.128/25,US,,Berlin,f\n", "10.0.0.0/24"); n != 0 {
		t.Errorf("namesakes: %d", n)
	}
}

func TestBGPTruthThresholds(t *testing.T) {
	// The cover: a /24 with 128 of its 256 addresses in feed rows is 50% (known), 127 is not.
	half := "10.0.0.0/25,DE,,Berlin,f\n"
	almost := "10.0.0.0/26,DE,,Berlin,f\n10.0.0.64/27,DE,,Berlin,f\n10.0.0.96/28,DE,,Berlin,f\n10.0.0.112/29,DE,,Berlin,f\n10.0.0.120/30,DE,,Berlin,f\n10.0.0.124/31,DE,,Berlin,f\n10.0.0.126/32,DE,,Berlin,f\n"
	if _, st := bgpLayerOf(t, "10.0.0.0/24,\n", half); st.Known != 1 {
		t.Errorf("50%% covered: %d", st.Known)
	}
	if _, st := bgpLayerOf(t, "10.0.0.0/24,\n", almost); st.Known != 0 {
		t.Errorf("49.6%% covered: %d", st.Known)
	}
	// The cover counts the owned addresses: a more specific BGP prefix is not part of the owner's.
	if _, st := bgpLayerOf(t, "10.0.0.0/24,\n10.0.0.128/25,\n", half); st.Known != 1 {
		t.Errorf("owned only: %d", st.Known)
	}
	// The share: Berlin 512 and Munich 128 of a /22 is 80% of the 640 covered (known); 144 Munich is 78%.
	berlin := "10.0.0.0/23,DE,,Berlin,f\n"
	if _, st := bgpLayerOf(t, "10.0.0.0/22,\n", berlin+"10.0.2.0/25,DE,,Munich,f\n"); st.Known != 1 {
		t.Errorf("80%% share: %d", st.Known)
	}
	if _, st := bgpLayerOf(t, "10.0.0.0/22,\n", berlin+"10.0.2.0/25,DE,,Munich,f\n10.0.2.128/28,DE,,Munich,f\n"); st.Known != 0 {
		t.Errorf("78%% share: %d", st.Known)
	}
}

// bgpWorld builds known prefixes: for each city, n /24s in the city with the value.
func bgpWorld(value string, cities map[string]int, countries map[string]string) (comm, feeds string) {
	i := 0
	for _, name := range []string{"Berlin", "Munich", "Paris", "Rome"} {
		for k := 0; k < cities[name]; k++ {
			p := fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)
			i++
			comm += p + "," + value + "\n"
			feeds += fmt.Sprintf("%s,%s,,%s,f\n", p, countries[name], name)
		}
	}
	return
}

var bgpCountries = map[string]string{"Berlin": "DE", "Munich": "DE", "Paris": "FR", "Rome": "IT"}

func TestBGPRuleThresholds(t *testing.T) {
	rules := func(cities map[string]int) int {
		comm, feeds := bgpWorld("100:1", cities, bgpCountries)
		_, st := bgpLayerOf(t, comm, feeds)
		return st.Rules
	}
	// At least 3 known prefixes.
	if n := rules(map[string]int{"Berlin": 2}); n != 0 {
		t.Errorf("2 prefixes: %d rules", n)
	}
	if n := rules(map[string]int{"Berlin": 3}); n != 1 {
		t.Errorf("3 prefixes: %d rules", n)
	}
	// The top city needs 80%: 4 of 5 yes, 3 of 4 no.
	if n := rules(map[string]int{"Berlin": 4, "Munich": 1}); n != 1 {
		t.Errorf("80%% city: %d rules", n)
	}
	if n := rules(map[string]int{"Berlin": 3, "Munich": 1}); n != 0 {
		t.Errorf("75%% city: %d rules", n)
	}
	// The top country needs 90%: Berlin 8, Munich 1 (both DE), Paris 1 is 90% DE; Berlin 8, Paris 2 is 80% DE.
	if n := rules(map[string]int{"Berlin": 8, "Munich": 1, "Paris": 1}); n != 1 {
		t.Errorf("90%% country: %d rules", n)
	}
	if n := rules(map[string]int{"Berlin": 8, "Paris": 2}); n != 0 {
		t.Errorf("80%% country: %d rules", n)
	}
	// A rule from a value of the prefixes that are not known counts nothing: only known prefixes teach.
	comm, feeds := bgpWorld("100:1", map[string]int{"Berlin": 2}, bgpCountries)
	comm += "11.0.0.0/24,100:1\n11.0.1.0/24,100:1\n"
	if _, st := bgpLayerOf(t, comm, feeds); st.Rules != 0 {
		t.Errorf("unknown prefixes taught: %d", st.Rules)
	}
}

func TestBGPAgreement(t *testing.T) {
	// 100:1 means Berlin, 200:1 means Munich (three known /24s each), 300:1 is no rule (two /24s).
	var comm, feeds string
	add := func(i int, city, value string) {
		p := fmt.Sprintf("10.%d.0.0/24", i)
		comm += p + "," + value + "\n"
		feeds += fmt.Sprintf("%s,DE,,%s,f\n", p, city)
	}
	for i := 0; i < 3; i++ {
		add(i, "Berlin", "100:1")
		add(10+i, "Munich", "200:1")
	}
	add(20, "Hamburg", "300:1")
	add(21, "Hamburg", "300:1")
	comm += "20.0.0.0/24,100:1 9:9\n"           // one rule and a value that is no rule: Berlin
	comm += "20.0.1.0/24,100:1 200:1\n"         // two rules, two cities: nothing
	comm += "20.0.2.0/24,100:1 300:1\n"         // one rule: Berlin
	comm += "20.0.3.0/24,300:1\n"               // no rule
	comm += "20.0.4.0/24,\n"                    // no value
	comm += "20.0.5.0/24,100:1 9:9 7:7 100:1\n" // a repeated value is one value
	rows, _ := bgpLayerOf(t, comm, feeds)
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(rows), "\n") {
		if strings.HasPrefix(line, "20.") {
			got = append(got, line)
		}
	}
	want := []string{"20.0.0.0/24,DE,,Berlin,bgp", "20.0.2.0/24,DE,,Berlin,bgp", "20.0.5.0/24,DE,,Berlin,bgp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows %v", got)
	}
	// Two rules that name the same city agree: 100:1 and 100:2 both mean Berlin.
	comm, feeds = "", ""
	for i := 0; i < 3; i++ {
		add(i, "Berlin", "100:1 100:2")
	}
	comm += "20.0.0.0/24,100:1 100:2\n20.0.1.0/24,100:2\n"
	rows, _ = bgpLayerOf(t, comm, feeds)
	if !strings.Contains(rows, "20.0.0.0/24,DE,,Berlin,bgp\n") || !strings.Contains(rows, "20.0.1.0/24,DE,,Berlin,bgp\n") {
		t.Errorf("agreeing rules:\n%s", rows)
	}
}

// The city is written in the spelling used most often in the feeds; the country is the city's.
func TestBGPSpellingAndCountry(t *testing.T) {
	comm := "10.0.0.0/24,100:1\n10.0.1.0/24,100:1\n10.0.2.0/24,100:1\n10.0.3.0/24,100:1\n"
	feeds := "10.0.0.0/24,fr,,Saint-Étienne,f\n10.0.1.0/24,FR,,saint-étienne,f\n10.0.2.0/24,FR,,Saint-Étienne,f\n"
	rows, _ := bgpLayerOf(t, comm, feeds)
	if !strings.HasSuffix(rows, "10.0.3.0/24,FR,,Saint-Étienne,bgp\n") {
		t.Errorf("rows:\n%s", rows)
	}
}

func TestBGPDeterministic(t *testing.T) {
	comm, feeds := bgpWorld("100:1", map[string]int{"Berlin": 5, "Munich": 1}, bgpCountries)
	comm += "10.50.0.0/24,100:1\n10.50.1.0/24,100:1 100:2\n"
	a, _ := bgpLayerOf(t, comm, feeds)
	for i := 0; i < 5; i++ {
		if b, _ := bgpLayerOf(t, comm, feeds); a != b {
			t.Fatal("rows differ between runs")
		}
	}
	// The order of the communities rows does not matter.
	lines := strings.Split(strings.TrimSpace(comm), "\n")
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	if b, _ := bgpLayerOf(t, strings.Join(lines, "\n")+"\n", feeds); a != b {
		t.Error("rows depend on the order of the file")
	}
}

func TestReadBGPFile(t *testing.T) {
	dir := t.TempDir()
	read := func(body string) (*bgpTable, error) {
		path := filepath.Join(dir, "c.csv.gz")
		writeGzip(t, path, body)
		return readBGPFile(path)
	}
	tbl, err := read(bgpHeader + "10.0.1.0/24,5:5 1:1 5:5\n10.0.0.0/24,1:1\n10.0.1.0/24,2:2\n2001:db8::/32,1:1\nbad,1:1\nno comma\n10.0.2.5/24,\n")
	if err != nil {
		t.Fatal(err)
	}
	// Sorted; the repeated prefix keeps its last row; other families and bad rows are gone; host bits cleared.
	if len(tbl.prefixes) != 3 || tbl.prefixes[0].addr != 10<<24 || tbl.prefixes[1].addr != 10<<24|1<<8 || tbl.prefixes[2].addr != 10<<24|2<<8 {
		t.Fatalf("prefixes %+v", tbl.prefixes)
	}
	if tbl.prefixes[1].vn != 1 || tbl.prefixes[2].vn != 0 || tbl.nvals != 3 {
		t.Errorf("values %+v nvals %d", tbl.prefixes, tbl.nvals)
	}
	for _, body := range []string{"", "network,values\n10.0.0.0/24,1:1\n"} {
		var in *bgpInputError
		if _, err := read(body); !errors.As(err, &in) {
			t.Errorf("%q: %v", body, err)
		}
	}
	var in *bgpInputError
	if _, err := readBGPFile(filepath.Join(dir, "missing")); !errors.As(err, &in) {
		t.Errorf("missing: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "plain"), []byte("not gzip"), 0o640)
	if _, err := readBGPFile(filepath.Join(dir, "plain")); !errors.As(err, &in) {
		t.Errorf("not gzip: %v", err)
	}
}

func TestSweepOwned(t *testing.T) {
	type span struct {
		start, end uint32
		item       int
	}
	// Sorted by (address, length): 0.0.0.0/7, 0.0.0.0/8, 1.0.0.0/16, 1.0.0.0/24, 1.0.2.0/24.
	items := []bgpPrefix{{addr: 0, bits: 7}, {addr: 0, bits: 8}, {addr: 1 << 24, bits: 16}, {addr: 1 << 24, bits: 24}, {addr: 1<<24 | 2<<8, bits: 24}}
	var got []span
	sweepOwned(len(items), func(i int) (uint32, uint32) { return items[i].first(), items[i].last() },
		func(start, end uint32, i int) { got = append(got, span{start, end, i}) })
	// The /7 is 0.0.0.0-1.255.255.255; the /16 is 1.0.0.0-1.0.255.255.
	want := []span{
		{0x00000000, 0x00ffffff, 1},
		{0x01000000, 0x010000ff, 3},
		{0x01000100, 0x010001ff, 2},
		{0x01000200, 0x010002ff, 4},
		{0x01000300, 0x0100ffff, 2},
		{0x01010000, 0x01ffffff, 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %x", got)
	}
}

// fakeObject and fakeFetch are in layers_test.go.
func TestDownloadBGP(t *testing.T) {
	now := time.Date(2026, 11, 15, 7, 0, 0, 0, time.UTC)
	root := t.TempDir()
	layers, downloads := filepath.Join(root, "layers"), filepath.Join(root, "downloads")
	reset := func(withFeeds bool) {
		os.RemoveAll(layers)
		os.MkdirAll(layers, 0o750)
		os.MkdirAll(downloads, 0o750)
		if withFeeds {
			writeGzip(t, filepath.Join(layers, FeedsLayer), feedHeader+feedRows("Berlin", "10.0.0.0/24", "10.0.1.0/24", "10.0.2.0/24"))
		}
	}
	comm := bgpHeader + "10.0.0.0/24,100:1\n10.0.1.0/24,100:1\n10.0.2.0/24,100:1\n10.0.9.0/24,100:1\n"
	good := func(uploaded time.Time) map[string]fakeObject {
		return map[string]fakeObject{bgp.CommunitiesKey: {gzipText(t, comm), "e1", uploaded, nil}}
	}
	var logs, fetched []string
	run := func(objects map[string]fakeObject) (Source, error) {
		logs, fetched = nil, nil
		p := &Pipeline{Progress: func(s string) { logs = append(logs, s) }}
		src := Source{Rows: map[string]int{}, R2Uploaded: map[string]string{}}
		return src, p.downloadBGP(context.Background(), fakeFetch(objects, &fetched), downloads, layers, now, &src)
	}
	skipped := func(src Source, err error, what string) {
		t.Helper()
		if err != nil || !reflect.DeepEqual(src.SkippedLayers, []string{BGPLayer}) || len(src.StaleLayers) != 0 {
			t.Errorf("%s: %+v %v", what, src, err)
		}
		if _, err := os.Stat(filepath.Join(layers, BGPLayer)); err == nil {
			t.Errorf("%s: a file for a skipped layer", what)
		}
		if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "WARNING") {
			t.Errorf("%s: logs %q", what, logs)
		}
	}

	// Fresh object: the layer is written, counts are reported, the saved copy has its ETag.
	reset(true)
	src, err := run(good(now.Add(-time.Hour)))
	if err != nil || len(src.SkippedLayers) != 0 || src.Rows[BGPLayer] != 4 || src.BGPRules != 1 || src.BGPKnown != 3 || src.BGPPrefixes != 4 || len(src.StaleLayers) != 0 {
		t.Fatalf("fresh: %+v %v", src, err)
	}
	if got := readGzip(t, filepath.Join(layers, BGPLayer)); !strings.HasSuffix(got, "10.0.9.0/24,DE,,Berlin,bgp\n") || !strings.HasPrefix(got, feedHeader) {
		t.Errorf("layer = %q", got)
	}
	if data, _ := os.ReadFile(filepath.Join(downloads, bgp.OutFile+".etag")); string(data) != "e1" {
		t.Errorf("saved ETag = %q", data)
	}
	// Unchanged object: the saved copy is used.
	reset(true)
	src, err = run(good(now.Add(-time.Hour)))
	if err != nil || len(fetched) != 0 || !reflect.DeepEqual(src.R2Reused, []string{BGPLayer}) || src.Rows[BGPLayer] != 4 {
		t.Errorf("unchanged: %+v %v %v", src, err, fetched)
	}
	// Exactly 3 days old is used; older is skipped, without marking the ping stale.
	reset(true)
	if src, err = run(good(now.Add(-rowsMaxAge))); err != nil || len(src.SkippedLayers) != 0 {
		t.Errorf("3 days: %+v %v", src, err)
	}
	reset(true)
	src, err = run(good(now.Add(-rowsMaxAge - time.Hour)))
	skipped(src, err, "old object")
	// Missing, or any R2 error, skips too.
	reset(true)
	src, err = run(map[string]fakeObject{bgp.CommunitiesKey: {err: fmt.Errorf("download: stat: %w", publish.ErrNotFound)}})
	skipped(src, err, "missing object")
	reset(true)
	src, err = run(map[string]fakeObject{bgp.CommunitiesKey: {err: errors.New("boom")}})
	skipped(src, err, "R2 error")
	// No geofeed layer: skipped, and R2 is not asked.
	reset(false)
	src, err = run(good(now))
	skipped(src, err, "no feeds")
	if len(fetched) != 0 {
		t.Errorf("fetched without feeds: %v", fetched)
	}
	// An unreadable file skips the layer and is not kept under its ETag.
	reset(true)
	src, err = run(map[string]fakeObject{bgp.CommunitiesKey: {"not gzip", "e2", now, nil}})
	skipped(src, err, "unreadable")
	if _, err := os.Stat(filepath.Join(downloads, bgp.OutFile)); err == nil {
		t.Error("an unreadable copy kept")
	}
	if _, err := os.Stat(filepath.Join(downloads, bgp.OutFile+".etag")); err == nil {
		t.Error("the ETag of an unreadable copy kept")
	}
	// Nothing learned: skipped.
	reset(true)
	src, err = run(map[string]fakeObject{bgp.CommunitiesKey: {gzipText(t, bgpHeader+"10.0.9.0/24,100:1\n"), "e3", now, nil}})
	skipped(src, err, "no rows")
	// A cancelled run fails.
	reset(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &Pipeline{}
	cancelled := func(context.Context, string, string, string) (publish.ObjectInfo, bool, error) {
		return publish.ObjectInfo{}, false, context.Canceled
	}
	if err := p.downloadBGP(ctx, cancelled, downloads, layers, now, &Source{Rows: map[string]int{}, R2Uploaded: map[string]string{}}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func gzipText(t *testing.T, s string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.gz")
	writeGzip(t, path, s)
	data, _ := os.ReadFile(path)
	return string(data)
}

// The layer is optional, below 30-pdb, and has the -same-country rule.
func TestBGPLayerRules(t *testing.T) {
	if BGPLayer != "25-bgp.csv.gz" || layerRules[BGPLayer] != "-same-country" {
		t.Errorf("rule %q", layerRules[BGPLayer])
	}
	if !ValidSkip(BGPLayer) || BGPLayer >= PDBLayer || BGPLayer <= IPverseLayer {
		t.Errorf("layer order or skip list")
	}
	p := &Pipeline{DataDir: t.TempDir()}
	touchLayers(t, p, RIRLayer, BGPLayer, PDBLayer, FeedsLayer)
	got, err := p.geoArgs("o.mmdb", nil)
	if err != nil {
		t.Fatal(err)
	}
	if line := strings.Join(got, " "); !strings.Contains(line, "-same-country "+BGPLayer+","+PDBLayer+" ") {
		t.Errorf("args: %s", line)
	}
	// skip_layers accepts it and removes the file.
	src := Source{}
	if err := p.SkipLayers(&src, []string{BGPLayer}); err != nil || !src.skipped(BGPLayer) {
		t.Errorf("skip: %v %+v", err, src)
	}
	if names, _ := p.layerFiles(); len(names) != 3 {
		t.Errorf("layer files = %v", names)
	}
}

func ValidSkip(name string) bool { return ValidateSkipLayers([]string{name}) == nil }

// The holdout build recomputes the layer from the kept feeds: the city of a held-out feed
// must not reach it through a learned rule.
func TestPrepareHoldoutBGP(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"), 0o755)
	p := &Pipeline{DataDir: dir, MMDBBuilder: script, heldOutURLs: heldOutSet(testHeldOut)}
	layers := p.layersDir()
	os.MkdirAll(layers, 0o750)
	os.MkdirAll(filepath.Join(p.dir(), "downloads"), 0o750)
	writeGzip(t, filepath.Join(layers, RIRLayer), feedHeader)
	// Kept feed0: Berlin for the /24s carrying 100:1. Held-out feed1: Paris for the /24s carrying 200:1.
	feeds := feedHeader
	comm := bgpHeader
	for i := 0; i < 3; i++ {
		feeds += fmt.Sprintf("10.0.%d.0/24,DE,,Berlin,https://feed0.example/geo.csv\n", i)
		comm += fmt.Sprintf("10.0.%d.0/24,100:1\n", i)
		feeds += fmt.Sprintf("10.1.%d.0/24,FR,,Paris,https://feed1.example/geo.csv\n", i)
		comm += fmt.Sprintf("10.1.%d.0/24,200:1\n", i)
	}
	comm += "10.0.9.0/24,100:1\n10.1.9.0/24,200:1\n"
	writeGzip(t, filepath.Join(layers, FeedsLayer), feeds)
	writeGzip(t, filepath.Join(p.dir(), "downloads", bgp.OutFile), comm)
	// The main layer, as Download wrote it from all the feeds: both rules.
	writeGzip(t, filepath.Join(layers, BGPLayer), feedHeader+"10.0.9.0/24,DE,,Berlin,bgp\n10.1.9.0/24,FR,,Paris,bgp\n")

	eval := p.evalDir()
	os.MkdirAll(eval, 0o750)
	mmdb, _, status, err := p.prepareHoldout(context.Background(), eval)
	if err != nil || mmdb == "" {
		t.Fatalf("%q %q %v", mmdb, status, err)
	}
	own := filepath.Join(eval, "holdout", BGPLayer)
	got := readGzip(t, own)
	if !strings.Contains(got, "10.0.9.0/24,DE,,Berlin,bgp\n") || strings.Contains(got, "10.1.9.0/24") || strings.Contains(got, "Paris") {
		t.Errorf("holdout layer = %q", got)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), " "+own+" ") || strings.Contains(string(args), filepath.Join(layers, BGPLayer)) ||
		!strings.Contains(string(args), "-same-country "+BGPLayer) || strings.Contains(status, "left out") {
		t.Errorf("args = %s status = %q", args, status)
	}

	// No saved communities: the layer is left out, with its flag.
	os.Remove(filepath.Join(p.dir(), "downloads", bgp.OutFile))
	os.Remove(argsFile)
	_, _, status, err = p.prepareHoldout(context.Background(), eval)
	args, _ = os.ReadFile(argsFile)
	if err != nil || strings.Contains(string(args), "25-bgp") || strings.Contains(string(args), "-same-country") || !strings.Contains(status, BGPLayer+" left out") {
		t.Errorf("no communities: %v args = %s status = %q", err, args, status)
	}
	// The kept feeds teach nothing: left out as well.
	writeGzip(t, filepath.Join(p.dir(), "downloads", bgp.OutFile), bgpHeader+"10.1.9.0/24,200:1\n10.1.0.0/24,200:1\n10.1.1.0/24,200:1\n10.1.2.0/24,200:1\n")
	os.Remove(argsFile)
	_, _, status, err = p.prepareHoldout(context.Background(), eval)
	args, _ = os.ReadFile(argsFile)
	if err != nil || strings.Contains(string(args), "25-bgp") || !strings.Contains(status, "no rows") {
		t.Errorf("no rows: %v args = %s status = %q", err, args, status)
	}
	// A run whose build skipped the layer has no layer file: the holdout does not add one.
	os.Remove(filepath.Join(layers, BGPLayer))
	os.Remove(argsFile)
	_, _, status, err = p.prepareHoldout(context.Background(), eval)
	args, _ = os.ReadFile(argsFile)
	if err != nil || strings.Contains(string(args), "25-bgp") || strings.Contains(status, BGPLayer) {
		t.Errorf("skipped layer: %v args = %s status = %q", err, args, status)
	}
}
