package geofeeds

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// The GitHub feeds layer (39-github-geofeeds). A feed that only search-github found (its sources
// are exactly "github") never goes into RowsFile: nothing says the operator owns the rows more
// than the feed on their own site, and the layer is weaker. Its rows go to GitHubRowsFile, which
// build-geo-data applies below the stronger layers. A row with no city is dropped (dropNoCity), and so is one broader than a /16 or a /32 (too_broad). The ownership check is the geofeed rows': a
// row that the most specific whois block covering it gives to another site is dropped, and a row
// a whois block backs goes last.
//
// A feed that is already read under another URL is left out (a duplicate): this matters for the
// holdout of build-geo-data, where a held-out feed must not come back through this layer.
// Whatever goes wrong here is in the output and never fails export-rows or touches RowsFile.

// GitHubExportOutput is the GitHub part of the export-rows result.
type GitHubExportOutput struct {
	Feeds           int               `json:"feeds"` // usable GitHub-only feeds written
	Rows            int               `json:"rows"`  // rows written
	Dropped         map[string]int    `json:"dropped,omitempty"`
	DuplicateFeeds  int               `json:"duplicate_feeds,omitempty"`  // left out: the same repository or the same rows as another feed
	MissingFeeds    int               `json:"missing_feeds,omitempty"`    // with no stored copy
	UnreadableFeeds int               `json:"unreadable_feeds,omitempty"` // stored copies that could not be read; skipped
	Bytes           int64             `json:"bytes"`
	Upload          WhoisUploadOutput `json:"upload"`
}

var githubUpload = layerUpload{GitHubRowsFile, GitHubRowsKey, githubUploadFile, "github geofeed rows", 0, 0}

// githubRepoOf returns the repository (owner/repo, lower case) a raw.githubusercontent.com or
// github.com URL points into.
func githubRepoOf(feedURL string) (string, bool) {
	u, err := url.Parse(feedURL)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(u.Hostname()) {
	case "raw.githubusercontent.com", "github.com":
	default:
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return strings.ToLower(parts[0] + "/" + parts[1]), true
}

// storedRowsHash is the sha256 of the rows of a stored feed copy: everything after its header
// line, which holds the fetch time and validators, not the rows.
func storedRowsHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	r := bufio.NewReader(file)
	if _, err := r.ReadString('\n'); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return string(h.Sum(nil)), nil
}

// exportGitHub writes GitHubRowsFile from the stored copies of the working GitHub-only feeds and,
// if upload is not nil, puts it at GitHubRowsKey. usable is the list of main feeds read for
// RowsFile (their stored copies were just read: their rows hashes and their GitHub repositories are the
// duplicate checks; a working feed with no readable copy writes no row and blocks nothing).
func (p *Pipeline) exportGitHub(ctx context.Context, upload uploadFunc, force bool, idx *owners, urls, usable []string, outDir string) GitHubExportOutput {
	out := GitHubExportOutput{Dropped: map[string]int{}}
	path := filepath.Join(outDir, GitHubRowsFile)
	fail := func(err error) GitHubExportOutput {
		out.Upload.Skipped = "export failed: " + err.Error()
		p.log("export: WARNING github geofeed rows: %s", out.Upload.Skipped)
		return out
	}
	if len(urls) == 0 {
		// No file: an old one must not be uploaded again as if it were current.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		out.Upload.Skipped = "no GitHub-only feeds"
		return out
	}
	feedsDir := filepath.Join(p.dir(), FeedsDir)

	otherRepos := map[string]bool{}
	for _, u := range usable {
		if repo, ok := githubRepoOf(u); ok {
			otherRepos[repo] = true
		}
	}
	// Readable copies, minus the feeds of a repository that another feed already covers.
	type candidate struct {
		url, hash string
		rows      int // the row count in the header of the copy; -1 when it cannot be read
	}
	var candidates []candidate
	for _, u := range urls {
		if ctx.Err() != nil {
			return out
		}
		if repo, ok := githubRepoOf(u); ok && otherRepos[repo] {
			out.DuplicateFeeds++
			continue
		}
		copyPath := feedPath(feedsDir, u)
		err := readStoredFeed(copyPath, func([]string, netip.Prefix) {})
		if errors.Is(err, os.ErrNotExist) {
			out.MissingFeeds++
			continue
		}
		if err != nil {
			p.log("export: skip %s: %v", filepath.Base(copyPath), err)
			out.UnreadableFeeds++
			continue
		}
		hash, err := storedRowsHash(copyPath)
		if err != nil {
			out.UnreadableFeeds++
			continue
		}
		rows := -1
		if h, err := readFeedHeader(copyPath); err == nil {
			rows = h.Rows
		}
		candidates = append(candidates, candidate{u, hash, rows})
	}
	// The same rows as a usable main feed (or an earlier GitHub feed): a duplicate.
	if len(candidates) > 0 {
		want := map[string]bool{}
		counts := map[int]bool{} // row counts of the candidates: only a main copy with one of them can be a duplicate
		hashAll := false
		for _, c := range candidates {
			want[c.hash] = true
			counts[c.rows] = true
			hashAll = hashAll || c.rows < 0
		}
		inMain := map[string]bool{}
		for _, u := range usable {
			if ctx.Err() != nil {
				return out
			}
			mainPath := feedPath(feedsDir, u)
			if !hashAll {
				// The same rows are the same number of rows: a copy with another count is not read again.
				if h, err := readFeedHeader(mainPath); err == nil && !counts[h.Rows] {
					continue
				}
			}
			if hash, err := storedRowsHash(mainPath); err == nil && want[hash] {
				inMain[hash] = true
			}
		}
		seen := map[string]bool{}
		kept := candidates[:0]
		for _, c := range candidates {
			if inMain[c.hash] || seen[c.hash] {
				out.DuplicateFeeds++
				continue
			}
			seen[c.hash] = true
			kept = append(kept, c)
		}
		candidates = kept
	}
	out.Feeds = len(candidates)
	if len(candidates) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		out.Upload.Skipped = "no usable GitHub-only feeds"
		return out
	}

	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return fail(err)
	}
	defer os.Remove(tmp)
	defer file.Close()
	gz := gzip.NewWriter(file)
	w := csv.NewWriter(gz)
	w.Write(layerHeader)
	backedFile, err := os.CreateTemp(outDir, "github-backed-*.csv.tmp")
	if err != nil {
		return fail(err)
	}
	defer os.Remove(backedFile.Name())
	defer backedFile.Close()
	backedW := csv.NewWriter(backedFile)
	var broad int
	for _, c := range candidates {
		if ctx.Err() != nil {
			return out
		}
		rows, err := p.exportFeed(w, backedW, idx, nil, c.url, feedPath(feedsDir, c.url), out.Dropped, &broad, true)
		if err != nil {
			return fail(err)
		}
		out.Rows += rows
	}
	w.Flush()
	backedW.Flush()
	if err := errors.Join(w.Error(), backedW.Error()); err != nil {
		return fail(err)
	}
	if _, err := backedFile.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	if _, err := io.Copy(gz, backedFile); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		return fail(err)
	}
	if out.Rows == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		out.Upload.Skipped = "no rows"
		return out
	}
	if err := os.Rename(tmp, path); err != nil {
		return fail(err)
	}
	if info, err := os.Stat(path); err == nil {
		out.Bytes = info.Size()
	}
	if len(out.Dropped) == 0 {
		out.Dropped = nil
	}
	p.log("export: github: %d rows from %d feeds, dropped %v, %d duplicate feeds, %d missing, %d unreadable, %d bytes",
		out.Rows, out.Feeds, out.Dropped, out.DuplicateFeeds, out.MissingFeeds, out.UnreadableFeeds, out.Bytes)
	out.Upload = p.uploadLayer(ctx, upload, force, githubUpload)
	return out
}
