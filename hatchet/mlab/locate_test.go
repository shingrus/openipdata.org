package mlab

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

const (
	cl1 = 0x080808 // 8.8.8.0/24
	cl2 = 0x080809
	cl3 = 0x08080A
)

func pl(country, city string) place { return place{country, city} }

func votePairs(ps ...pair) []pair { sortPairs(ps); return ps }

func TestVoteWeightedMajority(t *testing.T) {
	h1, h2, h3 := ipv("1.1.1.1"), ipv("1.1.1.2"), ipv("1.1.1.3")
	rule := map[uint32]place{h1: pl("DE", "Berlin"), h2: pl("DE", "berlin"), h3: pl("DE", "Munich")}
	pairs := votePairs(
		// Two hops say Berlin (spelled two ways, 3+2 traces), one says Munich (4): Berlin wins on weight.
		makePair(cl1, 0, h1, 3), makePair(cl1, 1, h2, 2), makePair(cl1, 2, h3, 4),
		// Munich 4 against Berlin 3: the weight, not the number of hops.
		makePair(cl2, 0, h1, 3), makePair(cl2, 1, h3, 4),
	)
	rows, st := vote(pairs, rule, nil, nil)
	if len(rows) != 2 || rows[0] != (layerRow{cl1, "DE", "Berlin"}) || rows[1] != (layerRow{cl2, "DE", "Munich"}) {
		t.Errorf("rows = %+v", rows)
	}
	if st != (voteStats{Clients: 2, Located: 2, ByRule: 2}) {
		t.Errorf("stats = %+v", st)
	}
}

func TestVoteTieGivesNone(t *testing.T) {
	h1, h2 := ipv("1.1.1.1"), ipv("1.1.1.2")
	rule := map[uint32]place{h1: pl("DE", "Berlin"), h2: pl("DE", "Munich")}
	rows, st := vote(votePairs(makePair(cl1, 0, h1, 4), makePair(cl1, 0, h2, 4)), rule, nil, nil)
	if len(rows) != 0 || st.Ties != 1 || st.Clients != 1 || st.Located != 0 || st.NoCity != 0 {
		t.Errorf("rows %+v stats %+v", rows, st)
	}
	// A third city with less weight does not break the tie; a heavier one does.
	h3 := ipv("1.1.1.3")
	rule[h3] = pl("DE", "Bonn")
	rows, st = vote(votePairs(makePair(cl1, 0, h1, 4), makePair(cl1, 0, h2, 4), makePair(cl1, 0, h3, 1)), rule, nil, nil)
	if len(rows) != 0 || st.Ties != 1 {
		t.Errorf("rows %+v stats %+v", rows, st)
	}
	rows, _ = vote(votePairs(makePair(cl1, 0, h1, 4), makePair(cl1, 0, h2, 4), makePair(cl1, 0, h3, 5)), rule, nil, nil)
	if len(rows) != 1 || rows[0].City != "Bonn" {
		t.Errorf("rows %+v", rows)
	}
}

// R then M: the rule round decides when it has a winner, whatever the MMDB says; when it has none
// (no rule city, or a tie) the MMDB round decides; an MMDB city is not used for a hop inside the
// client /24 itself, a rule city is.
func TestVoteRuleRoundThenMMDBRound(t *testing.T) {
	hRule, hMMDB, hOwn := ipv("1.1.1.1"), ipv("1.1.1.3"), uint32(cl1<<8|9)
	rule := map[uint32]place{hRule: pl("FR", "Paris")}
	mmdb := map[uint32]place{hRule: pl("FR", "Lyon"), hMMDB: pl("FR", "Lyon"), hOwn: pl("FR", "Nice")}
	// The rule hop (Paris, 2) wins the first round although the MMDB hops (Lyon 3+2) outweigh it.
	rows, st := vote(votePairs(makePair(cl1, 0, hRule, 2), makePair(cl1, 2, hMMDB, 3)), rule, mmdb, nil)
	if len(rows) != 1 || rows[0].City != "Paris" || st.ByRule != 1 || st.ByMMDB != 0 {
		t.Errorf("rows %+v stats %+v", rows, st)
	}
	// No rule hop: the MMDB round; counted as an MMDB winner.
	rows, st = vote(votePairs(makePair(cl2, 0, hMMDB, 2)), rule, mmdb, nil)
	if len(rows) != 1 || rows[0].City != "Lyon" || st.ByMMDB != 1 || st.ByRule != 0 {
		t.Errorf("rows %+v stats %+v", rows, st)
	}
	// A tie in the rule round falls to the MMDB round.
	h2 := ipv("1.1.1.2")
	rule2 := map[uint32]place{hRule: pl("FR", "Paris"), h2: pl("FR", "Nantes")}
	mmdb2 := map[uint32]place{hMMDB: pl("FR", "Lyon")}
	rows, st = vote(votePairs(makePair(cl1, 0, hRule, 2), makePair(cl1, 1, h2, 2), makePair(cl1, 2, hMMDB, 1)), rule2, mmdb2, nil)
	if len(rows) != 1 || rows[0].City != "Lyon" || st.ByMMDB != 1 || st.Ties != 0 {
		t.Errorf("tie then MMDB: rows %+v stats %+v", rows, st)
	}
	// A tie in the rule round and nothing in the MMDB round is a tie.
	rows, st = vote(votePairs(makePair(cl1, 0, hRule, 2), makePair(cl1, 1, h2, 2)), rule2, nil, nil)
	if len(rows) != 0 || st.Ties != 1 {
		t.Errorf("tie: rows %+v stats %+v", rows, st)
	}
	// A hop inside the client /24 gives no MMDB vote, a rule vote it does.
	rows, st = vote(votePairs(makePair(cl1, 0, hOwn, 9)), nil, mmdb, nil)
	if len(rows) != 0 || st.NoCity != 1 {
		t.Errorf("own hop: rows %+v stats %+v", rows, st)
	}
	rows, _ = vote(votePairs(makePair(cl1, 0, hOwn, 9)), map[uint32]place{hOwn: pl("FR", "Paris")}, mmdb, nil)
	if len(rows) != 1 || rows[0].City != "Paris" {
		t.Errorf("own hop with a rule: %+v", rows)
	}
	// No hop city at all: the /24 is counted and skipped.
	rows, st = vote(votePairs(makePair(cl3, 0, ipv("9.9.9.9"), 1)), rule, mmdb, nil)
	if len(rows) != 0 || st.Clients != 1 || st.NoCity != 1 {
		t.Errorf("rows %+v stats %+v", rows, st)
	}
}

// With the clients' countries known only a city in the client's country votes, and a client with
// no country does not vote.
func TestVoteCountryFilter(t *testing.T) {
	h1, h2 := ipv("1.1.1.1"), ipv("1.1.1.2")
	rule := map[uint32]place{h1: pl("NL", "Amsterdam"), h2: pl("BE", "Brussels")}
	pairs := votePairs(makePair(cl1, 0, h1, 9), makePair(cl1, 1, h2, 2), makePair(cl2, 0, h1, 9), makePair(cl3, 0, h1, 9))
	countries := map[uint32][2]byte{cl1: {'B', 'E'}, cl2: {'F', 'R'}}
	rows, st := vote(pairs, rule, nil, countries)
	// cl1: Amsterdam is another country than BE, so Brussels (2) wins; cl2 (FR): none; cl3: no country.
	if len(rows) != 1 || rows[0] != (layerRow{cl1, "BE", "Brussels"}) || st.NoCity != 1 || st.NoCountry != 1 || st.Clients != 3 {
		t.Errorf("rows %+v stats %+v", rows, st)
	}
	// Without countries (no MMDB) there is no filter.
	rows, _ = vote(pairs, rule, nil, nil)
	if len(rows) != 3 || rows[0].City != "Amsterdam" {
		t.Errorf("no filter: %+v", rows)
	}
}

// The M part uses an allowlist of strong sources only; the clients' countries come from the .1 rows.
func TestReadLookupAllowlistAndCountries(t *testing.T) {
	var in strings.Builder
	strong := []string{"70-geofeeds", "60-whois", "40-hosting", "50-ix", "38-asn-geofeed"}
	other := []string{"36-mlab", "25-bgp", "30-pdb", "35-rdns-wide", "65-rdns", "67-rdns-names", "80-asn-main", "", "10-rir"}
	var need []uint32
	i := uint32(2)
	add := func(source string) {
		ip := ipv("1.1.1.0") + i
		need = append(need, ip)
		in.WriteString(addrOf(ip).String() + "\tde\tBerlin\t" + source + "\n")
		i += 2 // never a .1
	}
	for _, s := range strong {
		add(s)
	}
	for _, s := range other {
		add(s)
	}
	in.WriteString("1.1.1.250\tGermany\tBerlin\t70-geofeeds\n1.1.1.252\tDE\t\t70-geofeeds\n") // bad country, no city
	need = append(need, ipv("1.1.1.250"), ipv("1.1.1.252"))
	in.WriteString("1.1.1.254\tDE\tBerlin\t70-geofeeds\n") // a strong row of an IP that is not asked for
	// Client countries: the .1 of a client /24 (here 8.8.8 and 8.8.9; 8.8.10 has no row); another IP of a client does not count.
	in.WriteString("8.8.8.1\tUS\t\t\n8.8.9.1\tfr\tParis\t70-geofeeds\n8.8.9.7\tDE\t\t\nbad line\n::1\tUS\t\t\n")
	sortedNeed := append([]uint32{}, need...)
	sortU32(sortedNeed)
	places, countries, err := readLookup(strings.NewReader(in.String()), sortedNeed, []uint32{cl1, cl2, cl3})
	if err != nil {
		t.Fatal(err)
	}
	if len(places) != len(strong) {
		t.Errorf("kept %d places, want the %d strong ones: %v", len(places), len(strong), places)
	}
	for k := 0; k < len(strong); k++ {
		if places[ipv("1.1.1.0")+uint32(2+2*k)] != pl("DE", "Berlin") {
			t.Errorf("source %s dropped", strong[k])
		}
	}
	if len(countries) != 2 || countries[cl1] != [2]byte{'U', 'S'} || countries[cl2] != [2]byte{'F', 'R'} {
		t.Errorf("countries = %v", countries)
	}
}

func sortU32(v []uint32) { sort.Slice(v, func(i, j int) bool { return v[i] < v[j] }) }

func TestRulePlaces(t *testing.T) {
	names := map[uint32]string{ipv("1.1.1.1"): "ae1.fra.example.net", ipv("1.1.1.2"): "x.example.net", ipv("1.1.1.3"): "ae2.xx.example.net"}
	decode := func(name string) (string, string, bool) {
		switch {
		case strings.Contains(name, ".fra."):
			return "de", "Frankfurt", true
		case strings.Contains(name, ".xx."):
			return "Germany", "Nowhere", true // an invalid country is no city
		}
		return "", "", false
	}
	places, named := rulePlaces([]uint32{ipv("1.1.1.1"), ipv("1.1.1.2"), ipv("1.1.1.3"), ipv("1.1.1.4")}, names, decode)
	if named != 3 || len(places) != 1 || places[ipv("1.1.1.1")] != pl("DE", "Frankfurt") {
		t.Errorf("places %v named %d", places, named)
	}
	if places, named = rulePlaces([]uint32{ipv("1.1.1.1")}, names, nil); len(places) != 0 || named != 1 {
		t.Errorf("no decoder: %v %d", places, named)
	}
}

func TestWriteLayerFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), LayerFile)
	rows := []layerRow{{cl1, "DE", "Berlin"}, {cl2, "FR", "Saint-Étienne, Loire"}}
	if _, err := writeLayer(path, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temporary file left")
	}
	f, _ := os.Open(path)
	defer f.Close()
	r, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	want := "prefix,country,region,city,source\n8.8.8.0/24,DE,,Berlin,mlab\n8.8.9.0/24,FR,,\"Saint-Étienne, Loire\",mlab\n"
	if string(data) != want {
		t.Errorf("layer:\n%s\nwant:\n%s", data, want)
	}
}

// fakeObjects serves the dated MMDB key: today's, else yesterday's.
type fakeObjects struct {
	have       map[string]bool
	downloaded int
}

func (f *fakeObjects) Stat(_ context.Context, key string) (publish.ObjectInfo, error) {
	if !f.have[key] {
		return publish.ObjectInfo{}, publish.ErrNotFound
	}
	return publish.ObjectInfo{ETag: "etag-1"}, nil
}

func (f *fakeObjects) DownloadIfChanged(_ context.Context, key, path, etag string) (publish.ObjectInfo, bool, error) {
	if etag == "etag-1" {
		return publish.ObjectInfo{ETag: "etag-1"}, false, nil
	}
	f.downloaded++
	return publish.ObjectInfo{ETag: "etag-1"}, true, os.WriteFile(path, []byte("mmdb"), 0o640)
}

// lookupAnswer is a fake "mmdbbuilder lookup -with-source -countries": a row for every IP asked
// that has an answer in hopRows (ip -> "country\tcity\tsource"), and for every client .1 (an
// address ending in .1 outside 1.1.1.0/24) the country clientCountry with no city.
func lookupAnswer(t *testing.T, in, out string, hopRows map[string]string, clientCountry string) {
	t.Helper()
	data, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, ip := range strings.Fields(string(data)) {
		if row, ok := hopRows[ip]; ok {
			b.WriteString(ip + "\t" + row + "\n")
		} else if strings.HasSuffix(ip, ".1") && !strings.HasPrefix(ip, "1.1.1.") && clientCountry != "" {
			b.WriteString(ip + "\t" + clientCountry + "\t\t\n")
		}
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o640); err != nil {
		t.Fatal(err)
	}
}

// locateWorld is a pairs file of 1500 client /24s (137.x.y.0/24, in DE): each has hop A (a rule says
// Berlin, 5 traces) and hop B (MMDB says Munich from a strong source, 3 traces) and, for the first
// 600, hop C (MMDB says Paris from 25-bgp: never counts).
func locateWorld(t *testing.T) *Pipeline {
	t.Helper()
	p := &Pipeline{DataDir: t.TempDir(), MMDBBuilder: "unused", R2: "unused"}
	var pairs []pair
	hopA, hopB, hopC := ipv("1.1.1.1"), ipv("1.1.1.2"), ipv("1.1.1.3")
	for i := 0; i < 1500; i++ {
		c := uint32(0x890000 + i) // 137.0.0.0 and up: public
		pairs = append(pairs, makePair(c, 0, hopA, 5), makePair(c, 1, hopB, 3))
		if i < 600 {
			pairs = append(pairs, makePair(c, 2, hopC, 9))
		}
	}
	sortPairs(pairs)
	os.MkdirAll(p.dir(), 0o750)
	if _, err := writePairs(p.pairsPath(), pairs); err != nil {
		t.Fatal(err)
	}
	p.readNames = func(context.Context, []uint32) (map[uint32]string, error) {
		return map[uint32]string{hopA: "ae1.ber.example.net"}, nil
	}
	p.loadDecoder = func(context.Context) (decodeFunc, error) {
		return func(name string) (string, string, bool) {
			if strings.Contains(name, ".ber.") {
				return "DE", "Berlin", true
			}
			return "", "", false
		}, nil
	}
	p.lookup = func(_ context.Context, db, in, out string) error {
		lookupAnswer(t, in, out, map[string]string{"1.1.1.2": "DE\tMunich\t70-geofeeds", "1.1.1.3": "FR\tParis\t25-bgp"}, "DE")
		return nil
	}
	return p
}

func TestLocateRulesAndMMDB(t *testing.T) {
	p := locateWorld(t)
	objects := &fakeObjects{have: map[string]bool{}}
	p.store = objects
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	objects.have["2026-11-14/ip2geo-openipdata-20261114.mmdb"] = true // today's is not there yet: yesterday's
	var asked []string
	inner := p.lookup
	p.lookup = func(ctx context.Context, db, in, out string) error {
		data, _ := os.ReadFile(in)
		asked = strings.Fields(string(data))
		return inner(ctx, db, in, out)
	}
	out, err := p.Locate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The rule (Berlin 5) wins the first round in every /24; MMDB hops: B only (C is 25-bgp).
	if out.Clients != 1500 || out.Hops != 3 || out.Named != 1 || out.RuleHops != 1 || out.MMDBHops != 1 ||
		out.Located != 1500 || out.ByRule != 1500 || out.ByMMDB != 0 || out.Ties != 0 || out.NoCountry != 0 || out.MMDB != "used" || out.MMDBReason != "" {
		t.Errorf("output %+v", out)
	}
	// Asked: the three hops, then the .1 of each client /24.
	if len(asked) != 3+1500 || asked[0] != "1.1.1.1" || asked[3] != "137.0.0.1" || asked[len(asked)-1] != "137.5.219.1" {
		t.Errorf("asked %d: %v ...", len(asked), asked[:5])
	}
	if objects.downloaded != 1 {
		t.Errorf("downloaded %d", objects.downloaded)
	}
	if _, err := os.Stat(filepath.Join(p.dir(), "ip2geo.mmdb")); err != nil {
		t.Error("the MMDB copy is not in the mlab dir")
	}
	for _, f := range []string{hopsInFile, hopsOutFile} {
		if _, err := os.Stat(filepath.Join(p.dir(), f)); err == nil {
			t.Errorf("%s left behind", f)
		}
	}
	data := readLayer(t, p.LayerPath())
	if !strings.HasPrefix(data, "prefix,country,region,city,source\n137.0.0.0/24,DE,,Berlin,mlab\n") || strings.Count(data, "\n") != 1501 {
		t.Errorf("layer starts %q (%d lines)", data[:min(90, len(data))], strings.Count(data, "\n"))
	}
	// A second run reuses the saved copy (same ETag).
	if _, err := p.Locate(context.Background()); err != nil || objects.downloaded != 1 {
		t.Errorf("second run: %v downloaded %d", err, objects.downloaded)
	}
}

// Without a rule decoder only the MMDB round works; weights decide; the country of the client /24
// from the MMDB is checked.
func TestLocateMMDBOnlyAndSkips(t *testing.T) {
	p := locateWorld(t)
	p.loadDecoder = func(context.Context) (decodeFunc, error) { return nil, nil }
	p.readNames = func(context.Context, []uint32) (map[uint32]string, error) { return nil, nil }
	hops := map[string]string{"1.1.1.1": "DE\tBerlin\t60-whois", "1.1.1.2": "DE\tMunich\t70-geofeeds", "1.1.1.3": "FR\tParis\t36-mlab"}
	p.lookup = func(_ context.Context, db, in, out string) error { lookupAnswer(t, in, out, hops, "DE"); return nil }
	p.store = &fakeObjects{have: map[string]bool{mmdbKeyOf(time.Now()): true}}
	out, err := p.Locate(context.Background())
	// Berlin 5 against Munich 3: Berlin, by MMDB; the layer's own 36-mlab source never counts.
	if err != nil || out.Located != 1500 || out.ByMMDB != 1500 || out.ByRule != 0 || out.RuleHops != 0 || out.MMDBHops != 2 {
		t.Fatalf("%+v %v", out, err)
	}
	if !strings.Contains(readLayer(t, p.LayerPath()), "137.0.0.0/24,DE,,Berlin,mlab\n") {
		t.Error("wrong layer")
	}

	// Clients in another country than the hops' cities: nobody votes.
	f := locateWorld(t)
	f.minLayerRows = 1
	f.store = &fakeObjects{have: map[string]bool{mmdbKeyOf(time.Now()): true}}
	f.lookup = func(_ context.Context, db, in, out string) error {
		lookupAnswer(t, in, out, map[string]string{"1.1.1.2": "DE\tMunich\t70-geofeeds"}, "FR")
		return nil
	}
	if _, err := f.Locate(context.Background()); !errors.Is(err, ErrPermanent) {
		t.Errorf("country mismatch: %v", err)
	}
	// Clients with no country in the MMDB do not vote.
	n := locateWorld(t)
	n.minLayerRows = 1
	n.store = &fakeObjects{have: map[string]bool{mmdbKeyOf(time.Now()): true}}
	n.lookup = func(_ context.Context, db, in, out string) error {
		lookupAnswer(t, in, out, map[string]string{"1.1.1.2": "DE\tMunich\t70-geofeeds"}, "")
		return nil
	}
	if _, err := n.Locate(context.Background()); !errors.Is(err, ErrPermanent) {
		t.Errorf("no client country: %v", err)
	}

	// No MMDB (R2 holds none): the MMDB part is skipped and reported; the rules still work, with no
	// country check.
	q := locateWorld(t)
	q.store = &fakeObjects{have: map[string]bool{}}
	out, err = q.Locate(context.Background())
	if err != nil || out.MMDB != "skipped" || out.MMDBReason == "" || out.MMDBHops != 0 || out.Located != 1500 || out.ByRule != 1500 {
		t.Errorf("no MMDB: %+v %v", out, err)
	}
	// No R2 setting and no builder: skipped, never an error.
	r := locateWorld(t)
	r.R2, r.MMDBBuilder, r.lookup = "", "", nil
	out, err = r.Locate(context.Background())
	if err != nil || out.MMDB != "skipped" || out.MMDBReason != reasonNoBuilder || out.Located != 1500 {
		t.Errorf("no builder: %+v %v", out, err)
	}
	// A failing mmdbbuilder skips the MMDB part too, and the error text carries no IP.
	var logs []string
	s := locateWorld(t)
	s.Progress = func(m string) { logs = append(logs, m) }
	s.store = &fakeObjects{have: map[string]bool{mmdbKeyOf(time.Now()): true}}
	s.lookup = func(context.Context, string, string, string) error { return errors.New("boom 1.2.3.4") }
	out, err = s.Locate(context.Background())
	if err != nil || out.MMDB != "skipped" || out.MMDBReason != reasonLookup || out.Located != 1500 {
		t.Errorf("failing lookup: %+v %v", out, err)
	}
	if strings.Contains(strings.Join(logs, "\n"), "1.2.3.4") {
		t.Errorf("an IP in the log: %q", logs)
	}
}

func mmdbKeyOf(day time.Time) string {
	day = day.UTC()
	return day.Format("2006-01-02") + "/ip2geo-openipdata-" + day.Format("20060102") + ".mmdb"
}

func readLayer(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	io.Copy(&b, r)
	return b.String()
}

// Fewer than the minimum of rows fails clearly and leaves the old layer file as it was.
func TestLocateTooFewRowsKeepsOldLayer(t *testing.T) {
	p := locateWorld(t)
	p.store = &fakeObjects{have: map[string]bool{}}
	p.minLayerRows = 5000
	old := []byte("old layer")
	os.WriteFile(p.LayerPath(), old, 0o640)
	if _, err := p.Locate(context.Background()); !errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "5000") {
		t.Errorf("error = %v", err)
	}
	if data, _ := os.ReadFile(p.LayerPath()); !bytes.Equal(data, old) {
		t.Errorf("the old layer changed: %q", data)
	}
	if _, err := os.Stat(p.LayerPath() + ".tmp"); err == nil {
		t.Error("temporary file left")
	}
	// The default minimum is 1000 rows.
	q := &Pipeline{DataDir: t.TempDir()}
	os.MkdirAll(q.dir(), 0o750)
	writePairs(q.pairsPath(), []pair{makePair(cl1, 0, ipv("1.1.1.1"), 1)})
	q.readNames = func(context.Context, []uint32) (map[uint32]string, error) { return nil, nil }
	q.loadDecoder = func(context.Context) (decodeFunc, error) {
		return func(string) (string, string, bool) { return "DE", "Berlin", true }, nil
	}
	if _, err := q.Locate(context.Background()); !errors.Is(err, ErrPermanent) {
		t.Errorf("default minimum: %v", err)
	}
	if _, err := (&Pipeline{DataDir: t.TempDir()}).Locate(context.Background()); !errors.Is(err, ErrPermanent) {
		t.Errorf("no pairs: %v", err)
	}
}
