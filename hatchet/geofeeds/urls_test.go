package geofeeds

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore is an in-memory public.geofeed_urls with the same rules as the
// Postgres statements: an upsert merges the sources and never removes a row.
type fakeStore struct {
	mu       sync.Mutex
	rows     map[string]*urlRow
	lastSeen map[string]time.Time
	upserts  int
	saves    int                  // SaveChecks calls, failed ones too
	saveErr  func(call int) error // a save fails with this error (call is 1-based); nil: never
}

func newFakeStore(rows ...urlRow) *fakeStore {
	s := &fakeStore{rows: map[string]*urlRow{}, lastSeen: map[string]time.Time{}}
	for _, r := range rows {
		r := r
		s.rows[r.URL] = &r
	}
	return s
}

func (s *fakeStore) Load(context.Context) ([]urlRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []urlRow
	for _, r := range s.rows {
		r := *r
		r.Sources = slices.Clone(r.Sources)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out, nil
}

func (s *fakeStore) Upsert(_ context.Context, found []discoveredURL, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts++
	for _, f := range found {
		if r := s.rows[f.URL]; r != nil {
			r.Sources = mergeSources(r.Sources, f.Sources)
		} else {
			s.rows[f.URL] = &urlRow{URL: f.URL, Sources: mergeSources(nil, f.Sources), FirstSeen: now}
		}
		s.lastSeen[f.URL] = now
	}
	return nil
}

func (s *fakeStore) SaveChecks(_ context.Context, rows []urlRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if s.saveErr != nil {
		if err := s.saveErr(s.saves); err != nil {
			return err
		}
	}
	for _, r := range rows {
		if cur := s.rows[r.URL]; cur != nil {
			cur.LastChecked, cur.LastSuccess, cur.Status, cur.Result = r.LastChecked, r.LastSuccess, r.Status, r.Result
		}
	}
	return nil
}

// row returns a copy of the stored row, or nil.
func (s *fakeStore) row(url string) *urlRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[url]; r != nil {
		c := *r
		return &c
	}
	return nil
}

func TestUpsertMergesSourcesAndNeverRemoves(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)
	t1 := t0.Add(24 * time.Hour)
	s := newFakeStore()
	if err := s.Upsert(ctx, []discoveredURL{
		{"https://a.example/g.csv", []string{"ripe"}},
		{"https://b.example/g.csv", []string{"caida", "list"}},
	}, t0); err != nil {
		t.Fatal(err)
	}
	// The next run: a has a new source, b has fewer, c is new, and d (stored by an older tool) is not listed.
	s.rows["https://d.example/g.csv"] = &urlRow{URL: "https://d.example/g.csv", Sources: []string{"arin"}, FirstSeen: t0}
	if err := s.Upsert(ctx, []discoveredURL{
		{"https://a.example/g.csv", []string{"apnic"}},
		{"https://b.example/g.csv", []string{"list"}},
		{"https://c.example/g.csv", []string{"ripe"}},
	}, t1); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"https://a.example/g.csv": {"apnic", "ripe"},
		"https://b.example/g.csv": {"caida", "list"}, // caida stays
		"https://c.example/g.csv": {"ripe"},
		"https://d.example/g.csv": {"arin"}, // never removed
	}
	rows, _ := s.Load(ctx)
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if !reflect.DeepEqual(r.Sources, want[r.URL]) {
			t.Errorf("%s: sources %v, want %v", r.URL, r.Sources, want[r.URL])
		}
	}
	if !s.row("https://a.example/g.csv").FirstSeen.Equal(t0) || !s.lastSeen["https://a.example/g.csv"].Equal(t1) ||
		!s.row("https://c.example/g.csv").FirstSeen.Equal(t1) || !s.lastSeen["https://d.example/g.csv"].IsZero() {
		t.Errorf("first seen / last seen wrong: %+v %v", rows, s.lastSeen)
	}
}

func TestMergeSources(t *testing.T) {
	if got := mergeSources([]string{"ripe", "irr"}, []string{"caida", "ripe", "arin"}); !reflect.DeepEqual(got, []string{"arin", "caida", "irr", "ripe"}) {
		t.Errorf("got %v", got)
	}
	if got := mergeSources(nil, nil); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestResultText(t *testing.T) {
	tests := []struct {
		status       string
		http, rows   int
		detail, want string
		wantRows     int
	}{
		{feedOK, 200, 3412, "", "HTTP 200, 3412 rows", 3412},
		{feedOK, 304, 7, "", "HTTP 304, 7 rows", 7},
		{feedHTTPError, 404, 0, "", "HTTP 404", 0},
		{feedHTML, 200, 0, "web page", "HTTP 200, web page", 0},
		{feedInvalid, 200, 0, "no valid rows", "HTTP 200, no valid rows", 0},
		{feedTimedOut, 0, 0, "context deadline exceeded", "context deadline exceeded", 0},
	}
	for _, tt := range tests {
		got := resultText(tt.status, tt.http, tt.rows, tt.detail)
		if got != tt.want || resultRows(got) != tt.wantRows {
			t.Errorf("resultText(%s) = %q (rows %d), want %q (%d)", tt.status, got, resultRows(got), tt.want, tt.wantRows)
		}
	}
	// The error text has no URL and is cut short.
	err := &url.Error{Op: "Get", URL: "https://x.example/f?token=secret", Err: errors.New(strings.Repeat("boom ", 100))}
	if got := errDetail(err); strings.Contains(got, "secret") || strings.Contains(got, "x.example") || len(got) > 210 || !strings.HasPrefix(got, "boom") {
		t.Errorf("errDetail = %q", got)
	}
}

func TestDueFromTimestamps(t *testing.T) {
	now := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	day := 24 * time.Hour
	tests := []struct {
		name string
		row  urlRow
		want bool
	}{
		{"never checked", urlRow{FirstSeen: ago(day)}, true},
		{"worked, checked yesterday", urlRow{LastChecked: ago(day), LastSuccess: ago(day)}, true},
		{"checked minutes ago", urlRow{LastChecked: ago(10 * time.Minute)}, false},
		// Failing since the last success.
		{"last success 1 day ago", urlRow{LastChecked: ago(2 * time.Hour), LastSuccess: ago(day)}, true},
		{"last success 10 days ago, checked 2 days ago", urlRow{LastChecked: ago(2 * day), LastSuccess: ago(10 * day)}, false},
		{"last success 10 days ago, checked 7 days ago", urlRow{LastChecked: ago(7 * day), LastSuccess: ago(10 * day)}, true},
		{"last success 90 days ago, checked 7 days ago", urlRow{LastChecked: ago(7 * day), LastSuccess: ago(90 * day)}, false},
		{"last success 90 days ago, checked 30 days ago", urlRow{LastChecked: ago(30 * day), LastSuccess: ago(90 * day)}, true},
		// Never worked: failing since it was first seen.
		{"never worked, seen 1 day ago", urlRow{FirstSeen: ago(day), LastChecked: ago(2 * time.Hour)}, true},
		{"never worked, seen 10 days ago, checked 2 days ago", urlRow{FirstSeen: ago(10 * day), LastChecked: ago(2 * day)}, false},
		{"never worked, seen 10 days ago, checked 7 days ago", urlRow{FirstSeen: ago(10 * day), LastChecked: ago(7 * day)}, true},
		{"never worked, seen 90 days ago, checked 7 days ago", urlRow{FirstSeen: ago(90 * day), LastChecked: ago(7 * day)}, false},
		{"never worked, seen 90 days ago, checked 30 days ago", urlRow{FirstSeen: ago(90 * day), LastChecked: ago(30 * day)}, true},
		// Rows of an older tool: only the timestamps count, not the status.
		{"old tool, fresh check", urlRow{Status: "downloaded", Result: "whatever", LastChecked: ago(time.Minute), LastSuccess: ago(time.Minute)}, false},
		{"old tool, success yesterday", urlRow{Status: "downloaded", LastChecked: ago(day), LastSuccess: ago(day)}, true},
		{"old tool, failed long ago", urlRow{Status: "error", FirstSeen: ago(400 * day), LastChecked: ago(31 * day)}, true},
	}
	for _, tt := range tests {
		if got := tt.row.due(now); got != tt.want {
			t.Errorf("%s: due = %v", tt.name, got)
		}
	}
}

func TestWorkingWindow(t *testing.T) {
	now := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name string
		last time.Time
		want bool
	}{
		{"never", time.Time{}, false},
		{"today", now, true},
		{"29 days ago", now.Add(-29 * 24 * time.Hour), true},
		{"30 days ago", now.Add(-30 * 24 * time.Hour), true},
		{"30 days and a minute ago", now.Add(-30*24*time.Hour - time.Minute), false},
		{"8 days ago (the old window)", now.Add(-8 * 24 * time.Hour), true},
	} {
		if got := (&urlRow{LastSuccess: tt.last}).working(now); got != tt.want {
			t.Errorf("%s: working = %v", tt.name, got)
		}
	}
	if workingDays != 30 {
		t.Errorf("workingDays = %d", workingDays)
	}
}

// Rows from an older tool have other statuses and old timestamps: they come
// due and are overwritten.
func TestCheckFeedsOverwritesForeignRows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/good.csv" {
			w.Write([]byte("45.10.2.0/24,US,,\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	now := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	old := now.Add(-200 * 24 * time.Hour)
	store := newFakeStore(
		urlRow{URL: server.URL + "/good.csv", Sources: []string{"ripe"}, FirstSeen: old, LastChecked: old, LastSuccess: old,
			Status: "downloaded", Result: "old tool text"},
		urlRow{URL: server.URL + "/bad.csv", Sources: []string{"ripe"}, FirstSeen: old, LastChecked: old, Status: "error", Result: "old"},
		urlRow{URL: server.URL + "/new.csv", Sources: []string{"list"}, FirstSeen: old}, // never checked
	)
	p := &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return now }, allowPrivate: true, store: store}
	out, err := p.CheckFeeds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Requests != 3 || out.Skipped != 0 || out.Working != 1 {
		t.Errorf("out = %+v", out)
	}
	if r := store.row(server.URL + "/good.csv"); r.Status != feedOK || r.Result != "HTTP 200, 1 rows" || !r.LastChecked.Equal(now) || !r.LastSuccess.Equal(now) {
		t.Errorf("good = %+v", r)
	}
	if r := store.row(server.URL + "/bad.csv"); r.Status != feedHTTPError || r.Result != "HTTP 404" || !r.LastChecked.Equal(now) || !r.LastSuccess.IsZero() {
		t.Errorf("bad = %+v", r)
	}
	if r := store.row(server.URL + "/new.csv"); r.Status != feedHTTPError || !r.LastChecked.Equal(now) {
		t.Errorf("new = %+v", r)
	}
}

// Results are written back every saveEvery feeds, so a task that is cut off
// and retried does not check those feeds again.
func TestCheckFeedsSavesInBatchesAndResumes(t *testing.T) {
	const total = 2*saveEvery + 50
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte("45.10.2.0/24,US,,\n"))
	}))
	defer server.Close()
	now := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	var rows []urlRow
	for i := range total {
		rows = append(rows, urlRow{URL: server.URL + "/" + strconv.Itoa(i) + ".csv", Sources: []string{"ripe"}, FirstSeen: now.Add(-time.Hour)})
	}
	store := newFakeStore(rows...)
	// The task is cut off: every save after the first one fails, as if the database went away.
	store.saveErr = func(call int) error {
		if call > 1 {
			return errors.New("connection lost")
		}
		return nil
	}
	p := &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return now }, allowPrivate: true, store: store}
	if _, err := p.CheckFeeds(context.Background()); err == nil {
		t.Fatal("want the failed save reported")
	}
	stored := 0
	for _, r := range store.rows {
		if !r.LastChecked.IsZero() {
			stored++
		}
	}
	if stored < saveEvery || stored >= total {
		t.Fatalf("%d feeds stored after the cut-off, want at least %d and fewer than %d", stored, saveEvery, total)
	}

	// The retry fetches only the rest.
	store.saveErr = nil
	requests.Store(0)
	now = now.Add(time.Minute) // within the hour: what was saved is skipped
	out, err := p.CheckFeeds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Skipped != stored || int(requests.Load()) != total-stored || out.Requests != total-stored {
		t.Errorf("retry: %d skipped, %d requests; stored %d of %d", out.Skipped, requests.Load(), stored, total)
	}
	for _, r := range store.rows {
		if r.LastChecked.IsZero() || r.Status != feedOK {
			t.Fatalf("not checked after the retry: %+v", r)
		}
	}
}

func TestStoreNeedsPGSQL(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	if err := p.CheckPGSQL(); err == nil {
		t.Error("want an error without PGSQL")
	}
	if _, err := p.openStore(); err == nil {
		t.Error("a store opened without PGSQL")
	}
	if _, err := p.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "PGSQL is not set") {
		t.Errorf("Discover without PGSQL: %v", err)
	}
	if _, err := p.CheckFeeds(context.Background()); err == nil || !strings.Contains(err.Error(), "PGSQL is not set") {
		t.Errorf("CheckFeeds without PGSQL: %v", err)
	}
	if err := (&Pipeline{PGSQL: "postgres://x"}).CheckPGSQL(); err != nil {
		t.Error(err)
	}
	// A DSN that does not parse must not end up in the error.
	if _, err := (&pgStore{dsn: "s3cr3t-password"}).Load(context.Background()); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("error = %v", err)
	}
}
