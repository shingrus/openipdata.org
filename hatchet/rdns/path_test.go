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
	tr, ok := parseTrace("5.6.7.0/24", kindGateway, "5.6.7.1", `[{"ttl":1,"ip":"10.0.0.1","rtt_ms":0.4},{"ttl":2,"ip":null,"rtt_ms":null}]`)
	if !ok || tr.Kind != kindGateway || tr.Prefix != netip.MustParsePrefix("5.6.7.0/24") || tr.Target != netip.MustParseAddr("5.6.7.1") || len(tr.Hops) != 2 {
		t.Fatalf("trace = %+v %v", tr, ok)
	}
	if a, ok := tr.Hops[0].addr(); !ok || a != netip.MustParseAddr("10.0.0.1") {
		t.Errorf("hop 1 = %v %v", a, ok)
	}
	if _, ok := tr.Hops[1].addr(); ok {
		t.Error("a silent hop has an address")
	}
	// A path that cannot be read is an empty one; a bad target is not a trace.
	if tr, ok := parseTrace("5.6.7.0/24", kindVisitor, "5.6.7.9", `not json`); !ok || tr.Hops != nil {
		t.Errorf("bad path: %+v %v", tr, ok)
	}
	for _, target := range []string{"", "x", "2001:db8::1"} {
		if _, ok := parseTrace("5.6.7.0/24", kindVisitor, target, "[]"); ok {
			t.Errorf("target %q accepted", target)
		}
	}
}

func TestScrubIPs(t *testing.T) {
	got := scrubIPs("  scamper: cannot probe 5.6.7.8 via 10.0.0.1 (version 20240101)\n")
	if strings.Contains(got, "5.6.7.8") || strings.Contains(got, "10.0.0.1") || !strings.Contains(got, "<ip>") || !strings.Contains(got, "20240101") {
		t.Errorf("scrubIPs = %q", got)
	}
}
