package main

import (
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

func TestNetworkNameLink(t *testing.T) {
	link := `<dd><a href="/asn-example-net">Example Net</a><a class="as" href="/as64500">AS64500</a></dd>`
	s, _ := asnServer(t)
	s.public = func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsPrivate() }
	for _, target := range []string{"/?203.0.113.9"} {
		if b := get(s, "GET", target).Body.String(); !strings.Contains(b, link) {
			t.Errorf("%s: name not linked", target)
		}
	}
	w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), link) {
		t.Errorf("visitor page: name not linked (%d)", w.Code)
	}

	// No ASN data: plain text.
	f := fixtureServer(t)
	if b := get(f, "GET", "/?203.0.113.9").Body.String(); !strings.Contains(b, `<dd>Example Net<span class="as">AS64500</span></dd>`) {
		t.Error("without data the name is not plain text")
	}

	// A network with no slug: plain text, AS link stays.
	path := filepath.Join(t.TempDir(), "asn-openipdata.sqlite3")
	writeASNFile(t, path, []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", cc: "PT", v4: 512}})
	n := fixtureServer(t)
	n.useASN(path)
	if b := get(n, "GET", "/?203.0.113.9").Body.String(); !strings.Contains(b, `<dd>Example Net<a class="as" href="/as64500">AS64500</a></dd>`) {
		t.Error("without a slug the name is not plain text")
	}
}
