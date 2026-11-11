package geofeeds

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testStats = "2|arin|20261109|4|19700101|20261109|-0500\n" +
	"arin|*|ipv4|*|3|summary\n" +
	"arin|US|ipv4|45.10.0.0|1024|20100101|allocated|abc\n" +
	"arin|CA|ipv4|45.20.0.0|768|20100101|assigned|abc\n" +
	"arin|US|ipv6|2600:1000::|28|20100101|allocated|abc\n" +
	"arin|US|ipv4|45.30.0.0|256|20100101|reserved|abc\n"

func TestReadStatsBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats")
	os.WriteFile(path, []byte(testStats), 0o640)
	got, err := readStatsBlocks(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.String())
	}
	want := []string{"45.10.0.0/22", "45.20.0.0/23", "45.20.2.0/24", "2600:1000::/28"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("blocks = %v", names)
	}
	if got, _ := readStatsBlocks(path, map[string]bool{"CA": true}); len(got) != 2 {
		t.Errorf("CA blocks = %v", got)
	}
}

func TestNetworkPrefixes(t *testing.T) {
	inside := rdapRecord{Query: "45.10.0.0/22", Start: "45.10.1.0", End: "45.10.1.255"}
	parent := rdapRecord{Query: "45.10.0.0/22", Start: "45.0.0.0", End: "45.255.255.255"}
	for r, want := range map[*rdapRecord]string{&inside: "[45.10.1.0/24]", &parent: "[45.10.0.0/22]"} {
		if got := fmt.Sprint(networkPrefixes(*r)); got != want {
			t.Errorf("%+v: %s, want %s", *r, got, want)
		}
	}
}

func TestLookupRDAP(t *testing.T) {
	var mu sync.Mutex
	limitedOnce := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/stats":
			w.Write([]byte(testStats))
		case r.URL.Path == "/ip/45.10.0.0/22":
			if !limitedOnce {
				limitedOnce = true
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.Write([]byte(`{"startAddress":"45.10.0.0","endAddress":"45.10.3.255",
				"remarks":[{"description":["Geofeed: https://isp.example/geofeed.csv"]}]}`))
		case r.URL.Path == "/ip/2600:1000::/28":
			w.Write([]byte(`{"startAddress":"2600:1000::","endAddress":"2600:100f:ffff:ffff:ffff:ffff:ffff:ffff",
				"links":[{"rel":"geo","type":"application/geofeed+csv","href":"https://v6.example/feed.csv"}]}`))
		case strings.HasPrefix(r.URL.Path, "/ip/45.20."):
			w.Write([]byte(`{"startAddress":"45.20.0.0","endAddress":"45.20.2.255","remarks":[{"description":["no feed"]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	now := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	p := &Pipeline{DataDir: t.TempDir(), allowPrivate: true, store: newFakeStore(), now: func() time.Time { return now },
		rdap: []rdapRegistry{{"arin", server.URL + "/stats", nil, server.URL, time.Millisecond, 100, time.Millisecond}}}

	out, err := p.LookupRDAP(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	s := out.Registries["arin"]
	if s.Status != "ok" || s.Blocks != 4 || s.Due != 4 || s.Lookups != 4 || s.Found != 2 || s.RateLimited != 1 {
		t.Errorf("stats = %+v", s)
	}
	if out.Known != 4 || out.WithFeeds != 2 {
		t.Errorf("out = %+v", out)
	}

	// Nothing is due again until the recheck time.
	if out, err := p.LookupRDAP(context.Background(), 0); err != nil || out.Registries["arin"].Due != 0 {
		t.Errorf("second run: %+v %v", out, err)
	}

	// discover turns the results into blocks with the registry as source.
	p.dumps = []dump{}
	p.caidaBase, p.listPages = server.URL+"/caida", []string{}
	if _, err := p.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	blocks, _ := os.ReadFile(filepath.Join(p.dir(), BlocksFile))
	for _, want := range []string{"arin,45.10.0.0/22,https://isp.example/geofeed.csv\n", "arin,2600:1000::/28,https://v6.example/feed.csv\n"} {
		if !strings.Contains(string(blocks), want) {
			t.Errorf("blocks.csv lacks %q:\n%s", want, blocks)
		}
	}
}

func TestLookupRDAPStopsWhenRateLimited(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stats" {
			w.Write([]byte(testStats))
			return
		}
		requests++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	p := &Pipeline{DataDir: t.TempDir(), allowPrivate: true,
		rdap: []rdapRegistry{{"arin", server.URL + "/stats", nil, server.URL, time.Millisecond, 100, time.Millisecond}}}
	out, err := p.LookupRDAP(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s := out.Registries["arin"]; s.Status != "stopped" || s.Lookups != 0 || requests != rdapMaxLimited {
		t.Errorf("stats = %+v, %d requests", s, requests)
	}
}

func TestLoadRDAPSkipsCutLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), RDAPFile)
	os.WriteFile(path, []byte(`{"registry":"arin","query":"45.10.0.0/22","status":"none"}
{"registry":"arin","query":"45.10.0.0/22","status":"found","feeds":["https://a.example/f.csv"]}
{"registry":"arin","query":"45.20`), 0o640)
	known, err := loadRDAP(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(known) != 1 || known["arin|45.10.0.0/22"].Status != "found" {
		t.Errorf("known = %+v", known)
	}
}

// A minimal anonymous FTP server, enough for fetchFTP.
func fakeFTP(t *testing.T, body string, modified time.Time) (addr string, retrs *int) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	retrs = new(int)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				fmt.Fprint(conn, "220 hi\r\n")
				var data net.Listener
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.Fields(strings.TrimSpace(line))[0]
					switch cmd {
					case "USER":
						fmt.Fprint(conn, "331 password\r\n")
					case "PASS":
						fmt.Fprint(conn, "230 ok\r\n")
					case "TYPE":
						fmt.Fprint(conn, "200 ok\r\n")
					case "MDTM":
						if modified.IsZero() {
							fmt.Fprint(conn, "550 no MDTM here\r\n")
							continue
						}
						fmt.Fprintf(conn, "213 %s\r\n", modified.Format("20060102150405"))
					case "SIZE":
						fmt.Fprintf(conn, "213 %d\r\n", len(body))
					case "EPSV":
						data, _ = net.Listen("tcp4", "127.0.0.1:0")
						fmt.Fprintf(conn, "229 Entering Extended Passive Mode (|||%d|)\r\n", data.Addr().(*net.TCPAddr).Port)
					case "RETR":
						*retrs++
						fmt.Fprint(conn, "150 sending\r\n")
						d, err := data.Accept()
						if err == nil {
							d.Write([]byte(body))
							d.Close()
						}
						data.Close()
						fmt.Fprint(conn, "226 done\r\n")
					case "QUIT":
						fmt.Fprint(conn, "221 bye\r\n")
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), retrs
}

func TestFetchFTP(t *testing.T) {
	modified := time.Date(2026, 11, 8, 18, 26, 0, 0, time.UTC)
	addr, retrs := fakeFTP(t, "dump bytes", modified)
	path := filepath.Join(t.TempDir(), "radb.gz")
	ctx := context.Background()
	if status, err := fetchFTP(ctx, "ftp://"+addr+"/radb/dbase/radb.db.gz", path, true); err != nil || status != statusDownloaded {
		t.Fatalf("first fetch: %s %v", status, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "dump bytes" {
		t.Errorf("file = %q", data)
	}
	if status, err := fetchFTP(ctx, "ftp://"+addr+"/radb/dbase/radb.db.gz", path, true); err != nil || status != statusCached || *retrs != 1 {
		t.Errorf("second fetch: %s %v, %d downloads", status, err, *retrs)
	}
	if _, err := fetchFTP(ctx, "ftp://"+addr+"/x.gz", path, false); err == nil {
		t.Error("an FTP server on 127.0.0.1 was not blocked")
	}
}

// Without MDTM, an unchanged size keeps the cached copy.
func TestFetchFTPWithoutMDTM(t *testing.T) {
	addr, retrs := fakeFTP(t, "dump bytes", time.Time{})
	path := filepath.Join(t.TempDir(), "radb.gz")
	for i, want := range []string{statusDownloaded, statusCached} {
		if status, err := fetchFTP(context.Background(), "ftp://"+addr+"/radb.db.gz", path, true); err != nil || status != want {
			t.Errorf("fetch %d: %s %v, want %s", i+1, status, err, want)
		}
	}
	if *retrs != 1 {
		t.Errorf("%d downloads", *retrs)
	}
}

// A failed lookup keeps the block's last answer, until it is too old to trust.
func TestLookupRDAPFailureKeepsLastAnswer(t *testing.T) {
	var down atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/stats":
			w.Write([]byte("arin|US|ipv4|45.10.0.0|1024|20100101|allocated|abc\n"))
		case down.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.Write([]byte(`{"startAddress":"45.10.0.0","endAddress":"45.10.3.255",
				"remarks":[{"description":["Geofeed: https://isp.example/geofeed.csv"]}]}`))
		}
	}))
	defer server.Close()
	now := time.Now().UTC() // lookup times are wall-clock times
	p := &Pipeline{DataDir: t.TempDir(), allowPrivate: true, store: newFakeStore(), now: func() time.Time { return now },
		rdap:  []rdapRegistry{{"arin", server.URL + "/stats", nil, server.URL, time.Millisecond, 100, time.Millisecond}},
		dumps: []dump{}, caidaBase: server.URL + "/caida", listPages: []string{}}
	if _, err := p.LookupRDAP(context.Background(), 0); err != nil {
		t.Fatal(err)
	}

	down.Store(true)
	now = now.Add(8 * 24 * time.Hour)
	out, err := p.LookupRDAP(context.Background(), 0)
	if err != nil || out.Registries["arin"].Failed != 1 || out.WithFeeds != 1 {
		t.Fatalf("failed recheck: %+v %v", out, err)
	}
	block := "arin,45.10.0.0/22,https://isp.example/geofeed.csv\n"
	for _, tt := range []struct {
		after time.Duration
		kept  bool
	}{{0, true}, {60 * 24 * time.Hour, false}} {
		now = now.Add(tt.after)
		if _, err := p.Discover(context.Background()); err != nil {
			t.Fatal(err)
		}
		blocks, _ := os.ReadFile(filepath.Join(p.dir(), BlocksFile))
		if strings.Contains(string(blocks), block) != tt.kept {
			t.Errorf("after %s: block kept = %v, want %v:\n%s", tt.after, !tt.kept, tt.kept, blocks)
		}
	}
}

// A registry that keeps failing stops for the run, and registries that share
// a stats file download it once.
func TestLookupRDAPStopsWhenFailing(t *testing.T) {
	var statsRequests, lookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stats" {
			statsRequests.Add(1)
			for i := range 3 * rdapMaxFailed {
				fmt.Fprintf(w, "lacnic|BR|ipv4|45.10.%d.0|256|20100101|allocated|abc\n", i)
			}
			fmt.Fprint(w, "lacnic|MX|ipv4|45.20.0.0|256|20100101|allocated|abc\n")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/br/") {
			lookups.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	p := &Pipeline{DataDir: t.TempDir(), allowPrivate: true, rdap: []rdapRegistry{
		{"registro.br", server.URL + "/stats", map[string]bool{"BR": true}, server.URL + "/br", time.Millisecond, 100, time.Millisecond},
		{"lacnic-mx", server.URL + "/stats", map[string]bool{"MX": true}, server.URL + "/mx", time.Millisecond, 100, time.Millisecond},
	}}
	out, err := p.LookupRDAP(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s := out.Registries["registro.br"]; s.Status != "stopped" || s.Failed != rdapMaxFailed || lookups.Load() != rdapMaxFailed {
		t.Errorf("registro.br = %+v, %d lookups", s, lookups.Load())
	}
	if s := out.Registries["lacnic-mx"]; s.Status != "ok" || s.Lookups != 1 {
		t.Errorf("lacnic-mx = %+v", s)
	}
	if statsRequests.Load() != 1 {
		t.Errorf("%d stats downloads", statsRequests.Load())
	}
}

func TestLookupRDAPStopsWithLastError(t *testing.T) {
	var stats strings.Builder
	for i := 0; i <= rdapMaxFailed; i++ {
		fmt.Fprintf(&stats, "arin|US|ipv4|45.%d.0.0|256|20100101|allocated|abc\n", 10+i)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stats" {
			w.Write([]byte(stats.String()))
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var logs []string
	p := &Pipeline{DataDir: t.TempDir(), allowPrivate: true, Progress: func(s string) { logs = append(logs, s) },
		rdap: []rdapRegistry{{"arin", server.URL + "/stats", nil, server.URL, time.Millisecond, 100, time.Millisecond}}}
	out, err := p.LookupRDAP(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	s := out.Registries["arin"]
	if s.Status != "stopped" || s.Failed != rdapMaxFailed || !strings.Contains(s.Error, "last: HTTP 503") {
		t.Errorf("stats = %+v", s)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "rdap: arin: 11 blocks, 11 due, looking up 11") {
		t.Errorf("first log line = %q", logs)
	}
}
