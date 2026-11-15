package main

import (
	"net/http"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Network row: the handle links to the organisation page (the network page
// without a valid slug), then the announced prefix as a link; no category, no
// domain; with no handle the MMDB text stands in; with nothing, a dash.
var dtRE = regexp.MustCompile(`<dt>([^<]+)</dt>`)

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
	netRow := func(s *server) string {
		b := get(s, "GET", "/?203.0.113.9").Body.String()
		i := strings.Index(b, "<dt>Network</dt>")
		if i < 0 {
			t.Fatalf("no Network row:\n%s", b)
		}
		rest := b[i:]
		return rest[:strings.Index(rest, "</dd>")+len("</dd>")]
	}
	for _, c := range []struct {
		name string
		rows []fixtureRow
		want string
	}{
		{"slug present: handle links to the organisation, then the prefix", []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", slug: "example-net", cc: "PT", v4: 512, category: "isp", domain: "example.net"}},
			`<dt>Network</dt><dd><a href="/asn-example-net">EXAMPLE-NET</a> · <a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></dd>`},
		{"no slug: handle links to the network", []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", cc: "PT", v4: 512}},
			`<dt>Network</dt><dd><a href="/as64500">EXAMPLE-NET</a> · <a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></dd>`},
		{"category and domain are not shown", []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", cc: "PT", v4: 512, category: "cable_dsl_isp", domain: "example.net"}},
			`<dt>Network</dt><dd><a href="/as64500">EXAMPLE-NET</a> · <a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></dd>`},
		{"invalid slug: handle links to the network", []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", slug: "Bad Slug", cc: "PT", v4: 512}},
			`<dt>Network</dt><dd><a href="/as64500">EXAMPLE-NET</a> · <a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></dd>`},
		{"no handle: the MMDB text, then the prefix", []fixtureRow{{asn: 64500, noMetadata: true, noPrefixKeys: true}},
			`<dt>Network</dt><dd>Example Net · <a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></dd>`},
		{"network not in the data: the MMDB text, then the prefix", []fixtureRow{{asn: 64999, handle: "OTHER", org: "Other", cc: "PT", v4: 1}},
			`<dt>Network</dt><dd>Example Net · <a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></dd>`},
	} {
		if got := netRow(build(c.rows)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
	// No ASN data at all: the MMDB text, plain, then the MMDB tree prefix.
	f := fixtureServer(t)
	if b := get(f, "GET", "/?203.0.113.9").Body.String(); !strings.Contains(b, `<dt>Network</dt><dd>Example Net · <a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></dd>`) || strings.Contains(b, "AS64500") {
		t.Error("without data the Network row is not the plain MMDB text and the prefix")
	}
	// An address the MMDB does not know: a dash.
	if b := get(f, "GET", "/?192.0.2.1").Body.String(); !strings.Contains(b, `<dt>Network</dt><dd>—</dd>`) {
		t.Error("unknown network is not a dash")
	}
}

// Rows as rendered: the visitor's page has IPv4, IPv6, DNS resolver, Network
// (no Prefix row, no AS number); a lookup page has the address and Network.
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
	if got := strings.Join(keys(b), ","); got != "IPv4,IPv6,DNS resolver,Network,Browser" {
		t.Errorf("visitor rows: %s", got)
	}
	if strings.Contains(b, "AS64500") || strings.Contains(b, "<dt>Prefix") {
		t.Error("the visitor page has an AS number or a Prefix row")
	}
	if i, j := strings.Index(b, "<dl>"), strings.Index(b, `<div class="place`); j < 0 || i < j {
		t.Error("the place block is not before the rows")
	}
	b = get(s, "GET", "/?203.0.113.9").Body.String()
	if got := strings.Join(keys(b), ","); got != "IPv4,Network" {
		t.Errorf("lookup rows: %s", got)
	}
	if got := strings.Join(keys(get(s, "GET", "/?2001:db8::5").Body.String()), ","); got != "IPv6,Network" {
		t.Errorf("lookup v6 rows: %s", got)
	}
	if strings.Contains(b, "<dt>Prefix") {
		t.Error("lookup page has a Prefix row")
	}
}
