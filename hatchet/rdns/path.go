package rdns

import (
	"encoding/json"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"openipdata.org/hatchet/ipdata"
)

// Target kinds: the three hosts traced in each /24.
const (
	kindVisitor = "visitor" // the newest visitor IP of the /24
	kindGateway = "gateway" // .1
	kindRandom  = "random"  // one random host
)

// kindOrder is the order of the kinds wherever one must be preferred (ties).
var kindOrder = []string{kindVisitor, kindGateway, kindRandom}

// Hop is one hop of a stored path: the probe TTL, the router's IP (nil when no
// reply came) and the round trip time in milliseconds.
type Hop struct {
	TTL   int      `json:"ttl"`
	IP    *string  `json:"ip"`
	RTTms *float64 `json:"rtt_ms"`
}

// addr returns the hop's IP, or false for a hop that did not answer.
func (h Hop) addr() (netip.Addr, bool) {
	if h.IP == nil {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(*h.IP)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// candidates lists the IPs of a trace that are named and matched against the
// rules, closest to the target first: the target itself, then the last quarter
// (rounded up, at least one) of the other hops that answered from a public IPv4
// address, the one nearest the target first. Private and shared-space hops (the
// end of an access network's path is often 10/8 or 100.64/10) have no PTR name
// worth reading, so they neither count nor fill the quarter. Each IP appears
// once. resolve-names and locate both use it, so they agree on which hops matter.
func candidates(target netip.Addr, hops []Hop) []netip.Addr {
	out := []netip.Addr{target}
	var path []netip.Addr
	seen := map[netip.Addr]bool{target: true}
	for _, h := range hops {
		if a, ok := h.addr(); ok && !seen[a] && ipdata.PublicV4(a) {
			seen[a] = true
			path = append(path, a)
		}
	}
	if len(path) == 0 {
		return out
	}
	tail := (len(path) + 3) / 4
	for i := len(path) - 1; i >= len(path)-tail; i-- {
		out = append(out, path[i])
	}
	return out
}

// ipv4Text finds IPv4 addresses in text that is going to a log or an error.
var ipv4Text = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)

// scrubIPs hides IPv4 addresses in text: visitor and target IPs must never
// reach Hatchet task outputs, logs or errors.
func scrubIPs(text string) string {
	return ipv4Text.ReplaceAllString(strings.TrimSpace(text), "<ip>")
}

// traceIn is a stored trace: the target and its candidates (see candidates), as
// stored in network_traces.candidates. The path itself is never read back.
type traceIn struct {
	Prefix     netip.Prefix
	Kind       string
	Target     netip.Addr
	Candidates []netip.Addr // the target first, then the tail hops, closest to the target first
	TracedAt   time.Time    // when it was traced; zero in tests
}

// parseTrace reads a trace from its stored text forms; false when the target
// is not an IPv4 address. A candidate that is not an IPv4 address is left out,
// and a trace with none is given its target.
func parseTrace(prefix, kind, target string, cands []string) (traceIn, bool) {
	t := traceIn{Kind: kind}
	addr, err := netip.ParseAddr(target)
	if err != nil || !addr.Unmap().Is4() {
		return t, false
	}
	t.Target = addr.Unmap()
	if prefix != "" {
		if t.Prefix, err = netip.ParsePrefix(prefix); err != nil {
			return t, false
		}
	}
	for _, text := range cands {
		if a, err := netip.ParseAddr(text); err == nil && a.Unmap().Is4() {
			t.Candidates = append(t.Candidates, a.Unmap())
		}
	}
	if len(t.Candidates) == 0 {
		t.Candidates = []netip.Addr{t.Target}
	}
	return t, true
}

// candidatesOfStored computes the candidates of a stored trace from its target and
// its path (the hops JSON), for the one-time backfill of network_traces.candidates.
// A target that is not an IPv4 address has none (an empty list); a path that cannot
// be read is an empty one.
func candidatesOfStored(target, hops string) []netip.Addr {
	addr, err := netip.ParseAddr(target)
	if err != nil || !addr.Unmap().Is4() {
		return nil
	}
	var path []Hop
	if json.Unmarshal([]byte(hops), &path) != nil {
		path = nil
	}
	return candidates(addr.Unmap(), path)
}
