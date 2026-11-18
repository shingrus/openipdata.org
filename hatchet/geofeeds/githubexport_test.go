package geofeeds

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubRepoOf(t *testing.T) {
	tests := map[string]string{
		"https://raw.githubusercontent.com/Alice/GeoFeed/main/geofeed.csv": "alice/geofeed",
		"https://github.com/alice/geofeed/blob/main/g.csv":                 "alice/geofeed",
		"https://RAW.githubusercontent.com/alice/geofeed":                  "alice/geofeed",
		"https://raw.githubusercontent.com/alice":                          "",
		"https://raw.githubusercontent.com//x/y":                           "",
		"https://isp.example/alice/geofeed/g.csv":                          "",
		"https://gist.githubusercontent.com/alice/abc/raw/g.csv":           "",
		"::bad": "",
	}
	for in, want := range tests {
		if got, ok := githubRepoOf(in); got != want || ok != (want != "") {
			t.Errorf("githubRepoOf(%q) = %q %v", in, got, ok)
		}
	}
}

func readLayer(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(gz)
	return string(data)
}

const (
	ghURL1 = "https://raw.githubusercontent.com/alice/geofeed/main/geofeed.csv"
	ghURL2 = "https://raw.githubusercontent.com/bob/geofeed/main/geofeed.csv"
	ghURL3 = "https://raw.githubusercontent.com/carol/geofeed/main/copy.csv"
	ghURL4 = "https://raw.githubusercontent.com/dave/geofeed/main/geofeed.csv"
)

// exportSetup stores the main feed and four GitHub-only feeds:
//
//	alice: written (one row dropped: another site's whois block covers it)
//	bob:   written, because the only other feed in bob/geofeed does not work (a dead URL)
//	       (TestExportGitHubSameRepository makes it work: then bob is a duplicate)
//	carol: a duplicate, the same rows as the main feed
//	dave:  written
func exportSetup(t *testing.T, now time.Time) (*Pipeline, *fakeStore) {
	store := newFakeStore()
	p := &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return now }, store: store}
	feedsDir := filepath.Join(p.dir(), FeedsDir)
	os.MkdirAll(feedsDir, 0o750)
	writeTestBlocks(t, p.dir())
	mainRows := "45.10.2.0/24,DE,,\n45.10.112.0/24,DE,DE-BE,Berlin\n"
	feeds := map[string]string{
		"https://isp.example/feed.csv": mainRows,
		ghURL1:                         "45.10.200.0/24,FR,,Paris\n45.10.113.0/25,FR,,Lyon\n",
		ghURL2:                         "45.10.201.0/24,FR,,Nice\n",
		ghURL3:                         mainRows,
		ghURL4:                         "45.10.202.0/24,ES,,Madrid\n",
	}
	for u, body := range feeds {
		if _, err := writeFeed(strings.NewReader(body), feedPath(feedsDir, u), feedHeader{URL: u, Fetched: now.Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
	}
	store.rows["https://isp.example/feed.csv"] = &urlRow{URL: "https://isp.example/feed.csv", Sources: []string{"ripe"}, LastSuccess: now}
	for _, u := range []string{ghURL1, ghURL2, ghURL3, ghURL4} {
		store.rows[u] = &urlRow{URL: u, Sources: []string{"github"}, LastSuccess: now}
	}
	// A feed in bob's repository that another source found, and that does not work at all.
	other := "https://raw.githubusercontent.com/Bob/Geofeed/main/old.csv"
	store.rows[other] = &urlRow{URL: other, Sources: []string{"list"}}
	return p, store
}

func TestExportRowsGitHubLayer(t *testing.T) {
	now := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	p, _ := exportSetup(t, now)
	out, err := p.ExportRows(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	// The main file is what it was without the GitHub feeds.
	if out.Feeds != 1 || out.Rows != 2 || out.Dropped != nil || out.BroadRows != 0 || out.MissingFeeds != 0 {
		t.Errorf("main = %+v", out)
	}
	main := readLayer(t, filepath.Join(p.dir(), OutDir, RowsFile))
	if strings.Contains(main, "githubusercontent") || !strings.Contains(main, "https://isp.example/feed.csv") {
		t.Errorf("main file:\n%s", main)
	}
	gh := out.GitHub
	if gh.Feeds != 3 || gh.Rows != 3 || gh.DuplicateFeeds != 1 || gh.Dropped[dropOtherOwner] != 1 || gh.Bytes == 0 ||
		gh.Upload.Skipped != "" || gh.Upload.Uploaded || gh.Upload.Rows != 3 {
		t.Errorf("github = %+v", gh)
	}
	got := readLayer(t, filepath.Join(p.dir(), OutDir, GitHubRowsFile))
	want := "prefix,country,region,city,source\n" +
		"45.10.200.0/24,FR,,Paris," + ghURL1 + "\n" +
		"45.10.201.0/24,FR,,Nice," + ghURL2 + "\n" +
		"45.10.202.0/24,ES,,Madrid," + ghURL4 + "\n"
	if got != want {
		t.Errorf("github file:\n%s\nwant:\n%s", got, want)
	}
	if GitHubRowsKey != "internal/layers/39-github-geofeeds.csv.gz" {
		t.Errorf("key = %s", GitHubRowsKey)
	}
}

// With no GitHub-only feed no file is written (an old one is removed) and the upload is skipped.
func TestExportRowsNoGitHubFeeds(t *testing.T) {
	now := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	p, store := exportSetup(t, now)
	for _, u := range []string{ghURL1, ghURL2, ghURL3, ghURL4} {
		delete(store.rows, u)
	}
	stale := filepath.Join(p.dir(), OutDir, GitHubRowsFile)
	os.MkdirAll(filepath.Dir(stale), 0o750)
	os.WriteFile(stale, []byte("old"), 0o640)
	out, err := p.ExportRows(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("an old layer file was left")
	}
	if out.GitHub.Upload.Skipped == "" || out.GitHub.Upload.Uploaded || out.GitHub.Feeds != 0 || out.GitHub.Rows != 0 {
		t.Errorf("github = %+v", out.GitHub)
	}
	if out.Feeds != 1 || out.Rows != 2 {
		t.Errorf("main = %+v", out)
	}
	// A feed that exists but is not a GitHub-only one (a second source) goes to the main file.
	store.rows[ghURL4] = &urlRow{URL: ghURL4, Sources: []string{"github", "list"}, LastSuccess: now}
	feedsDir := filepath.Join(p.dir(), FeedsDir)
	writeFeed(strings.NewReader("45.10.202.0/24,ES,,Madrid\n"), feedPath(feedsDir, ghURL4), feedHeader{URL: ghURL4})
	out, err = p.ExportRows(context.Background(), false, false)
	if err != nil || out.Feeds != 2 || out.GitHub.Feeds != 0 {
		t.Errorf("two sources: %+v %v", out, err)
	}
}

// Feeds with no stored copy, or a damaged one, are counted and skipped; the layer is not written
// when no usable feed is left.
func TestExportGitHubMissingAndUnreadable(t *testing.T) {
	now := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	p, store := exportSetup(t, now)
	feedsDir := filepath.Join(p.dir(), FeedsDir)
	os.Remove(feedPath(feedsDir, ghURL1))
	os.WriteFile(feedPath(feedsDir, ghURL4), []byte("# {}\nnot-a-prefix,DE,,\n"), 0o640)
	delete(store.rows, ghURL2) // its repository has a dead feed only; keep this test about copies
	out, err := p.ExportRows(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	gh := out.GitHub
	if gh.MissingFeeds != 1 || gh.UnreadableFeeds != 1 || gh.Feeds != 0 || gh.Upload.Skipped == "" {
		t.Errorf("github = %+v", gh)
	}
	if _, err := os.Stat(filepath.Join(p.dir(), OutDir, GitHubRowsFile)); err == nil {
		t.Error("a layer file with no feeds")
	}
}

// The duplicate rules in isolation: the repository is compared case-insensitively, and the same
// rows under two GitHub URLs are kept once.
func TestExportGitHubDuplicates(t *testing.T) {
	now := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	p, _ := exportSetup(t, now)
	feedsDir := filepath.Join(p.dir(), FeedsDir)
	idx, _ := loadOwners(filepath.Join(p.dir(), BlocksFile))
	outDir := filepath.Join(p.dir(), OutDir)
	os.MkdirAll(outDir, 0o750)
	twin := "https://raw.githubusercontent.com/frank/geofeed/main/geofeed.csv"
	writeFeed(strings.NewReader("45.10.202.0/24,ES,,Madrid\n"), feedPath(feedsDir, twin), feedHeader{URL: twin, Fetched: "2020-01-01T00:00:00Z"})
	// ghURL4 and twin have the same rows (their headers differ): one is written.
	got := p.exportGitHub(context.Background(), nil, false, idx, []string{ghURL4, twin}, nil, outDir)
	if got.Feeds != 1 || got.DuplicateFeeds != 1 || got.Rows != 1 {
		t.Errorf("twins: %+v", got)
	}
	// Same repository as a feed read under another URL: left out.
	got = p.exportGitHub(context.Background(), nil, false, idx, []string{ghURL4}, []string{"https://raw.githubusercontent.com/Dave/Geofeed/main/x.csv"}, outDir)
	if got.Feeds != 0 || got.DuplicateFeeds != 1 {
		t.Errorf("repository: %+v", got)
	}
}

// Only a WORKING feed that is not GitHub-only makes a GitHub feed of the same repository a
// duplicate: a dead whois-linked URL (a renamed branch) reads nothing.
func TestExportGitHubSameRepository(t *testing.T) {
	now := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	p, store := exportSetup(t, now)
	other := "https://raw.githubusercontent.com/Bob/Geofeed/main/old.csv"
	out, err := p.ExportRows(context.Background(), false, false)
	if err != nil || out.GitHub.Feeds != 3 || out.GitHub.DuplicateFeeds != 1 {
		t.Fatalf("dead URL: %+v %v", out.GitHub, err)
	}
	store.rows[other].LastSuccess = now
	feedsDir := filepath.Join(p.dir(), FeedsDir)
	writeFeed(strings.NewReader("45.10.230.0/24,GB,,London\n"), feedPath(feedsDir, other), feedHeader{URL: other})
	out, err = p.ExportRows(context.Background(), false, false)
	if err != nil || out.GitHub.Feeds != 2 || out.GitHub.DuplicateFeeds != 2 {
		t.Errorf("working URL: %+v %v", out.GitHub, err)
	}
	// A working feed with no stored copy writes no row and blocks nothing.
	os.Remove(feedPath(feedsDir, other))
	out, err = p.ExportRows(context.Background(), false, false)
	if err != nil || out.GitHub.Feeds != 3 || out.GitHub.DuplicateFeeds != 1 {
		t.Errorf("working URL with no copy: %+v %v", out.GitHub, err)
	}
}
