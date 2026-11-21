package main

import (
	"bufio"
	"net"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const tok = "0123456789abcdef0123456789abcdef"

// fakeRedis answers each connection with reply (after reading the command),
// or calls fn when set. It records the commands.
func fakeRedis(t *testing.T, reply string, fn func(net.Conn)) (addr string, cmds *[]string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var got []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if fn != nil {
					fn(c)
					return
				}
				br := bufio.NewReader(c)
				var cmd []string
				for i := 0; i < 7; i++ {
					l, _ := br.ReadString('\n')
					cmd = append(cmd, strings.TrimSpace(l))
				}
				got = append(got, strings.Join(cmd, " "))
				c.Write([]byte(reply))
			}()
		}
	}()
	return ln.Addr().String(), &got
}

func TestRedisHGet(t *testing.T) {
	for _, c := range []struct {
		name, reply string
		fn          func(net.Conn)
		val         string
		found       bool
		err         error
	}{
		{"value", "$10\r\n8.8.8.8xyz\r\n", nil, "8.8.8.8xyz", true, nil},
		{"null", "$-1\r\n", nil, "", false, nil},
		{"error reply", "-ERR wrong\r\n", nil, "", false, errRedisReply},
		{"integer", ":1\r\n", nil, "", false, errRedisReply},
		{"oversized", "$65\r\n" + strings.Repeat("a", 65) + "\r\n", nil, "", false, errRedisReply},
		{"bad length", "$x\r\n", nil, "", false, errRedisReply},
		{"huge line", "$" + strings.Repeat("1", 300) + "\r\n", nil, "", false, errRedisReply},
		{"short body", "$10\r\n8.8", nil, "", false, errRedisReply},
		{"closed", "", func(c net.Conn) { c.Close() }, "", false, errRedisReply},
		{"slow", "", func(c net.Conn) { time.Sleep(time.Second) }, "", false, errRedisTimeout},
	} {
		addr, _ := fakeRedis(t, c.reply, c.fn)
		rc := &redisClient{addr: addr}
		start := time.Now()
		v, ok, err := rc.hget("beacon:"+tok, "resolver_ip")
		if v != c.val || ok != c.found || err != c.err {
			t.Errorf("%s: %q %v %v", c.name, v, ok, err)
		}
		if time.Since(start) > 700*time.Millisecond {
			t.Errorf("%s: took %v", c.name, time.Since(start))
		}
	}
	// Nothing listens.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	if _, _, err := (&redisClient{addr: addr}).hget("k", "f"); err != errRedisConnect {
		t.Errorf("connect: %v", err)
	}
	// The command on the wire.
	addr, cmds := fakeRedis(t, "$-1\r\n", nil)
	(&redisClient{addr: addr}).hget("beacon:"+tok, "resolver_ip")
	want := "*3 $4 HGET $39 beacon:" + tok + " $11 resolver_ip"
	if len(*cmds) != 1 || (*cmds)[0] != want {
		t.Errorf("command %q, want %q", *cmds, want)
	}
}

func TestCheckRedisAddrAndZone(t *testing.T) {
	for v, ok := range map[string]bool{"127.0.0.1:6379": true, "[::1]:6379": true, "localhost:6379": true,
		"10.0.0.1:6379": false, "example.com:6379": false, "127.0.0.1": false, "127.0.0.1:0": false, "": false} {
		if (checkRedisAddr(v) == nil) != ok {
			t.Errorf("checkRedisAddr(%q)", v)
		}
	}
	for v, ok := range map[string]bool{"r.openipdata.org": true, "a-b.c1": true, "R.example.org": false, "a..b": false,
		".a.b": false, "a.b.": false, "-a.b": false, "a_b.c": false, "a b.c": false, "https://a.b": false, "a/b": false, "": false} {
		if (checkZone(v) == nil) != ok {
			t.Errorf("checkZone(%q)", v)
		}
	}
}

func beaconServer(t *testing.T, reply string) *server {
	t.Helper()
	addr, _ := fakeRedis(t, reply, nil)
	s := fixtureServer(t)
	s.useURLs(u4, u6)
	s.useBeacon("r.openipdata.org", addr)
	return s
}

func resolverGet(s *server, method, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func bulk(v string) string {
	return "$" + string(rune('0'+len(v)/10)) + string(rune('0'+len(v)%10)) + "\r\n" + v + "\r\n"
}

func TestResolverRoute(t *testing.T) {
	for _, c := range []struct {
		name, target, reply string
		code                int
		body                string
	}{
		{"bad token", "/resolver?t=xyz", "$-1\r\n", 400, "{}"},
		{"upper", "/resolver?t=" + strings.ToUpper(tok), "$-1\r\n", 400, "{}"},
		{"no token", "/resolver", "$-1\r\n", 400, "{}"},
		{"two tokens", "/resolver?t=" + tok + "&t=" + tok, "$-1\r\n", 400, "{}"},
		{"not yet", "/resolver?t=" + tok, "$-1\r\n", 404, "{}"},
		{"redis error", "/resolver?t=" + tok, "-ERR x\r\n", 503, "{}"},
		{"known", "/resolver?t=" + tok, bulk("203.0.113.9"), 200, `{"ip":"203.0.113.9","name":"Example Net"}`},
		{"unknown network", "/resolver?t=" + tok, bulk("8.8.8.8"), 200, `{"ip":"8.8.8.8"}`},
		{"private", "/resolver?t=" + tok, bulk("10.0.0.1"), 200, `{}`},
		{"garbage", "/resolver?t=" + tok, bulk("nope"), 200, `{}`},
	} {
		w := resolverGet(beaconServer(t, c.reply), "GET", c.target)
		if w.Code != c.code || w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body.String(), c.code, c.body)
		}
		h := w.Header()
		if h.Get("Cache-Control") != "no-store" || h.Get("Content-Type") != "application/json" || h.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: headers %v", c.name, h)
		}
		if strings.Contains(w.Body.String(), `"asn"`) {
			t.Errorf("%s: asn key", c.name)
		}
	}
	// With ASN data: the organisation page; a network without a slug: the network page.
	sa, _ := asnServer(t)
	sa.public = func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsPrivate() }
	addr, _ := fakeRedis(t, bulk("203.0.113.9"), nil)
	sa.useURLs(u4, u6)
	sa.useBeacon("r.openipdata.org", addr)
	if w := resolverGet(sa, "GET", "/resolver?t="+tok); w.Body.String() != `{"ip":"203.0.113.9","name":"EXAMPLE-NET","org":"/asn-example-net"}` {
		t.Errorf("org: %q", w.Body.String())
	}
	path := filepath.Join(t.TempDir(), "asn-openipdata.sqlite3")
	writeASNFile(t, path, []fixtureRow{{asn: 64500, handle: "EXAMPLE-NET", org: "Example Net", cc: "PT", v4: 512}})
	sn := beaconServer(t, bulk("203.0.113.9"))
	sn.useASN(path)
	if w := resolverGet(sn, "GET", "/resolver?t="+tok); w.Body.String() != `{"ip":"203.0.113.9","name":"EXAMPLE-NET","org":"/as64500"}` {
		t.Errorf("no slug: %q", w.Body.String())
	}
	// HEAD: same status and headers, no body; POST is refused.
	s := beaconServer(t, bulk("203.0.113.9"))
	if w := resolverGet(s, "HEAD", "/resolver?t="+tok); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("head: %d %q", w.Code, w.Body.String())
	}
	if w := resolverGet(s, "POST", "/resolver?t="+tok); w.Code != 405 {
		t.Errorf("post: %d", w.Code)
	}
}

func TestDNSRowScriptAndCSP(t *testing.T) {
	s := beaconServer(t, "$-1\r\n")
	w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
	b, c := w.Body.String(), w.Header().Get("Content-Security-Policy")
	if !strings.Contains(b, `<dt>DNS resolver</dt><dd id="dns" class="od" data-zone="r.openipdata.org">—</dd>`) {
		t.Errorf("row missing:\n%s", b)
	}
	if !strings.Contains(b, "<script>"+fillJS+"</script>\n<script>"+resolverJS+"</script>\n<script>"+panelJS+"</script>\n<script>"+shareJS+"</script>\n</body>") {
		t.Error("scripts not last in order fill, resolver, panel, share")
	}
	for _, x := range []string{scriptHash(headJS), scriptHash(themeJS), scriptHash(fillJS), scriptHash(resolverJS),
		"connect-src 'self' https://v4.openipdata.org https://v6.openipdata.org https://*.r.openipdata.org;", "default-src 'none'"} {
		if !strings.Contains(c, x) {
			t.Errorf("csp lacks %q: %s", x, c)
		}
	}
	if strings.Contains(b, "preconnect") || strings.Contains(b, "dns-prefetch") || strings.Contains(c, "unsafe-eval") {
		t.Error("hint or loose csp")
	}
	// Without the address URLs: the DNS script alone.
	s.useURLs("", "")
	w = fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
	c = w.Header().Get("Content-Security-Policy")
	if strings.Contains(c, scriptHash(fillJS)) || !strings.Contains(c, scriptHash(resolverJS)) || !strings.Contains(c, "connect-src 'self' https://*.r.openipdata.org;") ||
		!strings.Contains(w.Body.String(), "<script>"+resolverJS+"</script>\n<script>"+panelJS+"</script>\n<script>"+shareJS+"</script>\n</body>") {
		t.Errorf("dns only: %s", c)
	}
	// Pages without the row: no row, no script, the old policy.
	s.useURLs(u4, u6)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"lookup":  fromPeer(s, "/?203.0.113.9", "127.0.0.1:1"),
		"bad":     fromPeer(s, "/?1.2.3", "127.0.0.1:1"),
		"unknown": fromPeer(s, "/", "127.0.0.1:1"),
		"private": fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "10.0.0.1"),
		"asn":     fromPeer(s, "/as64500", "127.0.0.1:1"),
		"top":     fromPeer(s, "/top-asn-by-ip-address", "127.0.0.1:1"),
	} {
		c, b := w.Header().Get("Content-Security-Policy"), w.Body.String()
		if c != "" && (c != s.csp && c != s.cspPnl[0] || strings.Contains(c, scriptHash(resolverJS)) || strings.Contains(c, "*")) {
			t.Errorf("%s: csp %s", name, c)
		}
		if strings.Contains(b, `id="dns"`) || strings.Contains(b, resolverJS) || strings.Contains(b, "<dt>DNS resolver</dt>") {
			t.Errorf("%s: has the DNS row or script", name)
		}
	}
}

func TestBeaconOff(t *testing.T) {
	s := fillServer(t, u4, u6)
	s.useBeacon("", "127.0.0.1:1")
	w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
	if b := w.Body.String(); strings.Contains(b, "DNS") || strings.Contains(b, resolverJS) || strings.Contains(w.Header().Get("Content-Security-Policy"), "*") ||
		w.Header().Get("Content-Security-Policy") != s.cspPnl[5] { // fill script, panel.js, share.js: connect to self and the two address hosts
		t.Error("feature off but visible")
	}
	if w := resolverGet(s, "GET", "/resolver?t="+tok); w.Code != 404 {
		t.Errorf("route: %d", w.Code)
	}
	if s.csp != csp() {
		t.Error("base csp changed")
	}
}

func TestResolverScriptShape(t *testing.T) {
	for _, x := range []string{"AbortController", "3000", "textContent", "createElement", "DOMContentLoaded", `credentials: "omit"`,
		"crypto.getRandomValues", "k ? 30 : 8", "k++ ? stop() : go()", "i ? 2000 : 2200", "0-9a-fA-F:.", "45", "120"} {
		if !strings.Contains(resolverJS, x) {
			t.Errorf("resolverJS lacks %s", x)
		}
	}
	for _, x := range []string{"innerHTML", "document.write", "eval(", "XMLHttpRequest", "localStorage", "insertAdjacent"} {
		if strings.Contains(resolverJS, x) {
			t.Errorf("resolverJS has %s", x)
		}
	}
	if regexp.MustCompile(`[^\x20-\x7e\n]`).MatchString(resolverJS) {
		t.Error("non-ASCII in resolverJS")
	}
	if strings.Contains(resolverJS, "/resolver") {
		t.Error("resolverJS must read the answer of its own name only")
	}
	// One no-cors request: the trigger that only causes the DNS lookup (its answer is not read).
	if strings.Count(resolverJS, "no-cors") != 1 || !strings.Contains(resolverJS, `get("t", { mode: "no-cors" })`) {
		t.Error("resolverJS must send exactly one trigger request to <name>/t")
	}
}
