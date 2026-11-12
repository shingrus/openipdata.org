package rdns

import (
	"net/netip"
	"testing"
)

func TestOriginLongestPrefix(t *testing.T) {
	o := newOriginTable()
	o.add(netip.MustParsePrefix("5.0.0.0/12"), 100)
	o.add(netip.MustParsePrefix("5.6.0.0/16"), 200)
	o.add(netip.MustParsePrefix("5.6.7.0/25"), 300)
	o.add(netip.MustParsePrefix("5.6.7.0/25"), 50) // the same prefix again: the first stays
	o.add(netip.MustParsePrefix("0.0.0.0/0"), 1)   // not a real announcement
	o.add(netip.MustParsePrefix("2001:db8::/32"), 2)

	for _, c := range []struct {
		addr   string
		prefix string
		asn    int64
	}{
		{"5.6.7.9", "5.6.7.0/25", 300},
		{"5.6.7.200", "5.6.0.0/16", 200},
		{"5.7.0.1", "5.0.0.0/12", 100},
	} {
		p, asn, ok := o.lookup(netip.MustParseAddr(c.addr))
		if !ok || p.String() != c.prefix || asn != c.asn {
			t.Errorf("lookup(%s) = %v %d %v, want %s %d", c.addr, p, asn, ok, c.prefix, c.asn)
		}
	}
	for _, addr := range []string{"6.0.0.1", "2001:db8::1"} {
		if _, _, ok := o.lookup(netip.MustParseAddr(addr)); ok {
			t.Errorf("lookup(%s) found a prefix", addr)
		}
	}
	// The prefix that contains a whole /24 is never longer than /24: the /25 does not.
	if p, asn, ok := o.containing(netip.MustParsePrefix("5.6.7.0/24")); !ok || p.String() != "5.6.0.0/16" || asn != 200 {
		t.Errorf("containing = %v %d %v", p, asn, ok)
	}
}

func TestOriginLowerASNWinsSamePrefix(t *testing.T) {
	// loadOrigin reads the ASNs in ASN order and add keeps the first.
	o := newOriginTable()
	o.add(netip.MustParsePrefix("5.6.0.0/16"), 100)
	o.add(netip.MustParsePrefix("5.6.0.0/16"), 200)
	if _, asn, _ := o.lookup(netip.MustParseAddr("5.6.1.1")); asn != 100 {
		t.Errorf("asn = %d", asn)
	}
}

func TestWidePrefix(t *testing.T) {
	o := newOriginTable()
	o.add(netip.MustParsePrefix("5.0.0.0/12"), 1)
	o.add(netip.MustParsePrefix("20.1.0.0/16"), 2)
	o.add(netip.MustParsePrefix("30.2.0.0/20"), 3)
	o.add(netip.MustParsePrefix("40.3.4.0/24"), 4)
	for _, c := range []struct{ p24, want string }{
		{"5.6.7.0/24", "5.6.0.0/16"},   // shorter than /16: the /16 around the /24
		{"20.1.9.0/24", "20.1.0.0/16"}, // exactly /16
		{"30.2.3.0/24", "30.2.0.0/20"}, // longer than /16: as announced
		{"40.3.4.0/24", "40.3.4.0/24"}, // the /24 itself
		{"50.0.0.0/24", ""},            // not announced
	} {
		got, ok := widePrefix(o, netip.MustParsePrefix(c.p24))
		if c.want == "" {
			if ok {
				t.Errorf("widePrefix(%s) = %v", c.p24, got)
			}
			continue
		}
		if !ok || got.String() != c.want {
			t.Errorf("widePrefix(%s) = %v %v, want %s", c.p24, got, ok, c.want)
		}
	}
}
