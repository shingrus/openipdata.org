package arin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"openipdata.org/hatchet/ipdata"
	"openipdata.org/hatchet/publish"
	"openipdata.org/hatchet/rdns"
)

var testNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// fakeDB is the lookupStore of the tests: it keeps the position and the records, and notes
// every save.
type fakeDB struct {
	mu      sync.Mutex
	hasRow  bool
	cursor  int
	started time.Time
	recs    map[[2]uint32]record
	saves   []save
	nets    []record // what Compact returns
	pruned  int
	saveErr error
}

type save struct {
	cursor int
	recs   int
}

func (f *fakeDB) Progress(_ context.Context, now time.Time) (int, time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.hasRow {
		return 0, now, false, nil
	}
	return f.cursor, f.started, false, nil
}

func (f *fakeDB) Save(_ context.Context, recs []record, cursor int, started, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.recs == nil {
		f.recs = map[[2]uint32]record{}
	}
	for _, r := range recs {
		f.recs[[2]uint32{r.Start, r.End}] = r
	}
	f.hasRow, f.cursor, f.started = true, cursor, started
	f.saves = append(f.saves, save{cursor, len(recs)})
	return nil
}

func (f *fakeDB) Compact(context.Context, time.Time) ([]record, int, error) {
	return f.nets, f.pruned, nil
}

// rdapServer answers the lookups of a test: handler(w, octet, n) gets the third octet of the
// address (the tests walk 192.0.<octet>.x) and how often that octet was asked so far.
type rdapServer struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
	times []time.Time
	count map[int]int
}

func newRDAP(t *testing.T, handler func(w http.ResponseWriter, octet, n int)) *rdapServer {
	s := &rdapServer{count: map[int]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, err := netip.ParseAddr(strings.TrimPrefix(r.URL.Path, "/ip/"))
		if err != nil || r.Header.Get("Accept") != "application/rdap+json" || r.Header.Get("User-Agent") == "" {
			http.Error(w, "bad request", 400)
			return
		}
		octet := int(addr.As4()[2])
		s.mu.Lock()
		s.paths = append(s.paths, addr.String())
		s.times = append(s.times, time.Now())
		s.count[octet]++
		n := s.count[octet]
		s.mu.Unlock()
		handler(w, octet, n)
	}))
	t.Cleanup(s.Close)
	return s
}

// block is the answer for the /24 192.0.<octet>.0 with a registrant in Springfield, IL.
func block(octet int, label string) []byte {
	start, end := fmt.Sprintf("192.0.%d.0", octet), fmt.Sprintf("192.0.%d.255", octet)
	return rdapJSON(start, end, `,"entities":[`+entity(`"registrant"`, label)+`]`)
}

func serve(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/rdap+json")
	w.Write(body)
}

// statusHandler gives each octet of the tests a different behaviour.
func statusHandler(w http.ResponseWriter, octet, n int) {
	switch octet {
	case 0:
		serve(w, block(0, goodLabel))
	case 1:
		http.NotFound(w, nil)
	case 2:
		w.Header().Set("Location", "https://rdap.example.net/ip/x")
		w.WriteHeader(http.StatusFound)
	case 3: // wider than a /16
		serve(w, rdapJSON("192.0.0.0", "192.1.255.255", ""))
	case 4:
		serve(w, []byte(`{"startAddress":`))
	case 5:
		http.Error(w, "boom", 500)
	case 6: // 429 without Retry-After, then the answer
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		serve(w, block(6, goodLabel))
	case 7: // 429 with Retry-After, then an answer with no usable city
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		serve(w, block(7, `"1 Example Road\nBerlin\nBE\n10115\nGermany"`))
	default:
		http.NotFound(w, nil)
	}
}

// env is a pipeline with every outside part replaced: stats, MMDB and gap list, ARIN's RDAP.
type env struct {
	p     *Pipeline
	db    *fakeDB
	logs  []string
	gaps  int
	store *fakeObjects
	mu    sync.Mutex
}

type fakeObjects struct {
	objects map[string]publish.ObjectInfo
}

func (f *fakeObjects) Stat(_ context.Context, key string) (publish.ObjectInfo, error) {
	if info, ok := f.objects[key]; ok {
		return info, nil
	}
	return publish.ObjectInfo{}, fmt.Errorf("stat: %w", publish.ErrNotFound)
}

func (f *fakeObjects) DownloadIfChanged(ctx context.Context, key, path, etag string) (publish.ObjectInfo, bool, error) {
	info, err := f.Stat(ctx, key)
	if err != nil {
		return publish.ObjectInfo{}, false, err
	}
	if etag != "" && info.ETag == etag {
		return info, false, nil
	}
	return info, true, os.WriteFile(path, []byte("mmdb"), 0o640)
}

const statsText = "2|arin|20261117|3|19700101|20261117|-0500\n" +
	"arin|*|ipv4|*|2|summary\n" +
	"arin|US|ipv4|192.0.0.0|65536|20200101|allocated|abc\n" +
	"arin|US|ipv4|10.0.0.0|16777216|20200101|assigned|abc\n" +
	"arin|US|ipv6|2001:db8::|32|20200101|allocated|abc\n"

// newEnv builds a pipeline whose walk is 192.0.<i>.0 for position i (256 positions) and whose gap
// list holds the first gapN of those /24s.
func newEnv(t *testing.T, rdap *rdapServer, gapN int) *env {
	t.Helper()
	e := &env{db: &fakeDB{}}
	stats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(statsText)) }))
	t.Cleanup(stats.Close)
	e.store = &fakeObjects{objects: map[string]publish.ObjectInfo{
		fmt.Sprintf("%s/openipdata-org-ip2geo-%s.mmdb", testNow.Format("2006-01-02"), testNow.Format("20060102")): {ETag: "e1"},
	}}
	e.p = &Pipeline{
		DataDir:  t.TempDir(),
		Progress: func(s string) { e.mu.Lock(); e.logs = append(e.logs, s); e.mu.Unlock() },
		db:       e.db, mmdbStore: e.store, statsURL: stats.URL, now: func() time.Time { return testNow },
		dlClient: stats.Client(),
		interval: time.Millisecond, budget: time.Minute, batch: 3, workers: 4,
		size: 256, at: func(i uint32) uint32 { return 0xC00000 + i },
		retryPause: time.Millisecond, backoff: time.Millisecond, retryUnit: time.Millisecond,
		public: func(netip.Addr) bool { return true },
		gaps: func(_ context.Context, _, out string) error {
			e.gaps++
			return os.WriteFile(out, []byte(fmt.Sprintf("192.0.0.0/24 %d US\n", gapN)), 0o640)
		},
	}
	if rdap != nil {
		e.p.base = rdap.URL
		e.p.client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return e
}

var ipText = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)

// noAddress fails when the logs or the output JSON of a run hold something that looks like an address.
func (e *env) noAddress(t *testing.T, out any) {
	t.Helper()
	data, _ := json.Marshal(out)
	if ipText.Match(data) {
		t.Errorf("an address in the output: %s", data)
	}
	for _, l := range e.logs {
		if ipText.MatchString(l) {
			t.Errorf("an address in a log line: %q", l)
		}
	}
}

func TestLookupResults(t *testing.T) {
	srv := newRDAP(t, statusHandler)
	e := newEnv(t, srv, 8)
	out, err := e.p.Lookup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := LookupOutput{
		Stats: "downloaded", MMDB: "used", GapsBuilt: true, ARINRanges: 2, Gaps: 8, Steps: 256, Targets: 8, Batches: 3,
		Found: 3, WithCity: 2, Wide: 1, NotARIN: 1, NotFound: 1, Bad: 1, Failed: 1, Stored: 3,
		RateLimited: 2, Retries: 2 + 0, CycleDone: true,
	}
	// Requests: 8 lookups + 2 retries after the 500 + 2 asks again after a 429.
	want.Requests = 8 + 2 + 2
	if out != want {
		t.Errorf("output\n got %+v\nwant %+v", out, want)
	}
	// The cursor moved past each stored batch: octets 0 to 2 give one record, 3 to 5 none (wide, bad,
	// failed), and 6 and 7 the last two, with the rest of the walk.
	if fmt.Sprint(e.db.saves) != "[{3 1} {6 0} {256 2}]" {
		t.Errorf("saves = %v", e.db.saves)
	}
	if e.db.cursor != 256 || !e.db.started.Equal(testNow) {
		t.Errorf("progress %d %v", e.db.cursor, e.db.started)
	}
	if len(e.db.recs) != 3 {
		t.Errorf("records = %d", len(e.db.recs))
	}
	if r := e.db.recs[[2]uint32{u32(netip.MustParseAddr("192.0.0.0")), u32(netip.MustParseAddr("192.0.0.255"))}]; r.City != "Springfield" || r.State != "IL" || r.Country != "US" {
		t.Errorf("record 0 = %+v", r)
	}
	if r := e.db.recs[[2]uint32{u32(netip.MustParseAddr("192.0.7.0")), u32(netip.MustParseAddr("192.0.7.255"))}]; r.City != "" || r.Start == 0 {
		t.Errorf("record 7 (no usable city, still stored) = %+v", r)
	}
	e.noAddress(t, out)
}

// The cursor moves past a batch only when it is stored: a failing store keeps it, and a rerun
// asks the same addresses.
func TestLookupRerunAsksTheSameAddresses(t *testing.T) {
	srv := newRDAP(t, statusHandler)
	e := newEnv(t, srv, 8)
	if _, err := e.p.Lookup(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := append([]string(nil), srv.paths...)
	sort.Strings(first)

	srv2 := newRDAP(t, statusHandler)
	e2 := newEnv(t, srv2, 8)
	if _, err := e2.p.Lookup(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := append([]string(nil), srv2.paths...)
	sort.Strings(second)
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Errorf("addresses differ:\n%v\n%v", first, second)
	}
	// Every asked host is one of 1 to 254 of its /24, chosen by the /24 alone.
	for _, a := range first {
		last := netip.MustParseAddr(a).As4()[3]
		if last == 0 || last == 255 {
			t.Errorf("host %d", last)
		}
	}

	// A failing save is an error and the cursor stays.
	e3 := newEnv(t, newRDAP(t, statusHandler), 8)
	e3.db.saveErr = fmt.Errorf("db down")
	if _, err := e3.p.Lookup(context.Background()); err == nil {
		t.Error("a failed save did not fail the task")
	}
	if e3.db.hasRow || e3.db.cursor != 0 {
		t.Errorf("the cursor moved: %+v", e3.db)
	}
}

func TestLookupStopsAfterFiveRateLimits(t *testing.T) {
	srv := newRDAP(t, func(w http.ResponseWriter, octet, n int) { w.WriteHeader(http.StatusTooManyRequests) })
	e := newEnv(t, srv, 40)
	e.p.batch = 40
	out, err := e.p.Lookup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Stopped != "rate limited" || out.Batches != 0 || len(e.db.saves) != 0 || out.RateLimited < 5 {
		t.Errorf("%+v saves %v", out, e.db.saves)
	}
	e.noAddress(t, out)
}

// A burst of 429 answers to requests that were on their way together is one pause, not eight:
// every worker waits it out and the run goes on.
func TestLookupRateLimitPausesAllWorkers(t *testing.T) {
	const burst = 8
	var mu sync.Mutex
	arrived, released := 0, time.Time{}
	var afterBurst []time.Time
	gate := make(chan struct{})
	srv := newRDAP(t, func(w http.ResponseWriter, octet, n int) {
		mu.Lock()
		arrived++
		k := arrived
		if k == burst {
			close(gate)
		}
		if k > burst {
			afterBurst = append(afterBurst, time.Now())
		}
		mu.Unlock()
		if k > burst {
			serve(w, block(octet, goodLabel))
			return
		}
		select { // the first eight answer together
		case <-gate:
		case <-time.After(5 * time.Second):
		}
		mu.Lock()
		if released.IsZero() {
			released = time.Now()
		}
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
	})
	e := newEnv(t, srv, 16)
	e.p.batch, e.p.workers, e.p.backoff = 16, burst, 150*time.Millisecond
	out, err := e.p.Lookup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Stopped != "" || out.Found != 16 || out.RateLimited != burst || !out.CycleDone {
		t.Fatalf("%+v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, at := range afterBurst {
		if wait := at.Sub(released); wait < 100*time.Millisecond {
			t.Errorf("a request started %s after the 429 answers, inside the pause", wait)
		}
	}
	e.noAddress(t, out)
}

func TestLookupStopsAfterTenFailures(t *testing.T) {
	srv := newRDAP(t, func(w http.ResponseWriter, octet, n int) { http.Error(w, "boom", 500) })
	e := newEnv(t, srv, 40)
	e.p.batch = 40
	out, err := e.p.Lookup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Stopped != "failing" || len(e.db.saves) != 0 || out.Retries < 20 {
		t.Errorf("%+v saves %v", out, e.db.saves)
	}
	// A success in between resets the count: 9 failures, a success, 9 failures do not end the batch
	// early (all 19 are asked), but a batch with that many failed lookups is not stored.
	srv2 := newRDAP(t, func(w http.ResponseWriter, octet, n int) {
		if octet == 9 {
			serve(w, block(9, goodLabel))
			return
		}
		http.Error(w, "boom", 500)
	})
	e2 := newEnv(t, srv2, 19)
	e2.p.batch, e2.p.workers = 19, 1
	out, err = e2.p.Lookup(context.Background())
	if err != nil || out.Stopped != "failing" || out.Requests != 18*3+1 || len(e2.db.saves) != 0 {
		t.Errorf("reset: %+v %v saves %v", out, err, e2.db.saves)
	}
	// The last batch of the walk is stored whatever failed in it: otherwise addresses that fail
	// every time would hold the walk at that position and the cycle would never end.
	e4 := newEnv(t, srv2, 19)
	e4.p.batch, e4.p.workers = 40, 1 // 19 targets, then the walk ends
	out, err = e4.p.Lookup(context.Background())
	if err != nil || out.Stopped != "" || !out.CycleDone || out.Failed != 18 || out.Found != 1 || len(e4.db.saves) != 1 {
		t.Errorf("last batch: %+v %v saves %v", out, err, e4.db.saves)
	}
	// A few failed lookups in a big batch are stored with it: 4 of 40.
	srv3 := newRDAP(t, func(w http.ResponseWriter, octet, n int) {
		if octet%10 == 3 {
			http.Error(w, "boom", 500)
			return
		}
		serve(w, block(octet, goodLabel))
	})
	e3 := newEnv(t, srv3, 40)
	e3.p.batch = 40
	out, err = e3.p.Lookup(context.Background())
	if err != nil || out.Stopped != "" || out.Failed != 4 || out.Found != 36 || len(e3.db.saves) == 0 {
		t.Errorf("few failures: %+v %v", out, err)
	}
}

// A Retry-After too large to be a duration is the longest pause, not no pause at all.
func TestLookupHugeRetryAfter(t *testing.T) {
	var mu sync.Mutex
	var limitedAt, nextAt time.Time
	srv := newRDAP(t, func(w http.ResponseWriter, octet, n int) {
		mu.Lock()
		first := limitedAt.IsZero()
		if first {
			limitedAt = time.Now()
		} else if nextAt.IsZero() {
			nextAt = time.Now()
		}
		mu.Unlock()
		if first {
			w.Header().Set("Retry-After", "99999999999999")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		serve(w, block(octet, goodLabel))
	})
	e := newEnv(t, srv, 4)
	e.p.batch, e.p.workers, e.p.maxWait, e.p.retryUnit = 4, 1, 120*time.Millisecond, time.Second
	out, err := e.p.Lookup(context.Background())
	if err != nil || out.Stopped != "" || out.RateLimited != 1 || out.Found != 4 {
		t.Fatalf("%+v %v", out, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if wait := nextAt.Sub(limitedAt); wait < 100*time.Millisecond {
		t.Errorf("the next request came %s after the 429: no pause", wait)
	}
}

func TestLookupBudgetAndCancel(t *testing.T) {
	srv := newRDAP(t, statusHandler)
	e := newEnv(t, srv, 100)
	e.p.batch, e.p.interval, e.p.budget = 100, 20*time.Millisecond, 100*time.Millisecond
	out, err := e.p.Lookup(context.Background())
	if err != nil || out.Stopped != "time" || len(e.db.saves) != 0 {
		t.Errorf("budget: %+v %v saves %v", out, err, e.db.saves)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e2 := newEnv(t, srv, 100)
	if _, err := e2.p.Lookup(ctx); err != context.Canceled {
		t.Errorf("cancelled run: %v", err)
	}
}

// Two lookups a second over all workers together: the server sees at most one request start per interval.
func TestLookupRate(t *testing.T) {
	srv := newRDAP(t, func(w http.ResponseWriter, octet, n int) { http.NotFound(w, nil) })
	e := newEnv(t, srv, 8)
	e.p.batch, e.p.interval = 8, 30*time.Millisecond
	started := time.Now()
	if _, err := e.p.Lookup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(srv.times) != 8 {
		t.Fatalf("requests = %d", len(srv.times))
	}
	if took := time.Since(started); took < 7*30*time.Millisecond {
		t.Errorf("8 lookups took %v: faster than one per interval", took)
	}
	// The arrival times at the server: the first and the last are at least six intervals apart
	// (single gaps are not checked: a slow machine can deliver one request late and the next on time).
	ts := append([]time.Time(nil), srv.times...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	if span := ts[len(ts)-1].Sub(ts[0]); span < 6*30*time.Millisecond {
		t.Errorf("8 requests arrived within %v", span)
	}
}

func TestLookupCycle(t *testing.T) {
	srv := newRDAP(t, statusHandler)
	// The cycle is done and the next starts later: nothing is asked.
	e := newEnv(t, srv, 8)
	e.db.hasRow, e.db.cursor, e.db.started = true, 256, testNow.Add(-10*24*time.Hour)
	out, err := e.p.Lookup(context.Background())
	if err != nil || out.Skipped != "cycle done" || len(srv.paths) != 0 || out.Requests != 0 {
		t.Errorf("cycle done: %+v %v", out, err)
	}
	// 180 days after it began the next cycle starts at position 0 with a new start time.
	e.db.started = testNow.Add(-cycleLength)
	out, err = e.p.Lookup(context.Background())
	if err != nil || out.Skipped != "" || out.Targets != 8 || !out.CycleDone {
		t.Errorf("new cycle: %+v %v", out, err)
	}
	if !e.db.started.Equal(testNow) || e.db.saves[0].cursor != 3 {
		t.Errorf("progress %v saves %v", e.db.started, e.db.saves)
	}
	// A run that stops in the middle of the cycle goes on at the saved position.
	srv2 := newRDAP(t, statusHandler)
	e2 := newEnv(t, srv2, 8)
	e2.db.hasRow, e2.db.cursor, e2.db.started = true, 6, testNow.Add(-time.Hour)
	out, err = e2.p.Lookup(context.Background())
	if err != nil || out.Targets != 2 || len(srv2.paths) < 2 || e2.db.started.Equal(testNow) {
		t.Errorf("resume: %+v %v %v", out, err, e2.db.started)
	}
}

func TestLookupSkips(t *testing.T) {
	srv := newRDAP(t, statusHandler)
	// No stats file at all.
	e := newEnv(t, srv, 8)
	bad := httptest.NewServer(http.NotFoundHandler())
	defer bad.Close()
	e.p.statsURL, e.p.dlClient = bad.URL, bad.Client()
	out, err := e.p.Lookup(context.Background())
	if err != nil || out.Skipped != "no ARIN stats" || len(srv.paths) != 0 {
		t.Errorf("no stats: %+v %v", out, err)
	}
	// A saved copy is used when the download fails (the stamp is old).
	e = newEnv(t, srv, 8)
	if _, err := e.p.Lookup(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.db.hasRow = false
	stamp := filepath.Join(e.p.dir(), statsFile+checkedSuffix)
	os.Chtimes(stamp, testNow.Add(-48*time.Hour), testNow.Add(-48*time.Hour))
	e.p.statsURL, e.p.dlClient = bad.URL, bad.Client()
	if out, err = e.p.Lookup(context.Background()); err != nil || out.Stats != "saved copy" || out.Skipped != "" {
		t.Errorf("saved copy: %+v %v", out, err)
	}
	e.noAddress(t, out)
	// No MMDB.
	e = newEnv(t, srv, 8)
	e.p.mmdbStore = nil
	if out, err = e.p.Lookup(context.Background()); err != nil || out.Skipped != "MMDB: R2 is not set" || out.MMDB != "skipped" {
		t.Errorf("no MMDB: %+v %v", out, err)
	}
	// A failing gaps run.
	e = newEnv(t, srv, 8)
	e.p.gaps = func(context.Context, string, string) error { return fmt.Errorf("mmdbbuilder gaps: boom 192.0.2.1") }
	if out, err = e.p.Lookup(context.Background()); err != nil || out.Skipped != "mmdbbuilder gaps failed" {
		t.Errorf("gaps failed: %+v %v", out, err)
	}
	e.noAddress(t, out)
	// An empty gap list.
	e = newEnv(t, srv, 8)
	e.p.gaps = func(_ context.Context, _, out string) error { return os.WriteFile(out, nil, 0o640) }
	if out, err = e.p.Lookup(context.Background()); err != nil || out.Skipped != "no gap /24s" {
		t.Errorf("no gaps: %+v %v", out, err)
	}
}

// mmdbbuilder gaps runs again only when the MMDB's ETag changed.
func TestLookupGapsOnlyOnNewMMDB(t *testing.T) {
	srv := newRDAP(t, statusHandler)
	e := newEnv(t, srv, 8)
	for i := 0; i < 2; i++ {
		e.db.hasRow = false
		if _, err := e.p.Lookup(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if e.gaps != 1 {
		t.Errorf("gaps ran %d times for the same MMDB", e.gaps)
	}
	for k := range e.store.objects {
		e.store.objects[k] = publish.ObjectInfo{ETag: "e2"}
	}
	e.db.hasRow = false
	if out, err := e.p.Lookup(context.Background()); err != nil || !out.GapsBuilt || e.gaps != 2 {
		t.Errorf("new MMDB: %+v %v gaps %d", out, err, e.gaps)
	}
}

func TestWalkTargets(t *testing.T) {
	// ARIN's space: 192.0.0.0/16 and 10.0.0.0/8 (merged with a neighbour range).
	dir := t.TempDir()
	stats := filepath.Join(dir, "stats")
	text := statsText + "arin|US|ipv4|11.0.0.0|1048576|20200101|allocated|abc\narin|US|ipv4|203.0.113.0|256|20200101|reserved|abc\n"
	os.WriteFile(stats, []byte(text), 0o640)
	ranges, err := readARINRanges(stats)
	if err != nil || len(ranges) != 2 {
		t.Fatalf("ranges %v %v", ranges, err)
	}
	if ranges[0][0] != 0x0A000000 || ranges[0][1] != 0x0B0FFFFF {
		t.Errorf("merged range = %x", ranges[0])
	}
	// The gap: 192.0.2.0/24 (ARIN), 198.51.100.0/24 (not ARIN), 203.0.113.0/24 (reserved, not ARIN),
	// 10.0.0.0/24 (ARIN but not public), 192.0.255.0/24 (ARIN).
	gap := rdns.ParseGaps([]byte("10.0.0.0/24 1 US\n192.0.2.0/24 1 US\n192.0.255.0/24 1 US\n198.51.100.0/24 1 US\n203.0.113.0/24 1 US\n"))
	idx := func(s string) uint32 { return u32(netip.MustParseAddr(s)) >> 8 }
	list := []uint32{idx("192.0.2.0"), idx("198.51.100.0"), idx("203.0.113.0"), idx("10.0.0.0"), idx("192.0.255.0"), idx("192.0.3.0")}
	at := func(i uint32) uint32 { return list[i] }
	notTen := func(a netip.Addr) bool { return a.As4()[0] != 10 } // the documentation ranges are not public either
	if ipdata.PublicV4(netip.MustParseAddr("10.0.0.1")) || !ipdata.PublicV4(netip.MustParseAddr("8.8.8.8")) {
		t.Error("PublicV4")
	}
	got, end := walk(gap, ranges, at, notTen, len(list), 0, 10, 100)
	if end != len(list) || len(got) != 2 {
		t.Fatalf("got %v end %d", got, end)
	}
	if got[0].Pos != 0 || got[0].Addr.As4()[2] != 2 || got[1].Pos != 4 || got[1].Addr.As4()[2] != 255 {
		t.Errorf("targets %v", got)
	}
	for _, g := range got {
		if g.Addr != addrOf(u32(g.Addr)&^255|uint32(hostOf(u32(g.Addr)>>8))) {
			t.Errorf("address %v is not the host of its /24", g.Addr)
		}
	}
	// A full batch ends the walk after its last target; maxSteps ends it early.
	got, end = walk(gap, ranges, at, notTen, len(list), 0, 1, 100)
	if len(got) != 1 || end != 1 {
		t.Errorf("want 1: %v %d", got, end)
	}
	got, end = walk(gap, ranges, at, notTen, len(list), 1, 10, 2)
	if len(got) != 0 || end != 3 {
		t.Errorf("max steps: %v %d", got, end)
	}
	// The same /24 always gives the same host, from 1 to 254.
	seen := map[byte]bool{}
	for i := uint32(0); i < 5000; i++ {
		h := hostOf(i)
		if h < 1 || h > 254 || h != hostOf(i) {
			t.Fatalf("host %d of %d", h, i)
		}
		seen[h] = true
	}
	if len(seen) < 200 {
		t.Errorf("only %d distinct hosts", len(seen))
	}
	// A /24 that sticks out of the ARIN range is not a target.
	if (arinRanges{{0x0A000000, 0x0A0000FE}}).containsBlock(0x0A000000, 0x0A0000FF) {
		t.Error("a block that sticks out is inside")
	}
}
