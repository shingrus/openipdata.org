package main

import (
	"net/http"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The network rows of the connection panel: Organization (linked to its page
// with a valid slug, else plain; left out when unknown), Network (the handle and
// the AS number both linked to the network page; plain AS number when the network
// is not in the data), Prefix (linked) and Domain (the site, left out when unknown).
var dtRE = regexp.MustCompile(`<dt>([^<]+)</dt>`)

var rowRE = regexp.MustCompile(`<dt>[^<]+</dt><dd>.*?</dd>`)

func TestNetworkRowVariants(t *testing.T) {
	pub := func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsPrivate() }
	build := func(rows []fixtureRow) *server {
		path := filepath.Join(t.TempDir(), "asn-openipdata.sqlite3")
		writeASNFile(t, path, rows)
		s := fixtureServer(t)
		s.public = pub
		s.useASN(path)
		return s
	}
	// netRows are the rows after the address row, one per line.
	netRows := func(s *server) string {
		return netRowsOf(t, get(s, "GET", "/?203.0.113.9").Body.String())
	}
	const pfx = `<dt>Prefix</dt><dd><a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></dd>`
	const dom = `<dt>Domain</dt><dd><a href="https://example.net/?utm_source=openipdata.org" rel="nofollow noopener">example.net</a></dd>`
	for _, c := range []struct {
		name string
		rows []fixtureRow
		want string
	}{
		{"slug present: organization linked, handle and AS to the network, prefix, domain", []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", slug: "example-net", cc: "PT", v4: 512, category: "isp", domain: "example.net"}},
			`<dt>Organization</dt><dd><a href="/asn-example-net">Example Net</a></dd>
<dt>Network</dt><dd><a href="/as64500">EXAMPLE-NET</a> <a href="/as64500">AS64500</a></dd>
` + pfx + "\n" + dom},
		{"no slug: organization is plain text, no domain row", []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", cc: "PT", v4: 512}},
			`<dt>Organization</dt><dd>Example Net</dd>
<dt>Network</dt><dd><a href="/as64500">EXAMPLE-NET</a> <a href="/as64500">AS64500</a></dd>
` + pfx},
		{"category is not shown, the domain is", []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", cc: "PT", v4: 512, category: "cable_dsl_isp", domain: "example.net"}},
			`<dt>Organization</dt><dd>Example Net</dd>
<dt>Network</dt><dd><a href="/as64500">EXAMPLE-NET</a> <a href="/as64500">AS64500</a></dd>
` + pfx + "\n" + dom},
		{"invalid slug: organization is plain text", []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", slug: "Bad Slug", cc: "PT", v4: 512}},
			`<dt>Organization</dt><dd>Example Net</dd>
<dt>Network</dt><dd><a href="/as64500">EXAMPLE-NET</a> <a href="/as64500">AS64500</a></dd>
` + pfx},
		{"no handle: the MMDB text, plain AS number, prefix", []fixtureRow{{asn: 64500, noMetadata: true, noPrefixKeys: true}},
			`<dt>Organization</dt><dd>Example Net</dd>
<dt>Network</dt><dd>AS64500</dd>
` + pfx},
		{"network not in the data: the MMDB text, plain AS number, prefix", []fixtureRow{{asn: 64999, handle: "OTHER", org: "Other", cc: "PT", v4: 1}},
			`<dt>Organization</dt><dd>Example Net</dd>
<dt>Network</dt><dd>AS64500</dd>
` + pfx},
	} {
		if got := netRows(build(c.rows)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
	// No ASN data at all: the MMDB text, plain, the plain AS number, the MMDB tree prefix, no domain.
	f := fixtureServer(t)
	if got, want := netRowsOf(t, get(f, "GET", "/?203.0.113.9").Body.String()), "<dt>Organization</dt><dd>Example Net</dd>\n<dt>Network</dt><dd>AS64500</dd>\n"+pfx; got != want {
		t.Errorf("without data:\n got %s\nwant %s", got, want)
	}
	// An address the MMDB does not know: a dash, nothing else.
	if b := get(f, "GET", "/?192.0.2.1").Body.String(); !strings.Contains(b, `<dt>Network</dt><dd>—</dd>`) || strings.Contains(b, "<dt>Organization") || strings.Contains(b, "<dt>Prefix") {
		t.Error("unknown network is not a dash alone")
	}
}

// netRowsOf returns the rows of the panel from the first network row on
// (Organization, or Network when there is no organization), one per line.
func netRowsOf(t *testing.T, body string) string {
	t.Helper()
	dl := body[strings.Index(body, "<dl>"):]
	dl = dl[:strings.Index(dl, "</dl>")]
	var out []string
	on := false
	for _, r := range rowRE.FindAllString(dl, -1) {
		if strings.HasPrefix(r, "<dt>Organization") || strings.HasPrefix(r, "<dt>Network") {
			on = true
		}
		if on {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no network rows:\n%s", body)
	}
	return strings.Join(out, "\n")
}

// Rows as rendered: the visitor's page has IPv4, IPv6, Organization, Network, Prefix,
// Domain, DNS resolver, Browser; a lookup page has the address, Organization, Network,
// Prefix and Domain.
func TestPanelRowOrder(t *testing.T) {
	keys := func(body string) []string {
		var out []string
		for _, m := range dtRE.FindAllStringSubmatch(body[strings.Index(body, `<div class="conn`):], -1) {
			out = append(out, m[1])
		}
		return out
	}
	s, _ := asnServer(t)
	s.public = func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsPrivate() }
	s.useURLs(u4, u6)
	s.useBeacon("r.openipdata.org", "127.0.0.1:1")
	w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	b := w.Body.String()
	if got := strings.Join(keys(b), ","); got != "IPv4,IPv6,Organization,Network,Prefix,Domain,DNS resolver,Browser" {
		t.Errorf("visitor rows: %s", got)
	}
	if want := `<dt>Network</dt><dd><a href="/as64500">EXAMPLE-NET</a> <a href="/as64500">AS64500</a></dd>`; !strings.Contains(b, want) || !strings.Contains(b, `<dt>Prefix</dt><dd><a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></dd>`) {
		t.Error("the visitor page lacks the Network or the Prefix row")
	}
	if i, j := strings.Index(b, "<dl>"), strings.Index(b, `<div class="place`); j < 0 || i < j {
		t.Error("the place block is not before the rows")
	}
	b = get(s, "GET", "/?203.0.113.9").Body.String()
	if got := strings.Join(keys(b), ","); got != "IPv4,Organization,Network,Prefix,Domain" {
		t.Errorf("lookup rows: %s", got)
	}
	if got := strings.Join(keys(get(s, "GET", "/?2001:db8::5").Body.String()), ","); got != "IPv6,Organization,Network,Prefix,Domain" {
		t.Errorf("lookup v6 rows: %s", got)
	}
}
