package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func fillServer(t *testing.T, v4, v6 string) *server {
	t.Helper()
	s := fixtureServer(t)
	s.useURLs(v4, v6)
	return s
}

const (
	u4 = "https://v4.openipdata.org/"
	u6 = "https://v6.openipdata.org/"
)

func TestIPEndpoint(t *testing.T) {
	s := fixtureServer(t)
	for _, c := range []struct {
		name, method, remote string
		hdr                  []string
		code                 int
		body                 string
	}{
		{"v4", "GET", "127.0.0.1:1", []string{"X-Real-IP", "203.0.113.9"}, 200, "203.0.113.9\n"},
		{"v6 canonical", "GET", "127.0.0.1:1", []string{"X-Real-IP", "2001:DB8:0:0::1"}, 200, "2001:db8::1\n"},
		{"mapped", "GET", "127.0.0.1:1", []string{"X-Real-IP", "::ffff:203.0.113.9"}, 200, "203.0.113.9\n"},
		{"direct peer", "GET", "198.51.100.1:1", []string{"X-Real-IP", "203.0.113.9"}, 200, "198.51.100.1\n"},
		{"unknown", "GET", "127.0.0.1:1", nil, 503, "unknown\n"},
		{"invalid", "GET", "127.0.0.1:1", []string{"X-Real-IP", "nope"}, 503, "unknown\n"},
		{"head", "HEAD", "127.0.0.1:1", []string{"X-Real-IP", "203.0.113.9"}, 200, ""},
		{"post", "POST", "127.0.0.1:1", []string{"X-Real-IP", "203.0.113.9"}, 405, "method not allowed\n"},
	} {
		r, _ := http.NewRequest(c.method, "/ip", nil)
		r.RemoteAddr = c.remote
		for i := 0; i+1 < len(c.hdr); i += 2 {
			r.Header.Set(c.hdr[i], c.hdr[i+1])
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != c.code || w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body.String(), c.code, c.body)
		}
		if c.code == 405 {
			continue
		}
		h := w.Header()
		for k, v := range map[string]string{
			"Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store",
			"X-Content-Type-Options": "nosniff", "Access-Control-Allow-Origin": "*",
		} {
			if h.Get(k) != v {
				t.Errorf("%s: %s = %q", c.name, k, h.Get(k))
			}
		}
	}
}

func TestVisitorRows(t *testing.T) {
	s := fillServer(t, u4, u6)
	// An IPv4 visitor: IPv4 filled by the server, IPv6 a dash with the IPv6 URL.
	b := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9").Body.String()
	i4 := strings.Index(b, "<dt>IPv4</dt>")
	i6 := strings.Index(b, "<dt>IPv6</dt>")
	if i4 < 0 || i6 < i4 {
		t.Fatalf("rows IPv4, IPv6 not in order: %d %d", i4, i6)
	}
	if !strings.Contains(b, "<dt>IPv4</dt><dd class=\"c\"><span class=\"w\"><span class=\"v\">203.0.113.9</span><button class=\"cp\"") ||
		!strings.Contains(b, `<dt>IPv6</dt><dd id="other" class="c o6" data-fam="6" data-from="`+u6+`"><span class="w"><span class="v">—</span><button class="cp" type="button" hidden`) {
		t.Errorf("v4 visitor rows wrong:\n%s", b)
	}
	// An IPv6 visitor: IPv4 is the dash row.
	b = fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "2001:db8::9").Body.String()
	if !strings.Contains(b, "<dt>IPv6</dt><dd class=\"c\"><span class=\"w\"><span class=\"v\">2001:db8::9</span><button class=\"cp\"") ||
		!strings.Contains(b, `<dt>IPv4</dt><dd id="other" class="c o4" data-fam="4" data-from="`+u4+`"><span class="w"><span class="v">—</span><button class="cp" type="button" hidden`) ||
		strings.Index(b, "<dt>IPv4</dt>") > strings.Index(b, "<dt>IPv6</dt>") {
		t.Errorf("v6 visitor rows wrong:\n%s", b)
	}
	if strings.Count(b, `id="other"`) != 1 {
		t.Error("more than one row to fill")
	}
}

func TestFillScriptAndCSP(t *testing.T) {
	s := fillServer(t, u4, u6)
	w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
	b, c := w.Body.String(), w.Header().Get("Content-Security-Policy")
	if !strings.Contains(b, "<script>"+fillJS+"</script>\n<script>"+panelJS+"</script>\n</body>") {
		t.Error("fill script and panel script are not the last things in the body")
	}
	if strings.Index(b, fillJS) < strings.Index(b, themeJS) || strings.Contains(b[:strings.Index(b, "</head>")], "fetch(") {
		t.Error("fill script before the click script or in the head")
	}
	for _, x := range []string{scriptHash(headJS), scriptHash(themeJS), scriptHash(fillJS),
		"connect-src https://v4.openipdata.org https://v6.openipdata.org;", "default-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(c, x) {
			t.Errorf("csp lacks %q: %s", x, c)
		}
	}
	if strings.Contains(c, "unsafe-eval") || strings.Contains(c, "script-src 'unsafe") || strings.Contains(c, "*") {
		t.Errorf("csp too loose: %s", c)
	}
	// No hints in the head for the two hosts.
	if strings.Contains(b, "preconnect") || strings.Contains(b, "dns-prefetch") {
		t.Error("connection hint")
	}

	// Pages without a row to fill: the CSP has no script hash and no connect-src.
	for name, w := range map[string]*httptest.ResponseRecorder{
		"lookup":  fromPeer(s, "/?203.0.113.9", "127.0.0.1:1"),
		"bad":     fromPeer(s, "/?1.2.3", "127.0.0.1:1"),
		"unknown": fromPeer(s, "/", "127.0.0.1:1"),
		"private": fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "10.0.0.1"),
	} {
		c, b := w.Header().Get("Content-Security-Policy"), w.Body.String()
		if strings.Contains(c, "connect-src") || strings.Contains(c, scriptHash(fillJS)) {
			t.Errorf("%s: csp %s", name, c)
		}
		if strings.Contains(b, "fetch(") || strings.Contains(b, `id="other"`) {
			t.Errorf("%s: has the fill script or row", name)
		}
	}
	// The lookup page of an address keeps exactly one family row.
	b = fromPeer(s, "/?203.0.113.9", "127.0.0.1:1").Body.String()
	if strings.Contains(b, "<dt>IPv6</dt>") || !strings.Contains(b, "<dt>IPv4</dt>") {
		t.Error("lookup page rows")
	}
}

func TestFillOnlyOneURL(t *testing.T) {
	s := fillServer(t, u4, "")
	// IPv6 visitor: needs the IPv4 URL, present.
	w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "2001:db8::9")
	if !strings.Contains(w.Body.String(), `id="other"`) || !strings.Contains(w.Header().Get("Content-Security-Policy"), "connect-src https://v4.openipdata.org;") {
		t.Error("v6 visitor should get the IPv4 row")
	}
	// IPv4 visitor: the IPv6 URL is empty, so the page is as before.
	w = fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
	if b := w.Body.String(); strings.Contains(b, "<dt>IPv6</dt>") || strings.Contains(b, "fetch(") || strings.Contains(w.Header().Get("Content-Security-Policy"), "connect-src") {
		t.Error("v4 visitor with no V6_URL must see the old page")
	}
}

func TestEmptyURLsPageAsToday(t *testing.T) {
	for _, s := range []*server{fixtureServer(t), fillServer(t, "", "")} {
		w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
		b, c := w.Body.String(), w.Header().Get("Content-Security-Policy")
		if strings.Contains(b, "fetch(") || strings.Contains(b, `id="other"`) || strings.Contains(b, "<dt>IPv6</dt>") {
			t.Error("page differs from today")
		}
		if c != s.cspPnl[0] || strings.Contains(c, "connect-src") {
			t.Errorf("csp: %s", c)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for v, ok := range map[string]bool{
		"": true, "https://v4.openipdata.org/": true, "https://v4.openipdata.org": true, "https://example.com:8443/x": true,
		"http://127.0.0.1:8080/ip": true, "http://localhost/": true,
		"http://v4.openipdata.org/": false, "ftp://example.com/": false, "v4.openipdata.org": false, "//v4.openipdata.org/": false,
		"https://": false, "https://u:p@example.com/": false, "https://example.com/#x": false, "https://exa mple.com/": false,
		"https://example.com/\"": false, "javascript:alert(1)": false, "http://127.0.0.1.evil.com/": false, "/ip": false,
	} {
		if err := checkURL(v); (err == nil) != ok {
			t.Errorf("checkURL(%q) = %v, want ok=%v", v, err, ok)
		}
	}
}

// The script is a constant: no request data, and its answers are checked in
// the script; here only that it keeps its safety limits.
func TestFillScriptShape(t *testing.T) {
	for _, x := range []string{"AbortController", "4000", "textContent", "DOMContentLoaded", "credentials: \"omit\""} {
		if !strings.Contains(fillJS, x) {
			t.Errorf("fillJS lacks %s", x)
		}
	}
	for _, x := range []string{"innerHTML", "document.write", "eval(", "XMLHttpRequest", "localStorage"} {
		if strings.Contains(fillJS, x) {
			t.Errorf("fillJS has %s", x)
		}
	}
	if regexp.MustCompile(`[^\x20-\x7e\n]`).MatchString(fillJS) {
		t.Error("non-ASCII in fillJS")
	}
}

// The second-address row is placed by the family row, not by the row count:
// an unrelated row before, between or after changes nothing.
func TestAddFillIgnoresOtherRows(t *testing.T) {
	s := fillServer(t, u4, u6)
	for _, c := range []struct {
		addr string
		want []string
	}{
		{"203.0.113.9", []string{"X", "IPv4", "IPv6", "Network", "Y", "Prefix"}},
		{"2001:db8::9", []string{"X", "IPv4", "IPv6", "Network", "Y", "Prefix"}},
	} {
		a := netip.MustParseAddr(c.addr)
		p := s.panelFor("Your connection", a)
		p.Rows = append([]row{{K: "X", V: "x"}}, p.Rows...)
		p.Rows = slices.Insert(p.Rows, 3, row{K: "Y", V: "y"})
		if !s.addFill(&p, a) {
			t.Fatalf("%s: no row added", c.addr)
		}
		var got []string
		for _, r := range p.Rows {
			got = append(got, r.K)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: rows %v, want %v", c.addr, got, c.want)
		}
	}
	// No family row (a message panel or a panel without it): nothing is added.
	p := panel{Rows: []row{{K: "Network"}}}
	if s.addFill(&p, netip.MustParseAddr("203.0.113.9")) || len(p.Rows) != 1 {
		t.Error("row added without a family row")
	}
}

// Both URLs on one origin: connect-src names it once.
func TestConnectSrcOnce(t *testing.T) {
	s := fillServer(t, "https://x.example/4", "https://x.example/6")
	if c := s.cspFill; strings.Count(c, "https://x.example") != 1 || !strings.Contains(c, "connect-src https://x.example;") {
		t.Errorf("csp: %s", c)
	}
}

// An empty script file stops the program at start instead of giving a page without the script.
func TestScriptTextRejectsEmpty(t *testing.T) {
	for _, src := range []string{"", "\n  \n", "// only a comment\n"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("scriptText(%q) did not panic", src)
				}
			}()
			scriptText("x.js", src)
		}()
	}
	if got := scriptText("x.js", "// c\nvar a = 1;\n"); got != "var a = 1;" {
		t.Errorf("scriptText = %q", got)
	}
}

// The visitor page with the fill script, with a map (Lisbon fixture) and
// without one (an address the geo fixture does not know), stays under the size
// limits of TestMapSizes (see the numbers there). Logged: 19849 and 17967 bytes
// (7212 and 6314 gzipped) on 2026-11-15, after the copy and share work.
func TestVisitorPageSize(t *testing.T) {
	s := fillServer(t, u4, u6)
	for _, c := range []struct {
		addr     string
		max, gzm int
	}{{"203.0.113.9", visitorMapMaxRaw, visitorMapMaxGz}, {"192.0.2.9", visitorMaxRaw, visitorMaxGz}} {
		body := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", c.addr).Body.Bytes()
		t.Logf("visitor page for %s: %d bytes, %d gzipped", c.addr, len(body), gzipped(body))
		if len(body) > c.max || gzipped(body) > c.gzm {
			t.Errorf("visitor page for %s is %d bytes (limit %d), %d gzipped (limit %d)", c.addr, len(body), c.max, gzipped(body), c.gzm)
		}
	}
}
