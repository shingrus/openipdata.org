package main

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func jsonBody(t *testing.T, body string) map[string]any {
	t.Helper()
	if !strings.HasSuffix(body, "}\n") || strings.Count(body, "\n") != 1 {
		t.Errorf("not one object and a newline: %q", body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("%v: %q", err, body)
	}
	return m
}

func TestIPJSONLookup(t *testing.T) {
	s, _ := asnServer(t)
	s.public = func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsPrivate() }
	w := get(s, "GET", "/json/203.0.113.9")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	h := w.Header()
	if h.Get("Content-Type") != "application/json; charset=utf-8" || h.Get("Access-Control-Allow-Origin") != "*" ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "public, max-age=300" {
		t.Errorf("headers %v", h)
	}
	m := jsonBody(t, w.Body.String())
	for k, want := range map[string]any{"ip": "203.0.113.9", "country_code": "PT", "country_name": "Portugal", "city_name": "Lisbon",
		"asn": float64(64500), "as_name": "EXAMPLE-NET", "org": "Example Net", "prefix": "203.0.113.0/24"} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	if _, ok := m["latitude"].(float64); !ok {
		t.Errorf("no latitude: %v", m)
	}
	if _, ok := m["longitude"].(float64); !ok {
		t.Errorf("no longitude: %v", m)
	}
	// HEAD: the same headers, no body.
	if w := get(s, "HEAD", "/json/203.0.113.9"); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD: %d, %d bytes", w.Code, w.Body.Len())
	}
	if w := get(s, "POST", "/json/203.0.113.9"); w.Code != 405 {
		t.Errorf("POST: %d", w.Code)
	}
	// IPv6.
	if m := jsonBody(t, get(s, "GET", "/json/2001:db8::5").Body.String()); m["ip"] != "2001:db8::5" || m["city_name"] != "Lisbon" {
		t.Errorf("v6: %v", m)
	}
}

func TestIPJSONFieldsLeftOut(t *testing.T) {
	s := fixtureServer(t) // no ASN data: no handle, the MMDB text
	for target, gone := range map[string][]string{
		"/json/198.51.100.1": {"city_name", "latitude", "longitude", "asn", "as_name", "org", "prefix"}, // a country only
		"/json/192.0.2.1":    {"country_code", "country_name", "city_name", "latitude", "longitude", "asn", "as_name", "org", "prefix"},
	} {
		w := get(s, "GET", target)
		if w.Code != 200 {
			t.Fatalf("%s: %d", target, w.Code)
		}
		m := jsonBody(t, w.Body.String())
		for _, k := range gone {
			if _, ok := m[k]; ok {
				t.Errorf("%s: %s is there: %v", target, k, m)
			}
		}
		if m["ip"] == nil {
			t.Errorf("%s: no ip", target)
		}
	}
	// Without ASN data the name is the MMDB's, and the prefix the MMDB network.
	m := jsonBody(t, get(s, "GET", "/json/203.0.113.9").Body.String())
	if m["asn"] != float64(64500) || m["prefix"] != "203.0.113.0/24" || m["as_name"] == nil {
		t.Errorf("without ASN data: %v", m)
	}
}

func TestIPJSONRedirectsAndErrors(t *testing.T) {
	s := fixtureServer(t)
	for _, c := range []struct {
		target, loc string
		code        int
		body        string
		cache       string
	}{
		{"/json/2001:DB8::5", "/json/2001:db8::5", 301, "", "public, max-age=300"},
		{"/json/::ffff:203.0.113.9", "/json/203.0.113.9", 301, "", "public, max-age=300"},
		{"/json/%32%30%33.0.113.9", "/json/203.0.113.9", 301, "", "public, max-age=300"},
		{"/json/%20203.0.113.9", "/json/203.0.113.9", 301, "", "public, max-age=300"},
		{"/json/nope", "", 400, `{"error":"not an IP address"}`, "no-store"},
		{"/json/1.2.3", "", 400, `{"error":"not an IP address"}`, "no-store"},
		{"/json/fe80::1%25eth0", "", 400, `{"error":"not an IP address"}`, "no-store"},
		{"/json/10.1.2.3", "", 400, `{"ip":"10.1.2.3","error":"not a public address"}`, "public, max-age=300"},
		{"/json/203.0.113.9/x", "", 404, `{"error":"not found"}`, "public, max-age=300"},
		{"/json/203.0.113.9/", "", 404, `{"error":"not found"}`, "public, max-age=300"},
		{"/json//", "", 404, `{"error":"not found"}`, "public, max-age=300"},
	} {
		w := get(s, "GET", c.target)
		if w.Code != c.code || w.Header().Get("Location") != c.loc || w.Header().Get("Cache-Control") != c.cache {
			t.Errorf("%s: %d %q %q", c.target, w.Code, w.Header().Get("Location"), w.Header().Get("Cache-Control"))
		}
		if c.body != "" {
			if w.Body.String() != c.body+"\n" || w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
				t.Errorf("%s: %q %v", c.target, w.Body, w.Header())
			}
		}
	}
	if w := get(s, "GET", "/jsonx"); w.Code != 404 || strings.Contains(w.Header().Get("Content-Type"), "json") {
		t.Errorf("/jsonx: %d", w.Code)
	}
}

func TestIPJSONOwnAddress(t *testing.T) {
	s := fixtureServer(t)
	for _, target := range []string{"/json", "/json/"} {
		w := fromPeer(s, target, "127.0.0.1:5000", "X-Real-IP", "203.0.113.9")
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: %d %v", target, w.Code, w.Header())
		}
		if m := jsonBody(t, w.Body.String()); m["ip"] != "203.0.113.9" || m["city_name"] != "Lisbon" {
			t.Errorf("%s: %v", target, m)
		}
	}
	// Unknown address (loopback peer, no header).
	w := fromPeer(s, "/json", "127.0.0.1:5000")
	if w.Code != 503 || w.Body.String() != `{"error":"unknown address"}`+"\n" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("unknown: %d %q %v", w.Code, w.Body, w.Header())
	}
	// A header from a direct peer is ignored: that peer is the visitor.
	if m := jsonBody(t, fromPeer(s, "/json", "198.51.100.1:5000", "X-Real-IP", "203.0.113.9").Body.String()); m["ip"] != "198.51.100.1" {
		t.Errorf("forged header: %v", m)
	}
	// Not public: the address and the error, never cached.
	w = fromPeer(s, "/json", "127.0.0.1:5000", "X-Real-IP", "10.1.2.3")
	if w.Code != 400 || w.Body.String() != `{"ip":"10.1.2.3","error":"not a public address"}`+"\n" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("not public: %d %q %v", w.Code, w.Body, w.Header())
	}
}

func TestRobotsAndLLMsMentionJSON(t *testing.T) {
	s := fixtureServer(t)
	if b := get(s, "GET", "/robots.txt").Body.String(); !strings.Contains(b, "Disallow: /json\n") {
		t.Errorf("robots.txt: %q", b)
	}
	b := get(s, "GET", "/llms.txt").Body.String()
	for _, x := range []string{"/json:", "/json/<ip>:", "/<country>-asn-list"} {
		if !strings.Contains(b, x) {
			t.Errorf("llms.txt lacks %s", x)
		}
	}
}
