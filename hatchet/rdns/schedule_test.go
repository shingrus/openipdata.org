package rdns

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func daysAfter(base time.Time, n int) *time.Time {
	t := base.Add(time.Duration(n) * day)
	return &t
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func TestNextTraceAt(t *testing.T) {
	for _, c := range []struct {
		status   string
		attempts int
		days     int // 0: nil
	}{
		{netSilent, 1, 7}, {netSilent, 2, 7}, {netSilent, 3, 0}, {netSilent, 4, 0}, {netSilent, 0, 0},
		{netUnmatched, 1, 7}, {netUnmatched, 2, 20}, {netUnmatched, 3, 60}, {netUnmatched, 4, 0}, {netUnmatched, 0, 0},
		{netLocated, 0, 150}, {netLocated, 1, 150}, {netLocated, 9, 150},
		{"other", 1, 0}, {"", 1, 0},
	} {
		var want *time.Time
		if c.days > 0 {
			want = daysAfter(t0, c.days)
		}
		if got := nextTraceAt(c.status, c.attempts, t0); !sameTime(got, want) {
			t.Errorf("nextTraceAt(%s, %d) = %v, want %v", c.status, c.attempts, got, want)
		}
	}
}

// traceAgain runs afterTrace and feeds the row back, as the next trace's prev.
func traceAgain(prev *tracedNet, answered bool, at time.Time) *tracedNet {
	n := afterTrace(prev, "5.6.7.0/24", answered, at)
	return &n
}

func TestAfterTraceSilent(t *testing.T) {
	n := traceAgain(nil, false, t0)
	if n.Status != netSilent || n.Attempts != 1 || !sameTime(n.NextTraceAt, daysAfter(t0, 7)) || !n.LastTracedAt.Equal(t0) || n.LocatedAt != nil {
		t.Fatalf("first: %+v", n)
	}
	at := t0.Add(7 * day)
	n = traceAgain(n, false, at)
	if n.Status != netSilent || n.Attempts != 2 || !sameTime(n.NextTraceAt, daysAfter(at, 7)) {
		t.Fatalf("second: %+v", n)
	}
	at = at.Add(7 * day)
	n = traceAgain(n, false, at)
	if n.Status != netSilent || n.Attempts != 3 || n.NextTraceAt != nil {
		t.Fatalf("third: %+v (no more attempts)", n)
	}
	// Brought back by a visitor (no next_trace_at): the attempts start again.
	at = at.Add(61 * day)
	n = traceAgain(n, false, at)
	if n.Attempts != 1 || !sameTime(n.NextTraceAt, daysAfter(at, 7)) {
		t.Fatalf("revived: %+v", n)
	}
}

func TestAfterTraceUnmatched(t *testing.T) {
	var n *tracedNet
	at := t0
	for i, wait := range []int{7, 20, 60} {
		n = traceAgain(n, true, at)
		if n.Status != netUnmatched || n.Attempts != i+1 || !sameTime(n.NextTraceAt, daysAfter(at, wait)) {
			t.Fatalf("attempt %d: %+v", i+1, n)
		}
		at = at.Add(time.Duration(wait) * day)
	}
	// After the 4th trace: nothing more, only a visitor brings it back.
	n = traceAgain(n, true, at)
	if n.Attempts != 4 || n.NextTraceAt != nil {
		t.Fatalf("4th: %+v", n)
	}
	// Revived: attempts start again at 1.
	at = at.Add(70 * day)
	n = traceAgain(n, true, at)
	if n.Attempts != 1 || !sameTime(n.NextTraceAt, daysAfter(at, 7)) {
		t.Fatalf("revived: %+v", n)
	}
}

// Only consecutive traces with the same status count.
func TestAfterTraceStatusChangeStartsOver(t *testing.T) {
	n := traceAgain(nil, false, t0)
	n = traceAgain(n, false, t0.Add(7*day)) // silent, attempt 2
	n = traceAgain(n, true, t0.Add(14*day)) // a hop answered: unmatched, attempt 1
	if n.Status != netUnmatched || n.Attempts != 1 || !sameTime(n.NextTraceAt, daysAfter(t0.Add(14*day), 7)) {
		t.Fatalf("silent -> unmatched: %+v", n)
	}
	n = traceAgain(n, true, t0.Add(21*day)) // attempt 2
	n = traceAgain(n, false, t0.Add(41*day))
	if n.Status != netSilent || n.Attempts != 1 {
		t.Fatalf("unmatched -> silent: %+v", n)
	}
}

// A located /24 stays located: the trace is a recheck, the next one is 150 days later,
// and locate decides whether the city stays.
func TestAfterTraceLocatedStaysLocated(t *testing.T) {
	loc := t0.Add(-10 * day)
	prev := &tracedNet{Prefix: "5.6.7.0/24", Status: netLocated, LastTracedAt: t0.Add(-150 * day), NextTraceAt: daysAfter(t0, -1), LocatedAt: &loc}
	for _, answered := range []bool{true, false} {
		n := afterTrace(prev, prev.Prefix, answered, t0)
		if n.Status != netLocated || n.Attempts != 0 || !sameTime(n.NextTraceAt, daysAfter(t0, 150)) || !n.LastTracedAt.Equal(t0) || !sameTime(n.LocatedAt, &loc) {
			t.Errorf("answered=%v: %+v", answered, n)
		}
	}
}

func TestScheduleStatements(t *testing.T) {
	// Cleanup: traces older than 200 days, and unlocated rows with no next_trace_at older than 200 days.
	if !strings.Contains(cleanupTracesSQL, "DELETE FROM public.network_traces") || !strings.Contains(cleanupTracesSQL, "interval '200 days'") {
		t.Errorf("cleanupTracesSQL = %s", cleanupTracesSQL)
	}
	for _, part := range []string{"DELETE FROM public.traced_networks", "status <> 'located'", "next_trace_at IS NULL", "last_traced_at < now() - interval '200 days'"} {
		if !strings.Contains(cleanupNetworksSQL, part) {
			t.Errorf("cleanupNetworksSQL lacks %q", part)
		}
	}
	// Visitor IPs: all but the newest of each traced /24; every one of a located /24.
	for _, part := range []string{"DELETE FROM public.visitor_ips", "row_number() OVER (PARTITION BY network(set_masklen(ip, 24)) ORDER BY last_seen_at DESC, ip)", "r.n > 1", "= ANY ($1::text[]::cidr[])"} {
		if !strings.Contains(keepNewestVisitorSQL, part) {
			t.Errorf("keepNewestVisitorSQL lacks %q", part)
		}
	}
	for _, part := range []string{"DELETE FROM public.visitor_ips", "family(ip) = 4", "FROM public.traced_networks WHERE status = 'located'"} {
		if !strings.Contains(deleteLocatedVisitorsSQL, part) {
			t.Errorf("deleteLocatedVisitorsSQL lacks %q", part)
		}
	}
	// A row-level lock on the schedule rows the trace updates, and an upsert on the key.
	if !strings.Contains(readScheduleSQL, "FOR UPDATE") || !strings.Contains(writeScheduleSQL, "ON CONFLICT (prefix) DO UPDATE") {
		t.Error("schedule read/write statements")
	}
	// Located rows come from the traced rows of the layer, next_trace_at 150 days after the last trace.
	for _, part := range []string{"scope = 'traced'", "'located', 0", "t.last_traced_at + interval '150 days'", "IS DISTINCT FROM"} {
		if !strings.Contains(markLocatedSQL, part) {
			t.Errorf("markLocatedSQL lacks %q", part)
		}
	}
	// The backfill: located from the layer, else unmatched/silent with attempts 1 and +7 days.
	for _, part := range []string{"'located'", "'unmatched'", "'silent'", "interval '150 days'", "interval '7 days'", "scope = 'traced'", "ON CONFLICT (prefix) DO NOTHING"} {
		if !strings.Contains(backfillScheduleSQL, part) {
			t.Errorf("backfillScheduleSQL lacks %q", part)
		}
	}
	// The path comparison leaves the target out and compares the two newest traces.
	for _, part := range []string{"array_remove(candidates, target)", "a.n = 1 AND b.n = 2", "a.c <> b.c"} {
		if !strings.Contains(comparePathsSQL, part) {
			t.Errorf("comparePathsSQL lacks %q", part)
		}
	}
	// The stale-name refresh: old answers of located /24s, oldest first, capped.
	for _, part := range []string{"public.rdns_geo_layer", "scope = 'traced'", "n.resolved_at < now() - interval '30 days'", "ORDER BY n.resolved_at", "LIMIT $1"} {
		if !strings.Contains(refreshQuery, part) {
			t.Errorf("refreshQuery lacks %q", part)
		}
	}
	for _, part := range []string{"n.tried_at IS NULL OR n.tried_at <= now() - CASE", "n.error_count >= 3", "n.error_count = 2", "$4::int", "$3::int", "$2::int"} {
		if !strings.Contains(refreshQuery, part) {
			t.Errorf("refreshQuery lacks retry backoff term %q", part)
		}
	}
	// Every lookup sets tried_at; an error moves nothing else of a stored ok/not_found row.
	for _, part := range []string{"tried_at", "tried_at = excluded.tried_at", "THEN excluded.ptr ELSE t.ptr", "THEN excluded.status ELSE t.status", "THEN excluded.resolved_at ELSE t.resolved_at", "now(), now()"} {
		if !strings.Contains(storeNamesSQL, part) {
			t.Errorf("storeNamesSQL lacks %q", part)
		}
	}
	if strings.Contains(storeNamesSQL, "WHERE excluded") {
		t.Error("storeNamesSQL still skips the update of tried_at for an error")
	}
	// Answer freshness stays on resolved_at; tried_at gates only recent errors.
	if !strings.Contains(freshNamesSQL, "resolved_at") || !strings.Contains(freshNamesSQL, "tried_at") || !strings.Contains(freshNamesSQL, "status = 'ok'") || !strings.Contains(freshNamesSQL, "status = 'not_found'") {
		t.Error("freshNamesSQL must use resolved_at for answers and tried_at for error backoff")
	}
	for _, part := range []string{"error_count > 0", "DISTINCT ON (prefix, target_kind)", "unnest(t.candidates)", "scope = 'traced'", "LIMIT $1"} {
		if !strings.Contains(hasDueNameErrorsSQL+retryFailedNamesQuery, part) {
			t.Errorf("due retry queries lack %q", part)
		}
	}
	for _, bad := range []string{"INSERT", "UPDATE", "DELETE"} {
		if strings.Contains(strings.ToUpper(refreshQuery), bad) {
			t.Errorf("refreshQuery contains %s", bad)
		}
	}
	if refreshMax != 500 {
		t.Errorf("refreshMax = %d", refreshMax)
	}
}

func TestShouldChain(t *testing.T) {
	full := PickOutput{Picked: maxNetworksPerRun, Limit: maxNetworksPerRun}
	for name, c := range map[string]struct {
		pick  PickOutput
		trace TraceOutput
		want  bool
	}{
		"full run, traces stored":        {full, TraceOutput{Stored: 3000}, true},
		"full run, nothing stored":       {full, TraceOutput{}, false},
		"short run":                      {PickOutput{Picked: 999, Limit: 1000}, TraceOutput{Stored: 2997}, false},
		"empty run":                      {PickOutput{Limit: 1000}, TraceOutput{}, false},
		"no limit (zero output)":         {PickOutput{}, TraceOutput{Stored: 5}, false},
		"picked more than the limit":     {PickOutput{Picked: 1001, Limit: 1000}, TraceOutput{Stored: 5}, false},
		"stored one trace is enough":     {full, TraceOutput{Stored: 1}, true},
		"the batch was traced but empty": {full, TraceOutput{Networks: 1000}, false},
	} {
		if got := ShouldChain(c.pick, c.trace); got != c.want {
			t.Errorf("%s: ShouldChain = %v, want %v", name, got, c.want)
		}
	}
}
