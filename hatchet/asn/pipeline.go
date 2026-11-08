package asn

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"openipdata.org/hatchet/publish"
)

const (
	sourceRepoURL   = "https://github.com/ipverse/asn-ip"
	mmdbFileFormat  = "ip2asn-openipdata-%s.mmdb"
	sqliteFileName  = "asn.sqlite3"
	releaseAsset    = "ip2asn-openipdata.mmdb"
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
}

// mmdbSummary matches mmdbbuilder's "wrote <path> (N ASN entries, M prefixes)".
var mmdbSummary = regexp.MustCompile(`\((\d+) ASN entries, (\d+) prefixes\)`)

// BuildMMDB runs "mmdbbuilder asn" on the synced snapshot.
func (p *Pipeline) BuildMMDB(ctx context.Context, src Source) (BuildMMDBOutput, error) {
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
		return BuildMMDBOutput{}, fmt.Errorf("mmdbbuilder: %w: %s", err, lastLines(stderr.String(), 5))
	}
	p.log("mmdb: %s", strings.TrimSpace(stdout.String()))
	if summary := lastLines(stderr.String(), 1); summary != "" {
		p.log("mmdb: %s", summary)
	}
	out := BuildMMDBOutput{Path: final, SizeBytes: fileSize(final)}
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
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// ExportSQLite writes a fresh asn.sqlite3 from Postgres-backed data, then
// prunes old output directories.
func (p *Pipeline) ExportSQLite(ctx context.Context, src Source) (ExportOutput, error) {
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

	dir := p.outDir(src.DateTag)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return ExportOutput{}, err
	}
	final := filepath.Join(dir, sqliteFileName)
	tmp := final + ".tmp"
	stats, err := ExportSQLite(ctx, tmp, append(parsed.Records, stale...), domainRows, geo)
	if err != nil {
		os.Remove(tmp)
		return ExportOutput{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return ExportOutput{}, err
	}
	p.log("export: wrote %s (asns=%d domains=%d geo_pdb=%d)", final, stats.ASNs, stats.Domains, stats.GeoPDB)
	p.pruneOutputs()
	return ExportOutput{Path: final, SizeBytes: fileSize(final), Stats: stats}, nil
}

// PublishOutput lists where the artifacts went.
type PublishOutput struct {
	Skipped         bool     `json:"skipped,omitempty"`
	R2              []string `json:"r2,omitempty"`
	ReleaseURL      string   `json:"release_url,omitempty"`
	Bytes           int64    `json:"bytes"` // total size of the published files
	DurationSeconds float64  `json:"duration_seconds"`
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

// Publish uploads the MMDB and asn.sqlite3 to R2 under <YYYY-MM-DD>/, replaces
// the MMDB on the rolling GitHub release, then pings HCPing if set. Missing
// settings fail the task, so a misconfiguration cannot go unnoticed.
func (p *Pipeline) Publish(ctx context.Context, src Source, mmdbPath, sqlitePath string) (PublishOutput, error) {
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

	folder := date.Format(dateFolderShape)
	for _, path := range []string{mmdbPath, sqlitePath} {
		location, err := r2.Upload(ctx, folder+"/"+filepath.Base(path), path)
		if err != nil {
			return out, err
		}
		p.log("publish: %s", location)
		out.R2 = append(out.R2, location)
		out.Bytes += fileSize(path)
	}

	body := fmt.Sprintf("Rolling release of the openipdata ip2asn MMDB.\n\nBuilt: %s\nSource: %s@%s",
		date.Format(dateFolderShape), strings.TrimPrefix(sourceRepoURL, "https://github.com/"), src.Commit)
	out.ReleaseURL, err = release.Replace(ctx, mmdbPath, releaseAsset, body)
	if err != nil {
		return out, err
	}
	p.log("publish: %s", out.ReleaseURL)

	if err := publish.Ping(ctx, p.HCPing); err != nil {
		p.log("publish: %v", err) // the uploads succeeded; a missed ping only delays the alert
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	return out, nil
}

// pruneOutputs keeps the newest few dated output directories.
func (p *Pipeline) pruneOutputs() {
	root := filepath.Join(p.DataDir, "out", "asn")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	for len(dirs) > keepOutputDirs {
		os.RemoveAll(filepath.Join(root, dirs[0]))
		dirs = dirs[1:]
	}
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
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
