package rdns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/schema"
)

// The integration tests run the real tasks against a Postgres (with a fake scamper and a
// fake resolver) and simulate days by moving every stored timestamp back. They need a
// server: set OPENIPDATA_TEST_PGSQL to a DSN that may create databases, e.g.
//
//	docker run -d --rm -p 55432:5432 -e POSTGRES_PASSWORD=pw postgres:16
//	OPENIPDATA_TEST_PGSQL='postgres://postgres:pw@localhost:55432/postgres?sslmode=disable' go test -run Integration ./rdns
//
// Without the variable they are skipped.
const testPGEnv = "OPENIPDATA_TEST_PGSQL"

// newTestDB creates an empty database and returns its DSN; it is dropped at the end of the test.
func newTestDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv(testPGEnv)
	if base == "" {
		t.Skipf("%s is not set", testPGEnv)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(schemaHide(err))
	}
	name := fmt.Sprintf("rdns_it_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `ALTER DATABASE `+name+` SET timezone TO 'UTC'`); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	// Schedule fixtures advance calendar-day intervals in SQL. Pin the session to
	// UTC so DST cannot make a 150-day retry look like 149 days of elapsed time.
	t.Cleanup(func() {
		admin.Exec(ctx, `DROP DATABASE `+name+` WITH (FORCE)`)
		admin.Close(ctx)
	})
	return u.String()
}

func schemaHide(err error) error { return schema.HideDSN(err) }

type itWorld struct {
	t    *testing.T
	dsn  string
	conn *pgx.Conn
	p    *Pipeline
	// scenario: the hops (public ones) that scamper reports for each /24 (first three octets), by target.
	hops map[string]func(target string) []string
	// names: the PTR answers of the fake resolver; a name of "!err" is a lookup error.
	ptrs map[string]string
}

func newWorld(t *testing.T) *itWorld {
	t.Helper()
	dsn := newTestDB(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(schemaHide(err))
	}
	t.Cleanup(func() { conn.Close(ctx) })
	if err := schema.Ensure(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := schema.EnsureVisitorIPs(ctx, conn); err != nil {
		t.Fatal(err)
	}
	w := &itWorld{t: t, dsn: dsn, conn: conn, hops: map[string]func(string) []string{}, ptrs: map[string]string{}}
	w.exec(`CREATE TABLE public.generated_rules (name text, pattern text, domains_json jsonb, country text, city text)`)
	w.exec(`INSERT INTO public.generated_rules VALUES
		('fra', '\bfra\d*\b', '["isp.net"]', 'DE', 'Frankfurt'), ('muc', '\bmuc\d*\b', '["isp.net"]', 'DE', 'Munich')`)
	w.exec(`INSERT INTO public.asn (asn, json, seen_at) VALUES (100, '{"prefixes":{"ipv4":["5.6.0.0/16"]}}', now())`)
	w.ptrs["5.6.0.1"] = "ae1.fra01.isp.net"
	w.ptrs["5.6.0.2"] = "ae1.muc02.isp.net"
	w.ptrs["5.6.0.9"] = "core1.isp.net"
	w.ptrs["5.6.0.10"] = "core2.isp.net"
	w.p = &Pipeline{DataDir: t.TempDir(), PGSQL: dsn, intn: func(int) int { return 40 }}
	w.p.scamper = w.fakeScamper
	w.p.resolver = &ptrResolver{
		system: func(_ context.Context, ip string) ([]string, error) {
			name, ok := w.ptrs[ip]
			switch {
			case name == "!err":
				return nil, &net.DNSError{Err: "servfail", IsTemporary: true}
			case !ok:
				return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
			}
			return []string{name + "."}, nil
		},
		doh: func(context.Context, netip.Addr) ptrAnswer { return ptrAnswer{Status: statusError} },
	}
	return w
}

func (w *itWorld) exec(sql string, args ...any) {
	w.t.Helper()
	if _, err := w.conn.Exec(context.Background(), sql, args...); err != nil {
		w.t.Fatalf("%v\n%s", err, sql)
	}
}

func (w *itWorld) scan(sql string, dest ...any) {
	w.t.Helper()
	if err := w.conn.QueryRow(context.Background(), sql).Scan(dest...); err != nil {
		w.t.Fatalf("%v\n%s", err, sql)
	}
}

func (w *itWorld) count(sql string) int {
	w.t.Helper()
	var n int
	w.scan(sql, &n)
	return n
}

// fakeScamper writes a trace for every target: the hops its /24's scenario names, then the
// private first hop; a /24 with no scenario or no hops is silent.
func (w *itWorld) fakeScamper(_ context.Context, targets, outFile string, _ int) error {
	data, err := os.ReadFile(targets)
	if err != nil {
		return err
	}
	var out strings.Builder
	out.WriteString(`{"type":"cycle-start","id":1}` + "\n")
	for _, target := range strings.Fields(string(data)) {
		var hops []map[string]any
		if f := w.hops[target[:strings.LastIndex(target, ".")]]; f != nil {
			if path := f(target); len(path) > 0 {
				path = append([]string{"10.0.0.1"}, path...)
				for i, h := range path {
					hops = append(hops, map[string]any{"addr": h, "probe_ttl": i + 1, "rtt": 1.5})
				}
			}
		}
		line, _ := json.Marshal(map[string]any{"type": "trace", "dst": target, "hops": hops})
		out.Write(line)
		out.WriteString("\n")
	}
	return os.WriteFile(outFile, []byte(out.String()), 0o640)
}

// run is one run of the four tasks, in order.
func (w *itWorld) run() (PickOutput, TraceOutput, NamesOutput, LocateOutput) {
	w.t.Helper()
	ctx := context.Background()
	pick, err := w.p.PickNetworks(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	tr, err := w.p.Trace(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	names, err := w.p.ResolveNames(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	// The saved "nothing changed" state would skip a rebuild when only time passed.
	os.Remove(filepath.Join(w.p.dir(), locateStateFile))
	loc, err := w.p.Locate(ctx, false)
	if err != nil {
		w.t.Fatal(err)
	}
	return pick, tr, names, loc
}

// age moves every timestamp back by n days, which is the same as waiting n days.
func (w *itWorld) age(n int) {
	w.t.Helper()
	for _, sql := range []string{
		`UPDATE public.traced_networks SET last_traced_at = last_traced_at - make_interval(days => $1::int), next_trace_at = next_trace_at - make_interval(days => $1::int), located_at = located_at - make_interval(days => $1::int)`,
		`UPDATE public.network_traces SET traced_at = traced_at - make_interval(days => $1::int)`,
		`UPDATE public.ip_ptr_names SET resolved_at = resolved_at - make_interval(days => $1::int), tried_at = tried_at - make_interval(days => $1::int)`,
		`UPDATE public.rdns_geo_layer SET located_at = located_at - make_interval(days => $1::int)`,
		`UPDATE public.visitor_ips SET last_seen_at = last_seen_at - make_interval(days => $1::int), first_seen_at = first_seen_at - make_interval(days => $1::int)`,
	} {
		w.exec(sql, n)
	}
}

func (w *itWorld) visitors(prefix string, seenDaysAgo ...int) {
	for i, d := range seenDaysAgo {
		w.exec(`INSERT INTO public.visitor_ips (ip, source, last_seen_at) VALUES ($1, 'test', now() - make_interval(days => $2::int))`,
			fmt.Sprintf("%s.%d", prefix, 10+10*i), d)
	}
}

type netState struct {
	Status   string
	Attempts int
	Next     *time.Time
	Last     time.Time
	Located  *time.Time
}

func (w *itWorld) state(prefix string) *netState {
	w.t.Helper()
	var s netState
	err := w.conn.QueryRow(context.Background(), `SELECT status, attempts, next_trace_at, last_traced_at, located_at FROM public.traced_networks WHERE prefix = $1::cidr`,
		prefix).Scan(&s.Status, &s.Attempts, &s.Next, &s.Last, &s.Located)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		w.t.Fatal(err)
	}
	return &s
}

// wantState checks the status and attempts of a /24 and that next_trace_at is wantDays after
// its last trace (wantDays 0: NULL).
func (w *itWorld) wantState(prefix, status string, attempts, wantDays int) *netState {
	w.t.Helper()
	s := w.state(prefix)
	if s == nil {
		w.t.Fatalf("%s: no schedule row", prefix)
	}
	gotDays := 0
	if s.Next != nil {
		// PostgreSQL calendar-day intervals across daylight-saving changes can be
		// an hour shorter or longer than an exact multiple of 24 hours.
		gotDays = int(s.Next.Sub(s.Last).Round(24*time.Hour) / (24 * time.Hour))
	}
	if s.Status != status || s.Attempts != attempts || gotDays != wantDays || (wantDays == 0) != (s.Next == nil) {
		w.t.Fatalf("%s: status %s attempts %d next after %d days (nil=%v), want %s %d %d", prefix, s.Status, s.Attempts, gotDays, s.Next == nil, status, attempts, wantDays)
	}
	return s
}

func (w *itWorld) layer(prefix, scope string) (city string, located time.Time, ok bool) {
	err := w.conn.QueryRow(context.Background(), `SELECT city, located_at FROM public.rdns_geo_layer WHERE prefix = $1::cidr AND scope = $2`, prefix, scope).Scan(&city, &located)
	if err == pgx.ErrNoRows {
		return "", time.Time{}, false
	}
	if err != nil {
		w.t.Fatal(err)
	}
	return city, located, true
}

// The whole life of three /24s: one nobody answers in, one that answers without a city, one that is located.
func TestIntegrationSchedule(t *testing.T) {
	w := newWorld(t)
	tail := 9                                                                                             // the last hop of /24 B alternates between 5.6.0.9 and 5.6.0.10: the path changes
	w.hops["5.6.1"] = func(string) []string { return nil }                                                // A: silent
	w.hops["5.6.2"] = func(string) []string { return []string{"9.9.9.1", fmt.Sprintf("5.6.0.%d", tail)} } // B: unmatched
	cLocated := true
	w.hops["5.6.3"] = func(string) []string { // C: a router named after Frankfurt, or nothing useful
		if cLocated {
			return []string{"9.9.9.1", "5.6.0.1"}
		}
		return []string{"9.9.9.1", "5.6.0.9"}
	}
	w.visitors("5.6.1", 3, 2, 1) // newest .30
	w.visitors("5.6.2", 3, 2, 1)
	w.visitors("5.6.3", 3, 2, 1)
	w.visitors("5.6.4", 3, 2, 1) // D: no scenario, so silent

	// Day 0: all four never-traced /24s are picked.
	pick, tr, _, loc := w.run()
	if pick.PickedNew != 4 || pick.PickedDue != 0 || pick.PickedRevived != 0 || pick.Eligible != 4 || pick.Backfilled != 0 || tr.Stored != 12 {
		t.Fatalf("day 0: %+v %+v", pick, tr)
	}
	if tr.SilentNets != 2 { // A, and D (no scenario: nobody answers)
		t.Errorf("silent nets = %d", tr.SilentNets)
	}
	w.wantState("5.6.1.0/24", netSilent, 1, 7)
	w.wantState("5.6.2.0/24", netUnmatched, 1, 7)
	w.wantState("5.6.4.0/24", netSilent, 1, 7)
	c := w.wantState("5.6.3.0/24", netLocated, 0, 150)
	if c.Located == nil {
		t.Error("located_at not set")
	}
	if city, _, ok := w.layer("5.6.3.0/24", scopeTraced); !ok || city != "Frankfurt" || loc.Traced != 1 {
		t.Errorf("layer: %s %v %+v", city, ok, loc)
	}
	// Visitors: all but the newest left for traced /24s, none for the located one.
	if n := w.count(`SELECT count(*) FROM public.visitor_ips WHERE ip <<= '5.6.1.0/24'`); n != 1 {
		t.Errorf("visitors of A = %d", n)
	}
	var newest string
	w.scan(`SELECT host(ip) FROM public.visitor_ips WHERE ip <<= '5.6.2.0/24'`, &newest)
	if newest != "5.6.2.30" {
		t.Errorf("kept visitor = %s, want the newest", newest)
	}
	if n := w.count(`SELECT count(*) FROM public.visitor_ips WHERE ip <<= '5.6.3.0/24'`); n != 0 {
		t.Errorf("visitors of C = %d", n)
	}
	if tr.VisitorsCut != 8 { // 4 /24s, 2 deleted each
		t.Errorf("trace deleted %d visitor IPs, want 8", tr.VisitorsCut)
	}
	if loc.VisitorsCut != 1 { // the one visitor IP of C that trace left
		t.Errorf("locate deleted %d visitor IPs, want 1", loc.VisitorsCut)
	}

	// Day 3: nothing is due.
	w.age(3)
	if pick, _, _, _ := w.run(); pick.Picked != 0 || pick.Eligible != 0 {
		t.Fatalf("day 3: %+v", pick)
	}

	// Day 7 (+4 more days of ageing: 3+4): A, B and D are due; C is not. B's path changes.
	w.age(4)
	tail = 10
	pick, tr, _, _ = w.run()
	if pick.PickedDue != 3 || pick.PickedNew != 0 || pick.Picked != 3 {
		t.Fatalf("day 7: %+v", pick)
	}
	w.wantState("5.6.1.0/24", netSilent, 2, 7)
	w.wantState("5.6.2.0/24", netUnmatched, 2, 20)
	// Paths: A, B and D have visitor, gateway and random traces twice (D and A: no hop at all, so no change); B's tail changed.
	if tr.PathCompared != 9 || tr.PathChanged != 3 {
		t.Errorf("paths: %d compared, %d changed", tr.PathCompared, tr.PathChanged)
	}
	if tr.Pruned != 0 {
		t.Errorf("pruned = %d", tr.Pruned)
	}

	// Day 14: A has its 3rd attempt, then no next_trace_at.
	w.age(7)
	pick, _, _, _ = w.run()
	if pick.PickedDue != 2 { // A and D
		t.Fatalf("day 14: %+v", pick)
	}
	w.wantState("5.6.1.0/24", netSilent, 3, 0)
	w.wantState("5.6.4.0/24", netSilent, 3, 0)
	// Day 21: nothing for A, B is not due yet.
	w.age(7)
	if pick, _, _, _ = w.run(); pick.Picked != 0 {
		t.Fatalf("day 21: %+v", pick)
	}
	// Day 27: B's 3rd trace, backoff 60 days.
	w.age(6)
	pick, _, _, _ = w.run()
	if pick.PickedDue != 1 {
		t.Fatalf("day 27: %+v", pick)
	}
	w.wantState("5.6.2.0/24", netUnmatched, 3, 60)
	// Day 87: B's 4th trace: no next_trace_at any more.
	w.age(60)
	pick, _, _, _ = w.run()
	if pick.PickedDue != 1 || pick.Picked != 1 {
		t.Fatalf("day 87: %+v", pick)
	}
	b := w.wantState("5.6.2.0/24", netUnmatched, 4, 0)

	// Day 117: a visitor of B comes, but B was traced 30 days ago: not yet.
	w.age(30)
	w.visitors("5.6.2", 0)
	if pick, _, _, _ = w.run(); pick.Picked != 0 {
		t.Fatalf("day 117: %+v", pick)
	}
	// Day 152: B was traced 65 days ago and its visitor came after that; a visitor of A comes now
	// (A was traced long ago): both are brought back. C is due for its recheck (150 days) and
	// finds nothing: the city stays.
	w.age(35)
	w.visitors("5.6.1", 0)
	cLocated = false
	var cCityBefore string
	var cLocatedBefore time.Time
	cCityBefore, cLocatedBefore, _ = w.layer("5.6.3.0/24", scopeTraced)
	var staleBefore time.Time
	w.scan(`SELECT resolved_at FROM public.ip_ptr_names WHERE ip = '5.6.0.1'`, &staleBefore)
	pick, tr, names, loc := w.run()
	if pick.PickedDue != 1 || pick.PickedRevived != 2 || pick.PickedNew != 0 {
		t.Fatalf("day 152: %+v", pick)
	}
	w.wantState("5.6.2.0/24", netUnmatched, 1, 7)
	w.wantState("5.6.1.0/24", netSilent, 1, 7)
	_ = b
	// C: no visitor was left, so only .1 and the random host were traced, and the row is kept.
	if n := w.count(`SELECT count(*) FROM public.network_traces WHERE prefix = '5.6.3.0/24' AND target_kind = 'visitor'`); n != 1 {
		t.Errorf("visitor traces of C = %d, want only the old one", n)
	}
	if n := w.count(`SELECT count(*) FROM public.network_traces WHERE prefix = '5.6.3.0/24' AND target_kind = 'gateway'`); n != 2 {
		t.Errorf("gateway traces of C = %d", n)
	}
	city, located, ok := w.layer("5.6.3.0/24", scopeTraced)
	if !ok || city != cCityBefore || !located.Equal(cLocatedBefore) || loc.Carried != 1 {
		t.Errorf("recheck that found nothing: %s %v %v (was %s %v), %+v", city, located, ok, cCityBefore, cLocatedBefore, loc.Carried)
	}
	if city, _, ok := w.layer("5.6.0.0/16", scopeWide); !ok || city != "Frankfurt" {
		t.Errorf("the wide row lost its carried vote: %s %v", city, ok)
	}
	c = w.wantState("5.6.3.0/24", netLocated, 0, 150)
	if time.Since(c.Last) > time.Hour {
		t.Errorf("last_traced_at of C = %v", c.Last)
	}
	// The router's name was 152 days old: refreshed this run.
	var staleAfter time.Time
	w.scan(`SELECT resolved_at FROM public.ip_ptr_names WHERE ip = '5.6.0.1'`, &staleAfter)
	if names.Refreshed < 1 || !staleAfter.After(staleBefore) || time.Since(staleAfter) > time.Hour {
		t.Errorf("refresh: %+v, resolved_at %v -> %v", names.Refreshed, staleBefore, staleAfter)
	}
	_ = tr

	// Day 302: C's next recheck finds Munich: the row is replaced (new city, new located_at).
	w.age(150)
	cLocated = true
	w.hops["5.6.3"] = func(string) []string { return []string{"9.9.9.1", "5.6.0.2"} }
	pick, tr, _, loc = w.run()
	if pick.PickedDue < 1 {
		t.Fatalf("day 302: %+v", pick)
	}
	city, located, _ = w.layer("5.6.3.0/24", scopeTraced)
	if city != "Munich" || !located.After(cLocatedBefore.Add(100*24*time.Hour)) || loc.Traced != 1 {
		t.Errorf("recheck that found Munich: %s %v %+v (before %v)", city, located, loc.Traced, cLocatedBefore)
	}
	w.wantState("5.6.3.0/24", netLocated, 0, 150)
	// Cleanup: D's last trace was at day 14 (288 days ago), it is not located and has no
	// next_trace_at: gone, with its 6 traces; C's visitor trace of day 0 expires too.
	if w.state("5.6.4.0/24") != nil || tr.NetworksExpired != 1 {
		t.Errorf("D after cleanup: %+v expired %d", w.state("5.6.4.0/24"), tr.NetworksExpired)
	}
	if n := w.count(`SELECT count(*) FROM public.network_traces WHERE prefix = '5.6.4.0/24'`); n != 0 || tr.TracesExpired != 7 {
		t.Errorf("traces of D left: %d, expired %d", n, tr.TracesExpired)
	}
	if w.state("5.6.3.0/24") == nil {
		t.Error("a located /24 was cleaned up")
	}
}

// A stale name that cannot be looked up again stays as it was.
func TestIntegrationRefreshKeepsNameOnError(t *testing.T) {
	w := newWorld(t)
	w.hops["5.6.3"] = func(string) []string { return []string{"9.9.9.1", "5.6.0.1"} }
	w.visitors("5.6.3", 1)
	w.run()
	w.age(40)
	// A row written before tried_at existed has none: it is a candidate at once.
	w.exec(`UPDATE public.ip_ptr_names SET tried_at = NULL`)
	w.ptrs["5.6.0.1"] = "!err"
	_, _, names, _ := w.run()
	if names.Refreshed < 1 || names.Errors != 1 {
		t.Fatalf("names = %+v", names)
	}
	var ptr, status string
	var ageDays int
	var triedAgo float64
	w.scan(`SELECT ptr, status, extract(day FROM now() - resolved_at)::int, extract(epoch FROM now() - tried_at) FROM public.ip_ptr_names WHERE ip = '5.6.0.1'`, &ptr, &status, &ageDays, &triedAgo)
	if ptr != "ae1.fra01.isp.net" || status != statusOK || ageDays < 39 {
		t.Errorf("name = %q (%s) resolved %d days ago: an error must not replace a name", ptr, status, ageDays)
	}
	if triedAgo > 3600 {
		t.Errorf("tried_at is %.0f s old: a failed lookup must still set it", triedAgo)
	}
	// It was just tried: the next run leaves it alone, though its answer is still old.
	_, _, names, _ = w.run()
	var triedAgain float64
	w.scan(`SELECT extract(epoch FROM now() - tried_at) FROM public.ip_ptr_names WHERE ip = '5.6.0.1'`, &triedAgain)
	if triedAgain < triedAgo {
		t.Errorf("the IP was looked up again within 7 days (tried %.0f s, then %.0f s ago): %+v", triedAgo, triedAgain, names)
	}
	// Seven days later it is a candidate again, and a good answer replaces the old one.
	w.age(8)
	w.ptrs["5.6.0.1"] = "ae9.fra09.isp.net"
	_, _, names, _ = w.run()
	w.scan(`SELECT ptr, status, extract(day FROM now() - resolved_at)::int FROM public.ip_ptr_names WHERE ip = '5.6.0.1'`, &ptr, &status, &ageDays)
	if names.Refreshed < 1 || ptr != "ae9.fra09.isp.net" || ageDays != 0 {
		t.Errorf("after 8 more days: %+v, name %q resolved %d days ago", names, ptr, ageDays)
	}
	if city, _, ok := w.layer("5.6.3.0/24", scopeTraced); !ok || city != "Frankfurt" {
		t.Errorf("layer = %s %v", city, ok)
	}
}

// Traces and /24s that were there before traced_networks existed get their rows once.
func TestIntegrationBackfill(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := schema.EnsureRDNS(ctx, w.conn); err != nil {
		t.Fatal(err)
	}
	trace := func(prefix, kind, target, hops string, daysAgo int) {
		w.exec(`INSERT INTO public.network_traces (prefix, target_kind, target, traced_at, hops, candidates)
			VALUES ($1::cidr, $2, $3::inet, now() - make_interval(days => $4::int), $5::jsonb, ARRAY[$3::inet])`, prefix, kind, target, daysAgo, hops)
	}
	answered := `[{"ttl":1,"ip":"10.0.0.1","rtt_ms":1},{"ttl":2,"ip":null,"rtt_ms":null}]`
	silent := `[{"ttl":1,"ip":null,"rtt_ms":null}]`
	// U: its newest traces answered (the older ones were silent); S: nothing ever answered;
	// L: located (a row in the layer); X: old traces, silent newest.
	trace("5.6.5.0/24", "visitor", "5.6.5.9", silent, 40)
	trace("5.6.5.0/24", "visitor", "5.6.5.9", answered, 20)
	trace("5.6.5.0/24", "gateway", "5.6.5.1", silent, 20)
	trace("5.6.6.0/24", "visitor", "5.6.6.9", silent, 20)
	trace("5.6.6.0/24", "gateway", "5.6.6.1", `[]`, 20)
	trace("5.6.7.0/24", "visitor", "5.6.7.9", answered, 30)
	w.exec(`INSERT INTO public.rdns_geo_layer (prefix, scope, country, city, router_ip, located_at) VALUES ('5.6.7.0/24', 'traced', 'DE', 'Frankfurt', '5.6.0.1', now() - interval '10 days')`)
	w.exec(`INSERT INTO public.rdns_geo_layer (prefix, scope, country, city, router_ip, located_at) VALUES ('5.6.8.0/24', 'traced', 'DE', 'Munich', '5.6.0.2', now() - interval '12 days')`) // no trace left
	w.exec(`INSERT INTO public.rdns_geo_layer (prefix, scope, country, city, located_at) VALUES ('5.6.0.0/16', 'wide', 'DE', 'Frankfurt', now())`)

	pick, err := w.p.PickNetworks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pick.Backfilled != 4 {
		t.Fatalf("backfilled %d rows", pick.Backfilled)
	}
	u := w.wantState("5.6.5.0/24", netUnmatched, 1, 7)
	if d := time.Since(u.Last); d < 19*24*time.Hour || d > 21*24*time.Hour {
		t.Errorf("last_traced_at of U is %v ago", d)
	}
	w.wantState("5.6.6.0/24", netSilent, 1, 7)
	l := w.wantState("5.6.7.0/24", netLocated, 0, 150)
	if l.Located == nil || time.Since(*l.Located) < 9*24*time.Hour || time.Since(*l.Located) > 11*24*time.Hour {
		t.Errorf("located_at of L = %v", l.Located)
	}
	if l.Last.After(time.Now().Add(-29 * 24 * time.Hour)) {
		t.Errorf("last_traced_at of L = %v (its newest trace is 30 days old)", l.Last)
	}
	w.wantState("5.6.8.0/24", netLocated, 0, 150)
	// U and S were traced more than 7 days ago: due at once; L is not; nothing is "new".
	if pick.PickedDue != 2 || pick.PickedNew != 0 || pick.Picked != 2 {
		t.Errorf("pick = %+v", pick)
	}
	// A second run does not backfill again.
	w.exec(`DELETE FROM public.traced_networks WHERE prefix = '5.6.6.0/24'`)
	if pick, err = w.p.PickNetworks(ctx); err != nil || pick.Backfilled != 0 || w.state("5.6.6.0/24") != nil {
		t.Errorf("second pick: %+v %v", pick, err)
	}
}

// The three real pick queries: new /24s before due ones before revived ones, each tier in its own
// order, a private /24 never, and a visitor IP only when the /24 has one.
func TestIntegrationPickOrderAndLimit(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := schema.EnsureRDNS(ctx, w.conn); err != nil {
		t.Fatal(err)
	}
	// Two due rows (the older first), one revived, one not due, one located and not due.
	w.exec(`INSERT INTO public.traced_networks (prefix, status, attempts, last_traced_at, next_trace_at, located_at) VALUES
		('5.7.1.0/24', 'silent', 1, now() - interval '9 days', now() - interval '2 days', NULL),
		('5.7.2.0/24', 'silent', 1, now() - interval '10 days', now() - interval '3 days', NULL),
		('5.7.3.0/24', 'unmatched', 4, now() - interval '100 days', NULL, NULL),
		('5.7.4.0/24', 'unmatched', 2, now() - interval '5 days', now() + interval '15 days', NULL),
		('5.7.5.0/24', 'located', 0, now() - interval '5 days', now() + interval '145 days', now()),
		('5.7.6.0/24', 'silent', 3, now() - interval '100 days', NULL, NULL)`)
	// Visitors: 5.7.3 came back after its trace (revived), 5.7.6 did not; 5.7.2 has one (used as target), 5.7.1 has none.
	w.exec(`INSERT INTO public.visitor_ips (ip, source, last_seen_at) VALUES
		('5.7.3.7', 't', now() - interval '1 day'), ('5.7.6.7', 't', now() - interval '200 days'), ('5.7.2.7', 't', now() - interval '90 days'),
		('9.9.0.1', 't', now() - interval '1 day'), ('9.9.0.2', 't', now() - interval '1 day'), ('9.9.1.1', 't', now() - interval '1 day'),
		('10.1.1.1', 't', now() - interval '1 day')`)
	pick, err := w.p.PickNetworks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pick.PickedNew != 2 || pick.PickedDue != 2 || pick.PickedRevived != 1 || pick.Picked != 5 || pick.Eligible != 5 {
		t.Fatalf("pick = %+v", pick)
	}
	data, _ := os.ReadFile(filepath.Join(w.p.dir(), batchFile))
	want := `{"prefix":"9.9.0.0/24","visitor":"9.9.0.1"}
{"prefix":"9.9.1.0/24","visitor":"9.9.1.1"}
{"prefix":"5.7.2.0/24","visitor":"5.7.2.7"}
{"prefix":"5.7.1.0/24"}
{"prefix":"5.7.3.0/24","visitor":"5.7.3.7"}
`
	// New ones: most visitors first, a private /24 never; due ones: the oldest next_trace_at first.
	if string(data) != want {
		t.Errorf("batch file:\n%s\nwant\n%s", data, want)
	}
}

func TestIntegrationLocateCandidateRuleWideVote(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := schema.EnsureRDNS(ctx, w.conn); err != nil {
		t.Fatal(err)
	}
	if err := schema.EnsureRDNSCityRules(ctx, w.conn); err != nil {
		t.Fatal(err)
	}
	w.exec(`INSERT INTO public.rdns_city_rules (domain, pattern, dictionary, places, version, status)
		VALUES ('transit.net', '^be2\.([a-z]+)[0-9]*\.transit\.net$', 'codes', '{"xyz":["NL","Amsterdam"]}', 'v1', 'candidate')`)
	w.exec(`INSERT INTO public.network_traces (prefix, target_kind, target, candidates, hops)
		VALUES ('5.6.8.0/24', 'visitor', '5.6.8.9', ARRAY['5.6.8.9'::inet, '9.9.0.2'::inet], '[]')`)
	w.exec(`INSERT INTO public.ip_ptr_names (ip, ptr, status) VALUES ('9.9.0.2', 'be2.xyz1.transit.net', 'ok')`)

	out, err := w.p.Locate(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var city, method string
	var candidates, mmdb int
	if err := w.conn.QueryRow(ctx, `SELECT city, method, candidate_votes, mmdb_votes FROM public.rdns_geo_layer WHERE prefix = '5.6.0.0/16' AND scope = 'wide'`).Scan(&city, &method, &candidates, &mmdb); err != nil {
		t.Fatal(err)
	}
	if city != "Amsterdam" || method != methodCandidate || candidates != 1 || mmdb != 0 || out.CandidateVotes != 1 {
		t.Fatalf("wide row city=%s method=%s candidate=%d mmdb=%d output=%+v", city, method, candidates, mmdb, out)
	}
	var traced int
	if err := w.conn.QueryRow(ctx, `SELECT count(*) FROM public.rdns_geo_layer WHERE prefix = '5.6.8.0/24' AND scope = 'traced'`).Scan(&traced); err != nil || traced != 0 {
		t.Fatalf("candidate created a traced row: count=%d err=%v", traced, err)
	}

	// An effective map/version change invalidates the saved fingerprint and changes
	// the candidate's wide result immediately.
	w.exec(`UPDATE public.rdns_city_rules SET places = '{"xyz":["BE","Brussels"]}', version = 'v2' WHERE domain = 'transit.net'`)
	out, err = w.p.Locate(ctx, false)
	if err != nil || out.Skipped {
		t.Fatalf("remap did not rebuild: out=%+v err=%v", out, err)
	}
	if err := w.conn.QueryRow(ctx, `SELECT city FROM public.rdns_geo_layer WHERE prefix = '5.6.0.0/16' AND scope = 'wide'`).Scan(&city); err != nil || city != "Brussels" {
		t.Fatalf("remapped city=%q err=%v", city, err)
	}

	// Rejection changes the fingerprint too. The wide row is volatile, so the withdrawal goes
	// through without force_replace and the drop is reported.
	w.exec(`UPDATE public.rdns_city_rules SET status = 'rejected' WHERE domain = 'transit.net'`)
	out, err = w.p.Locate(ctx, false)
	if err != nil || out.Skipped {
		t.Fatalf("rejection rebuild: out=%+v err=%v", out, err)
	}
	if out.PreviousVolatile != 1 || out.StoredVolatile != 0 {
		t.Fatalf("volatile counts: %+v", out)
	}
	if count := w.count(`SELECT count(*) FROM public.rdns_geo_layer WHERE scope = 'wide'`); count != 0 {
		t.Fatalf("rejection retained %d wide rows", count)
	}

	// Rollout without the optional table remains safe.
	w.exec(`DROP TABLE public.rdns_city_rules`)
	out, err = w.p.Locate(ctx, true)
	if err != nil || !out.CandidateRulesMissing {
		t.Fatalf("missing candidate table: out=%+v err=%v", out, err)
	}
}

// A candidate rule locates a /24 (traced row, method candidate_rule) without making it located:
// the schedule stays unmatched, the row is recomputed (not carried) and goes away with the
// rule, while resolve-names names every hop (hop_ips), not only the candidates.
func TestIntegrationCandidateTracedRowIsNotLocated(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := schema.EnsureRDNSCityRules(ctx, w.conn); err != nil {
		t.Fatal(err)
	}
	w.exec(`INSERT INTO public.rdns_city_rules (domain, rank, pattern, dictionary, places, version, status)
		VALUES ('isp.net', 2, '^pool-([a-z]+)\d*\.isp\.net$', 'codes', '{"ber":["DE","Berlin"]}', 'v2', 'candidate'),
		       ('isp.net', 1, '^be2\.([a-z]+)\d*\.isp\.net$', 'codes', '{"xyz":["NL","Amsterdam"]}', 'v1', 'candidate')`)
	// Five public hops: the last two are the candidates, the first three are only in hop_ips.
	w.hops["5.6.30"] = func(string) []string {
		return []string{"5.6.0.20", "5.6.0.21", "5.6.0.22", "5.6.0.23", "5.6.0.24"}
	}
	w.ptrs["5.6.0.20"] = "ae1.fra20.isp.net" // a hop outside the candidates, named all the same
	w.ptrs["5.6.0.24"] = "pool-ber1.isp.net" // rank 2 of the domain
	w.visitors("5.6.30", 1)

	_, _, names, loc := w.run()
	if names.IPs < 5 || names.OK != 2 {
		t.Fatalf("resolve-names did not look at every hop: %+v", names)
	}
	var stored int
	if err := w.conn.QueryRow(ctx, `SELECT cardinality(hop_ips) FROM public.network_traces WHERE prefix = '5.6.30.0/24' AND target_kind = 'visitor'`).Scan(&stored); err != nil || stored != 5 {
		t.Fatalf("hop_ips stored = %d, %v", stored, err)
	}
	if n := w.count(`SELECT count(*) FROM public.ip_ptr_names WHERE ip = '5.6.0.20' AND status = 'ok'`); n != 1 {
		t.Fatalf("a hop outside the candidates was not named")
	}
	var method, rule, city string
	if err := w.conn.QueryRow(ctx, `SELECT city, method, rule_name FROM public.rdns_geo_layer WHERE prefix = '5.6.30.0/24' AND scope = 'traced'`).Scan(&city, &method, &rule); err != nil {
		t.Fatalf("no candidate traced row: %v (%+v)", err, loc)
	}
	// 5.6.0.20 (fra, legacy rule) is not a candidate, so only the candidate rule at the tail applies.
	if city != "Berlin" || method != methodCandidate || rule != "isp.net#2" || loc.CandidateTraced != 1 {
		t.Fatalf("traced row city=%s method=%s rule=%s out=%+v", city, method, rule, loc)
	}
	w.wantState("5.6.30.0/24", netUnmatched, 1, 7)
	var located int
	if err := w.conn.QueryRow(ctx, `SELECT count(*) FROM public.traced_networks WHERE status = 'located'`).Scan(&located); err != nil || located != 0 {
		t.Fatalf("a candidate row made a /24 located: %d %v", located, err)
	}

	// A second rebuild neither carries nor duplicates the row.
	out, err := w.p.Locate(ctx, true)
	if err != nil || out.Carried != 0 || out.CandidateTraced != 1 || out.Stored != 2 { // traced + wide
		t.Fatalf("rebuild: %+v %v", out, err)
	}

	// Withdrawal: the row and the wide vote vanish at the next rebuild without force_replace
	// (both are volatile; the drop is reported), and the schedule is untouched.
	w.exec(`UPDATE public.rdns_city_rules SET status = 'rejected'`)
	out, err = w.p.Locate(ctx, false)
	if err != nil || out.Stored != 0 || out.Carried != 0 || out.PreviousVolatile != 2 || out.StoredVolatile != 0 {
		t.Fatalf("withdrawal: %+v %v", out, err)
	}
	if n := w.count(`SELECT count(*) FROM public.rdns_geo_layer`); n != 0 {
		t.Fatalf("%d rows left after the withdrawal", n)
	}
	w.wantState("5.6.30.0/24", netUnmatched, 1, 7)
	// And none of it feeds the located-only readers.
	if ips, err := staleLocatedIPs(ctx, w.conn, 10); err != nil || len(ips) != 0 {
		t.Fatalf("stale located IPs: %v %v", ips, err)
	}
}

// Names of a candidate_rule row's /24 are not refreshed or retried as those of a located /24.
func TestIntegrationCandidateRowsAreNotLocatedForRefresh(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := schema.EnsureRDNS(ctx, w.conn); err != nil {
		t.Fatal(err)
	}
	w.exec(`INSERT INTO public.ip_ptr_names (ip, ptr, status, resolved_at) VALUES ('5.6.0.5', 'x.isp.net', 'ok', now() - interval '40 days')`)
	w.exec(`INSERT INTO public.rdns_geo_layer (prefix, scope, country, city, router_ip, router_ptr, rule_name, method)
		VALUES ('5.6.31.0/24', 'traced', 'DE', 'Berlin', '5.6.0.5', 'x.isp.net', 'isp.net#1', 'candidate_rule')`)
	if ips, err := staleLocatedIPs(ctx, w.conn, 10); err != nil || len(ips) != 0 {
		t.Fatalf("candidate row's router refreshed: %v %v", ips, err)
	}
	w.exec(`UPDATE public.rdns_geo_layer SET method = 'rdns_rule'`)
	if ips, err := staleLocatedIPs(ctx, w.conn, 10); err != nil || len(ips) != 1 {
		t.Fatalf("legacy row's router not refreshed: %v %v", ips, err)
	}
}

// The guard still protects the sticky rows (legacy-located traced rows), and only them.
func TestIntegrationShrinkGuardCountsStickyRowsOnly(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := schema.EnsureRDNS(ctx, w.conn); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		w.exec(`INSERT INTO public.rdns_geo_layer (prefix, scope, country, city, method) VALUES ($1::cidr, 'traced', 'DE', 'Frankfurt', 'rdns_rule')`, fmt.Sprintf("5.6.%d.0/24", 100+i))
	}
	for i := 0; i < 20; i++ {
		w.exec(`INSERT INTO public.rdns_geo_layer (prefix, scope, country, city, method) VALUES ($1::cidr, 'wide', 'DE', 'Berlin', 'candidate_rule')`, fmt.Sprintf("7.%d.0.0/16", i))
	}
	var sticky []geoRow
	for i := 0; i < 10; i++ {
		sticky = append(sticky, geoRow{Prefix: netip.MustParsePrefix(fmt.Sprintf("5.6.%d.0/24", 100+i)), Scope: scopeTraced, Country: "DE", City: "Frankfurt", Method: methodRule})
	}
	// Only volatile rows survive: the sticky ones fell from 10 to 0, so the guard refuses.
	volatile := []geoRow{{Prefix: netip.MustParsePrefix("7.0.0.0/16"), Scope: scopeWide, Country: "DE", City: "Berlin", Method: methodCandidate}}
	if _, err := replaceLayer(ctx, w.conn, volatile, false); !errors.Is(err, ErrShrink) {
		t.Fatalf("sticky drop not refused: %v", err)
	}
	// All sticky rows kept, every volatile row gone: allowed, and the swing is reported.
	rs, err := replaceLayer(ctx, w.conn, sticky, false)
	if err != nil || rs.Previous != 30 || rs.PreviousVolatile != 20 || rs.StoredVolatile != 0 {
		t.Fatalf("replace: %+v %v", rs, err)
	}
}
