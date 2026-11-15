package main

// Benchmarks of the cached routes (tiny temp fixtures, no real data) and the
// tests of their caching.

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func benchGet(b *testing.B, s http.Handler, target string) {
	b.Helper()
	b.ReportAllocs()
	r := httptest.NewRequest("GET", target, nil)
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			b.Fatalf("%s: %d", target, w.Code)
		}
	}
}

func benchServer(b *testing.B) *server {
	b.Helper()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	path := b.TempDir() + "/asn-openipdata.sqlite3"
	writeASNFile(b, path, fixtureRows())
	s, err := newServer("testdata/lookup-geo.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		b.Fatal(err)
	}
	s.useASN(path)
	return s
}

func BenchmarkDownload(b *testing.B)        { benchGet(b, benchServer(b), "/download") }
func BenchmarkSitemapIndex(b *testing.B)    { benchGet(b, benchServer(b), "/sitemap.xml") }
func BenchmarkSitemapNetworks(b *testing.B) { benchGet(b, benchServer(b), "/sitemap-networks-1.xml") }
func BenchmarkRobots(b *testing.B)          { benchGet(b, benchServer(b), "/robots.txt") }
func BenchmarkLLMs(b *testing.B)            { benchGet(b, benchServer(b), "/llms.txt") }

// The second request of every cached route returns the same bytes and its
// ETag gives 304.
func TestCachedRoutesRepeat(t *testing.T) {
	s, _ := asnServer(t)
	for _, target := range []string{"/download", "/sitemap.xml", "/sitemap-pages.xml", "/sitemap-networks-1.xml", "/sitemap-organizations-1.xml", "/robots.txt", "/llms.txt"} {
		a, b := get(s, "GET", target), get(s, "GET", target)
		if a.Code != 200 || a.Body.String() != b.Body.String() || a.Body.Len() == 0 {
			t.Errorf("%s: %d, bodies differ", target, a.Code)
		}
		if target == "/robots.txt" || target == "/llms.txt" {
			continue
		}
		etag := a.Header().Get("ETag")
		if etag == "" || etag != b.Header().Get("ETag") {
			t.Errorf("%s: etag %q", target, etag)
		}
		if w := get(s, "GET", target, "If-None-Match", etag); w.Code != 304 || w.Body.Len() != 0 {
			t.Errorf("%s: If-None-Match gives %d", target, w.Code)
		}
		if w := get(s, "GET", target, "If-None-Match", `"other"`); w.Code != 200 {
			t.Errorf("%s: other etag gives %d", target, w.Code)
		}
	}
}

func TestSitemapFollowsReload(t *testing.T) {
	s, path := asnServer(t)
	before := get(s, "GET", "/sitemap-networks-1.xml")
	orgBefore := get(s, "GET", "/sitemap-organizations-1.xml")
	if strings.Contains(before.Body.String(), "/as64999") {
		t.Fatal("already there")
	}
	rows := append(fixtureRows(), fixtureRow{asn: 64999, handle: "NEW-NET", org: "New Net", slug: "new-net", cc: "IT", v4: 99, country: "Italy", v4Prefixes: []string{"203.0.113.0/24"}})
	writeASNFile(t, path, rows)
	os.Chtimes(path, time.Now(), time.Now().Add(time.Hour))
	s.check()
	after := get(s, "GET", "/sitemap-networks-1.xml")
	if !strings.Contains(after.Body.String(), "https://openipdata.org/as64999<") || after.Header().Get("ETag") == before.Header().Get("ETag") {
		t.Error("the networks sitemap does not show the new network")
	}
	if o := get(s, "GET", "/sitemap-organizations-1.xml"); !strings.Contains(o.Body.String(), "/asn-new-net<") || o.Header().Get("ETag") == orgBefore.Header().Get("ETag") {
		t.Error("the organizations sitemap does not show the new organization")
	}
	if strings.Contains(get(s, "GET", "/sitemap.xml").Body.String(), "as64999") {
		t.Error("index lists a network")
	}
}

// /download is rendered again only when what it shows changes.
func TestDownloadRerendersOnKeyChange(t *testing.T) {
	s, path := asnServer(t)
	a := get(s, "GET", "/download")
	if b := get(s, "GET", "/download"); b.Header().Get("ETag") != a.Header().Get("ETag") {
		t.Error("etag changed without a change")
	}
	first := s.dl.pg
	get(s, "GET", "/download")
	if s.dl.pg != first {
		t.Error("rendered again without a change")
	}
	// A reloaded file of the same shown size (0.1 MB) is the same page.
	rows := append(fixtureRows(), fixtureRow{asn: 64999, handle: "NEW-NET", org: "New Net", slug: "new-net", cc: "IT", v4: 99, country: "Italy", v4Prefixes: []string{"203.0.113.0/24"}})
	writeASNFile(t, path, rows)
	os.Chtimes(path, time.Now(), time.Now().Add(time.Hour))
	s.check()
	if get(s, "GET", "/download"); s.dl.pg != first {
		t.Error("rendered again for the same shown values")
	}
	// A new origin changes the canonical link.
	s.origin = "https://example.org"
	if d := get(s, "GET", "/download"); !strings.Contains(d.Body.String(), `href="https://example.org/download"`) || s.dl.pg == first {
		t.Error("origin change not shown")
	}
}
