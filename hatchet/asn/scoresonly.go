package asn

// The light run of build-asn-data (BuildASNInput.ScoresOnly), started by the publish task
// of build-geo-data after each published geo build. It is the one exception to "output
// files are rebuilt from scratch": it rewrites ONE table (geo_scores) of the asn.sqlite3 of
// the last full build, so the website's score chart has the newest geo build soon instead
// of at the next ASN cron run. It reads only public.geo_eval, writes nothing to Postgres,
// runs no builder, and publishes only the SQLite file (no MMDB, no health-check ping: the
// ping means a full build succeeded).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"openipdata.org/hatchet/outputs"
	"openipdata.org/hatchet/schema"
)

// publishedMark is the empty file a full run's publish leaves in its output dir once its
// files are on the release. The light run only works on such a dir: a full run that wrote
// its SQLite but failed later (build-mmdb, publish) must not reach the release through it.
const publishedMark = "published"

const (
	skipNoSQLite    = "no local SQLite"
	skipUnpublished = "newest build not published"
	skipScoresOnly  = "scores_only"
	skipScoresRead  = "geo_scores could not be read"
)

// skipReason is why the full-build tasks (ingest, build-mmdb) do nothing: a light run
// builds nothing, and a light run with no local file has nothing to work on.
func (s Source) skipReason() string {
	if s.Skipped != "" {
		return s.Skipped
	}
	if s.ScoresOnly {
		return skipScoresOnly
	}
	return ""
}

// newestSQLiteDir returns the date tag of the newest output dir under root (dir names are
// date tags) that holds a complete, non-empty regular asn.sqlite3, and whether a full run
// published that file (publishedMark). It never falls back to an older dir: when the newest
// file is not published (a later full run failed), the release may hold a newer file than
// any older dir, and the light run must not replace it with an older one. "" when none.
func newestSQLiteDir(root string) (string, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	var tags []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := time.Parse(dateTagLayout, e.Name()); err != nil {
			continue
		}
		tags = append(tags, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(tags)))
	for _, tag := range tags {
		info, err := os.Lstat(filepath.Join(root, tag, sqliteFileName))
		if err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			mark, err := os.Lstat(filepath.Join(root, tag, publishedMark))
			return tag, err == nil && mark.Mode().IsRegular()
		}
	}
	return "", false
}

// SyncScoresOnly is sync-source of the light run: no git fetch. It finds the newest local
// build with an asn.sqlite3. With none, the Source says so (Skipped) and every later task
// is skipped; the run still succeeds. The commit of that build is not recorded anywhere,
// so Commit stays empty.
func (p *Pipeline) SyncScoresOnly() Source {
	tag, published := newestSQLiteDir(filepath.Join(p.DataDir, "out", "asn"))
	if tag == "" {
		p.log("sync: scores only, no local %s, nothing to do", sqliteFileName)
		return Source{ScoresOnly: true, Skipped: skipNoSQLite}
	}
	if !published {
		p.log("sync: scores only, the newest build (%s) was not published, nothing to do", tag)
		return Source{ScoresOnly: true, Skipped: skipUnpublished}
	}
	p.log("sync: scores only, working on the build of %s", tag)
	return Source{DateTag: tag, ScoresOnly: true}
}

// exportScoresOnly is export-sqlite of the light run.
func (p *Pipeline) exportScoresOnly(ctx context.Context, src Source) (ExportOutput, error) {
	out := ExportOutput{ScoresOnly: true}
	if src.Skipped != "" {
		out.Skipped = src.Skipped
		return out, nil
	}
	if p.PGSQL == "" {
		return out, fmt.Errorf("PGSQL is not set")
	}
	// OpenStore only: no schema work, the light run writes nothing to Postgres.
	store, err := OpenStore(ctx, p.PGSQL)
	if err != nil {
		return out, err
	}
	defer store.Close()
	return p.exportScoresOnlyWith(ctx, src, store.GeoScores)
}

// exportScoresOnlyWith does the work with the scores reader given (tests fake it).
func (p *Pipeline) exportScoresOnlyWith(ctx context.Context, src Source, read func(context.Context) ([]GeoScoreRow, error)) (ExportOutput, error) {
	out := ExportOutput{ScoresOnly: true}
	final := filepath.Join(p.outDir(src.DateTag), sqliteFileName)
	scores, err := read(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		// Nothing is rewritten: the file keeps its old table.
		p.log("export: scores only, public.geo_eval could not be read, the file is left as it is: %v", schema.HideDSN(err))
		out.Skipped = skipScoresRead
		return out, nil
	}
	tmp := final + ".tmp"
	n, err := replaceGeoScores(ctx, final, tmp, scores)
	if err != nil {
		return out, err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return out, err
	}
	p.log("export: scores only, rewrote geo_scores of %s (geo_scores=%d)", final, n)
	out.Path = final
	out.SizeBytes = outputs.FileSize(final)
	out.Stats = ExportStats{GeoScores: n}
	return out, nil
}

// publishScoresOnly is publish of the light run: only the SQLite file goes to R2 (the
// dated key a full run of that date tag uses) and onto the rolling release (the release
// text and the MMDB asset stay). An empty sqlitePath (a skipped export) publishes nothing.
// No health-check ping.
func (p *Pipeline) publishScoresOnly(ctx context.Context, src Source, sqlitePath string) (PublishOutput, error) {
	out := PublishOutput{ScoresOnly: true}
	if src.Skipped != "" || sqlitePath == "" {
		out.Skipped = true
		return out, nil
	}
	r2, release, err := p.publishers()
	if err != nil {
		return out, err
	}
	return p.publishScoresOnlyWith(ctx, r2, release, src, sqlitePath)
}

func (p *Pipeline) publishScoresOnlyWith(ctx context.Context, r2 r2Putter, release releaseReplacer, src Source, sqlitePath string) (PublishOutput, error) {
	started := time.Now()
	out := PublishOutput{ScoresOnly: true}
	date, err := time.Parse(dateTagLayout, src.DateTag)
	if err != nil {
		return out, err
	}
	if err := p.putR2(ctx, r2, date, &out, sqlitePath); err != nil {
		return out, err
	}
	var replaced bool
	out.ReleaseSQLiteURL, replaced, err = release.Replace(ctx, sqlitePath, releaseSQLite, "")
	if err != nil {
		return out, fmt.Errorf("release SQLite asset: %w", err)
	}
	out.ReleaseSQLiteUnchanged = !replaced
	p.log("publish: %s (%s)", out.ReleaseSQLiteURL, uploadedOrUnchanged(replaced))
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	return out, nil
}
