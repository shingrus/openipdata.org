package rdns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

// hopCities builds the answer of the MMDB: ip, country, city, ip, country, city, ...
func hopCities(triples ...string) map[netip.Addr]cityKey {
	m := map[netip.Addr]cityKey{}
	for i := 0; i < len(triples); i += 3 {
		m[netip.MustParseAddr(triples[i])] = cityKey{triples[i+1], triples[i+2]}
	}
	return m
}

// Five public hops: the last two are candidates, so a trace has [target, last hop, second to last].
func twoHopTrace(prefix, kind, target, second, last string) traceIn {
	return tr(prefix, kind, target, "11.0.0.1", "11.0.0.2", "11.0.0.3", second, last)
}

// A /24 that no rule matches votes the city of a router hop, and gets no traced row.
func TestLocateMMDBFallbackVotes(t *testing.T) {
	o, rules := locateWorld(t)
	traces := []traceIn{twoHopTrace("5.6.7.0/24", kindVisitor, "5.6.7.9", "9.9.9.1", "5.6.0.2")}
	rows, out := locate(traces, nil, rules, o, hopCities("5.6.0.2", "DE", "Frankfurt"), nil)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	w := rowFor(rows, "5.6.0.0/16", scopeWide)
	if w == nil || w.Country != "DE" || w.City != "Frankfurt" || w.Method != methodMMDB || w.RuleVotes != 0 || w.MMDBVotes != 1 ||
		w.RouterIP.IsValid() || w.SameASN != nil {
		t.Errorf("wide row = %+v", w)
	}
	if out.Traced != 0 || out.Wide != 1 || out.Votes != 1 || out.MMDBVotes != 1 || out.MMDBCompared != 0 {
		t.Errorf("out = %+v", out)
	}
	if k := out.Kinds[kindVisitor]; k.Located != 0 {
		t.Errorf("an MMDB city is not a rule match: %+v", k)
	}
}

// Same-ASN hops before closer ones of another ASN; the traces by kind; never the target.
func TestLocateMMDBFallbackOrder(t *testing.T) {
	o, rules := locateWorld(t)
	p := "5.6.7.0/24"
	frankfurt, amsterdam, munich := cityKey{"DE", "Frankfurt"}, cityKey{"NL", "Amsterdam"}, cityKey{"DE", "Munich"}
	for name, c := range map[string]struct {
		traces []traceIn
		hops   map[netip.Addr]cityKey
		want   *cityKey
	}{
		"same ASN first": {
			// Candidates [target, 9.9.9.1 (AS200), 5.6.0.2 (AS100)]: the own network is tried first.
			[]traceIn{twoHopTrace(p, kindVisitor, "5.6.7.9", "5.6.0.2", "9.9.9.1")},
			hopCities("9.9.9.1", "NL", "Amsterdam", "5.6.0.2", "DE", "Frankfurt"), &frankfurt,
		},
		"other ASN when it is all there is": {
			[]traceIn{twoHopTrace(p, kindVisitor, "5.6.7.9", "5.6.0.2", "9.9.9.1")},
			hopCities("9.9.9.1", "NL", "Amsterdam"), &amsterdam,
		},
		"closest first": {
			// Candidates [target, 5.6.0.3, 5.6.0.2]: both in AS100, the closer one first.
			[]traceIn{twoHopTrace(p, kindVisitor, "5.6.7.9", "5.6.0.2", "5.6.0.3")},
			hopCities("5.6.0.2", "DE", "Frankfurt", "5.6.0.3", "DE", "Munich"), &munich,
		},
		"kind order, whatever the order of the traces": {
			[]traceIn{
				twoHopTrace(p, kindRandom, "5.6.7.50", "5.6.0.8", "5.6.0.8"),
				twoHopTrace(p, kindGateway, "5.6.7.1", "5.6.0.3", "5.6.0.3"),
				twoHopTrace(p, kindVisitor, "5.6.7.9", "5.6.0.2", "5.6.0.2"),
			},
			hopCities("5.6.0.8", "NL", "Amsterdam", "5.6.0.3", "DE", "Munich", "5.6.0.2", "DE", "Frankfurt"), &frankfurt,
		},
		"the next trace when the first has no city": {
			[]traceIn{
				twoHopTrace(p, kindVisitor, "5.6.7.9", "5.6.0.2", "5.6.0.2"),
				twoHopTrace(p, kindGateway, "5.6.7.1", "5.6.0.3", "5.6.0.3"),
			},
			hopCities("5.6.0.3", "DE", "Munich"), &munich,
		},
		"never the target itself": {
			[]traceIn{twoHopTrace(p, kindVisitor, "5.6.7.9", "5.6.0.2", "5.6.0.2")},
			hopCities("5.6.7.9", "DE", "Frankfurt"), nil,
		},
		"no city for any hop": {
			[]traceIn{twoHopTrace(p, kindVisitor, "5.6.7.9", "5.6.0.2", "5.6.0.2")},
			hopCities("5.6.0.99", "DE", "Frankfurt"), nil,
		},
		"no map": {
			[]traceIn{twoHopTrace(p, kindVisitor, "5.6.7.9", "5.6.0.2", "5.6.0.2")},
			nil, nil,
		},
	} {
		rows, out := locate(c.traces, nil, rules, o, c.hops, nil)
		w := rowFor(rows, "5.6.0.0/16", scopeWide)
		switch {
		case c.want == nil && (w != nil || out.MMDBVotes != 0 || out.Votes != 0):
			t.Errorf("%s: expected no vote: %+v %+v", name, w, out)
		case c.want != nil && (w == nil || w.Country != c.want.Country || w.City != c.want.City || out.MMDBVotes != 1):
			t.Errorf("%s: wide row = %+v, out = %+v, want %+v", name, w, out, *c.want)
		}
		if len(rows) > 1 {
			t.Errorf("%s: more than a wide row: %+v", name, rows)
		}
	}
}

// A /24 whose target has no origin ASN casts no vote: no announced prefix to give it to.
func TestLocateMMDBFallbackNeedsASN(t *testing.T) {
	o, rules := locateWorld(t)
	traces := []traceIn{twoHopTrace("77.1.2.0/24", kindVisitor, "77.1.2.9", "5.6.0.2", "5.6.0.2")}
	rows, out := locate(traces, nil, rules, o, hopCities("5.6.0.2", "DE", "Frankfurt"), nil)
	if len(rows) != 0 || out.MMDBVotes != 0 || out.Votes != 0 || out.NoASN != 1 {
		t.Errorf("rows = %+v, out = %+v", rows, out)
	}
}

// A rule match is never replaced by the MMDB; the MMDB city of the winning router is only compared.
func TestLocateMMDBAgreement(t *testing.T) {
	o, rules := locateWorld(t)
	traces := []traceIn{
		tr("5.6.1.0/24", kindVisitor, "5.6.1.9", "5.6.0.1"), // fra by rule; the MMDB says Frankfurt too
		tr("5.6.2.0/24", kindVisitor, "5.6.2.9", "5.6.0.2"), // fra by rule; the MMDB says Munich
		tr("5.6.3.0/24", kindVisitor, "5.6.3.9", "5.6.0.3"), // fra by rule; the MMDB has no city
		tr("5.6.4.0/24", kindVisitor, "5.6.4.9", "5.6.0.4"), // the target's own name matches: not a hop, not compared
	}
	ptrs := names("5.6.0.1", "a.fra1.isp.net", "5.6.0.2", "a.fra2.isp.net", "5.6.0.3", "a.fra3.isp.net", "5.6.4.9", "h.fra4.isp.net")
	hops := hopCities("5.6.0.1", "DE", "Frankfurt", "5.6.0.2", "DE", "Munich", "5.6.4.9", "DE", "Munich", "5.6.0.4", "DE", "Munich")
	rows, out := locate(traces, ptrs, rules, o, hops, nil)
	if out.MMDBCompared != 2 || out.MMDBAgreed != 1 || out.MMDBVotes != 0 {
		t.Errorf("out = %+v", out)
	}
	w := rowFor(rows, "5.6.0.0/16", scopeWide)
	if w == nil || w.City != "Frankfurt" || w.Method != methodRule || w.RuleVotes != 4 || w.MMDBVotes != 0 {
		t.Errorf("wide row = %+v", w)
	}
	for _, r := range rows {
		if r.Scope == scopeTraced && (r.Method != methodRule || r.City != "Frankfurt") {
			t.Errorf("traced row = %+v", r)
		}
	}
}

// Rule votes and MMDB votes count the same; the method says whether a rule took part.
func TestLocateMMDBMixedVotes(t *testing.T) {
	o, rules := locateWorld(t)
	ptrs := names("5.6.0.1", "a.fra1.isp.net")
	traces := func(extra ...traceIn) []traceIn {
		return append([]traceIn{tr("5.6.1.0/24", kindVisitor, "5.6.1.9", "5.6.0.1")}, extra...)
	}
	hops := hopCities("5.6.0.2", "DE", "Frankfurt", "5.6.0.3", "DE", "Munich", "5.6.0.4", "DE", "Munich")
	// One rule vote and one MMDB vote for Frankfurt against one MMDB vote for Munich.
	rows, out := locate(traces(tr("5.6.2.0/24", kindVisitor, "5.6.2.9", "5.6.0.2"), tr("5.6.3.0/24", kindVisitor, "5.6.3.9", "5.6.0.3")), ptrs, rules, o, hops, nil)
	w := rowFor(rows, "5.6.0.0/16", scopeWide)
	if w == nil || w.City != "Frankfurt" || w.Method != methodRule || w.RuleVotes != 1 || w.MMDBVotes != 1 || out.Votes != 3 || out.MMDBVotes != 2 {
		t.Errorf("mixed: %+v %+v", w, out)
	}
	// MMDB votes only: two for Munich.
	rows, _ = locate([]traceIn{tr("5.6.3.0/24", kindVisitor, "5.6.3.9", "5.6.0.3"), tr("5.6.4.0/24", kindVisitor, "5.6.4.9", "5.6.0.4")}, nil, rules, o, hops, nil)
	w = rowFor(rows, "5.6.0.0/16", scopeWide)
	if w == nil || w.City != "Munich" || w.Method != methodMMDB || w.RuleVotes != 0 || w.MMDBVotes != 2 {
		t.Errorf("mmdb only: %+v", w)
	}
	// A rule vote against an MMDB vote for another city is a tie: no wide row, as before.
	rows, out = locate(traces(tr("5.6.3.0/24", kindVisitor, "5.6.3.9", "5.6.0.3")), ptrs, rules, o, hops, nil)
	if rowFor(rows, "5.6.0.0/16", scopeWide) != nil || out.WideTies != 1 {
		t.Errorf("tie: %+v %+v", rows, out)
	}
}

// hopIPs asks for exactly the IPs locate can use: with only those answered, locate gives the same rows.
func TestHopIPsCoverWhatLocateUses(t *testing.T) {
	o, rules := locateWorld(t)
	traces := []traceIn{
		// No rule match: all the hops of its traces with an ASN, never the targets.
		twoHopTrace("5.6.1.0/24", kindVisitor, "5.6.1.9", "9.9.9.1", "5.6.0.2"),
		twoHopTrace("5.6.1.0/24", kindGateway, "5.6.1.1", "9.9.9.1", "5.6.0.3"),
		// A rule match: only the winning router, for the agreement count.
		twoHopTrace("5.6.2.0/24", kindVisitor, "5.6.2.9", "9.9.9.2", "5.6.0.1"),
		// The winner is the target's own name: no hop.
		twoHopTrace("5.6.3.0/24", kindVisitor, "5.6.3.9", "9.9.9.3", "5.6.0.7"),
		// No origin ASN: no vote, so no hop to look up.
		twoHopTrace("77.1.2.0/24", kindVisitor, "77.1.2.9", "9.9.9.4", "5.6.0.9"),
	}
	ptrs := names("5.6.0.1", "a.fra1.isp.net", "5.6.3.9", "h.muc1.isp.net")
	got := hopIPs(traces, ptrs, rules, o)
	var want []netip.Addr
	for _, s := range []string{"5.6.0.1", "5.6.0.2", "5.6.0.3", "9.9.9.1"} {
		want = append(want, netip.MustParseAddr(s))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hopIPs = %v, want %v", got, want)
	}
	// Every candidate has a city: the full map and the map of the asked IPs must not differ.
	all, asked := map[netip.Addr]cityKey{}, map[netip.Addr]cityKey{}
	for i, t := range traces {
		for j, c := range t.Candidates {
			city := cityKey{"DE", fmt.Sprintf("City%d%d", i, j)}
			all[c] = city
			if slices.Contains(got, c) {
				asked[c] = city
			}
		}
	}
	rowsAll, outAll := locate(traces, ptrs, rules, o, all, nil)
	rowsAsked, outAsked := locate(traces, ptrs, rules, o, asked, nil)
	if !reflect.DeepEqual(rowsAll, rowsAsked) || outAll.MMDBVotes != outAsked.MMDBVotes ||
		outAll.MMDBCompared != outAsked.MMDBCompared || outAll.MMDBAgreed != outAsked.MMDBAgreed {
		t.Errorf("rows differ:\nall   %+v %+v\nasked %+v %+v", rowsAll, outAll, rowsAsked, outAsked)
	}
}

func TestParseHopCities(t *testing.T) {
	got := parseHopCities([]byte("5.6.0.2\tde\tFrankfurt\n" +
		"::ffff:5.6.0.3\tDE\tMunich \n" +
		"bad\tDE\tBerlin\n" + // not an IP
		"5.6.0.4\tDEU\tBerlin\n" + // not a country code
		"5.6.0.5\tDE\t\n" + // no city
		"5.6.0.6\tDE\n" + // short
		"5.6.0.7\tDE\tBonn\textra\n")) // long
	want := hopCities("5.6.0.2", "DE", "Frankfurt", "5.6.0.3", "DE", "Munich")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFingerprintOf(t *testing.T) {
	summary := [4]string{"1:a", "2:b", "3:c", "4:d"}
	if fingerprintOf(summary, "etag1") == fingerprintOf(summary, "etag2") {
		t.Error("a new MMDB does not change the fingerprint")
	}
	if fingerprintOf(summary, "") != fingerprintOf(summary, noMMDB) {
		t.Error("an empty ETag is not 'none'")
	}
	if fingerprintOf(summary, "etag1") == fingerprintOf(summary, noMMDB) {
		t.Error("an MMDB is not 'none'")
	}
	changed := summary
	changed[1] = "2:x"
	if fingerprintOf(summary, "e") == fingerprintOf(changed, "e") {
		t.Error("a database input does not change the fingerprint")
	}
	if !strings.Contains(fingerprintOf(summary, "etag1"), `"etag1"`) {
		t.Errorf("fingerprint = %s", fingerprintOf(summary, "etag1"))
	}
}

// fakeStore is an R2 bucket in memory.
type fakeStore struct {
	objects     map[string]publish.ObjectInfo
	content     map[string]string
	statErr     error
	downloadErr error
	stats       []string
	downloads   []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string]publish.ObjectInfo{}, content: map[string]string{}}
}

func (f *fakeStore) put(key, etag, content string) {
	f.objects[key] = publish.ObjectInfo{ETag: etag}
	f.content[key] = content
}

func (f *fakeStore) Stat(_ context.Context, key string) (publish.ObjectInfo, error) {
	f.stats = append(f.stats, key)
	if f.statErr != nil {
		return publish.ObjectInfo{}, f.statErr
	}
	info, ok := f.objects[key]
	if !ok {
		return publish.ObjectInfo{}, fmt.Errorf("stat %s: %w", key, publish.ErrNotFound)
	}
	return info, nil
}

// DownloadIfChanged follows publish.R2.DownloadIfChanged.
func (f *fakeStore) DownloadIfChanged(ctx context.Context, key, path, etag string) (publish.ObjectInfo, bool, error) {
	info, err := f.Stat(ctx, key)
	if err != nil {
		return publish.ObjectInfo{}, false, fmt.Errorf("download: %w", err)
	}
	if etag != "" && info.ETag == etag {
		return info, false, nil
	}
	if f.downloadErr != nil {
		return publish.ObjectInfo{}, false, f.downloadErr
	}
	f.downloads = append(f.downloads, key)
	if err := os.WriteFile(path, []byte(f.content[key]), 0o644); err != nil {
		return publish.ObjectInfo{}, false, err
	}
	return info, true, nil
}

var testNow = time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)

const (
	todayKey     = "2026-11-12/ip2geo-openipdata-20261112.mmdb"
	yesterdayKey = "2026-11-11/ip2geo-openipdata-20261111.mmdb"
)

func testPipeline(t *testing.T, store objectStore) (*Pipeline, *[]string) {
	t.Helper()
	var logs []string
	return &Pipeline{
		DataDir: t.TempDir(), store: store, MMDBBuilder: "/not/used", now: func() time.Time { return testNow },
		Progress: func(s string) { logs = append(logs, s) },
	}, &logs
}

func TestMMDBKey(t *testing.T) {
	if got := mmdbKey(testNow); got != todayKey {
		t.Errorf("key = %s", got)
	}
	// The day is the UTC day, whatever zone the time is in.
	if got := mmdbKey(time.Date(2026, 11, 12, 1, 0, 0, 0, time.FixedZone("x", 3*3600))); got != yesterdayKey {
		t.Errorf("key = %s", got)
	}
}

func TestStatMMDB(t *testing.T) {
	ctx := context.Background()

	// Today's object wins over yesterday's, and yesterday's is not even asked for.
	store := newFakeStore()
	store.put(todayKey, "t1", "x")
	store.put(yesterdayKey, "y1", "x")
	p, _ := testPipeline(t, store)
	if ref := p.statMMDB(ctx); ref.Reason != "" || ref.Key != todayKey || ref.ETag != "t1" || ref.etag() != "t1" || !slices.Equal(store.stats, []string{todayKey}) {
		t.Errorf("both: %+v, stats %v", ref, store.stats)
	}

	// Yesterday's when today's is not there yet.
	store = newFakeStore()
	store.put(yesterdayKey, "y1", "x")
	p, _ = testPipeline(t, store)
	if ref := p.statMMDB(ctx); ref.Reason != "" || ref.Key != yesterdayKey || ref.ETag != "y1" || !slices.Equal(store.stats, []string{todayKey, yesterdayKey}) {
		t.Errorf("yesterday: %+v, stats %v", ref, store.stats)
	}

	// Neither: skipped, and the fingerprint says "none".
	p, _ = testPipeline(t, newFakeStore())
	if ref := p.statMMDB(ctx); ref.Reason != reasonNoObject || ref.etag() != noMMDB {
		t.Errorf("none: %+v", ref)
	}

	// Any other error skips it, without trying yesterday, and is logged.
	store = newFakeStore()
	store.put(yesterdayKey, "y1", "x")
	store.statErr = errors.New("connection reset by 203.0.113.9")
	p, logs := testPipeline(t, store)
	if ref := p.statMMDB(ctx); ref.Reason != reasonStat || ref.etag() != noMMDB || len(store.stats) != 1 {
		t.Errorf("error: %+v, stats %v", ref, store.stats)
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "WARNING") || strings.Contains((*logs)[0], "203.0.113.9") {
		t.Errorf("logs = %q", *logs)
	}

	// No R2 setting.
	p = &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return testNow }}
	if ref := p.statMMDB(ctx); ref.Reason != reasonNoR2 {
		t.Errorf("no R2: %+v", ref)
	}
}

// A bad R2 setting is reported by a fixed text: the value holds the secret key.
func TestStatMMDBInvalidR2HidesSecret(t *testing.T) {
	const secret = "s3cr3t-key"
	p := &Pipeline{DataDir: t.TempDir(), R2: "http://id:" + secret + "@acct.r2.cloudflarestorage.com/bucket", now: func() time.Time { return testNow }}
	var logs []string
	p.Progress = func(s string) { logs = append(logs, s) }
	ref := p.statMMDB(context.Background())
	if ref.Reason != reasonBadR2 {
		t.Fatalf("ref = %+v", ref)
	}
	if text := ref.Reason + strings.Join(logs, "\n"); strings.Contains(text, secret) {
		t.Errorf("the secret is in %q", text)
	}
}

func TestFetchMMDBDownloadsOnlyWhenChanged(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.put(todayKey, "e1", "first")
	p, _ := testPipeline(t, store)
	path := filepath.Join(p.dir(), mmdbFile) // the rdns dir does not exist yet
	ref := mmdbRef{store: store, Key: todayKey, ETag: "e1"}

	got, downloaded, err := p.fetchMMDB(ctx, ref)
	if err != nil || !downloaded || got != path {
		t.Fatalf("first: %q %v %v", got, downloaded, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "first" {
		t.Errorf("copy = %q", data)
	}
	if savedMMDBETag(path) != "e1" {
		t.Errorf("saved ETag = %q", savedMMDBETag(path))
	}

	// Same ETag: nothing downloaded.
	if _, downloaded, err = p.fetchMMDB(ctx, ref); err != nil || downloaded || len(store.downloads) != 1 {
		t.Errorf("same ETag: %v %v, downloads %v", downloaded, err, store.downloads)
	}

	// The same content under yesterday's key and the same ETag is the same file.
	store.put(yesterdayKey, "e1", "first")
	if _, downloaded, _ = p.fetchMMDB(ctx, mmdbRef{store: store, Key: yesterdayKey, ETag: "e1"}); downloaded {
		t.Error("downloaded the same ETag again")
	}

	// A replaced object has a new ETag.
	store.put(todayKey, "e2", "second")
	if _, downloaded, err = p.fetchMMDB(ctx, ref); err != nil || !downloaded {
		t.Errorf("new ETag: %v %v", downloaded, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "second" || savedMMDBETag(path) != "e2" {
		t.Errorf("copy = %q, ETag %q", data, savedMMDBETag(path))
	}

	// A lost copy, or a lost ETag, is downloaded again.
	os.Remove(path)
	if _, downloaded, _ = p.fetchMMDB(ctx, ref); !downloaded {
		t.Error("lost copy not downloaded")
	}
	os.Remove(path + mmdbETagSuffix)
	if _, downloaded, _ = p.fetchMMDB(ctx, ref); !downloaded {
		t.Error("lost ETag not downloaded")
	}
	if len(store.downloads) != 4 {
		t.Errorf("downloads = %v", store.downloads)
	}
	if _, err := os.Stat(path + mmdbETagSuffix + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}

	// A failed download is an error and the saved copy stays what it was.
	store.put(todayKey, "e3", "third")
	store.downloadErr = errors.New("boom")
	if _, _, err = p.fetchMMDB(ctx, ref); err == nil {
		t.Error("no error")
	}
	if data, _ := os.ReadFile(path); string(data) != "second" {
		t.Errorf("copy = %q", data)
	}
}

// fakeLookup answers like mmdbbuilder lookup, from a table; it checks its input.
func fakeLookup(t *testing.T, cities map[string]string, calls *[][]string) lookupFunc {
	return func(_ context.Context, db, in, out string) error {
		data, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		ips := strings.Fields(string(data))
		*calls = append(*calls, append([]string{db}, ips...))
		var rows strings.Builder
		for _, ip := range ips {
			if city, ok := cities[ip]; ok {
				rows.WriteString(ip + "\t" + city + "\n")
			}
		}
		return os.WriteFile(out, []byte(rows.String()), 0o644)
	}
}

func rdnsFiles(t *testing.T, p *Pipeline) []string {
	t.Helper()
	entries, err := os.ReadDir(p.dir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestMMDBHopsUsesCopyAndCleansUp(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.put(todayKey, "e1", "mmdb")
	p, _ := testPipeline(t, store)
	var calls [][]string
	p.lookup = fakeLookup(t, map[string]string{"5.6.0.2": "DE\tFrankfurt"}, &calls)
	ref := mmdbRef{store: store, Key: todayKey, ETag: "e1"}
	ips := []netip.Addr{netip.MustParseAddr("5.6.0.2"), netip.MustParseAddr("5.6.0.3")}

	res := p.mmdbHops(ctx, ref, ips)
	if res.Status != statusUsed || res.Reason != "" || !res.Downloaded || res.ETag != "e1" || !reflect.DeepEqual(res.Hops, hopCities("5.6.0.2", "DE", "Frankfurt")) {
		t.Fatalf("res = %+v", res)
	}
	if want := [][]string{{filepath.Join(p.dir(), mmdbFile), "5.6.0.2", "5.6.0.3"}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("lookup calls = %v", calls)
	}
	// The run files are gone; the copy and its ETag stay.
	if got := rdnsFiles(t, p); !slices.Equal(got, []string{mmdbFile, mmdbFile + mmdbETagSuffix}) {
		t.Errorf("files = %v", got)
	}

	// The second run uses the copy.
	if res = p.mmdbHops(ctx, ref, ips); res.Status != statusUsed || res.Reason != "" || res.Downloaded {
		t.Errorf("second: %+v", res)
	}

	// No IPs: no lookup process.
	calls = nil
	if res = p.mmdbHops(ctx, ref, nil); res.Status != statusUsed || len(res.Hops) != 0 || len(calls) != 0 {
		t.Errorf("no IPs: %+v %v", res, calls)
	}
}

func TestMMDBHopsFailuresSkipTheFallback(t *testing.T) {
	ctx := context.Background()
	ips := []netip.Addr{netip.MustParseAddr("5.6.0.2")}
	skippedWith := func(res mmdbResult, reason string) bool {
		return res.Status == statusSkipped && res.Reason == reason && res.Hops == nil && res.ETag == noMMDB
	}

	// A failed download with no saved copy.
	store := newFakeStore()
	store.put(todayKey, "e1", "mmdb")
	store.downloadErr = errors.New("boom at 203.0.113.9")
	p, logs := testPipeline(t, store)
	ref := mmdbRef{store: store, Key: todayKey, ETag: "e1"}
	if res := p.mmdbHops(ctx, ref, ips); !skippedWith(res, reasonDownload) {
		t.Errorf("download: %+v", res)
	}
	if text := strings.Join(*logs, "\n"); !strings.Contains(text, "WARNING") || strings.Contains(text, "203.0.113.9") {
		t.Errorf("logs = %q", text)
	}

	// An object that vanished between the Stat and the download.
	store = newFakeStore()
	p, _ = testPipeline(t, store)
	if res := p.mmdbHops(ctx, mmdbRef{store: store, Key: todayKey, ETag: "e1"}, ips); !skippedWith(res, reasonDownload) {
		t.Errorf("vanished: %+v", res)
	}

	// A lookup that fails, not through mmdbbuilder itself: the copy stays.
	store = newFakeStore()
	store.put(todayKey, "e1", "mmdb")
	p, _ = testPipeline(t, store)
	p.lookup = func(context.Context, string, string, string) error { return errors.New("no such file") }
	if res := p.mmdbHops(ctx, mmdbRef{store: store, Key: todayKey, ETag: "e1"}, ips); !skippedWith(res, reasonLookup) {
		t.Errorf("lookup: %+v", res)
	}
	if got := rdnsFiles(t, p); !slices.Equal(got, []string{mmdbFile, mmdbFile + mmdbETagSuffix}) {
		t.Errorf("files = %v", got)
	}

	// mmdbbuilder ran and failed (a damaged copy): the copy is dropped, so the next run fetches it again.
	p.lookup = func(context.Context, string, string, string) error {
		return fmt.Errorf("mmdbbuilder lookup: %w", &exec.ExitError{})
	}
	if res := p.mmdbHops(ctx, mmdbRef{store: store, Key: todayKey, ETag: "e1"}, ips); !skippedWith(res, reasonLookup) {
		t.Errorf("exit error: %+v", res)
	}
	if got := rdnsFiles(t, p); len(got) != 0 {
		t.Errorf("files after a damaged copy = %v", got)
	}

	// No mmdbbuilder path: skipped before anything is downloaded.
	store = newFakeStore()
	store.put(todayKey, "e1", "mmdb")
	p, _ = testPipeline(t, store)
	p.MMDBBuilder = ""
	if res := p.mmdbHops(ctx, mmdbRef{store: store, Key: todayKey, ETag: "e1"}, ips); !skippedWith(res, reasonNoBuilder) || len(store.downloads) != 0 {
		t.Errorf("no builder: %+v, downloads %v", res, store.downloads)
	}
}

// saveCopy leaves a copy of the MMDB with its ETag, as an earlier run would have.
func saveCopy(t *testing.T, p *Pipeline, etag string) {
	t.Helper()
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.savedPath(), []byte("old mmdb"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.savedPath()+mmdbETagSuffix, []byte(etag+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
}

// With R2 out of reach the saved copy and its ETag stand in: the fingerprint, and so the rows, do not change.
func TestStatMMDBFallsBackOnSavedCopy(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		setup  func(s *fakeStore, p *Pipeline)
		reason string
	}{
		"stat error":    {func(s *fakeStore, _ *Pipeline) { s.statErr = errors.New("connection reset") }, reasonStat},
		"no object":     {func(*fakeStore, *Pipeline) {}, reasonNoObject},
		"no R2 setting": {func(_ *fakeStore, p *Pipeline) { p.store = nil }, reasonNoR2},
		"invalid R2":    {func(_ *fakeStore, p *Pipeline) { p.store, p.R2 = nil, "http://id:secret@h/b" }, reasonBadR2},
	} {
		store := newFakeStore()
		p, _ := testPipeline(t, store)
		saveCopy(t, p, "saved1")
		c.setup(store, p)
		ref := p.statMMDB(ctx)
		if ref.Reason != c.reason || !ref.Saved || !ref.usable() || ref.ETag != "saved1" || ref.etag() != "saved1" {
			t.Errorf("%s: %+v", name, ref)
		}

		// Without a saved copy, or a copy that has no ETag, there is no MMDB.
		os.Remove(p.savedPath() + mmdbETagSuffix)
		if ref = p.statMMDB(ctx); ref.Reason != c.reason || ref.Saved || ref.usable() || ref.etag() != noMMDB {
			t.Errorf("%s, no ETag: %+v", name, ref)
		}
		os.Remove(p.savedPath())
		os.WriteFile(p.savedPath()+mmdbETagSuffix, []byte("saved1"), 0o640)
		if ref = p.statMMDB(ctx); ref.Saved || ref.usable() || ref.etag() != noMMDB {
			t.Errorf("%s, no copy: %+v", name, ref)
		}
	}

	// An object in R2 is the MMDB, whatever the saved copy is.
	store := newFakeStore()
	store.put(todayKey, "t1", "x")
	p, _ := testPipeline(t, store)
	saveCopy(t, p, "saved1")
	if ref := p.statMMDB(ctx); ref.Reason != "" || ref.Saved || ref.etag() != "t1" {
		t.Errorf("object: %+v", ref)
	}
}

// The saved copy is read, and the run says so, when R2 gave no object.
func TestMMDBHopsSavedCopyWhenR2Unusable(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore() // nothing in it
	p, logs := testPipeline(t, store)
	saveCopy(t, p, "saved1")
	var calls [][]string
	p.lookup = fakeLookup(t, map[string]string{"5.6.0.2": "DE\tFrankfurt"}, &calls)
	ips := []netip.Addr{netip.MustParseAddr("5.6.0.2")}

	ref := p.statMMDB(ctx)
	res := p.mmdbHops(ctx, ref, ips)
	if res.Status != statusSavedCopy || res.Reason != reasonNoObject || res.Downloaded || res.ETag != "saved1" ||
		!reflect.DeepEqual(res.Hops, hopCities("5.6.0.2", "DE", "Frankfurt")) || res.ETag != ref.etag() {
		t.Fatalf("res = %+v", res)
	}
	if len(calls) != 1 || calls[0][0] != p.savedPath() || len(store.downloads) != 0 {
		t.Errorf("calls %v, downloads %v", calls, store.downloads)
	}
	if text := strings.Join(*logs, "\n"); !strings.Contains(text, "WARNING") || !strings.Contains(text, "saved copy") {
		t.Errorf("logs = %q", text)
	}
	if got := rdnsFiles(t, p); !slices.Equal(got, []string{mmdbFile, mmdbFile + mmdbETagSuffix}) {
		t.Errorf("files = %v", got)
	}

	// mmdbbuilder rejecting the saved copy drops it: skipped, and the next run has no copy.
	p.lookup = func(context.Context, string, string, string) error {
		return fmt.Errorf("lookup: %w", &exec.ExitError{})
	}
	if res = p.mmdbHops(ctx, ref, ips); res.Status != statusSkipped || res.Reason != reasonLookup || res.ETag != noMMDB {
		t.Errorf("damaged: %+v", res)
	}
	if got := rdnsFiles(t, p); len(got) != 0 {
		t.Errorf("files = %v", got)
	}
}

// A download that fails after a good Stat reads the saved copy, and the fingerprint names that copy,
// so the next run (whose Stat sees the new object) rebuilds and tries again.
func TestMMDBHopsDownloadFailureUsesSavedCopy(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.put(todayKey, "new", "new mmdb")
	store.downloadErr = errors.New("boom at 203.0.113.9")
	p, logs := testPipeline(t, store)
	saveCopy(t, p, "old")
	var calls [][]string
	p.lookup = fakeLookup(t, map[string]string{"5.6.0.2": "DE\tFrankfurt"}, &calls)

	ref := p.statMMDB(ctx)
	if ref.etag() != "new" {
		t.Fatalf("ref = %+v", ref)
	}
	res := p.mmdbHops(ctx, ref, []netip.Addr{netip.MustParseAddr("5.6.0.2")})
	if res.Status != statusSavedCopy || res.Reason != reasonDownload || res.ETag != "old" || res.ETag == ref.etag() ||
		!reflect.DeepEqual(res.Hops, hopCities("5.6.0.2", "DE", "Frankfurt")) {
		t.Fatalf("res = %+v", res)
	}
	// The failed download left the saved copy as it was.
	if data, _ := os.ReadFile(p.savedPath()); string(data) != "old mmdb" || savedMMDBETag(p.savedPath()) != "old" {
		t.Errorf("copy %q, ETag %q", data, savedMMDBETag(p.savedPath()))
	}
	if text := strings.Join(*logs, "\n"); !strings.Contains(text, "WARNING") || strings.Contains(text, "203.0.113.9") {
		t.Errorf("logs = %q", text)
	}
}

// A run file left by a killed run is not mistaken for this run's answer.
func TestLookupHopsIgnoresStaleFiles(t *testing.T) {
	store := newFakeStore()
	p, _ := testPipeline(t, store)
	os.MkdirAll(p.dir(), 0o750)
	os.WriteFile(filepath.Join(p.dir(), hopsOutFile), []byte("5.6.0.2\tDE\tStale\n"), 0o640)
	p.lookup = func(_ context.Context, _, _, out string) error {
		if _, err := os.Stat(out); err == nil {
			t.Error("the old answer is still there")
		}
		return os.WriteFile(out, nil, 0o640)
	}
	hops, err := p.lookupHops(context.Background(), "x.mmdb", []netip.Addr{netip.MustParseAddr("5.6.0.2")})
	if err != nil || len(hops) != 0 {
		t.Errorf("hops %v, err %v", hops, err)
	}
	if got := rdnsFiles(t, p); len(got) != 0 {
		t.Errorf("files = %v", got)
	}
}
