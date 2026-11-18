package geofeeds

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"openipdata.org/hatchet/outputs"
)

// The GitHub search (task search-github). Operators publish geofeed files (RFC 8805) in public
// GitHub repositories that no whois record links. The task searches for repositories whose name
// contains "geofeed", lists the files of each, keeps the ones that read as a geofeed and writes
// them to GitHubFile. Like probe-domains it has no parent and no child: discover reads whatever
// the file holds when it gets there, so a file found in this run reaches the rows in the next one.
//
// The token (GITHUB_TOKEN, optional) is sent only to the API, never to the raw file host. Every
// GitHub answer is untrusted: bodies are size-limited and every field is checked. Nothing that
// goes wrong with GitHub fails the task; it ends with what it has and says why in its output.
const (
	// GitHubFile, in <data>/geofeeds/, holds the repositories found and the files accepted as
	// feeds in each. It is sync-geofeeds' own state: losing it costs a search and a listing run.
	GitHubFile = "github.json"

	// sourceGitHub labels the feeds found in GitHub repositories.
	sourceGitHub = "github"

	githubAPI = "https://api.github.com"
	githubRaw = "https://raw.githubusercontent.com"

	githubMaxJSON      = 20 << 20 // bytes read from a search or tree answer
	githubMaxFile      = 32 << 20 // bytes of a candidate file: larger ones are not read
	githubMinFile      = 20       // a file smaller than this holds no row
	githubFilesPerRepo = 8        // candidate files fetched per repository
	githubKeepDays     = 30       // a repository the search no longer returns stays this long
	githubSearchPages  = 10       // the search returns at most 1000 repositories
	githubPageSize     = 100
	githubSaveEvery    = 25 // repositories listed between saves
	// githubListBudget is how many repositories one run lists: the unauthenticated API
	// allows 60 requests an hour.
	githubListBudget      = 40
	githubListBudgetToken = 1000
	githubWaitAnon        = 7 * time.Second // between search requests
	githubWaitToken       = 2 * time.Second
	githubTimeout         = 2 * time.Minute
	// githubRunBudget is the soft time budget of one run, counted from the start of the task
	// (the task timeout is one hour): the listing ends cleanly when it is used up.
	githubRunBudget = 45 * time.Minute
	// githubCheckDir, in <data>/geofeeds/, holds the temporary files of isFeed.
	githubCheckDir = "github-check"
)

// Where a feed's rows must come from to be kept: a file is a feed when at least this share of its
// data lines is a valid row.
const githubMinValidShare = 0.5

var (
	githubFullName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	githubBranch   = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,100}$`)
	// Names that look like a test or a blacklist, not an operator's feed (from the measurement).
	githubTestName = regexp.MustCompile(`(?i)(^|[^a-z])(test|tests|testing|example|examples|sample|samples|template|templates|demo|fake|dummy|mock|playground|sandbox)([^a-z]|$)`)
	githubListName = regexp.MustCompile(`(?i)(black|block|abuse|spam|malware|threat|attack|honeypot|botnet|scanner|firehol|ipset|reputation)`)
)

// Directory segments whose files are never feeds, and file name prefixes that are never feeds.
var (
	githubSkipDirs     = []string{"node_modules", "vendor", "test", "tests", "testdata", "example", "examples"}
	githubSkipPrefixes = []string{"rfc", "readme", "license", "licence", "."}
)

// GitHubOutput is the search-github task's result: counts only.
type GitHubOutput struct {
	Repos           int     `json:"repos"`         // repositories of the search that passed the checks
	SkippedNames    int     `json:"skipped_names"` // test-like or blacklist-like names
	SkippedOther    int     `json:"skipped_other"` // any other failed check
	Listed          int     `json:"listed"`        // repositories whose files were looked at
	FilesChecked    int     `json:"files_checked"` // candidate files fetched
	Feeds           int     `json:"feeds"`         // files accepted as feeds, all repositories, after this run
	NewFeeds        int     `json:"new_feeds"`
	Errors          int     `json:"errors"` // requests that got no usable answer
	Removed         int     `json:"removed"`
	Incomplete      int     `json:"incomplete"`     // trees that were truncated or larger than the read limit
	Token           bool    `json:"token"`          // a token was set
	TokenRejected   bool    `json:"token_rejected"` // the API answered 401: the rest of the run was anonymous
	Stopped         string  `json:"stopped,omitempty"`
	Skipped         string  `json:"skipped,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// githubRepo is what the state keeps of one repository.
type githubRepo struct {
	PushedAt string   `json:"pushed_at"`           // when it was last listed
	FailedAt string   `json:"failed_at,omitempty"` // a never-listed repository whose listing failed, last time
	Branch   string   `json:"branch"`
	ListedAt string   `json:"listed_at"`
	SeenAt   string   `json:"seen_at"` // the last search that returned it
	Files    []string `json:"files"`   // paths accepted as feeds
}

// githubItem is one repository of a search answer. Every field is untrusted.
type githubItem struct {
	FullName      string `json:"full_name"`
	Fork          bool   `json:"fork"`
	Private       bool   `json:"private"`
	Disabled      bool   `json:"disabled"`
	Size          int64  `json:"size"`
	DefaultBranch string `json:"default_branch"`
	PushedAt      string `json:"pushed_at"`
}

// githubTreeEntry is one entry of a repository tree.
type githubTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// Reasons a repository is not used.
const (
	repoOK = iota
	repoSkipName
	repoSkipOther
)

// checkRepo applies the rules of a search result: it is used only when all of them hold.
func checkRepo(it githubItem) int {
	if !validFullName(it.FullName) || it.Fork || it.Private || it.Disabled || it.Size <= 0 {
		return repoSkipOther
	}
	_, repo, _ := strings.Cut(it.FullName, "/")
	if !strings.Contains(strings.ToLower(repo), "geofeed") || !validBranch(it.DefaultBranch) ||
		it.PushedAt == "" || len(it.PushedAt) > 40 {
		return repoSkipOther
	}
	if githubTestName.MatchString(it.FullName) || githubListName.MatchString(it.FullName) {
		return repoSkipName
	}
	return repoOK
}

// validFullName accepts owner/repo of plain characters where neither part is "." or "..".
func validFullName(n string) bool {
	if !githubFullName.MatchString(n) {
		return false
	}
	owner, repo, _ := strings.Cut(n, "/")
	return owner != "." && owner != ".." && repo != "." && repo != ".."
}

// validBranch accepts a branch name that is safe in a URL path.
func validBranch(b string) bool {
	return githubBranch.MatchString(b) && !strings.Contains(b, "..") && !strings.HasPrefix(b, "/") && !strings.HasSuffix(b, "/")
}

// escapePath percent-escapes each segment of a slash-separated path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// githubFileURL is the URL of a file on a raw file host: base/<repo>/<branch>/<path>.
func githubFileURL(base, fullName, branch, filePath string) string {
	return base + "/" + fullName + "/" + escapePath(branch) + "/" + escapePath(filePath)
}

// candidateFiles picks the files of a tree that may be feeds, best first, at most
// githubFilesPerRepo: a .csv or .txt file or one with no extension, not a readme or licence, not
// in a test or vendor directory, of a plausible size, with a plain path.
func candidateFiles(tree []githubTreeEntry) []string {
	var out []string
	for _, e := range tree {
		if e.Type != "blob" || e.Size < githubMinFile || e.Size > githubMaxFile || !plainRepoPath(e.Path) {
			continue
		}
		dir, base := path.Split(e.Path)
		switch strings.ToLower(path.Ext(base)) {
		case ".csv", ".txt", "":
		default:
			continue
		}
		lower := strings.ToLower(base)
		skip := false
		for _, prefix := range githubSkipPrefixes {
			if strings.HasPrefix(lower, prefix) {
				skip = true
			}
		}
		for _, seg := range strings.Split(strings.TrimSuffix(dir, "/"), "/") {
			if slices.Contains(githubSkipDirs, strings.ToLower(seg)) {
				skip = true
			}
		}
		if !skip {
			out = append(out, e.Path)
		}
	}
	hint := func(p string) bool {
		l := strings.ToLower(p)
		return strings.Contains(l, "geo") || strings.Contains(l, "feed")
	}
	slices.SortFunc(out, func(a, b string) int {
		if ha, hb := hint(a), hint(b); ha != hb {
			if ha {
				return -1
			}
			return 1
		}
		if c := strings.Count(a, "/") - strings.Count(b, "/"); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	out = slices.Compact(out)
	if len(out) > githubFilesPerRepo {
		out = out[:githubFilesPerRepo]
	}
	return out
}

// plainRepoPath accepts a path of valid UTF-8, at most 300 bytes, with no control character and
// no empty, "." or ".." segment.
func plainRepoPath(p string) bool {
	if p == "" || len(p) > 300 || !utf8.ValidString(p) {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// isFeed reports whether a file reads as a geofeed: a valid row, and at least half of the data
// lines valid. The check is the feed check's own: writeFeed runs over the body into a temporary file
// in the geofeeds dir (removed afterwards), and its header counts decide. The valid rows are
// header.Rows (a row that only lost its region or city is kept and counted there); the data lines are
// those plus the rows rejected for any other reason. An error is a file error of our own state.
func (p *Pipeline) isFeed(body []byte) (bool, error) {
	if looksLikeHTML(bufio.NewReader(bytes.NewReader(body))) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Join(p.dir(), githubCheckDir), "check-*")
	if err != nil {
		return false, err
	}
	name := tmp.Name()
	tmp.Close()
	defer os.Remove(name)
	defer os.Remove(name + ".tmp")
	header, err := writeFeed(bytes.NewReader(body), name, feedHeader{})
	if err != nil {
		var bodyErr *bodyError
		if errors.As(err, &bodyErr) {
			return false, nil
		}
		return false, err
	}
	valid, data := header.Rows, header.Rows
	for reason, n := range header.Rejected {
		if reason != dropBadRegion && reason != dropBadCity {
			data += n
		}
	}
	return valid > 0 && float64(valid) >= githubMinValidShare*float64(data), nil
}

// githubResult is the answer to one request. A request that got no HTTP answer, or whose body
// could not be read, is an error with a fixed text (never the transport's, which could name a
// URL).
type githubResult struct {
	status int
	body   []byte
	big    bool // the body was longer than the limit; body holds the start
	// limited: GitHub says the rate limit is used up: a 429, or a 403 with a Retry-After header
	// or X-RateLimit-Remaining 0. Any other 403 (or 451) is an answer about the one repository.
	limited bool
}

var errGitHubRequest = errors.New("no usable answer from GitHub")

// githubAuth is the token state of one run. p.GitHubToken is never changed: a token the API
// rejects (401) is dropped here for the rest of the run, and the anonymous limits apply from then on.
type githubAuth struct {
	token string
	out   *GitHubOutput // gets TokenRejected
}

// githubGet requests one URL. The token goes only with api (a request to the API base). A 401 for a
// request sent with the token drops the token for the rest of the run and repeats that one request
// without it. The body is read up to max bytes.
func (p *Pipeline) githubGet(ctx context.Context, client *http.Client, a *githubAuth, rawURL string, api bool, max int64) (githubResult, error) {
	token := ""
	if api {
		token = a.token
	}
	res, err := p.githubDo(ctx, client, rawURL, token, max)
	if err == nil && api && a.token != "" && res.status == http.StatusUnauthorized {
		a.token, a.out.TokenRejected = "", true
		p.log("github: the token was rejected (401); continuing without it")
		return p.githubDo(ctx, client, rawURL, "", max)
	}
	return res, err
}

// githubDo makes one request with the given token ("" sends none).
func (p *Pipeline) githubDo(ctx context.Context, client *http.Client, rawURL, token string, max int64) (githubResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return githubResult{}, errGitHubRequest
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return githubResult{}, errGitHubRequest
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return githubResult{}, errGitHubRequest
	}
	res := githubResult{status: resp.StatusCode, body: body}
	res.limited = res.status == http.StatusTooManyRequests ||
		res.status == http.StatusForbidden && (resp.Header.Get("Retry-After") != "" || strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0")
	if int64(len(body)) > max {
		res.big, res.body = true, body[:max]
	}
	return res, nil
}

func (p *Pipeline) githubAPIBase() string {
	if p.ghAPI != "" {
		return p.ghAPI
	}
	return githubAPI
}

func (p *Pipeline) githubRawBase() string {
	if p.ghRaw != "" {
		return p.ghRaw
	}
	return githubRaw
}

// githubSearchWait is the pause between two search requests.
func (p *Pipeline) githubSearchWait(token bool) time.Duration {
	switch {
	case p.ghNoWait:
		return 0
	case token:
		return githubWaitToken
	}
	return githubWaitAnon
}

// githubRunTime is the soft time budget of a run.
func (p *Pipeline) githubRunTime() time.Duration {
	if p.ghRunTime > 0 {
		return p.ghRunTime
	}
	return githubRunBudget
}

func (p *Pipeline) githubBudget(token bool) int {
	switch {
	case p.ghBudget > 0:
		return p.ghBudget
	case token:
		return githubListBudgetToken
	}
	return githubListBudget
}

// loadGitHub reads GitHubFile. A missing file is an empty state; a damaged one is reported and
// read as empty, so the next save replaces it.
func loadGitHub(file string) (state map[string]*githubRepo, damaged bool, err error) {
	state = map[string]*githubRepo{}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return state, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if json.Unmarshal(data, &state) != nil || state == nil {
		return map[string]*githubRepo{}, true, nil
	}
	for name, r := range state {
		if r == nil || !validFullName(name) || !validBranch(r.Branch) {
			delete(state, name)
			continue
		}
		r.Files = slices.DeleteFunc(r.Files, func(f string) bool { return !plainRepoPath(f) })
	}
	return state, false, nil
}

func saveGitHub(file string, state map[string]*githubRepo) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeFileAtomic(file, data)
}

// githubFeedURLs lists the URL of every accepted file in GitHubFile, sorted, each once. The URL
// names the real raw file host. A missing file means none.
func githubFeedURLs(file string) ([]string, error) {
	state, damaged, err := loadGitHub(file)
	if err != nil {
		return nil, err
	}
	if damaged {
		return nil, fmt.Errorf("%s is damaged", filepath.Base(file))
	}
	var urls []string
	for name, r := range state {
		for _, f := range r.Files {
			urls = append(urls, githubFileURL(githubRaw, name, r.Branch, f))
		}
	}
	slices.Sort(urls)
	return slices.Compact(urls), nil
}

// searchGitHub lists the repositories of the search, page by page. complete is false when a page
// failed after the first. A rate limit is reported as limited.
func (p *Pipeline) searchGitHub(ctx context.Context, client *http.Client, a *githubAuth, out *GitHubOutput) (repos []githubItem, complete, limited, failed bool) {
	seen := map[string]bool{}
	complete = true
	for page := 1; page <= githubSearchPages; page++ {
		if page > 1 {
			select {
			case <-time.After(p.githubSearchWait(a.token != "")):
			case <-ctx.Done():
				return repos, false, false, page == 1
			}
		}
		u := fmt.Sprintf("%s/search/repositories?q=geofeed+in:name&sort=updated&order=desc&per_page=%d&page=%d", p.githubAPIBase(), githubPageSize, page)
		res, err := p.githubGet(ctx, client, a, u, true, githubMaxJSON)
		var body struct {
			Items             []githubItem `json:"items"`
			IncompleteResults bool         `json:"incomplete_results"`
		}
		switch {
		case err != nil:
			out.Errors++
		case res.limited:
			return repos, false, true, page == 1
		case res.status != http.StatusOK || res.big:
			out.Errors++
			err = errGitHubRequest
		default:
			if json.Unmarshal(res.body, &body) != nil {
				out.Errors++
				err = errGitHubRequest
			}
		}
		if err != nil {
			return repos, false, false, page == 1
		}
		if body.IncompleteResults {
			// GitHub says it timed out: what came is used, but nothing is removed in this run.
			complete = false
		}
		for _, it := range body.Items {
			if seen[it.FullName] {
				continue
			}
			seen[it.FullName] = true
			repos = append(repos, it)
		}
		if len(body.Items) < githubPageSize {
			break
		}
	}
	return repos, complete, false, false
}

// Outcomes of listing one repository.
const (
	listDone = iota
	listError
	listLimited
)

// listRepo looks at the files of one repository and returns the paths accepted as feeds. A tree
// that GitHub cut short (truncated) or that is larger than the read limit is counted as incomplete;
// the entries that came are used. The error is a file error of our own state.
func (p *Pipeline) listRepo(ctx context.Context, client *http.Client, a *githubAuth, name, branch string, out *GitHubOutput) ([]string, int, error) {
	u := p.githubAPIBase() + "/repos/" + name + "/git/trees/" + escapePath(branch) + "?recursive=1"
	res, err := p.githubGet(ctx, client, a, u, true, githubMaxJSON)
	switch {
	case err != nil || res.status >= 500:
		out.Errors++
		return nil, listError, nil
	case res.limited:
		return nil, listLimited, nil
	case res.status == http.StatusOK && res.big:
		// Too large to read: nothing to find, and the tree is not complete.
		out.Incomplete++
		return nil, listDone, nil
	case res.status != http.StatusOK:
		// Deleted, empty or blocked: nothing to find.
		return nil, listDone, nil
	}
	var tree struct {
		Tree      []githubTreeEntry `json:"tree"`
		Truncated bool              `json:"truncated"`
	}
	if json.Unmarshal(res.body, &tree) != nil {
		out.Errors++
		return nil, listError, nil
	}
	if tree.Truncated {
		out.Incomplete++
	}
	var accepted []string
	for _, f := range candidateFiles(tree.Tree) {
		if ctx.Err() != nil {
			return nil, listError, nil
		}
		out.FilesChecked++
		res, err := p.githubGet(ctx, client, a, githubFileURL(p.githubRawBase(), name, branch, f), false, githubMaxFile)
		if err == nil && res.status == http.StatusTooManyRequests {
			// The raw host asks us to slow down: stop the run like an API limit; the repository stays unlisted.
			return nil, listLimited, nil
		}
		if err != nil || res.status >= 500 {
			out.Errors++
			return nil, listError, nil
		}
		if res.status == http.StatusOK && !res.big {
			ok, err := p.isFeed(res.body)
			if err != nil {
				return nil, listError, err
			}
			if ok {
				accepted = append(accepted, f)
			}
		}
	}
	return accepted, listDone, nil
}

// SearchGitHub searches GitHub for repositories named like a geofeed, lists the files of the ones
// that are new or changed, and records the files that read as geofeeds in GitHubFile. It fails only
// for a cancelled context or a file error on its own state; whatever GitHub does is in the output.
func (p *Pipeline) SearchGitHub(ctx context.Context) (GitHubOutput, error) {
	started := time.Now()
	out := GitHubOutput{Token: p.GitHubToken != ""}
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return out, err
	}
	// The temporary files of isFeed have a directory of their own, made new for each run and
	// removed at its end, so a killed run leaves nothing for good.
	checkDir := filepath.Join(p.dir(), githubCheckDir)
	if err := os.RemoveAll(checkDir); err != nil {
		return out, err
	}
	if err := os.MkdirAll(checkDir, 0o750); err != nil {
		return out, err
	}
	defer os.RemoveAll(checkDir)
	file := filepath.Join(p.dir(), GitHubFile)
	state, damaged, err := loadGitHub(file)
	if err != nil {
		return out, err
	}
	if damaged {
		p.log("github: %s is damaged; starting empty", GitHubFile)
	}
	now := p.clock().UTC()
	stamp := now.Format(time.RFC3339)
	client := newHTTPClient(githubTimeout, p.allowPrivate)
	auth := &githubAuth{token: p.GitHubToken, out: &out}

	items, complete, limited, failed := p.searchGitHub(ctx, client, auth, &out)
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if failed || limited && len(items) == 0 {
		// A GitHub outage must never empty the state.
		out.Skipped = "search failed"
		if limited {
			out.Stopped = "rate limited"
		}
		out.Feeds = countFeeds(state)
		p.log("github: search failed%s", errSuffix(out.Stopped))
		out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
		return out, nil
	}

	var used []githubItem
	for _, it := range items {
		switch checkRepo(it) {
		case repoOK:
			used = append(used, it)
		case repoSkipName:
			out.SkippedNames++
		default:
			out.SkippedOther++
		}
	}
	out.Repos = len(used)
	// Due: first the repositories never tried, in search order; then the ones never listed whose
	// last try failed, the oldest failure first; then the changed ones, the one listed longest ago first, so repositories that push daily cannot starve the rest of the budget.
	var fresh, failedBefore, changed []githubItem
	for _, it := range used {
		r := state[it.FullName]
		if r != nil {
			r.SeenAt = stamp
		}
		switch {
		case r == nil:
			fresh = append(fresh, it)
		case r.ListedAt == "":
			failedBefore = append(failedBefore, it) // never listed, and the last try failed
		case r.PushedAt != it.PushedAt || r.Branch != it.DefaultBranch:
			changed = append(changed, it)
		}
	}
	slices.SortStableFunc(changed, func(x, y githubItem) int {
		if c := strings.Compare(state[x.FullName].ListedAt, state[y.FullName].ListedAt); c != 0 {
			return c
		}
		return strings.Compare(x.FullName, y.FullName)
	})
	slices.SortStableFunc(failedBefore, func(x, y githubItem) int {
		if c := strings.Compare(state[x.FullName].FailedAt, state[y.FullName].FailedAt); c != 0 {
			return c
		}
		return strings.Compare(x.FullName, y.FullName)
	})
	due := append(append(fresh, failedBefore...), changed...)
	if complete && !limited {
		for name, r := range state {
			if seen, err := time.Parse(time.RFC3339, r.SeenAt); err != nil || now.Sub(seen) > githubKeepDays*24*time.Hour {
				delete(state, name)
				out.Removed++
			}
		}
	}
	if err := saveGitHub(file, state); err != nil {
		return out, err
	}
	if limited {
		out.Stopped = "rate limited"
	}
	p.log("github: %d repositories (%d skipped), %d to list (budget %d)", out.Repos, out.SkippedNames+out.SkippedOther, len(due), p.githubBudget(auth.token != ""))

	pace := outputs.Pace{Every: outputs.ProgressEvery}
	sinceSave := 0
	for _, it := range due {
		if out.Stopped != "" || ctx.Err() != nil {
			break
		}
		if time.Since(started) >= p.githubRunTime() {
			out.Stopped = "time"
			break
		}
		if out.Listed >= p.githubBudget(auth.token != "") {
			out.Stopped = "budget"
			break
		}
		out.Listed++
		files, result, err := p.listRepo(ctx, client, auth, it.FullName, it.DefaultBranch, &out)
		if err != nil {
			saveGitHub(file, state)
			return out, err
		}
		if ctx.Err() != nil {
			// The run was cancelled in the middle of this repository: that is not a failed listing.
			out.Listed--
			break
		}
		switch result {
		case listLimited:
			out.Stopped = "rate limited"
			out.Listed--
			continue
		case listError:
			if r := state[it.FullName]; r == nil {
				// Never listed: remember the failure, so it does not take the first slot of every run.
				state[it.FullName] = &githubRepo{Branch: it.DefaultBranch, FailedAt: stamp, SeenAt: stamp}
			} else if r.ListedAt == "" {
				r.FailedAt = stamp
			}
			continue
		}
		old := state[it.FullName]
		var previous []string
		if old != nil {
			previous = old.Files
		}
		for _, f := range files {
			if !slices.Contains(previous, f) {
				out.NewFeeds++
			}
		}
		state[it.FullName] = &githubRepo{PushedAt: it.PushedAt, Branch: it.DefaultBranch, ListedAt: stamp, SeenAt: stamp, Files: files}
		if sinceSave++; sinceSave >= githubSaveEvery {
			sinceSave = 0
			if err := saveGitHub(file, state); err != nil {
				return out, err
			}
		}
		if pace.Due() {
			p.log("github: %d/%d listed, %d files checked, %d errors", out.Listed, len(due), out.FilesChecked, out.Errors)
		}
	}
	if err := saveGitHub(file, state); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	out.Feeds = countFeeds(state)
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("github: %d listed, %d files checked, %d feeds (%d new), %d errors, %d removed%s", out.Listed, out.FilesChecked, out.Feeds, out.NewFeeds, out.Errors, out.Removed, errSuffix(out.Stopped))
	return out, nil
}

func countFeeds(state map[string]*githubRepo) int {
	n := 0
	for _, r := range state {
		n += len(r.Files)
	}
	return n
}
