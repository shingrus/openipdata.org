package main

// Tests of the lookup box: AS numbers, names, and the order in which input is read.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Google LLC", "google-llc"},
		{"  Deutsche   Telekom ", "deutsche-telekom"},
		{"AT&T Services, Inc.", "at-t-services-inc"},
		{"a--b__c", "a-b-c"},
		{"---x---", "x"},
		{"example.com", "example-com"},
		{"Über", "ber"},
		{"é", ""},
		{"!!!", ""},
		{"", ""},
		{"abc123", "abc123"},
		{strings.Repeat("a", 100), strings.Repeat("a", 64)},
		{strings.Repeat("a", 63) + " b", strings.Repeat("a", 63)}, // cut at 64 leaves a trailing "-": trimmed
		{strings.Repeat("a", 62) + " b", strings.Repeat("a", 62) + "-b"},
	} {
		if got := slugify(c.in); got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseQueryOrder(t *testing.T) {
	for _, c := range []struct {
		raw, kind, val string // val: the address, the AS number or the term
	}{
		// 1. an IP address
		{"8.8.8.8", "lookup", "8.8.8.8"},
		{"q=8.8.8.8", "redirect", "8.8.8.8"},
		{"2001:db8::1", "lookup", "2001:db8::1"},
		// 2. AS<number>, 3. a bare number
		{"q=AS15169", "asn", "15169"},
		{"q=as015169", "asn", "15169"},
		{"AS15169", "asn", "15169"},
		{"q=15169", "asn", "15169"},
		{"15169", "asn", "15169"},
		{"q=0015169", "asn", "15169"},
		{"q=4294967295", "asn", "4294967295"},
		{"q=4294967296", "asn", ""},
		{"q=0", "asn", ""},
		{"q=000", "asn", ""},
		{"q=AS0", "asn", ""},
		{"q=" + strings.Repeat("9", 90), "asn", ""},
		// An attempt at an address is never a name: colon, or digits, dots and slashes with a dot.
		{"q=1.2.3", "bad", ""},
		{"1.2.3", "bad", ""},
		{"q=1.1.1.1:80", "bad", ""},
		{"q=fe80::1%25eth0", "bad", ""},
		{"q=1.1.1.1/24", "bad", ""},
		{"q=1.2.3.4.5", "bad", ""},
		{"q=2001:db8::zz", "bad", ""},
		{"q=AS15169:1", "bad", ""},
		// 4. a name
		{"q=google", "search", "google"},
		{"q=Google+LLC", "search", "google-llc"},
		{"q=google%20llc", "search", "google-llc"},
		{"google", "search", "google"},
		{"q=example.com", "search", "example-com"},
		{"q=AS", "search", "as"},
		{"q=AS+15169", "search", "as-15169"},
		{"q=64500x", "search", "64500x"},
		{"q=1.5g", "search", "1-5g"}, // has a letter: a name
		{"q=ab", "search", "ab"},
		// too short, or nothing to search for
		{"q=a", "bad", ""},
		{"q=%21%21", "bad", ""},
		{"q=%C3%A9", "bad", ""},
		{"q=-a-", "bad", ""},
	} {
		q := parseQuery(c.raw)
		got := ""
		switch q.kind {
		case "lookup", "redirect":
			got = q.addr.String()
			if !q.addr.IsValid() {
				got = ""
			}
		case "asn":
			got = q.asn
		case "search":
			got = q.term
		}
		if q.kind != c.kind || got != c.val {
			t.Errorf("%q: got %s %q, want %s %q", c.raw, q.kind, got, c.kind, c.val)
		}
	}
	// canon: only exactly q=<term> is the canonical URL.
	for raw, want := range map[string]bool{"q=google-llc": true, "q=google+llc": false, "q=Google-LLC": false, "google-llc": false, "q=google-llc&x=1": false} {
		if q := parseQuery(raw); q.kind != "search" || q.canon != want {
			t.Errorf("%q: canon %v kind %s, want %v", raw, q.canon, q.kind, want)
		}
	}
}

func searchRows() []fixtureRow {
	row := func(asn int64, handle, org, slug string, v4 int64) fixtureRow {
		return fixtureRow{asn: asn, handle: handle, org: org, slug: slug, cc: "PT", v4: v4, country: "Portugal"}
	}
	rows := []fixtureRow{
		row(64500, "ACME-1", "Acme", "acme", 10),
		row(64501, "ACMEC", "Acme Cloud", "acme-cloud", 50),
		row(64502, "ACMEH", "Acme Hosting", "acme-hosting", 5),
		row(64503, "SUPER", "Super Acme", "super-acme", 500),
		row(64504, "BIGACME", "Bigacme Corp", "bigacme-corp", 900),
		row(64505, "ACME", "Tiny Corp", "tiny-corp", 1), // the handle ACME: its organization comes first
		row(64506, "LONEHANDLE", "Zzz Holdings", "zzz-holdings", 1),
		row(64507, "OTHER", "Quokka Systems", "quokka-systems", 7),
		row(64508, "NOSLUG", "No Slug Org", "", 99),
		row(64509, "ZERO", "Acme Zero", "acme-zero", 0), // no addresses: still found, last of its group
		// "telecom": a common word. The largest "Telecom ..." comes first, the
		// small organization that owns the handle TELECOM stays in the first group by its size.
		row(64510, "TELECOM", "Zeta Net", "zeta-net", 2),
		row(64511, "TLC-GIANT", "Telecom Giant", "telecom-giant", 800000),
		row(64512, "TLC-SMALL", "Telecom Small", "telecom-small", 20),
		row(64513, "MEGA", "Mega Telecom", "mega-telecom", 9000),
		row(64514, "XTLC", "Xtelecom Inc", "xtelecom-inc", 90000),
	}
	// 60 organizations that all contain "bulk" and 1 address more each, to cap the page.
	for i := 0; i < 60; i++ {
		rows = append(rows, row(int64(70000+i), fmt.Sprintf("B%d", i), fmt.Sprintf("Bulk Org %02d", i), fmt.Sprintf("bulk-org-%02d", i), int64(100+i)))
	}
	return rows
}

func searchServer(t *testing.T) *server {
	t.Helper()
	s, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "asn.sqlite3")
	writeASNFile(t, path, searchRows())
	s.useASN(path)
	return s
}

var rowSlug = regexp.MustCompile(`<td><a href="/asn-([a-z0-9-]+)">`)

func slugsOf(body string) []string {
	var out []string
	for _, m := range rowSlug.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestSearchOrder(t *testing.T) {
	s := searchServer(t)
	for _, c := range []struct {
		term string
		want []string
	}{
		// group 1 (equal, starts with it, or the handle ACME: tiny-corp), then group 2 (word start), then group 3 (contains); each by IPv4 addresses
		{"acme", []string{"acme-cloud", "acme", "acme-hosting", "tiny-corp", "acme-zero", "super-acme", "bigacme-corp"}},
		// a big "telecom..." organization first, though a small one owns the handle; xtelecom-inc only contains it
		{"telecom", []string{"telecom-giant", "telecom-small", "zeta-net", "mega-telecom", "xtelecom-inc"}},
		{"cor", []string{"bigacme-corp", "tiny-corp"}}, // word start (tiny-corp: "-corp" no, "cor" starts "corp") both are word starts: more IPv4 first
	} {
		w := get(s, "GET", "/?q="+c.term)
		if w.Code != 200 {
			t.Fatalf("%s: status %d", c.term, w.Code)
		}
		got := slugsOf(w.Body.String())
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %v, want %v", c.term, got, c.want)
		}
	}
}

func TestSearchAnswers(t *testing.T) {
	s := searchServer(t)
	for _, c := range []struct {
		name, target string
		code         int
		loc          string   // Location of a redirect
		want, not    []string // in the body
	}{
		{"one match", "/?q=quokka", 302, "/asn-quokka-systems", nil, nil},
		{"one match, spelled differently", "/?q=Quokka+SYSTEMS", 302, "/asn-quokka-systems", nil, nil},
		{"one match by handle only", "/?q=lonehandle", 302, "/asn-zzz-holdings", nil, nil},
		{"handle in any case", "/?q=LoneHandle", 302, "/asn-zzz-holdings", nil, nil},
		{"raw form", "/?quokka", 302, "/asn-quokka-systems", nil, nil},
		{"canonical redirect, spelling", "/?q=ACME", 302, "/?q=acme", nil, nil},
		{"canonical redirect, extra parameter", "/?q=acme&x=1", 302, "/?q=acme", nil, nil},
		{"canonical redirect, raw form", "/?acme", 302, "/?q=acme", nil, nil},
		{"canonical redirect, space", "/?q=acme+cloud", 302, "/asn-acme-cloud", nil, nil}, // one match: straight to the page
		{"results", "/?q=acme", 200, "", []string{"<h1>Search</h1>", `<span class="r">acme</span>`, `value="acme"`, "<th>Organization</th>", "Acme Cloud", "IPv4 addresses", `href="/asn-super-acme"`}, []string{"more:", "Nothing found"}},
		{"cap and note", "/?q=bulk", 200, "", []string{"10 more: type more letters", `value="bulk"`}, nil},
		{"no match", "/?q=zzzzqqq", 404, "", []string{"<h1>Search</h1>", `<span class="r">zzzzqqq</span>`, "Nothing found", `value="zzzzqqq"`}, []string{"<table"}},
		{"no slug is not found", "/?q=no-slug", 404, "", []string{"Nothing found"}, nil},
		{"short", "/?q=a", 400, "", []string{"Not an IP address, AS number or name"}, []string{"Nothing found"}},
		{"symbols only", "/?q=%21%3F", 400, "", []string{"Not an IP address, AS number or name"}, nil},
		{"escaped input", "/?q=%3Cb%3Eacme%3C/b%3E", 302, "/?q=b-acme-b", nil, nil},
		{"AS number", "/?q=64500", 302, "/as64500", nil, nil},
	} {
		w := get(s, "GET", c.target)
		if w.Code != c.code {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.code)
			continue
		}
		if c.loc != "" && w.Header().Get("Location") != c.loc {
			t.Errorf("%s: Location %q, want %q", c.name, w.Header().Get("Location"), c.loc)
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
	}

	// The cap: 50 rows, the highest IPv4 first.
	w := get(s, "GET", "/?q=bulk")
	got := slugsOf(w.Body.String())
	if len(got) != maxResults || got[0] != "bulk-org-59" || got[maxResults-1] != "bulk-org-10" {
		t.Errorf("cap: %d rows, first %q last %q", len(got), got[0], got[len(got)-1])
	}
}

func TestSearchCacheAndHead(t *testing.T) {
	s := searchServer(t)
	for _, c := range []struct {
		target string
		code   int
		cache  string
	}{
		{"/?q=acme", 200, "public, max-age=300"},
		{"/?q=zzzzqqq", 404, "public, max-age=300"},
		{"/?q=quokka", 302, "public, max-age=300"},
		{"/?q=ACME", 302, "public, max-age=300"},
		{"/?q=15169", 302, "public, max-age=300"},
		{"/?q=0", 404, "public, max-age=300"},
		{"/?q=a", 400, "no-store"},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, c.target, nil)
			r.RemoteAddr = "127.0.0.1:1"
			r.Header.Set("X-Real-IP", "198.51.100.77")
			s.ServeHTTP(w, r)
			if w.Code != c.code || w.Header().Get("Cache-Control") != c.cache {
				t.Errorf("%s %s: %d %q", method, c.target, w.Code, w.Header().Get("Cache-Control"))
			}
			if method == http.MethodHead && w.Body.Len() != 0 {
				t.Errorf("HEAD %s: body", c.target)
			}
			if w.Header().Get("ETag") != "" {
				t.Errorf("%s %s: ETag", method, c.target)
			}
			if strings.Contains(w.Body.String(), "198.51.100.77") {
				t.Errorf("%s %s: visitor address in the page", method, c.target)
			}
		}
	}
}

func TestSearchNotLoaded(t *testing.T) {
	s := fixtureServer(t) // no ASN data
	for _, target := range []string{"/?q=google", "/?google", "/?q=4294967296", "/?q=0"} {
		w := get(s, "GET", target)
		if w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "ASN data is not loaded") {
			t.Errorf("%s: status %d", target, w.Code)
		}
	}
	// IP lookups and the bad answer still work, and an in-range number is still a redirect.
	if w := get(s, "GET", "/?q=8.8.8.8"); w.Code != 302 {
		t.Errorf("ip: %d", w.Code)
	}
	if w := get(s, "GET", "/?q=1.2.3"); w.Code != 400 {
		t.Errorf("bad: %d", w.Code)
	}
	if w := get(s, "GET", "/?q=15169"); w.Code != 302 || w.Header().Get("Location") != "/as15169" {
		t.Errorf("number: %d", w.Code)
	}
}

func TestSearchHint(t *testing.T) {
	s := searchServer(t)
	body := get(s, "GET", "/?q=acme").Body.String()
	if !strings.Contains(body, `placeholder="IP, AS or name"`) {
		t.Error("hint")
	}
}

// BenchmarkSearch measures one search over 100,000 organizations (more than
// the 84,571 of the real data).
func BenchmarkSearch(b *testing.B) {
	d := &asnData{byHandle: map[string]int32{}}
	words := []string{"net", "telecom", "cloud", "host", "data", "group", "systems", "online", "global", "comm"}
	for i := 0; i < 100000; i++ {
		slug := fmt.Sprintf("%s-%s-%d", words[i%10], words[(i/10)%10], i)
		d.orgs = append(d.orgs, orgRow{slug: slug, name: slug, v4: int64(100000 - i), n: 1})
	}
	for _, term := range []string{"telecom", "net", "zzzzqqq", "co"} {
		b.Run(term, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				d.searchOrgs(term)
			}
		})
	}
}
