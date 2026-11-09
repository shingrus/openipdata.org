// Package geofeeds is the sync-geofeeds workflow: it finds every geofeed URL
// that whois data, IRR data, and public lists point to, and checks each feed.
package geofeeds

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Pipeline runs the geofeed sync. Each method is one task; the Hatchet
// workflow and the local run-geofeeds command call them in the same order.
type Pipeline struct {
	DataDir  string
	Progress func(string)
	R2       string // private bucket for the rows file (see RowsKey)

	// Overridden in tests.
	dumps        []dump
	caidaBase    string
	now          func() time.Time
	allowPrivate bool // let the HTTP clients reach test servers on 127.0.0.1
}

func (p *Pipeline) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *Pipeline) dir() string { return filepath.Join(p.DataDir, "geofeeds") }

func (p *Pipeline) log(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

// Files Discover writes to <data>/geofeeds/, each replaced via a .tmp file.
const (
	BlocksFile = "blocks.csv" // source,prefix,url: one row per block that points to a feed
	URLsFile   = "urls.csv"   // url,sources,blocks: one row per feed URL
)

// SourceStats describes one dump or list in a run.
type SourceStats struct {
	Status      string `json:"status"`            // downloaded, cached, stale, failed
	Objects     int    `json:"objects,omitempty"` // RPSL objects read
	Blocks      int    `json:"blocks,omitempty"`  // prefixes that point to a feed
	URLs        int    `json:"urls"`              // distinct feed URLs
	BadPrefixes int    `json:"bad_prefixes,omitempty"`
	Error       string `json:"error,omitempty"`
}

// DiscoverOutput is the discover task's result.
type DiscoverOutput struct {
	Sources         map[string]SourceStats `json:"sources"`
	URLs            int                    `json:"urls"`
	Blocks          int                    `json:"blocks"`
	DurationSeconds float64                `json:"duration_seconds"`
}

// found collects what points to one URL.
type found struct {
	sources map[string]bool
	blocks  int
}

type collector struct {
	urls   map[string]*found
	blocks *csv.Writer
	nRows  int
}

func (c *collector) add(source, u string, prefix netip.Prefix) {
	f := c.urls[u]
	if f == nil {
		f = &found{sources: map[string]bool{}}
		c.urls[u] = f
	}
	f.sources[source] = true
	if prefix.IsValid() {
		f.blocks++
		c.nRows++
		c.blocks.Write([]string{source, prefix.String(), u})
	}
}

// Discover downloads the dumps and lists and writes BlocksFile and URLsFile.
// A source that fails is skipped (its previous download is used if there is
// one); the run fails only if no whois dump could be read.
func (p *Pipeline) Discover(ctx context.Context) (DiscoverOutput, error) {
	started := time.Now()
	out := DiscoverOutput{Sources: map[string]SourceStats{}}
	dumpDir := filepath.Join(p.dir(), "dumps")
	if err := os.MkdirAll(dumpDir, 0o750); err != nil {
		return out, err
	}
	dumps, base := p.dumps, p.caidaBase
	if dumps == nil {
		dumps = defaultDumps
	}
	if base == "" {
		base = caidaBase
	}

	blocksPath := filepath.Join(p.dir(), BlocksFile)
	blocksFile, err := os.Create(blocksPath + ".tmp")
	if err != nil {
		return out, err
	}
	defer os.Remove(blocksPath + ".tmp")
	defer blocksFile.Close()
	c := &collector{urls: map[string]*found{}, blocks: csv.NewWriter(blocksFile)}
	c.blocks.Write([]string{"source", "prefix", "url"})

	client := newHTTPClient(30*time.Minute, p.allowPrivate)
	var missing []string // whois dumps that could not be read: their space would look unowned
	for i, d := range dumps {
		p.log("discover: [%d/%d] %s", i+1, len(dumps), d.Name)
		stats := p.readDump(ctx, client, c, d, filepath.Join(dumpDir, d.Name+".gz"))
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if stats.Status == statusFailed && d.Source != "irr" {
			missing = append(missing, d.Name)
		}
		p.log("discover: %s: %s, %d objects, %d blocks, %d URLs, %d bad prefixes%s", d.Name, stats.Status,
			stats.Objects, stats.Blocks, stats.URLs, stats.BadPrefixes, errSuffix(stats.Error))
		out.Sources[d.Name] = stats
	}
	if len(missing) > 0 {
		// The previous outputs stay: export-rows must not run with a registry's blocks missing.
		return out, fmt.Errorf("whois dumps could not be read: %s", strings.Join(missing, ", "))
	}

	listed := parseFeedList(listedFeeds)
	for _, u := range listed {
		c.add("list", u, netip.Prefix{})
	}
	out.Sources["list"] = SourceStats{Status: "embedded", URLs: len(listed)}

	listClient := newHTTPClient(2*time.Minute, p.allowPrivate)
	caida, files, err := caidaURLs(ctx, listClient, base, p.clock())
	stats := SourceStats{Status: statusDownloaded, URLs: len(caida)}
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		stats = SourceStats{Status: statusFailed, Error: err.Error()}
	}
	for _, u := range caida {
		c.add("caida", u, netip.Prefix{})
	}
	p.log("discover: caida: %d URLs from %d lists%s", len(caida), files, errSuffix(stats.Error))
	out.Sources["caida"] = stats

	c.blocks.Flush()
	if err := c.blocks.Error(); err != nil {
		return out, err
	}
	if err := blocksFile.Close(); err != nil {
		return out, err
	}
	if err := writeURLs(filepath.Join(p.dir(), URLsFile), c.urls); err != nil {
		return out, err
	}
	if err := os.Rename(blocksPath+".tmp", blocksPath); err != nil {
		return out, err
	}
	out.URLs, out.Blocks = len(c.urls), c.nRows
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("discover: %d URLs, %d blocks in %s", out.URLs, out.Blocks, time.Since(started).Round(time.Second))
	return out, nil
}

// readDump refreshes one dump and adds its feed URLs to c.
func (p *Pipeline) readDump(ctx context.Context, client *http.Client, c *collector, d dump, path string) SourceStats {
	var stats SourceStats
	status, err := fetchFile(ctx, client, d.URL, path)
	if err != nil {
		stats.Error = err.Error()
		if _, statErr := os.Stat(path); statErr != nil {
			stats.Status = statusFailed
			return stats
		}
		status = statusStale
	}
	stats.Status = status

	// Blocks are held until the whole dump is read, so a dump that breaks
	// halfway adds nothing.
	type block struct {
		url    string
		prefix netip.Prefix
	}
	var blocks []block
	seen := map[string]bool{}
	file, err := os.Open(path)
	if err != nil {
		stats.Status, stats.Error = statusFailed, err.Error()
		return stats
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err == nil {
		err = scanObjects(gz, func(obj []attr) {
			stats.Objects++
			urls := feedURLs(obj)
			if len(urls) == 0 {
				return
			}
			prefixes, ok, perr := objectPrefixes(obj)
			if !ok {
				return
			}
			if perr != nil {
				stats.BadPrefixes++
				return
			}
			for _, u := range urls {
				seen[u] = true
				for _, prefix := range prefixes {
					blocks = append(blocks, block{u, prefix})
				}
			}
		})
	}
	if err != nil {
		// A broken file is downloaded again next run.
		os.Remove(path)
		return SourceStats{Status: statusFailed, Error: fmt.Sprintf("read %s: %v", filepath.Base(path), err)}
	}
	for _, b := range blocks {
		c.add(d.Source, b.url, b.prefix)
	}
	stats.Blocks, stats.URLs = len(blocks), len(seen)
	return stats
}

// writeURLs writes one row per URL, sorted, via a .tmp file.
func writeURLs(path string, urls map[string]*found) error {
	var text bytes.Buffer
	w := csv.NewWriter(&text)
	w.Write([]string{"url", "sources", "blocks"})
	keys := make([]string, 0, len(urls))
	for u := range urls {
		keys = append(keys, u)
	}
	sort.Strings(keys)
	for _, u := range keys {
		f := urls[u]
		sources := make([]string, 0, len(f.sources))
		for s := range f.sources {
			sources = append(sources, s)
		}
		slices.Sort(sources)
		w.Write([]string{u, strings.Join(sources, "|"), strconv.Itoa(f.blocks)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return writeFileAtomic(path, text.Bytes())
}

func errSuffix(msg string) string {
	if msg == "" {
		return ""
	}
	return " (" + msg + ")"
}
