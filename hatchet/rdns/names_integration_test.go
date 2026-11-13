package rdns

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/schema"
)

func namesTestDB(t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	dsn := newTestDB(t)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if err := schema.EnsureRDNS(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	return conn, dsn
}

func TestIntegrationNameErrorBackoff(t *testing.T) {
	conn, _ := namesTestDB(t)
	ctx := context.Background()
	ip := netip.MustParseAddr("5.6.7.9")
	ips := []netip.Addr{ip}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	wantWait := func(want bool) {
		t.Helper()
		waiting, err := freshNames(ctx, conn, ips)
		if err != nil {
			t.Fatal(err)
		}
		backoff, found := waiting[ip]
		if found != want || found && !backoff {
			t.Fatalf("waiting = %v (backoff %v), want %v", found, backoff, want)
		}
	}
	for i, days := range []int{1, 2, 7, 7} {
		if err := storeNames(ctx, conn, ips, []ptrAnswer{{Status: statusError}}); err != nil {
			t.Fatal(err)
		}
		var errors int
		if err := conn.QueryRow(ctx, `SELECT error_count FROM public.ip_ptr_names`).Scan(&errors); err != nil || errors != i+1 {
			t.Fatalf("failure count = %d, err %v", errors, err)
		}
		wantWait(true)
		exec(`UPDATE public.ip_ptr_names SET tried_at = now() - make_interval(days => $1::int) + interval '1 hour'`, days)
		wantWait(true)
		exec(`UPDATE public.ip_ptr_names SET tried_at = now() - make_interval(days => $1::int)`, days)
		wantWait(false)
	}
	// A real response resets the consecutive-failure counter. A later failed lookup
	// preserves that response and starts at the one-day delay again.
	for _, status := range []string{statusOK, statusNotFound} {
		answer := ptrAnswer{Status: status}
		if status == statusOK {
			answer.PTR = "router.example.net"
		}
		if err := storeNames(ctx, conn, ips, []ptrAnswer{answer}); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := conn.QueryRow(ctx, `SELECT error_count FROM public.ip_ptr_names`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("reset = %d, err %v", count, err)
		}
		exec(`UPDATE public.ip_ptr_names SET resolved_at = now() - interval '40 days'`)
		var before time.Time
		if err := conn.QueryRow(ctx, `SELECT resolved_at FROM public.ip_ptr_names`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := storeNames(ctx, conn, ips, []ptrAnswer{{Status: statusError}}); err != nil {
			t.Fatal(err)
		}
		var gotStatus, ptr string
		var after time.Time
		if err := conn.QueryRow(ctx, `SELECT status, coalesce(ptr, ''), resolved_at, error_count FROM public.ip_ptr_names`).Scan(&gotStatus, &ptr, &after, &count); err != nil {
			t.Fatal(err)
		}
		if gotStatus != status || ptr != answer.PTR || !before.Equal(after) || count != 1 {
			t.Fatalf("good answer changed: status %s, ptr %q, timestamps equal %v, failures %d", gotStatus, ptr, before.Equal(after), count)
		}
		wantWait(true)
		exec(`UPDATE public.ip_ptr_names SET tried_at = now() - interval '1 day'`)
		wantWait(false)
	}
	// Existing rows predating error_count are eligible once, then get normal backoff.
	exec(`UPDATE public.ip_ptr_names SET status = 'error', ptr = NULL, error_count = NULL, tried_at = NULL`)
	wantWait(false)
	if err := storeNames(ctx, conn, ips, []ptrAnswer{{Status: statusError}}); err != nil {
		t.Fatal(err)
	}
	wantWait(true)
}

func TestIntegrationLegacySuccessfulNamesWithNullErrorCount(t *testing.T) {
	conn, _ := namesTestDB(t)
	ctx := context.Background()
	ips := []netip.Addr{
		netip.MustParseAddr("5.6.7.9"),
		netip.MustParseAddr("5.6.7.10"),
	}
	if _, err := conn.Exec(ctx, `INSERT INTO public.ip_ptr_names (ip, ptr, status, resolved_at, tried_at, error_count)
		VALUES ($1::inet, 'router.example.net', 'ok', now() - interval '1 day', NULL, NULL),
		       ($2::inet, NULL, 'not_found', now() - interval '1 day', NULL, NULL)`, ips[0].String(), ips[1].String()); err != nil {
		t.Fatal(err)
	}
	waiting, err := freshNames(ctx, conn, ips)
	if err != nil {
		t.Fatalf("legacy success rows must scan without a NULL bool: %v", err)
	}
	for _, ip := range ips {
		backedOff, found := waiting[ip]
		if !found || backedOff {
			t.Fatalf("legacy %s row waiting = (%v, %v), want fresh success", ip, found, backedOff)
		}
	}
}

func TestIntegrationRefreshErrorBackoff(t *testing.T) {
	conn, _ := namesTestDB(t)
	ctx := context.Background()
	ip := netip.MustParseAddr("5.6.7.9")
	if _, err := conn.Exec(ctx, `INSERT INTO public.rdns_geo_layer
		(prefix, scope, country, city, router_ip) VALUES ('5.6.7.0/24', 'traced', 'DE', 'Frankfurt', $1::inet)`, ip.String()); err != nil {
		t.Fatal(err)
	}
	if err := storeNames(ctx, conn, []netip.Addr{ip}, []ptrAnswer{{PTR: "old.example.net", Status: statusOK}}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE public.ip_ptr_names SET resolved_at = now() - interval '40 days'`); err != nil {
		t.Fatal(err)
	}
	for _, days := range []int{1, 2, 7, 7} {
		if err := storeNames(ctx, conn, []netip.Addr{ip}, []ptrAnswer{{Status: statusError}}); err != nil {
			t.Fatal(err)
		}
		var status, ptr string
		if err := conn.QueryRow(ctx, `SELECT status, ptr FROM public.ip_ptr_names`).Scan(&status, &ptr); err != nil || status != statusOK || ptr != "old.example.net" {
			t.Fatalf("failed refresh replaced stored answer: status %q, ptr %q, err %v", status, ptr, err)
		}
		if ips, err := staleLocatedIPs(ctx, conn, 500); err != nil || len(ips) != 0 {
			t.Fatalf("fresh failure retried: %d, %v", len(ips), err)
		}
		if _, err := conn.Exec(ctx, `UPDATE public.ip_ptr_names SET tried_at = now() - make_interval(days => $1::int)`, days); err != nil {
			t.Fatal(err)
		}
		if ips, err := staleLocatedIPs(ctx, conn, 500); err != nil || len(ips) != 1 || ips[0] != ip {
			t.Fatalf("due failure missing: %v, %v", ips, err)
		}
	}
}

func TestIntegrationRetryAfterTraceWindow(t *testing.T) {
	conn, dsn := namesTestDB(t)
	ctx := context.Background()
	ip := netip.MustParseAddr("5.6.7.9")
	retired := netip.MustParseAddr("5.6.7.10")
	if _, err := conn.Exec(ctx, `INSERT INTO public.network_traces
		(prefix, target_kind, target, traced_at, hops, candidates) VALUES
		('5.6.7.0/24', 'gateway', '5.6.7.1', now(), '[]', ARRAY['5.6.7.9']::inet[]),
		('5.6.8.0/24', 'gateway', '5.6.8.1', now() - interval '181 days', '[]', ARRAY['5.6.7.10']::inet[])`); err != nil {
		t.Fatal(err)
	}
	if err := storeNames(ctx, conn, []netip.Addr{retired}, []ptrAnswer{{Status: statusError}}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE public.ip_ptr_names SET error_count = 3, tried_at = now() - interval '8 days' WHERE ip = $1::inet`, retired.String()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p := &Pipeline{PGSQL: dsn, resolver: &ptrResolver{
		system: func(context.Context, string) ([]string, error) {
			calls++
			return nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}
		},
		doh: func(context.Context, netip.Addr) ptrAnswer {
			return ptrAnswer{Status: statusError, ErrKind: errKindServfail}
		},
	}}
	run := func(wantCalls int) NamesOutput {
		t.Helper()
		out, err := p.ResolveNames(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if calls != wantCalls {
			t.Fatalf("resolver calls = %d, want %d", calls, wantCalls)
		}
		return out
	}
	if out := run(1); out.Traces != 1 || out.Retried != 0 || out.Resolved != 1 {
		t.Fatalf("day 0: %+v", out)
	}
	if _, err := conn.Exec(ctx, `UPDATE public.ip_ptr_names SET tried_at = now() - interval '1 day' - interval '1 second' WHERE ip = $1::inet`, ip.String()); err != nil {
		t.Fatal(err)
	}
	if out := run(2); out.Traces != 1 || out.Retried != 0 || out.Resolved != 1 {
		t.Fatalf("day 1: %+v", out)
	}
	if _, err := conn.Exec(ctx, `UPDATE public.ip_ptr_names SET tried_at = now() - interval '2 days' - interval '1 second' WHERE ip = $1::inet`, ip.String()); err != nil {
		t.Fatal(err)
	}
	if out := run(3); out.Traces != 1 || out.Retried != 0 || out.Resolved != 1 {
		t.Fatalf("day 3: %+v", out)
	}
	if _, err := conn.Exec(ctx, `UPDATE public.network_traces SET traced_at = now() - interval '8 days' WHERE prefix = '5.6.7.0/24'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE public.ip_ptr_names SET tried_at = now() - interval '7 days' - interval '1 second' WHERE ip = $1::inet`, ip.String()); err != nil {
		t.Fatal(err)
	}
	if out := run(4); out.Traces != 0 || out.Retried != 1 || out.Resolved != 1 || out.Errors != 1 {
		t.Fatalf("day 10: %+v", out)
	}
	var retiredCount int
	if err := conn.QueryRow(ctx, `SELECT error_count FROM public.ip_ptr_names WHERE ip = $1::inet`, retired.String()).Scan(&retiredCount); err != nil || retiredCount != 3 {
		t.Fatalf("retired failure was retried: count %d, err %v", retiredCount, err)
	}
}

func TestIntegrationLegacyErrorRetriesAfterTraceWindow(t *testing.T) {
	conn, dsn := namesTestDB(t)
	ctx := context.Background()
	ip := netip.MustParseAddr("5.6.7.9")
	if _, err := conn.Exec(ctx, `INSERT INTO public.network_traces
		(prefix, target_kind, target, traced_at, hops, candidates) VALUES
		('5.6.7.0/24', 'gateway', '5.6.7.1', now() - interval '8 days', '[]', ARRAY['5.6.7.9']::inet[])`); err != nil {
		t.Fatal(err)
	}
	// Rows written before error_count/tried_at existed have a NULL count and may
	// have no tried_at. Their resolved_at is the last failure time and starts the
	// one-day backoff without requiring a migration UPDATE.
	if _, err := conn.Exec(ctx, `INSERT INTO public.ip_ptr_names (ip, status, resolved_at, tried_at, error_count)
		VALUES ($1::inet, 'error', now() - interval '2 days', NULL, NULL)`, ip.String()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p := &Pipeline{PGSQL: dsn, resolver: &ptrResolver{
		system: func(context.Context, string) ([]string, error) {
			calls++
			return nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}
		},
		doh: func(context.Context, netip.Addr) ptrAnswer {
			return ptrAnswer{Status: statusError, ErrKind: errKindServfail}
		},
	}}
	out, err := p.ResolveNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Traces != 0 || out.Retried != 1 || out.Resolved != 1 || out.Errors != 1 || calls != 1 {
		t.Fatalf("legacy retry: output %+v, resolver calls %d", out, calls)
	}
	var count int
	if err := conn.QueryRow(ctx, `SELECT error_count FROM public.ip_ptr_names WHERE ip = $1::inet`, ip.String()).Scan(&count); err != nil || count != 2 {
		t.Fatalf("legacy failure count = %d, err %v; want second consecutive failure", count, err)
	}
}

func TestIntegrationNameErrorCountsAndSkip(t *testing.T) {
	conn, dsn := namesTestDB(t)
	ctx := context.Background()
	_, err := conn.Exec(ctx, `INSERT INTO public.network_traces
		(prefix, target_kind, target, traced_at, hops, candidates) VALUES
		('5.6.7.0/24', 'gateway', '5.6.7.1', now(), '[]', ARRAY['5.6.7.1','5.6.0.1','5.6.0.2','5.6.0.3']::inet[])`)
	if err != nil {
		t.Fatal(err)
	}
	p := &Pipeline{PGSQL: dsn, resolver: &ptrResolver{
		system: func(_ context.Context, ip string) ([]string, error) {
			if ip == "5.6.7.1" {
				return []string{"target.example.net"}, nil
			}
			return nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}
		},
		doh: func(_ context.Context, ip netip.Addr) ptrAnswer {
			return ptrAnswer{Status: statusError, ErrKind: map[byte]string{1: errKindTimeout, 2: errKindServfail, 3: errKindOther}[ip.As4()[3]]}
		},
	}}
	out, err := p.ResolveNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.OK != 1 || out.Errors != 3 || out.ErrorsTimeout != 1 || out.ErrorsServfail != 1 || out.ErrorsOther != 1 || out.DoH != 3 {
		t.Fatalf("counts: %+v", out)
	}
	out, err = p.ResolveNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Resolved != 0 || out.BackedOff != 3 || out.Fresh != 1 || out.DoH != 0 {
		t.Fatalf("second run should skip recent answers/failures: %+v", out)
	}
}

// Traces written before hop_ips existed get it once, from their path, and resolve-names
// then names every hop of them.
func TestIntegrationHopIPsBackfillAndNaming(t *testing.T) {
	conn, dsn := namesTestDB(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `INSERT INTO public.network_traces (prefix, target_kind, target, traced_at, hops, candidates) VALUES
		('5.6.7.0/24', 'visitor', '5.6.7.9', now(),
		 '[{"ttl":1,"ip":"10.0.0.1"},{"ttl":2,"ip":"9.9.9.1"},{"ttl":3,"ip":null},{"ttl":4,"ip":"9.9.9.2"},{"ttl":5,"ip":"9.9.9.3"},{"ttl":6,"ip":"9.9.9.4"},{"ttl":7,"ip":"9.9.9.5"}]',
		 ARRAY['5.6.7.9','9.9.9.5','9.9.9.4']::inet[])`); err != nil {
		t.Fatal(err)
	}
	asked := map[string]bool{}
	var mu sync.Mutex
	p := &Pipeline{PGSQL: dsn, resolver: &ptrResolver{
		system: func(_ context.Context, ip string) ([]string, error) {
			mu.Lock()
			asked[ip] = true
			mu.Unlock()
			return []string{"r." + ip + ".example."}, nil
		},
		doh: func(context.Context, netip.Addr) ptrAnswer { return ptrAnswer{Status: statusError} },
	}}
	out, err := p.ResolveNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.BackfilledHops != 1 || out.IPs != 6 || out.OK != 6 || len(asked) != 6 {
		t.Fatalf("output %+v, asked %v", out, asked)
	}
	for _, ip := range []string{"9.9.9.1", "9.9.9.2", "9.9.9.3", "9.9.9.4", "9.9.9.5", "5.6.7.9"} {
		if !asked[ip] {
			t.Errorf("%s was not looked up", ip)
		}
	}
	var hops []string
	if err := conn.QueryRow(ctx, `SELECT ARRAY(SELECT host(h) FROM unnest(hop_ips) WITH ORDINALITY u(h, n) ORDER BY n) FROM public.network_traces`).Scan(&hops); err != nil || len(hops) != 5 || hops[0] != "9.9.9.1" {
		t.Fatalf("hop_ips = %v, %v", hops, err)
	}
	// Nothing left to backfill, and the next run finds every name fresh.
	if n, err := backfillHopIPs(ctx, conn); err != nil || n != 0 {
		t.Fatalf("second backfill: %d %v", n, err)
	}
	if out, err = p.ResolveNames(ctx); err != nil || out.Resolved != 0 || out.Fresh != 6 || out.BackfilledHops != 0 {
		t.Fatalf("second run: %+v %v", out, err)
	}
}
