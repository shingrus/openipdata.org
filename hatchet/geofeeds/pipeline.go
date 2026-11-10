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

	"openipdata.org/hatchet/download"
)

// Pipeline runs the geofeed sync. Each method is one task; the Hatchet
// workflow and the local run-geofeeds command call them in the same order.
type Pipeline struct {
	DataDir  string
	Progress func(string)
	R2       string // private bucket for the rows file (see RowsKey)
	PGSQL    string // Postgres DSN; probe-domains only reads public.asn_domain and public.asn

	// Overridden in tests.
	dumps        []dump
	rdap         []rdapRegistry
	caidaBase    string
	now          func() time.Time
	allowPrivate bool // let the HTTP clients reach test servers on 127.0.0.1
	// Replace Postgres and the candidate URLs in probe-domains tests.
	domainRows  func(ctx context.Context) ([]asnDomain, error)
	asnPrefixes func(ctx context.Context, asns []int64) (map[int64][]netip.Prefix, error)
	candidates  func(domain string) []string
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

// Discover downloads the dumps and lists, adds the stored RDAP results, and
// writes BlocksFile and URLsFile. A source that fails is skipped (its previous
// download is used if there is one), except a whois dump: without one, its
// registry's space would look unowned, so the run fails.
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

	// Blocks found over RDAP (rdap-lookups), with the registry as source.
	known, err := loadRDAP(filepath.Join(p.dir(), RDAPFile))
	if err != nil {
		return out, err
	}
	rdapStats := map[string]*SourceStats{}
	rdapURLs := map[string]map[string]bool{}
	now := p.clock().UTC()
	for _, r := range known {
		// A result with no recent good lookup is left out: the block may have
		// left the registry, or changed hands.
		if len(r.Feeds) == 0 || !r.trusted(now) {
			continue
		}
		stats := rdapStats[r.Registry]
		if stats == nil {
			stats = &SourceStats{Status: "rdap"}
			rdapStats[r.Registry], rdapURLs[r.Registry] = stats, map[string]bool{}
		}
		for _, prefix := range networkPrefixes(r) {
			for _, u := range r.Feeds {
				c.add(r.Registry, u, prefix)
				stats.Blocks++
				rdapURLs[r.Registry][u] = true
			}
		}
	}
	for name, stats := range rdapStats {
		stats.URLs = len(rdapURLs[name])
		out.Sources["rdap-"+name] = *stats
		p.log("discover: rdap-%s: %d blocks, %d URLs", name, stats.Blocks, stats.URLs)
	}

	// Feeds that probe-domains found on ASN websites. They add no blocks:
	// nothing in a registry says the ASN owns the feed's rows (export-rows
	// holds such a feed to the ASN's announced prefixes instead). The URLs come
	// from DomainPrefixesFile, the snapshot export-rows reads, not from
	// DomainsFile: probe-domains appends to that one while this task runs, and
	// a feed found only there would be exported with no prefixes on record and
	// lose every row as outside_asn. A feed found in this run is listed by the
	// next run, once probe-domains has replaced the sidecar. No file: none yet.
	probed, err := loadDomainPrefixes(filepath.Join(p.dir(), DomainPrefixesFile))
	if err != nil {
		return out, err
	}
	domainURLs := make([]string, 0, len(probed))
	for u := range probed {
		domainURLs = append(domainURLs, u)
	}
	slices.Sort(domainURLs)
	for _, u := range domainURLs {
		c.add(sourceDomain, u, netip.Prefix{})
	}
	out.Sources[sourceDomain] = SourceStats{Status: "probed", URLs: len(domainURLs)}
	p.log("discover: domain: %d URLs", len(domainURLs))

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
	var status string
	var err error
	if strings.HasPrefix(d.URL, "ftp://") {
		status, err = fetchFTP(ctx, d.URL, path, p.allowPrivate)
	} else {
		status, err = download.File(ctx, client, d.URL, path)
	}
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
