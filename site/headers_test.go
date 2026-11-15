package main

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var hdrBlock = regexp.MustCompile(`(?s)<script type="application/json" id="hdrs">(.*?)</script>`)

// hdrsOf is the headers JSON of a page, or "" when it has none.
func hdrsOf(body string) string {
	if m := hdrBlock.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

func TestHeadersButton(t *testing.T) {
	s := fixtureServer(t)
	w := fromPeer(s, "/", "127.0.0.1:5000", "X-Real-IP", "203.0.113.9",
		"User-Agent", "Test/1.0", "Accept-Language", "en", "Cookie", "sid=secret", "Authorization", "Bearer secret",
		"X-Forwarded-For", "1.2.3.4", "X-Forwarded-Proto", "https", "Forwarded", "for=1.2.3.4", "True-Client-IP", "1.2.3.4",
		"CF-Connecting-IP", "1.2.3.4", "CF-Ray", "abc", "Cdn-Loop", "cloudflare", "X-Share", "1", "Connection", "close",
		"Proxy-Authorization", "x", "Sec-Fetch-Mode", "navigate")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `<button class="hd" type="button" hidden>Headers</button>`) {
		t.Fatalf("no Headers button: %d", w.Code)
	}
	if w.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("cache %q", w.Header().Get("Cache-Control"))
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(hdrsOf(body)), &m); err != nil {
		t.Fatalf("headers JSON: %v", err)
	}
	if m["user-agent"] != "Test/1.0" || m["accept-language"] != "en" || m["sec-fetch-mode"] != "navigate" || len(m) != 3 {
		t.Errorf("headers %v", m)
	}
	if !strings.Contains(hdrsOf(body), "{\n  \"accept-language\": \"en\",\n  \"sec-fetch-mode\"") {
		t.Errorf("not pretty and sorted:\n%s", hdrsOf(body))
	}
	for _, secret := range []string{"secret", "1.2.3.4", "cloudflare", "x-real-ip", "203.0.113.9\"", "abc"} {
		if strings.Contains(hdrsOf(body), secret) {
			t.Errorf("the headers hold %q", secret)
		}
	}
	// The CSP hash of panel.js is on the page; the data block needs none.
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), scriptHash(panelJS)) {
		t.Error("no hash of panel.js")
	}
}

func TestHeadersJoinedAndRealIPHeader(t *testing.T) {
	s := fixtureServer(t)
	s.realIP = "X-Visitor"
	r := fromPeer(s, "/", "198.51.100.1:5000", "User-Agent", "A", "X-Visitor", "10.0.0.1", "X-Other", "o")
	m := map[string]string{}
	json.Unmarshal([]byte(hdrsOf(r.Body.String())), &m)
	if _, ok := m["x-visitor"]; ok || m["x-other"] != "o" {
		t.Errorf("headers %v", m)
	}
	// Repeated headers are one string.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("Accept", "text/html")
	req.Header.Add("Accept", "image/avif")
	if got := string(s.requestHeaders(req)); !strings.Contains(got, `"accept": "text/html, image/avif"`) {
		t.Errorf("repeated header: %s", got)
	}
}

func TestHeadersEscapedAndCapped(t *testing.T) {
	s := fixtureServer(t)
	w := fromPeer(s, "/", "127.0.0.1:5000", "X-Real-IP", "203.0.113.9", "X-Evil", `</script><b>"&'`, "User-Agent", "ok")
	body := w.Body.String()
	if strings.Contains(body, `</script><b>`) || strings.Contains(body, `<b>"`) {
		t.Error("a header value broke out of the data block")
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(hdrsOf(body)), &m); err != nil || m["x-evil"] != `</script><b>"&'` {
		t.Errorf("round trip: %v %v", err, m)
	}
	if strings.Count(body, "</script>") != strings.Count(body, "<script") {
		t.Error("unbalanced script tags")
	}
	// A cap of 8 KB: the longest values go first.
	w = fromPeer(s, "/", "127.0.0.1:5000", "X-Real-IP", "203.0.113.9", "X-Long", strings.Repeat("a", 9000), "X-Mid", strings.Repeat("b", 3000), "User-Agent", "ok")
	j := hdrsOf(w.Body.String())
	if len(j) > maxHeadersJSON {
		t.Errorf("JSON is %d bytes", len(j))
	}
	m = nil
	json.Unmarshal([]byte(j), &m)
	if _, ok := m["x-long"]; ok || m["x-mid"] == "" || m["user-agent"] != "ok" {
		t.Errorf("cap: %d keys", len(m))
	}
}

func TestHeadersOnlyOnTheVisitorPage(t *testing.T) {
	s, m := shareServer(t)
	hdr := []string{"X-Real-IP", "203.0.113.9", "User-Agent", "Test/1.0", "X-Marker", "marker-value"}
	// The visitor's own page has them, with or without Share.
	if b := fromPeer(s, "/", "127.0.0.1:5000", hdr...).Body.String(); !strings.Contains(b, `class="sh"`) || !strings.Contains(b, `class="hd"`) || !strings.Contains(b, "marker-value") {
		t.Error("visitor page: no buttons or headers")
	}
	// A lookup page (publicly cached), an error, the unknown address and the not-public address do not.
	for _, target := range []string{"/?203.0.113.9", "/?1.2.3", "/?10.1.2.3", "/as64500", "/download"} {
		b := fromPeer(s, target, "127.0.0.1:5000", hdr...).Body.String()
		if strings.Contains(b, `class="hd"`) || strings.Contains(b, `id="hdrs"`) || strings.Contains(b, "marker-value") {
			t.Errorf("%s has the headers", target)
		}
	}
	for _, c := range []struct{ target, peer string }{{"/", "127.0.0.1:5000"}} {
		b := fromPeer(s, c.target, c.peer, "User-Agent", "Test/1.0", "X-Marker", "marker-value").Body.String() // no address
		if strings.Contains(b, `class="hd"`) || strings.Contains(b, "marker-value") {
			t.Error("the page of an unknown address has the headers")
		}
	}
	// A shared report never has them, and a report is not made from them.
	id := share(t, s, "203.0.113.9", "")
	if v := m.kv["report:"+id]; strings.Contains(v, "marker-value") || strings.Contains(v, "Test/1.0") {
		t.Error("the report holds headers")
	}
	b := fromPeer(s, "/report/"+id, "127.0.0.1:5000", hdr...).Body.String()
	if strings.Contains(b, `class="hd"`) || strings.Contains(b, `id="hdrs"`) || strings.Contains(b, "marker-value") {
		t.Error("the report page has the headers")
	}
	// panel.js is the only script that knows the button.
	if !strings.Contains(panelJS, `".hd"`) || !strings.Contains(panelJS, "Copy failed") || !strings.Contains(panelJS, `"Copied"`) {
		t.Error("panel.js lacks the Headers code")
	}
}
