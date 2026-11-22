package asn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"openipdata.org/hatchet/outputs"
	"openipdata.org/hatchet/publish"
	"openipdata.org/hatchet/schema"
)

const (
	sourceRepoURL   = "https://github.com/ipverse/asn-ip"
	mmdbFileFormat  = "ip2asn-openipdata-%s.mmdb" // the internal flat build output, never published
	sqliteFileName  = "asn.sqlite3"
	releaseAsset    = "openipdata-org-ip2asn.mmdb" // the published file (GeoLite2-ASN layout)
	releaseSQLite   = "asn-openipdata.sqlite3"
	publishedFmt    = "openipdata-org-ip2asn-%s.mmdb"
	keepOutputDirs  = 3
	dateTagLayout   = "20060102"
	dateFolderShape = "2006-01-02"
)

// Pipeline runs the ASN database build. Each method is one task; the Hatchet
// workflow and the local run-asn command call them in the same order.
type Pipeline struct {
	DataDir     string
	PGSQL       string
	MMDBBuilder string // path to the mmdbbuilder binary
	Progress    func(string)

	// Publishing (see Publish).
	R2          string // https://<key-id>:<secret>@<account-id>.r2.cloudflarestorage.com/<bucket>
	GitHubToken string
	GitHubRepo  string // owner/name of the repo whose "latest" release gets the MMDB
	HCPing      string // optional healthchecks.io ping URL
}

// Source identifies the asn-ip snapshot a run works on.
type Source struct {
	Commit  string `json:"commit"`
	DateTag string `json:"date_tag"`
	// ScoresOnly marks the light run (BuildASNInput.ScoresOnly): DateTag is the date tag of the
	// newest local build, and only the geo_scores table of its asn.sqlite3 is refreshed.
	ScoresOnly bool `json:"scores_only,omitempty"`
	// Skipped says why a scores-only run has nothing to work on; every later task is skipped.
	Skipped string `json:"skipped,omitempty"`
}

func (p *Pipeline) repoDir() string { return filepath.Join(p.DataDir, "repos", "asn-ip") }
func (p *Pipeline) asDir() string   { return filepath.Join(p.repoDir(), "as") }

func (p *Pipeline) outDir(dateTag string) string {
	return filepath.Join(p.DataDir, "out", "asn", dateTag)
}

func (p *Pipeline) log(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

// SyncSource clones ipverse/asn-ip once and afterwards updates it in place.
func (p *Pipeline) SyncSource(ctx context.Context, now time.Time) (Source, error) {
	repo := p.repoDir()
	if _, err := os.Stat(filepath.Join(repo, ".git")); err == nil {
		p.log("sync: updating %s", repo)
		if err := runGit(ctx, "-C", repo, "fetch", "-q", "--depth", "1", "origin", "HEAD"); err != nil {
			return Source{}, err
		}
		if err := runGit(ctx, "-C", repo, "reset", "-q", "--hard", "FETCH_HEAD"); err != nil {
			return Source{}, err
		}
		if err := runGit(ctx, "-C", repo, "clean", "-fdq"); err != nil {
			return Source{}, err
		}
	} else {
		p.log("sync: cloning %s into %s", sourceRepoURL, repo)
		if err := os.RemoveAll(repo); err != nil {
			return Source{}, err
		}
		if err := os.MkdirAll(filepath.Dir(repo), 0o750); err != nil {
			return Source{}, err
		}
		if err := runGit(ctx, "clone", "-q", "--depth", "1", "--single-branch", sourceRepoURL, repo); err != nil {
			return Source{}, err
		}
	}
	commit, err := p.headCommit(ctx)
	if err != nil {
		return Source{}, err
	}
	src := Source{Commit: commit, DateTag: now.UTC().Format(dateTagLayout)}
	p.log("sync: asn-ip at %s", commit)
	return src, nil
}

// checkSource fails when the checkout moved since SyncSource, so every task of
// a run reads the same snapshot.
func (p *Pipeline) checkSource(ctx context.Context, src Source) error {
	commit, err := p.headCommit(ctx)
	if err != nil {
		return err
	}
	if commit != src.Commit {
		return fmt.Errorf("asn-ip checkout moved from %s to %s during the run", src.Commit, commit)
	}
	return nil
}

func (p *Pipeline) headCommit(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", p.repoDir(), "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *Pipeline) parse(ctx context.Context, src Source) (ParseResult, error) {
	if err := p.checkSource(ctx, src); err != nil {
		return ParseResult{}, err
	}
	warnings := 0
	result, err := ParseDir(p.asDir(), func(msg string) {
		warnings++
		if warnings <= 20 {
			p.log("%s", msg)
		}
	})
	if warnings > 20 {
		p.log("parse: %d more warnings not shown", warnings-20)
	}
	return result, err
}

func (p *Pipeline) openStore(ctx context.Context) (*Store, error) {
	if p.PGSQL == "" {
		return nil, fmt.Errorf("PGSQL is not set")
	}
	store, err := OpenStore(ctx, p.PGSQL)
	if err != nil {
		return nil, err
	}
	if err := store.EnsureSchema(ctx); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

// Ingest writes changed ASN rows to Postgres.
func (p *Pipeline) Ingest(ctx context.Context, src Source) (IngestStats, error) {
	if reason := src.skipReason(); reason != "" {
		return IngestStats{Skipped: reason}, nil
	}
	parsed, err := p.parse(ctx, src)
	if err != nil {
		return IngestStats{}, err
	}
	p.log("ingest: parsed %d ASNs (%d skipped)", len(parsed.Records), parsed.Skipped)
	store, err := p.openStore(ctx)
	if err != nil {
		return IngestStats{}, err
	}
	defer store.Close()
	stats, err := store.Ingest(ctx, parsed.Records)
	if err != nil {
		return stats, err
	}
	p.log("ingest: inserted=%d updated=%d unchanged=%d stale=%d",
		stats.Inserted, stats.Updated, stats.Unchanged, stats.Stale)
	return stats, nil
}

// BuildMMDBOutput describes the built ip2asn MMDB.
type BuildMMDBOutput struct {
	Path      string `json:"path"`
	Entries   int    `json:"entries"`
	Prefixes  int    `json:"prefixes"`
	SizeBytes int64  `json:"size_bytes"`
	Skipped   string `json:"skipped,omitempty"`
}

// mmdbSummary matches mmdbbuilder's "wrote <path> (N ASN entries, M prefixes)".
var mmdbSummary = regexp.MustCompile(`\((\d+) ASN entries, (\d+) prefixes\)`)

// BuildMMDB runs "mmdbbuilder asn" on the synced snapshot.
func (p *Pipeline) BuildMMDB(ctx context.Context, src Source) (BuildMMDBOutput, error) {
	if reason := src.skipReason(); reason != "" {
		return BuildMMDBOutput{Skipped: reason}, nil
	}
	if err := p.checkSource(ctx, src); err != nil {
		return BuildMMDBOutput{}, err
	}
	dir := p.outDir(src.DateTag)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return BuildMMDBOutput{}, err
	}
	// mmdbbuilder writes through a temporary file and renames it into place.
	final := filepath.Join(dir, fmt.Sprintf(mmdbFileFormat, src.DateTag))

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.MMDBBuilder, "asn", "-in", p.asDir(), "-out", final)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return BuildMMDBOutput{}, fmt.Errorf("mmdbbuilder: %w: %s", err, outputs.LastLines(stderr.String(), 5))
	}
	p.log("mmdb: %s", strings.TrimSpace(stdout.String()))
	if summary := outputs.LastLines(stderr.String(), 1); summary != "" {
		p.log("mmdb: %s", summary)
	}
	out := BuildMMDBOutput{Path: final, SizeBytes: outputs.FileSize(final)}
	if m := mmdbSummary.FindStringSubmatch(stdout.String()); m != nil {
		out.Entries, _ = strconv.Atoi(m[1])
		out.Prefixes, _ = strconv.Atoi(m[2])
	}
	return out, nil
}

// ExportOutput describes the exported SQLite file.
type ExportOutput struct {
	Path      string      `json:"path"`
	SizeBytes int64       `json:"size_bytes"`
	Stats     ExportStats `json:"stats"`
	// ScoresOnly is true when only the geo_scores table was rewritten (the light run).
	ScoresOnly bool `json:"scores_only,omitempty"`
	// Skipped says why the file was left as it was (path is then empty).
	Skipped string `json:"skipped,omitempty"`
}

// ExportSQLite writes a fresh asn.sqlite3 from Postgres-backed data, then
// prunes old output directories.
func (p *Pipeline) ExportSQLite(ctx context.Context, src Source) (ExportOutput, error) {
	if src.ScoresOnly || src.Skipped != "" {
		return p.exportScoresOnly(ctx, src)
	}
	parsed, err := p.parse(ctx, src)
	if err != nil {
		return ExportOutput{}, err
	}
	store, err := p.openStore(ctx)
	if err != nil {
		return ExportOutput{}, err
	}
	defer store.Close()
	stale, err := store.StaleRecords(ctx, parsed.Records)
	if err != nil {
		return ExportOutput{}, err
	}
	domainRows, err := store.Domains(ctx)
	if err != nil {
		return ExportOutput{}, err
	}
	geo, err := store.GeoPDB(ctx)
	if err != nil {
		return ExportOutput{}, err
	}

	// The score history is an extra for the website's status page: build-asn-data does
	// not own public.geo_eval, so a problem reading it only leaves the table empty.
	scores, err := store.GeoScores(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ExportOutput{}, ctx.Err()
		}
		p.log("export: geo_scores left empty, public.geo_eval could not be read: %v", schema.HideDSN(err))
		scores = nil
	}

	dir := p.outDir(src.DateTag)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return ExportOutput{}, err
	}
	// Both daily runs share the dir of their day: this run's file is not published yet, so
	// the light run must not use the dir until this run's publish marks it again.
	if err := os.Remove(filepath.Join(dir, publishedMark)); err != nil && !os.IsNotExist(err) {
		return ExportOutput{}, err
	}
	final := filepath.Join(dir, sqliteFileName)
	tmp := final + ".tmp"
	stats, err := ExportSQLite(ctx, tmp, append(parsed.Records, stale...), domainRows, geo, scores)
	if err != nil {
		os.Remove(tmp)
		return ExportOutput{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return ExportOutput{}, err
	}
	p.log("export: wrote %s (asns=%d domains=%d geo_pdb=%d geo_scores=%d)", final, stats.ASNs, stats.Domains, stats.GeoPDB, stats.GeoScores)
	outputs.Prune(filepath.Join(p.DataDir, "out", "asn"), keepOutputDirs)
	return ExportOutput{Path: final, SizeBytes: outputs.FileSize(final), Stats: stats}, nil
}

// PublishOutput lists where the artifacts went: the published ASN MMDB (GeoLite2-ASN layout, made by
// "mmdbbuilder convert" from the flat build output) and the SQLite file.
type PublishOutput struct {
	Skipped         bool     `json:"skipped,omitempty"`
	R2              []string `json:"r2,omitempty"` // where the files are
	ReleaseURL      string   `json:"release_url,omitempty"`
	Bytes           int64    `json:"bytes"` // total size of the published files
	DurationSeconds float64  `json:"duration_seconds"`
	// A file that is byte-identical to the one already in place is not sent again:
	// R2 compares the object's sha256 metadata, the release the asset's digest.
	R2Unchanged      []string `json:"r2_unchanged,omitempty"`      // the locations of R2 that were left as they were
	ReleaseUnchanged bool     `json:"release_unchanged,omitempty"` // the release MMDB asset was left as it was
	// The SQLite file on the same release, reported like the MMDB.
	ReleaseSQLiteURL       string `json:"release_sqlite_url,omitempty"`
	ReleaseSQLiteUnchanged bool   `json:"release_sqlite_unchanged,omitempty"`
	// ScoresOnly is true for the light run: only the SQLite file went out, no MMDB, no ping.
	ScoresOnly bool `json:"scores_only,omitempty"`
}

// r2Putter is what publishFiles needs from R2 (tests fake it).
type r2Putter interface {
	Put(ctx context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error)
}

// releaseReplacer is what Publish needs from the GitHub release (tests fake it).
type releaseReplacer interface {
	Replace(ctx context.Context, path, assetName, body string) (link string, replaced bool, err error)
}

// publishers builds the R2 and GitHub clients from the publishing settings.
func (p *Pipeline) publishers() (*publish.R2, *publish.Release, error) {
	r2, err := publish.NewR2(p.R2)
	if err != nil {
		return nil, nil, err
	}
	release, err := publish.NewRelease(p.GitHubToken, p.GitHubRepo)
	if err != nil {
		return nil, nil, err
	}
	return r2, release, nil
}

// CheckPublish fails when a publishing setting is missing or invalid, so a run
// can stop before the build instead of after it.
func (p *Pipeline) CheckPublish() error {
	_, _, err := p.publishers()
	return err
}

// Publish converts the flat MMDB build output at mmdbPath into the published
// GeoLite2-ASN layout file (openipdata-org-ip2asn-<date>.mmdb, next to it; a failed
// conversion fails Publish before anything is uploaded), uploads that file and
// asn.sqlite3 to R2 under <YYYY-MM-DD>/, replaces openipdata-org-ip2asn.mmdb and the
// SQLite file (asn-openipdata.sqlite3) on the rolling GitHub release, then pings
// HCPing if set. The flat file is never uploaded. A file that is
// byte-identical to the one already in place is not sent again (an identical file is
// the published file). Missing settings fail the task, so a misconfiguration
// cannot go unnoticed.
func (p *Pipeline) Publish(ctx context.Context, src Source, mmdbPath, sqlitePath string) (PublishOutput, error) {
	if src.ScoresOnly || src.Skipped != "" {
		return p.publishScoresOnly(ctx, src, sqlitePath)
	}
	started := time.Now()
	out := PublishOutput{}
	r2, release, err := p.publishers()
	if err != nil {
		return out, err
	}
	date, err := time.Parse(dateTagLayout, src.DateTag)
	if err != nil {
		return out, err
	}
	if err := p.publishFiles(ctx, r2, release, src, date, mmdbPath, sqlitePath, &out); err != nil {
		return out, err
	}
	// The light run (scores_only) only works on a build that got this far.
	if err := os.WriteFile(filepath.Join(p.outDir(src.DateTag), publishedMark), nil, 0o640); err != nil {
		p.log("publish: could not mark the build as published, the next scores-only run skips it: %v", err)
	}
	if err := publish.Ping(ctx, p.HCPing); err != nil {
		p.log("publish: %v", err) // the uploads succeeded; a missed ping only delays the alert
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	return out, nil
}

// errConvert is the fixed error of a failed conversion; the builder's own last
// stderr lines follow it.
var errConvert = errors.New("convert to the published layout failed")

// publishFiles converts, then puts the published MMDB and the SQLite file in R2 and
// replaces both on the release.
func (p *Pipeline) publishFiles(ctx context.Context, r2 r2Putter, release releaseReplacer, src Source, date time.Time, mmdbPath, sqlitePath string, out *PublishOutput) error {
	pubPath, err := p.convertPublished(ctx, date, mmdbPath)
	if err != nil {
		return err
	}
	if err := p.putR2(ctx, r2, date, out, pubPath, sqlitePath); err != nil {
		return err
	}
	body := fmt.Sprintf("Rolling release of the openipdata ip2asn MMDB and the asn SQLite database.\n\nBuilt: %s\nSource: %s@%s",
		date.Format(dateFolderShape), strings.TrimPrefix(sourceRepoURL, "https://github.com/"), src.Commit)
	return p.publishRelease(ctx, release, pubPath, sqlitePath, body, out)
}

// putR2 puts the files in R2 under <YYYY-MM-DD>/ and records them in out.
func (p *Pipeline) putR2(ctx context.Context, r2 r2Putter, date time.Time, out *PublishOutput, paths ...string) error {
	folder := date.Format(dateFolderShape)
	for _, path := range paths {
		location, status, err := r2.Put(ctx, folder+"/"+filepath.Base(path), path, publish.PutOptions{})
		if err != nil {
			return err
		}
		p.log("publish: %s (%s)", location, status)
		out.R2 = append(out.R2, location)
		if status == publish.PutUnchanged {
			out.R2Unchanged = append(out.R2Unchanged, location)
		}
		out.Bytes += outputs.FileSize(path)
	}
	return nil
}

// convertPublished runs "mmdbbuilder convert" and returns the published file's path.
func (p *Pipeline) convertPublished(ctx context.Context, date time.Time, mmdbPath string) (string, error) {
	path := filepath.Join(filepath.Dir(mmdbPath), fmt.Sprintf(publishedFmt, date.Format(dateTagLayout)))
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.MMDBBuilder, "convert", "-db", mmdbPath, "-out", path)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		return "", fmt.Errorf("%w: mmdbbuilder convert: %w: %s", errConvert, err, outputs.LastLines(stderr.String(), 5))
	}
	p.log("publish: %s", strings.TrimSpace(stdout.String()))
	return path, nil
}

// publishRelease replaces the published MMDB, then the SQLite file, on the rolling release. If
// the MMDB is replaced and the SQLite upload then fails, the task fails (the error
// is returned, out keeps what was done, and Publish sends no health-check ping), so
// the half-updated release is seen; nothing is rolled back, and the next run
// repeats both (a file already in place is skipped by its digest). The release text
// is set with the MMDB; the SQLite call passes no body, which keeps it.
func (p *Pipeline) publishRelease(ctx context.Context, release releaseReplacer, mmdbPath, sqlitePath, body string, out *PublishOutput) error {
	var replaced bool
	var err error
	out.ReleaseURL, replaced, err = release.Replace(ctx, mmdbPath, releaseAsset, body)
	if err != nil {
		return err
	}
	out.ReleaseUnchanged = !replaced
	p.log("publish: %s (%s)", out.ReleaseURL, uploadedOrUnchanged(replaced))

	out.ReleaseSQLiteURL, replaced, err = release.Replace(ctx, sqlitePath, releaseSQLite, "")
	if err != nil {
		return fmt.Errorf("release SQLite asset (the MMDB asset is already updated): %w", err)
	}
	out.ReleaseSQLiteUnchanged = !replaced
	p.log("publish: %s (%s)", out.ReleaseSQLiteURL, uploadedOrUnchanged(replaced))
	return nil
}

func uploadedOrUnchanged(replaced bool) string {
	if replaced {
		return "uploaded"
	}
	return "unchanged"
}

func runGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
