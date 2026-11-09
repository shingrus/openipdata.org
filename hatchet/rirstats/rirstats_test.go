package rirstats

import (
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	stats := "2|arin|20261109|4|19700101|20261109|-0500\n" +
		"# a comment\n" +
		"arin|*|ipv4|*|3|summary\n" +
		"arin|US|ipv4|45.10.0.0|1024|20100101|allocated|abc\n" +
		"arin|ca|ipv4|45.20.0.0|768|20100101|assigned|abc\n" +
		"arin|US|ipv4|45.30.1.0|512|20100101|assigned|abc\n" + // not aligned
		"arin|US|ipv6|2600:1000::|28|20100101|allocated|abc\n" +
		"arin|US|ipv4|45.40.0.0|256|20100101|reserved|abc\n" +
		"arin|US|ipv4|255.255.255.0|512|20100101|allocated|abc\n" + // runs past the end of the space
		"arin|US|ipv4|45.50.0.0|0|20100101|allocated|abc\n" +
		"arin|US|ipv6|2600:2000::|129|20100101|allocated|abc\n" +
		"arin|US|asn|64512|1|20100101|allocated|abc\n" +
		"arin||ipv4|45.60.0.0|256|20100101|allocated|abc\n" +
		"iana|ZZ|ipv4|0.0.0.0|4294967296|20100101|assigned|abc\n"
	var got []string
	err := Parse(strings.NewReader(stats), func(country string, prefixes []netip.Prefix) {
		got = append(got, fmt.Sprintf("%s %v", country, prefixes))
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"US [45.10.0.0/22]",
		"ca [45.20.0.0/23 45.20.2.0/24]",
		"US [45.30.1.0/24 45.30.2.0/24]",
		"US [2600:1000::/28]",
		" [45.60.0.0/24]",
		"ZZ [0.0.0.0/0]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blocks:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRangeToPrefixes(t *testing.T) {
	tests := []struct {
		from, to string
		want     []string
	}{
		{"0.0.0.0", "255.255.255.255", []string{"0.0.0.0/0"}},
		{"255.255.255.255", "255.255.255.255", []string{"255.255.255.255/32"}},
		{"192.0.2.0", "192.0.3.127", []string{"192.0.2.0/24", "192.0.3.0/25"}},
		{"2001:db8::", "2001:db8::ffff", []string{"2001:db8::/112"}},
		{"::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", []string{"::/0"}},
	}
	for _, tt := range tests {
		var got []string
		for _, p := range RangeToPrefixes(netip.MustParseAddr(tt.from), netip.MustParseAddr(tt.to)) {
			got = append(got, p.String())
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s - %s: got %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestLastAddr(t *testing.T) {
	for prefix, want := range map[string]string{
		"45.10.0.0/22":   "45.10.3.255",
		"0.0.0.0/0":      "255.255.255.255",
		"45.10.0.7/32":   "45.10.0.7",
		"2001:db8::/32":  "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff",
		"2001:db8::1/64": "2001:db8::ffff:ffff:ffff:ffff",
	} {
		if got := LastAddr(netip.MustParsePrefix(prefix)).String(); got != want {
			t.Errorf("LastAddr(%s) = %s, want %s", prefix, got, want)
		}
	}
}
