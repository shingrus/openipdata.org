package geofeeds

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

// The GitHub layer has no first-upload minimum and no drop limit (three feeds carry most of its
// rows and one may move to 70-geofeeds): a small first upload and a 90% drop are sent. A whois
// layer keeps both checks. The upload goes to the layer's own key with its own state file.
func TestUploadGitHubLayer(t *testing.T) {
	now := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	p, _ := exportSetup(t, now)
	var gotKey string
	var gotOpts publish.PutOptions
	put := func(_ context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error) {
		gotKey, gotOpts = key, opts
		return "r2://bucket/" + key, publish.PutUploaded, nil
	}
	idx, err := loadOwners(filepath.Join(p.dir(), BlocksFile))
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(p.dir(), OutDir)
	os.MkdirAll(outDir, 0o750)
	run := func(force bool) GitHubExportOutput {
		return p.exportGitHub(context.Background(), put, force, idx, []string{ghURL4}, nil, outDir)
	}
	got := run(false)
	if !got.Upload.Uploaded || got.Upload.Skipped != "" || gotKey != GitHubRowsKey || !gotOpts.Refresh || gotOpts.Force {
		t.Errorf("small first upload: %+v key %q opts %+v", got, gotKey, gotOpts)
	}
	if got = run(true); !got.Upload.Uploaded || !gotOpts.Force {
		t.Errorf("forced: %+v opts %+v", got, gotOpts)
	}
	if data, err := os.ReadFile(filepath.Join(p.dir(), githubUploadFile)); err != nil || !strings.Contains(string(data), `"rows":1`) {
		t.Errorf("%s = %q %v", githubUploadFile, data, err)
	}
	if _, err := os.Stat(filepath.Join(p.dir(), uploadFile)); err == nil {
		t.Error("the geofeed upload state was written")
	}
	// The last upload had 10 rows, this one 1: a 90% drop is uploaded for this layer.
	os.WriteFile(filepath.Join(p.dir(), githubUploadFile), []byte(`{"date":"2026-11-10T00:00:00Z","rows":10}`), 0o640)
	gotKey = ""
	if got = run(false); !got.Upload.Uploaded || got.Upload.Skipped != "" || got.Upload.PreviousRows != 10 || gotKey != GitHubRowsKey {
		t.Errorf("90%% drop: %+v", got)
	}
	// The same drop is still refused for a whois layer.
	data, _ := os.ReadFile(filepath.Join(outDir, GitHubRowsFile))
	os.WriteFile(filepath.Join(outDir, WhoisTextFile), data, 0o640)
	os.WriteFile(filepath.Join(p.dir(), whoisTextUploadFile), []byte(`{"date":"2026-11-10T00:00:00Z","rows":10}`), 0o640)
	gotKey = ""
	if w := p.uploadLayer(context.Background(), put, false, whoisTextUpload); w.Uploaded || !strings.Contains(w.Skipped, "down from 10") || gotKey != "" {
		t.Errorf("whois layer drop: %+v", w)
	}
	// A damaged state file still refuses (without force).
	os.WriteFile(filepath.Join(p.dir(), githubUploadFile), []byte(`{"rows":`), 0o640)
	if got = run(false); got.Upload.Uploaded || !strings.Contains(got.Upload.Skipped, "damaged") {
		t.Errorf("damaged state: %+v", got)
	}
}

// Rows without a city never go to the GitHub layer (it would erase a lower layer's city); the same
// feed as a main feed keeps both rows.
func TestGitHubLayerDropsRowsWithoutCity(t *testing.T) {
	now := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	p, store := exportSetup(t, now)
	feedsDir := filepath.Join(p.dir(), FeedsDir)
	body := "45.10.203.0/24,DE,,\n45.10.204.0/24,DE,,Bonn\n"
	const gh = "https://raw.githubusercontent.com/gina/geofeed/main/geofeed.csv"
	if _, err := writeFeed(strings.NewReader(body), feedPath(feedsDir, gh), feedHeader{URL: gh}); err != nil {
		t.Fatal(err)
	}
	store.rows[gh] = &urlRow{URL: gh, Sources: []string{"github"}, LastSuccess: now}
	out, err := p.ExportRows(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	layer := readLayer(t, filepath.Join(p.dir(), OutDir, GitHubRowsFile))
	if strings.Contains(layer, "45.10.203.0/24") || !strings.Contains(layer, "45.10.204.0/24,DE,,Bonn,"+gh) || out.GitHub.Dropped[dropNoCity] != 1 {
		t.Errorf("layer:\n%s\ndropped %v", layer, out.GitHub.Dropped)
	}
	if out.Dropped[dropNoCity] != 0 {
		t.Errorf("main dropped = %v", out.Dropped)
	}
	// As a main feed (a second source) both rows stay.
	store.rows[gh].Sources = []string{"github", "list"}
	out, err = p.ExportRows(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	mainRows := readLayer(t, filepath.Join(p.dir(), OutDir, RowsFile))
	if !strings.Contains(mainRows, "45.10.203.0/24,DE,,,"+gh) || !strings.Contains(mainRows, "45.10.204.0/24,DE,,Bonn,"+gh) || out.Dropped[dropNoCity] != 0 {
		t.Errorf("main file:\n%s", mainRows)
	}
}

// The GitHub layer drops rows broader than a /16 (IPv4) or a /32 (IPv6) as too_broad; a main feed keeps them.
func TestGitHubLayerDropsBroadRows(t *testing.T) {
	now := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	p, store := exportSetup(t, now)
	feedsDir := filepath.Join(p.dir(), FeedsDir)
	body := "45.0.0.0/8,DE,,Berlin\n45.10.0.0/16,DE,,Bonn\n2a00::/16,DE,,Hamburg\n2a00:1450::/32,DE,,Essen\n"
	const gh = "https://raw.githubusercontent.com/gina/geofeed/main/geofeed.csv"
	if _, err := writeFeed(strings.NewReader(body), feedPath(feedsDir, gh), feedHeader{URL: gh}); err != nil {
		t.Fatal(err)
	}
	store.rows[gh] = &urlRow{URL: gh, Sources: []string{"github"}, LastSuccess: now}
	out, err := p.ExportRows(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	layer := readLayer(t, filepath.Join(p.dir(), OutDir, GitHubRowsFile))
	if strings.Contains(layer, "45.0.0.0/8") || strings.Contains(layer, "2a00::/16") ||
		!strings.Contains(layer, "45.10.0.0/16,DE,,Bonn") || !strings.Contains(layer, "2a00:1450::/32,DE,,Essen") || out.GitHub.Dropped[rejectTooBroad] != 2 {
		t.Errorf("layer:\n%s\ndropped %v", layer, out.GitHub.Dropped)
	}
	store.rows[gh].Sources = []string{"github", "list"}
	if _, err = p.ExportRows(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if mainRows := readLayer(t, filepath.Join(p.dir(), OutDir, RowsFile)); !strings.Contains(mainRows, "45.0.0.0/8,DE,,Berlin") {
		t.Errorf("main file:\n%s", mainRows)
	}
}
