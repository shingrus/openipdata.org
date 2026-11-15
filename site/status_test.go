package main

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgeText(t *testing.T) {
	now := time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		ago  time.Duration
		want string
	}{
		{0, "just now"}, {59 * time.Second, "just now"}, {-time.Hour, "just now"},
		{time.Minute, "1 minute ago"}, {2*time.Minute + 5*time.Second, "2 minutes ago"}, {59 * time.Minute, "59 minutes ago"},
		{time.Hour, "1 hour ago"}, {6 * time.Hour, "6 hours ago"}, {23*time.Hour + 59*time.Minute, "23 hours ago"},
		{24 * time.Hour, "1 day ago"}, {49 * time.Hour, "2 days ago"},
	} {
		if got := ageText(now.Add(-c.ago), now); got != c.want {
			t.Errorf("%v: %q, want %q", c.ago, got, c.want)
		}
	}
}

// statusServer is asnServer with a fixed clock: started and every file loaded
// six hours before now.
func statusServer(t *testing.T) *server {
	t.Helper()
	s, _ := asnServer(t)
	now := time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.started = now.Add(-6 * time.Hour)
	for _, f := range []*mmdbFile{s.geo, s.asn} {
		f.mu.Lock()
		f.st.LoadedAt = now.Add(-2 * time.Hour)
		f.mu.Unlock()
	}
	s.asnf.mu.Lock()
	s.asnf.cur.st.LoadedAt = now.Add(-time.Hour)
	s.asnf.mu.Unlock()
	return s
}

func TestStatusPage(t *testing.T) {
	s := statusServer(t)
	w := get(s, "GET", "/status?q=1.2.3.4")
	body := w.Body.String()
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") != s.csp {
		t.Fatalf("code %d headers %v", w.Code, w.Header())
	}
	if !strings.Contains(body, `<meta name="robots" content="noindex">`) || !strings.Contains(body, `<input name="q" value=""`) {
		t.Error("noindex or lookup bar")
	}
	for _, want := range []string{
		"<dt>Started</dt><dd>15 Nov 2026 07:00 UTC · 6 hours ago</dd>",
		"<dt>Version</dt><dd>" + version + "</dd>",
		"<h2>lookup-geo.mmdb</h2>", "<h2>lookup-asn.mmdb</h2>", "<h2>asn-openipdata.sqlite3</h2>",
		"<dt>Loaded</dt><dd>15 Nov 2026 11:00 UTC · 2 hours ago</dd>",
		"<dt>Loaded</dt><dd>15 Nov 2026 12:00 UTC · 1 hour ago</dd>",
		"<dt>Networks</dt>", "<dt>Organizations</dt>", "<dt>Prefixes</dt>", "<dt>Load time</dt>", "<dt>Size</dt><dd>",
		"<h2>Accuracy</h2>", "<dt>Right country</dt>", "<h2>DNS beacon</h2>", `<dd class="none">off</dd>`,
		`<a href="/status">Status</a></footer>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "testdata") || strings.Contains(body, "1.2.3.4") {
		t.Error("path or request data on the page")
	}
	t.Logf("page size %d bytes", len(body))

	h := get(s, "HEAD", "/status")
	if h.Code != 200 || h.Body.Len() != 0 || h.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD: %d, %d bytes", h.Code, h.Body.Len())
	}
}

func TestStatusNothingLoaded(t *testing.T) {
	dir := t.TempDir()
	s, err := newServer(filepath.Join(dir, "g.mmdb"), filepath.Join(dir, "a.mmdb"), "")
	if err != nil {
		t.Fatal(err)
	}
	s.useASN(filepath.Join(dir, "x.sqlite3"))
	w := get(s, "GET", "/status")
	body := w.Body.String()
	if w.Code != 200 || strings.Count(body, `<dd class="none">not loaded</dd>`) != 3 {
		t.Fatalf("code %d, body %s", w.Code, body)
	}
	if strings.Contains(body, dir) || !strings.Contains(body, "<h2>g.mmdb</h2>") || !strings.Contains(body, `<dt>Devices</dt><dd class="none">—</dd>`) {
		t.Error("names or dashes")
	}
}

func TestStatusRedirectAndFooter(t *testing.T) {
	s := statusServer(t)
	w := get(s, "GET", "/ss")
	if w.Code != 301 || w.Header().Get("Location") != "/status" {
		t.Errorf("/ss: %d %q", w.Code, w.Header().Get("Location"))
	}
	for _, target := range []string{"/", "/status"} {
		body := get(s, "GET", target).Body.String()
		if !strings.Contains(body, `<a href="/list-of-countries-by-ipv4-allocation">Countries</a><a href="/status">Status</a></footer>`) {
			t.Errorf("%s: Status is not the last footer link", target)
		}
	}
}

func TestStatusBeacon(t *testing.T) {
	s := statusServer(t)
	addr, _ := fakeRedis(t, "$-1\r\n", nil)
	s.useBeacon("r.example.org", addr)
	body := get(s, "GET", "/status").Body.String()
	for _, want := range []string{"<dt>Row</dt><dd>on</dd>", "<dt>Zone</dt><dd>r.example.org</dd>", "<dt>Redis</dt><dd>ok</dd>"} {
		if !strings.Contains(body, want) {
			t.Errorf("ok: missing %q", want)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	s.useBeacon("r.example.org", dead)
	if body := get(s, "GET", "/status").Body.String(); !strings.Contains(body, `<dt>Redis</dt><dd class="none">unreachable</dd>`) {
		t.Error("unreachable not shown")
	}
	s.useBeacon("", dead)
	body = get(s, "GET", "/status").Body.String()
	if strings.Contains(body, "Redis") || strings.Contains(body, "Zone") {
		t.Error("beacon off shows more than one row")
	}
}
