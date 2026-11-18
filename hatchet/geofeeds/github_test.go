package geofeeds

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const ghFeed = "# prefix,country,region,city\n45.10.112.0/24,DE,DE-BE,Berlin\n45.10.113.0/24,DE,,Hamburg\n"

// fakeGitHub is a GitHub API and a raw file host. The two servers record each request and the
// Authorization header it carried.
type fakeGitHub struct {
	api, raw *httptest.Server

	mu       sync.Mutex
	repos    []map[string]any             // search results, in order
	trees    map[string][]githubTreeEntry // by owner/repo
	files    map[string]string            // by owner/repo/branch/path (unescaped)
	searchFn func(page int) (int, string)
	treeFn   func(name string) (int, string)
	hdr      http.Header           // headers of the answers searchFn and treeFn give
	rawFn    func(path string) int // a status for a raw file (0: serve it)
	// rejectAuth: the API answers 401 to a request that carries an Authorization header.
	rejectAuth bool
	apiHits    []string
	rawHits    []string
	apiAuth    []string
	rawAuth    []string
}

func repoItem(name string, pushed string) map[string]any {
	return map[string]any{"full_name": name, "fork": false, "private": false, "disabled": false, "size": 10,
		"default_branch": "main", "pushed_at": pushed}
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{trees: map[string][]githubTreeEntry{}, files: map[string]string{}}
	g.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.apiHits = append(g.apiHits, r.URL.RequestURI())
		g.apiAuth = append(g.apiAuth, r.Header.Get("Authorization"))
		reject := g.rejectAuth && r.Header.Get("Authorization") != ""
		g.mu.Unlock()
		if reject {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/search/repositories":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if g.searchFn != nil {
				status, body := g.searchFn(page)
				for k, v := range g.hdr {
					w.Header()[k] = v
				}
				w.WriteHeader(status)
				fmt.Fprint(w, body)
				return
			}
			g.mu.Lock()
			lo, hi := (page-1)*100, min(page*100, len(g.repos))
			var items []map[string]any
			if lo < len(g.repos) {
				items = g.repos[lo:hi]
			}
			g.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"items": items})
		case strings.Contains(r.URL.Path, "/git/trees/"):
			name := strings.TrimPrefix(r.URL.Path, "/repos/")
			name = name[:strings.Index(name, "/git/trees/")]
			if g.treeFn != nil {
				if status, body := g.treeFn(name); status != 0 {
					for k, v := range g.hdr {
						w.Header()[k] = v
					}
					w.WriteHeader(status)
					fmt.Fprint(w, body)
					return
				}
			}
			g.mu.Lock()
			tree, ok := g.trees[name]
			g.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"tree": tree})
		default:
			http.NotFound(w, r)
		}
	}))
	g.raw = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.rawHits = append(g.rawHits, r.URL.EscapedPath())
		g.rawAuth = append(g.rawAuth, r.Header.Get("Authorization"))
		body, ok := g.files[strings.TrimPrefix(r.URL.Path, "/")]
		g.mu.Unlock()
		if g.rawFn != nil {
			if status := g.rawFn(r.URL.Path); status != 0 {
				w.WriteHeader(status)
				return
			}
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(g.api.Close)
	t.Cleanup(g.raw.Close)
	return g
}

func (g *fakeGitHub) hits() (api, raw []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.apiHits), slices.Clone(g.rawHits)
}

func (g *fakeGitHub) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.apiHits, g.rawHits, g.apiAuth, g.rawAuth = nil, nil, nil, nil
}

func (g *fakeGitHub) pipeline(t *testing.T, dir string, logs *[]string) *Pipeline {
	p := &Pipeline{DataDir: dir, allowPrivate: true, ghAPI: g.api.URL, ghRaw: g.raw.URL, ghNoWait: true,
		now: func() time.Time { return time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC) }}
	if logs != nil {
		p.Progress = func(s string) { *logs = append(*logs, s) }
	}
	return p
}

func blob(path string, size int64) githubTreeEntry {
	return githubTreeEntry{Path: path, Type: "blob", Size: size}
}

func readGitHubState(t *testing.T, p *Pipeline) map[string]*githubRepo {
	t.Helper()
	state, damaged, err := loadGitHub(filepath.Join(p.dir(), GitHubFile))
	if err != nil || damaged {
		t.Fatalf("state: %v %v", err, damaged)
	}
	return state
}

func TestCheckRepo(t *testing.T) {
	ok := repoItem("alice/geofeed", "2026-11-01T00:00:00Z")
	with := func(change func(m map[string]any)) githubItem {
		m := map[string]any{}
		for k, v := range ok {
			m[k] = v
		}
		change(m)
		data, _ := json.Marshal(m)
		var it githubItem
		json.Unmarshal(data, &it)
		return it
	}
	tests := []struct {
		what string
		item githubItem
		want int
	}{
		{"plain", with(func(m map[string]any) {}), repoOK},
		{"upper case name", with(func(m map[string]any) { m["full_name"] = "Alice/My-GeoFeed.v2" }), repoOK},
		{"dot owner", with(func(m map[string]any) { m["full_name"] = "./geofeed" }), repoSkipOther},
		{"dotdot owner", with(func(m map[string]any) { m["full_name"] = "../geofeed" }), repoSkipOther},
		{"bad full name", with(func(m map[string]any) { m["full_name"] = "alice/geo feed" }), repoSkipOther},
		{"no owner", with(func(m map[string]any) { m["full_name"] = "geofeed" }), repoSkipOther},
		{"too long", with(func(m map[string]any) { m["full_name"] = "alice/geofeed" + strings.Repeat("x", 100) }), repoSkipOther},
		{"fork", with(func(m map[string]any) { m["fork"] = true }), repoSkipOther},
		{"private", with(func(m map[string]any) { m["private"] = true }), repoSkipOther},
		{"disabled", with(func(m map[string]any) { m["disabled"] = true }), repoSkipOther},
		{"empty", with(func(m map[string]any) { m["size"] = 0 }), repoSkipOther},
		{"geofeed only in the owner", with(func(m map[string]any) { m["full_name"] = "geofeed/tools" }), repoSkipOther},
		{"bad branch dots", with(func(m map[string]any) { m["default_branch"] = "a..b" }), repoSkipOther},
		{"bad branch slash", with(func(m map[string]any) { m["default_branch"] = "/main" }), repoSkipOther},
		{"bad branch trailing slash", with(func(m map[string]any) { m["default_branch"] = "main/" }), repoSkipOther},
		{"bad branch space", with(func(m map[string]any) { m["default_branch"] = "my branch" }), repoSkipOther},
		{"empty branch", with(func(m map[string]any) { m["default_branch"] = "" }), repoSkipOther},
		{"branch with slash", with(func(m map[string]any) { m["default_branch"] = "release/1.0" }), repoOK},
		{"no pushed_at", with(func(m map[string]any) { m["pushed_at"] = "" }), repoSkipOther},
		{"test name", with(func(m map[string]any) { m["full_name"] = "alice/geofeed-test" }), repoSkipName},
		{"example name", with(func(m map[string]any) { m["full_name"] = "alice/example_geofeed" }), repoSkipName},
		{"test is part of a word", with(func(m map[string]any) { m["full_name"] = "alice/geofeed-latest" }), repoOK},
		{"blacklist name", with(func(m map[string]any) { m["full_name"] = "alice/geofeed-blocklist" }), repoSkipName},
		{"abuse in the owner", with(func(m map[string]any) { m["full_name"] = "abuse-team/geofeed" }), repoSkipName},
	}
	for _, tt := range tests {
		if got := checkRepo(tt.item); got != tt.want {
			t.Errorf("%s: %d, want %d", tt.what, got, tt.want)
		}
	}
}

func TestCandidateFiles(t *testing.T) {
	tree := []githubTreeEntry{
		blob("README.md", 500),
		blob("readme.txt", 500),
		blob("RFC8805.txt", 500),
		blob("LICENSE", 500),
		blob("licence.txt", 500),
		blob(".hidden.csv", 500),
		blob("data/other.csv", 500),
		blob("geofeed.csv", 500),
		blob("feeds/a.txt", 500),
		blob("notes", 500),
		blob("script.py", 500),
		blob("image.png", 500),
		blob("tiny.csv", 19),
		blob("huge.csv", githubMaxFile+1),
		blob("node_modules/x/geofeed.csv", 500),
		blob("Test/geofeed.csv", 500),
		blob("a/vendor/geofeed.csv", 500),
		blob("examples/geofeed.csv", 500),
		blob("testdata/geofeed.csv", 500),
		blob("a/../geofeed.csv", 500),
		blob("a//geofeed.csv", 500),
		blob("bad\x01name.csv", 500),
		blob("\xff.csv", 500),
		blob(strings.Repeat("x", 300)+".csv", 500),
		{Path: "dir", Type: "tree", Size: 500},
		{Path: "link.csv", Type: "commit", Size: 500},
		blob("Upper.CSV", 500),
	}
	got := candidateFiles(tree)
	// Paths with geo or feed first, then fewer slashes, then by name.
	want := []string{"geofeed.csv", "feeds/a.txt", "Upper.CSV", "notes", "data/other.csv"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("candidates = %q, want %q", got, want)
	}
	// At most 8, the best first.
	var many []githubTreeEntry
	for i := 0; i < 20; i++ {
		many = append(many, blob(fmt.Sprintf("d/f%02d.csv", i), 500))
	}
	many = append(many, blob("z/deep/geofeed.csv", 500), blob("z.csv", 500))
	got = candidateFiles(many)
	if len(got) != githubFilesPerRepo || got[0] != "z/deep/geofeed.csv" || got[1] != "z.csv" || got[2] != "d/f00.csv" {
		t.Errorf("capped = %q", got)
	}
}

func TestIsFeed(t *testing.T) {
	tests := []struct {
		what, body string
		want       bool
	}{
		{"feed", ghFeed, true},
		{"one row", "45.10.112.0/24,DE\n", true},
		{"header line among rows", "prefix,country,region,city\n45.10.112.0/24,DE,,\n", true},
		{"readme", "# Title\n\nThis repository holds our geofeed.\nSee RFC 8805 for the format.\n", false},
		{"other csv", "name,age\nbob,3\nanna,4\n", false},
		{"mostly junk", "45.10.112.0/24,DE\nx\ny\nz\n", false},
		{"html", "<!doctype html><html></html>", false},
		{"empty", "", false},
		{"comments only", "# a\n# b\n", false},
	}
	p := &Pipeline{DataDir: t.TempDir()}
	checkDir := filepath.Join(p.dir(), githubCheckDir)
	os.MkdirAll(checkDir, 0o750)
	for _, tt := range tests {
		got, err := p.isFeed([]byte(tt.body))
		if err != nil || got != tt.want {
			t.Errorf("%s: %v %v", tt.what, got, err)
		}
	}
	// No temporary file is left in the geofeeds dir.
	if entries, _ := os.ReadDir(checkDir); len(entries) != 0 {
		t.Errorf("left in %s: %v", checkDir, entries)
	}
	// A file error is returned.
	bad := &Pipeline{DataDir: filepath.Join(t.TempDir(), "missing")}
	if _, err := bad.isFeed([]byte(ghFeed)); err == nil {
		t.Error("a missing geofeeds dir must be an error")
	}
}

func TestGitHubFileURL(t *testing.T) {
	got := githubFileURL("https://raw.githubusercontent.com", "alice/geofeed", "release/1.0", "my dir/geo feed#1.csv")
	want := "https://raw.githubusercontent.com/alice/geofeed/release/1.0/my%20dir/geo%20feed%231.csv"
	if got != want {
		t.Errorf("url = %s", got)
	}
	if _, err := url.Parse(got); err != nil {
		t.Error(err)
	}
}

func TestGitHubPaginationAndFiles(t *testing.T) {
	g := newFakeGitHub(t)
	for i := 0; i < 150; i++ {
		g.repos = append(g.repos, repoItem(fmt.Sprintf("u%d/geofeed-%d", i, i), "2026-11-01T00:00:00Z"))
	}
	// Some that are filtered.
	g.repos[0]["fork"] = true
	g.repos[1]["full_name"] = "bad name/geofeed"
	g.repos[2]["full_name"] = "u2/geofeed-test"
	g.repos[3]["full_name"] = "u3/geofeed-blocklist"
	g.repos[4]["size"] = 0
	// One repository with real files.
	g.repos[5]["full_name"] = "alice/geofeed"
	g.trees["alice/geofeed"] = []githubTreeEntry{
		blob("README.md", 900), blob("geofeed.csv", 900), blob("other.csv", 900), blob("dir with space/geo feed.csv", 900), blob("notes.txt", 900),
	}
	g.files["alice/geofeed/main/geofeed.csv"] = ghFeed
	g.files["alice/geofeed/main/other.csv"] = "name,age\nbob,3\nanna,4\n"
	g.files["alice/geofeed/main/dir with space/geo feed.csv"] = ghFeed
	g.files["alice/geofeed/main/notes.txt"] = "# Notes\nsome text about feeds\n"
	var logs []string
	p := g.pipeline(t, t.TempDir(), &logs)
	p.ghBudget = 1000
	out, err := p.SearchGitHub(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	api, raw := g.hits()
	searches := 0
	for _, h := range api {
		if strings.HasPrefix(h, "/search/repositories?q=geofeed+in:name&sort=updated&order=desc&per_page=100&page=") {
			searches++
		}
	}
	if searches != 2 {
		t.Errorf("search requests = %d: a short second page ends the search", searches)
	}
	if out.Repos != 145 || out.SkippedNames != 2 || out.SkippedOther != 3 || out.Listed != 145 || out.Feeds != 2 || out.NewFeeds != 2 ||
		out.FilesChecked != 4 || out.Errors != 0 || out.Stopped != "" || out.Skipped != "" || out.Token {
		t.Errorf("out = %+v", out)
	}
	state := readGitHubState(t, p)
	if got := state["alice/geofeed"]; got == nil || !reflect.DeepEqual(got.Files, []string{"geofeed.csv", "dir with space/geo feed.csv"}) || got.Branch != "main" {
		t.Errorf("state = %+v", got)
	}
	// The paths of the raw requests are escaped, and the file host got no token.
	if !slices.Contains(raw, "/alice/geofeed/main/dir%20with%20space/geo%20feed.csv") {
		t.Errorf("raw requests = %q", raw)
	}
	urls, err := githubFeedURLs(filepath.Join(p.dir(), GitHubFile))
	wantURLs := []string{
		"https://raw.githubusercontent.com/alice/geofeed/main/dir%20with%20space/geo%20feed.csv",
		"https://raw.githubusercontent.com/alice/geofeed/main/geofeed.csv",
	}
	if err != nil || !reflect.DeepEqual(urls, wantURLs) {
		t.Errorf("urls = %q %v", urls, err)
	}
	// Logs hold counts only: no repository name.
	for _, l := range logs {
		if strings.Contains(l, "alice") || strings.Contains(l, "geofeed-") {
			t.Errorf("log names a repository: %q", l)
		}
	}
}

func TestGitHubMaxPages(t *testing.T) {
	g := newFakeGitHub(t)
	g.searchFn = func(page int) (int, string) {
		var items []map[string]any
		for i := 0; i < 100; i++ {
			items = append(items, repoItem(fmt.Sprintf("u%d/geofeed-%d-%d", page, page, i), "2026-11-01T00:00:00Z"))
		}
		data, _ := json.Marshal(map[string]any{"items": items})
		return 200, string(data)
	}
	p := g.pipeline(t, t.TempDir(), nil)
	p.ghBudget = 1
	out, err := p.SearchGitHub(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	api, _ := g.hits()
	searches := 0
	for _, h := range api {
		if strings.HasPrefix(h, "/search/") {
			searches++
		}
	}
	if searches != githubSearchPages || out.Repos != 1000 || out.Listed != 1 || out.Stopped != "budget" {
		t.Errorf("searches %d, out = %+v", searches, out)
	}
}

func TestGitHubStateAcrossRuns(t *testing.T) {
	g := newFakeGitHub(t)
	g.repos = []map[string]any{repoItem("alice/geofeed", "2026-11-01T00:00:00Z"), repoItem("bob/geofeed", "2026-10-01T00:00:00Z")}
	for _, name := range []string{"alice/geofeed", "bob/geofeed"} {
		g.trees[name] = []githubTreeEntry{blob("geofeed.csv", 900)}
		g.files[name+"/main/geofeed.csv"] = ghFeed
	}
	p := g.pipeline(t, t.TempDir(), nil)
	out, err := p.SearchGitHub(context.Background())
	if err != nil || out.Listed != 2 || out.Feeds != 2 || out.NewFeeds != 2 {
		t.Fatalf("first run: %+v %v", out, err)
	}
	// Unchanged: no repository is listed again.
	g.reset()
	out, err = p.SearchGitHub(context.Background())
	api, raw := g.hits()
	if err != nil || out.Listed != 0 || out.Feeds != 2 || out.NewFeeds != 0 || len(raw) != 0 {
		t.Errorf("second run: %+v %v api %v raw %v", out, err, api, raw)
	}
	for _, h := range api {
		if strings.Contains(h, "/git/trees/") {
			t.Errorf("tree requested again: %s", h)
		}
	}
	// A new push: only that repository is listed, and a file that appeared is a new feed.
	g.repos[0]["pushed_at"] = "2026-11-10T00:00:00Z"
	g.trees["alice/geofeed"] = []githubTreeEntry{blob("geofeed.csv", 900), blob("v6.csv", 900)}
	g.files["alice/geofeed/main/v6.csv"] = "2a00:1450::/32,DE\n"
	g.reset()
	out, err = p.SearchGitHub(context.Background())
	if err != nil || out.Listed != 1 || out.Feeds != 3 || out.NewFeeds != 1 {
		t.Errorf("changed push: %+v %v", out, err)
	}
	// A changed default branch is listed again too.
	g.repos[1]["default_branch"] = "trunk"
	g.trees["bob/geofeed"] = []githubTreeEntry{blob("geofeed.csv", 900)}
	g.files["bob/geofeed/trunk/geofeed.csv"] = ghFeed
	out, err = p.SearchGitHub(context.Background())
	if err != nil || out.Listed != 1 || readGitHubState(t, p)["bob/geofeed"].Branch != "trunk" {
		t.Errorf("changed branch: %+v %v", out, err)
	}
}

func TestGitHubBudget(t *testing.T) {
	g := newFakeGitHub(t)
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("u%d/geofeed", i)
		g.repos = append(g.repos, repoItem(name, "2026-11-01T00:00:00Z"))
		g.trees[name] = []githubTreeEntry{blob("geofeed.csv", 900)}
		g.files[name+"/main/geofeed.csv"] = ghFeed
	}
	p := g.pipeline(t, t.TempDir(), nil)
	p.ghBudget = 2
	out, err := p.SearchGitHub(context.Background())
	if err != nil || out.Listed != 2 || out.Stopped != "budget" || out.Feeds != 2 {
		t.Fatalf("first: %+v %v", out, err)
	}
	// The rest waits for the next runs, in search order.
	out, _ = p.SearchGitHub(context.Background())
	out2, _ := p.SearchGitHub(context.Background())
	if out.Listed != 2 || out.Feeds != 4 || out2.Listed != 1 || out2.Stopped != "" || out2.Feeds != 5 {
		t.Errorf("next runs: %+v %+v", out, out2)
	}
	// The default budgets.
	if (&Pipeline{}).githubBudget(false) != 40 || (&Pipeline{GitHubToken: "x"}).githubBudget(true) != 1000 {
		t.Error("default budgets")
	}
	if (&Pipeline{}).githubSearchWait(false) != 7*time.Second || (&Pipeline{GitHubToken: "x"}).githubSearchWait(true) != 2*time.Second {
		t.Error("default waits")
	}
}

func TestGitHubRateLimitKeepsProgress(t *testing.T) {
	for _, c := range []struct {
		status int
		hdr    http.Header
	}{
		{429, nil},
		{403, http.Header{"Retry-After": {"60"}}},
		{403, http.Header{"X-Ratelimit-Remaining": {"0"}}},
	} {
		status := c.status
		g := newFakeGitHub(t)
		g.hdr = c.hdr
		for i := 0; i < 3; i++ {
			name := fmt.Sprintf("u%d/geofeed", i)
			g.repos = append(g.repos, repoItem(name, "2026-11-01T00:00:00Z"))
			g.trees[name] = []githubTreeEntry{blob("geofeed.csv", 900)}
			g.files[name+"/main/geofeed.csv"] = ghFeed
		}
		g.treeFn = func(name string) (int, string) {
			if name == "u1/geofeed" {
				return status, `{"message":"rate limit"}`
			}
			return 0, ""
		}
		p := g.pipeline(t, t.TempDir(), nil)
		out, err := p.SearchGitHub(context.Background())
		if err != nil || out.Stopped != "rate limited" || out.Listed != 1 || out.Feeds != 1 || out.Errors != 0 {
			t.Errorf("%d: %+v %v", status, out, err)
		}
		if state := readGitHubState(t, p); state["u0/geofeed"] == nil || state["u1/geofeed"] != nil || state["u2/geofeed"] != nil {
			t.Errorf("%d: state = %v", status, state)
		}
		// The next run continues.
		g.treeFn = nil
		out, _ = p.SearchGitHub(context.Background())
		if out.Listed != 2 || out.Feeds != 3 || out.Stopped != "" {
			t.Errorf("%d next: %+v", status, out)
		}
	}
}

func TestGitHubErrorsLeaveRepoUnlisted(t *testing.T) {
	g := newFakeGitHub(t)
	g.repos = []map[string]any{repoItem("alice/geofeed", "2026-11-01T00:00:00Z"), repoItem("bob/geofeed", "2026-11-01T00:00:00Z"), repoItem("carol/geofeed", "2026-11-01T00:00:00Z")}
	g.trees["alice/geofeed"] = []githubTreeEntry{blob("geofeed.csv", 900)}
	g.files["alice/geofeed/main/geofeed.csv"] = ghFeed
	g.trees["bob/geofeed"] = []githubTreeEntry{blob("geofeed.csv", 900)} // its file is a 404: still an answer
	g.treeFn = func(name string) (int, string) {
		if name == "carol/geofeed" {
			return 502, "bad gateway"
		}
		return 0, ""
	}
	p := g.pipeline(t, t.TempDir(), nil)
	out, err := p.SearchGitHub(context.Background())
	if err != nil || out.Errors != 1 || out.Listed != 3 || out.Feeds != 1 {
		t.Fatalf("out = %+v %v", out, err)
	}
	state := readGitHubState(t, p)
	if state["bob/geofeed"] == nil || len(state["bob/geofeed"].Files) != 0 || (state["carol/geofeed"] != nil && state["carol/geofeed"].ListedAt != "") {
		t.Errorf("state = %v", state)
	}
	// A deleted repository (404 on the tree) is listed with no files.
	g.treeFn = nil
	out, _ = p.SearchGitHub(context.Background())
	if out.Listed != 1 || readGitHubState(t, p)["carol/geofeed"] == nil {
		t.Errorf("retry: %+v", out)
	}
	// A server error on a candidate file leaves the repository unlisted.
	g2 := newFakeGitHub(t)
	g2.repos = []map[string]any{repoItem("dave/geofeed", "2026-11-01T00:00:00Z")}
	g2.trees["dave/geofeed"] = []githubTreeEntry{blob("geofeed.csv", 900)}
	g2.raw.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) })
	p2 := g2.pipeline(t, t.TempDir(), nil)
	out, _ = p2.SearchGitHub(context.Background())
	if out.Errors != 1 || out.Feeds != 0 || readGitHubState(t, p2)["dave/geofeed"].ListedAt != "" {
		t.Errorf("file 500: %+v", out)
	}
}

func TestGitHubSearchFailureLeavesState(t *testing.T) {
	g := newFakeGitHub(t)
	g.repos = []map[string]any{repoItem("alice/geofeed", "2026-11-01T00:00:00Z")}
	g.trees["alice/geofeed"] = []githubTreeEntry{blob("geofeed.csv", 900)}
	g.files["alice/geofeed/main/geofeed.csv"] = ghFeed
	p := g.pipeline(t, t.TempDir(), nil)
	if _, err := p.SearchGitHub(context.Background()); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(p.dir(), GitHubFile)
	before, _ := os.ReadFile(file)
	// 100 days later the search fails (a 500, bad JSON, an empty answer of an outage): nothing is removed.
	later := func() time.Time { return time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC) }
	p.now = later
	for _, answer := range []struct {
		status int
		body   string
		hdr    http.Header
	}{{500, "boom", nil}, {200, "not json", nil}, {403, "{}", nil}, {451, "{}", nil}, {429, "{}", nil}, {403, "{}", http.Header{"Retry-After": {"5"}}}} {
		g.hdr = answer.hdr
		g.searchFn = func(int) (int, string) { return answer.status, answer.body }
		out, err := p.SearchGitHub(context.Background())
		after, _ := os.ReadFile(file)
		if err != nil || out.Skipped != "search failed" || out.Feeds != 1 || out.Removed != 0 || string(after) != string(before) {
			t.Errorf("%d: %+v %v", answer.status, out, err)
		}
		// A 403 with no rate-limit header is a plain failure.
		if limited := answer.status == 429 || answer.hdr != nil; limited != (out.Stopped == "rate limited") {
			t.Errorf("%d %v: stopped %q", answer.status, answer.hdr, out.Stopped)
		}
	}
	// A page that is too large is a failure too.
	g.searchFn = func(int) (int, string) { return 200, `{"items":[],"pad":"` + strings.Repeat("x", githubMaxJSON) + `"}` }
	out, err := p.SearchGitHub(context.Background())
	if err != nil || out.Skipped != "search failed" {
		t.Errorf("oversized: %+v %v", out, err)
	}
	// A damaged state file is read as empty and replaced.
	g.searchFn = nil
	os.WriteFile(file, []byte(`{"alice/geofeed":`), 0o640)
	var logs []string
	p.Progress = func(s string) { logs = append(logs, s) }
	p.now = func() time.Time { return time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC) }
	out, err = p.SearchGitHub(context.Background())
	if err != nil || out.Listed != 1 || out.Feeds != 1 || !strings.Contains(strings.Join(logs, "\n"), "damaged") {
		t.Errorf("damaged state: %+v %v %q", out, err, logs)
	}
}

func TestGitHubRemovalAfter30Days(t *testing.T) {
	g := newFakeGitHub(t)
	g.repos = []map[string]any{repoItem("alice/geofeed", "2026-11-01T00:00:00Z"), repoItem("bob/geofeed", "2026-11-01T00:00:00Z")}
	for _, name := range []string{"alice/geofeed", "bob/geofeed"} {
		g.trees[name] = []githubTreeEntry{blob("geofeed.csv", 900)}
		g.files[name+"/main/geofeed.csv"] = ghFeed
	}
	p := g.pipeline(t, t.TempDir(), nil)
	start := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	if _, err := p.SearchGitHub(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Bob leaves the search; alice stays.
	g.repos = g.repos[:1]
	p.now = func() time.Time { return start.Add(29 * 24 * time.Hour) }
	out, _ := p.SearchGitHub(context.Background())
	if out.Removed != 0 || out.Feeds != 2 {
		t.Errorf("day 29: %+v", out)
	}
	p.now = func() time.Time { return start.Add(31 * 24 * time.Hour) }
	out, _ = p.SearchGitHub(context.Background())
	state := readGitHubState(t, p)
	if out.Removed != 1 || out.Feeds != 1 || state["bob/geofeed"] != nil || state["alice/geofeed"] == nil {
		t.Errorf("day 31: %+v %v", out, state)
	}
}

func TestGitHubTokenOnlyToTheAPI(t *testing.T) {
	const token = "ghp_secretsecretsecret"
	g := newFakeGitHub(t)
	g.repos = []map[string]any{repoItem("alice/geofeed", "2026-11-01T00:00:00Z")}
	g.trees["alice/geofeed"] = []githubTreeEntry{blob("geofeed.csv", 900)}
	g.files["alice/geofeed/main/geofeed.csv"] = ghFeed
	var logs []string
	p := g.pipeline(t, t.TempDir(), &logs)
	p.GitHubToken = token
	out, err := p.SearchGitHub(context.Background())
	if err != nil || !out.Token || out.Feeds != 1 {
		t.Fatalf("out = %+v %v", out, err)
	}
	g.mu.Lock()
	apiAuth, rawAuth := slices.Clone(g.apiAuth), slices.Clone(g.rawAuth)
	g.mu.Unlock()
	if len(apiAuth) == 0 || len(rawAuth) == 0 {
		t.Fatalf("auth headers: api %v raw %v", apiAuth, rawAuth)
	}
	for _, a := range apiAuth {
		if a != "Bearer "+token {
			t.Errorf("API Authorization = %q", a)
		}
	}
	for _, a := range rawAuth {
		if a != "" {
			t.Errorf("raw host got Authorization %q", a)
		}
	}
	data, _ := json.Marshal(out)
	state, _ := os.ReadFile(filepath.Join(p.dir(), GitHubFile))
	for what, text := range map[string]string{"output": string(data), "logs": strings.Join(logs, "\n"), "state": string(state)} {
		if strings.Contains(text, token) {
			t.Errorf("%s holds the token", what)
		}
	}
}

// A failing server or transport never puts the token (or a URL) into an output, a log or an error.
func TestGitHubErrorsNeverLeakTheToken(t *testing.T) {
	const token = "ghp_secretsecretsecret"
	run := func(what string, setup func(g *fakeGitHub, p *Pipeline)) {
		g := newFakeGitHub(t)
		g.repos = []map[string]any{repoItem("alice/geofeed", "2026-11-01T00:00:00Z")}
		g.trees["alice/geofeed"] = []githubTreeEntry{blob("geofeed.csv", 900)}
		g.files["alice/geofeed/main/geofeed.csv"] = ghFeed
		var logs []string
		p := g.pipeline(t, t.TempDir(), &logs)
		p.GitHubToken = token
		setup(g, p)
		out, err := p.SearchGitHub(context.Background())
		data, _ := json.Marshal(out)
		text := string(data) + strings.Join(logs, "\n")
		if err != nil {
			text += err.Error()
		}
		if strings.Contains(text, token) || strings.Contains(text, "127.0.0.1") {
			t.Errorf("%s: leak in %q", what, text)
		}
	}
	run("server echoes the header", func(g *fakeGitHub, p *Pipeline) {
		g.searchFn = func(int) (int, string) { return 500, token }
	})
	run("tree error echoes the header", func(g *fakeGitHub, p *Pipeline) {
		g.treeFn = func(string) (int, string) { return 500, "Bearer " + token }
	})
	run("connection closed", func(g *fakeGitHub, p *Pipeline) {
		g.api.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		})
	})
	run("API down", func(g *fakeGitHub, p *Pipeline) {
		g.api.Close()
		p.ghAPI = "http://127.0.0.1:1"
	})
	run("tree transport error", func(g *fakeGitHub, p *Pipeline) {
		inner := g.api.Config.Handler
		g.api.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/git/trees/") {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			inner.ServeHTTP(w, r)
		})
	})
}

// A candidate file larger than the limit is not read as a feed.
func TestGitHubOversizedFile(t *testing.T) {
	g := newFakeGitHub(t)
	g.repos = []map[string]any{repoItem("alice/geofeed", "2026-11-01T00:00:00Z")}
	g.trees["alice/geofeed"] = []githubTreeEntry{blob("geofeed.csv", 900)} // the tree says it is small
	g.raw.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < githubMaxFile/len(ghFeed)+10; i++ {
			if _, err := fmt.Fprint(w, ghFeed); err != nil {
				return
			}
		}
	})
	p := g.pipeline(t, t.TempDir(), nil)
	out, err := p.SearchGitHub(context.Background())
	if err != nil || out.Feeds != 0 || out.FilesChecked != 1 || out.Listed != 1 || out.Errors != 0 {
		t.Errorf("out = %+v %v", out, err)
	}
}

func TestGitHubCancelled(t *testing.T) {
	g := newFakeGitHub(t)
	g.repos = []map[string]any{repoItem("alice/geofeed", "2026-11-01T00:00:00Z")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.pipeline(t, t.TempDir(), nil).SearchGitHub(ctx); err == nil {
		t.Error("a cancelled run must fail")
	}
}

// discover lists the files of github.json with the source "github" and no block.
func TestDiscoverGitHub(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	store := newFakeStore()
	p := &Pipeline{
		store: store, DataDir: t.TempDir(), dumps: []dump{}, caidaBase: server.URL + "/caida", listPages: []string{},
		now: func() time.Time { return time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC) }, allowPrivate: true,
	}
	// No file yet: none, and discover still works.
	out, err := p.Discover(context.Background())
	if err != nil || out.Sources[sourceGitHub].URLs != 0 || out.Sources[sourceGitHub].Status != "searched" {
		t.Fatalf("no file: %+v %v", out.Sources[sourceGitHub], err)
	}
	state := map[string]*githubRepo{
		"alice/geofeed": {PushedAt: "x", Branch: "main", ListedAt: "x", SeenAt: "x", Files: []string{"geofeed.csv", "my dir/v6.csv"}},
		"bob/geofeed":   {PushedAt: "x", Branch: "dev", ListedAt: "x", SeenAt: "x", Files: []string{"g.txt"}},
		"carol/geofeed": {PushedAt: "x", Branch: "main", ListedAt: "x", SeenAt: "x"},
	}
	if err := saveGitHub(filepath.Join(p.dir(), GitHubFile), state); err != nil {
		t.Fatal(err)
	}
	out, err = p.Discover(context.Background())
	if err != nil || out.Sources[sourceGitHub].URLs != 3 {
		t.Fatalf("with file: %+v %v", out.Sources[sourceGitHub], err)
	}
	for u, want := range map[string][]string{
		"https://raw.githubusercontent.com/alice/geofeed/main/geofeed.csv":     {"github"},
		"https://raw.githubusercontent.com/alice/geofeed/main/my%20dir/v6.csv": {"github"},
		"https://raw.githubusercontent.com/bob/geofeed/dev/g.txt":              {"github"},
	} {
		if row := store.rows[u]; row == nil || !reflect.DeepEqual(row.Sources, want) {
			t.Errorf("%s: %+v", u, row)
		}
	}
	blocks, _ := os.ReadFile(filepath.Join(p.dir(), BlocksFile))
	if strings.Contains(string(blocks), "github") {
		t.Errorf("blocks.csv names github: %s", blocks)
	}
	// An unreadable file adds none and never fails discover.
	os.WriteFile(filepath.Join(p.dir(), GitHubFile), []byte("{"), 0o640)
	out, err = p.Discover(context.Background())
	if err != nil || out.Sources[sourceGitHub].URLs != 0 {
		t.Errorf("damaged file: %+v %v", out.Sources[sourceGitHub], err)
	}
}

// A 403 or 451 on a tree without rate-limit headers (a blocked repository) is an answer about that
// repository: it is recorded as listed with no files and the run goes on.
func TestGitHubBlockedRepoDoesNotStopTheRun(t *testing.T) {
	for _, status := range []int{403, 451} {
		g := newFakeGitHub(t)
		for i := 0; i < 3; i++ {
			name := fmt.Sprintf("u%d/geofeed", i)
			g.repos = append(g.repos, repoItem(name, "2026-11-01T00:00:00Z"))
			g.trees[name] = []githubTreeEntry{blob("geofeed.csv", 900)}
			g.files[name+"/main/geofeed.csv"] = ghFeed
		}
		g.treeFn = func(name string) (int, string) {
			if name == "u1/geofeed" {
				return status, `{"message":"blocked"}`
			}
			return 0, ""
		}
		p := g.pipeline(t, t.TempDir(), nil)
		out, err := p.SearchGitHub(context.Background())
		state := readGitHubState(t, p)
		if err != nil || out.Stopped != "" || out.Listed != 3 || out.Feeds != 2 || out.Errors != 0 ||
			state["u1/geofeed"] == nil || len(state["u1/geofeed"].Files) != 0 || state["u2/geofeed"] == nil {
			t.Errorf("%d: %+v %v", status, out, err)
		}
	}
}

// A 429 from the raw file host stops the run like an API limit; the repository stays unlisted.
func TestGitHubRawRateLimit(t *testing.T) {
	g := newFakeGitHub(t)
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("u%d/geofeed", i)
		g.repos = append(g.repos, repoItem(name, "2026-11-01T00:00:00Z"))
		g.trees[name] = []githubTreeEntry{blob("geofeed.csv", 900)}
		g.files[name+"/main/geofeed.csv"] = ghFeed
	}
	g.rawFn = func(path string) int {
		if strings.HasPrefix(path, "/u1/") {
			return 429
		}
		return 0
	}
	p := g.pipeline(t, t.TempDir(), nil)
	out, err := p.SearchGitHub(context.Background())
	state := readGitHubState(t, p)
	_, raw := g.hits()
	if err != nil || out.Stopped != "rate limited" || out.Listed != 1 || out.Errors != 0 || out.Feeds != 1 ||
		state["u0/geofeed"] == nil || state["u1/geofeed"] != nil || len(raw) != 2 {
		t.Errorf("out = %+v %v state %v raw %v", out, err, state, raw)
	}
}

// Owner or repository "." and ".." are refused when the state is read too.
func TestLoadGitHubRejectsDotNames(t *testing.T) {
	file := filepath.Join(t.TempDir(), GitHubFile)
	repo := githubRepo{Branch: "main", Files: []string{"g.csv"}}
	if err := saveGitHub(file, map[string]*githubRepo{"alice/geofeed": &repo, "../geofeed": &repo, "alice/..": &repo, "./x": &repo}); err != nil {
		t.Fatal(err)
	}
	state, damaged, err := loadGitHub(file)
	if err != nil || damaged || len(state) != 1 || state["alice/geofeed"] == nil {
		t.Errorf("state = %v %v %v", state, damaged, err)
	}
}

// Never-listed repositories come first, then the changed ones, the one listed longest ago first:
// repositories that push every day must not use the whole budget.
func TestGitHubDueOrder(t *testing.T) {
	g := newFakeGitHub(t)
	add := func(name string) {
		g.repos = append(g.repos, repoItem(name, "2026-11-01T00:00:00Z"))
		g.trees[name] = []githubTreeEntry{blob("geofeed.csv", 900)}
		g.files[name+"/main/geofeed.csv"] = ghFeed
	}
	p := g.pipeline(t, t.TempDir(), nil)
	day := func(d int) func() time.Time {
		return func() time.Time { return time.Date(2026, 11, d, 5, 0, 0, 0, time.UTC) }
	}
	// a, b and c are listed on three different days.
	for i, name := range []string{"a/geofeed", "b/geofeed", "c/geofeed"} {
		add(name)
		p.now = day(10 + i)
		if out, err := p.SearchGitHub(context.Background()); err != nil || out.Listed != 1 {
			t.Fatalf("%s: %+v %v", name, out, err)
		}
	}
	// All three push again; two new ones appear. Search order puts the changed ones first.
	add("d/geofeed")
	add("e/geofeed")
	g.repos = []map[string]any{g.repos[2], g.repos[1], g.repos[0], g.repos[3], g.repos[4]} // c, b, a, d, e
	for _, r := range g.repos[:3] {
		r["pushed_at"] = "2026-11-12T00:00:00Z"
	}
	p.now = day(20)
	p.ghBudget = 2
	out, err := p.SearchGitHub(context.Background())
	state := readGitHubState(t, p)
	if err != nil || out.Listed != 2 || out.Stopped != "budget" || state["d/geofeed"] == nil || state["e/geofeed"] == nil ||
		state["a/geofeed"].PushedAt != "2026-11-01T00:00:00Z" {
		t.Fatalf("new ones first: %+v %v", out, err)
	}
	// Now only changed ones are due: the oldest listed (a, then b) go first, c waits.
	p.now = day(21)
	out, err = p.SearchGitHub(context.Background())
	state = readGitHubState(t, p)
	if err != nil || out.Listed != 2 || state["a/geofeed"].PushedAt != "2026-11-12T00:00:00Z" || state["b/geofeed"].PushedAt != "2026-11-12T00:00:00Z" ||
		state["c/geofeed"].PushedAt != "2026-11-01T00:00:00Z" {
		t.Errorf("oldest first: %+v %v", out, err)
	}
}

// A token the API rejects (401) is dropped for the rest of the run, the request is repeated
// without it, and the anonymous budget applies; the token is never shown.
func TestGitHubTokenRejected(t *testing.T) {
	const token = "ghp_rejectedrejectedrejected"
	g := newFakeGitHub(t)
	g.rejectAuth = true
	for i := 0; i < 45; i++ {
		name := fmt.Sprintf("u%d/geofeed", i)
		g.repos = append(g.repos, repoItem(name, "2026-11-01T00:00:00Z"))
		g.trees[name] = []githubTreeEntry{blob("geofeed.csv", 900)}
		g.files[name+"/main/geofeed.csv"] = ghFeed
	}
	var logs []string
	p := g.pipeline(t, t.TempDir(), &logs)
	p.GitHubToken = token
	out, err := p.SearchGitHub(context.Background())
	if err != nil || !out.TokenRejected || !out.Token || out.Listed != githubListBudget || out.Stopped != "budget" || out.Skipped != "" || out.Errors != 0 {
		t.Fatalf("out = %+v %v", out, err)
	}
	if p.GitHubToken != token {
		t.Error("the pipeline's token was changed")
	}
	g.mu.Lock()
	auth := append([]string(nil), g.apiAuth...)
	g.mu.Unlock()
	// Only the very first request carried the header.
	for i, a := range auth {
		if (i == 0) != (a != "") {
			t.Fatalf("request %d: Authorization %q", i, a)
		}
	}
	data, _ := json.Marshal(out)
	if text := string(data) + strings.Join(logs, "\n"); strings.Contains(text, token) || !strings.Contains(text, "rejected") {
		t.Errorf("output and logs: %s", text)
	}
}

// A truncated tree is used as it came and counted; so is one larger than the read limit.
func TestGitHubIncompleteTrees(t *testing.T) {
	g := newFakeGitHub(t)
	g.repos = []map[string]any{repoItem("alice/geofeed", "2026-11-01T00:00:00Z"), repoItem("bob/geofeed", "2026-11-01T00:00:00Z")}
	g.files["alice/geofeed/main/geofeed.csv"] = ghFeed
	g.treeFn = func(name string) (int, string) {
		if name == "alice/geofeed" {
			return 200, `{"tree":[{"path":"geofeed.csv","type":"blob","size":900}],"truncated":true}`
		}
		return 200, `{"tree":[],"pad":"` + strings.Repeat("x", githubMaxJSON) + `"}`
	}
	p := g.pipeline(t, t.TempDir(), nil)
	out, err := p.SearchGitHub(context.Background())
	state := readGitHubState(t, p)
	if err != nil || out.Incomplete != 2 || out.Listed != 2 || out.Feeds != 1 || state["alice/geofeed"] == nil || state["bob/geofeed"] == nil || out.Errors != 0 {
		t.Errorf("out = %+v %v", out, err)
	}
}

// The check directory is made new for each run (a killed run's leftovers go) and removed at its end.
func TestGitHubCheckDir(t *testing.T) {
	g := newFakeGitHub(t)
	g.repos = []map[string]any{repoItem("alice/geofeed", "2026-11-01T00:00:00Z")}
	g.trees["alice/geofeed"] = []githubTreeEntry{blob("geofeed.csv", 900)}
	g.files["alice/geofeed/main/geofeed.csv"] = ghFeed
	p := g.pipeline(t, t.TempDir(), nil)
	dir := filepath.Join(p.dir(), githubCheckDir)
	os.MkdirAll(dir, 0o750)
	os.WriteFile(filepath.Join(dir, "check-left-behind"), []byte("x"), 0o640)
	out, err := p.SearchGitHub(context.Background())
	if err != nil || out.Feeds != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the check directory was left")
	}
}

// incomplete_results: what came is used, but nothing is removed in that run.
func TestGitHubIncompleteSearchKeepsRepos(t *testing.T) {
	g := newFakeGitHub(t)
	g.repos = []map[string]any{repoItem("alice/geofeed", "2026-11-01T00:00:00Z"), repoItem("bob/geofeed", "2026-11-01T00:00:00Z")}
	for _, n := range []string{"alice/geofeed", "bob/geofeed"} {
		g.trees[n] = []githubTreeEntry{blob("geofeed.csv", 900)}
		g.files[n+"/main/geofeed.csv"] = ghFeed
	}
	p := g.pipeline(t, t.TempDir(), nil)
	start := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	if _, err := p.SearchGitHub(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Bob is missing from a search that says it is incomplete, 40 days later: he stays.
	g.searchFn = func(int) (int, string) {
		return 200, `{"incomplete_results":true,"items":[{"full_name":"alice/geofeed","size":10,"default_branch":"main","pushed_at":"2026-11-01T00:00:00Z"}]}`
	}
	p.now = func() time.Time { return start.Add(40 * 24 * time.Hour) }
	out, err := p.SearchGitHub(context.Background())
	if err != nil || out.Removed != 0 || readGitHubState(t, p)["bob/geofeed"] == nil {
		t.Errorf("incomplete: %+v %v", out, err)
	}
	// A complete search removes him.
	g.searchFn = nil
	g.repos = g.repos[:1]
	out, _ = p.SearchGitHub(context.Background())
	if out.Removed != 1 {
		t.Errorf("complete: %+v", out)
	}
}

// A repository whose first listing keeps failing must not take the first budget slot in every run.
func TestGitHubFailedRepositoriesGoAfterNewOnes(t *testing.T) {
	g := newFakeGitHub(t)
	for _, n := range []string{"a/geofeed", "b/geofeed"} {
		g.repos = append(g.repos, repoItem(n, "2026-11-01T00:00:00Z"))
		g.trees[n] = []githubTreeEntry{blob("geofeed.csv", 900)}
		g.files[n+"/main/geofeed.csv"] = ghFeed
	}
	g.treeFn = func(name string) (int, string) {
		if name == "a/geofeed" || name == "b/geofeed" {
			return 502, "down"
		}
		return 0, ""
	}
	p := g.pipeline(t, t.TempDir(), nil)
	out, err := p.SearchGitHub(context.Background())
	state := readGitHubState(t, p)
	if err != nil || out.Errors != 2 || state["a/geofeed"] == nil || state["a/geofeed"].FailedAt == "" || state["a/geofeed"].ListedAt != "" {
		t.Fatalf("failed run: %+v %v %+v", out, err, state["a/geofeed"])
	}
	for _, n := range []string{"c/geofeed", "d/geofeed"} {
		g.repos = append(g.repos, repoItem(n, "2026-11-01T00:00:00Z"))
		g.trees[n] = []githubTreeEntry{blob("geofeed.csv", 900)}
		g.files[n+"/main/geofeed.csv"] = ghFeed
	}
	p.ghBudget = 2
	p.now = func() time.Time { return time.Date(2026, 11, 18, 5, 0, 0, 0, time.UTC) }
	out, err = p.SearchGitHub(context.Background())
	state = readGitHubState(t, p)
	if err != nil || out.Listed != 2 || out.Errors != 0 || state["c/geofeed"].ListedAt == "" || state["d/geofeed"].ListedAt == "" || state["a/geofeed"].ListedAt != "" {
		t.Errorf("new ones first: %+v %v", out, err)
	}
	// The failed ones are tried next, once the server is back; a success replaces the entry.
	g.treeFn = nil
	out, _ = p.SearchGitHub(context.Background())
	state = readGitHubState(t, p)
	if out.Listed != 2 || state["a/geofeed"].ListedAt == "" || state["a/geofeed"].FailedAt != "" || out.Feeds != 4 {
		t.Errorf("failed ones: %+v", out)
	}
}

// The soft time budget ends the listing cleanly; the state is saved and the task succeeds.
func TestGitHubRunTimeBudget(t *testing.T) {
	g := newFakeGitHub(t)
	for i := 0; i < 3; i++ {
		n := fmt.Sprintf("u%d/geofeed", i)
		g.repos = append(g.repos, repoItem(n, "2026-11-01T00:00:00Z"))
		g.trees[n] = []githubTreeEntry{blob("geofeed.csv", 900)}
		g.files[n+"/main/geofeed.csv"] = ghFeed
	}
	p := g.pipeline(t, t.TempDir(), nil)
	p.ghRunTime = time.Nanosecond
	out, err := p.SearchGitHub(context.Background())
	if err != nil || out.Stopped != "time" || out.Listed != 0 {
		t.Errorf("out = %+v %v", out, err)
	}
	if (&Pipeline{}).githubRunTime() != 45*time.Minute {
		t.Error("default run budget")
	}
	p.ghRunTime = 0
	if out, err = p.SearchGitHub(context.Background()); err != nil || out.Listed != 3 || out.Stopped != "" {
		t.Errorf("next run: %+v %v", out, err)
	}
}
