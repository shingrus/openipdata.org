package rdns

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func addrs(texts ...string) []netip.Addr {
	var out []netip.Addr
	for _, s := range texts {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

// hopsOf builds a stored path from IPs; "" is a hop that did not answer.
func hopsOf(ips ...string) []Hop {
	hops := make([]Hop, len(ips))
	for i, ip := range ips {
		hops[i] = Hop{TTL: i + 1}
		if ip != "" {
			s := ip
			rtt := float64(i) + 0.5
			hops[i].IP, hops[i].RTTms = &s, &rtt
		}
	}
	return hops
}

func TestCandidates(t *testing.T) {
	target := netip.MustParseAddr("5.6.7.9")
	for name, c := range map[string]struct {
		hops []Hop
		want []string
	}{
		"no hops":          {nil, []string{"5.6.7.9"}},
		"all silent":       {hopsOf("", "", ""), []string{"5.6.7.9"}},
		"one router":       {hopsOf("1.1.1.1"), []string{"5.6.7.9", "1.1.1.1"}},
		"target replied":   {hopsOf("1.1.1.1", "2.2.2.2", "5.6.7.9"), []string{"5.6.7.9", "2.2.2.2"}},
		"four: one":        {hopsOf("1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"), []string{"5.6.7.9", "4.4.4.4"}},
		"five: two":        {hopsOf("1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"), []string{"5.6.7.9", "5.5.5.5", "4.4.4.4"}},
		"silent hops skip": {hopsOf("1.1.1.1", "", "3.3.3.3", "", "", "6.6.6.6"), []string{"5.6.7.9", "6.6.6.6"}},
		"eight: two":       {hopsOf("1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5", "6.6.6.6", "7.7.7.7", "8.8.8.8"), []string{"5.6.7.9", "8.8.8.8", "7.7.7.7"}},
		"nine: three":      {hopsOf("1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5", "6.6.6.6", "7.7.7.7", "8.8.8.8", "9.9.9.9"), []string{"5.6.7.9", "9.9.9.9", "8.8.8.8", "7.7.7.7"}},
		"private tail":     {hopsOf("1.1.1.1", "2.2.2.2", "3.3.3.3", "10.0.0.1", "100.64.0.1", "192.168.5.5"), []string{"5.6.7.9", "3.3.3.3"}},
		"private only":     {hopsOf("10.0.0.1", "100.64.0.1"), []string{"5.6.7.9"}},
		"private mid":      {hopsOf("1.1.1.1", "10.0.0.1", "2.2.2.2", "100.64.0.9", "3.3.3.3", "4.4.4.4", "5.5.5.5"), []string{"5.6.7.9", "5.5.5.5", "4.4.4.4"}},
		"repeats once":     {hopsOf("1.1.1.1", "2.2.2.2", "2.2.2.2", "2.2.2.2"), []string{"5.6.7.9", "2.2.2.2"}},
	} {
		if got := candidates(target, c.hops); !reflect.DeepEqual(got, addrs(c.want...)) {
			t.Errorf("%s: candidates = %v, want %v", name, got, c.want)
		}
	}
}

func TestParseTrace(t *testing.T) {
	tr, ok := parseTrace("5.6.7.0/24", kindGateway, "5.6.7.1", []string{"5.6.7.1", "9.9.9.1", "2001:db8::1", "x"})
	if !ok || tr.Kind != kindGateway || tr.Prefix != netip.MustParsePrefix("5.6.7.0/24") || tr.Target != netip.MustParseAddr("5.6.7.1") ||
		!reflect.DeepEqual(tr.Candidates, addrs("5.6.7.1", "9.9.9.1")) {
		t.Fatalf("trace = %+v %v", tr, ok)
	}
	// A row with no candidates (not backfilled yet) has its target.
	if tr, ok := parseTrace("5.6.7.0/24", kindVisitor, "5.6.7.9", nil); !ok || !reflect.DeepEqual(tr.Candidates, addrs("5.6.7.9")) {
		t.Errorf("no candidates: %+v %v", tr, ok)
	}
	for _, target := range []string{"", "x", "2001:db8::1"} {
		if _, ok := parseTrace("5.6.7.0/24", kindVisitor, target, nil); ok {
			t.Errorf("target %q accepted", target)
		}
	}
	if _, ok := parseTrace("bad", kindVisitor, "5.6.7.9", nil); ok {
		t.Error("a bad prefix was accepted")
	}
}

// The backfill computes the candidates from the stored path, as trace does when it stores them.
func TestCandidatesOfStored(t *testing.T) {
	hops := `[{"ttl":1,"ip":"10.0.0.1","rtt_ms":0.4},{"ttl":2,"ip":"9.9.9.1","rtt_ms":2},{"ttl":3,"ip":null,"rtt_ms":null},{"ttl":4,"ip":"5.6.0.1","rtt_ms":3}]`
	want := candidates(netip.MustParseAddr("5.6.7.9"), []Hop{{TTL: 2, IP: ptr("9.9.9.1")}, {TTL: 4, IP: ptr("5.6.0.1")}})
	if got := candidatesOfStored("5.6.7.9", hops); !reflect.DeepEqual(got, want) || len(got) != 2 {
		t.Errorf("candidates = %v, want %v", got, want)
	}
	// A path that cannot be read is an empty one; a target that is not IPv4 has no candidates.
	if got := candidatesOfStored("5.6.7.9", "not json"); !reflect.DeepEqual(got, addrs("5.6.7.9")) {
		t.Errorf("bad path: %v", got)
	}
	if got := candidatesOfStored("2001:db8::1", "[]"); got != nil || joinAddrs(got) != "" {
		t.Errorf("IPv6 target: %v", got)
	}
}

func ptr(s string) *string { return &s }

func TestJoinAddrs(t *testing.T) {
	if got := joinAddrs(addrs("5.6.7.9", "1.1.1.1")); got != "5.6.7.9,1.1.1.1" {
		t.Errorf("joinAddrs = %q", got)
	}
	if joinAddrs(nil) != "" {
		t.Error("joinAddrs(nil) is not empty")
	}
}

func TestScrubIPs(t *testing.T) {
	got := scrubIPs("  scamper: cannot probe 5.6.7.8 via 10.0.0.1 (version 20240101)\n")
	if strings.Contains(got, "5.6.7.8") || strings.Contains(got, "10.0.0.1") || !strings.Contains(got, "<ip>") || !strings.Contains(got, "20240101") {
		t.Errorf("scrubIPs = %q", got)
	}
}

// hop_ips holds every public answering hop (the target and private hops left out, each IP
// once, path order); candidates only takes the last quarter of them.
func TestPublicHopsAndHopIPsOfStored(t *testing.T) {
	hops := `[{"ttl":1,"ip":"10.0.0.1"},{"ttl":2,"ip":"9.9.9.1"},{"ttl":3,"ip":null},{"ttl":4,"ip":"9.9.9.1"},{"ttl":5,"ip":"9.9.9.2"},{"ttl":6,"ip":"8.8.8.1"},{"ttl":7,"ip":"5.6.7.9"}]`
	want := addrs("9.9.9.1", "9.9.9.2", "8.8.8.1")
	if got := hopIPsOfStored("5.6.7.9", hops); !reflect.DeepEqual(got, want) {
		t.Errorf("hop IPs = %v, want %v", got, want)
	}
	// The quarter of 3 hops is 1: candidates is a subset of target + hop IPs.
	if got := candidatesOfStored("5.6.7.9", hops); !reflect.DeepEqual(got, addrs("5.6.7.9", "8.8.8.1")) {
		t.Errorf("candidates = %v", got)
	}
	if got := hopIPsOfStored("5.6.7.9", "not json"); len(got) != 0 {
		t.Errorf("bad path: %v", got)
	}
	if got := hopIPsOfStored("2001:db8::1", hops); got != nil {
		t.Errorf("IPv6 target: %v", got)
	}
}

// resolve-names names the candidates and every hop; locate's candidateIPs stays the short list.
func TestNameIPsIncludeAllHops(t *testing.T) {
	trace := tr("5.6.7.0/24", kindVisitor, "5.6.7.9", "9.9.9.1", "9.9.9.2", "9.9.9.3", "9.9.9.4", "5.6.0.1")
	trace.HopIPs = addrs("9.9.9.1", "9.9.9.2", "9.9.9.3", "9.9.9.4", "5.6.0.1", "10.0.0.1")
	if got := candidateIPs([]traceIn{trace}); !reflect.DeepEqual(got, addrs("5.6.0.1", "5.6.7.9", "9.9.9.4")) {
		t.Errorf("candidateIPs = %v", got)
	}
	want := addrs("5.6.0.1", "5.6.7.9", "9.9.9.1", "9.9.9.2", "9.9.9.3", "9.9.9.4")
	if got := nameIPs([]traceIn{trace}); !reflect.DeepEqual(got, want) {
		t.Errorf("nameIPs = %v, want %v", got, want)
	}
}

func TestTracesQueryHopIPsOnlyWhenAsked(t *testing.T) {
	if contains(tracesQuery, "hop_ips") {
		t.Errorf("locate's trace query reads hop_ips: %s", tracesQuery)
	}
	if !contains(tracesWithHopsQuery, "hop_ips") || contains(tracesWithHopsQuery, "hops,") || contains(tracesWithHopsQuery, ", hops") {
		t.Errorf("resolve-names trace query: %s", tracesWithHopsQuery)
	}
	if !contains(backfillHopIPsQuery, "hop_ips IS NULL") || !contains(backfillHopIPsSQL, "hop_ips IS NULL") {
		t.Error("hop_ips backfill does not target NULL rows only")
	}
}
