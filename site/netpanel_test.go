package main

import (
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

func TestNetPanelWords(t *testing.T) {
	for _, c := range []struct{ in, cat, role string }{
		{"isp", "ISP", "ISP"},
		{"education_research", "Education research", "Education research"},
		{"tier1_transit", "Tier1 transit", "Tier 1 transit"},
		{"access_provider", "Access provider", "Access provider"},
		{"stub", "Stub", "Stub"},
		{"", "", ""},
		{"  ", "", ""},
	} {
		if got := words(c.in); got != c.cat {
			t.Errorf("words(%q) = %q, want %q", c.in, got, c.cat)
		}
		if got := roleText(c.in); got != c.role {
			t.Errorf("roleText(%q) = %q, want %q", c.in, got, c.role)
		}
	}
}

func TestCompactCount(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{
		{0, "0"}, {999, "999"}, {1000, "1K"}, {1234, "1.2K"}, {1250, "1.3K"}, {12400000, "12.4M"},
		{1250000, "1.3M"}, {2000000, "2M"}, {999949, "999.9K"}, {999950, "1M"}, {1600000000, "1.6B"}, {-5, "0"},
	} {
		if got := compactCount(c.n); got != c.want {
			t.Errorf("compactCount(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func netServer(t *testing.T, rows []fixtureRow) *server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "asn-openipdata.sqlite3")
	writeASNFile(t, path, rows)
	s := fixtureServer(t)
	s.public = func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsPrivate() }
	s.useASN(path)
	return s
}

func TestNetworkPanelLookup(t *testing.T) {
	s := netServer(t, []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", slug: "example-net", cc: "PT", v4: 1250000,
		country: "Portugal", category: "isp", role: "tier1_transit", domain: "example.net",
		v4Prefixes: []string{"203.0.113.0/24", "198.51.100.0/24"}, v6Prefixes: []string{"2001:db8::/32"}}})
	b := get(s, "GET", "/?203.0.113.9").Body.String()
	want := `<section class="panel">
<div class="head"><h2>Network</h2><span class="r"><a href="/as64500">EXAMPLE-NET</a> <a href="/as64500">AS64500</a></span></div>
<div class="cells">
<div><span>Type</span><b>ISP</b></div>
<div><span>Country</span><b><a href="/portugal-asn-list">Portugal</a></b></div>
<div><span>Role</span><b>Tier 1 transit</b></div>
<div><span>Domain</span><b><a href="https://example.net/?utm_source=openipdata.org" rel="nofollow noopener">example.net</a></b></div>
<div><span>Prefix</span><b><a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></b></div>
<div><span>IPv4 addresses</span><b>1,250,000</b></div>
</div>
</section>`
	if !strings.Contains(b, want) {
		t.Errorf("no Network panel:\n%s", b)
	}
	// After the connection panel, before the API panel and Accuracy.
	i, j, k := strings.Index(b, `class="cells"`), strings.Index(b, `class="apip"`), strings.Index(b, "<h2>Accuracy</h2>")
	if !(0 < i && i < j && j < k) {
		t.Errorf("panel order: network %d, api %d, accuracy %d", i, j, k)
	}
	// The visitor's own page has it too.
	v := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9").Body.String()
	if !strings.Contains(v, `<div class="head"><h2>Network</h2><span class="r"><a href="/as64500">EXAMPLE-NET</a> <a href="/as64500">AS64500</a></span></div>`) {
		t.Error("visitor page has no Network panel")
	}
}

func TestNetworkPanelUnknowns(t *testing.T) {
	// No category, role, prefix lists or country, and no slug.
	s := netServer(t, []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", noMetadata: true, noPrefixKeys: true}})
	b := get(s, "GET", "/?203.0.113.9").Body.String()
	want := `<div><span>Type</span><b>—</b></div>
<div><span>Country</span><b>—</b></div>
<div><span>Role</span><b>—</b></div>
<div><span>Domain</span><b>—</b></div>
<div><span>Prefix</span><b>—</b></div>
<div><span>IPv4 addresses</span><b>0</b></div>`
	if !strings.Contains(b, want) {
		t.Errorf("unknown cells:\n%s", b)
	}
}

func TestNetworkPanelAbsent(t *testing.T) {
	// No ASN data at all.
	f := fixtureServer(t)
	f.public = func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsPrivate() }
	if b := get(f, "GET", "/?203.0.113.9").Body.String(); strings.Contains(b, `class="cells"`) {
		t.Error("Network panel without ASN data")
	}
	// The network of the address is not in the data.
	s := netServer(t, []fixtureRow{{asn: 64999, handle: "OTHER", org: "Other", cc: "PT", v4: 1}})
	if b := get(s, "GET", "/?203.0.113.9").Body.String(); strings.Contains(b, `class="cells"`) {
		t.Error("Network panel for a network not in the data")
	}
	// Non-public and bad queries, and an address the MMDB does not know.
	s = netServer(t, []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", cc: "PT", v4: 5}})
	for _, target := range []string{"/?10.0.0.1", "/?1.2.3.4.5", "/?192.0.2.1"} {
		if b := get(s, "GET", target).Body.String(); strings.Contains(b, `class="cells"`) {
			t.Errorf("%s: Network panel", target)
		}
	}
	// A visitor with an unknown address.
	if b := get(s, "GET", "/").Body.String(); strings.Contains(b, "<h2>Network</h2>") {
		t.Error("Network panel for an unknown visitor")
	}
	// Not on the other pages.
	if b := get(s, "GET", "/as64500").Body.String(); strings.Contains(b, `<h2>Network</h2>`) { // its own head is a grid of cells too
		t.Error("Network panel on the network page")
	}
}

func TestVisitorRowOrderWithDNS(t *testing.T) {
	s := netServer(t, []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", slug: "example-net", cc: "PT", v4: 512}})
	s.useURLs(u4, u6)
	s.useBeacon("r.openipdata.org", "127.0.0.1:1")
	b := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9").Body.String()
	last := -1
	for _, k := range []string{"IPv4", "IPv6", "Organization", "DNS resolver", "Browser"} {
		i := strings.Index(b, "<dt>"+k+"</dt>")
		if i <= last {
			t.Fatalf("row %s at %d, after %d", k, i, last)
		}
		last = i
	}
}

func TestResolverSmallMarkup(t *testing.T) {
	if !strings.Contains(resolverJS, `createElement("small")`) || strings.Contains(resolverJS, `"dn"`) {
		t.Error("resolver.js does not build the small element")
	}
}
