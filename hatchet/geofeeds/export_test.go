package geofeeds

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testBlocks = "source,prefix,url\n" +
	"ripe,45.10.112.0/22,https://isp.example/feed.csv\n" +
	"ripe,45.10.113.0/24,https://customer.example/feed.csv\n" +
	"ripe,45.10.114.0/24,https://geo.isp.example/v6.csv\n" +
	"irr,45.10.100.0/24,https://announcer.example/feed.csv\n"

func writeTestBlocks(t *testing.T, dir string) string {
	path := filepath.Join(dir, BlocksFile)
	if err := os.WriteFile(path, []byte(testBlocks), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOwnersAllows(t *testing.T) {
	idx, err := loadOwners(writeTestBlocks(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		feed, prefix string
		want         bool
	}{
		{"https://isp.example/feed.csv", "45.10.112.0/24", true},
		{"https://isp.example/feed.csv", "45.10.113.0/25", false},     // a customer block links its own feed
		{"https://isp.example/feed.csv", "45.10.2.0/24", true},        // no block covers it
		{"https://isp.example/feed.csv", "45.10.112.0/21", true},      // broader than every block
		{"https://www.isp.example/other.csv", "45.10.114.0/24", true}, // same site, another feed URL
		{"https://customer.example/feed.csv", "45.10.113.128/25", true},
		{"https://list.example/feed.csv", "45.10.112.0/24", false},
		{"https://announcer.example/feed.csv", "45.10.100.0/24", true}, // IRR gives no ownership
	}
	for _, tt := range tests {
		if got := idx.allows(feedSite(tt.feed), netip.MustParsePrefix(tt.prefix)); got != tt.want {
			t.Errorf("%s %s: allows = %v", tt.feed, tt.prefix, got)
		}
	}
	if site := feedSite("https://api.cloudflare.com/local-ip-ranges.csv"); site != "cloudflare.com" {
		t.Errorf("site = %q", site)
	}
}

// One lookup tells backed rows (the registry links this feed) from the rest.
func TestOwnersClassify(t *testing.T) {
	idx, err := loadOwners(writeTestBlocks(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		feed, prefix    string
		backed, allowed bool
	}{
		{"https://isp.example/feed.csv", "45.10.112.0/24", true, true},
		{"https://isp.example/feed.csv", "45.10.2.0/24", false, true},
		{"https://isp.example/feed.csv", "45.10.113.0/25", false, false},
		{"https://list.example/feed.csv", "45.10.112.0/24", false, false},
	}
	for _, tt := range tests {
		backed, allowed := idx.classify(feedSite(tt.feed), netip.MustParsePrefix(tt.prefix))
		if backed != tt.backed || allowed != tt.allowed {
			t.Errorf("%s %s: backed %v allowed %v", tt.feed, tt.prefix, backed, allowed)
		}
	}
}

func TestExportRows(t *testing.T) {
	now := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	store := newFakeStore()
	p := &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return now }, store: store}
	feedsDir := filepath.Join(p.dir(), FeedsDir)
	os.MkdirAll(feedsDir, 0o750)
	writeTestBlocks(t, p.dir())

	feeds := map[string]string{
		"https://isp.example/feed.csv":  "45.10.112.0/24,DE,DE-BE,Berlin\n45.10.113.0/24,DE,,\n45.10.2.0/24,DE,,\n45.10.112.0/21,DE,,\n",
		"https://list.example/feed.csv": "45.10.100.0/24,US,US-CA,San Jose\n",
	}
	// A feed that last worked 31 days ago is out of the window.
	store.rows["https://old.example/feed.csv"] = &urlRow{URL: "https://old.example/feed.csv", LastSuccess: now.Add(-31 * 24 * time.Hour)}
	for u, body := range feeds {
		if _, err := writeFeed(strings.NewReader(body), feedPath(feedsDir, u), feedHeader{URL: u}); err != nil {
			t.Fatal(err)
		}
		store.rows[u] = &urlRow{URL: u, LastSuccess: now}
	}
	// A damaged stored copy is skipped, not fatal.
	damaged := "https://damaged.example/feed.csv"
	os.WriteFile(feedPath(feedsDir, damaged), []byte("# {}\nnot-a-prefix,DE,,\n"), 0o640)
	store.rows[damaged] = &urlRow{URL: damaged, LastSuccess: now}

	out, err := p.ExportRows(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Feeds != 2 || out.Rows != 4 || out.Dropped[dropOtherOwner] != 1 || out.BroadRows != 1 ||
		out.UnreadableFeeds != 1 || out.Uploaded {
		t.Errorf("out = %+v", out)
	}
	file, err := os.Open(filepath.Join(p.dir(), OutDir, RowsFile))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(gz)
	// The row a whois block backs comes last, so it wins a repeated prefix.
	want := "prefix,country,region,city,feed_url\n" +
		"45.10.2.0/24,DE,,,https://isp.example/feed.csv\n" +
		"45.10.112.0/21,DE,,,https://isp.example/feed.csv\n" +
		"45.10.100.0/24,US,US-CA,San Jose,https://list.example/feed.csv\n" +
		"45.10.112.0/24,DE,DE-BE,Berlin,https://isp.example/feed.csv\n"
	if string(data) != want {
		t.Errorf("rows file:\n%s\nwant:\n%s", data, want)
	}
}

func TestUploadGate(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()}
	os.MkdirAll(p.dir(), 0o750)
	if err := p.uploadRows(context.Background(), nil, "unused", false, &ExportOutput{Rows: 2_000_000, Feeds: minFeeds - 1}); !errors.Is(err, ErrGate) {
		t.Errorf("upload from %d feeds: %v", minFeeds-1, err)
	}
	out := &ExportOutput{Rows: 850, Feeds: minFeeds}
	if err := p.uploadRows(context.Background(), nil, "unused", false, out); !errors.Is(err, ErrGate) {
		t.Errorf("first upload of 850 rows: %v", err)
	}
	statePath := filepath.Join(p.dir(), uploadFile)
	os.WriteFile(statePath, []byte(`{"date":"2026-11-08T05:00:00Z","rows":1000}`), 0o640)
	if err := p.uploadRows(context.Background(), nil, "unused", false, out); !errors.Is(err, ErrGate) || out.PreviousRows != 1000 {
		t.Errorf("drop from 1000 to 850: %v, previous %d", err, out.PreviousRows)
	}
	os.WriteFile(statePath, []byte(`{"rows":`), 0o640)
	if err := p.uploadRows(context.Background(), nil, "unused", false, out); !errors.Is(err, ErrGate) {
		t.Errorf("damaged upload.json: %v", err)
	}
}

func TestFeedSite(t *testing.T) {
	tests := map[string]string{
		"https://api.cloudflare.com/local-ip-ranges.csv":                    "cloudflare.com",
		"https://raw.githubusercontent.com/Netflix/nflx-geofeed/main/g.csv": "raw.githubusercontent.com/netflix",
		"https://raw.githubusercontent.com/hostinger/geofeed/main/g.csv":    "raw.githubusercontent.com/hostinger",
		"https://s3.amazonaws.com/charter-geo-feed/Charter-Geofeed.csv":     "s3.amazonaws.com/charter-geo-feed",
		"https://as51043geofeed.s3.eu-west-2.amazonaws.com/geofeed.csv":     "as51043geofeed.s3.eu-west-2.amazonaws.com",
		"https://s3.eu-west-2.amazonaws.com/bucket/geofeed.csv":             "s3.eu-west-2.amazonaws.com/bucket",
		"https://geofeed.info/geofeed-as26383.csv":                          "geofeed.info/geofeed-as26383.csv",
		"https://opengeofeed.org/feed/as11448.csv?x=1":                      "opengeofeed.org/feed/as11448.csv",
		"http://203.0.113.5/geofeed.csv":                                    "203.0.113.5",
		"https://[2001:db8::1]/geofeed.csv":                                 "2001:db8::1",
	}
	for in, want := range tests {
		if got := feedSite(in); got != want {
			t.Errorf("feedSite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckUpload(t *testing.T) {
	if err := (&Pipeline{}).CheckUpload(); err == nil {
		t.Error("want an error without R2")
	}
	if err := (&Pipeline{R2: "https://key:secret@acc.r2.cloudflarestorage.com/bucket"}).CheckUpload(); err != nil {
		t.Error(err)
	}
}
