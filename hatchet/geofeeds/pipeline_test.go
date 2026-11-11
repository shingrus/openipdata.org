package geofeeds

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func gzipped(t *testing.T, text string) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write([]byte(text))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDiscover(t *testing.T) {
	whois := gzipped(t, "inetnum: 192.0.2.0 - 192.0.2.255\nremarks: Geofeed https://isp.example/geofeed.csv\n\n"+
		"inetnum: 198.51.100.0 - 198.51.100.255\ngeofeed: https://isp.example/geofeed.csv\n\n"+
		"inetnum: 203.0.113.0 - 203.0.113.255\nremarks: no feed here\n")
	irr := gzipped(t, "route: 203.0.113.0/24\nremarks: geofeed https://other.example/feed.csv\n")
	day := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/ripe.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(whois) })
	mux.HandleFunc("/irr.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(irr) })
	mux.HandleFunc("/broken.gz", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/caida/2026/11/08/standard_geofeeds.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ripe_remarks_1_https://isp.example/geofeed.csv\narin_nonstandard_1_https://arin.example/g.csv\n"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	store := newFakeStore()
	p := &Pipeline{
		store:   store,
		DataDir: t.TempDir(),
		dumps: []dump{
			{"ripe", "ripe", server.URL + "/ripe.gz"},
			{"irr", "irr", server.URL + "/irr.gz"},
			{"lacnic-irr", "irr", server.URL + "/broken.gz"}, // a missing IRR dump is not fatal
		},
		caidaBase: server.URL + "/caida", listPages: []string{},
		now:          func() time.Time { return day },
		allowPrivate: true,
	}
	out, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s := out.Sources["ripe"]; s.Status != statusDownloaded || s.Objects != 3 || s.Blocks != 2 || s.URLs != 1 {
		t.Errorf("ripe stats = %+v", s)
	}
	if s := out.Sources["lacnic-irr"]; s.Status != statusFailed || s.Error == "" {
		t.Errorf("lacnic-irr stats = %+v", s)
	}
	if s := out.Sources["caida"]; s.Status != statusDownloaded || s.URLs != 2 {
		t.Errorf("caida stats = %+v", s)
	}
	if out.Blocks != 3 {
		t.Errorf("blocks = %d", out.Blocks)
	}

	for u, want := range map[string][]string{
		"https://arin.example/g.csv":                       {"caida"},
		"https://isp.example/geofeed.csv":                  {"caida", "ripe"},
		"https://other.example/feed.csv":                   {"irr"},
		"https://mask-api.icloud.com/egress-ip-ranges.csv": {"list"},
	} {
		if r := store.row(u); r == nil || !reflect.DeepEqual(r.Sources, want) || !r.FirstSeen.Equal(day) || !store.lastSeen[u].Equal(day) {
			t.Errorf("%s: row %+v, want sources %v first seen %v", u, r, want, day)
		}
	}
	if _, err := os.Stat(filepath.Join(p.dir(), "urls.csv")); err == nil {
		t.Error("urls.csv was written")
	}
	blocks, err := os.ReadFile(filepath.Join(p.dir(), BlocksFile))
	if err != nil {
		t.Fatal(err)
	}
	wantBlocks := "source,prefix,url\n" +
		"ripe,192.0.2.0/24,https://isp.example/geofeed.csv\n" +
		"ripe,198.51.100.0/24,https://isp.example/geofeed.csv\n" +
		"irr,203.0.113.0/24,https://other.example/feed.csv\n"
	if string(blocks) != wantBlocks {
		t.Errorf("blocks.csv:\n%s\nwant:\n%s", blocks, wantBlocks)
	}

	// A later run that finds less changes nothing about the rows the table already holds:
	// sources are never removed, and a URL nobody lists any more stays.
	later := day.Add(24 * time.Hour)
	p.now = func() time.Time { return later }
	caida := p.caidaBase
	p.dumps, p.caidaBase = []dump{{"irr", "irr", server.URL + "/irr.gz"}}, server.URL+"/no-caida"
	if _, err := p.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := store.row("https://isp.example/geofeed.csv"); r == nil || !reflect.DeepEqual(r.Sources, []string{"caida", "ripe"}) || !store.lastSeen[r.URL].Equal(day) {
		t.Errorf("a URL nobody listed: %+v", r)
	}
	if r := store.row("https://other.example/feed.csv"); !reflect.DeepEqual(r.Sources, []string{"irr"}) || !store.lastSeen[r.URL].Equal(later) {
		t.Errorf("a URL listed again: %+v, last seen %v", r, store.lastSeen[r.URL])
	}
	p.now, p.caidaBase = func() time.Time { return day }, caida

	// One whois dump missing fails the run and keeps the previous outputs.
	before, _ := os.ReadFile(filepath.Join(p.dir(), BlocksFile))
	upserts := store.upserts
	// Not stored yet: a failed run must not touch the table.
	p.dumps = []dump{{"ripe", "ripe", server.URL + "/ripe.gz"}, {"lacnic", "lacnic", server.URL + "/broken.gz"}}
	if _, err := p.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "lacnic") {
		t.Errorf("want an error naming the missing dump, got %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(p.dir(), BlocksFile)); string(after) != string(before) || store.upserts != upserts {
		t.Error("a failed run replaced blocks.csv or updated the table")
	}

	// A corrupt cached dump is removed, so the next run downloads it again.
	os.WriteFile(filepath.Join(p.dir(), "dumps", "ripe.gz"), []byte("not gzip"), 0o640)
	p.dumps = []dump{{"ripe", "ripe", server.URL + "/broken.gz"}}
	if _, err := p.Discover(context.Background()); err == nil {
		t.Error("want an error when no whois dump can be read")
	}
	if _, err := os.Stat(filepath.Join(p.dir(), "dumps", "ripe.gz")); !os.IsNotExist(err) {
		t.Errorf("corrupt dump kept: %v", err)
	}
}

// A dump that breaks halfway adds none of its blocks.
func TestReadDumpBroken(t *testing.T) {
	whole := gzipped(t, "inetnum: 192.0.2.0 - 192.0.2.255\ngeofeed: https://isp.example/geofeed.csv\n\n"+
		"inetnum: 198.51.100.0 - 198.51.100.255\ngeofeed: https://isp.example/geofeed.csv\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(whole[:len(whole)-8]) // no gzip trailer
	}))
	defer server.Close()
	p := &Pipeline{DataDir: t.TempDir(), allowPrivate: true}
	c := &collector{urls: map[string]*found{}, blocks: csv.NewWriter(io.Discard)}
	path := filepath.Join(t.TempDir(), "ripe.gz")
	stats := p.readDump(context.Background(), newHTTPClient(time.Minute, true), c, dump{"ripe", "ripe", server.URL}, path)
	if stats.Status != statusFailed || stats.Blocks != 0 || len(c.urls) != 0 || c.nRows != 0 {
		t.Errorf("stats = %+v, %d URLs, %d rows", stats, len(c.urls), c.nRows)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the broken dump was kept")
	}
}

// rdap-lookups runs beside discover: Discover (and loadRDAP) must never fail or
// see a partial result while rdap.jsonl is appended to in pieces and compacted.
func TestDiscoverListPages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"url":"https:\/\/dl.example\/f1\/geo.csv"} <a href="https://dl.example/f1/byoip.csv">x</a> https://dl.example/f1/geo.csv`))
	})
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>no links</html>")) })
	server := httptest.NewServer(mux)
	defer server.Close()
	store := newFakeStore()
	p := &Pipeline{
		DataDir:      t.TempDir(),
		store:        store,
		dumps:        []dump{},
		caidaBase:    server.URL + "/caida",
		listPages:    []string{server.URL + "/page", server.URL + "/empty", server.URL + "/gone"},
		allowPrivate: true,
	}
	out, err := p.Discover(context.Background()) // the empty and the missing page do not fail it
	if err != nil {
		t.Fatal(err)
	}
	s := out.Sources["list"]
	if s.URLs < 2 || !strings.Contains(s.Error, "/empty: no feed links") || !strings.Contains(s.Error, "/gone: HTTP 404") || strings.Contains(s.Error, "/page:") {
		t.Errorf("list stats = %+v", s)
	}
	for _, u := range []string{"https://dl.example/f1/geo.csv", "https://dl.example/f1/byoip.csv"} {
		if r := store.row(u); r == nil || !reflect.DeepEqual(r.Sources, []string{"list"}) {
			t.Errorf("%s: row %+v", u, r)
		}
	}
}

func TestDiscoverWhileRDAPIsWritten(t *testing.T) {
	whois := gzipped(t, "inetnum: 192.0.2.0 - 192.0.2.255\ngeofeed: https://isp.example/geofeed.csv\n")
	mux := http.NewServeMux()
	mux.HandleFunc("/ripe.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(whois) })
	server := httptest.NewServer(mux)
	defer server.Close()
	day := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	p := &Pipeline{
		DataDir:   t.TempDir(),
		store:     newFakeStore(),
		dumps:     []dump{{"ripe", "ripe", server.URL + "/ripe.gz"}},
		caidaBase: server.URL + "/caida", listPages: []string{}, // 404: the CAIDA lists are optional
		now:          func() time.Time { return day },
		allowPrivate: true,
	}
	path := filepath.Join(p.dir(), RDAPFile)
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		t.Fatal(err)
	}

	const total = 600
	var writeErr error
	written := make(chan struct{})
	go func() {
		defer close(written)
		known := map[string]rdapRecord{}
		var file *os.File
		for i := 0; i < total && writeErr == nil; i++ {
			if file == nil {
				file, writeErr = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
				if writeErr != nil {
					return
				}
			}
			r := rdapRecord{Registry: "arin", Query: fmt.Sprintf("45.%d.%d.0/24", i/256, i%256), Status: "found",
				OK: day.Format(time.RFC3339), Checked: day.Format(time.RFC3339), Next: day.Add(time.Hour).Format(time.RFC3339),
				Feeds: []string{fmt.Sprintf("https://feed%d.example/geofeed.csv", i)}}
			known[r.key()] = r
			line, _ := json.Marshal(r)
			line = append(line, '\n')
			// Like a bufio flush in the middle of a line: a reader can see half of it.
			for _, part := range [][]byte{line[:len(line)/2], line[len(line)/2:]} {
				if _, writeErr = file.Write(part); writeErr != nil {
					return
				}
				runtime.Gosched()
			}
			if i%100 == 99 { // the end of a run: close, then compact
				if writeErr = file.Close(); writeErr != nil {
					return
				}
				file = nil
				writeErr = compactRDAP(path, known)
			}
		}
		if file != nil {
			writeErr = file.Close()
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	var readErr error
	go func() { // a bare reader, as fast as it can go
		defer wg.Done()
		last := 0
		for {
			known, err := loadRDAP(path)
			if err != nil {
				readErr = err
				return
			}
			// Every record is whole, and none that was complete goes missing.
			for _, r := range known {
				if len(r.Feeds) != 1 || r.Status != "found" || r.OK == "" {
					readErr = fmt.Errorf("partial record %+v", r)
					return
				}
			}
			if len(known) < last {
				readErr = fmt.Errorf("records went from %d to %d", last, len(known))
				return
			}
			last = len(known)
			select {
			case <-written:
				return
			default:
			}
		}
	}()

	lastBlocks := 0
	for runs := 0; ; runs++ {
		out, err := p.Discover(context.Background())
		if err != nil {
			t.Fatalf("Discover run %d: %v", runs, err)
		}
		if out.Blocks < lastBlocks {
			t.Fatalf("blocks went from %d to %d", lastBlocks, out.Blocks)
		}
		lastBlocks = out.Blocks
		select {
		case <-written:
		default:
			if runs < 1000 {
				continue
			}
		}
		break
	}
	wg.Wait()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if out, err := p.Discover(context.Background()); err != nil || out.Blocks != total+1 {
		t.Errorf("final Discover: %d blocks, %v; want %d", out.Blocks, err, total+1)
	}
}

// Discover and LookupRDAP share the dumps directory: no cache file may have two owners.
func TestDownloadNamesDoNotClash(t *testing.T) {
	seen := map[string]string{}
	for _, d := range defaultDumps {
		seen[d.Name+".gz"] = "dump " + d.Name
	}
	if len(seen) != len(defaultDumps) {
		t.Error("two dumps share a cache file name")
	}
	dir := t.TempDir()
	for _, reg := range defaultRDAP {
		name := statsPath(dir, reg.StatsURL)
		if who, ok := seen[filepath.Base(name)]; ok {
			t.Errorf("stats file of %s is also the file of %s", reg.Name, who)
		}
	}
}
