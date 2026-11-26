package main

import "testing"

func TestSecurityHeadersOnEveryAnswer(t *testing.T) {
	s := testServer(t, "testdata/stamped.mmdb")
	for _, c := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/download"}, {"GET", "/json"}, {"GET", "/nope"},
		{"PUT", "/"}, {"GET", "/robots.txt"}, {"GET", "/ip"}, {"POST", "/share"},
	} {
		h := get(s, c.method, c.path).Result().Header
		if h.Get("Strict-Transport-Security") != hstsValue || h.Get("Permissions-Policy") != permissionsVal {
			t.Errorf("%s %s: missing security headers", c.method, c.path)
		}
	}
}
