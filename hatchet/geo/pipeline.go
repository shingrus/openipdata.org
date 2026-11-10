// Package geo builds the ip2geo MMDB (build-geo-data). Each Pipeline method
// is one task; the Hatchet workflow and the local run-geo command call them
// in the same order.
//
// The database is built in layers, each replacing what the layers before it
// put under its prefixes:
//
//  1. RIR delegated stats: the country every block was delegated to (IPv4, IPv6)
//  2. ipverse/geo-ip-blocks: the country in registration records (IPv4 only)
//  3. PeeringDB: country and city of ASNs with one dominant city (Postgres);
//     applied only where layers 1 and 2 gave the same country or none
//  4. geofeed rows from sync-geofeeds (private R2); a row broader than one
//     RIR block is skipped
package geo

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"openipdata.org/hatchet/geofeeds"
	"openipdata.org/hatchet/outputs"
	"openipdata.org/hatchet/publish"
)

const (
	mmdbFileFormat  = "ip2geo-openipdata-%s.mmdb"
	releaseAsset    = "ip2geo-openipdata.mmdb"
	keepOutputDirs  = 3
	dateTagLayout   = "20060102"
	dateFolderShape = "2006-01-02"

	// Minimum sizes; a smaller source fails the run instead of thinning the database.
	minStatsBlocks  = 1000 // per RIR
	minGeoCountries = 200
	minPDBASNs      = 100

	// Geofeed rows older than rowsMaxAge are still used (old rows are better
	// than none), but the run warns and skips the health check ping.
	rowsMaxAge = 3 * 24 * time.Hour
)

// Layer files in DataDir/geo/layers, rebuilt by Download, in the order
// mmdbbuilder applies them.
const (
	RIRLayer    = "rir.csv"
	BlocksLayer = "geo-ip-blocks.csv"
	PDBLayer    = "pdb.csv"
	FeedsLayer  = "geofeed-rows.csv.gz"
	NamesFile   = "names.tsv"
)

var layerOrder = []string{RIRLayer, BlocksLayer, PDBLayer, FeedsLayer}

// Pipeline runs the geo database build.
type Pipeline struct {
	DataDir     string
	PGSQL       string
	MMDBBuilder string // path to the mmdbbuilder binary
	Progress    func(string)

	R2          string // the private bucket: geofeed rows in, MMDB out
	GitHubToken string
	GitHubRepo  string // owner/name of the repo whose "latest" release gets the MMDB
	HCPing      string // optional healthchecks.io ping URL
	IPinfoToken string // optional; without it Evaluate skips the IPinfo reference. A secret: never log it

	// Tests only.
	stats     []struct{ Name, URL string }
	geoBlocks string
	refURLs   *evalURLs
}

// Source is what Download prepared for the run.
type Source struct {
	DateTag       string            `json:"date_tag"`
	Rows          map[string]int    `json:"rows"` // rows per layer file
	StatsStatus   map[string]string `json:"stats_status"`
	GeoBlocks     string            `json:"geo_ip_blocks_status"`
	PDBASNs       int               `json:"pdb_asns"`
	FeedsUploaded string            `json:"geofeed_rows_uploaded"` // when sync-geofeeds uploaded them
	FeedsStale    bool              `json:"geofeed_rows_stale,omitempty"`
}

func (p *Pipeline) dir() string       { return filepath.Join(p.DataDir, "geo") }
func (p *Pipeline) layersDir() string { return filepath.Join(p.dir(), "layers") }
func (p *Pipeline) outDir(dateTag string) string {
	return filepath.Join(p.DataDir, "out", "geo", dateTag)
}

func (p *Pipeline) log(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

// CheckSettings fails when a setting the run needs is missing or invalid, so
// a run stops before the downloads. R2 and PGSQL are always needed; GitHub
// only when publishing.
func (p *Pipeline) CheckSettings(publishing bool) error {
	if p.PGSQL == "" {
		return errors.New("PGSQL is not set")
	}
	if _, err := publish.NewR2(p.R2); err != nil {
		return err
	}
	if publishing {
		_, err := publish.NewRelease(p.GitHubToken, p.GitHubRepo)
		return err
	}
	return nil
}

// Download rebuilds the layer files: RIR stats and geo-ip-blocks from the
// web, the PeeringDB rows from Postgres, the geofeed rows from R2.
func (p *Pipeline) Download(ctx context.Context, now time.Time) (Source, error) {
	src := Source{DateTag: now.UTC().Format(dateTagLayout), Rows: map[string]int{}, StatsStatus: map[string]string{}}
	layers := p.layersDir()
	downloads := filepath.Join(p.dir(), "downloads")
	for _, dir := range []string{layers, downloads} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return src, err
		}
	}
	client := newHTTPClient()

	// 1. RIR stats.
	stats := p.stats
	if stats == nil {
		stats = rirStats
	}
	for _, s := range stats {
		path := filepath.Join(downloads, "delegated-"+s.Name)
		status, err := fetch(ctx, client, s.URL, path, now)
		if err != nil {
			return src, fmt.Errorf("%s stats: %w", s.Name, err)
		}
		src.StatsStatus[s.Name] = status
	}
	rows, err := writeLayer(filepath.Join(layers, RIRLayer), func(w *csv.Writer) error {
		for _, s := range stats {
			blocks, err := writeStatsRows(filepath.Join(downloads, "delegated-"+s.Name), w)
			if err != nil {
				return fmt.Errorf("%s stats: %w", s.Name, err)
			}
			if blocks < minStatsBlocks {
				return fmt.Errorf("%s stats: %d blocks, fewer than %d", s.Name, blocks, minStatsBlocks)
			}
			p.log("download: %s stats %s, %d blocks", s.Name, src.StatsStatus[s.Name], blocks)
		}
		return nil
	})
	if err != nil {
		return src, err
	}
	src.Rows[RIRLayer] = rows

	// 2. geo-ip-blocks.
	blocksURL := p.geoBlocks
	if blocksURL == "" {
		blocksURL = geoIPBlocksURL
	}
	tarball := filepath.Join(downloads, "geo-ip-blocks.tar.gz")
	if src.GeoBlocks, err = fetch(ctx, client, blocksURL, tarball, now); err != nil {
		return src, fmt.Errorf("geo-ip-blocks: %w", err)
	}
	var names map[string]string
	rows, err = writeLayer(filepath.Join(layers, BlocksLayer), func(w *csv.Writer) error {
		var err error
		names, _, err = writeGeoIPBlocksRows(tarball, w)
		if err == nil && len(names) < minGeoCountries {
			err = fmt.Errorf("%d countries, fewer than %d", len(names), minGeoCountries)
		}
		if err != nil {
			return fmt.Errorf("geo-ip-blocks: %w", err)
		}
		return nil
	})
	if err != nil {
		return src, err
	}
	src.Rows[BlocksLayer] = rows
	p.log("download: geo-ip-blocks %s, %d countries, %d IPv4 prefixes", src.GeoBlocks, len(names), rows)
	if err := writeNames(filepath.Join(layers, NamesFile), names); err != nil {
		return src, err
	}

	// 3. PeeringDB rows.
	rows, err = writeLayer(filepath.Join(layers, PDBLayer), func(w *csv.Writer) error {
		asns, _, err := writePDBRows(ctx, p.PGSQL, w)
		if err == nil && asns < minPDBASNs {
			err = fmt.Errorf("%d ASNs, fewer than %d", asns, minPDBASNs)
		}
		if err != nil {
			return fmt.Errorf("PeeringDB rows: %w", err)
		}
		src.PDBASNs = asns
		return nil
	})
	if err != nil {
		return src, err
	}
	src.Rows[PDBLayer] = rows
	p.log("download: PeeringDB rows for %d ASNs, %d prefixes", src.PDBASNs, rows)

	// 4. Geofeed rows.
	r2, err := publish.NewR2(p.R2)
	if err != nil {
		return src, err
	}
	uploaded, err := r2.Download(ctx, geofeeds.RowsKey, filepath.Join(layers, FeedsLayer))
	if err != nil {
		return src, fmt.Errorf("geofeed rows (has sync-geofeeds uploaded them?): %w", err)
	}
	src.FeedsUploaded = uploaded.UTC().Format(time.RFC3339)
	if now.Sub(uploaded) > rowsMaxAge {
		src.FeedsStale = true
		p.log("download: WARNING geofeed rows are from %s; is sync-geofeeds failing?", src.FeedsUploaded)
	}
	p.log("download: geofeed rows uploaded %s", src.FeedsUploaded)
	return src, nil
}

// writeLayer writes a layer file through a temporary file and returns its
// row count.
func writeLayer(path string, fill func(w *csv.Writer) error) (int, error) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	defer f.Close()
	counter := &countingWriter{f: f}
	w := csv.NewWriter(counter)
	w.Write([]string{"prefix", "country", "city"})
	if err := fill(w); err != nil {
		return 0, err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	return counter.lines - 1, os.Rename(tmp, path)
}

// countingWriter counts the lines written through it.
type countingWriter struct {
	f     *os.File
	lines int
}

func (c *countingWriter) Write(b []byte) (int, error) {
	c.lines += bytes.Count(b, []byte{'\n'})
	return c.f.Write(b)
}

func writeNames(path string, names map[string]string) error {
	codes := make([]string, 0, len(names))
	for code := range names {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	var b strings.Builder
	for _, code := range codes {
		fmt.Fprintf(&b, "%s\t%s\n", code, names[code])
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// BuildMMDBOutput describes the built ip2geo MMDB.
type BuildMMDBOutput struct {
	Path      string         `json:"path"`
	Prefixes  map[string]int `json:"prefixes"` // networks written per layer
	Skipped   map[string]int `json:"skipped,omitempty"`
	SizeBytes int64          `json:"size_bytes"`
}

// layerSummary matches mmdbbuilder's "layer <name>: R rows, P prefixes, S skipped".
var layerSummary = regexp.MustCompile(`(?m)^layer (\S+): \d+ rows, (\d+) prefixes, (\d+) skipped$`)

// geoArgs are the arguments of "mmdbbuilder geo": the layer files in order,
// with feeds in place of the geofeed rows layer. The layer file names matter
// to the flags, so feeds must be named FeedsLayer.
func (p *Pipeline) geoArgs(out, feeds string) []string {
	// The PeeringDB rows only refine: an ASN's city must not replace more
	// specific networks that the registration data puts in another country.
	args := []string{"geo", "-out", out, "-names", filepath.Join(p.layersDir(), NamesFile), "-same-country", PDBLayer, "-within-first", FeedsLayer}
	for _, name := range layerOrder {
		if name == FeedsLayer {
			args = append(args, feeds)
		} else {
			args = append(args, filepath.Join(p.layersDir(), name))
		}
	}
	return args
}

// runBuilder runs mmdbbuilder and returns what it printed.
func (p *Pipeline) runBuilder(ctx context.Context, args []string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.MMDBBuilder, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mmdbbuilder %s: %w: %s", args[0], err, outputs.LastLines(stderr.String(), 5))
	}
	return stdout.String(), nil
}

// BuildMMDB runs "mmdbbuilder geo" on the layer files.
func (p *Pipeline) BuildMMDB(ctx context.Context, src Source) (BuildMMDBOutput, error) {
	dir := p.outDir(src.DateTag)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return BuildMMDBOutput{}, err
	}
	// mmdbbuilder writes through a temporary file and renames it into place.
	final := filepath.Join(dir, fmt.Sprintf(mmdbFileFormat, src.DateTag))
	stdout, err := p.runBuilder(ctx, p.geoArgs(final, filepath.Join(p.layersDir(), FeedsLayer)))
	if err != nil {
		return BuildMMDBOutput{}, err
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		p.log("mmdb: %s", line)
	}
	out := BuildMMDBOutput{Path: final, Prefixes: map[string]int{}, Skipped: map[string]int{}, SizeBytes: outputs.FileSize(final)}
	for _, m := range layerSummary.FindAllStringSubmatch(stdout, -1) {
		out.Prefixes[m[1]], _ = strconv.Atoi(m[2])
		if n, _ := strconv.Atoi(m[3]); n > 0 {
			out.Skipped[m[1]] = n
		}
	}
	if len(out.Skipped) == 0 {
		out.Skipped = nil
	}
	outputs.Prune(filepath.Join(p.DataDir, "out", "geo"), keepOutputDirs)
	return out, nil
}

// PublishOutput lists where the MMDB went.
type PublishOutput struct {
	Skipped         bool    `json:"skipped,omitempty"`
	R2              string  `json:"r2,omitempty"`
	ReleaseURL      string  `json:"release_url,omitempty"`
	Bytes           int64   `json:"bytes"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// evalFailure tells why ev cannot be trusted: Evaluate recorded an error, or
// no scores row was stored. It is empty for a usable result.
func evalFailure(ev EvalOutput) string {
	switch {
	case ev.Error != "":
		return ev.Error
	case ev.ID == 0:
		return "no scores were stored"
	}
	return ""
}

// Publish first checks the publish gate against the latest published scores
// (ErrGate; force skips it), then uploads the MMDB to R2 under <YYYY-MM-DD>/,
// replaces it on the rolling GitHub release (leaving the release text, which
// build-asn-data writes), marks ev's row in public.geo_eval as published, and
// pings HCPing if set and the geofeed rows are not stale. A failed evaluation
// (ev.Error, or no stored row) is refused with ErrGate before anything else
// unless force is set; the row is then not marked, as there is none.
func (p *Pipeline) Publish(ctx context.Context, src Source, mmdbPath string, ev EvalOutput, force bool) (PublishOutput, error) {
	started := time.Now()
	out := PublishOutput{}
	if failure := evalFailure(ev); failure != "" {
		if !force {
			return out, fmt.Errorf("%w: evaluation failed: %s; check it, then run with force_publish", ErrGate, failure)
		}
		p.log("publish: evaluation failed (%s), publishing anyway (force_publish)", failure)
	}
	r2, err := publish.NewR2(p.R2)
	if err != nil {
		return out, err
	}
	release, err := publish.NewRelease(p.GitHubToken, p.GitHubRepo)
	if err != nil {
		return out, err
	}
	date, err := time.Parse(dateTagLayout, src.DateTag)
	if err != nil {
		return out, err
	}
	if force {
		p.log("publish: gate skipped (force_publish)")
	} else if err := p.CheckGate(ctx, ev); err != nil {
		return out, err
	}
	if out.R2, err = r2.Upload(ctx, date.Format(dateFolderShape)+"/"+filepath.Base(mmdbPath), mmdbPath); err != nil {
		return out, err
	}
	p.log("publish: %s", out.R2)
	out.Bytes = outputs.FileSize(mmdbPath)
	if out.ReleaseURL, err = release.Replace(ctx, mmdbPath, releaseAsset, ""); err != nil {
		return out, err
	}
	p.log("publish: %s", out.ReleaseURL)
	if ev.ID == 0 {
		p.log("publish: no scores row to mark as published")
	} else if err := p.markPublished(ctx, ev.ID); err != nil {
		return out, err
	}
	if src.FeedsStale {
		// No ping: the missing one raises the alert that sync-geofeeds needs.
		p.log("publish: no health check ping, the geofeed rows are from %s", src.FeedsUploaded)
	} else if err := publish.Ping(ctx, p.HCPing); err != nil {
		p.log("publish: %v", err) // the uploads succeeded; a missed ping only delays the alert
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	return out, nil
}
