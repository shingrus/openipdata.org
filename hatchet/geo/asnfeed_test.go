package geo

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const feedHeader = "prefix,country,region,city,source\n"

func ann(asn int64, prefixes ...string) []asnPrefix {
	var out []asnPrefix
	for _, p := range prefixes {
		out = append(out, asnPrefix{asn, netip.MustParsePrefix(p)})
	}
	return out
}

// runASNFeed computes the layer for the feed rows and returns its rows as text.
func runASNFeed(t *testing.T, feeds string, announced ...[]asnPrefix) (string, int, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feeds.csv.gz")
	writeGzip(t, path, feedHeader+feeds)
	var all []asnPrefix
	for _, a := range announced {
		all = append(all, a...)
	}
	var asns, n int
	out := layerRows(t, func(w *csv.Writer) error {
		var err error
		asns, n, err = asnFeedRows(path, all, w)
		return err
	})
	return strings.TrimPrefix(out, feedHeader), asns, n
}

func TestASNFeedDominance(t *testing.T) {
	a := ann(64500, "10.0.0.0/21", "10.1.0.0/24")
	// 4 x /24 Berlin + 1 x /24 Munich = exactly 80%: dominant.
	rows := "10.0.0.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Berlin,f\n10.0.2.0/24,DE,,berlin,f\n10.0.3.0/24,DE,,Berlin,f\n10.0.4.0/24,DE,,Munich,f\n"
	got, asns, n := runASNFeed(t, rows, a)
	if got != "10.0.0.0/21,DE,,Berlin,asn:64500\n10.1.0.0/24,DE,,Berlin,asn:64500\n" || asns != 1 || n != 2 {
		t.Errorf("at 80%%: %q %d %d", got, asns, n)
	}
	// Just under 80%.
	got, _, n = runASNFeed(t, rows+"10.0.5.0/24,DE,,Munich,f\n", a)
	if got != "" || n != 0 {
		t.Errorf("under 80%%: %q", got)
	}
	// The same name in another country is another city.
	_, _, n = runASNFeed(t, "10.0.0.0/24,US,,Paris,f\n10.0.1.0/24,FR,,Paris,f\n", a)
	if n != 0 {
		t.Errorf("namesakes: %d", n)
	}
	// One row is not enough, even at 100%; two rows are.
	_, _, n = runASNFeed(t, "10.0.0.0/16,DE,,Berlin,f\n", a)
	if n != 0 {
		t.Errorf("one row: %d", n)
	}
	_, _, n = runASNFeed(t, "10.0.0.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Berlin,f\n", ann(64500, "10.0.0.0/22"))
	if n != 1 {
		t.Errorf("two rows: %d", n)
	}
	// A row with a bad country does not count at all.
	_, _, n = runASNFeed(t, "10.0.0.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Berlin,f\n10.0.4.0/22,Germany,,Bonn,f\n", ann(64500, "10.0.0.0/22"))
	if n != 1 {
		t.Errorf("bad country: %d", n)
	}
	// Rows without a city count in the total: a country-only /12 and two /24 city rows have no dominant city.
	_, _, n = runASNFeed(t, "10.0.0.0/12,DE,,,f\n10.0.0.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Berlin,f\n", ann(1, "10.0.0.0/12"))
	if n != 0 {
		t.Errorf("country-only /12: %d", n)
	}
	// A small country-only row does not stop the city (4 x /24 city + 1 x /26 without = 99%).
	_, _, n = runASNFeed(t, "10.0.0.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Berlin,f\n10.0.2.0/26,DE,,,f\n", ann(64500, "10.0.0.0/22"))
	if n != 1 {
		t.Errorf("small country-only row: %d", n)
	}
	// Last row wins: a later row without a city replaces the city row and counts as no-city.
	_, _, n = runASNFeed(t, "10.0.0.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Berlin,f\n10.0.2.0/24,DE,,Berlin,f\n10.0.0.0/24,DE,,,f\n10.0.1.0/24,DE,,,f\n", ann(64500, "10.0.0.0/22"))
	if n != 0 {
		t.Errorf("later no-city rows: %d", n)
	}
}

func TestASNFeedCoverage(t *testing.T) {
	two := "10.0.0.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Berlin,f\n"
	// A /16 announced and two /24 rows (under 1% of it): no city.
	if _, _, n := runASNFeed(t, two, ann(1, "10.0.0.0/16")); n != 0 {
		t.Errorf("small coverage: %d", n)
	}
	// Rows covering half of the announced space (a /23 of a /22): a city.
	if got, _, n := runASNFeed(t, two, ann(1, "10.0.0.0/22")); n != 1 || got != "10.0.0.0/22,DE,,Berlin,asn:1\n" {
		t.Errorf("half coverage: %q", got)
	}
	// Just under half: no city.
	if _, _, n := runASNFeed(t, two, ann(1, "10.0.0.0/22", "10.1.0.0/24")); n != 0 {
		t.Errorf("under half: %d", n)
	}
	// An inner announced prefix of the same ASN counts once, through the outer one:
	// 512 of 1024 covers half; counting the /24 twice (1280) would not.
	if _, _, n := runASNFeed(t, two, ann(1, "10.0.0.0/22", "10.0.2.0/24")); n != 2 {
		t.Errorf("nested: %d", n)
	}
	// A prefix of another ASN inside the outer one does not reduce the outer weight.
	if _, _, n := runASNFeed(t, two, append(ann(1, "10.0.0.0/22"), ann(2, "10.0.2.0/24")...)); n != 1 {
		t.Errorf("other ASN inside: %d", n)
	}
	// An ASN with no announced prefix in a family has no weight: nothing, no panic.
	if _, _, n := runASNFeed(t, two, ann(1, "2001:db8::/32")); n != 0 {
		t.Errorf("other family: %d", n)
	}
}

func TestASNFeedAttribution(t *testing.T) {
	// MOAS: 10.2.0.0/16 is announced by two ASNs and is ignored entirely; its rows give nothing
	// (not even to the shorter prefix around it).
	moas := append(ann(1, "10.2.0.0/16", "10.0.0.0/8"), ann(2, "10.2.0.0/16")...)
	got, _, n := runASNFeed(t, "10.2.0.0/24,DE,,Berlin,f\n10.2.1.0/24,DE,,Berlin,f\n", moas)
	if n != 0 {
		t.Errorf("MOAS: %q", got)
	}
	// The same ASN listing a prefix twice is not MOAS.
	got, _, n = runASNFeed(t, "10.2.0.0/24,DE,,Berlin,f\n10.2.1.0/24,DE,,Berlin,f\n", append(ann(1, "10.2.0.0/22"), ann(1, "10.2.0.0/22")...))
	if n != 1 || !strings.Contains(got, "asn:1") {
		t.Errorf("same ASN twice: %q", got)
	}
	// A row broader than the announced prefix, or outside all of them, is ignored.
	_, _, n = runASNFeed(t, "10.0.0.0/8,DE,,Berlin,f\n10.0.0.0/15,DE,,Berlin,f\n11.0.0.0/24,DE,,Berlin,f\n", ann(1, "10.0.0.0/16"))
	if n != 0 {
		t.Errorf("broader or outside: %d", n)
	}
	// Longest announced prefix wins: the /24 rows belong to AS2, the rest of the /16 to AS1.
	a := append(ann(1, "10.0.0.0/21"), ann(2, "10.0.5.0/24")...)
	rows := "10.0.5.0/25,FR,,Paris,f\n10.0.5.128/25,FR,,Paris,f\n10.0.1.0/24,DE,,Berlin,f\n10.0.2.0/24,DE,,Berlin,f\n10.0.3.0/24,DE,,Berlin,f\n10.0.4.0/24,DE,,Berlin,f\n"
	got, asns, n := runASNFeed(t, rows, a)
	if got != "10.0.0.0/21,DE,,Berlin,asn:1\n10.0.5.0/24,FR,,Paris,asn:2\n" || asns != 2 || n != 2 {
		t.Errorf("longest match: %q %d %d", got, asns, n)
	}
	// Exact duplicates count once (last wins): without the dedupe Berlin would have 2 rows.
	_, _, n = runASNFeed(t, "10.0.1.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Berlin,f\n", ann(1, "10.0.0.0/22"))
	if n != 0 {
		t.Errorf("duplicate rows counted twice: %d", n)
	}
	got, _, n = runASNFeed(t, "10.0.1.0/24,DE,,Berlin,f\n10.0.2.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Hamburg,f\n", ann(1, "10.0.0.0/22"))
	if n != 0 {
		t.Errorf("last row must win: %q", got)
	}
}

func TestASNFeedFamilies(t *testing.T) {
	a := ann(7, "10.0.0.0/23", "2001:db8::/47", "2001:db9::/48")
	rows := "10.0.0.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Berlin,f\n" + // v4: Berlin
		"2001:db8::/48,FR,,Paris,f\n2001:db8:1::/48,FR,,Paris,f\n" // v6: Paris
	got, asns, n := runASNFeed(t, rows, a)
	if got != "10.0.0.0/23,DE,,Berlin,asn:7\n2001:db8::/47,FR,,Paris,asn:7\n2001:db9::/48,FR,,Paris,asn:7\n" || asns != 1 || n != 3 {
		t.Errorf("families: %q %d %d", got, asns, n)
	}
	// A family without a dominant city gets nothing, the other family still does.
	rows = "10.0.0.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Berlin,f\n2001:db8::/48,FR,,Paris,f\n2001:db8:1::/48,FR,,Lyon,f\n"
	got, _, n = runASNFeed(t, rows, a)
	if got != "10.0.0.0/23,DE,,Berlin,asn:7\n" || n != 1 {
		t.Errorf("one family: %q", got)
	}
	// IPv6 weight counts /64s: one /48 (65536 /64s) outweighs a /56 (256) and a /64 by far.
	rows = "2001:db8::/48,FR,,Paris,f\n2001:db8:1::/56,DE,,Berlin,f\n2001:db8:1:ff::/64,DE,,Berlin,f\n"
	got, _, n = runASNFeed(t, rows, ann(7, "2001:db8::/47"))
	if got != "" || n != 0 { // Paris has one row only
		t.Errorf("v6 weights: %q", got)
	}
	if w := prefixWeight(netip.MustParsePrefix("2001:db8::/32")); w != 1<<32 {
		t.Errorf("/32 weight = %v", w)
	}
	if prefixWeight(netip.MustParsePrefix("2001:db8::1/128")) != 1 || prefixWeight(netip.MustParsePrefix("10.0.0.0/24")) != 256 {
		t.Error("weights")
	}
}

// A row's city is shown as it is written most often.
func TestASNFeedSpelling(t *testing.T) {
	rows := "10.0.0.0/24,de,,Köln,f\n10.0.1.0/24,DE,,köln,f\n10.0.2.0/24,DE,,Köln,f\n"
	got, _, _ := runASNFeed(t, rows, ann(1, "10.0.0.0/22"))
	if got != "10.0.0.0/22,DE,,Köln,asn:1\n" {
		t.Errorf("got %q", got)
	}
}

func TestASNFeedBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feeds.csv.gz")
	writeGzip(t, path, "prefix,country\n10.0.0.0/24,DE\n")
	if _, _, err := asnFeedRows(path, nil, csv.NewWriter(&strings.Builder{})); err == nil {
		t.Error("no error for a file without city")
	}
	if _, _, err := asnFeedRows(path+".none", nil, csv.NewWriter(&strings.Builder{})); err == nil {
		t.Error("no error for a missing file")
	}
}

func TestAnnouncedFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), asnPrefixesFile)
	in := append(ann(1, "10.0.0.0/16"), ann(2, "2001:db8::/32")...)
	if err := writeAnnouncedFile(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := readAnnouncedFile(path)
	if err != nil || len(out) != 2 || out[0] != in[0] || out[1] != in[1] {
		t.Errorf("%v %v", out, err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("temporary file left")
	}
}

func asnFeedFixture(t *testing.T) (*Pipeline, []string) {
	p := &Pipeline{DataDir: t.TempDir()}
	var logs []string
	p.Progress = func(s string) { logs = append(logs, s) }
	os.MkdirAll(p.layersDir(), 0o750)
	return p, logs
}

func TestDownloadASNFeed(t *testing.T) {
	p, _ := asnFeedFixture(t)
	writeGzip(t, filepath.Join(p.layersDir(), FeedsLayer), feedHeader+"10.0.0.0/24,DE,,Berlin,f\n10.0.1.0/24,DE,,Berlin,f\n")
	calls := 0
	p.readAnnounced = func(context.Context) ([]asnPrefix, error) { calls++; return ann(1, "10.0.0.0/22"), nil }
	src := Source{Rows: map[string]int{}}
	if err := p.downloadASNFeed(context.Background(), p.layersDir(), &src); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || src.skipped(ASNFeedLayer) || src.ASNFeedASNs != 1 || src.Rows[ASNFeedLayer] != 1 {
		t.Fatalf("%d %+v", calls, src)
	}
	if got := readGzip(t, filepath.Join(p.layersDir(), ASNFeedLayer)); got != feedHeader+"10.0.0.0/22,DE,,Berlin,asn:1\n" {
		t.Errorf("layer = %q", got)
	}
	// The prefixes are saved outside the layers directory.
	saved, err := readAnnouncedFile(filepath.Join(p.dir(), asnPrefixesFile))
	if err != nil || len(saved) != 1 {
		t.Errorf("saved = %v %v", saved, err)
	}
	if names, _ := p.layerFiles(); len(names) != 2 {
		t.Errorf("layer files = %v", names)
	}

	// No dominant city: skipped, no file.
	os.Remove(filepath.Join(p.layersDir(), ASNFeedLayer))
	p.readAnnounced = func(context.Context) ([]asnPrefix, error) { return ann(1, "11.0.0.0/16"), nil }
	src = Source{Rows: map[string]int{}}
	if err := p.downloadASNFeed(context.Background(), p.layersDir(), &src); err != nil || !src.skipped(ASNFeedLayer) {
		t.Fatalf("no rows: %+v %v", src, err)
	}
	if _, err := os.Stat(filepath.Join(p.layersDir(), ASNFeedLayer)); err == nil {
		t.Error("a file for a skipped layer")
	}

	// No feeds layer: skipped without reading Postgres, and an old prefixes file is removed.
	os.Remove(filepath.Join(p.layersDir(), FeedsLayer))
	calls = 0
	p.readAnnounced = func(context.Context) ([]asnPrefix, error) { calls++; return nil, nil }
	src = Source{Rows: map[string]int{}}
	if err := p.downloadASNFeed(context.Background(), p.layersDir(), &src); err != nil || !src.skipped(ASNFeedLayer) || calls != 0 {
		t.Fatalf("no feeds: %+v %v %d", src, err, calls)
	}
	if _, err := os.Stat(filepath.Join(p.dir(), asnPrefixesFile)); err == nil {
		t.Error("old prefixes file kept")
	}

	// A database error fails.
	writeGzip(t, filepath.Join(p.layersDir(), FeedsLayer), feedHeader)
	p.readAnnounced = func(context.Context) ([]asnPrefix, error) { return nil, errors.New("boom") }
	if err := p.downloadASNFeed(context.Background(), p.layersDir(), &Source{Rows: map[string]int{}}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("db error = %v", err)
	}
}

func TestGeoArgsReplacements(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	touchLayers(t, p, RIRLayer, PDBLayer, ASNFeedLayer, FeedsLayer)
	layers := p.layersDir() + "/"
	join := func(replace map[string]string) string {
		got, err := p.geoArgs("o.mmdb", replace)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, " ")
	}
	got := join(map[string]string{ASNFeedLayer: "/h/" + ASNFeedLayer, FeedsLayer: "/h/" + FeedsLayer})
	if want := "-same-country " + PDBLayer + "," + ASNFeedLayer + " -within-first " + FeedsLayer + " " + layers + RIRLayer + " " + layers + PDBLayer + " /h/" + ASNFeedLayer + " /h/" + FeedsLayer; !strings.HasSuffix(got, want) {
		t.Errorf("replaced:\n%s", got)
	}
	// An empty path leaves the layer out, and out of its flag.
	got = join(map[string]string{ASNFeedLayer: ""})
	if strings.Contains(got, ASNFeedLayer) || !strings.Contains(got, "-same-country "+PDBLayer+" ") || !strings.HasSuffix(got, layers+FeedsLayer) {
		t.Errorf("omitted:\n%s", got)
	}
	// Omitting the only layer of a rule drops the flag.
	os.Remove(filepath.Join(p.layersDir(), PDBLayer))
	if got = join(map[string]string{ASNFeedLayer: ""}); strings.Contains(got, "-same-country") {
		t.Errorf("flag kept:\n%s", got)
	}
}

// The holdout build recomputes the ASN layer from the kept feeds: a held-out feed's
// city must not reach it.
func TestPrepareHoldoutASNFeed(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"), 0o755)
	p := &Pipeline{DataDir: dir, MMDBBuilder: script, heldOutURLs: heldOutSet(testHeldOut)}
	layers := p.layersDir()
	os.MkdirAll(layers, 0o750)
	writeGzip(t, filepath.Join(layers, RIRLayer), feedHeader)
	// Kept feed0: eight Berlin rows in AS1 (half of its /20). Held-out feed1: eight Paris rows in AS1 (without the holdout there is no dominant city).
	feeds := feedHeader
	for i := 0; i < 8; i++ {
		feeds += fmt.Sprintf("10.0.%d.0/24,DE,,Berlin,https://feed0.example/geo.csv\n", i)
	}
	for i := 8; i < 16; i++ {
		feeds += fmt.Sprintf("10.0.%d.0/24,FR,,Paris,https://feed1.example/geo.csv\n", i)
	}
	writeGzip(t, filepath.Join(layers, FeedsLayer), feeds)
	announced := ann(1, "10.0.0.0/20")
	if err := writeAnnouncedFile(filepath.Join(p.dir(), asnPrefixesFile), announced); err != nil {
		t.Fatal(err)
	}
	// The main layer, as Download wrote it (from all the feeds, here a made-up Paris).
	writeGzip(t, filepath.Join(layers, ASNFeedLayer), feedHeader+"10.0.0.0/20,FR,,Paris,asn:1\n")

	eval := p.evalDir()
	os.MkdirAll(eval, 0o750)
	mmdb, _, status, err := p.prepareHoldout(context.Background(), eval)
	if err != nil || mmdb == "" {
		t.Fatalf("%q %q %v", mmdb, status, err)
	}
	own := filepath.Join(eval, "holdout", ASNFeedLayer)
	if got := readGzip(t, own); got != feedHeader+"10.0.0.0/20,DE,,Berlin,asn:1\n" {
		t.Errorf("holdout layer = %q", got)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), " "+own+" ") || strings.Contains(string(args), filepath.Join(layers, ASNFeedLayer)) ||
		!strings.Contains(string(args), "-same-country "+ASNFeedLayer) || strings.Contains(status, "left out") {
		t.Errorf("args = %s status = %q", args, status)
	}

	// No saved prefixes: the layer is left out, with its flag, and the status says so.
	os.Remove(filepath.Join(p.dir(), asnPrefixesFile))
	os.Remove(argsFile)
	_, _, status, err = p.prepareHoldout(context.Background(), eval)
	args, _ = os.ReadFile(argsFile)
	if err != nil || strings.Contains(string(args), "38-") || strings.Contains(string(args), "-same-country") || !strings.Contains(status, ASNFeedLayer+" left out") {
		t.Errorf("no prefixes: %v args = %s status = %q", err, args, status)
	}

	// The kept feeds give no dominant city: left out as well.
	writeAnnouncedFile(filepath.Join(p.dir(), asnPrefixesFile), ann(2, "20.0.0.0/16"))
	os.Remove(argsFile)
	_, _, status, err = p.prepareHoldout(context.Background(), eval)
	args, _ = os.ReadFile(argsFile)
	if err != nil || strings.Contains(string(args), "38-") || !strings.Contains(status, "no rows") {
		t.Errorf("no rows: %v args = %s status = %q", err, args, status)
	}
}
