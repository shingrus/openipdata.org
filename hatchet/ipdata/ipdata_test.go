package ipdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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

// reducedJSON mirrors PrefixesJSON in Go for a valid JSON object: only the prefixes member stays,
// null when there is none.
func reducedJSON(t *testing.T, whole string) string {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(whole), &doc); err != nil {
		t.Fatal(err)
	}
	p, ok := doc["prefixes"]
	if !ok {
		p = json.RawMessage("null")
	}
	out, _ := json.Marshal(map[string]json.RawMessage{"prefixes": p})
	return string(out)
}

// The reduced document PrefixesJSON builds gives the same prefixes as the whole one, and the
// shapes without prefixes give none.
func TestASNPrefixesReducedShapes(t *testing.T) {
	for name, c := range map[string]struct {
		whole string
		want  string
	}{
		"both families": {`{"asn": 1, "metadata": {"x": [1]}, "prefixes": {"ipv4": ["45.10.0.1/24", "bad"], "ipv6": ["2a00::/32"]}}`, "45.10.0.1/24 2a00::/32"},
		"ipv4 only":     {`{"prefixes": {"ipv4": ["8.8.8.0/24"]}}`, "8.8.8.0/24"},
		"empty object":  {`{"asn": 1, "prefixes": {}}`, ""},
		"null":          {`{"asn": 1, "prefixes": null}`, ""},
		"missing":       {`{"asn": 1, "metadata": {}}`, ""},
		"not an object": {`{"asn": 1, "prefixes": 3}`, ""},
	} {
		texts := func(raw string) string {
			var s []string
			for _, p := range ASNPrefixes(raw) {
				s = append(s, p.String())
			}
			return strings.Join(s, " ")
		}
		reduced := reducedJSON(t, c.whole)
		if got, whole := texts(reduced), texts(c.whole); got != c.want || whole != c.want {
			t.Errorf("%s: reduced %q whole %q, want %q", name, got, whole, c.want)
		}
	}
	// What the CASE gives for a value that is no object.
	if got := ASNPrefixes(""); len(got) != 0 {
		t.Errorf("empty string: %v", got)
	}
}

func TestPrefixesJSONText(t *testing.T) {
	for _, part := range []string{"a.json LIKE '{%'", "jsonb_build_object('prefixes'", "-> 'prefixes'", "ELSE ''"} {
		if !strings.Contains(PrefixesJSON, part) {
			t.Errorf("PrefixesJSON lacks %q: %s", part, PrefixesJSON)
		}
	}
	if strings.Contains(PrefixesJSON, "pg_input_is_valid") {
		t.Error("PrefixesJSON needs Postgres 16")
	}
}

func TestWithJSONFallback(t *testing.T) {
	damaged := &pgconn.PgError{Code: "22P02"}
	other := &pgconn.PgError{Code: "42P01"}
	for name, c := range map[string]struct {
		errs  []error // the error of each pass
		want  []string
		final error
	}{
		"first pass fine":         {[]error{nil}, []string{"reduced"}, nil},
		"damaged, then fine":      {[]error{fmt.Errorf("read: %w", damaged), nil}, []string{"reduced", "whole"}, nil},
		"a NUL escape, then fine": {[]error{&pgconn.PgError{Code: "22P05"}, nil}, []string{"reduced", "whole"}, nil},
		"a number out of range":   {[]error{&pgconn.PgError{Code: "22003"}, nil}, []string{"reduced", "whole"}, nil},
		"nesting too deep":        {[]error{&pgconn.PgError{Code: "54001"}, nil}, []string{"reduced", "whole"}, nil},
		"damaged twice":           {[]error{damaged, damaged}, []string{"reduced", "whole"}, damaged},
		"other error, no retry":   {[]error{other}, []string{"reduced"}, other},
		"plain error, no retry":   {[]error{errors.New("boom")}, []string{"reduced"}, nil},
	} {
		var queries []string
		err := WithJSONFallback("reduced", "whole", func(q string) error {
			queries = append(queries, q)
			return c.errs[len(queries)-1]
		})
		if strings.Join(queries, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: queries %v, want %v", name, queries, c.want)
		}
		if c.final != nil && !errors.Is(err, c.final) {
			t.Errorf("%s: err = %v, want %v", name, err, c.final)
		}
		if c.final == nil && (err != nil) != (c.errs[len(c.errs)-1] != nil) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
