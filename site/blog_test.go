package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"
)

const goodPost = "title: A post\ndate: 2026-11-21\nsummary: One sentence.\n---\n<p>Hello <a href=\"/download\">link</a>.</p>\n"

func postFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for n, c := range files {
		m["blog/"+n] = &fstest.MapFile{Data: []byte(c)}
	}
	return m
}

func TestCommittedPostsAreValid(t *testing.T) {
	posts, err := parseBlog(blogFS, "blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) < 6 {
		t.Fatalf("%d posts", len(posts))
	}
	for i, p := range posts {
		words := len(strings.Fields(string(p.Body)))
		if words < 150 || words > 450 { // markup counts as words here, so the band is wide
			t.Errorf("%s: %d words", p.Slug, words)
		}
		if i > 0 && posts[i-1].Date.Before(p.Date) {
			t.Errorf("%s: not newest first", p.Slug)
		}
		l := strings.ToLower(string(p.Body) + p.Title + p.Summary)
		for _, bad := range []string{"drop-in", "replacement", "hatchet", "r2 ", "vm "} {
			if strings.Contains(l, bad) {
				t.Errorf("%s: %q", p.Slug, bad)
			}
		}
	}
}

func TestParseBlogRejects(t *testing.T) {
	if _, err := parseBlog(postFS(map[string]string{"a-1.html": goodPost, "b.html": strings.Replace(goodPost, "2026-11-21", "2026-11-22\nupdated: 2026-11-23", 1)}), "blog"); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"A.html":         goodPost,
		"x_y.html":       goodPost,
		"x.txt":          goodPost,
		"no-sep.html":    "title: T\ndate: 2026-11-21\nsummary: S\n<p>x</p>",
		"no-title.html":  strings.Replace(goodPost, "title: A post\n", "", 1),
		"no-sum.html":    strings.Replace(goodPost, "summary: One sentence.\n", "", 1),
		"long-sum.html":  strings.Replace(goodPost, "One sentence.", strings.Repeat("x", 155), 1),
		"bad-date.html":  strings.Replace(goodPost, "2026-11-21", "21 Nov 2026", 1),
		"no-date.html":   strings.Replace(goodPost, "date: 2026-11-21\n", "", 1),
		"upd-early.html": strings.Replace(goodPost, "summary", "updated: 2026-11-01\nsummary", 1),
		"key.html":       strings.Replace(goodPost, "title:", "author: me\ntitle:", 1),
		"empty.html":     "title: T\ndate: 2026-11-21\nsummary: S\n---\n  \n",
		"script.html":    goodPost + "<script>x</script>",
		"style.html":     goodPost + "<style>a{}</style>",
		"onclick.html":   goodPost + "<p onclick=\"x()\">a</p>",
		"styleattr.html": goodPost + "<p style=\"color:red\">a</p>",
		"img.html":       goodPost + "<img src=\"x.png\">",
		"iframe.html":    goodPost + "<IFRAME src=\"x\"></iframe>",
		"js.html":        goodPost + "<a href=\"javascript:x()\">a</a>",
	}
	for name, c := range bad {
		if _, err := parseBlog(postFS(map[string]string{name: c}), "blog"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// A post may talk about tags when they are escaped.
	if _, err := parseBlog(postFS(map[string]string{"ok.html": goodPost + "<code>&lt;script&gt;</code>"}), "blog"); err != nil {
		t.Error(err)
	}
}

func TestBlogPages(t *testing.T) {
	s := fixtureServer(t)
	w := get(s, "GET", "/blog")
	b := w.Body.String()
	if w.Code != 200 || w.Header().Get("Cache-Control") != edgeCache || w.Header().Get("ETag") == "" {
		t.Fatalf("list: %d %v", w.Code, w.Header())
	}
	if !strings.Contains(b, `<link rel="canonical" href="https://openipdata.org/blog">`) || strings.Contains(b, "noindex") || !strings.Contains(b, `name="description"`) {
		t.Error("list is not indexable")
	}
	if n := utf8.RuneCountInString(blogTitle); n > 60 {
		t.Errorf("list title %d runes", n)
	}
	for _, p := range blogPosts {
		if !strings.Contains(b, `<a href="`+p.Href+`">`+p.Title+`</a>`) {
			t.Errorf("list lacks %s", p.Slug)
		}
		pw := get(s, "GET", p.Href)
		pb := pw.Body.String()
		if pw.Code != 200 || pw.Header().Get("Cache-Control") != edgeCache {
			t.Errorf("%s: %d", p.Slug, pw.Code)
		}
		for _, x := range []string{`<link rel="canonical" href="https://openipdata.org` + p.Href + `">`, "<h1>", `<a href="/blog">&larr; Blog</a>`, p.DateText, `"@type":"BlogPosting"`, `"@type":"BreadcrumbList"`} {
			if !strings.Contains(pb, x) {
				t.Errorf("%s lacks %q", p.Slug, x)
			}
		}
		if strings.Count(pb, "<h1") != 1 || strings.Contains(pb, "noindex") {
			t.Errorf("%s: h1 or noindex", p.Slug)
		}
		m := regexp.MustCompile(`<title>([^<]*)</title>`).FindStringSubmatch(pb)
		if m == nil || utf8.RuneCountInString(m[1]) > 62 || !strings.Contains(m[1], "openipdata.org") {
			t.Errorf("%s: title %q", p.Slug, m)
		}
		ld := regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`).FindStringSubmatch(pb)
		var v []map[string]any
		if ld == nil || json.Unmarshal([]byte(ld[1]), &v) != nil || len(v) != 2 || v[0]["headline"] != p.Title {
			t.Errorf("%s: JSON-LD", p.Slug)
		}
		// ETag gives 304.
		if r := fromPeer(s, p.Href, "127.0.0.1:1", "If-None-Match", pw.Header().Get("ETag")).Code; r != 304 {
			t.Errorf("%s: If-None-Match gave %d", p.Slug, r)
		}
	}
	if w := get(s, "GET", "/blog/"); w.Code != 301 || w.Header().Get("Location") != "/blog" {
		t.Errorf("/blog/: %d %v", w.Code, w.Header())
	}
	for _, p := range []string{"/blog/nope", "/blog/" + strings.ToUpper(blogPosts[0].Slug), "/blog/" + blogPosts[0].Slug + "/", "/blogx"} {
		w := get(s, "GET", p)
		if w.Code != 404 || !strings.Contains(w.Body.String(), `name="robots" content="noindex"`) && p != "/blogx" {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := get(s, "POST", "/blog"); w.Code != 405 {
		t.Errorf("POST: %d", w.Code)
	}
	if w := get(s, "HEAD", "/blog"); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d", w.Code)
	}
}

func TestBlogLinks(t *testing.T) {
	s := fixtureServer(t)
	if b := get(s, "GET", "/download").Body.String(); !strings.Contains(b, `<a href="/blog">Blog</a>`) {
		t.Error("no footer link")
	}
	l := blogPosts[0]
	home := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9").Body.String()
	if !strings.Contains(home, `<p class="note">Latest: <a href="`+l.Href+`">`+l.Title+`</a>`) {
		t.Error("no Latest line on the visitor page")
	}
	if strings.Contains(get(s, "GET", "/?203.0.113.9").Body.String(), "Latest:") {
		t.Error("Latest line on a lookup page")
	}
	sm := get(s, "GET", "/sitemap.xml").Body.String()
	if !strings.Contains(sm, "<loc>https://openipdata.org"+l.Href+"</loc><lastmod>"+l.lastChange().Format("2006-01-02")+"T00:00:00Z</lastmod>") {
		t.Errorf("sitemap: %s", sm)
	}
	if !strings.Contains(s.llmsText(), "/blog)") {
		t.Error("llms.txt has no blog line")
	}
}

func TestCompareScoreHistory(t *testing.T) {
	plain := get(fixtureServer(t), "GET", "/geolite2-alternative")
	if strings.Contains(plain.Body.String(), "Accuracy over time") || plain.Header().Get("Cache-Control") == edgeCache {
		t.Error("chart block without ASN data, or a long cache")
	}
	w := get(historyServer(t), "GET", "/geolite2-alternative")
	b := w.Body.String()
	i, j := strings.Index(b, "<h2>Accuracy</h2>"), strings.Index(b, "<h2>Accuracy over time</h2>")
	if j < 0 || i < 0 || j < i || j > strings.Index(b, "<h2>Switch from GeoLite2</h2>") || !strings.Contains(b, `<div class="chart">`) {
		t.Error("chart block is missing or misplaced")
	}
	if strings.Count(b, `class="chart"`) != 1 {
		t.Error("chart drawn more than once")
	}
}
