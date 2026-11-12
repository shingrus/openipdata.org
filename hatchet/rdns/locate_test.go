package rdns

import (
	"errors"
	"net/netip"
	"testing"

	"openipdata.org/hatchet/ipdata"
)

// locateWorld is a small internet: AS100 (5.6.0.0/16, the networks under test), AS200
// (9.9.0.0/16, a transit carrier), AS300 (20.0.0.0/12, announced wider than /16)
// and a /24 announced alone. Rules: fra and muc in isp.net, ams in transit.net.
func locateWorld(t *testing.T) (*originTable, []rule) {
	t.Helper()
	o := newOriginTable()
	o.add(netip.MustParsePrefix("5.6.0.0/16"), 100)
	o.add(netip.MustParsePrefix("9.9.0.0/16"), 200)
	o.add(netip.MustParsePrefix("20.0.0.0/12"), 300)
	o.add(netip.MustParsePrefix("40.3.4.0/24"), 400)
	rules, st := compileRules([]ruleRow{
		{"fra", `\bfra\d*\b`, `["isp.net"]`, "DE", "Frankfurt"},
		{"muc", `\bmuc\d*\b`, `["isp.net"]`, "DE", "Munich"},
		{"ams", `\bams\d*\b`, `["transit.net"]`, "NL", "Amsterdam"},
		{"lon", `\blon\d*\b`, `["isp.net", "transit.net"]`, "GB", "London"},
	})
	if st.Loaded != 4 {
		t.Fatalf("rules: %+v", st)
	}
	return o, rules
}

func tr(prefix, kind, target string, hops ...string) traceIn {
	t := netip.MustParseAddr(target)
	return traceIn{Prefix: netip.MustParsePrefix(prefix), Kind: kind, Target: t, Candidates: candidates(t, hopsOf(hops...))}
}

func names(pairs ...string) map[netip.Addr]string {
	m := map[netip.Addr]string{}
	for i := 0; i < len(pairs); i += 2 {
		m[netip.MustParseAddr(pairs[i])] = pairs[i+1]
	}
	return m
}

func rowFor(rows []geoRow, prefix, scope string) *geoRow {
	for i := range rows {
		if rows[i].Prefix.String() == prefix && rows[i].Scope == scope {
			return &rows[i]
		}
	}
	return nil
}

// A router in the target's ASN gives the /24 a traced row and a vote for the announced prefix.
func TestLocateSameASNMatch(t *testing.T) {
	o, rules := locateWorld(t)
	traces := []traceIn{tr("5.6.7.0/24", kindVisitor, "5.6.7.9", "10.0.0.1", "9.9.9.1", "5.6.0.1", "5.6.7.9")}
	rows, out := locate(traces, names("5.6.0.1", "ae1.fra01.isp.net"), rules, o)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rowFor(rows, "5.6.7.0/24", scopeTraced)
	if r == nil || r.Country != "DE" || r.City != "Frankfurt" || r.RouterIP.String() != "5.6.0.1" ||
		r.PTR != "ae1.fra01.isp.net" || r.Rule != "fra" || r.SameASN == nil || !*r.SameASN {
		t.Errorf("traced row = %+v", r)
	}
	w := rowFor(rows, "5.6.0.0/16", scopeWide)
	if w == nil || w.City != "Frankfurt" || w.RouterIP.IsValid() || w.SameASN != nil {
		t.Errorf("wide row = %+v", w)
	}
	if out.Traced != 1 || out.Wide != 1 || out.Votes != 1 || out.Networks != 1 || out.Traces != 1 || out.NoASN != 0 {
		t.Errorf("out = %+v", out)
	}
	ks := out.Kinds[kindVisitor]
	if ks.Traces != 1 || ks.ReachedASN != 1 || ks.Located != 1 || ks.UniqueWins != 1 || ks.Compared != 0 {
		t.Errorf("visitor stats = %+v", ks)
	}
}

// A transit router in another ASN is named after a city: it only votes.
func TestLocateOtherASNOnlyVotes(t *testing.T) {
	o, rules := locateWorld(t)
	traces := []traceIn{tr("5.6.7.0/24", kindVisitor, "5.6.7.9", "10.0.0.1", "9.9.9.1")}
	rows, out := locate(traces, names("9.9.9.1", "be2.ams1.transit.net"), rules, o)
	if rowFor(rows, "5.6.7.0/24", scopeTraced) != nil {
		t.Error("a traced row from a router of another ASN")
	}
	if w := rowFor(rows, "5.6.0.0/16", scopeWide); w == nil || w.City != "Amsterdam" || w.Country != "NL" {
		t.Errorf("wide row = %+v", w)
	}
	if out.Traced != 0 || out.Wide != 1 || out.Votes != 1 || out.Kinds[kindVisitor].Located != 1 || out.Kinds[kindVisitor].ReachedASN != 0 {
		t.Errorf("out = %+v", out)
	}
}

// Candidates in the target's ASN come first, even when another ASN's router is closer.
func TestLocateSameASNBeforeCloserOtherASN(t *testing.T) {
	o, rules := locateWorld(t)
	// Six answering public hops: the last two are candidates. 9.9.9.2 (transit, ams) is the closest,
	// 5.6.0.2 (the target's own ASN, fra) is next.
	traces := []traceIn{tr("5.6.7.0/24", kindVisitor, "5.6.7.9", "11.0.0.1", "11.0.0.2", "9.9.9.1", "9.9.9.3", "5.6.0.2", "9.9.9.2")}
	ptrs := names("9.9.9.2", "be2.ams1.transit.net", "5.6.0.2", "ae0.fra2.isp.net")
	rows, _ := locate(traces, ptrs, rules, o)
	r := rowFor(rows, "5.6.7.0/24", scopeTraced)
	if r == nil || r.City != "Frankfurt" {
		t.Fatalf("traced row = %+v", r)
	}
	if w := rowFor(rows, "5.6.0.0/16", scopeWide); w == nil || w.City != "Frankfurt" {
		t.Errorf("the vote follows the traced row: %+v", w)
	}
	// With nothing to prefer, the closest candidate wins: the target itself before a router.
	ptrs = names("5.6.7.9", "host.muc.isp.net", "5.6.0.2", "ae0.fra2.isp.net")
	rows, _ = locate(traces, ptrs, rules, o)
	if r := rowFor(rows, "5.6.7.0/24", scopeTraced); r == nil || r.City != "Munich" || r.RouterIP.String() != "5.6.7.9" {
		t.Errorf("target's own name: %+v", r)
	}
}

// The path matters only to its last quarter: a name far from the target is not a candidate.
func TestLocateIgnoresHopsFarFromTarget(t *testing.T) {
	o, rules := locateWorld(t)
	traces := []traceIn{tr("5.6.7.0/24", kindVisitor, "5.6.7.9", "5.6.0.7", "11.0.0.2", "11.0.0.3", "11.0.0.4")}
	rows, out := locate(traces, names("5.6.0.7", "ae0.fra2.isp.net"), rules, o)
	if len(rows) != 0 || out.Kinds[kindVisitor].Located != 0 {
		t.Errorf("rows = %+v, out = %+v", rows, out)
	}
}

// Targets of one /24 that name different cities: the match of the same ASN closest to the target wins; a tie goes to the earlier kind.
func TestLocateDisagreeingTargets(t *testing.T) {
	o, rules := locateWorld(t)
	p := "5.6.7.0/24"
	// visitor: the router 5.6.0.1 is its only candidate (rank 1), fra. gateway: its own name
	// (rank 0), muc. random: no match.
	traces := []traceIn{
		tr(p, kindVisitor, "5.6.7.9", "10.0.0.1", "5.6.0.1"),
		tr(p, kindGateway, "5.6.7.1", "10.0.0.1", "5.6.0.1"),
		tr(p, kindRandom, "5.6.7.77", "10.0.0.1"),
	}
	ptrs := names("5.6.0.1", "ae1.fra1.isp.net", "5.6.7.1", "gw.muc.isp.net")
	rows, out := locate(traces, ptrs, rules, o)
	r := rowFor(rows, p, scopeTraced)
	if r == nil || r.City != "Munich" || r.RouterIP.String() != "5.6.7.1" {
		t.Fatalf("row = %+v", r)
	}
	if out.Disagreements != 1 || out.Traced != 1 {
		t.Errorf("out = %+v", out)
	}
	// The visitor trace has rank 1 with the router that the gateway trace has too, so both name fra (rank 1) -> tie 1 vs 0:
	// the gateway's own-name match has the lower rank. Equal ranks go to the visitor.
	ptrs = names("5.6.0.1", "ae1.fra1.isp.net")
	traces[1] = tr(p, kindGateway, "5.6.7.1", "10.0.0.1", "5.6.0.3")
	ptrs[netip.MustParseAddr("5.6.0.3")] = "ae1.muc1.isp.net"
	rows, out = locate(traces, ptrs, rules, o)
	if r := rowFor(rows, p, scopeTraced); r == nil || r.City != "Frankfurt" || out.Disagreements != 1 {
		t.Errorf("tie: row = %+v, out = %+v", r, out)
	}
	ks := out.Kinds
	if ks[kindVisitor].Compared != 1 || ks[kindVisitor].Agreed != 1 || ks[kindGateway].Compared != 1 || ks[kindGateway].Agreed != 0 ||
		ks[kindVisitor].UniqueWins != 0 || ks[kindRandom].Located != 0 {
		t.Errorf("kind stats: %+v %+v %+v", ks[kindVisitor], ks[kindGateway], ks[kindRandom])
	}
}

// A target that found the city alone has a unique win; two that agree compare and agree.
func TestLocateKindStats(t *testing.T) {
	o, rules := locateWorld(t)
	traces := []traceIn{
		// /24 one: only the random target is located.
		tr("5.6.1.0/24", kindVisitor, "5.6.1.9", "5.6.0.9"),
		tr("5.6.1.0/24", kindGateway, "5.6.1.1", "5.6.0.9"),
		tr("5.6.1.0/24", kindRandom, "5.6.1.50", "5.6.0.8"),
		// /24 two: visitor and gateway agree.
		tr("5.6.2.0/24", kindVisitor, "5.6.2.9", "5.6.0.1"),
		tr("5.6.2.0/24", kindGateway, "5.6.2.1", "5.6.0.1"),
		// /24 three: nothing answers.
		tr("5.6.3.0/24", kindVisitor, "5.6.3.9"),
	}
	ptrs := names("5.6.0.8", "r1.fra1.isp.net", "5.6.0.1", "r2.fra2.isp.net")
	rows, out := locate(traces, ptrs, rules, o)
	if out.Networks != 3 || out.Traced != 2 || len(rows) != 3 { // two traced rows and one wide
		t.Fatalf("out = %+v rows = %+v", out, rows)
	}
	v, g, r := out.Kinds[kindVisitor], out.Kinds[kindGateway], out.Kinds[kindRandom]
	if v.Traces != 3 || v.Located != 1 || v.Compared != 1 || v.Agreed != 1 || v.UniqueWins != 0 {
		t.Errorf("visitor = %+v", v)
	}
	if g.Traces != 2 || g.Located != 1 || g.Compared != 1 || g.Agreed != 1 {
		t.Errorf("gateway = %+v", g)
	}
	if r.Traces != 1 || r.Located != 1 || r.UniqueWins != 1 || r.Compared != 0 {
		t.Errorf("random = %+v", r)
	}
	// Every trace reached the ASN through 5.6.0.x, except the silent one.
	if v.ReachedASN != 2 || g.ReachedASN != 2 || r.ReachedASN != 1 {
		t.Errorf("reached: %d %d %d", v.ReachedASN, g.ReachedASN, r.ReachedASN)
	}
}

// reached_asn counts tail hop candidates in the target's ASN: the target itself does not count.
func TestLocateReachedASNIgnoresTarget(t *testing.T) {
	o, rules := locateWorld(t)
	for name, c := range map[string]struct {
		trace   traceIn
		reached int
	}{
		"only the target":  {tr("5.6.7.0/24", kindVisitor, "5.6.7.9"), 0},
		"target replied":   {tr("5.6.7.0/24", kindVisitor, "5.6.7.9", "9.9.9.1", "5.6.7.9"), 0},
		"router in the AS": {tr("5.6.7.0/24", kindVisitor, "5.6.7.9", "9.9.9.1", "5.6.0.1"), 1},
		"transit only":     {tr("5.6.7.0/24", kindVisitor, "5.6.7.9", "9.9.9.1"), 0},
	} {
		_, out := locate([]traceIn{c.trace}, nil, rules, o)
		if got := out.Kinds[kindVisitor].ReachedASN; got != c.reached {
			t.Errorf("%s: reached_asn = %d, want %d", name, got, c.reached)
		}
	}
}

// With no origin ASN for the target the first match gives a traced row (same_asn
// unknown) but there is no announced prefix to vote for.
func TestLocateUnknownASN(t *testing.T) {
	o, rules := locateWorld(t)
	traces := []traceIn{tr("77.1.1.0/24", kindVisitor, "77.1.1.9", "10.0.0.1", "9.9.9.1")}
	rows, out := locate(traces, names("9.9.9.1", "be2.ams1.transit.net"), rules, o)
	r := rowFor(rows, "77.1.1.0/24", scopeTraced)
	if r == nil || r.City != "Amsterdam" || r.SameASN != nil {
		t.Fatalf("row = %+v", r)
	}
	if len(rows) != 1 || out.Votes != 0 || out.Wide != 0 || out.NoASN != 1 || out.Kinds[kindVisitor].ReachedASN != 0 {
		t.Errorf("out = %+v rows = %+v", out, rows)
	}
}

// Per announced prefix the city with most votes wins; a tie gets no wide row.
func TestLocateWideVotes(t *testing.T) {
	o, rules := locateWorld(t)
	// Four /24s of 5.6.0.0/16: three Frankfurt, one Munich.
	var traces []traceIn
	ptrs := names("5.6.0.1", "a.fra1.isp.net", "5.6.0.2", "a.muc1.isp.net")
	for i, router := range []string{"5.6.0.1", "5.6.0.1", "5.6.0.1", "5.6.0.2"} {
		prefix := "5.6." + string(rune('1'+i)) + ".0/24"
		traces = append(traces, tr(prefix, kindVisitor, "5.6."+string(rune('1'+i))+".9", router))
	}
	rows, out := locate(traces, ptrs, rules, o)
	w := rowFor(rows, "5.6.0.0/16", scopeWide)
	if w == nil || w.City != "Frankfurt" || out.Wide != 1 || out.Votes != 4 || out.Traced != 4 {
		t.Fatalf("wide = %+v, out = %+v", w, out)
	}
	// One against one: a tie gets no wide row, the /24s keep their own.
	rows, out = locate([]traceIn{traces[0], traces[3]}, ptrs, rules, o)
	if rowFor(rows, "5.6.0.0/16", scopeWide) != nil || out.WideTies != 1 || out.Wide != 0 || out.Traced != 2 {
		t.Errorf("tie: rows = %+v, out = %+v", rows, out)
	}
}

// The wide prefix is the announced prefix of the /24, never wider than /16.
func TestLocateWidePrefixes(t *testing.T) {
	o, rules := locateWorld(t)
	ptrs := names("20.1.0.1", "a.fra1.isp.net", "20.9.0.1", "a.muc1.isp.net", "40.3.4.2", "a.lon1.isp.net")
	traces := []traceIn{
		tr("20.1.5.0/24", kindVisitor, "20.1.5.9", "20.1.0.1"), // inside the /12: its /16 is 20.1.0.0/16
		tr("20.9.5.0/24", kindVisitor, "20.9.5.9", "20.9.0.1"), // the same /12, another /16: another row
		tr("40.3.4.0/24", kindVisitor, "40.3.4.9", "40.3.4.2"), // announced as a /24 itself
	}
	rows, _ := locate(traces, ptrs, rules, o)
	for prefix, city := range map[string]string{"20.1.0.0/16": "Frankfurt", "20.9.0.0/16": "Munich", "40.3.4.0/24": "London"} {
		if w := rowFor(rows, prefix, scopeWide); w == nil || w.City != city {
			t.Errorf("wide row %s = %+v, want %s", prefix, w, city)
		}
	}
	// 40.3.4.0/24 is both a traced row and a wide row: the key is (scope, prefix).
	if rowFor(rows, "40.3.4.0/24", scopeTraced) == nil {
		t.Errorf("rows = %+v", rows)
	}
	if rowFor(rows, "20.0.0.0/12", scopeWide) != nil {
		t.Error("a wide row broader than /16")
	}
}

// Output is sorted: traced rows by address, then wide rows.
func TestLocateRowOrderIsStable(t *testing.T) {
	o, rules := locateWorld(t)
	ptrs := names("5.6.0.1", "a.fra1.isp.net", "20.1.0.1", "a.muc1.isp.net")
	traces := []traceIn{
		tr("20.1.5.0/24", kindVisitor, "20.1.5.9", "20.1.0.1"),
		tr("5.6.9.0/24", kindVisitor, "5.6.9.9", "5.6.0.1"),
		tr("5.6.2.0/24", kindVisitor, "5.6.2.9", "5.6.0.1"),
	}
	first, _ := locate(traces, ptrs, rules, o)
	var got []string
	for _, r := range first {
		got = append(got, r.Scope+" "+r.Prefix.String())
	}
	want := []string{"traced 5.6.2.0/24", "traced 5.6.9.0/24", "traced 20.1.5.0/24", "wide 5.6.0.0/16", "wide 20.1.0.0/16"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// Without rules, or without names, nothing is located.
func TestLocateNothingToMatch(t *testing.T) {
	o, rules := locateWorld(t)
	traces := []traceIn{tr("5.6.7.0/24", kindVisitor, "5.6.7.9", "5.6.0.1")}
	if rows, out := locate(traces, names("5.6.0.1", "a.fra1.isp.net"), nil, o); len(rows) != 0 || out.Rules != 0 {
		t.Errorf("no rules: %+v %+v", rows, out)
	}
	if rows, _ := locate(traces, nil, rules, o); len(rows) != 0 {
		t.Errorf("no names: %+v", rows)
	}
	// A name outside the rule's domain is no match.
	if rows, _ := locate(traces, names("5.6.0.1", "a.fra1.other.net"), rules, o); len(rows) != 0 {
		t.Errorf("wrong domain: %+v", rows)
	}
	if rows, out := locate(nil, nil, rules, o); len(rows) != 0 || out.Networks != 0 || out.Kinds[kindVisitor] == nil {
		t.Errorf("no traces: %+v %+v", rows, out)
	}
}

func TestCheckShrink(t *testing.T) {
	for _, c := range []struct {
		previous, rows int
		refuse         bool
	}{
		{0, 0, false}, // an empty table accepts anything, even nothing
		{0, 10, false},
		{100, 100, false},
		{100, 150, false},
		{100, 90, false}, // exactly 10% fewer is allowed
		{100, 89, true},
		{100, 0, true}, // nothing found: a broken run
		{10, 8, true},
	} {
		err := checkShrink(c.previous, c.rows)
		if (err != nil) != c.refuse || (err != nil && !errors.Is(err, ErrShrink)) {
			t.Errorf("checkShrink(%d, %d) = %v, want refuse=%v", c.previous, c.rows, err, c.refuse)
		}
	}
}

func TestTracesQueryReadsOnly(t *testing.T) {
	for _, part := range []string{"DISTINCT ON (prefix, target_kind)", "$1::text::interval", "ORDER BY prefix, target_kind, traced_at DESC", "candidates"} {
		if !contains(tracesQuery, part) {
			t.Errorf("tracesQuery lacks %q", part)
		}
	}
	// The path is never read back, except once by the backfill.
	if contains(tracesQuery, "hops") {
		t.Errorf("tracesQuery reads the path: %s", tracesQuery)
	}
	// public.asn is only read, with the seen_at rule of the other readers, and only its IPv4 prefixes.
	if !contains(originQuery, "FROM public.asn a") || !contains(originQuery, ipdata.SeenAtFilter) ||
		!contains(originQuery, "{prefixes,ipv4}") || contains(originQuery, "a.json,") || contains(originQuery, ", a.json ") {
		t.Errorf("originQuery = %s", originQuery)
	}
	// The fallback for a damaged json value keeps the same current-ASN rule.
	if !contains(originFallbackQuery, "FROM public.asn a") || !contains(originFallbackQuery, ipdata.SeenAtFilter) {
		t.Errorf("originFallbackQuery = %s", originFallbackQuery)
	}
	for _, part := range []string{"public.ip_ptr_names", "ip = ANY ($1::text[]::inet[])", "status = 'ok'"} {
		if !contains(namesQuery, part) {
			t.Errorf("namesQuery lacks %q", part)
		}
	}
	if locateWindow != "180 days" || resolveWindow != "7 days" {
		t.Errorf("windows: locate %s, resolve-names %s", locateWindow, resolveWindow)
	}
	for _, q := range []string{tracesQuery, originQuery, originFallbackQuery, namesQuery, fingerprintTracesQuery, fingerprintNamesQuery, fingerprintASNQuery, fingerprintRulesQuery} {
		for _, bad := range []string{"INSERT", "UPDATE", "ALTER", "DELETE", "CREATE"} {
			if contains(upper(q), bad) {
				t.Errorf("%s contains %s", q, bad)
			}
		}
	}
}

// locate reads the names of the candidate IPs only: the public target and the last
// public hops of every trace, each once, in address order.
func TestCandidateIPs(t *testing.T) {
	traces := []traceIn{
		tr("5.6.7.0/24", kindVisitor, "5.6.7.9", "10.0.0.1", "9.9.9.1", "5.6.0.1"),
		tr("5.6.7.0/24", kindGateway, "5.6.7.1", "10.0.0.1", "9.9.9.1", "5.6.0.1"),
		tr("5.6.8.0/24", kindVisitor, "5.6.8.9"),
	}
	got := candidateIPs(traces)
	want := addrs("5.6.0.1", "5.6.7.1", "5.6.7.9", "5.6.8.9")
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d = %v, want %v", i, got[i], want[i])
		}
	}
	if got := candidateIPs(nil); len(got) != 0 {
		t.Errorf("no traces: %v", got)
	}
}
