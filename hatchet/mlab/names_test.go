package mlab

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/rdns"
	"openipdata.org/hatchet/schema"
)

type fakeStore struct {
	fresh   map[uint32]struct{}
	old     []uint32
	saved   []string // IPs, in the order saved
	answers map[uint32]rdns.NameAnswer
	deleted []uint32
	saveErr error
}

func (f *fakeStore) Fresh(context.Context) (map[uint32]struct{}, error) { return f.fresh, nil }
func (f *fakeStore) Old(context.Context) ([]uint32, error)              { return f.old, nil }
func (f *fakeStore) Delete(_ context.Context, ips []uint32) (int, error) {
	f.deleted = append(f.deleted, ips...)
	return len(ips), nil
}
func (f *fakeStore) Save(_ context.Context, ips []netip.Addr, answers []rdns.NameAnswer) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.answers == nil {
		f.answers = map[uint32]rdns.NameAnswer{}
	}
	for i, a := range ips {
		f.saved = append(f.saved, a.String())
		f.answers[u32Of(a)] = answers[i]
	}
	return nil
}

// resolver answers by the last octet: even = a name, 1 = not found, 3 = error, 5 = DoH name.
func fakeResolver(asked *[]string) resolveFunc {
	var mu sync.Mutex
	return func(_ context.Context, ips []netip.Addr, _ int) ([]rdns.NameAnswer, int) {
		mu.Lock()
		defer mu.Unlock()
		out := make([]rdns.NameAnswer, len(ips))
		doh := 0
		for i, ip := range ips {
			*asked = append(*asked, ip.String())
			switch ip.As4()[3] {
			case 1:
				out[i] = rdns.NameAnswer{Status: rdns.StatusNotFound}
			case 3:
				out[i] = rdns.NameAnswer{Status: rdns.StatusError, ErrKind: "timeout"}
			case 5:
				out[i], doh = rdns.NameAnswer{PTR: "r." + ip.String() + ".example.net", Status: rdns.StatusOK}, doh+1
			default:
				out[i] = rdns.NameAnswer{PTR: "r.example.net", Status: rdns.StatusOK}
			}
		}
		return out, doh
	}
}

func ipv(s string) uint32 { return u32Of(netip.MustParseAddr(s)) }

func TestResolveNamesSkipsFreshRetriesErrorsAndPrunes(t *testing.T) {
	hops := []uint32{ipv("1.1.1.1"), ipv("1.1.1.2"), ipv("1.1.1.3"), ipv("1.1.1.5"), ipv("1.1.1.6")}
	st := &fakeStore{
		fresh: map[uint32]struct{}{ipv("1.1.1.1"): {}, ipv("1.1.1.2"): {}}, // answered recently (an error row is not in it)
		old:   []uint32{ipv("1.1.1.2"), ipv("9.9.9.9"), ipv("9.9.9.8")},    // 1.1.1.2 is still a hop: kept
	}
	var asked []string
	out, err := resolveNames(context.Background(), st, fakeResolver(&asked), hops, time.Hour, 2, nolog)
	if err != nil {
		t.Fatal(err)
	}
	want := NamesOutput{Hops: 5, Fresh: 2, Queried: 3, OK: 2, NotFound: 0, Errors: 1, DoH: 1, Left: 0, Pruned: 2}
	if out != want {
		t.Errorf("output %+v, want %+v", out, want)
	}
	if len(asked) != 3 || asked[0] != "1.1.1.3" || asked[1] != "1.1.1.5" || asked[2] != "1.1.1.6" {
		t.Errorf("asked %v", asked)
	}
	sort.Slice(st.deleted, func(i, j int) bool { return st.deleted[i] < st.deleted[j] })
	if len(st.deleted) != 2 || st.deleted[0] != ipv("9.9.9.8") {
		t.Errorf("deleted %v", st.deleted)
	}
	// The error answer is stored as an error (it is asked again next run: it was not "fresh").
	if st.answers[ipv("1.1.1.3")].Status != rdns.StatusError {
		t.Errorf("answers %+v", st.answers)
	}
}

// Answers are stored batch by batch, so a stop (or a crash) keeps what is done.
func TestResolveNamesStopsOnBudgetAndKeepsBatches(t *testing.T) {
	var hops []uint32
	for i := 0; i < 10; i++ {
		hops = append(hops, ipv("2.2.2.0")+uint32(i)*8) // last octets 0,8,..: all plain names
	}
	st := &fakeStore{}
	calls := 0
	resolve := func(ctx context.Context, ips []netip.Addr, _ int) ([]rdns.NameAnswer, int) {
		calls++
		out := make([]rdns.NameAnswer, len(ips))
		for i := range out {
			out[i] = rdns.NameAnswer{PTR: "r.example.net", Status: rdns.StatusOK}
		}
		if calls == 3 { // the budget runs out during the third batch
			<-ctx.Done()
		}
		return out, 0
	}
	out, err := resolveNames(context.Background(), st, resolve, hops, 50*time.Millisecond, 3, nolog)
	if err != nil {
		t.Fatal(err)
	}
	if out.Queried != 6 || out.OK != 6 || out.Left != 4 || out.Stopped != "time" || len(st.saved) != 6 {
		t.Errorf("output %+v, saved %d", out, len(st.saved))
	}
	// The next run continues where this stopped.
	st.fresh = map[uint32]struct{}{}
	for _, ip := range st.saved {
		st.fresh[u32Of(netip.MustParseAddr(ip))] = struct{}{}
	}
	var asked []string
	out, err = resolveNames(context.Background(), st, fakeResolver(&asked), hops, time.Hour, 3, nolog)
	if err != nil || out.Fresh != 6 || out.Queried != 4 || out.Left != 0 || out.Stopped != "" {
		t.Errorf("second run %+v %v", out, err)
	}
}

func TestResolveNamesCancelledAndDBErrors(t *testing.T) {
	hops := []uint32{ipv("1.1.1.2")}
	ctx, cancel := context.WithCancel(context.Background())
	resolve := func(ctx context.Context, ips []netip.Addr, _ int) ([]rdns.NameAnswer, int) {
		cancel()
		return make([]rdns.NameAnswer, len(ips)), 0
	}
	st := &fakeStore{}
	if _, err := resolveNames(ctx, st, resolve, hops, time.Hour, 1, nolog); !errors.Is(err, context.Canceled) || len(st.saved) != 0 {
		t.Errorf("cancelled: %v saved %v", err, st.saved)
	}
	var asked []string
	st = &fakeStore{saveErr: errors.New("db down")}
	if _, err := resolveNames(context.Background(), st, fakeResolver(&asked), hops, time.Hour, 1, nolog); err == nil {
		t.Error("a database error did not fail the task")
	}
}

func TestResolveNamesNeedsPairs(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir(), PGSQL: "postgres://x"}
	if _, err := p.ResolveNames(context.Background()); !errors.Is(err, ErrPermanent) {
		t.Errorf("no pairs: %v", err)
	}
	if _, err := (&Pipeline{DataDir: t.TempDir()}).ResolveNames(context.Background()); err == nil {
		t.Error("no PGSQL accepted")
	}
}

func testConn(t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	base := os.Getenv("OPENIPDATA_TEST_PGSQL")
	if base == "" {
		t.Skip("set OPENIPDATA_TEST_PGSQL to run disposable Postgres integration tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	name := fmt.Sprintf("mlab_it_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()
	t.Cleanup(func() {
		admin.Exec(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`)
		admin.Close(context.Background())
	})
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn, dsn
}

// The Postgres store: fresh = ok or not_found within 60 days; an error never replaces an older ok
// or not_found row; old rows are the ones past 200 days.
func TestIntegrationNameStore(t *testing.T) {
	conn, _ := testConn(t)
	ctx := context.Background()
	if err := schema.EnsureMLab(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := schema.EnsureMLab(ctx, conn); err != nil {
		t.Fatal(err) // idempotent
	}
	st := &pgNames{conn: conn}
	ips := []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("1.1.1.2"), netip.MustParseAddr("1.1.1.3"), netip.MustParseAddr("1.1.1.4")}
	answers := []rdns.NameAnswer{
		{PTR: "a.example.net", Status: rdns.StatusOK}, {Status: rdns.StatusNotFound}, {Status: rdns.StatusError}, {PTR: "ignored", Status: "weird"},
	}
	if err := st.Save(ctx, ips, answers); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.Fresh(ctx)
	if err != nil || len(fresh) != 2 {
		t.Fatalf("fresh %v %v", fresh, err)
	}
	if _, ok := fresh[ipv("1.1.1.3")]; ok {
		t.Error("an error row is fresh")
	}
	var ptr, status string
	if err := conn.QueryRow(ctx, `SELECT ptr, status FROM public.mlab_hop_names WHERE ip = '1.1.1.4'`).Scan(&ptr, &status); err != nil || ptr != "" || status != "error" {
		t.Errorf("unknown status stored as %q %q %v", ptr, status, err)
	}
	// 60 days later the ok row is stale; an error then does not replace it, a new name does.
	conn.Exec(ctx, `UPDATE public.mlab_hop_names SET resolved_at = now() - interval '61 days' WHERE ip IN ('1.1.1.1','1.1.1.2')`)
	if fresh, _ = st.Fresh(ctx); len(fresh) != 0 {
		t.Errorf("stale rows are fresh: %v", fresh)
	}
	st.Save(ctx, ips[:1], []rdns.NameAnswer{{Status: rdns.StatusError}})
	if err := conn.QueryRow(ctx, `SELECT ptr, status FROM public.mlab_hop_names WHERE ip = '1.1.1.1'`).Scan(&ptr, &status); err != nil || ptr != "a.example.net" || status != "ok" {
		t.Errorf("an error replaced an ok row: %q %q", ptr, status)
	}
	st.Save(ctx, ips[:1], []rdns.NameAnswer{{PTR: "b.example.net", Status: rdns.StatusOK}})
	conn.QueryRow(ctx, `SELECT ptr FROM public.mlab_hop_names WHERE ip = '1.1.1.1'`).Scan(&ptr)
	if ptr != "b.example.net" {
		t.Errorf("a new name did not replace: %q", ptr)
	}
	// An error row is replaced by an error row (tried_at moves) and by a name.
	st.Save(ctx, ips[2:3], []rdns.NameAnswer{{PTR: "c.example.net", Status: rdns.StatusOK}})
	conn.QueryRow(ctx, `SELECT ptr FROM public.mlab_hop_names WHERE ip = '1.1.1.3'`).Scan(&ptr)
	if ptr != "c.example.net" {
		t.Errorf("an error row was not replaced: %q", ptr)
	}

	conn.Exec(ctx, `UPDATE public.mlab_hop_names SET resolved_at = now() - interval '201 days' WHERE ip IN ('1.1.1.2','1.1.1.4')`)
	old, err := st.Old(ctx)
	if err != nil || len(old) != 2 {
		t.Fatalf("old %v %v", old, err)
	}
	n, err := st.Delete(ctx, []uint32{ipv("1.1.1.2"), ipv("1.1.1.3")}) // 1.1.1.3 is not old: stays
	if err != nil || n != 1 {
		t.Errorf("deleted %d %v", n, err)
	}
	var count int
	conn.QueryRow(ctx, `SELECT count(*) FROM public.mlab_hop_names`).Scan(&count)
	if count != 3 {
		t.Errorf("rows left %d", count)
	}
}

// The task end to end against Postgres: it creates the table, stores the names (the second run
// asks only for what is not fresh) and counts only.
func TestIntegrationResolveNamesTask(t *testing.T) {
	conn, dsn := testConn(t)
	ctx := context.Background()
	p := &Pipeline{DataDir: t.TempDir(), PGSQL: dsn}
	if _, err := writePairs(p.pairsPath(), nil); err == nil {
		t.Fatal("wrote into a missing directory")
	}
	os.MkdirAll(p.dir(), 0o750)
	pairs := []pair{makePair(0x080808, 0, ipv("1.1.1.2"), 5), makePair(0x080808, 1, ipv("1.1.1.1"), 2), makePair(0x080809, 0, ipv("1.1.1.2"), 1)}
	sortPairs(pairs)
	if _, err := writePairs(p.pairsPath(), pairs); err != nil {
		t.Fatal(err)
	}
	var asked []string
	p.resolve = fakeResolver(&asked)
	out, err := p.ResolveNames(ctx)
	if err != nil || out.Hops != 2 || out.Queried != 2 || out.OK != 1 || out.NotFound != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	var n int
	conn.QueryRow(ctx, `SELECT count(*) FROM public.mlab_hop_names`).Scan(&n)
	if n != 2 {
		t.Errorf("rows %d", n)
	}
	asked = nil
	if out, err = p.ResolveNames(ctx); err != nil || out.Fresh != 2 || out.Queried != 0 || len(asked) != 0 {
		t.Errorf("second run %+v %v %v", out, err, asked)
	}
}

func sortPairs(p []pair) {
	sort.Slice(p, func(i, j int) bool { return cmpPair(p[i], p[j]) < 0 })
}

func nolog(string, ...any) {}

// The first progress line says what there is to do, in counts only.
func TestResolveNamesProgressLine(t *testing.T) {
	hops := []uint32{ipv("1.1.1.1"), ipv("1.1.1.3"), ipv("1.1.1.5")}
	st := &fakeStore{fresh: map[uint32]struct{}{ipv("1.1.1.1"): {}}}
	var asked, lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if _, err := resolveNames(context.Background(), st, fakeResolver(&asked), hops, time.Hour, 2, logf); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "resolve-names: 3 hops, 1 fresh, 2 to look up" {
		t.Errorf("lines %q", lines)
	}
}
