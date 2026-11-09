package geofeeds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRow(t *testing.T) {
	tests := []struct {
		line, want, reason string
	}{
		{"45.10.2.0/24,US,US-CA,San Jose,95131", "45.10.2.0/24 US US-CA San Jose", ""},
		{" 2a00:1450:1::/48 , de , de-be , Berlin", "2a00:1450:1::/48 DE DE-BE Berlin", ""},
		{"45.10.100.7,GB,,", "45.10.100.7/32 GB  ", ""},
		{"45.10.113.9/24,JP,Tokyo,Tokyo", "45.10.113.0/24 JP  Tokyo", dropBadRegion},
		{"45.10.113.0/24,JP,US-CA,", "45.10.113.0/24 JP  ", dropBadRegion},
		{"10.0.0.0/8,US,,", "", rejectReserved},
		{"100.64.1.0/24,US,,", "", rejectReserved},
		{"192.0.2.0/24,US,,", "", rejectReserved}, // documentation range
		{"fe80::/64,US,,", "", rejectReserved},
		{"::800:0/104,RU,,Moscow", "", rejectReserved}, // 8.0.0.0/8 in an IPv6 MMDB tree
		{"2002:c000:204::/48,US,,", "", rejectReserved},
		{"2001:db8::/32,US,,", "", rejectReserved}, // documentation
		{"2001::/32,US,,", "", rejectReserved},     // Teredo
		{"45.10.113.0/24,JP,,<b>Tokyo</b>", "45.10.113.0/24 JP  ", dropBadCity},
		{"45.10.113.0/24,JP,," + strings.Repeat("x", maxCityLen+1), "45.10.113.0/24 JP  ", dropBadCity},
		{"1.0.0.0/7,US,,", "", rejectTooBroad},
		{"45.10.2.0/24,ZZ,,", "", rejectBadCountry},
		{"45.10.2.0/24,,,", "", rejectBadCountry},
		{"not-an-ip,US,,", "", rejectBadPrefix},
		{"45.10.2.0/24", "", rejectBadLine},
	}
	for _, tt := range tests {
		row, reason := parseRow(strings.Split(tt.line, ","))
		got := ""
		if row.prefix.IsValid() {
			got = strings.Join([]string{row.prefix.String(), row.country, row.region, row.city}, " ")
		}
		if got != tt.want || reason != tt.reason {
			t.Errorf("%q: got %q %q, want %q %q", tt.line, got, reason, tt.want, tt.reason)
		}
	}
}

func TestWriteFeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.csv")
	body := "# comment\n45.10.2.0/24,US,US-CA,\"San Jose\",95131\nbad\n10.0.0.0/8,US,,\n\"unterminated,US\n"
	h, err := writeFeed(strings.NewReader(body), path, feedHeader{URL: "https://a.example/f", ETag: `"x"`})
	if err != nil {
		t.Fatal(err)
	}
	if h.Rows != 1 || h.Rejected[rejectBadLine] < 1 || h.Rejected[rejectReserved] != 1 {
		t.Errorf("header = %+v", h)
	}
	read, err := readFeedHeader(path)
	if err != nil || read.URL != "https://a.example/f" || read.ETag != `"x"` || read.Rows != 1 {
		t.Errorf("read header = %+v, %v", read, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(data), "\n45.10.2.0/24,US,US-CA,San Jose\n") {
		t.Errorf("file:\n%s", data)
	}
}

func TestFetchFeed(t *testing.T) {
	feeds := map[string]string{
		"/ok":    "45.10.2.0/24,US,US-CA,San Jose\n2a00:1450::/32,DE,,\n",
		"/html":  "\n  <!DOCTYPE html><html></html>",
		"/empty": "# nothing here\n",
		"/bad":   "hello,world\n",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := feeds[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "text/html") // misconfigured servers do this; content decides
		w.Write([]byte(body))
	}))
	defer server.Close()
	client := newHTTPClient(time.Minute, true)
	dir := t.TempDir()
	ctx, now := context.Background(), time.Now()

	tests := []struct {
		path, status string
		rows         int
		stored       bool
	}{
		{"/ok", feedOK, 2, true},
		{"/html", feedHTML, 0, false},
		{"/empty", feedEmpty, 0, false},
		{"/bad", feedInvalid, 0, false},
		{"/missing", feedHTTPError, 0, false},
	}
	for _, tt := range tests {
		path := feedPath(dir, server.URL+tt.path)
		res := fetchFeed(ctx, client, server.URL+tt.path, path, now)
		_, statErr := os.Stat(path)
		if res.status != tt.status || res.rows != tt.rows || (statErr == nil) != tt.stored {
			t.Errorf("%s: %+v, stored=%v", tt.path, res, statErr == nil)
		}
	}

	again := fetchFeed(ctx, client, server.URL+"/ok", feedPath(dir, server.URL+"/ok"), now)
	if again.status != feedOK || !again.notModified || again.rows != 2 {
		t.Errorf("revalidation: %+v", again)
	}

	blocked := fetchFeed(ctx, newHTTPClient(time.Minute, false), server.URL+"/ok", filepath.Join(dir, "x.csv"), now)
	if blocked.status != feedBlocked {
		t.Errorf("a feed on 127.0.0.1 was not blocked: %+v", blocked)
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	day := 24 * time.Hour
	tests := []struct {
		name  string
		state feedState
		want  bool
	}{
		{"new", feedState{}, true},
		{"working, checked yesterday", feedState{Checked: ago(day)}, true},
		{"checked minutes ago", feedState{Checked: ago(10 * time.Minute)}, false},
		{"failing 1 day", feedState{Checked: ago(day), FailingSince: ago(day)}, true},
		{"failing 10 days, checked 2 days ago", feedState{Checked: ago(2 * day), FailingSince: ago(10 * day)}, false},
		{"failing 10 days, checked 7 days ago", feedState{Checked: ago(7 * day), FailingSince: ago(10 * day)}, true},
		{"failing 90 days, checked 7 days ago", feedState{Checked: ago(7 * day), FailingSince: ago(90 * day)}, false},
		{"failing 90 days, checked 30 days ago", feedState{Checked: ago(30 * day), FailingSince: ago(90 * day)}, true},
	}
	for _, tt := range tests {
		if got := tt.state.due(now); got != tt.want {
			t.Errorf("%s: due = %v", tt.name, got)
		}
	}
}

func TestInterleaveByHost(t *testing.T) {
	got := interleaveByHost([]string{"https://a/1", "https://a/2", "https://a/3", "https://b/1", "https://c/1"})
	want := []string{"https://a/1", "https://b/1", "https://c/1", "https://a/2", "https://a/3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestCheckFeeds(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/good.csv" {
			w.Write([]byte("45.10.2.0/24,US,,\n45.10.100.0/24,US,,\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	now := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	p := &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return now }, allowPrivate: true}
	os.MkdirAll(p.dir(), 0o750)
	urls := "url,sources,blocks\n" + server.URL + "/good.csv,ripe,3\n" + server.URL + "/gone.csv,caida,0\n"
	os.WriteFile(filepath.Join(p.dir(), URLsFile), []byte(urls), 0o640)

	out, err := p.CheckFeeds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Feeds != 2 || out.Working != 1 || out.Rows != 2 || out.Statuses[feedOK] != 1 || out.Statuses[feedHTTPError] != 1 {
		t.Errorf("out = %+v", out)
	}
	txt, _ := os.ReadFile(filepath.Join(p.dir(), OutDir, ListTXT))
	if !strings.HasSuffix(string(txt), "\n"+server.URL+"/good.csv\n") || strings.Contains(string(txt), "gone") {
		t.Errorf("txt:\n%s", txt)
	}
	csvText, _ := os.ReadFile(filepath.Join(p.dir(), OutDir, ListCSV))
	for _, want := range []string{
		server.URL + "/good.csv,ripe,3,ok,200,2,2026-11-09\n",
		server.URL + "/gone.csv,caida,0,http_error,404,0,\n",
	} {
		if !strings.Contains(string(csvText), want) {
			t.Errorf("csv lacks %q:\n%s", want, csvText)
		}
	}

	// A retry within the hour fetches nothing again.
	requests.Store(0)
	if out, err := p.CheckFeeds(context.Background()); err != nil || requests.Load() != 0 || out.Skipped != 2 || out.Working != 1 {
		t.Errorf("retry: %+v %v, %d requests", out, err, requests.Load())
	}

	// After 30 days without discovery listing them, feeds are dropped.
	now = now.Add(31 * 24 * time.Hour)
	os.WriteFile(filepath.Join(p.dir(), URLsFile), []byte("url,sources,blocks\n"), 0o640)
	if out, err := p.CheckFeeds(context.Background()); err != nil || out.Dropped != 2 || out.Feeds != 0 {
		t.Errorf("drop: %+v %v", out, err)
	}
}

func TestWriteFeedLongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.csv")
	body := strings.Repeat("x", 3*maxFeedLine) + "\n45.10.2.0/24,US,,\n"
	h, err := writeFeed(strings.NewReader(body), path, feedHeader{})
	if err != nil || h.Rows != 1 || h.Rejected[rejectBadLine] != 1 {
		t.Errorf("header = %+v, %v", h, err)
	}
}

// A transfer that breaks keeps the stored copy; a local file error is not a
// feed status.
func TestFetchFeedFailures(t *testing.T) {
	cut := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cut {
			w.Header().Set("Content-Length", "1000") // the connection closes short of this
		}
		w.Write([]byte("45.10.2.0/24,US,,\n"))
	}))
	defer server.Close()
	client := newHTTPClient(time.Minute, true)
	dir := t.TempDir()
	ctx, now := context.Background(), time.Now()
	path := feedPath(dir, server.URL)
	if res := fetchFeed(ctx, client, server.URL, path, now); res.status != feedOK {
		t.Fatalf("first fetch: %+v", res)
	}

	cut = true
	res := fetchFeed(ctx, client, server.URL, path, now)
	if res.status != feedConnection || res.err != nil {
		t.Errorf("cut transfer: %+v", res)
	}
	if h, err := readFeedHeader(path); err != nil || h.Rows != 1 {
		t.Errorf("stored copy after a cut transfer: %+v, %v", h, err)
	}

	cut = false
	res = fetchFeed(ctx, client, server.URL, filepath.Join(dir, "missing", "x.csv"), now)
	if res.err == nil || res.status != "" {
		t.Errorf("local error: %+v", res)
	}
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "193.0.0.1": true,
		"10.0.0.1": false, "127.0.0.1": false, "169.254.169.254": false, "100.64.0.1": false,
		"2a00:1450::1": true, "2001:db8::1": false, "2001::1": false, "2001:1::1": false, "2001:200::1": true,
		"3fff::1": false, "2002::1": false, "::1": false, "0.1.2.3": false, "192.0.0.8": false, "198.18.0.5": false, "240.0.0.1": false, "::ffff:10.0.0.1": false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("publicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

func checkedState(t *testing.T, p *Pipeline) map[string]*feedState {
	t.Helper()
	state, err := loadState(filepath.Join(p.dir(), StateFile))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// A cancelled run must not record the feeds it cut off as failing.
func TestCheckFeedsCancelled(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	p := &Pipeline{DataDir: t.TempDir(), allowPrivate: true}
	os.MkdirAll(p.dir(), 0o750)
	os.WriteFile(filepath.Join(p.dir(), URLsFile), []byte("url,sources,blocks\n"+server.URL+"/slow.csv,ripe,1\n"), 0o640)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := p.CheckFeeds(ctx); err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	if s := checkedState(t, p)[server.URL+"/slow.csv"]; s == nil || s.Checked != "" || s.FailingSince != "" {
		t.Errorf("state = %+v", s)
	}
}

// Once a host stops answering, its other feeds wait for the next run.
func TestCheckFeedsUnreachableHost(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()} // 127.0.0.1 is blocked, which counts as unreachable
	os.MkdirAll(p.dir(), 0o750)
	urls := "url,sources,blocks\n"
	for i := range 20 {
		urls += "http://127.0.0.1:9/" + strconv.Itoa(i) + ".csv,ripe,1\n"
	}
	os.WriteFile(filepath.Join(p.dir(), URLsFile), []byte(urls), 0o640)
	out, err := p.CheckFeeds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Requests+out.Skipped != 20 || out.Requests < hostFailLimit || out.Requests > hostFailLimit+perHostLimit {
		t.Errorf("out = %+v", out)
	}
	unchecked := 0
	for _, s := range checkedState(t, p) {
		if s.Checked == "" {
			unchecked++
			if s.Status != feedHostDown || s.FailingSince == "" {
				t.Errorf("skipped feed state = %+v", s)
			}
		}
	}
	if unchecked != out.Skipped {
		t.Errorf("%d unchecked feeds, %d skipped", unchecked, out.Skipped)
	}
}

// A feed that stopped answering loses its stored copy once it no longer counts
// as working.
func TestCheckFeedsRemovesStaleCopy(t *testing.T) {
	gone := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gone {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("45.10.2.0/24,US,,\n"))
	}))
	defer server.Close()
	now := time.Now().UTC() // Checked and LastOK are wall-clock times
	p := &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return now }, allowPrivate: true}
	os.MkdirAll(p.dir(), 0o750)
	os.WriteFile(filepath.Join(p.dir(), URLsFile), []byte("url,sources,blocks\n"+server.URL+"/f.csv,ripe,1\n"), 0o640)
	path := feedPath(filepath.Join(p.dir(), FeedsDir), server.URL+"/f.csv")
	for _, tt := range []struct {
		after  time.Duration
		stored bool
	}{{0, true}, {2 * 24 * time.Hour, true}, {8 * 24 * time.Hour, false}} {
		now = now.Add(tt.after)
		if _, err := p.CheckFeeds(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); (err == nil) != tt.stored {
			t.Errorf("after %s: stored = %v, want %v", tt.after, err == nil, tt.stored)
		}
		gone = true
	}
}
