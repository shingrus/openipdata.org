package main

// The blog: posts are HTML files in blog/ embedded in the binary. Each file is
// a header of "key: value" lines, a line "---", then the HTML body (our own
// text, trusted). They are parsed once at start; a bad post stops the start.
// GET /blog lists them, GET /blog/<slug> shows one; both are rendered once per
// origin and kept with an ETag. No visitor data is involved.

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

//go:embed blog/*.html
var blogFS embed.FS

//go:embed static/blog.css
var blogSrc string

//go:embed static/post.css
var postSrc string

var blogCSS = template.CSS(styleCSS + cssText(blogSrc))
var postCSS = template.CSS(styleCSS + cssText(postSrc))

const (
	blogPath  = "/blog"
	blogTitle = "Blog: free IP geolocation database updates | openipdata.org"
	blogDesc  = "Updates to the free openipdata.org IP geolocation database: new sources, accuracy measurements and how to use the files."
	blogTail  = " | openipdata.org"
)

// blogPost is one parsed post.
type blogPost struct {
	Slug, Title, Summary string
	Date, Updated        time.Time // Updated is zero when not set
	Body                 template.HTML
	DateText, UpdText    string // "21 Nov 2026"; UpdText empty without an update
	ISO                  string // the date as 2026-11-21
	Href                 string
}

// lastChange is the day of the newest change of the post.
func (p *blogPost) lastChange() time.Time {
	if !p.Updated.IsZero() {
		return p.Updated
	}
	return p.Date
}

var (
	blogSlugRe = regexp.MustCompile(`^[a-z0-9-]{1,100}$`)
	// What a post body must not hold: the page has no script and no style of
	// its own, and a post may not bring any.
	blogBadRe = regexp.MustCompile(`(?i)<\s*(script|style|img|iframe|object|embed|form|link|meta|svg)\b|\son[a-z]+\s*=|\sstyle\s*=|javascript:`)
)

// parseBlog reads every *.html file of dir in fsys. The posts come back newest
// first (the same date: by slug).
func parseBlog(fsys fs.FS, dir string) ([]blogPost, error) {
	ents, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var posts []blogPost
	seen := map[string]bool{}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".html") {
			return nil, fmt.Errorf("blog: %s: not a post file", name)
		}
		slug := strings.TrimSuffix(name, ".html")
		if !blogSlugRe.MatchString(slug) {
			return nil, fmt.Errorf("blog: %s: bad slug", name)
		}
		if seen[slug] {
			return nil, fmt.Errorf("blog: %s: duplicate slug", name)
		}
		seen[slug] = true
		raw, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return nil, err
		}
		p, err := parsePost(slug, string(raw))
		if err != nil {
			return nil, fmt.Errorf("blog: %s: %w", name, err)
		}
		posts = append(posts, p)
	}
	sort.Slice(posts, func(i, j int) bool {
		if !posts[i].Date.Equal(posts[j].Date) {
			return posts[i].Date.After(posts[j].Date)
		}
		return posts[i].Slug < posts[j].Slug
	})
	return posts, nil
}

func parsePost(slug, raw string) (blogPost, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	head, body, ok := strings.Cut(raw, "\n---\n")
	if !ok {
		return blogPost{}, fmt.Errorf("no --- line after the header")
	}
	p := blogPost{Slug: slug, Href: blogPath + "/" + slug}
	for _, l := range strings.Split(head, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			return p, fmt.Errorf("bad header line")
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "title":
			p.Title = v
		case "summary":
			p.Summary = v
		case "date", "updated":
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				return p, fmt.Errorf("bad %s", k)
			}
			if k == "date" {
				p.Date = t
			} else {
				p.Updated = t
			}
		default:
			return p, fmt.Errorf("unknown header key %q", k)
		}
	}
	switch {
	case p.Title == "" || !utf8.ValidString(p.Title):
		return p, fmt.Errorf("no title")
	case p.Summary == "" || utf8.RuneCountInString(p.Summary) >= 155:
		return p, fmt.Errorf("summary missing or 155 characters or more")
	case p.Date.IsZero():
		return p, fmt.Errorf("no date")
	case !p.Updated.IsZero() && p.Updated.Before(p.Date):
		return p, fmt.Errorf("updated is before date")
	case strings.TrimSpace(body) == "":
		return p, fmt.Errorf("empty body")
	}
	if m := blogBadRe.FindString(body); m != "" {
		return p, fmt.Errorf("the body holds %q", strings.TrimSpace(m))
	}
	p.Body = template.HTML(body)
	p.DateText, p.ISO = p.Date.Format("2 Jan 2006"), p.Date.Format("2006-01-02")
	if !p.Updated.IsZero() {
		p.UpdText = p.Updated.Format("2 Jan 2006")
	}
	return p, nil
}

func mustBlog() []blogPost {
	posts, err := parseBlog(blogFS, "blog")
	if err != nil {
		panic(err)
	}
	return posts
}

// blogPosts are the posts of this binary, newest first; a bad one stops the start.
var blogPosts = mustBlog()

// blogLatest is the newest post, or nil.
func blogLatest() *blogPost {
	if len(blogPosts) == 0 {
		return nil
	}
	return &blogPosts[0]
}

// blogNewest is the newest change of any post, for the sitemap.
func blogNewest() time.Time {
	var t time.Time
	for i := range blogPosts {
		if c := blogPosts[i].lastChange(); c.After(t) {
			t = c
		}
	}
	return t
}

type blogView struct {
	pageBase
	Posts []blogPost // the list page
	Post  *blogPost  // a post page
}

type ldPublisher struct {
	Type string `json:"@type"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type ldBlogPosting struct {
	Context       string      `json:"@context"`
	Type          string      `json:"@type"`
	Headline      string      `json:"headline"`
	Description   string      `json:"description"`
	DatePublished string      `json:"datePublished"`
	DateModified  string      `json:"dateModified"`
	URL           string      `json:"url"`
	Main          string      `json:"mainEntityOfPage"`
	Author        ldPublisher `json:"author"`
	Publisher     ldPublisher `json:"publisher"`
}

// blogSet holds the rendered pages of one origin.
type blogSet struct {
	origin string
	list   *cachedPage
	posts  map[string]*cachedPage
}

type blogCache struct {
	mu  sync.Mutex
	set *blogSet
}

func (s *server) blogBase(css template.CSS, title, desc, canon string) pageBase {
	return pageBase{HeadJS: template.JS(headJS), ThemeJS: template.JS(themeJS), CSS: css,
		Title: title, Desc: desc, Canon: canon}
}

// blogPages renders every blog page for the current origin, once.
func (s *server) blogPages() (*blogSet, error) {
	c := &s.blog
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set != nil && c.set.origin == s.origin {
		return c.set, nil
	}
	set := &blogSet{origin: s.origin, posts: map[string]*cachedPage{}}
	body, err := render(s.tmpl, "blog.html", &blogView{
		pageBase: s.blogBase(blogCSS, blogTitle, blogDesc, s.canon(blogPath)), Posts: blogPosts})
	if err != nil {
		return nil, err
	}
	set.list = newPage(body)
	org := ldPublisher{"Organization", "openipdata.org", s.canon("/")}
	for i := range blogPosts {
		p := &blogPosts[i]
		url := s.canon(p.Href)
		ld, err := json.Marshal([]any{
			ldBlogPosting{"https://schema.org", "BlogPosting", p.Title, p.Summary, p.ISO, p.lastChange().Format("2006-01-02"),
				url, url, org, org},
			ldCrumbs{Context: "https://schema.org", Type: "BreadcrumbList", Items: []ldCrumb{
				{"ListItem", 1, "Blog", s.canon(blogPath)}, {"ListItem", 2, p.Title, url}}},
		})
		if err != nil {
			return nil, err
		}
		b := s.blogBase(postCSS, fitName("", p.Title, blogTail), p.Summary, url)
		b.LD = template.JS(ld)
		body, err := render(s.tmpl, "blog.html", &blogView{pageBase: b, Post: p})
		if err != nil {
			return nil, err
		}
		set.posts[p.Slug] = newPage(body)
	}
	c.set = set
	return set, nil
}

// serveBlog answers /blog and /blog/<slug>.
func (s *server) serveBlog(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, blogPath)
	if slug == "/" {
		w.Header().Set("Cache-Control", asnCache)
		to := blogPath
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", to)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}
	if len(blogPosts) == 0 || (slug != "" && !strings.HasPrefix(slug, "/")) {
		s.blogNotFound(w, r)
		return
	}
	set, err := s.blogPages()
	if err != nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	pg := set.list
	if slug != "" {
		if pg = set.posts[slug[1:]]; pg == nil {
			s.blogNotFound(w, r)
			return
		}
	}
	pageHeaders(w.Header(), s.csp)
	servePage(w, r, pg, "text/html; charset=utf-8", edgeCache)
}

func (s *server) blogNotFound(w http.ResponseWriter, r *http.Request) {
	s.asnNotice(w, r, http.StatusNotFound, "Not found", "Post not found", asnCache)
}
