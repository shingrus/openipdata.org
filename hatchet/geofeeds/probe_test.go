package geofeeds

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const goodFeed = "# prefix,country,region,city\n45.10.112.0/24,DE,DE-BE,Berlin\n"

// probeServer serves the paths the probe tests ask for and records the requests.
func probeServer(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	mux := http.NewServeMux()
	text := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }
	}
	mux.HandleFunc("/valid", text(goodFeed))
	mux.HandleFunc("/html", text("<!doctype html><title>Not found</title>"))
	mux.HandleFunc("/notfeed", text("name,age\nbob,3\nanna,4\n"))
	mux.HandleFunc("/late", text(strings.Repeat("junk\n", 49)+"45.10.112.0/24,DE\n"))
	mux.HandleFunc("/toolate", text(strings.Repeat("junk\n", 50)+"45.10.112.0/24,DE\n"))
	mux.HandleFunc("/beyond", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("# padding padding padding padding padding padding\n", domainMaxBody/49+10))
		io.WriteString(w, goodFeed)
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/valid", http.StatusFound) })
	mux.HandleFunc("/500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestCandidateURLs(t *testing.T) {
	want := []string{
		"https://example.com/geofeed.csv",
		"https://geofeed.example.com/geofeed.csv",
		"https://www.example.com/geofeed.csv",
		"https://example.com/.well-known/geofeed.csv",
	}
	if got := candidateURLs("example.com"); !reflect.DeepEqual(got, want) {
		t.Errorf("candidates = %v", got)
	}
}

func TestCleanDomain(t *testing.T) {
	tests := map[string]string{
		" Example.COM. ": "example.com",
		"bücher.example": "xn--bcher-kva.example",
		"localhost":      "",
		"1.2.3.4":        "",
		"a b.example":    "",
		"x.example/path": "",
		"x.example:8080": "",
		"":               "",
	}
	for in, want := range tests {
		if got := cleanDomain(in); got != want {
			t.Errorf("cleanDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProbeDomainStopsAtFirstHit(t *testing.T) {
	server, seen := probeServer(t)
	client := newHTTPClient(5*time.Second, true)
	got := probeDomain(context.Background(), client, []string{
		server.URL + "/missing", server.URL + "/html", server.URL + "/valid", server.URL + "/never",
	})
	if got.status != domainFound || got.url != server.URL+"/valid" || got.requests != 3 {
		t.Errorf("result = %+v", got)
	}
	if want := []string{"/missing", "/html", "/valid"}; !reflect.DeepEqual(seen(), want) {
		t.Errorf("requests = %v, want %v", seen(), want)
	}
}

func TestProbeDomainHits(t *testing.T) {
	server, _ := probeServer(t)
	client := newHTTPClient(5*time.Second, true)
	tests := []struct {
		path, want string
	}{
		{"/valid", domainFound},
		{"/redirect", domainFound},
		{"/late", domainFound},   // a valid row among the first 50 data lines
		{"/toolate", domainNone}, // ... but not as the 51st
		{"/beyond", domainNone},  // nothing within the first megabyte
		{"/html", domainNone},    // a web page
		{"/notfeed", domainNone}, // CSV that is not RFC 8805
		{"/missing", domainNone}, // 404
		{"/500", domainFailed},   // a server error says nothing about the site
	}
	for _, tt := range tests {
		got := probeDomain(context.Background(), client, []string{server.URL + tt.path})
		if got.status != tt.want {
			t.Errorf("%s: %s, want %s", tt.path, got.status, tt.want)
		}
	}
	// One server error among misses fails the domain; a hit after it still wins.
	if got := probeDomain(context.Background(), client, []string{server.URL + "/500", server.URL + "/missing"}); got.status != domainFailed {
		t.Errorf("500 then 404: %s", got.status)
	}
	if got := probeDomain(context.Background(), client, []string{server.URL + "/500", server.URL + "/valid"}); got.status != domainFound {
		t.Errorf("500 then valid: %s", got.status)
	}
}

func TestProbeDomainNoAnswer(t *testing.T) {
	server, _ := probeServer(t)
	// Nothing listens: a refused connection is a definite no, and the other
	// candidates on that host are skipped.
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	got := probeDomain(context.Background(), newHTTPClient(5*time.Second, true), []string{closedURL + "/a", closedURL + "/b", strings.Replace(server.URL, "127.0.0.1", "localhost", 1) + "/missing"})
	if got.status != domainNone || got.requests != 2 {
		t.Errorf("refused: %+v", got)
	}
	// The real client never reaches a loopback address: a miss, not a retry.
	got = probeDomain(context.Background(), newHTTPClient(5*time.Second, false), []string{server.URL + "/valid"})
	if got.status != domainNone {
		t.Errorf("blocked address: %+v", got)
	}
}

func TestDueDomains(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	input := map[string][]int64{"b.example": {1}, "a.example": {2}, "old.example": {3}, "older.example": {4}, "fresh.example": {5}, "new.example": {6}}
	known := map[string]domainRecord{
		"old.example":   {Next: at(-time.Hour)},
		"older.example": {Next: at(-48 * time.Hour)},
		"fresh.example": {Next: at(time.Hour)},
		"gone.example":  {Next: at(-time.Hour)}, // no longer in public.asn_domain
	}
	want := []string{"a.example", "b.example", "new.example", "older.example", "old.example"}
	if got := dueDomains(input, known, now); !reflect.DeepEqual(got, want) {
		t.Errorf("due = %v, want %v", got, want)
	}
}

func TestLoadDomainsSkipsCutLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), DomainsFile)
	good1, _ := json.Marshal(domainRecord{Domain: "a.example", Status: domainNone})
	good2, _ := json.Marshal(domainRecord{Domain: "a.example", Status: domainFound, URL: "https://a.example/geofeed.csv"})
	good3, _ := json.Marshal(domainRecord{Domain: "b.example", Status: domainNone})
	os.WriteFile(path, []byte(string(good1)+"\n"+string(good2)+"\n"+string(good3)+"\n"+`{"domain":"c.exa`), 0o640)
	got, err := loadDomains(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a.example"].Status != domainFound || got["b.example"].Status != domainNone {
		t.Errorf("loaded %+v", got)
	}
	if got, err := loadDomains(filepath.Join(t.TempDir(), "missing")); err != nil || len(got) != 0 {
		t.Errorf("missing file: %v, %v", got, err)
	}
}

func TestAnnouncedPrefixes(t *testing.T) {
	raw := `{"prefixes":{"ipv4":["45.10.112.0/22","0.0.0.0/0","bad","45.10.113.5/24"],"ipv6":["2a00:1450::/32","::/0"]}}`
	got := fmt.Sprint(announcedPrefixes(raw))
	if want := "[45.10.112.0/22 45.10.113.0/24 2a00:1450::/32]"; got != want {
		t.Errorf("prefixes = %s, want %s", got, want)
	}
	if announcedPrefixes("not json") != nil {
		t.Error("bad json gave prefixes")
	}
}

func TestPrefixSet(t *testing.T) {
	var s prefixSet
	s.add(netip.MustParsePrefix("45.20.0.0/22"))
	s.add(netip.MustParsePrefix("2a00:1450::/32"))
	tests := map[string]bool{
		"45.20.0.0/22": true, "45.20.1.0/24": true, "45.20.3.255/32": true,
		"45.20.0.0/21": false, "45.20.4.0/24": false, "45.0.0.0/8": false,
		"2a00:1450:4001::/48": true, "2a00:1451::/32": false,
	}
	for in, want := range tests {
		if got := s.contains(netip.MustParsePrefix(in)); got != want {
			t.Errorf("contains(%s) = %v", in, got)
		}
	}
	var none *prefixSet
	if none.contains(netip.MustParsePrefix("45.20.0.0/24")) || (&prefixSet{}).contains(netip.MustParsePrefix("45.20.0.0/24")) {
		t.Error("an empty set contains a prefix")
	}
}

// domainPipeline probes a fake site tree instead of the internet and Postgres.
func domainPipeline(t *testing.T, clock *time.Time, rows []asnDomain, prefixAsks *[][]int64) (*Pipeline, *httptest.Server) {
	// A flaky site answers 500.
	status := map[string]int{"/flaky.example/geofeed.csv": 500}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		code := status[r.URL.Path]
		mu.Unlock()
		switch {
		case code != 0:
			http.Error(w, "x", code)
		case r.URL.Path == "/found.example/.well-known/geofeed.csv":
			io.WriteString(w, goodFeed)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	p := &Pipeline{
		DataDir:      t.TempDir(),
		now:          func() time.Time { return *clock },
		allowPrivate: true,
		domainRows:   func(context.Context) ([]asnDomain, error) { return rows, nil },
		asnPrefixes: func(_ context.Context, asns []int64) (map[int64][]netip.Prefix, error) {
			*prefixAsks = append(*prefixAsks, asns)
			return map[int64][]netip.Prefix{
				1: {netip.MustParsePrefix("45.10.112.0/22")},
				2: {netip.MustParsePrefix("2a00:1450::/32"), netip.MustParsePrefix("45.10.112.0/22")},
				3: {netip.MustParsePrefix("45.99.0.0/16")},
			}, nil
		},
		candidates: func(d string) []string {
			return []string{server.URL + "/" + d + "/geofeed.csv", server.URL + "/" + d + "/.well-known/geofeed.csv"}
		},
	}
	return p, server
}

func readLines(t *testing.T, path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestProbeDomains(t *testing.T) {
	clock := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	rows := []asnDomain{
		{1, "found.example"}, {2, "FOUND.example."}, {3, "none.example"}, {4, "flaky.example"},
		{5, "bad domain"}, {6, "1.2.3.4"},
	}
	var asks [][]int64
	p, server := domainPipeline(t, &clock, rows, &asks)
	foundURL := server.URL + "/found.example/.well-known/geofeed.csv"

	out, err := p.ProbeDomains(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Domains != 3 || out.Invalid != 2 || out.Due != 3 || out.Probed != 3 || out.Found != 1 || out.None != 1 ||
		out.Failed != 1 || out.WithFeeds != 1 || out.Known != 3 {
		t.Errorf("out = %+v", out)
	}
	// Compacted: one line per domain, by name.
	var got []domainRecord
	for _, line := range readLines(t, filepath.Join(p.dir(), DomainsFile)) {
		var r domainRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	at := func(d time.Duration) string { return clock.Add(d).Format(time.RFC3339) }
	want := []domainRecord{
		{Domain: "flaky.example", ASNs: []int64{4}, Checked: at(0), Next: at(domainFailedRecheck), Status: domainFailed},
		{Domain: "found.example", ASNs: []int64{1, 2}, Checked: at(0), Next: at(domainFoundRecheck),
			OK: at(0), Status: domainFound, URL: foundURL},
		{Domain: "none.example", ASNs: []int64{3}, Checked: at(0), Next: at(domainNoneRecheck), Status: domainNone},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("domains.jsonl:\n%+v\nwant:\n%+v", got, want)
	}
	// Prefixes are read only for the ASNs of found domains.
	if !reflect.DeepEqual(asks, [][]int64{{1, 2}}) {
		t.Errorf("prefix reads = %v", asks)
	}
	prefixes, _ := os.ReadFile(filepath.Join(p.dir(), DomainPrefixesFile))
	if want := "url,prefix\n" + foundURL + ",45.10.112.0/22\n" + foundURL + ",2a00:1450::/32\n"; string(prefixes) != want {
		t.Errorf("domain-prefixes.csv:\n%s\nwant:\n%s", prefixes, want)
	}

	// Nothing is due again right away.
	if out, err = p.ProbeDomains(context.Background(), 0); err != nil || out.Due != 0 || out.Probed != 0 || out.WithFeeds != 1 {
		t.Errorf("second run: %+v, %v", out, err)
	}
	// After a day only the failed domain is due.
	clock = clock.Add(25 * time.Hour)
	if out, err = p.ProbeDomains(context.Background(), 0); err != nil || out.Due != 1 || out.Failed != 1 {
		t.Errorf("run after a day: %+v, %v", out, err)
	}

	// A failed recheck keeps the last answer; the domain is retried the next day.
	clock = clock.Add(8 * 24 * time.Hour)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 503) }))
	defer broken.Close()
	p.candidates = func(d string) []string { return []string{broken.URL + "/" + d} }
	if out, err = p.ProbeDomains(context.Background(), 0); err != nil || out.Failed < 1 || out.WithFeeds != 1 {
		t.Errorf("failed recheck: %+v, %v", out, err)
	}
	known, _ := loadDomains(filepath.Join(p.dir(), DomainsFile))
	r := known["found.example"]
	if r.Status != domainFound || r.URL != foundURL || r.OK != clock.Add(-(25*time.Hour+8*24*time.Hour)).Format(time.RFC3339) ||
		r.Next != clock.Add(domainFailedRecheck).Format(time.RFC3339) {
		t.Errorf("record after failed recheck = %+v", r)
	}

	// A domain that left public.asn_domain is forgotten.
	p.domainRows = func(context.Context) ([]asnDomain, error) { return rows[:1], nil }
	if _, err = p.ProbeDomains(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if known, _ = loadDomains(filepath.Join(p.dir(), DomainsFile)); len(known) != 1 || known["found.example"].Domain == "" {
		t.Errorf("after a domain left: %+v", known)
	}
}

func TestProbeDomainsBudget(t *testing.T) {
	clock := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	rows := []asnDomain{{1, "c.example"}, {2, "a.example"}, {3, "b.example"}, {4, "d.example"}}
	var asks [][]int64
	p, _ := domainPipeline(t, &clock, rows, &asks)
	out, err := p.ProbeDomains(context.Background(), 2)
	if err != nil || out.Due != 4 || out.Probed != 2 {
		t.Fatalf("first run: %+v, %v", out, err)
	}
	known, _ := loadDomains(filepath.Join(p.dir(), DomainsFile))
	if _, ok := known["a.example"]; !ok || known["b.example"].Domain == "" || len(known) != 2 {
		t.Errorf("never-probed domains go in name order: %v", known)
	}
	// The next run takes the two never-probed ones first, before anything overdue.
	clock = clock.Add(31 * 24 * time.Hour)
	p.domainRows = func(context.Context) ([]asnDomain, error) { return rows, nil }
	out, _ = p.ProbeDomains(context.Background(), 2)
	known, _ = loadDomains(filepath.Join(p.dir(), DomainsFile))
	if out.Due != 4 || known["c.example"].Checked == "" || known["d.example"].Checked == "" ||
		parseTime(known["c.example"].Checked) != clock || parseTime(known["a.example"].Checked) == clock {
		t.Errorf("second run: %+v, %v", out, known)
	}
}

func TestProbeDomainsNeedsPostgres(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	if err := p.CheckDomains(); err == nil {
		t.Error("want an error without PGSQL")
	}
	if _, err := p.ProbeDomains(context.Background(), 0); err == nil {
		t.Error("ProbeDomains ran without PGSQL")
	}
	if err := (&Pipeline{PGSQL: "postgres://x"}).CheckDomains(); err != nil {
		t.Error(err)
	}
	// An empty table must not wipe the stored results.
	p = &Pipeline{DataDir: t.TempDir(), domainRows: func(context.Context) ([]asnDomain, error) { return nil, nil }}
	if _, err := p.ProbeDomains(context.Background(), 0); err == nil {
		t.Error("an empty public.asn_domain was accepted")
	}
}

func TestDiscoverAddsDomainFeeds(t *testing.T) {
	whois := gzipped(t, "inetnum: 192.0.2.0 - 192.0.2.255\ngeofeed: https://isp.example/geofeed.csv\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ripe.gz" {
			w.Write(whois)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	day := time.Date(2026, 11, 10, 5, 0, 0, 0, time.UTC)
	p := &Pipeline{
		DataDir:      t.TempDir(),
		dumps:        []dump{{"ripe", "ripe", server.URL + "/ripe.gz"}},
		caidaBase:    server.URL + "/caida",
		now:          func() time.Time { return day },
		allowPrivate: true,
	}
	os.MkdirAll(p.dir(), 0o750)
	stamp := func(d time.Duration) string { return day.Add(d).Format(time.RFC3339) }
	// domains.jsonl is appended to while discover runs: a feed found in this run
	// is in it, but not yet in the domain-prefixes.csv snapshot that export-rows
	// reads. Only the sidecar decides what discover lists.
	err := compactJSONL(filepath.Join(p.dir(), DomainsFile), map[string]domainRecord{
		"found.example": {Domain: "found.example", ASNs: []int64{1}, Status: domainFound, URL: "https://found.example/geofeed.csv", OK: stamp(-time.Hour)},
		"also.example":  {Domain: "also.example", ASNs: []int64{2}, Status: domainFound, URL: "https://isp.example/geofeed.csv", OK: stamp(-time.Hour)},
		"new.example":   {Domain: "new.example", ASNs: []int64{3}, Status: domainFound, URL: "https://new.example/geofeed.csv", OK: stamp(-time.Minute)},
		"none.example":  {Domain: "none.example", Status: domainNone},
	})
	if err != nil {
		t.Fatal(err)
	}
	side := "url,prefix\n" +
		"https://found.example/geofeed.csv,198.51.100.0/24\n" +
		"https://isp.example/geofeed.csv,203.0.113.0/24\n"
	if err := os.WriteFile(filepath.Join(p.dir(), DomainPrefixesFile), []byte(side), 0o640); err != nil {
		t.Fatal(err)
	}
	out, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s := out.Sources[sourceDomain]; s.URLs != 2 {
		t.Errorf("domain stats = %+v", s)
	}
	urls, _ := os.ReadFile(filepath.Join(p.dir(), URLsFile))
	for _, want := range []string{
		"https://found.example/geofeed.csv,domain,0",
		"https://isp.example/geofeed.csv,domain|ripe,1", // another source as well: handled as before
	} {
		if !strings.Contains(string(urls), want+"\n") {
			t.Errorf("urls.csv lacks %q:\n%s", want, urls)
		}
	}
	if strings.Contains(string(urls), "new.example") || strings.Contains(string(urls), "none.example") {
		t.Errorf("urls.csv lists a feed that is not in domain-prefixes.csv:\n%s", urls)
	}
	// No ownership block comes from a domain probe.
	blocks, _ := os.ReadFile(filepath.Join(p.dir(), BlocksFile))
	if want := "source,prefix,url\nripe,192.0.2.0/24,https://isp.example/geofeed.csv\n"; string(blocks) != want {
		t.Errorf("blocks.csv:\n%s\nwant:\n%s", blocks, want)
	}
	idx, err := loadOwners(filepath.Join(p.dir(), BlocksFile))
	if err != nil || len(idx.blocks) != 1 || len(idx.sites) != 1 {
		t.Errorf("owner index = %+v, %v", idx, err)
	}
}

func TestExportDomainOnlyFeeds(t *testing.T) {
	now := time.Date(2026, 11, 10, 5, 0, 0, 0, time.UTC)
	p := &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return now }}
	feedsDir := filepath.Join(p.dir(), FeedsDir)
	os.MkdirAll(feedsDir, 0o750)
	writeTestBlocks(t, p.dir())

	const rows = "45.20.0.0/24,DE,,\n45.20.1.128/25,DE,,\n45.20.4.0/24,DE,,\n45.20.0.0/21,DE,,\n2a00:1450:4001::/48,DE,,\n"
	const (
		domainOnly = "https://asn.example/geofeed.csv"
		withWhois  = "https://other.example/geofeed.csv"
		unknown    = "https://nobody.example/geofeed.csv"
	)
	state := map[string]*feedState{
		domainOnly: {Sources: "domain"},
		withWhois:  {Sources: "domain|list"},
		unknown:    {Sources: "domain"}, // no prefixes on record
	}
	for u, s := range state {
		s.LastOK = now.Format(time.RFC3339)
		if _, err := writeFeed(strings.NewReader(rows), feedPath(feedsDir, u), feedHeader{URL: u}); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := json.Marshal(state)
	if err := writeFileAtomic(filepath.Join(p.dir(), StateFile), data); err != nil {
		t.Fatal(err)
	}
	side := "url,prefix\n" +
		domainOnly + ",45.20.0.0/22\n" + domainOnly + ",2a00:1450::/32\n" +
		withWhois + ",45.30.0.0/16\n" // a prefix that covers none of its rows: ignored
	os.WriteFile(filepath.Join(p.dir(), DomainPrefixesFile), []byte(side), 0o640)

	out, err := p.ExportRows(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	// 2 rows of the domain-only feed lie outside, and all 5 of the feed with no prefixes.
	if out.Dropped[dropOutsideASN] != 2+5 || out.Dropped[dropOtherOwner] != 0 || out.Rows != 3+5 || out.Feeds != 3 {
		t.Errorf("out = %+v", out)
	}
	file, err := os.Open(filepath.Join(p.dir(), OutDir, RowsFile))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(gz)
	counts := map[string]int{}
	var domainRows []string
	for sc.Scan() {
		line := sc.Text()
		for _, u := range []string{domainOnly, withWhois, unknown} {
			if strings.HasSuffix(line, ","+u) {
				counts[u]++
				if u == domainOnly {
					domainRows = append(domainRows, strings.TrimSuffix(line, ",,,"+u))
				}
			}
		}
	}
	if counts[domainOnly] != 3 || counts[withWhois] != 5 || counts[unknown] != 0 {
		t.Errorf("rows per feed = %v", counts)
	}
	if want := []string{"45.20.0.0/24,DE", "45.20.1.128/25,DE", "2a00:1450:4001::/48,DE"}; !reflect.DeepEqual(domainRows, want) {
		t.Errorf("domain-only rows = %v, want %v", domainRows, want)
	}
}

func TestLoadDomainPrefixes(t *testing.T) {
	dir := t.TempDir()
	if sets, err := loadDomainPrefixes(filepath.Join(dir, "missing.csv")); err != nil || len(sets) != 0 {
		t.Errorf("missing file: %v, %v", sets, err)
	}
	path := filepath.Join(dir, DomainPrefixesFile)
	os.WriteFile(path, []byte("url,prefix\nhttps://a.example/g.csv,45.20.0.5/22\nhttps://a.example/g.csv,bad\n"), 0o640)
	sets, err := loadDomainPrefixes(path)
	if err != nil || !sets["https://a.example/g.csv"].contains(netip.MustParsePrefix("45.20.1.0/24")) {
		t.Errorf("sets = %v, %v", sets, err)
	}
}
