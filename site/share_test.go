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

var (
	shareRE = regexp.MustCompile(`data-url="https://openipdata\.org/report/([0-9a-f]{32})"`)
	tokRE   = regexp.MustCompile(`data-token="([0-9a-f]{32})"`)
)

func visit(s *server, ip string) (*httptest.ResponseRecorder, string, string) {
	w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", ip)
	b := w.Body.String()
	var id, token string
	if m := shareRE.FindStringSubmatch(b); m != nil {
		id = m[1]
	}
	if m := tokRE.FindStringSubmatch(b); m != nil {
		token = m[1]
	}
	return w, id, token
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

func TestShareSnapshotOnVisit(t *testing.T) {
	logs := captureLog(t)
	s, m := shareServer(t)
	w, id, token := visit(s, "203.0.113.9")
	if w.Code != 200 || id == "" || token == "" || id == token {
		t.Fatalf("status %d, id %q, token %q", w.Code, id, token)
	}
	b := w.Body.String()
	if !strings.Contains(b, `<button class="sh" type="button" hidden data-url="https://openipdata.org/report/`+id+`">Share</button>`) {
		t.Error("no hidden Share button")
	}
	if !strings.Contains(b, `data-from="`+u6+`?r=`+id+`"`) {
		t.Error("the request of the other family does not carry the report id")
	}
	if !strings.Contains(b, `<dd id="dns" class="od" data-zone="r.openipdata.org" data-token="`+token+`">`) {
		t.Error("the DNS row has no server token")
	}
	if strings.Count(b, id) != 2 || strings.Count(b, token) != 1 { // the button and the other-family URL; the DNS row
		t.Errorf("id appears %d times, token %d times", strings.Count(b, id), strings.Count(b, token))
	}
	// Only the report and its beacon key were written.
	if got := m.keys(""); len(got) != 2 {
		t.Errorf("keys: %v", got)
	}
	v, ttl := m.get("report:" + id)
	if ttl != 2592000 {
		t.Errorf("report expires in %d s", ttl)
	}
	if rt, rttl := m.get("rt:" + token); rt != id || rttl != 600 {
		t.Errorf("beacon key %q ttl %d", rt, rttl)
	}
	var raw map[string]any
	json.Unmarshal([]byte(v), &raw)
	t.Logf("snapshot: %s", v)
	for _, k := range []string{"created", "ip", "family", "place", "network"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("snapshot lacks %s", k)
		}
	}
	sn := m.snapshot(t, id)
	if sn.Created != "2026-11-15T16:50:12Z" || sn.IP != "203.0.113.9" || sn.Family != 4 || sn.Other != "" || sn.Resolver != nil ||
		sn.Place.City != "Lisbon" || sn.Place.Country != "Portugal" || sn.Place.CC != "PT" || sn.Place.Lat == nil || sn.Place.Lon == nil ||
		sn.Network.ASN != 64500 || sn.Network.Handle != "EXAMPLE-NET" || sn.Network.Category != "isp" || sn.Network.Domain != "example.net" || sn.Network.Org != "Example Net" {
		t.Errorf("snapshot: %+v", sn)
	}
	for _, bad := range []string{"User-Agent", "Mozilla", "Referer", "X-Real-IP", "headers"} {
		if strings.Contains(v, bad) {
			t.Errorf("snapshot has %q", bad)
		}
	}
	// Every visit saves its own report.
	_, id2, _ := visit(s, "203.0.113.9")
	if id2 == "" || id2 == id || len(m.keys("report:")) != 2 {
		t.Error("a second visit did not make its own report")
	}
	if strings.Contains(logs.String(), id) || strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "203.0.113.9") {
		t.Errorf("an id, token or address in the log:\n%s", logs)
	}
}

func TestShareNotSavedElsewhere(t *testing.T) {
	s, m := shareServer(t)
	for name, target := range map[string]string{"lookup": "/?203.0.113.9", "bad": "/?1.2.3", "asn": "/as64500", "top": "/top-asn-by-ip-address", "status": "/status"} {
		w := get(s, "GET", target)
		if strings.Contains(w.Body.String(), `class="sh"`) {
			t.Errorf("%s: Share button", name)
		}
	}
	if w := fromPeer(s, "/", "127.0.0.1:1"); strings.Contains(w.Body.String(), `class="sh"`) { // address unknown
		t.Error("button on the unknown-address page")
	}
	if w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "10.0.0.1"); strings.Contains(w.Body.String(), `class="sh"`) { // not a public address
		t.Error("button for a private address")
	}
	// HEAD saves nothing.
	r := httptest.NewRequest("HEAD", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Real-IP", "203.0.113.9")
	s.ServeHTTP(httptest.NewRecorder(), r)
	if got := m.commands("SET"); len(got) != 0 {
		t.Errorf("writes without a visit: %v", got)
	}
}

func TestShareOff(t *testing.T) {
	s, m := shareServer(t)
	s.useShare(false, "")
	w, id, token := visit(s, "203.0.113.9")
	b := w.Body.String()
	if w.Code != 200 || id != "" || token != "" || strings.Contains(b, `class="sh"`) || strings.Contains(b, "?r=") {
		t.Error("share is off but visible")
	}
	if len(m.commands("SET")) != 0 {
		t.Error("something was written")
	}
	// The DNS row still works, with its own token made by the script.
	if !strings.Contains(b, `id="dns"`) {
		t.Error("DNS row missing")
	}
	if w := get(s, "GET", "/report/"+tok); w.Code != 404 {
		t.Errorf("/report with SHARE=off: %d", w.Code)
	}
	// And /ip with r adds nothing.
	fromPeer(s, "/ip?r="+tok, "127.0.0.1:1", "X-Real-IP", "2001:db8::9")
	if len(m.commands("GET")) != 0 || len(m.commands("SET")) != 0 {
		t.Error("/ip touched Redis with SHARE=off")
	}
}

func TestShareRedisDown(t *testing.T) {
	s, _ := shareServer(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	s.useBeacon("r.openipdata.org", dead)
	w, id, token := visit(s, "203.0.113.9")
	b := w.Body.String()
	if w.Code != 200 || id != "" || token != "" || strings.Contains(b, `class="sh"`) || !strings.Contains(b, `id="dns"`) || !strings.Contains(b, `id="other"`) {
		t.Errorf("Redis down: %d, the page is not as before", w.Code)
	}
	if strings.Contains(b, "?r=") {
		t.Error("report id in the page")
	}
	if w := get(s, "GET", "/report/"+tok); w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "not available") {
		t.Errorf("/report with Redis down: %d", w.Code)
	}
	// /ip still answers.
	if w := fromPeer(s, "/ip?r="+tok, "127.0.0.1:1", "X-Real-IP", "2001:db8::9"); w.Code != 200 || w.Body.String() != "2001:db8::9\n" {
		t.Errorf("/ip with Redis down: %d %q", w.Code, w.Body.String())
	}
}

func TestShareOtherFamilyThroughIP(t *testing.T) {
	logs := captureLog(t)
	s, m := shareServer(t)
	_, id, _ := visit(s, "203.0.113.9")
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
	if len(m.keys("")) != 2 {
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

func TestShareResolverThroughBeacon(t *testing.T) {
	logs := captureLog(t)
	s, m := shareServer(t)
	_, id, token := visit(s, "203.0.113.9")
	m.hash["beacon:"+token] = map[string]string{"resolver_ip": "203.0.113.50"}
	if w := resolverGet(s, "GET", "/resolver?t="+token); w.Code != 200 || !strings.Contains(w.Body.String(), `"ip":"203.0.113.50"`) {
		t.Fatalf("resolver: %d %s", w.Code, w.Body.String())
	}
	sn := m.snapshot(t, id)
	if sn.Resolver == nil || sn.Resolver.IP != "203.0.113.50" {
		t.Fatalf("resolver not in the report: %+v", sn.Resolver)
	}
	if _, ttl := m.get("report:" + id); ttl != 2592000 {
		t.Errorf("lifetime %d", ttl)
	}
	// A token the server did not make for a page, or a private resolver, adds nothing.
	other := "fedcba9876543210fedcba9876543210"
	m.hash["beacon:"+other] = map[string]string{"resolver_ip": "203.0.113.51"}
	before := len(m.commands("SET"))
	resolverGet(s, "GET", "/resolver?t="+other)
	if len(m.commands("SET")) != before {
		t.Error("a token without a report wrote")
	}
	// HEAD adds nothing.
	_, id2, token2 := visit(s, "203.0.113.9")
	m.hash["beacon:"+token2] = map[string]string{"resolver_ip": "203.0.113.50"}
	resolverGet(s, "HEAD", "/resolver?t="+token2)
	if m.snapshot(t, id2).Resolver != nil {
		t.Error("HEAD added the resolver")
	}
	if strings.Contains(logs.String(), id) || strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "203.0.113.50") {
		t.Error("id, token or address in the log")
	}
}

func TestReportPage(t *testing.T) {
	logs := captureLog(t)
	s, m := shareServer(t)
	_, id, token := visit(s, "203.0.113.9")
	fromPeer(s, "/ip?r="+id, "127.0.0.1:1", "X-Real-IP", "2001:db8::9")
	m.hash["beacon:"+token] = map[string]string{"resolver_ip": "203.0.113.50"}
	resolverGet(s, "GET", "/resolver?t="+token)

	w := get(s, "GET", "/report/"+id)
	b := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	for _, x := range []string{
		"<title>Shared report · openipdata.org</title>", `<meta name="robots" content="noindex">`, "<h2>Shared report</h2>", `<span class="r">Shared 15 Nov 2026 16:50 UTC</span>`,
		`<dt>IPv4</dt><dd class="c"><span class="w"><span class="v">203.0.113.9</span><button class="cp"`, `<dt>IPv6</dt><dd class="c"><span class="w"><span class="v">2001:db8::9</span><button class="cp"`,
		`<dt>DNS resolver</dt><dd class="od"><a href="/?203.0.113.50">203.0.113.50</a><a class="dn" href="/asn-example-net">EXAMPLE-NET</a></dd>`,
		`<dt>Network</dt><dd><a href="/as64500">EXAMPLE-NET</a> · <a href="https://example.net/?utm_source=openipdata.org" rel="nofollow noopener">example.net</a></dd>`,
		"<h1>Lisbon</h1>", "<p>Portugal</p>", `class="place mapped"`, `<svg class="map"`,
	} {
		if !strings.Contains(b, x) {
			t.Errorf("report lacks %q", x)
		}
	}
	for _, bad := range []string{"fetch(", `class="sh"`, `id="other"`, `id="dns"`, "<h2>Accuracy</h2>", "Prefix"} {
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
	_, id2, _ := visit(s, "2001:db8::5")
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

// A report shows the values of the visit, not what the data says now.
func TestReportShownAsStored(t *testing.T) {
	s, m := shareServer(t)
	_, id, _ := visit(s, "203.0.113.9")
	sn := m.snapshot(t, id)
	sn.Network.Handle, sn.Network.ASN, sn.Network.Domain, sn.Network.Category = "OLD-NAME", 1, "", ""
	sn.Place = placeSnap{Country: "Spain", CC: "ES"}
	b, _ := json.Marshal(sn)
	m.mu.Lock()
	m.kv["report:"+id] = string(b)
	m.mu.Unlock()
	body := get(s, "GET", "/report/"+id).Body.String()
	if !strings.Contains(body, `<dd><a href="/as1">OLD-NAME</a></dd>`) || !strings.Contains(body, "<h1>Spain</h1>") || strings.Contains(body, "Lisbon") || !strings.Contains(body, `class="here"`) {
		t.Error("the report is not shown as stored")
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
	w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9", "Host", "evil.example")
	if !regexp.MustCompile(`data-url="https://example\.test/report/[0-9a-f]{32}"`).MatchString(w.Body.String()) || strings.Contains(w.Body.String(), "evil.example") {
		t.Error("the share link is not built from SITE_ORIGIN")
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
	for _, x := range []string{"navigator.clipboard.writeText", "execCommand", "Link copied", "Copying...", "Copy failed", "1500", "hidden"} {
		if !strings.Contains(panelJS, x) {
			t.Errorf("panelJS lacks %s", x)
		}
	}
	for _, x := range []string{"innerHTML", "document.write", "eval(", "XMLHttpRequest", "fetch(", "localStorage", "insertAdjacent", "WebSocket"} {
		if strings.Contains(panelJS, x) {
			t.Errorf("panelJS has %s", x)
		}
	}
	if regexp.MustCompile(`[^\x20-\x7e\n]`).MatchString(panelJS) {
		t.Error("non-ASCII in panelJS")
	}
	// fill.js keeps the copy button of the row it fills and unhides it only with an address.
	if !strings.Contains(fillJS, ".cp") || !strings.Contains(fillJS, "hidden = false") {
		t.Error("fillJS does not unhide the copy button")
	}
	// resolver.js uses the token of the server, and still makes its own.
	if !strings.Contains(resolverJS, "dataset.token") || !strings.Contains(resolverJS, "crypto.getRandomValues") {
		t.Error("resolverJS token handling")
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
