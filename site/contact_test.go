package main

import (
	"net/netip"
	"strings"
	"testing"
)

func TestPlainDomain(t *testing.T) {
	for in, want := range map[string]string{
		"example.net": "example.net", " Example.NET ": "example.net", "a-b.example.co.uk": "a-b.example.co.uk", "xn--e1afmkfd.xn--p1ai": "xn--e1afmkfd.xn--p1ai",
		"": "", "example": "", "example.n": "", "example.123": "", "-a.example.net": "", "a-.example.net": "", "a..example.net": "",
		"exa mple.net": "", `bad"x.example`: "", "example.net/path": "", "https://example.net": "", "javascript:alert(1)": "", "a_b.example.net": "",
		strings.Repeat("a", 64) + ".example.net": "", strings.Repeat("a.", 127) + "example": "",
	} {
		if got := plainDomain(in); got != want {
			t.Errorf("plainDomain(%q) = %q, want %q", in, got, want)
		}
	}
	if kv := domainKV(`bad"x.example`); kv.Href != "" || kv.V != `bad"x.example` {
		t.Errorf("an odd domain is linked: %+v", kv)
	}
	if kv := domainKV(""); kv.Href != "" || kv.V != dash {
		t.Errorf("no domain: %+v", kv)
	}
}

// A prefix page shows the domain of the first announcing network as a link,
// between the Network and Addresses rows; a prefix that is only covered has none.
func TestPrefixDomain(t *testing.T) {
	s, _ := asnServer(t)
	s.public = func(a netip.Addr) bool { return true }
	b := get(s, "GET", "/prefix/203.0.113.0/24").Body.String()
	dom := `<span>Domain</span><b><a href="https://example.net/?utm_source=openipdata.org" rel="nofollow noopener">example.net</a></b>`
	n, d, x := strings.Index(b, "<span>Network</span>"), strings.Index(b, dom), strings.Index(b, "<span>Addresses</span>")
	if n < 0 || d < n || x < d {
		t.Errorf("rows Network, Domain, Addresses are not there in this order (%d %d %d)", n, d, x)
	}
	if b := get(s, "GET", "/prefix/203.0.113.0/25").Body.String(); strings.Contains(b, "<span>Domain</span>") || !strings.Contains(b, "Part of") {
		t.Error("a covered prefix has a Domain row, or is not a covered prefix page")
	}
}

// A prefix announced exactly lists the other networks of the organization and
// the other prefixes of its network (not itself); a covered prefix has neither.
func TestPrefixRelated(t *testing.T) {
	s, _ := asnServer(t)
	s.public = func(a netip.Addr) bool { return true }
	b := get(s, "GET", "/prefix/203.0.113.0/24").Body.String()
	for _, want := range []string{"<h2>Same organization</h2>", `<td class="a"><a href="/as64501">AS64501</a></td>`, "<h2>Other prefixes of AS64500</h2>"} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	_, list, _ := strings.Cut(b, "Other prefixes of AS64500")
	if strings.Contains(list, `href="/prefix/203.0.113.0/24"`) || !strings.Contains(list, `<li><a href="/prefix/`) {
		t.Error("the list of other prefixes has the page's own prefix, or no prefix")
	}
	if b := get(s, "GET", "/prefix/203.0.113.0/25").Body.String(); strings.Contains(b, "Same organization") || strings.Contains(b, "Other prefixes") {
		t.Error("a covered prefix lists networks or prefixes")
	}
}

// The IPv6 size of an organization: short below 1e15 /64 networks, the full
// number above (the short form would overflow), singular for exactly one.
func TestV6KV(t *testing.T) {
	const net64 = 18446744073709551616.0 // 2^64 addresses
	for _, c := range []struct {
		v6          float64
		want, title string
	}{
		{0, "0 /64 networks", ""},
		{1, "<1 /64 networks", ""},
		{net64, "1 /64 network", ""},
		{999 * net64, "999 /64 networks", ""},
		{65536 * net64, "65.5K /64 networks", "65,536 /64 networks"},
		{4518725 * net64, "4.5M /64 networks", "4,518,725 /64 networks"},
	} {
		if got := v6KV(c.v6); got.V != c.want || got.Title != c.title {
			t.Errorf("v6KV(%g) = %q, title %q; want %q, %q", c.v6, got.V, got.Title, c.want, c.title)
		}
	}
	for _, v6 := range []float64{1e15 * net64, 2.1267647932558654e37, 8.507059173023462e37} { // up to 2^126 addresses
		if got := v6KV(v6); strings.Contains(got.V, "-") || got.Title != "" || !strings.HasSuffix(got.V, " /64 networks") {
			t.Errorf("v6KV(%g) = %q, title %q", v6, got.V, got.Title)
		}
	}
}
