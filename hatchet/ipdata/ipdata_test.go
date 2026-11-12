package ipdata

import (
	"net/netip"
	"strings"
	"testing"
)

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "193.0.0.1": true, "100.63.255.255": true, "100.128.0.1": true, "172.32.0.1": true, "198.20.0.1": true,
		"10.0.0.1": false, "127.0.0.1": false, "169.254.169.254": false, "100.64.0.1": false, "192.168.1.1": false, "172.16.0.1": false,
		"224.0.0.1": false, "255.255.255.255": false, "0.0.0.0": false,
		"2a00:1450::1": true, "2001:db8::1": false, "2001::1": false, "2001:1::1": false, "2001:200::1": true,
		"3fff::1": false, "2002::1": false, "::1": false, "0.1.2.3": false, "192.0.0.8": false, "198.18.0.5": false, "240.0.0.1": false, "::ffff:10.0.0.1": false,
		"::ffff:8.8.8.8": true, "fe80::1": false, "fc00::1": false,
	} {
		if got := PublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("PublicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
	if PublicAddr(netip.Addr{}) {
		t.Error("the zero address is public")
	}
}

// The IPv4 list is the union of what net/netip calls non-global-unicast or private
// and the RFC 6890 blocks netip still accepts: PublicAddr must agree with that
// definition (the one geofeeds used before the check was shared) on every /24.
func TestPublicAddrMatchesNetip(t *testing.T) {
	reserved := []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	}
	old := func(a netip.Addr) bool {
		if !a.IsGlobalUnicast() || a.IsPrivate() {
			return false
		}
		for _, p := range reserved {
			if p.Contains(a) {
				return false
			}
		}
		return true
	}
	for v := uint64(0); v < 1<<32; v += 4099 {
		a := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		if PublicAddr(a) != old(a) {
			t.Fatalf("PublicAddr(%s) = %v, netip rules say %v", a, PublicAddr(a), old(a))
		}
	}
	// The edges of every block, both sides.
	for _, p := range NonPublicV4 {
		first := p.Masked().Addr()
		b := first.As4()
		host := uint32(1)<<(32-p.Bits()) - 1
		n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		last := netip.AddrFrom4([4]byte{byte((n + host) >> 24), byte((n + host) >> 16), byte((n + host) >> 8), byte(n + host)})
		for _, a := range []netip.Addr{first, last, first.Prev(), last.Next()} {
			if a.IsValid() && PublicAddr(a) != old(a) {
				t.Errorf("edge of %s: PublicAddr(%s) = %v, netip rules say %v", p, a, PublicAddr(a), old(a))
			}
		}
	}
	for _, p := range NonPublicV4 {
		if p.Bits() > 24 {
			t.Errorf("%s is narrower than a /24", p)
		}
	}
}

func TestPublicV4(t *testing.T) {
	for addr, want := range map[string]bool{"8.8.8.8": true, "10.1.2.3": false, "2a00:1450::1": false, "::ffff:8.8.8.8": true, "::ffff:10.0.0.1": false} {
		if got := PublicV4(netip.MustParseAddr(addr)); got != want {
			t.Errorf("PublicV4(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestIsCountryCode(t *testing.T) {
	for code, want := range map[string]bool{"DE": true, "US": true, "ZZ": true, "de": false, "D": false, "DEU": false, "": false, "1A": false, "D-": false} {
		if got := IsCountryCode(code); got != want {
			t.Errorf("IsCountryCode(%q) = %v, want %v", code, got, want)
		}
	}
}

func TestASNPrefixes(t *testing.T) {
	got := ASNPrefixes(`{"asn": 1, "metadata": {}, "prefixes": {"ipv4": ["45.10.0.1/24", " 8.8.8.0/24 ", "bad"], "ipv6": ["2a00::/32"]}}`)
	var texts []string
	for _, p := range got {
		texts = append(texts, p.String())
	}
	// As written, not masked; IPv4 first.
	if strings.Join(texts, " ") != "45.10.0.1/24 8.8.8.0/24 2a00::/32" {
		t.Errorf("prefixes = %v", texts)
	}
	for _, bad := range []string{"", "not json", `{"prefixes": 3}`} {
		if got := ASNPrefixes(bad); len(got) != 0 {
			t.Errorf("ASNPrefixes(%q) = %v", bad, got)
		}
	}
}

func TestSeenAtFilter(t *testing.T) {
	for _, part := range []string{"a.seen_at > now() - interval '7 days'", "a.seen_at IS NULL", "FROM public.asn WHERE seen_at IS NOT NULL"} {
		if !strings.Contains(SeenAtFilter, part) {
			t.Errorf("SeenAtFilter lacks %q", part)
		}
	}
}
