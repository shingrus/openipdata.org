package main

import (
	"net/http"
	"testing"
)

func TestMovedPaths(t *testing.T) {
	s, _ := asnServer(t)
	for _, c := range []struct{ target, loc string }{
		{"/as64500/", "/as64500"},
		{"/asn-example-net/", "/asn-example-net"},
		{"/top-asn-by-ipv6/", "/top-asn-by-ipv6"},
		{"/download/", "/download"},
		{"/download/?a=b", "/download?a=b"},
		{"/asn-Example-Net", "/asn-example-net"},
		{"/asn-Example%20Net", "/asn-example-net"},
		{"/asn-example%20%20net", "/asn-example-net"},
	} {
		want := c.loc
		for _, m := range []string{"GET", "HEAD"} {
			w := get(s, m, c.target)
			if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != want || w.Header().Get("Cache-Control") != asnCache {
				t.Errorf("%s %s = %d %q %q, want 301 %q", m, c.target, w.Code, w.Header().Get("Location"), w.Header().Get("Cache-Control"), want)
			}
		}
	}
}

func TestMovedNotRedirected(t *testing.T) {
	s, _ := asnServer(t)
	for _, target := range []string{"//evil.example/", "/%5Cevil.example/", "/a%20b/", "/a/b/", "/nope", "/", "/asn-nope", "/asn-Example_Net!", "/asn-NoPe%20Corp", "/asn-", "/free-geo-ip/x", "/free-geo-ip-database", "/top-countries-by-ip-address", "/free-geo-ip", "/lookup", "/lookup?q=google"} {
		w := get(s, "GET", target)
		if target == "/" {
			if w.Code != 200 {
				t.Errorf("/ = %d", w.Code)
			}
			continue
		}
		if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" {
			t.Errorf("GET %s = %d Location %q, want 404 with none", target, w.Code, w.Header().Get("Location"))
		}
	}
	if w := get(s, "GET", "/json/"); w.Code == http.StatusMovedPermanently {
		t.Errorf("/json/ redirected")
	}
	if w := get(s, "POST", "/lookup"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /lookup = %d", w.Code)
	}
}

func TestCleanOrgSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Google LLC": "google-llc", "  a  b ": "a-b", "-x--y-": "x-y", "a_b!": "ab", "A-B": "a-b", "!!": "", "a - b": "a-b",
	} {
		if got := cleanOrgSlug(in); got != want {
			t.Errorf("cleanOrgSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
