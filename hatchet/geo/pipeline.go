// Package geo builds the ip2geo MMDB (build-geo-data). Each Pipeline method
// is one task; the Hatchet workflow and the local run-geo command call them
// in the same order.
//
// The database is built in layers. A layer is a gzipped CSV file named
// NN-name.csv.gz in the geofeed (RFC 8805) layout, with a header line
// prefix,country,region,city,source (region may be empty; source is free
// text: the feed URL, whois:<registry>, or empty). mmdbbuilder applies every
// file of the layers directory in name order, each replacing what the files
// before it put under its prefixes:
//
//	10-rir        RIR delegated stats: the country every block was delegated to (IPv4, IPv6)
//	20-ipverse    ipverse/geo-ip-blocks: the country in registration records (IPv4 only)
//	30-pdb        PeeringDB: country and city of ASNs with one dominant city (Postgres);
//	              applied only where 10 and 20 gave the same country or none
//	40-hosting    hosting networks whose PeeringDB facilities are all in one city: that
//	              city and country for every announced prefix (Postgres); applied only
//	              where the RIR stats give the same country, so it can only change a
//	              country back to the delegated one; skipped while no network matches
//	50-ix         PeeringDB internet exchange peering LAN prefixes: the exchange's country
//	              and city (Postgres); exchanges without a city are left out, and the
//	              whole layer is skipped while the table has no rows
//	60-whois      whois geoloc rows from sync-geofeeds (private R2): the most populous
//	              place within 25 km of the "geoloc:" point of RIPE, APNIC and AFRINIC
//	              inetnums; a row broader than one RIR block is skipped; skipped while
//	              the object is missing
//	70-geofeeds   geofeed rows from sync-geofeeds (private R2); a row broader than one
//	              RIR block is skipped
//
// The layers directory is emptied at the start of each Download and the build
// passes whatever files are in it: the file name is the order, and layerRules
// is the one place that says which layer gets a special rule.
package geo

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	minIXPrefixes   = 200 // the hosting and whois layers have no minimum: they may legitimately be small

	// An R2 layer older than rowsMaxAge is still used (old rows are better
	// than none), but the run warns and skips the health check ping.
	rowsMaxAge = 3 * 24 * time.Hour
)

// Layer files in DataDir/geo/layers, rebuilt by Download. The name is the
// order. The last two are the files sync-geofeeds uploads to R2, under the
// same names.
const (
	RIRLayer     = "10-rir.csv.gz"
	IPverseLayer = "20-ipverse.csv.gz"
	PDBLayer     = "30-pdb.csv.gz"
	HostingLayer = "40-hosting.csv.gz"
	IXLayer      = "50-ix.csv.gz"
	WhoisLayer   = geofeeds.WhoisRowsFile // 60-whois.csv.gz
	FeedsLayer   = geofeeds.RowsFile      // 70-geofeeds.csv.gz
	NamesFile    = "names.tsv"            // not a layer
)

// layerRules is the one place that gives a layer a special rule: the
// "mmdbbuilder geo" flag that names the layer. A layer that is not here is a
// plain layer: it replaces what the layers before it put under its prefixes.
//
//   - -same-country: the PeeringDB rows only refine. An ASN's city must not
//     replace more specific networks that the registration data puts in another
//     country.
//   - -first-country: a hosting row applies only where the RIR stats (the first
//     layer) give the row's country, so the layer can only change a country back
//     to the delegated one; PeeringDB facility lists can be incomplete.
//   - -within-first: rows no broader than one prefix of the first layer. A broad
//     row cannot change the country of networks that belong to someone else.
//
// The whois layer, like the geofeeds, may change a network's country: it has
// only -within-first, no -first-country, because the inetnum's own country is
// usually right (measured: 4% of geoloc /24s lie in another country than the
// geofeed truth).
var layerRules = map[string]string{
	PDBLayer:     "-same-country",
	HostingLayer: "-first-country",
	WhoisLayer:   "-within-first",
	FeedsLayer:   "-within-first",
}

// optionalLayers may be missing: a run without one is reported in
// Source.SkippedLayers. Every other layer is required: a run without it fails.
// Only this list and r2Layers know about layers, and only for reporting;
// the build takes whatever files the directory holds.
var optionalLayers = []string{HostingLayer, IXLayer, WhoisLayer}

// r2Layers are the layer files that sync-geofeeds uploads; Download fetches
// them as they are. One that is missing or older than rowsMaxAge also skips
// the health check ping.
var r2Layers = []struct{ Name, Key string }{
	{WhoisLayer, geofeeds.WhoisRowsKey},
	{FeedsLayer, geofeeds.RowsKey},
}

// Pipeline runs the geo database build.
type Pipeline struct {
	DataDir     string
	PGSQL       string
	MMDBBuilder string // path to the mmdbbuilder binary
	Progress    func(string)

	R2          string // the private bucket: the R2 layers in, MMDB out
	GitHubToken string
	GitHubRepo  string // owner/name of the repo whose "latest" release gets the MMDB
	HCPing      string // optional healthchecks.io ping URL
	IPinfoToken string // optional; without it Evaluate skips the IPinfo reference. A secret: never log it

	// Tests only.
	stats        []struct{ Name, URL string }
	geoBlocks    string
	refURLs      *evalURLs
	writeIX      func(ctx context.Context, w *csv.Writer) (int, error)
	writeHosting func(ctx context.Context, w *csv.Writer) (int, error)
	heldOutURLs  func(ctx context.Context) (map[string]bool, error)
}

// Source is what Download prepared for the run.
type Source struct {
	DateTag     string            `json:"date_tag"`
	Rows        map[string]int    `json:"rows"` // rows per layer file written from a source here (not the R2 layers)
	StatsStatus map[string]string `json:"stats_status"`
	GeoBlocks   string            `json:"geo_ip_blocks_status"`
	PDBASNs     int               `json:"pdb_asns"`
	// R2Uploaded tells when sync-geofeeds uploaded each R2 layer that was downloaded.
	R2Uploaded map[string]string `json:"r2_layers_uploaded"`
	// StaleLayers are the R2 layers that were missing or older than rowsMaxAge;
	// Publish sends no health check ping then.
	StaleLayers []string `json:"stale_layers,omitempty"`

	// SkippedLayers are the optional layers (see optionalLayers) that are not
	// in the build this run: they had no rows or no object, or skip_layers
	// named them.
	SkippedLayers []string `json:"skipped_layers,omitempty"`
}

func (s Source) skipped(layer string) bool { return slices.Contains(s.SkippedLayers, layer) }

// ValidateSkipLayers accepts only the optional layers.
func ValidateSkipLayers(layers []string) error {
	for _, name := range layers {
		if !slices.Contains(optionalLayers, name) {
			return fmt.Errorf("skip_layers: unknown layer %q (allowed: %s)", name, strings.Join(optionalLayers, ", "))
		}
	}
	return nil
}

// SkipLayers removes the named optional layers (see ValidateSkipLayers) from
// the layers directory and adds them to src.SkippedLayers, for manual test
// runs; the build and the holdout build then leave them out. A layer that is
// already skipped is not added twice.
func (p *Pipeline) SkipLayers(src *Source, layers []string) error {
	for _, name := range layers {
		if err := os.Remove(filepath.Join(p.layersDir(), name)); err != nil && !os.IsNotExist(err) {
			return err
		}
		if !src.skipped(name) {
			src.SkippedLayers = append(src.SkippedLayers, name)
			p.log("download: skipping %s (skip_layers)", name)
		}
	}
	return nil
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

// Download rebuilds the layer files from scratch: RIR stats and geo-ip-blocks
// from the web, the PeeringDB rows and exchange prefixes from Postgres, and
// the whois geoloc and geofeed layers as sync-geofeeds uploaded them to R2.
func (p *Pipeline) Download(ctx context.Context, now time.Time) (Source, error) {
	src := Source{DateTag: now.UTC().Format(dateTagLayout), Rows: map[string]int{}, StatsStatus: map[string]string{},
		R2Uploaded: map[string]string{}}
	layers := p.layersDir()
	downloads := filepath.Join(p.dir(), "downloads")
	// Outputs are rebuilt each run: no layer file of an earlier run may outlive its source.
	if err := os.RemoveAll(layers); err != nil {
		return src, err
	}
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
	rows, err = writeLayer(filepath.Join(layers, IPverseLayer), func(w *csv.Writer) error {
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
	src.Rows[IPverseLayer] = rows
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

	// 4. Hosting networks with all facilities in one city.
	write := p.writeHosting
	if write == nil {
		write = func(ctx context.Context, w *csv.Writer) (int, error) { return writeHostingRows(ctx, p.PGSQL, w) }
	}
	if err := p.downloadOptional(ctx, layers, &src, HostingLayer, 0, "PeeringDB hosting networks",
		"has sync-peeringdb run since the facility columns were added?", write); err != nil {
		return src, err
	}

	// 5. Internet exchange peering LAN prefixes.
	write = p.writeIX
	if write == nil {
		write = func(ctx context.Context, w *csv.Writer) (int, error) { return writeIXRows(ctx, p.PGSQL, w) }
	}
	if err := p.downloadOptional(ctx, layers, &src, IXLayer, minIXPrefixes, "PeeringDB exchange prefixes",
		"has sync-peeringdb run since the exchange layer was added?", write); err != nil {
		return src, err
	}

	// 6. and 7. The layers sync-geofeeds uploaded: whois geoloc and geofeeds.
	r2, err := publish.NewR2(p.R2)
	if err != nil {
		return src, err
	}
	if err := p.downloadR2Layers(ctx, r2.Download, layers, now, &src); err != nil {
		return src, err
	}
	return src, nil
}

// downloadR2Layers fetches the R2 layers into the layers directory as they are
// (no conversion). A missing optional one is skipped with a warning; any other
// error fails. A layer that is missing or older than rowsMaxAge goes into
// src.StaleLayers.
func (p *Pipeline) downloadR2Layers(ctx context.Context, get func(ctx context.Context, key, path string) (time.Time, error),
	layers string, now time.Time, src *Source) error {
	for _, l := range r2Layers {
		uploaded, err := get(ctx, l.Key, filepath.Join(layers, l.Name))
		if errors.Is(err, publish.ErrNotFound) && slices.Contains(optionalLayers, l.Name) {
			src.SkippedLayers = append(src.SkippedLayers, l.Name)
			src.StaleLayers = append(src.StaleLayers, l.Name)
			p.log("download: WARNING %s is not in R2 (has sync-geofeeds uploaded it?); building without it", l.Name)
			continue
		}
		if err != nil {
			return fmt.Errorf("%s (has sync-geofeeds uploaded it?): %w", l.Name, err)
		}
		src.R2Uploaded[l.Name] = uploaded.UTC().Format(time.RFC3339)
		if now.Sub(uploaded) > rowsMaxAge {
			src.StaleLayers = append(src.StaleLayers, l.Name)
			p.log("download: WARNING %s is from %s; is sync-geofeeds failing?", l.Name, src.R2Uploaded[l.Name])
		}
		p.log("download: %s uploaded %s", l.Name, src.R2Uploaded[l.Name])
	}
	return nil
}

// downloadOptional writes an optional layer file. When write returns ErrNoRows
// (its source is missing or empty, the state before sync-peeringdb has filled
// it) the layer is skipped for this run: its name goes into src.SkippedLayers
// and no file is written, so BuildMMDB and Evaluate leave it out. Any other
// error fails, and so does fewer than minRows rows (a minRows of 0 accepts any
// count).
func (p *Pipeline) downloadOptional(ctx context.Context, layers string, src *Source, name string, minRows int,
	label, hint string, write func(context.Context, *csv.Writer) (int, error)) error {
	path := filepath.Join(layers, name)
	rows, err := writeLayer(path, func(w *csv.Writer) error {
		n, err := write(ctx, w)
		if err == nil && n < minRows {
			err = fmt.Errorf("%d prefixes, fewer than %d", n, minRows)
		}
		return err
	})
	if errors.Is(err, ErrNoRows) {
		src.SkippedLayers = append(src.SkippedLayers, name)
		p.log("download: WARNING %s has no rows (%s); building without the %s layer", label, hint, name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	src.Rows[name] = rows
	p.log("download: %s, %d rows", label, rows)
	return nil
}

// writeLayer writes a layer file (gzipped, with the header line
// prefix,country,region,city,source) through a temporary file and returns its
// row count. Every source here writes its layer through it; fill writes the
// rows with layerRow.
func writeLayer(path string, fill func(w *csv.Writer) error) (int, error) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	defer f.Close()
	gz := gzip.NewWriter(f)
	counter := &countingWriter{w: gz}
	w := csv.NewWriter(counter)
	w.Write([]string{"prefix", "country", "region", "city", "source"})
	if err := fill(w); err != nil {
		return 0, err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	return counter.lines - 1, os.Rename(tmp, path)
}

// countingWriter counts the lines written through it.
type countingWriter struct {
	w     io.Writer
	lines int
}

func (c *countingWriter) Write(b []byte) (int, error) {
	c.lines += bytes.Count(b, []byte{'\n'})
	return c.w.Write(b)
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

// layerFiles lists the layer files in the layers directory, sorted by name:
// that is the order they are applied in.
func (p *Pipeline) layerFiles() ([]string, error) {
	entries, err := os.ReadDir(p.layersDir())
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".csv.gz") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// geoArgs are the arguments of "mmdbbuilder geo": every layer file in the layers
// directory in name order (the first must be RIRLayer, which the
// -within-first and -first-country rules judge rows against), each rule of layerRules named for the layers that
// are there (a rule for an absent file is not passed), and, if feeds is not
// empty, that file in place of the FeedsLayer one (the holdout build). BuildMMDB
// and the holdout build both go through here, so they use the same layers and rules.
func (p *Pipeline) geoArgs(out, feeds string) ([]string, error) {
	names, err := p.layerFiles()
	if err != nil {
		return nil, err
	}
	// -within-first and -first-country judge rows against the first layer passed.
	if len(names) == 0 || names[0] != RIRLayer {
		return nil, fmt.Errorf("the layers must start with %s, not %v", RIRLayer, names)
	}
	args := []string{"geo", "-out", out, "-names", filepath.Join(p.layersDir(), NamesFile)}
	// One flag per distinct rule of layerRules, sorted, naming its layers that are there.
	var flags []string
	for _, flag := range layerRules {
		if !slices.Contains(flags, flag) {
			flags = append(flags, flag)
		}
	}
	sort.Strings(flags)
	for _, flag := range flags {
		var named []string
		for _, name := range names {
			if layerRules[name] == flag {
				named = append(named, name)
			}
		}
		if len(named) > 0 {
			args = append(args, flag, strings.Join(named, ","))
		}
	}
	for _, name := range names {
		if name == FeedsLayer && feeds != "" {
			args = append(args, feeds)
		} else {
			args = append(args, filepath.Join(p.layersDir(), name))
		}
	}
	return args, nil
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
	args, err := p.geoArgs(final, "")
	if err != nil {
		return BuildMMDBOutput{}, err
	}
	stdout, err := p.runBuilder(ctx, args)
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
// pings HCPing if set and no R2 layer is missing or stale. A failed evaluation
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
	if len(src.StaleLayers) > 0 {
		// No ping: the missing one raises the alert that sync-geofeeds needs.
		p.log("publish: no health check ping, missing or old in R2: %s", strings.Join(src.StaleLayers, ", "))
	} else if err := publish.Ping(ctx, p.HCPing); err != nil {
		p.log("publish: %v", err) // the uploads succeeded; a missed ping only delays the alert
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	return out, nil
}
