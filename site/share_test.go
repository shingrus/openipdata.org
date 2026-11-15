package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// memRedis is a small Redis for the share tests: SET (EX, XX, KEEPTTL), GET and
// HGET, one command per connection, and a record of every command.
type memRedis struct {
	mu   sync.Mutex
	kv   map[string]string
	ttl  map[string]int // seconds; -1 = none
	hash map[string]map[string]string
	cmds [][]string
	addr string
}

func newMemRedis(t *testing.T) *memRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &memRedis{kv: map[string]string{}, ttl: map[string]int{}, hash: map[string]map[string]string{}, addr: ln.Addr().String()}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
	return m
}

func readCmd(br *bufio.Reader) []string {
	line, err := br.ReadString('\n')
	if err != nil || len(line) < 3 || line[0] != '*' {
		return nil
	}
	n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	var args []string
	for i := 0; i < n; i++ {
		l, err := br.ReadString('\n')
		if err != nil {
			return nil
		}
		size, _ := strconv.Atoi(strings.TrimSpace(l[1:]))
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil
		}
		args = append(args, string(buf[:size]))
	}
	return args
}

func (m *memRedis) serve(c net.Conn) {
	defer c.Close()
	args := readCmd(bufio.NewReader(c))
	if len(args) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cmds = append(m.cmds, args)
	bulk := func(v string, ok bool) string {
		if !ok {
			return "$-1\r\n"
		}
		return "$" + strconv.Itoa(len(v)) + "\r\n" + v + "\r\n"
	}
	switch strings.ToUpper(args[0]) {
	case "SET":
		key, val := args[1], args[2]
		_, exists := m.kv[key]
		ttl, xx, keep := -1, false, false
		for i := 3; i < len(args); i++ {
			switch strings.ToUpper(args[i]) {
			case "EX":
				i++
				ttl, _ = strconv.Atoi(args[i])
			case "XX":
				xx = true
			case "KEEPTTL":
				keep = true
			}
		}
		if xx && !exists {
			io.WriteString(c, "$-1\r\n")
			return
		}
		if keep {
			ttl = m.ttl[key]
		}
		m.kv[key], m.ttl[key] = val, ttl
		io.WriteString(c, "+OK\r\n")
	case "GET":
		v, ok := m.kv[args[1]]
		io.WriteString(c, bulk(v, ok))
	case "HGET":
		v, ok := m.hash[args[1]][args[2]]
		io.WriteString(c, bulk(v, ok))
	default:
		io.WriteString(c, "-ERR unknown\r\n")
	}
}

func (m *memRedis) commands(name string) [][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out [][]string
	for _, c := range m.cmds {
		if strings.EqualFold(c[0], name) {
			out = append(out, c)
		}
	}
	return out
}

func (m *memRedis) keys(prefix string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.kv {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

func (m *memRedis) get(k string) (string, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kv[k], m.ttl[k]
}

func (m *memRedis) snapshot(t *testing.T, id string) snapshot {
	t.Helper()
	v, _ := m.get("report:" + id)
	var sn snapshot
	if err := json.Unmarshal([]byte(v), &sn); err != nil {
		t.Fatalf("report %s: %v", "<id>", err)
	}
	return sn
}

// shareServer is a server with ASN data, both address URLs, the DNS row and a
// memory Redis; its clock is fixed.
func shareServer(t *testing.T) (*server, *memRedis) {
	t.Helper()
	s, _ := asnServer(t)
	s.public = func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsPrivate() }
	m := newMemRedis(t)
	s.useURLs(u4, u6)
	s.useBeacon("r.openipdata.org", m.addr)
	s.now = func() time.Time { return time.Date(2026, 11, 15, 16, 50, 12, 0, time.UTC) }
	return s, m
}

// postShare sends POST /share as the visitor ip (through the loopback proxy
// peer) with the X-Share header and an optional form body.
func postShare(s http.Handler, ip, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/share", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Share", "1")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if ip != "" {
		r.Header.Set("X-Real-IP", ip)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			r.Header.Del(hdr[i])
		} else {
			r.Header.Set(hdr[i], hdr[i+1])
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// share makes a report as the visitor ip and returns its id.
func share(t *testing.T, s http.Handler, ip, body string) string {
	t.Helper()
	w := postShare(s, ip, body)
	var j struct{ URL, ID string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &j) != nil || len(j.ID) != 32 {
		t.Fatalf("share: %d %s", w.Code, w.Body.String())
	}
	return j.ID
}

// captureLog collects what the standard logger writes during a test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestRedisSetAndGet(t *testing.T) {
	m := newMemRedis(t)
	c := &redisClient{addr: m.addr}
	if err := c.setEX("report:x", `{"a":1}`, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if v, ttl := m.get("report:x"); v != `{"a":1}` || ttl != 2592000 {
		t.Errorf("stored %q ttl %d", v, ttl)
	}
	if got := strings.Join(m.commands("SET")[0], " "); got != `SET report:x {"a":1} EX 2592000` {
		t.Errorf("wire: %s", got)
	}
	if v, ok, err := c.get("report:x", 100); v != `{"a":1}` || !ok || err != nil {
		t.Errorf("get: %q %v %v", v, ok, err)
	}
	if _, ok, err := c.get("report:none", 100); ok || err != nil {
		t.Errorf("get none: %v %v", ok, err)
	}
	if _, _, err := c.get("report:x", 3); err != errRedisReply {
		t.Errorf("get over the limit: %v", err)
	}
	// Replace keeps the lifetime, and never creates a key.
	if ok, err := c.replaceKeepTTL("report:x", `{"a":2}`); !ok || err != nil {
		t.Errorf("replace: %v %v", ok, err)
	}
	if v, ttl := m.get("report:x"); v != `{"a":2}` || ttl != 2592000 {
		t.Errorf("after replace %q ttl %d", v, ttl)
	}
	if got := strings.Join(m.commands("SET")[1], " "); got != `SET report:x {"a":2} XX KEEPTTL` {
		t.Errorf("wire: %s", got)
	}
	if ok, err := c.replaceKeepTTL("report:gone", "x"); ok || err != nil {
		t.Errorf("replace of a missing key: %v %v", ok, err)
	}
	if len(m.keys("report:gone")) != 0 {
		t.Error("a missing key was created")
	}
	// An error reply is a fixed kind of error.
	bad, _ := fakeRedis(t, "-ERR no\r\n", nil)
	if err := (&redisClient{addr: bad}).setEX("k", "v", time.Second); err != errRedisReply {
		t.Errorf("error reply: %v", err)
	}
}

// A page view does no Redis work at all: no read, no write.
func TestHomeViewMakesNoRedisCall(t *testing.T) {
	s, m := shareServer(t)
	for _, target := range []string{"/", "/?203.0.113.9", "/as64500", "/top-asn-by-ip-address"} {
		w := fromPeer(s, target, "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
		b := w.Body.String()
		if w.Code != 200 || strings.Contains(b, "data-token") || strings.Contains(b, "?r=") || strings.Contains(b, "data-url") {
			t.Errorf("%s: status %d or old attributes", target, w.Code)
		}
	}
	r := httptest.NewRequest("HEAD", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Real-IP", "203.0.113.9")
	s.ServeHTTP(httptest.NewRecorder(), r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.cmds) != 0 || len(m.kv) != 0 {
		t.Errorf("page views used Redis: %v", m.cmds)
	}
}

func TestShareButtonPresence(t *testing.T) {
	s, _ := shareServer(t)
	btn := `<button class="sh" type="button" hidden>Share</button>`
	b := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9").Body.String()
	if !strings.Contains(b, btn) {
		t.Error("no hidden Share button on the visitor page")
	}
	for name, target := range map[string]string{"lookup": "/?203.0.113.9", "bad": "/?1.2.3", "asn": "/as64500", "top": "/top-asn-by-ip-address", "status": "/status"} {
		if strings.Contains(get(s, "GET", target).Body.String(), `class="sh"`) {
			t.Errorf("%s: Share button", name)
		}
	}
	if strings.Contains(fromPeer(s, "/", "127.0.0.1:1").Body.String(), `class="sh"`) { // address unknown
		t.Error("button on the unknown-address page")
	}
	if strings.Contains(fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "10.0.0.1").Body.String(), `class="sh"`) { // not public
		t.Error("button for a private address")
	}
	s.useShare(false, "")
	if strings.Contains(fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9").Body.String(), `class="sh"`) {
		t.Error("button with SHARE=off")
	}
}

func TestShareCSP(t *testing.T) {
	s, _ := shareServer(t)
	cspOf := func(target, ip string) string {
		return fromPeer(s, target, "127.0.0.1:1", "X-Real-IP", ip).Header().Get("Content-Security-Policy")
	}
	if c := cspOf("/", "203.0.113.9"); !strings.Contains(c, "connect-src 'self' "+"https://v4.openipdata.org") || !strings.Contains(c, "https://*.r.openipdata.org") {
		t.Errorf("visitor page CSP: %s", c)
	}
	for _, c := range []string{cspOf("/?203.0.113.9", "203.0.113.9"), cspOf("/status", "203.0.113.9"), cspOf("/", "10.0.0.1"),
		get(s, "GET", "/download").Header().Get("Content-Security-Policy")} {
		if strings.Contains(c, "'self'") && strings.Contains(c, "connect-src") {
			t.Errorf("'self' in connect-src of a page without the button: %s", c)
		}
	}
	// Without both addresses and the DNS row, the button page still gets 'self'.
	s2, _ := shareServer(t)
	s2.useURLs("", "")
	s2.useBeacon("", s2.redis.addr)
	if c := fromPeer(s2, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9").Header().Get("Content-Security-Policy"); !strings.Contains(c, "connect-src 'self';") {
		t.Errorf("button page CSP: %s", c)
	}
}

func TestShareEndpoint(t *testing.T) {
	logs := captureLog(t)
	s, m := shareServer(t)
	m.hash["beacon:"+tok] = map[string]string{"resolver_ip": "203.0.113.50"}
	m.hash["beacon:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"] = map[string]string{"resolver_ip": "10.1.1.1"}

	// A normal share with the page's token.
	w := postShare(s, "203.0.113.9", "t="+tok)
	h := w.Header()
	var j struct{ URL, ID string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &j) != nil || j.URL != "https://openipdata.org/report/"+j.ID || len(j.ID) != 32 ||
		h.Get("Cache-Control") != "no-store" || !strings.HasPrefix(h.Get("Content-Type"), "application/json") ||
		h.Get("Access-Control-Allow-Origin") != "" || h.Get("Access-Control-Allow-Headers") != "" {
		t.Fatalf("share: %d %v %s", w.Code, h, w.Body.String())
	}
	v, ttl := m.get("report:" + j.ID)
	if ttl != 2592000 {
		t.Errorf("report expires in %d s", ttl)
	}
	sn := m.snapshot(t, j.ID)
	if sn.Created != "2026-11-15T16:50:12Z" || sn.IP != "203.0.113.9" || sn.Family != 4 || sn.Other != "" ||
		sn.Place.City != "Lisbon" || sn.Place.Country != "Portugal" || sn.Place.CC != "PT" || sn.Place.Lat == nil || sn.Place.Lon == nil ||
		sn.Network.ASN != 64500 || sn.Network.Handle != "EXAMPLE-NET" || sn.Network.Slug != "example-net" || sn.Network.Prefix != "203.0.113.0/24" || sn.Network.Org != "Example Net" ||
		sn.Resolver == nil || sn.Resolver.IP != "203.0.113.50" || sn.Resolver.Name != "EXAMPLE-NET" || sn.Resolver.Org != "/asn-example-net" {
		t.Errorf("snapshot: %+v", sn)
	}
	if cmds := m.commands("SET"); len(cmds) != 1 || len(m.kv) != 1 {
		t.Errorf("writes: %d, keys %d", len(cmds), len(m.kv))
	}
	for _, bad := range []string{"User-Agent", "Mozilla", "X-Share", "X-Real-IP", "headers", "t="} {
		if strings.Contains(v, bad) {
			t.Errorf("snapshot has %q", bad)
		}
	}
	// Each press makes its own report; no token = no resolver.
	w2 := postShare(s, "2001:db8::5", "")
	var j2 struct{ URL, ID string }
	json.Unmarshal(w2.Body.Bytes(), &j2)
	if sn2 := m.snapshot(t, j2.ID); w2.Code != 200 || j2.ID == j.ID || sn2.Resolver != nil || sn2.Family != 6 || sn2.IP != "2001:db8::5" {
		t.Errorf("second share: %d %+v", w2.Code, sn2)
	}
	// A missing, malformed, doubled or private-resolver token adds no resolver; the query works too.
	for name, c := range map[string]struct {
		target, body string
		want         bool
	}{
		"missing":  {"/share", "t=fedcba9876543210fedcba9876543210", false},
		"bad":      {"/share", "t=xyz", false},
		"upper":    {"/share", "t=" + strings.ToUpper(tok), false},
		"doubled":  {"/share", "t=" + tok + "&t=" + tok, false},
		"private":  {"/share", "t=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		"in query": {"/share?t=" + tok, "", true},
		"junk":     {"/share", "x=1&t=" + tok + "&ip=8.8.8.8", true},
	} {
		r := httptest.NewRequest("POST", c.target, strings.NewReader(c.body))
		r.RemoteAddr = "127.0.0.1:1"
		r.Header.Set("X-Share", "1")
		r.Header.Set("X-Real-IP", "203.0.113.9")
		rw := httptest.NewRecorder()
		s.ServeHTTP(rw, r)
		var jj struct{ ID string }
		json.Unmarshal(rw.Body.Bytes(), &jj)
		if rw.Code != 200 || (m.snapshot(t, jj.ID).Resolver != nil) != c.want {
			t.Errorf("%s: %d, resolver %v, want %v", name, rw.Code, m.snapshot(t, jj.ID).Resolver != nil, c.want)
		}
	}
	// The body is never stored: nothing the request carries but the token reaches Redis.
	m.mu.Lock()
	for k, val := range m.kv {
		if strings.Contains(val, "8.8.8.8") || strings.Contains(val, "x=1") || strings.Contains(k, "8.8.8.8") {
			t.Errorf("request body stored in %s", k[:7])
		}
	}
	m.mu.Unlock()
	// The visitor's address comes from the usual rules only: a body or query ip does nothing.
	if sn := m.snapshot(t, share(t, s, "203.0.113.9", "ip=8.8.8.8&q=8.8.8.8")); sn.IP != "203.0.113.9" {
		t.Errorf("address %s", sn.IP)
	}
	if strings.Contains(logs.String(), j.ID) || strings.Contains(logs.String(), tok) || strings.Contains(logs.String(), "203.0.113.9") || strings.Contains(logs.String(), "203.0.113.50") {
		t.Errorf("an id, token or address in the log:\n%s", logs)
	}
}

func TestShareEndpointRefusals(t *testing.T) {
	s, m := shareServer(t)
	for _, method := range []string{"GET", "HEAD", "PUT", "DELETE"} {
		w := get(s, method, "/share")
		if w.Code != 405 || w.Header().Get("Allow") != "POST" {
			t.Errorf("%s /share: %d %v", method, w.Code, w.Header())
		}
	}
	if w := postShare(s, "203.0.113.9", "", "X-Share", ""); w.Code != 403 {
		t.Errorf("no header: %d", w.Code)
	}
	if w := postShare(s, "203.0.113.9", "", "X-Share", "0"); w.Code != 403 {
		t.Errorf("wrong header value: %d", w.Code)
	}
	for name, ip := range map[string]string{"private": "10.0.0.1", "loopback": "127.0.0.1", "unknown": ""} {
		if w := postShare(s, ip, ""); w.Code != 400 {
			t.Errorf("%s address: %d", name, w.Code)
		}
	}
	if w := postShare(s, "203.0.113.9", strings.Repeat("a", 1025)); w.Code != 413 {
		t.Errorf("big body: %d", w.Code)
	}
	if len(m.cmds) != 0 {
		t.Errorf("a refused request used Redis: %v", m.cmds)
	}
	// A cross-site page cannot get a CORS answer: no preflight is granted.
	r := httptest.NewRequest("OPTIONS", "/share", nil)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "x-share")
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 405 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("preflight: %d %v", w.Code, w.Header())
	}
}

func TestShareOff(t *testing.T) {
	s, m := shareServer(t)
	s.useShare(false, "")
	if w := postShare(s, "203.0.113.9", ""); w.Code != 404 {
		t.Errorf("POST /share with SHARE=off: %d", w.Code)
	}
	if w := get(s, "GET", "/report/"+tok); w.Code != 404 {
		t.Errorf("/report with SHARE=off: %d", w.Code)
	}
	b := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9").Body.String()
	if !strings.Contains(b, `id="dns"`) || strings.Contains(b, `class="sh"`) {
		t.Error("DNS row missing or button present")
	}
	// And /ip with r adds nothing.
	fromPeer(s, "/ip?r="+tok, "127.0.0.1:1", "X-Real-IP", "2001:db8::9")
	if len(m.cmds) != 0 {
		t.Error("Redis used with SHARE=off")
	}
}

func TestShareRedisDown(t *testing.T) {
	logs := captureLog(t)
	s, _ := shareServer(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	s.useBeacon("r.openipdata.org", dead)
	w := postShare(s, "203.0.113.9", "t="+tok)
	if w.Code != 503 || strings.TrimSpace(w.Body.String()) != "{}" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("share with Redis down: %d %q", w.Code, w.Body.String())
	}
	// The page itself is as before (the button is always there).
	b := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9").Body.String()
	if !strings.Contains(b, `id="dns"`) || !strings.Contains(b, `id="other"`) {
		t.Error("the page changed with Redis down")
	}
	if w := get(s, "GET", "/report/"+tok); w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "not available") {
		t.Errorf("/report with Redis down: %d", w.Code)
	}
	if w := fromPeer(s, "/ip?r="+tok, "127.0.0.1:1", "X-Real-IP", "2001:db8::9"); w.Code != 200 || w.Body.String() != "2001:db8::9\n" {
		t.Errorf("/ip with Redis down: %d %q", w.Code, w.Body.String())
	}
	if strings.Contains(logs.String(), "203.0.113.9") || strings.Contains(logs.String(), tok) {
		t.Error("an address or token in the log")
	}
}

func TestShareOtherFamilyThroughIP(t *testing.T) {
	logs := captureLog(t)
	s, m := shareServer(t)
	id := share(t, s, "203.0.113.9", "")
	ip := func(target, addr string) int {
		w := fromPeer(s, target, "127.0.0.1:1", "X-Real-IP", addr)
		if w.Body.String() != addr+"\n" {
			t.Errorf("%s answers %q", target, w.Body.String())
		}
		return w.Code
	}
	// The same family, a bad id, an unknown id and two ids change nothing.
	ip("/ip?r="+id, "198.51.100.1")
	ip("/ip?r=nothex", "2001:db8::9")
	ip("/ip?r="+tok, "2001:db8::9")
	ip("/ip?r="+id+"&r="+id, "2001:db8::9")
	if len(m.keys("report:")) != 1 || m.snapshot(t, id).Other != "" {
		t.Fatalf("report changed: %+v", m.snapshot(t, id))
	}
	if len(m.keys("")) != 1 {
		t.Error("an unknown id created a key")
	}
	// A private address of the other family is not added either.
	fromPeer(s, "/ip?r="+id, "127.0.0.1:1", "X-Real-IP", "fd00::1")
	if m.snapshot(t, id).Other != "" {
		t.Error("a private address was added")
	}
	// The other family: added, lifetime kept.
	ip("/ip?r="+id, "2001:db8::9")
	if sn := m.snapshot(t, id); sn.Other != "2001:db8::9" {
		t.Errorf("other: %q", sn.Other)
	}
	if _, ttl := m.get("report:" + id); ttl != 2592000 {
		t.Errorf("lifetime %d", ttl)
	}
	last := m.commands("SET")
	if got := last[len(last)-1]; got[0] != "SET" || got[3] != "XX" || got[4] != "KEEPTTL" {
		t.Errorf("update was not SET XX KEEPTTL: %v", got[:2])
	}
	// Only the first one counts.
	ip("/ip?r="+id, "2001:db8::77")
	if sn := m.snapshot(t, id); sn.Other != "2001:db8::9" {
		t.Errorf("other changed to %q", sn.Other)
	}
	if strings.Contains(logs.String(), id) || strings.Contains(logs.String(), "2001:db8") {
		t.Error("id or address in the log")
	}
}

// /resolver answers about a token and writes nothing: it no longer touches reports.
func TestResolverDoesNotTouchReports(t *testing.T) {
	s, m := shareServer(t)
	id := share(t, s, "203.0.113.9", "")
	m.hash["beacon:"+tok] = map[string]string{"resolver_ip": "203.0.113.50"}
	before := len(m.commands("SET"))
	if w := resolverGet(s, "GET", "/resolver?t="+tok); w.Code != 200 || !strings.Contains(w.Body.String(), `"ip":"203.0.113.50"`) {
		t.Fatalf("resolver: %d %s", w.Code, w.Body.String())
	}
	if len(m.commands("SET")) != before || m.snapshot(t, id).Resolver != nil {
		t.Error("/resolver changed a report")
	}
	if len(m.commands("GET")) != 0 {
		t.Error("/resolver read a report or a token key")
	}
}

func TestReportPage(t *testing.T) {
	logs := captureLog(t)
	s, m := shareServer(t)
	m.hash["beacon:"+tok] = map[string]string{"resolver_ip": "203.0.113.50"}
	id := share(t, s, "203.0.113.9", "t="+tok)
	fromPeer(s, "/ip?r="+id, "127.0.0.1:1", "X-Real-IP", "2001:db8::9")

	w := get(s, "GET", "/report/"+id)
	b := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	for _, x := range []string{
		"<title>Shared report · openipdata.org</title>", `<meta name="robots" content="noindex">`, "<h2>Shared report</h2>", `<span class="r">Shared 15 Nov 2026 16:50 UTC · expires 15 Dec 2026</span>`,
		`<dt>IPv4</dt><dd class="c"><span class="w"><span class="v">203.0.113.9</span><button class="cp"`, `<dt>IPv6</dt><dd class="c"><span class="w"><span class="v">2001:db8::9</span><button class="cp"`,
		`<dt>DNS resolver</dt><dd class="od"><a href="/?203.0.113.50">203.0.113.50</a><a class="dn" href="/asn-example-net">EXAMPLE-NET</a></dd>`,
		`<dt>Network</dt><dd><a href="/asn-example-net">EXAMPLE-NET</a> · <a href="/prefix/203.0.113.0/24">203.0.113.0/24</a></dd>`,
		"<h1>Lisbon</h1>", "<p>Portugal</p>", `class="place mapped"`, `<svg class="map"`,
	} {
		if !strings.Contains(b, x) {
			t.Errorf("report lacks %q", x)
		}
	}
	for _, bad := range []string{resolverJS, fillJS, `class="sh"`, `id="other"`, `id="dns"`, "<h2>Accuracy</h2>", "<dt>Prefix"} {
		if strings.Contains(b, bad) {
			t.Errorf("report has %q", bad)
		}
	}
	h := w.Header()
	if h.Get("Cache-Control") != "private, no-store" || h.Get("ETag") != "" || h.Get("Referrer-Policy") != "no-referrer" ||
		!strings.Contains(h.Get("Content-Security-Policy"), scriptHash(panelJS)) || strings.Contains(h.Get("Content-Security-Policy"), "connect-src") {
		t.Errorf("headers: %v", h)
	}
	if n := strings.Count(b, "<script"); n != 3 {
		t.Errorf("%d scripts", n)
	}
	// HEAD: same status, no body.
	if hw := get(s, "HEAD", "/report/"+id); hw.Code != 200 || hw.Body.Len() != 0 {
		t.Errorf("HEAD: %d %d", hw.Code, hw.Body.Len())
	}
	// A report with only what the first view knew: dashes, and no copy button on a dash.
	id2 := share(t, s, "2001:db8::5", "")
	b2 := get(s, "GET", "/report/"+id2).Body.String()
	if !strings.Contains(b2, `<dt>IPv4</dt><dd>—</dd>`) || !strings.Contains(b2, `<dt>DNS resolver</dt><dd>—</dd>`) || !strings.Contains(b2, `<dt>IPv6</dt><dd class="c"><span class="w"><span class="v">2001:db8::5</span>`) {
		t.Errorf("sparse report:\n%s", b2[strings.Index(b2, "<dl>"):])
	}

	// Unknown, malformed and expired ids: 410; values are not looked up again.
	for name, target := range map[string]string{"unknown": "/report/" + tok, "short": "/report/abc", "upper": "/report/" + strings.ToUpper(id), "slash": "/report/" + id + "/x", "empty": "/report/", "traversal": "/report/../x"} {
		w := get(s, "GET", target)
		if w.Code != 410 || !strings.Contains(w.Body.String(), "This shared report has expired") || w.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(w.Body.String(), `<meta name="robots" content="noindex">`) {
			t.Errorf("%s: %d %v", name, w.Code, w.Header())
		}
	}
	m.mu.Lock()
	delete(m.kv, "report:"+id)
	m.mu.Unlock()
	if w := get(s, "GET", "/report/"+id); w.Code != 410 {
		t.Errorf("expired: %d", w.Code)
	}
	m.mu.Lock()
	m.kv["report:"+id] = "not json"
	m.mu.Unlock()
	if w := get(s, "GET", "/report/"+id); w.Code != 410 {
		t.Errorf("garbage: %d", w.Code)
	}
	if w := get(s, "POST", "/report/"+id); w.Code != 405 {
		t.Errorf("POST: %d", w.Code)
	}
	if strings.Contains(logs.String(), id) || strings.Contains(logs.String(), "203.0.113.9") {
		t.Errorf("id or address in the log:\n%s", logs)
	}
}

// A report shows the values of the visit, not what the data says now. Older
// snapshots have no slug and no prefix: the handle links to the network page
// and there is no prefix part.
func TestReportShownAsStored(t *testing.T) {
	s, m := shareServer(t)
	id := share(t, s, "203.0.113.9", "")
	for _, c := range []struct {
		name         string
		slug, prefix string
		want         string
	}{
		{"older snapshot: no slug, no prefix", "", "", `<dt>Network</dt><dd><a href="/as1">OLD-NAME</a></dd>`},
		{"slug and prefix", "old-name", "198.51.100.0/24", `<dt>Network</dt><dd><a href="/asn-old-name">OLD-NAME</a> · <a href="/prefix/198.51.100.0/24">198.51.100.0/24</a></dd>`},
		{"slug, prefix outside the page range: plain", "old-name", "2.0.0.0/7", `<dt>Network</dt><dd><a href="/asn-old-name">OLD-NAME</a> · 2.0.0.0/7</dd>`},
		{"prefix, no slug", "", "198.51.100.0/24", `<dt>Network</dt><dd><a href="/as1">OLD-NAME</a> · <a href="/prefix/198.51.100.0/24">198.51.100.0/24</a></dd>`},
	} {
		sn := m.snapshot(t, id)
		sn.Network = netSnap{ASN: 1, Handle: "OLD-NAME", Slug: c.slug, Prefix: c.prefix}
		sn.Place = placeSnap{Country: "Spain", CC: "ES"}
		b, _ := json.Marshal(sn)
		m.mu.Lock()
		m.kv["report:"+id] = string(b)
		m.mu.Unlock()
		body := get(s, "GET", "/report/"+id).Body.String()
		if !strings.Contains(body, c.want) || !strings.Contains(body, "<h1>Spain</h1>") || strings.Contains(body, "Lisbon") || !strings.Contains(body, `class="here"`) {
			t.Errorf("%s: the report is not shown as stored", c.name)
		}
	}
}

func TestSnapshotSize(t *testing.T) {
	long := strings.Repeat("\U0001D11E", 120) // four bytes each
	sn := snapshot{Created: "2026-11-15T16:50:12Z", IP: "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff", Family: 6,
		Place:   placeSnap{City: long, Country: long, CC: "PT"},
		Network: netSnap{ASN: 4294967295, Handle: long, Category: strings.Repeat("\U0001D11E", 40), Domain: strings.Repeat("\U0001D11E", 100), Org: strings.Repeat("\U0001D11E", 200)},
		Other:   "2001:db8:ffff:ffff:ffff:ffff:ffff:fffe", Resolver: &resolverSnap{IP: "2001:db8:ffff:ffff:ffff:ffff:ffff:fffd", Name: long, Org: "/asn-" + strings.Repeat("a", 255)}}
	b, _ := marshalSnap(sn)
	t.Logf("worst case snapshot: %d bytes (limit %d)", len(b), maxReportBytes)
	if len(b) > maxReportBytes {
		t.Errorf("a snapshot can be %d bytes", len(b))
	}
}

func TestCheckOrigin(t *testing.T) {
	for v, want := range map[string]string{"https://openipdata.org": "https://openipdata.org", "https://openipdata.org/": "https://openipdata.org",
		"http://127.0.0.1:8080": "http://127.0.0.1:8080", "https://x.example:8443": "https://x.example:8443"} {
		if got, err := checkOrigin(v); err != nil || got != want {
			t.Errorf("checkOrigin(%q) = %q, %v", v, got, err)
		}
	}
	for _, v := range []string{"", "openipdata.org", "http://openipdata.org", "https://openipdata.org/path", "https://openipdata.org/?x=1", "https://u@openipdata.org", "https://openipdata.org/#f", "ftp://x"} {
		if _, err := checkOrigin(v); err == nil {
			t.Errorf("checkOrigin(%q) accepted", v)
		}
	}
}

func TestShareOriginSetting(t *testing.T) {
	s, _ := shareServer(t)
	s.useShare(true, "https://example.test")
	w := postShare(s, "203.0.113.9", "", "Host", "evil.example")
	if !regexp.MustCompile(`"url":"https://example\.test/report/[0-9a-f]{32}"`).MatchString(w.Body.String()) || strings.Contains(w.Body.String(), "evil.example") {
		t.Errorf("the share link is not built from SITE_ORIGIN: %s", w.Body.String())
	}
}

func TestStatusShareRow(t *testing.T) {
	s, _ := shareServer(t)
	if b := get(s, "GET", "/status").Body.String(); !strings.Contains(b, "<h2>Share</h2>") || !strings.Contains(b, "<dt>Share</dt><dd>on</dd>") {
		t.Error("status: share on")
	}
	s.useShare(false, "")
	if b := get(s, "GET", "/status").Body.String(); !strings.Contains(b, `<dt>Share</dt><dd class="none">off</dd>`) {
		t.Error("status: share off")
	}
}

// The copy buttons are in the HTML but hidden: without JavaScript they never show.
func TestCopyButtonsHiddenWithoutScript(t *testing.T) {
	s := fillServer(t, u4, u6)
	b := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9").Body.String()
	if n := strings.Count(b, `<button class="cp" type="button" hidden aria-label="Copy">`); n != 2 {
		t.Errorf("%d hidden copy buttons on the visitor page, want 2 (IPv4, IPv6)", n)
	}
	if strings.Contains(b, `<button class="cp" type="button" aria-label`) {
		t.Error("a copy button is not hidden")
	}
	if !strings.Contains(CSSForTest(), ".cp[hidden]") {
		t.Error("the CSS does not keep hidden buttons hidden")
	}
	l := get(s, "GET", "/?203.0.113.9").Body.String()
	if n := strings.Count(l, `<button class="cp" type="button" hidden`); n != 1 {
		t.Errorf("%d copy buttons on a lookup page, want 1", n)
	}
	for _, target := range []string{"/?10.1.2.3", "/?1.2.3", "/as64500"} {
		if strings.Contains(get(s, "GET", target).Body.String(), `class="cp"`) {
			t.Errorf("%s has a copy button", target)
		}
	}
}

func CSSForTest() string { return styleCSS + liveCSS }

func TestPanelScriptShape(t *testing.T) {
	// panel.js is only the copy buttons: no Share code, no textarea fallback.
	for _, x := range []string{"navigator.clipboard.writeText", "Copy failed", "1500", "hidden"} {
		if !strings.Contains(panelJS, x) {
			t.Errorf("panelJS lacks %s", x)
		}
	}
	for _, x := range []string{"innerHTML", "document.write", "eval(", "XMLHttpRequest", "localStorage", "insertAdjacent", "WebSocket", "data-url", "dataset.url",
		"execCommand", "textarea", "/share", "POST", "fetch(", "ClipboardItem", "prompt", ".sh"} {
		if strings.Contains(panelJS, x) {
			t.Errorf("panelJS has %s", x)
		}
	}
	// share.js is the Share button.
	for _, x := range []string{"navigator.clipboard.writeText", "navigator.clipboard.write(", "ClipboardItem", "new Blob", "Link copied", "Copying...", "Copy failed", "1500", "hidden",
		`"/share"`, `"POST"`, `"X-Share": "1"`, "5000", "AbortController", "busy", `"t=" + t`, "window.prompt", "dataset.t"} {
		if !strings.Contains(shareJS, x) {
			t.Errorf("shareJS lacks %s", x)
		}
	}
	for _, x := range []string{"innerHTML", "document.write", "eval(", "XMLHttpRequest", "localStorage", "insertAdjacent", "WebSocket", "data-url", "dataset.url", "execCommand", "textarea"} {
		if strings.Contains(shareJS, x) {
			t.Errorf("shareJS has %s", x)
		}
	}
	// The clipboard write is made inside the click, before the POST is answered.
	if i, j := strings.Index(shareJS, "var p = share(), w;"), strings.Index(shareJS, "navigator.clipboard.write("); i < 0 || j < i {
		t.Error("the clipboard write must come right after the synchronous share() call in the click handler")
	}
	for name, js := range map[string]string{"panelJS": panelJS, "shareJS": shareJS} {
		if regexp.MustCompile(`[^\x20-\x7e\n]`).MatchString(js) {
			t.Errorf("non-ASCII in %s", name)
		}
		t.Logf("%s: %d bytes", name, len(js))
	}
	// fill.js keeps the copy button of the row it fills and unhides it only with an address.
	if !strings.Contains(fillJS, ".cp") || !strings.Contains(fillJS, "hidden = false") {
		t.Error("fillJS does not unhide the copy button")
	}
	// resolver.js makes its own token and leaves it for share.js; the server makes none.
	if !strings.Contains(resolverJS, "dataset.t = t") || !strings.Contains(resolverJS, "crypto.getRandomValues") || strings.Contains(resolverJS, "dataset.token") {
		t.Error("resolverJS token handling")
	}
}

// share.js and its hash are only on the page that has the Share button; every
// other page with panel.js (lookup, report, download) has neither.
func TestShareScriptOnlyOnVisitorPage(t *testing.T) {
	s, _ := shareServer(t)
	w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
	b, c := w.Body.String(), w.Header().Get("Content-Security-Policy")
	if !strings.Contains(b, "<script>"+panelJS+"</script>\n<script>"+shareJS+"</script>\n</body>") || !strings.Contains(c, scriptHash(shareJS)) || !strings.Contains(c, scriptHash(panelJS)) {
		t.Errorf("visitor page lacks share.js or its hash: %s", c)
	}
	for _, target := range []string{"/?203.0.113.9", "/download", "/status"} {
		w := get(s, "GET", target)
		if strings.Contains(w.Body.String(), shareJS[:60]) || strings.Contains(w.Header().Get("Content-Security-Policy"), scriptHash(shareJS)) {
			t.Errorf("%s has share.js", target)
		}
	}
	id := share(t, s, "203.0.113.9", "")
	w = get(s, "GET", "/report/"+id)
	if strings.Contains(w.Body.String(), shareJS[:60]) || strings.Contains(w.Header().Get("Content-Security-Policy"), scriptHash(shareJS)) {
		t.Error("the report page has share.js")
	}
	// A visitor page without the Share button (SHARE off) has no share.js either.
	s.share = false
	w = fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9")
	if strings.Contains(w.Body.String(), shareJS[:60]) || strings.Contains(w.Header().Get("Content-Security-Policy"), scriptHash(shareJS)) {
		t.Error("the page without the Share button has share.js")
	}
}

// Every link is gray with the accent on hover: no rule colours a link in any
// other way.
func TestLinkColourRule(t *testing.T) {
	// The design's rule: links inherit their text colour and turn orange on hover;
	// links in the data are orange and underlined on hover. Nothing else colours a link.
	for _, x := range []string{
		"a{color:inherit;text-decoration:none}",
		"a:hover{color:var(--accent)}",
		".tbl a,.pf a,.note a,.head a{color:var(--accent)}",
		".tbl a:hover,.pf a:hover,.note a:hover,.head a:hover{text-decoration:underline}",
	} {
		if !strings.Contains(styleCSS, x) {
			t.Errorf("style.css lacks the link rule %s", x)
		}
	}
	for _, old := range []string{".lnk", "a.as", "dd .as", "a.dn", ".foot a", "dd a"} {
		if strings.Contains(styleCSS+liveCSS+accCSS+asnCSS, old) {
			t.Errorf("per-class link rule left: %s", old)
		}
	}
}

// Nothing a test run logs holds a report id, a token or an address.
var _ http.Handler = (*server)(nil)
