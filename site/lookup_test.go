package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"1.1.1.1": true, "8.8.8.8": true, "93.184.216.34": true, "172.15.255.255": true, "172.32.0.1": true,
		"100.63.255.255": true, "100.128.0.1": true, "198.17.0.1": true, "198.20.0.1": true,
		"2606:4700:4700::1111": true, "2a00:1450:4001::1": true, "2002:808:808::1": true,
		"::ffff:8.8.8.8": true, // mapped: the IPv4 address inside
		// not public
		"0.0.0.0": false, "0.1.2.3": false, "10.1.2.3": false, "127.0.0.1": false, "127.255.255.254": false,
		"100.64.0.1": false, "100.127.255.255": false, "169.254.1.1": false, "172.16.0.1": false,
		"172.31.255.255": false, "192.0.0.8": false, "192.0.2.1": false, "192.88.99.1": false,
		"192.168.1.1": false, "198.18.0.1": false, "198.19.255.255": false, "198.51.100.7": false,
		"203.0.113.9": false, "224.0.0.1": false, "239.255.255.250": false, "240.0.0.1": false,
		"255.255.255.255": false,
		"::":              false, "::1": false, "fe80::1": false, "fc00::1": false, "fd12:3456::1": false,
		"ff02::1": false, "2001:db8::1": false, "2001::1": false, "2001:10::1": false, "3fff::1": false,
		"64:ff9b::1.2.3.4": false, "100::1": false, "::2": false,
		"::ffff:10.0.0.1": false, "::ffff:127.0.0.1": false,
	} {
		if got := isPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isPublic(%s) = %v, want %v", addr, got, want)
		}
	}
	if isPublic(netip.Addr{}) || isPublic(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("invalid or zoned address is public")
	}
}

func TestClientAddr(t *testing.T) {
	for _, c := range []struct {
		name, remote, header string
		hdr                  []string // values of X-Real-IP
		want                 string   // "" = unknown
	}{
		{"direct", "198.51.100.5:4000", "", nil, "198.51.100.5"},
		{"direct v6", "[2001:db8::5]:4000", "", nil, "2001:db8::5"},
		{"direct mapped", "[::ffff:198.51.100.5]:4000", "", nil, "198.51.100.5"},
		{"header not configured is ignored", "127.0.0.1:4000", "", []string{"203.0.113.9"}, "127.0.0.1"},
		{"trusted header", "127.0.0.1:4000", "X-Real-IP", []string{"203.0.113.9"}, "203.0.113.9"},
		{"trusted header v6", "[::1]:4000", "X-Real-IP", []string{"2001:db8::9"}, "2001:db8::9"},
		{"trusted header mapped", "127.0.0.1:4000", "X-Real-IP", []string{"::ffff:203.0.113.9"}, "203.0.113.9"},
		{"trusted header with spaces", "127.0.0.1:4000", "X-Real-IP", []string{" 203.0.113.9 "}, "203.0.113.9"},
		{"header from a non-loopback peer is ignored", "198.51.100.5:4000", "X-Real-IP", []string{"203.0.113.9"}, "198.51.100.5"},
		{"missing header", "127.0.0.1:4000", "X-Real-IP", nil, ""},
		{"bad header", "127.0.0.1:4000", "X-Real-IP", []string{"not-an-ip"}, ""},
		{"empty header", "127.0.0.1:4000", "X-Real-IP", []string{""}, ""},
		{"list in header", "127.0.0.1:4000", "X-Real-IP", []string{"203.0.113.9, 198.51.100.1"}, ""},
		{"zone in header", "127.0.0.1:4000", "X-Real-IP", []string{"fe80::1%eth0"}, ""},
		{"two headers", "127.0.0.1:4000", "X-Real-IP", []string{"203.0.113.9", "198.51.100.1"}, ""},
		{"private peer with a header set is unknown", "10.0.0.7:4000", "X-Real-IP", []string{"203.0.113.9"}, ""},
		{"link-local peer with a header set is unknown", "169.254.1.2:4000", "X-Real-IP", nil, ""},
		{"private peer without a header is the peer", "10.0.0.7:4000", "", nil, "10.0.0.7"},
		{"bad peer", "garbage", "", nil, ""},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.hdr {
			r.Header.Add("X-Real-IP", v)
		}
		a, ok := clientAddr(r, c.header)
		if got := a.String(); (c.want == "") == ok || (ok && got != c.want) {
			t.Errorf("%s: got %q ok=%v, want %q", c.name, got, ok, c.want)
		}
	}
}

func TestParseQuery(t *testing.T) {
	long := strings.Repeat("1", 200)
	for _, c := range []struct {
		raw, kind, addr string
	}{
		{"", "visitor", ""},
		{"1.1.1.1", "lookup", "1.1.1.1"},
		{"2001:db8::1", "lookup", "2001:db8::1"},
		{"2001%3Adb8%3A%3A1", "redirect", "2001:db8::1"},
		{"2001:DB8:0:0:0:0:0:1", "redirect", "2001:db8::1"},
		{"::ffff:1.2.3.4", "redirect", "1.2.3.4"},
		{"1.1.1.1%20", "redirect", "1.1.1.1"},
		{"q=1.1.1.1", "redirect", "1.1.1.1"},
		{"q=2001%3Adb8%3A%3A1", "redirect", "2001:db8::1"},
		{"q=+1.1.1.1+", "redirect", "1.1.1.1"},
		{"q=", "redirect", ""}, // the empty lookup box: back to the home page
		{"q=++", "redirect", ""},
		{"q=15169", "asn", ""},
		{"q=example.com", "search", ""},
		{"x=1.1.1.1", "visitor", ""}, // not our parameter: the home page
		{"15169", "asn", ""},
		{"example.com", "search", ""},
		{"1.1.1.1:80", "bad", ""},
		{"1.1.1.1/24", "bad", ""},
		{"fe80::1%25eth0", "bad", ""},
		{"1.1.1", "bad", ""},
		{"%zz", "bad", ""},
		{long, "bad", ""},
		{"q=" + long, "bad", ""},
	} {
		q := parseQuery(c.raw)
		if q.kind != c.kind || (c.addr != "" && q.addr.String() != c.addr) {
			t.Errorf("%q: got %s %s, want %s %s", c.raw, q.kind, q.addr, c.kind, c.addr)
		}
		if q.kind == "bad" && len([]rune(q.input)) > maxInputLen {
			t.Errorf("%q: input not cut: %d", c.raw, len(q.input))
		}
	}
}

// publicAll lets the tests use the documentation addresses of the fixtures.
func fixtureServer(t *testing.T) *server {
	t.Helper()
	s, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	s.public = func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsPrivate() }
	return s
}

func fromPeer(s http.Handler, target, remote string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	r.RemoteAddr = remote
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestPanelStates(t *testing.T) {
	s := fixtureServer(t)
	for _, c := range []struct {
		name, target string
		code         int
		want, not    []string
	}{
		{"city", "/?203.0.113.9", 200,
			[]string{"<h2>Lookup</h2>", "<h1>Lisbon</h1>", "<p>Portugal</p>", "<dt>Network</dt>", "Example Net", `<span class="as">AS64500</span>`,
				"<dt>IPv4</dt><dd>203.0.113.9</dd>", "<dt>Prefix</dt><dd>203.0.113.0/24</dd>", `value="203.0.113.9"`}, []string{"Your connection", "Location unknown"}},
		{"city v6", "/?2001:db8::5", 200,
			[]string{"<h1>Lisbon</h1>", "<dt>IPv6</dt><dd>2001:db8::5</dd>", "<dt>Prefix</dt><dd>2001:db8::/32</dd>", "AS64500"}, nil},
		{"IPv4", "/?203.0.113.9", 200, []string{"<dt>IPv4</dt><dd>203.0.113.9</dd>", `value="203.0.113.9"`}, []string{"::ffff"}},
		{"country only", "/?198.51.100.1", 200,
			[]string{"<h1>Spain</h1>", `<div class="place mapped"><svg class="map"`, "<dt>Network</dt><dd>—</dd>", "<dt>Prefix</dt><dd>—</dd>"}, []string{"Lisbon", "city unknown", "<p>"}},
		{"nothing", "/?192.0.2.1", 200,
			[]string{`<h1>Location unknown</h1>`, `class="place none"`, "<dt>Network</dt><dd>—</dd>", "<dt>IPv4</dt><dd>192.0.2.1</dd>"}, []string{"city unknown"}},
		{"not public", "/?10.1.2.3", 200,
			[]string{"10.1.2.3", "Not a public address", `value="10.1.2.3"`}, []string{"<dt>", "Prefix"}},
		{"not an IP", "/?1.2.3", 400,
			[]string{"<h2>Lookup</h2>", "Not an IP address, AS number or name", `value="1.2.3"`}, []string{"<dt>", "Not a public"}},
		{"escaped", "/?%3Cscript%3Ealert(1)%3C/script%3E:", 400,
			[]string{"Not an IP address", "&lt;script&gt;"}, []string{"<script>alert"}},
	} {
		w := get(s, "GET", c.target)
		if w.Code != c.code {
			t.Errorf("%s: status %d", c.name, w.Code)
		}
		body := w.Body.String()
		for _, x := range c.want {
			if !strings.Contains(body, x) {
				t.Errorf("%s: page lacks %q", c.name, x)
			}
		}
		for _, x := range c.not {
			if strings.Contains(body, x) {
				t.Errorf("%s: page has %q", c.name, x)
			}
		}
		if !strings.Contains(body, "<h2>Accuracy</h2>") {
			t.Errorf("%s: Accuracy panel missing", c.name)
		}
	}

	// Long junk is cut before it goes back into the input.
	long := strings.Repeat("1.", 45) // looks like an address, so it is "bad", not a name
	w := get(s, "GET", "/?"+long)
	if w.Code != 400 || strings.Contains(w.Body.String(), long) || !strings.Contains(w.Body.String(), long[:maxInputLen]) {
		t.Errorf("long input: %d", w.Code)
	}
}

func TestVisitorPanel(t *testing.T) {
	s := fixtureServer(t)
	// Behind nginx: peer is loopback, the header names the visitor.
	w := fromPeer(s, "/", "127.0.0.1:5000", "X-Real-IP", "203.0.113.9")
	body := w.Body.String()
	for _, x := range []string{"<h2>Your connection</h2>", "<h1>Lisbon</h1>", `value="203.0.113.9"`, "AS64500"} {
		if !strings.Contains(body, x) {
			t.Errorf("visitor page lacks %q", x)
		}
	}
	// A forged header from a direct, non-loopback peer is ignored.
	w = fromPeer(s, "/", "198.51.100.1:5000", "X-Real-IP", "203.0.113.9")
	if b := w.Body.String(); !strings.Contains(b, "<h1>Spain</h1>") || strings.Contains(b, "203.0.113.9") {
		t.Error("untrusted header was used")
	}
	// Loopback with no header: unknown, never 127.0.0.1.
	w = fromPeer(s, "/", "127.0.0.1:5000")
	if b := w.Body.String(); !strings.Contains(b, "Address unknown") || strings.Contains(b, "127.0.0.1") {
		t.Errorf("missing header: %s", b)
	}
	// Loopback peer, no trusted header setting: the loopback address is not public.
	real, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "")
	if err != nil {
		t.Fatal(err)
	}
	w = fromPeer(real, "/", "127.0.0.1:5000", "X-Real-IP", "203.0.113.9")
	if b := w.Body.String(); !strings.Contains(b, "Not a public address") || strings.Contains(b, "203.0.113.9") {
		t.Errorf("loopback: %s", b)
	}
}

func TestHomeVersusLookupHeaders(t *testing.T) {
	s := fixtureServer(t)
	for _, c := range []struct {
		name, target, remote, cache string
		code                        int
	}{
		{"home", "/", "127.0.0.1:1", "private, no-store", 200},
		{"home unknown visitor", "/", "[::1]:1", "private, no-store", 200},
		{"home empty query", "/?", "127.0.0.1:1", "private, no-store", 200},
		{"lookup", "/?203.0.113.9", "127.0.0.1:1", "public, max-age=300", 200},
		{"lookup not public", "/?10.0.0.1", "127.0.0.1:1", "public, max-age=300", 200},
		{"bad", "/?1.2.3", "127.0.0.1:1", "no-store", 400},
		{"redirect", "/?q=203.0.113.9", "127.0.0.1:1", "public, max-age=300", 302},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			r := httptest.NewRequest(method, c.target, nil)
			r.RemoteAddr = c.remote
			r.Header.Set("X-Real-IP", "203.0.113.9")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			h := w.Header()
			if w.Code != c.code || h.Get("Cache-Control") != c.cache {
				t.Errorf("%s %s: %d %q", method, c.name, w.Code, h.Get("Cache-Control"))
			}
			if h.Get("ETag") != "" {
				t.Errorf("%s %s: ETag %q", method, c.name, h.Get("ETag"))
			}
			if method == "HEAD" && w.Body.Len() != 0 {
				t.Errorf("HEAD %s: body", c.name)
			}
			if method == "GET" && c.code != 302 && w.Body.Len() == 0 {
				t.Errorf("GET %s: empty body", c.name)
			}
		}
	}
	// A lookup page never holds the visitor's own address.
	w := fromPeer(s, "/?198.51.100.1", "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
	if strings.Contains(w.Body.String(), "203.0.113.9") {
		t.Error("lookup page shows the visitor address")
	}
	// The CSP still allows only the form target 'self' and nothing new.
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self'") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("csp: %s", csp)
	}
}

func TestQRedirect(t *testing.T) {
	s := fixtureServer(t)
	for q, loc := range map[string]string{
		"q=203.0.113.9":                        "/?203.0.113.9",
		"q=2001%3ADB8%3A0%3A0%3A0%3A0%3A0%3A1": "/?2001:db8::1",
		"q=+::ffff:1.2.3.4":                    "/?1.2.3.4",
		"::ffff:203.0.113.9":                   "/?203.0.113.9",
	} {
		w := get(s, "GET", "/?"+q)
		if w.Code != 302 || w.Header().Get("Location") != loc {
			t.Errorf("%s: %d %q, want %q", q, w.Code, w.Header().Get("Location"), loc)
		}
	}
	// Another spelling of an address redirects the same way.
	w := get(s, "GET", "/?2001:DB8::1")
	if w.Code != 302 || w.Header().Get("Location") != "/?2001:db8::1" || w.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Errorf("non-canonical: %d %q %q", w.Code, w.Header().Get("Location"), w.Header().Get("Cache-Control"))
	}
	// The redirect target is a lookup page again.
	if w := get(s, "GET", "/?2001:db8::1"); w.Code != 200 {
		t.Errorf("canonical: %d", w.Code)
	}
}

func TestLookupFilesMissing(t *testing.T) {
	dir := t.TempDir()
	s, err := newServer(filepath.Join(dir, "none-geo.mmdb"), filepath.Join(dir, "none-asn.mmdb"), "")
	if err != nil {
		t.Fatal(err)
	}
	s.public = func(netip.Addr) bool { return true }
	w := get(s, "GET", "/?203.0.113.9")
	body := w.Body.String()
	for _, x := range []string{"Location unknown", "<dt>Network</dt><dd>—</dd>", "<dt>Prefix</dt><dd>—</dd>", "<dt>IPv4</dt><dd>203.0.113.9</dd>"} {
		if w.Code != 200 || !strings.Contains(body, x) {
			t.Errorf("no files: %d lacks %q", w.Code, x)
		}
	}
	// Garbage files: the same.
	bad := filepath.Join(dir, "bad.mmdb")
	os.WriteFile(bad, []byte("junk"), 0o644)
	s2, err := newServer(bad, bad, "")
	if err != nil {
		t.Fatal(err)
	}
	s2.public = s.public
	if !strings.Contains(get(s2, "GET", "/?203.0.113.9").Body.String(), "Location unknown") {
		t.Error("garbage files: want unknowns")
	}
}

func TestLookupReloadPicksUpNewFile(t *testing.T) {
	dir := t.TempDir()
	geo, asn := filepath.Join(dir, "geo.mmdb"), filepath.Join(dir, "asn.mmdb")
	s, err := newServer(geo, asn, "")
	if err != nil {
		t.Fatal(err)
	}
	s.public = func(netip.Addr) bool { return true }
	if !strings.Contains(get(s, "GET", "/?203.0.113.9").Body.String(), "Location unknown") {
		t.Fatal("want unknown first")
	}
	copyFile(t, "testdata/lookup-geo.mmdb", geo)
	copyFile(t, "testdata/lookup-asn.mmdb", asn)
	s.check()
	if b := get(s, "GET", "/?203.0.113.9").Body.String(); !strings.Contains(b, "Lisbon") || !strings.Contains(b, "AS64500") {
		t.Error("new files not picked up")
	}
	// A broken replacement keeps the open reader.
	// (replaced by rename, like the puller does: a file is never edited in place)
	os.WriteFile(asn+".new", []byte("broken"), 0o644)
	os.Rename(asn+".new", asn)
	os.Chtimes(asn, time.Now(), time.Now().Add(time.Minute))
	s.check()
	if !strings.Contains(get(s, "GET", "/?203.0.113.9").Body.String(), "AS64500") {
		t.Error("broken file dropped the reader")
	}
}

// Swapping readers while requests look up must be race free (go test -race)
// and must never fail a lookup or use a closed reader.
func TestReaderSwapDuringLookups(t *testing.T) {
	dir := t.TempDir()
	geo, asn := filepath.Join(dir, "geo.mmdb"), filepath.Join(dir, "asn.mmdb")
	copyFile(t, "testdata/lookup-geo.mmdb", geo)
	copyFile(t, "testdata/lookup-asn.mmdb", asn)
	s, err := newServer(geo, asn, "")
	if err != nil {
		t.Fatal(err)
	}
	s.public = func(netip.Addr) bool { return true }

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				w := get(s, "GET", "/?203.0.113.9")
				if b := w.Body.String(); w.Code != 200 || !strings.Contains(b, "Lisbon") || !strings.Contains(b, "AS64500") {
					t.Error("lookup failed during a swap")
					return
				}
			}
		}()
	}
	for i := 0; i < 40; i++ {
		for _, p := range []string{geo, asn} {
			copyFile(t, "testdata/lookup-"+map[bool]string{true: "geo", false: "asn"}[p == geo]+".mmdb", p+".new")
			if err := os.Rename(p+".new", p); err != nil {
				t.Fatal(err)
			}
			os.Chtimes(p, time.Now(), time.Now().Add(time.Duration(i+1)*time.Second))
		}
		s.check()
	}
	close(stop)
	wg.Wait()
}

func TestEmptyBoxAndTitles(t *testing.T) {
	s, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	s.public = func(netip.Addr) bool { return true }

	w := get(s, "GET", "/?q=")
	if w.Code != 302 || w.Header().Get("Location") != "/" {
		t.Errorf("empty box: status %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	for target, title := range map[string]string{
		"/":             "<title>openipdata.org</title>",
		"/?203.0.113.9": "<title>203.0.113.9 · openipdata.org</title>",
		"/?1.2.3":       "<title>openipdata.org</title>",
	} {
		if body := get(s, "GET", target).Body.String(); !strings.Contains(body, title) {
			t.Errorf("%s: page lacks %q", target, title)
		}
	}
}
