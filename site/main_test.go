package main

import (
	"crypto/sha256"
	"encoding/base64"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, mmdb string) *server {
	t.Helper()
	s, err := newServer(mmdb)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(s http.Handler, method, target string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadScoresFixture(t *testing.T) {
	s, err := readScores("testdata/stamped.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	if s.Country != 91.5 || s.City != 42.5 || s.Coverage != 77.5 || s.Probes != 1234 {
		t.Errorf("scores: %+v", s)
	}
	if s.Built.Format(time.RFC3339) != "2026-11-14T07:42:10Z" {
		t.Errorf("built: %v", s.Built)
	}

	// An MMDB with no stamp: numbers missing, time from the build epoch.
	s, err = readScores("testdata/unstamped.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(s.Country) || s.Probes != -1 || s.Built.IsZero() {
		t.Errorf("unstamped: %+v", s)
	}

	if _, err := readScores("testdata/missing.mmdb"); err == nil {
		t.Error("missing file: want an error")
	}
	bad := filepath.Join(t.TempDir(), "bad.mmdb")
	os.WriteFile(bad, []byte("not an mmdb"), 0o644)
	if _, err := readScores(bad); err == nil {
		t.Error("garbage file: want an error")
	}
}

func TestHome(t *testing.T) {
	s := testServer(t, "testdata/stamped.mmdb")
	w := get(s, "GET", "/")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<h1>Accuracy</h1>", "right country", "right city", "of addresses have a city",
		`<span class="num">91.5<i>%</i></span>`, `<span class="num">42.5<i>%</i></span>`, `<span class="num">77.5<i>%</i></span>`,
		`style="--v:91.5%"`, `<span class="mono">1,234</span>`,
		`<time datetime="2026-11-14T07:42:10Z">14 Nov 2026</time>`,
		`href="https://github.com/shingrus/openipdata.org"`, `id="theme"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	h := w.Header()
	if h.Get("Content-Type") != "text/html; charset=utf-8" || h.Get("Cache-Control") != "public, max-age=60" ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers: %v", h)
	}

	// CSP: two script hashes, equal to the hashes of the scripts in the body.
	csp := h.Get("Content-Security-Policy")
	hashes := regexp.MustCompile(`'sha256-([^']+)'`).FindAllStringSubmatch(csp, -1)
	scripts := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(body, -1)
	if len(hashes) != 2 || len(scripts) != 2 {
		t.Fatalf("hashes %d, scripts %d", len(hashes), len(scripts))
	}
	for i, sc := range scripts {
		sum := sha256.Sum256([]byte(sc[1]))
		if got := base64.StdEncoding.EncodeToString(sum[:]); got != hashes[i][1] {
			t.Errorf("script %d: hash %s, CSP has %s", i, got, hashes[i][1])
		}
	}
	for _, want := range []string{"default-src 'none'", "font-src 'self'", "img-src data:", "base-uri 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}

	// ETag and 304
	etag := h.Get("ETag")
	if !strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, "W/") {
		t.Fatalf("etag %q", etag)
	}
	w2 := get(s, "GET", "/", "If-None-Match", etag)
	if w2.Code != 304 || w2.Body.Len() != 0 || w2.Header().Get("ETag") != etag {
		t.Errorf("conditional: %d, %d bytes", w2.Code, w2.Body.Len())
	}
	if w3 := get(s, "GET", "/", "If-None-Match", `"other"`); w3.Code != 200 {
		t.Errorf("other etag: %d", w3.Code)
	}
	if w4 := get(s, "HEAD", "/"); w4.Code != 200 {
		t.Errorf("HEAD: %d", w4.Code)
	}
}

func TestRoutes(t *testing.T) {
	s := testServer(t, "testdata/stamped.mmdb")
	if w := get(s, "GET", "/?1.1.1.1"); w.Code != 200 || !strings.Contains(w.Body.String(), "Accuracy") {
		t.Errorf("query: %d", w.Code)
	}
	for _, p := range []string{"/x", "/index.html", "/fonts/", "/fonts/nope.woff2", "/healthz/"} {
		if w := get(s, "GET", p); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if w := get(s, m, "/"); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s /: %d", m, w.Code)
		}
	}
	w := get(s, "GET", "/healthz")
	if w.Code != 200 || w.Body.String() != "ok "+version+"\n" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("healthz: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	w = get(s, "GET", "/favicon.ico")
	if w.Code != 204 || !strings.Contains(w.Header().Get("Cache-Control"), "max-age=31536000") {
		t.Errorf("favicon: %d %v", w.Code, w.Header())
	}
	for _, f := range []string{"geist-latin.woff2", "geist-mono-latin.woff2"} {
		w = get(s, "GET", "/fonts/"+f)
		if w.Code != 200 || w.Header().Get("Content-Type") != "font/woff2" ||
			w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || w.Body.Len() < 1000 {
			t.Errorf("font %s: %d %v", f, w.Code, w.Header())
		}
		if !strings.Contains(get(s, "GET", "/").Body.String(), "/fonts/"+f) {
			t.Errorf("page does not reference %s", f)
		}
	}
}

func TestNoMMDB(t *testing.T) {
	for name, path := range map[string]string{
		"missing":  filepath.Join(t.TempDir(), "none.mmdb"),
		"no stamp": "testdata/unstamped.mmdb",
		"unreadable": func() string {
			p := filepath.Join(t.TempDir(), "x.mmdb")
			os.WriteFile(p, []byte("junk"), 0o644)
			return p
		}(),
	} {
		body := get(testServer(t, path), "GET", "/").Body.String()
		if n := strings.Count(body, `<span class="num">—</span>`); n != 3 {
			t.Errorf("%s: %d dashes", name, n)
		}
		if strings.Contains(body, "%</i>") || strings.Contains(body, `<span class="mono">`) {
			t.Errorf("%s: shows a number", name)
		}
		if !strings.Contains(body, "tested on devices whose real location is known.") {
			t.Errorf("%s: sentence", name)
		}
		// Only an MMDB that opened can give "updated" (from its build epoch).
		if got := strings.Contains(body, "updated"); got != (name == "no stamp") {
			t.Errorf("%s: updated = %v", name, got)
		}
	}
}

func TestReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ip2geo.mmdb")
	s := testServer(t, path) // file does not exist yet
	if !strings.Contains(get(s, "GET", "/").Body.String(), "—") {
		t.Fatal("want dashes first")
	}
	old := s.home.Load()

	s.check() // still missing: keeps the page
	if s.home.Load() != old {
		t.Error("page changed for a missing file")
	}

	copyFile(t, "testdata/stamped.mmdb", path+".tmp")
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
	s.check()
	if !strings.Contains(get(s, "GET", "/").Body.String(), "91.5") {
		t.Error("new file not picked up")
	}
	cur := s.home.Load()
	s.check() // unchanged: nothing re-rendered
	if s.home.Load() != cur {
		t.Error("re-rendered without a change")
	}

	// A broken replacement keeps the page.
	os.WriteFile(path, []byte("broken broken broken"), 0o644)
	s.check()
	if s.home.Load() != cur || !strings.Contains(get(s, "GET", "/").Body.String(), "91.5") {
		t.Error("failed reload did not keep the page")
	}

	// A good file again, even with the same size, is read after its time changes.
	copyFile(t, "testdata/stamped.mmdb", path)
	os.Chtimes(path, time.Now(), time.Now().Add(time.Hour))
	s.check()
	if s.home.Load() == cur {
		t.Error("fixed file not picked up")
	}
}

func TestPageHygiene(t *testing.T) {
	s := testServer(t, "testdata/stamped.mmdb")
	body := get(s, "GET", "/").Body.String()
	if len(body) >= 8<<10 {
		t.Errorf("page is %d bytes", len(body))
	}
	// The only URL is the GitHub link (the SVG namespace of the data: favicon is no request).
	clean := strings.ReplaceAll(body, "http://www.w3.org/2000/svg", "")
	for _, m := range regexp.MustCompile(`https?://[^\s"'<>]+`).FindAllString(clean, -1) {
		if m != "https://github.com/shingrus/openipdata.org" {
			t.Errorf("unexpected URL %s", m)
		}
	}
	if strings.Contains(body, "<link rel=\"stylesheet\"") || strings.Contains(body, "<img") {
		t.Error("external stylesheet or image")
	}
}

func TestStartUnreadableThenFixed(t *testing.T) {
	good, err := os.ReadFile("testdata/stamped.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	for name, sameSize := range map[string]bool{"other size": false, "same size and time": true} {
		path := filepath.Join(t.TempDir(), "ip2geo.mmdb")
		junk := []byte("garbage")
		if sameSize {
			junk = make([]byte, len(good))
			copy(junk, "garbage")
		}
		if err := os.WriteFile(path, junk, 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		s := testServer(t, path)
		if n := strings.Count(get(s, "GET", "/").Body.String(), `<span class="num">—</span>`); n != 3 {
			t.Fatalf("%s: %d dashes", name, n)
		}
		if err := os.WriteFile(path, good, 0o644); err != nil {
			t.Fatal(err)
		}
		mt := info.ModTime()
		if !sameSize {
			mt = mt.Add(time.Minute)
		}
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatal(err)
		}
		if after, _ := os.Stat(path); sameSize && (after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime())) {
			t.Fatalf("%s: size or time differs", name)
		}
		s.check()
		if !strings.Contains(get(s, "GET", "/").Body.String(), "91.5") {
			t.Errorf("%s: fixed file not picked up", name)
		}
	}
}
