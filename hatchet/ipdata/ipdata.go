// Package ipdata holds the small helpers about IP space and public.asn that
// several workflows need: which addresses are public, what a country code looks
// like, how the announced prefixes of public.asn.json are read, and which
// public.asn rows are current. Workflows share this code, not each other's files.
package ipdata

import (
	"encoding/json"
	"net/netip"
	"strings"
)

// NonPublicV4 lists the IPv4 blocks that are not global unicast: this-network,
// private, shared (RFC 6598), loopback, link-local, the special-purpose blocks of
// RFC 6890, benchmarking, documentation, multicast and reserved space (which
// includes the limited broadcast). All are /24 or wider, so a /24 is either inside
// one or outside all of them.
var NonPublicV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// globalV6 is IPv6 global unicast space. Everything outside 2000::/3 is
// special-purpose; ::/96 in particular is where an IPv6 MMDB tree keeps the
// IPv4 addresses.
var globalV6 = netip.MustParsePrefix("2000::/3")

// nonPublicV6 lists the special-purpose blocks inside 2000::/3.
var nonPublicV6 = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"),     // IETF protocol assignments, Teredo included
	netip.MustParsePrefix("2001:db8::/32"), // documentation
	netip.MustParsePrefix("3fff::/20"),     // documentation
	netip.MustParsePrefix("2002::/16"),     // 6to4: stands for IPv4 addresses
}

// PublicAddr reports whether addr is a public (global unicast) address. An
// IPv4-mapped IPv6 address counts as the IPv4 address.
func PublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false
	}
	if addr.Is6() {
		if !globalV6.Contains(addr) {
			return false
		}
		for _, p := range nonPublicV6 {
			if p.Contains(addr) {
				return false
			}
		}
		return true
	}
	for _, p := range NonPublicV4 {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// PublicV4 reports whether addr is a public IPv4 address (or an IPv4-mapped one).
func PublicV4(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.Is4() && PublicAddr(addr)
}

// IsCountryCode reports whether code is two capital letters. (It does not
// check that the code is assigned.)
func IsCountryCode(code string) bool {
	return len(code) == 2 && code[0] >= 'A' && code[0] <= 'Z' && code[1] >= 'A' && code[1] <= 'Z'
}

// SeenAtFilter is the WHERE condition on public.asn (aliased a) that keeps the
// current ASNs: public.asn never loses rows, so an ASN that left the source is
// left out after 7 days without a build-asn-data run seeing it. A NULL seen_at
// counts only until the first run has stamped some row: after that, NULL means
// the ASN was already gone when the column was added.
const SeenAtFilter = `(a.seen_at > now() - interval '7 days'
	       OR (a.seen_at IS NULL AND NOT EXISTS (SELECT 1 FROM public.asn WHERE seen_at IS NOT NULL)))`

// ASNPrefixes reads the announced prefixes (IPv4, then IPv6) from a public.asn
// json value, as written (not masked); unparsable ones are left out.
func ASNPrefixes(raw string) []netip.Prefix {
	var data struct {
		Prefixes struct {
			IPv4 []string `json:"ipv4"`
			IPv6 []string `json:"ipv6"`
		} `json:"prefixes"`
	}
	if json.Unmarshal([]byte(raw), &data) != nil {
		return nil
	}
	var out []netip.Prefix
	for _, text := range append(data.Prefixes.IPv4, data.Prefixes.IPv6...) {
		if p, err := netip.ParsePrefix(strings.TrimSpace(text)); err == nil {
			out = append(out, p)
		}
	}
	return out
}
