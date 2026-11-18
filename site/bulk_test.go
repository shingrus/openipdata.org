package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// bulkServer is the ASN fixture server with the documentation addresses public.
func bulkServer(t *testing.T) *server {
	t.Helper()
	s, _ := asnServer(t)
	s.public = func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsPrivate() }
	return s
}

func post(s http.Handler, target, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func form(k, v string) string { return k + "=" + url.QueryEscape(v) }

func strs(a []netip.Addr) []string {
	var o []string
	for _, x := range a {
		o = append(o, x.String())
	}
	return o
}

func TestExtractAddrs(t *testing.T) {
	s := bulkServer(t)
	type want struct {
		addrs   string
		np, inv int
		more    bool
	}
	for _, c := range []struct {
		name, in string
		max      int
		w        want
	}{
		{"log lines", `203.0.113.9 - - [17/Nov/2026:10:11:12 +0000] "GET /x" 198.51.100.1:443 "-"` + "\n" + `time 12:30:01 and 10.1.2.3`, 1000, want{"[203.0.113.9 198.51.100.1]", 1, 0, false}},
		{"ports and brackets", "[2001:db8::5]:443, 203.0.113.9:8080|192.0.2.1", 1000, want{"[2001:db8::5 203.0.113.9 192.0.2.1]", 0, 0, false}},
		{"dedupe order", "203.0.113.9 198.51.100.1 203.0.113.9 ::ffff:198.51.100.1 10.0.0.1 10.0.0.1", 1000, want{"[203.0.113.9 198.51.100.1]", 1, 0, false}},
		{"trailing dot and separators", "ip=203.0.113.9. a,b;198.51.100.1/24 'x'", 1000, want{"[203.0.113.9 198.51.100.1]", 0, 0, false}},
		{"invalid", "999.1.1.1 1.2.3.4.5 1.2.3 12345 2001:db8::5::6 dead:beef:cafe hello deadbeef 12:30:01 12:30", 1000, want{"[]", 0, 3, false}},
		{"cap", "203.0.113.1 203.0.113.2 203.0.113.3", 2, want{"[203.0.113.1 203.0.113.2]", 0, 0, true}},
		{"cap exact", "203.0.113.1 203.0.113.2", 2, want{"[203.0.113.1 203.0.113.2]", 0, 0, false}},
		{"empty", "", 10, want{"[]", 0, 0, false}},
	} {
		a, np, inv, more := s.extractAddrs(c.in, c.max)
		got := want{fmt.Sprint(strs(a)), np, inv, more}
		if got != c.w {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.w)
		}
	}
}

func TestExtractAddrsCap1000(t *testing.T) {
	s := bulkServer(t)
	var b strings.Builder
	for i := 0; i < 1100; i++ {
		fmt.Fprintf(&b, "20.%d.%d.1\n", i/250, i%250)
	}
	a, _, _, more := s.extractAddrs(b.String(), bulkMax)
	if len(a) != 1000 || !more {
		t.Errorf("%d addresses, more %v", len(a), more)
	}
}

func TestBulkGetPage(t *testing.T) {
	s := bulkServer(t)
	w := get(s, "GET", "/bulk-ip-lookup")
	body := w.Body.String()
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=300" || w.Header().Get("ETag") == "" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	if len(bulkDesc) >= 155 {
		t.Errorf("description is %d", len(bulkDesc))
	}
	for _, want := range []string{
		"<title>" + bulkTitle + "</title>", `<meta name="description" content="` + bulkDesc + `">`,
		`<link rel="canonical" href="https://openipdata.org/bulk-ip-lookup">`,
		`<form class="bf" method="post" action="/bulk-ip-lookup">`, `<textarea name="ips"`, `name="format" value="csv"`,
		"Up to 1000 addresses. IPv4 and IPv6.", "<h2>API</h2>", "<pre>curl https://openipdata.org/json/1.1.1.1,8.8.8.8</pre>",
		"<pre>curl --data-binary @ips.txt https://openipdata.org/json</pre>", `class="cp"`,
		`<a href="/bulk-ip-lookup">Bulk lookup</a>`, `<form class="lookup" method="post" action="/">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "noindex") {
		t.Error("noindex")
	}
	i := strings.Index(body, `<script type="application/ld+json">`)
	j := strings.Index(body[i:], "</script>")
	var ld map[string]any
	if i < 0 || json.Unmarshal([]byte(body[i+len(`<script type="application/ld+json">`):i+j]), &ld) != nil || ld["@type"] != "WebApplication" ||
		ld["url"] != "https://openipdata.org/bulk-ip-lookup" || ld["applicationCategory"] != "DeveloperApplication" {
		t.Errorf("json-ld: %v", ld)
	}
	if w2 := get(s, "GET", "/bulk-ip-lookup", "If-None-Match", w.Header().Get("ETag")); w2.Code != 304 {
		t.Errorf("If-None-Match: %d", w2.Code)
	}
	if w2 := get(s, "HEAD", "/bulk-ip-lookup"); w2.Code != 200 || w2.Body.Len() != 0 {
		t.Errorf("HEAD: %d", w2.Code)
	}
	if w2 := get(s, "PUT", "/bulk-ip-lookup"); w2.Code != 405 || w2.Header().Get("Allow") != "GET, HEAD, POST" {
		t.Errorf("PUT: %d %v", w2.Code, w2.Header())
	}
}

func TestBulkPost(t *testing.T) {
	s := bulkServer(t)
	text := "203.0.113.9 and 198.51.100.1, 10.1.2.3 999.1.1.1 203.0.113.9"
	w := post(s, "/bulk-ip-lookup", form("ips", text))
	body := w.Body.String()
	h := w.Header()
	if w.Code != 200 || h.Get("Cache-Control") != "no-store" || !strings.Contains(h.Get("Content-Security-Policy"), "script-src") {
		t.Fatalf("%d %v", w.Code, h)
	}
	for _, want := range []string{
		`<meta name="robots" content="noindex">`, "2 found · 1 not public · 1 invalid",
		">203.0.113.9\n198.51.100.1</textarea>", "<h2>Results</h2>", `<pre class="tsv" hidden>ip` + "\t" + "country_code", "<th>Coordinates</th>",
		"\n203.0.113.9\tPT\tPortugal\tLisbon\t",
		"\tEXAMPLE-NET\n198.51.100.1\t",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, bad := range []string{"rel=\"canonical\"", `name="description"`, "ld+json", "10.1.2.3"} {
		if strings.Contains(body, bad) {
			t.Errorf("page has %q", bad)
		}
	}
	// the row of the Lisbon address: lat and lon present, asn 64500.
	row := ""
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "203.0.113.9\t") {
			row = l
		}
	}
	f := strings.Split(row, "\t")
	if len(f) != 8 || f[6] != "64500" || f[4] == "" || f[5] == "" {
		t.Errorf("row %q", row)
	}
	// A country-only address has no lat/lon.
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "198.51.100.1\t") {
			if f := strings.Split(l, "\t"); len(f) != 8 || f[4] != "" || f[5] != "" || f[3] != "" {
				t.Errorf("country row %q", l)
			}
		}
	}
}

func TestBulkNoneFound(t *testing.T) {
	s := bulkServer(t)
	w := post(s, "/bulk-ip-lookup", form("ips", "nothing here 10.0.0.1"))
	b := w.Body.String()
	if w.Code != 200 || !strings.Contains(b, "No public addresses found") || strings.Contains(b, "<h2>Results</h2>") {
		t.Errorf("%d", w.Code)
	}
	w = post(s, "/bulk-ip-lookup", form("ips", "")+"&format=csv")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "No public addresses found") {
		t.Errorf("csv none: %d", w.Code)
	}
}

func TestBulkCSV(t *testing.T) {
	s := bulkServer(t)
	w := post(s, "/bulk-ip-lookup", form("ips", "203.0.113.9\n198.51.100.1")+"&format=csv")
	h := w.Header()
	if w.Code != 200 || h.Get("Content-Type") != "text/csv; charset=utf-8" || h.Get("Content-Disposition") != `attachment; filename="ip-geolocation.csv"` ||
		h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%d %v", w.Code, h)
	}
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil || len(rows) != 3 || strings.Join(rows[0], ",") != "ip,country_code,country,city,lat,lon,asn,as_name" {
		t.Fatalf("%v %v", err, rows)
	}
	if rows[1][0] != "203.0.113.9" || rows[1][1] != "PT" || rows[1][3] != "Lisbon" || rows[1][6] != "64500" || rows[2][0] != "198.51.100.1" {
		t.Errorf("rows %v", rows)
	}
}

func TestBulkTooLarge(t *testing.T) {
	s := bulkServer(t)
	for _, target := range []string{"/bulk-ip-lookup", "/"} {
		w := post(s, target, "ips="+strings.Repeat("1", 300<<10))
		if w.Code != 413 {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
}

func TestBulkEscapesInput(t *testing.T) {
	s := bulkServer(t)
	w := post(s, "/bulk-ip-lookup", form("ips", `</textarea><script>alert(1)</script> 203.0.113.9 203.0.113.10`))
	b := w.Body.String()
	if strings.Contains(b, "<script>alert") || strings.Contains(b, "</textarea><script") {
		t.Error("hostile text is not escaped")
	}
}

func TestJSONList(t *testing.T) {
	s := bulkServer(t)
	w := get(s, "GET", "/json/203.0.113.9,10.1.2.3,203.0.113.9,198.51.100.1")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=300" || w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	var arr []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil || len(arr) != 4 {
		t.Fatalf("%v %s", err, w.Body)
	}
	if arr[0]["ip"] != "203.0.113.9" || arr[0]["city_name"] != "Lisbon" || arr[1]["error"] != "not a public address" || arr[1]["ip"] != "10.1.2.3" ||
		arr[2]["ip"] != "203.0.113.9" || arr[3]["ip"] != "198.51.100.1" {
		t.Errorf("%v", arr)
	}
	// An item that does not parse: no redirect, an entry with the input.
	w = get(s, "GET", "/json/203.0.113.9,"+strings.Repeat("x", 100)+",1.2.3")
	if w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	arr = nil
	json.Unmarshal(w.Body.Bytes(), &arr)
	if len(arr) != 3 || arr[1]["input"] != strings.Repeat("x", 64) || arr[1]["error"] != "not an IP address" || arr[2]["input"] != "1.2.3" {
		t.Errorf("%v", arr)
	}
	// Another spelling: one 301 to the canonical list.
	w = get(s, "GET", "/json/::ffff:203.0.113.9,2001:DB8::5")
	if w.Code != 301 || w.Header().Get("Location") != "/json/203.0.113.9,2001:db8::5" || w.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Errorf("%d %v", w.Code, w.Header())
	}
	if w := get(s, "GET", "/json/203.0.113.9%2C198.51.100.1"); w.Code != 301 || w.Header().Get("Location") != "/json/203.0.113.9,198.51.100.1" {
		t.Errorf("%d %v", w.Code, w.Header())
	}
	// Limits and errors.
	many := strings.TrimSuffix(strings.Repeat("203.0.113.9,", 101), ",")
	if w := get(s, "GET", "/json/"+many); w.Code != 400 || w.Body.String() != `{"error":"at most 100 addresses"}`+"\n" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("101: %d %q", w.Code, w.Body)
	}
	if w := get(s, "GET", "/json/"+strings.TrimSuffix(strings.Repeat("203.0.113.9,", 100), ",")); w.Code != 200 {
		t.Errorf("100: %d", w.Code)
	}
	for _, p := range []string{"/json/203.0.113.9,", "/json/,203.0.113.9", "/json/203.0.113.9,,198.51.100.1"} {
		if w := get(s, "GET", p); w.Code != 400 || w.Body.String() != `{"error":"not an IP address"}`+"\n" {
			t.Errorf("%s: %d %q", p, w.Code, w.Body)
		}
	}
	if w := get(s, "GET", "/json/203.0.113.9,198.51.100.1/x"); w.Code != 404 {
		t.Errorf("slash: %d", w.Code)
	}
	// One address: the old object, not an array.
	if w := get(s, "GET", "/json/203.0.113.9"); !strings.HasPrefix(w.Body.String(), `{"ip":"203.0.113.9"`) {
		t.Errorf("single: %q", w.Body)
	}
}

func TestJSONPost(t *testing.T) {
	s := bulkServer(t)
	postRaw := func(target, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", target, strings.NewReader(body))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	for _, body := range []string{"203.0.113.9\n10.1.2.3, 198.51.100.1", `["203.0.113.9","10.1.2.3","198.51.100.1"]`, " [ \"203.0.113.9\" , \"10.1.2.3\", \"198.51.100.1\" ] "} {
		w := postRaw("/json", body)
		var arr []map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &arr) != nil || len(arr) != 3 || arr[0]["city_name"] != "Lisbon" || arr[1]["error"] != "not a public address" ||
			w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%q: %d %v %s", body, w.Code, w.Header(), w.Body)
		}
	}
	for _, c := range []struct {
		body string
		code int
		msg  string
	}{
		{`[1,2]`, 400, "bad JSON"}, {`["a"`, 400, "bad JSON"}, {"  \n ", 400, "no addresses"}, {"[]", 400, "no addresses"},
		{strings.Repeat("203.0.113.9 ", 1001), 400, "at most 1000 addresses"},
		{strings.Repeat("1", 70<<10), 413, "body too large"},
	} {
		w := postRaw("/json", c.body)
		if w.Code != c.code || w.Body.String() != `{"error":"`+c.msg+`"}`+"\n" {
			t.Errorf("%.20q: %d %q", c.body, w.Code, w.Body)
		}
	}
	if w := postRaw(strings.Repeat("203.0.113.9 ", 1000)[:0]+"/json", strings.Repeat("203.0.113.9 ", 1000)); w.Code != 200 {
		t.Errorf("1000: %d", w.Code)
	}
	if w := postRaw("/json/203.0.113.9", "x"); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /json/ip: %d %v", w.Code, w.Header())
	}
}

func TestJSONOptions(t *testing.T) {
	s := bulkServer(t)
	for _, p := range []string{"/json", "/json/203.0.113.9", "/json/1.1.1.1,8.8.8.8"} {
		w := get(s, "OPTIONS", p)
		h := w.Header()
		if w.Code != 204 || h.Get("Access-Control-Allow-Origin") != "*" || h.Get("Access-Control-Allow-Methods") != "GET, HEAD, POST" ||
			h.Get("Access-Control-Allow-Headers") != "Content-Type" || h.Get("Access-Control-Max-Age") != "86400" || w.Body.Len() != 0 {
			t.Errorf("%s: %d %v", p, w.Code, h)
		}
	}
	if w := get(s, "OPTIONS", "/"); w.Code != 405 {
		t.Errorf("OPTIONS /: %d", w.Code)
	}
	if w := get(s, "OPTIONS", "/download"); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("OPTIONS /download: %d %v", w.Code, w.Header())
	}
}

func TestPostHome(t *testing.T) {
	s := bulkServer(t)
	w := post(s, "/", form("q", "203.0.113.9, 198.51.100.1"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "2 found") || !strings.Contains(w.Body.String(), `content="noindex"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("several: %d", w.Code)
	}
	// One public and one private address is still a bulk answer.
	if w := post(s, "/", form("q", "203.0.113.9 10.1.2.3")); w.Code != 200 || !strings.Contains(w.Body.String(), "1 found · 1 not public") {
		t.Errorf("mixed: %d", w.Code)
	}
	for _, c := range []struct{ q, loc string }{
		{"203.0.113.9", "/?203.0.113.9"},
		{" ::FFFF:203.0.113.9 ", "/?203.0.113.9"},
		{"2001:DB8::5", "/?2001:db8::5"},
		{"10.1.2.3", "/?10.1.2.3"},
		{"AS64500", "/as64500"},
		{"015169", "/as15169"},
		{"203.0.113.0/24", "/prefix/203.0.113.0/24"},
		{"", "/"},
		{"   ", "/"},
		{"example net", "/?q=example+net"},
		{"1.2.3", "/?q=1.2.3"},
		{"AS0", "/?q=AS0"},
	} {
		w := post(s, "/", form("q", c.q))
		if w.Code != 303 || w.Header().Get("Location") != c.loc || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%q: %d %q", c.q, w.Code, w.Header().Get("Location"))
		}
	}
	// What the redirect points at is what GET gives.
	if w := get(s, "GET", "/?203.0.113.9"); w.Code != 200 {
		t.Errorf("GET: %d", w.Code)
	}
}

func TestBulkRoutes405(t *testing.T) {
	s := bulkServer(t)
	for _, c := range []struct{ m, p, allow string }{
		{"PUT", "/", "GET, HEAD, POST"}, {"DELETE", "/bulk-ip-lookup", "GET, HEAD, POST"}, {"POST", "/download", "GET, HEAD"},
		{"POST", "/as64500", "GET, HEAD"}, {"PUT", "/json/1.1.1.1", "GET, HEAD"},
	} {
		if w := get(s, c.m, c.p); w.Code != 405 || w.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s: %d %q", c.m, c.p, w.Code, w.Header().Get("Allow"))
		}
	}
}

func TestBulkSEO(t *testing.T) {
	s := bulkServer(t)
	if b := get(s, "GET", "/sitemap-pages.xml").Body.String(); !strings.Contains(b, "<loc>https://openipdata.org/bulk-ip-lookup</loc>") {
		t.Error("sitemap")
	}
	l := get(s, "GET", "/llms.txt").Body.String()
	for _, want := range []string{"/bulk-ip-lookup", "/json/1.1.1.1,8.8.8.8", "-s --data-binary @ips.txt https://openipdata.org/json"} {
		if !strings.Contains(l, want) {
			t.Errorf("llms.txt lacks %q", want)
		}
	}
}

func TestCellTextGuardsFormulas(t *testing.T) {
	for in, want := range map[string]string{"=HYPERLINK(1)": "'=HYPERLINK(1)", "+1": "'+1", "-Reserved AS-": "'-Reserved AS-", "@x": "'@x", "GOOGLE": "GOOGLE", "": ""} {
		if got := cellText(in); got != want {
			t.Errorf("cellText(%q) = %q, want %q", in, got, want)
		}
	}
}

// The review findings of 2026-11-17: a list that holds no public address stays
// off the URL, a long text is never redirected, and POST /json/ is not served.
func TestBulkReviewFixes(t *testing.T) {
	s := bulkServer(t)
	for _, q := range []string{"10.0.0.1\n10.0.0.2\n192.168.1.1", "203.0.113.9 1.1.1.300", strings.Repeat("word ", 30)} {
		req := httptest.NewRequest("POST", "/", strings.NewReader(url.Values{"q": {q}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != 200 || w.Header().Get("Location") != "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("POST / %q: %d %q, want the bulk page", q, w.Code, w.Header().Get("Location"))
		}
	}
	req := httptest.NewRequest("POST", "/json/", strings.NewReader("203.0.113.9"))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /json/: %d, want 405", w.Code)
	}
	if got := cellText("\t=1"); got != "'\t=1" {
		t.Errorf("cellText tab: %q", got)
	}
}
