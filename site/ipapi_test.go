package main

import (
	"encoding/json"
	"net/http/httptest"
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
	// Without ASN data the new MMDB layout has no handle (as_name is left out), and the prefix is the MMDB network.
	m := jsonBody(t, get(s, "GET", "/json/203.0.113.9").Body.String())
	if m["asn"] != float64(64500) || m["prefix"] != "203.0.113.0/24" || m["as_name"] != nil || m["org"] != "Example Net" {
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
	for _, x := range []string{"/json/8.8.8.8", "/<country>-asn-list"} {
		if !strings.Contains(b, x) {
			t.Errorf("llms.txt lacks %s", x)
		}
	}
}

func TestIsCLI(t *testing.T) {
	for ua, want := range map[string]bool{
		"curl/8.7.1": true, "Wget/1.21": true, "HTTPie/3.2": true, "CURL/8": true,
		"Go-http-client/1.1": true, "Go-http-client/2.0": true, "python-requests/2.32.3": true, "Python-urllib/3.12": true,
		"python-httpx/0.27.0": true, "Python/3.12 aiohttp/3.9.5": true, "node": true,
		"node-fetch/1.0 (+https://github.com/bitinn/node-fetch)": true, "undici": true, "axios/1.7.2": true,
		"got (https://github.com/sindresorhus/got)": true, "Deno/1.45.0": true, "Bun/1.1.20": true,
		"Mozilla/5.0 Chrome/120.0 Safari/537.36 Edg/120.0": false, "Mozilla/5.0 (compatible; Googlebot/2.1)": false,
		"Mozilla/5.0 (Macintosh) AppleWebKit/605.1.15 Version/17 Safari/605.1.15": false,
		"Mozilla/5.0 Chrome/120.0 Safari/537.36":                                  false,
		"Mozilla/5.0 (X11; Linux) Gecko/20100101 Firefox/121.0":                   false,
		"": false, "libcurl-agent": false, "NodePing": false, "nodejs-monitor": false, "undici/6.0": true,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("User-Agent", ua)
		if isCLI(r) != want {
			t.Errorf("isCLI(%q) = %v", ua, !want)
		}
	}
}

func TestCLIHome(t *testing.T) {
	s := fixtureServer(t)
	const curl, chrome = "curl/8.7.1", "Mozilla/5.0 Chrome/120.0 Safari/537.36"
	w := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9", "User-Agent", curl)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Vary") != "User-Agent" ||
		!strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("curl /: %d %v", w.Code, w.Header())
	}
	if m := jsonBody(t, w.Body.String()); m["ip"] != "203.0.113.9" {
		t.Errorf("ip = %v", m["ip"])
	}
	r := httptest.NewRequest("HEAD", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Real-IP", "203.0.113.9")
	r.Header.Set("User-Agent", curl)
	hw := httptest.NewRecorder()
	s.ServeHTTP(hw, r)
	if hw.Code != 200 || hw.Body.Len() != 0 {
		t.Errorf("HEAD: %d %q", hw.Code, hw.Body)
	}
	w = fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9", "User-Agent", chrome)
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || w.Header().Get("Vary") != "User-Agent" {
		t.Errorf("browser /: %v", w.Header())
	}
	w = fromPeer(s, "/?203.0.113.9", "127.0.0.1:1", "X-Real-IP", "203.0.113.9", "User-Agent", curl)
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("curl lookup page: %v", w.Header())
	}
	w = fromPeer(s, "/", "127.0.0.1:1", "User-Agent", curl)
	if w.Code != 503 || jsonBody(t, w.Body.String())["error"] == nil {
		t.Errorf("unknown: %d %s", w.Code, w.Body)
	}
}

func TestCLIIPHosts(t *testing.T) {
	s := fillServer(t, u4, u6)
	const curl, chrome = "curl/8.7.1", "Mozilla/5.0 Chrome/120.0 Safari/537.36"
	for _, c := range []struct {
		url, ua string
		json    bool
	}{
		{"http://v4.openipdata.org/ip", curl, true},
		{"https://V6.OPENIPDATA.ORG:443/ip", curl, true},
		{"http://v4.openipdata.org/ip", chrome, false},
		{"http://openipdata.org/ip", curl, false},
	} {
		w := fromPeer(s, c.url, "127.0.0.1:1", "X-Real-IP", "203.0.113.9", "User-Agent", c.ua)
		if w.Header().Get("Vary") != "User-Agent" {
			t.Errorf("%s: no Vary", c.url)
		}
		if c.json {
			if w.Code != 200 || jsonBody(t, w.Body.String())["ip"] != "203.0.113.9" {
				t.Errorf("%s: %d %q", c.url, w.Code, w.Body)
			}
		} else if w.Body.String() != "203.0.113.9\n" {
			t.Errorf("%s %s: %q", c.url, c.ua, w.Body)
		}
	}
}

func TestCLIIPHostEdges(t *testing.T) {
	s := fillServer(t, u4, "http://[::1]/")
	for _, host := range []string{"[::1]", "[::1]:80"} {
		w := fromPeer(s, "http://"+host+"/ip", "127.0.0.1:1", "X-Real-IP", "203.0.113.9", "User-Agent", "curl/8")
		if w.Code != 200 || jsonBody(t, w.Body.String())["ip"] != "203.0.113.9" {
			t.Errorf("%s: %d %q", host, w.Code, w.Body)
		}
	}
	// A non-public address on the family host: 400 JSON.
	w := fromPeer(s, "http://v4.openipdata.org/ip", "127.0.0.1:1", "X-Real-IP", "10.0.0.1", "User-Agent", "curl/8")
	if w.Code != 400 || jsonBody(t, w.Body.String())["error"] != "not a public address" {
		t.Errorf("private: %d %q", w.Code, w.Body)
	}
}

// A command-line client on the family host never adds its address to a report.
func TestCLIIPHostSkipsReport(t *testing.T) {
	s, m := shareServer(t)
	id := share(t, s, "203.0.113.9", "")
	w := fromPeer(s, "http://v6.openipdata.org/ip?r="+id, "127.0.0.1:1", "X-Real-IP", "2001:db8::9", "User-Agent", "curl/8")
	if w.Code != 200 || jsonBody(t, w.Body.String())["ip"] != "2001:db8::9" {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
	if m.snapshot(t, id).Other != "" {
		t.Error("curl added other_ip")
	}
	// A browser on the same name does add it.
	fromPeer(s, "http://v6.openipdata.org/ip?r="+id, "127.0.0.1:1", "X-Real-IP", "2001:db8::9", "User-Agent", "Mozilla/5.0 Chrome/120")
	if m.snapshot(t, id).Other != "2001:db8::9" {
		t.Error("browser did not add other_ip")
	}
}

func TestIPJSONNewFields(t *testing.T) {
	nw := fixtureServer(t)
	m := jsonBody(t, get(nw, "GET", "/json/203.0.113.9").Body.String())
	if m["continent_code"] != "EU" || m["continent_name"] != "Europe" || m["in_eu"] != true || m["timezone"] != "Europe/Lisbon" {
		t.Errorf("new layout: %v", m)
	}
	// A country only (Spain, no city): in_eu is there, no city fields.
	m = jsonBody(t, get(nw, "GET", "/json/198.51.100.1").Body.String())
	if m["in_eu"] != true || m["city_source"] != nil || m["city_guess"] != nil || m["timezone"] != nil {
		t.Errorf("country only: %v", m)
	}
	// Nothing known: no in_eu.
	if m := jsonBody(t, get(nw, "GET", "/json/192.0.2.1").Body.String()); m["in_eu"] != nil || m["continent_code"] != nil {
		t.Errorf("unknown: %v", m)
	}
	// The old flat layout has no EU data: in_eu is left out, never false.
	fl, err := newServer("testdata/lookup-geo-flat.mmdb", "testdata/lookup-asn-flat.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	fl.public = nw.public
	m = jsonBody(t, get(fl, "GET", "/json/203.0.113.9").Body.String())
	if _, ok := m["in_eu"]; ok || m["city_name"] != "Lisbon" || m["continent_code"] != nil {
		t.Errorf("flat: %v", m)
	}
	// city_guess, from the record's source.
	for src, want := range map[string]bool{"70-geofeeds": false, "36-mlab": false, "80-asn-main": true, "89-asn-anywhere": true, "86-country-main": true} {
		if guessSources[src] != want {
			t.Errorf("%s: guess %v", src, guessSources[src])
		}
	}
}

func TestIPJSONCityGuessFromSource(t *testing.T) {
	s := fixtureServer(t)
	m := jsonBody(t, get(s, "GET", "/json/203.0.113.9").Body.String())
	if src, ok := m["city_source"].(string); ok {
		if g, ok := m["city_guess"].(bool); !ok || g != guessSources[src] {
			t.Errorf("city_guess %v for %q", m["city_guess"], src)
		}
	} else if _, has := m["city_guess"]; has {
		t.Errorf("city_guess without a source: %v", m)
	}
}
