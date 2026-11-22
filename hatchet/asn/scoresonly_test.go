package asn

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"openipdata.org/hatchet/publish"
)

func writeSQLiteDir(t *testing.T, root, tag string, withFile bool) {
	t.Helper()
	dir := filepath.Join(root, "out", "asn", tag)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if withFile {
		if err := os.WriteFile(filepath.Join(dir, sqliteFileName), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, publishedMark), nil, 0o640); err != nil {
			t.Fatal(err)
		}
	}
}

// A newer build that wrote its SQLite but never published (build-mmdb or publish failed)
// is not used, and neither is an older one (the release may hold a newer file than it):
// the run is skipped.
func TestSyncScoresOnlySkipsUnpublished(t *testing.T) {
	root := t.TempDir()
	p := &Pipeline{DataDir: root}
	writeSQLiteDir(t, root, "20261121", true)
	writeSQLiteDir(t, root, "20261122", true)
	if err := os.Remove(filepath.Join(root, "out", "asn", "20261122", publishedMark)); err != nil {
		t.Fatal(err)
	}
	if src := p.SyncScoresOnly(); src.Skipped != skipUnpublished || src.DateTag != "" {
		t.Errorf("src = %+v, want skipped: newest build not published", src)
	}
}

func TestSyncScoresOnlyNewestDir(t *testing.T) {
	root := t.TempDir()
	p := &Pipeline{DataDir: root}
	if src := p.SyncScoresOnly(); !src.ScoresOnly || src.Skipped != skipNoSQLite || src.DateTag != "" {
		t.Errorf("no dirs: %+v", src)
	}
	writeSQLiteDir(t, root, "20261120", true)
	writeSQLiteDir(t, root, "20261121", true)
	writeSQLiteDir(t, root, "20261122", false) // newest, but no file
	os.MkdirAll(filepath.Join(root, "out", "asn", "junk"), 0o750)
	src := p.SyncScoresOnly()
	if !src.ScoresOnly || src.Skipped != "" || src.DateTag != "20261121" {
		t.Errorf("src = %+v, want 20261121", src)
	}
	// An empty file, a .tmp only and a directory named like the file do not count.
	os.WriteFile(filepath.Join(root, "out", "asn", "20261122", sqliteFileName+".tmp"), []byte("x"), 0o640)
	if src := p.SyncScoresOnly(); src.DateTag != "20261121" {
		t.Errorf("tmp counted: %+v", src)
	}
	os.WriteFile(filepath.Join(root, "out", "asn", "20261122", sqliteFileName), nil, 0o640)
	if src := p.SyncScoresOnly(); src.DateTag != "20261121" {
		t.Errorf("empty file counted: %+v", src)
	}
}

func TestSkippedTasksOfScoresOnly(t *testing.T) {
	p := &Pipeline{}
	for _, src := range []Source{{ScoresOnly: true, DateTag: "20261121"}, {ScoresOnly: true, Skipped: skipNoSQLite}} {
		if st, err := p.Ingest(context.Background(), src); err != nil || st.Skipped == "" {
			t.Errorf("ingest %+v: %+v %v", src, st, err)
		}
		if out, err := p.BuildMMDB(context.Background(), src); err != nil || out.Skipped == "" || out.Path != "" {
			t.Errorf("build-mmdb %+v: %+v %v", src, out, err)
		}
	}
	out, err := p.ExportSQLite(context.Background(), Source{ScoresOnly: true, Skipped: skipNoSQLite})
	if err != nil || out.Skipped != skipNoSQLite || out.Path != "" {
		t.Errorf("export: %+v %v", out, err)
	}
	pub, err := p.Publish(context.Background(), Source{ScoresOnly: true, Skipped: skipNoSQLite}, "", "")
	if err != nil || !pub.Skipped || !pub.ScoresOnly {
		t.Errorf("publish: %+v %v", pub, err)
	}
}

// fullFile makes a real asn.sqlite3 with every table and two old score rows.
func fullFile(t *testing.T, path string) {
	t.Helper()
	records := []Record{{ASN: 1, IPAmount: "256", IPv4Amount: "256", IPv6Amount: "0.00", JSON: "{}"}}
	domainRows := []DomainRow{{ASN: 1, Domain: strPtr("example.com"), Source: strPtr("pdb")}}
	geo := []GeoPDBRow{{ASN: 1}}
	one := 1.0
	scores := []GeoScoreRow{{ID: 1, RunAt: "2026-11-01T07:00:00Z", CityPct: &one}, {ID: 2, RunAt: "2026-11-02T07:00:00Z", CityPct: &one}}
	if _, err := ExportSQLite(context.Background(), path, records, domainRows, geo, scores); err != nil {
		t.Fatal(err)
	}
}

func queryInts(t *testing.T, path, q string) []int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestExportScoresOnlyReplacesTable(t *testing.T) {
	root := t.TempDir()
	writeSQLiteDir(t, root, "20261121", false)
	final := filepath.Join(root, "out", "asn", "20261121", sqliteFileName)
	fullFile(t, final)
	p := &Pipeline{DataDir: root}
	src := Source{ScoresOnly: true, DateTag: "20261121"}

	two := 2.0
	read := func(context.Context) ([]GeoScoreRow, error) {
		return []GeoScoreRow{
			{ID: 5, RunAt: "2026-11-21T07:00:00Z", CityPct: &two},
			{ID: 4, RunAt: "2026-11-21T07:00:00Z"}, // same second, earlier id: dropped
			{ID: 6, RunAt: "2026-11-22T07:00:00Z", AtlasCityPct: &two},
		}, nil
	}
	out, err := p.exportScoresOnlyWith(context.Background(), src, read)
	if err != nil {
		t.Fatal(err)
	}
	if out.Path != final || !out.ScoresOnly || out.Stats.GeoScores != 2 || out.Skipped != "" {
		t.Errorf("out = %+v", out)
	}
	if got := queryInts(t, final, `SELECT count(*) FROM geo_scores`); !slices.Equal(got, []int{2}) {
		t.Errorf("geo_scores rows = %v", got)
	}
	if got := queryInts(t, final, `SELECT count(*) FROM geo_scores WHERE run_at LIKE '2026-11-0%'`); !slices.Equal(got, []int{0}) {
		t.Errorf("old rows left: %v", got)
	}
	if got := queryInts(t, final, `SELECT count(*) FROM geo_scores WHERE run_at='2026-11-21T07:00:00Z' AND city_pct=2 AND atlas_city_pct IS NULL`); !slices.Equal(got, []int{1}) {
		t.Errorf("row 1 = %v", got)
	}
	for _, q := range []string{`SELECT count(*) FROM asn`, `SELECT count(*) FROM asn_domain`, `SELECT count(*) FROM asn_geo_pdb`} {
		if got := queryInts(t, final, q); !slices.Equal(got, []int{1}) {
			t.Errorf("%s = %v, want 1 (other tables untouched)", q, got)
		}
	}
	if got := queryInts(t, final, `SELECT count(*) FROM sqlite_master WHERE name LIKE 'idx_asn_%'`); !slices.Equal(got, []int{5}) {
		t.Errorf("indexes = %v", got)
	}
	if _, err := os.Stat(final + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp left: %v", err)
	}
}

func TestExportScoresOnlyReadErrorKeepsFile(t *testing.T) {
	root := t.TempDir()
	writeSQLiteDir(t, root, "20261121", false)
	final := filepath.Join(root, "out", "asn", "20261121", sqliteFileName)
	fullFile(t, final)
	before, _ := os.ReadFile(final)
	var logged []string
	p := &Pipeline{DataDir: root, Progress: func(s string) { logged = append(logged, s) }}
	read := func(context.Context) ([]GeoScoreRow, error) { return nil, errors.New("connection lost") }
	out, err := p.exportScoresOnlyWith(context.Background(), Source{ScoresOnly: true, DateTag: "20261121"}, read)
	if err != nil || out.Skipped == "" || out.Path != "" {
		t.Errorf("out = %+v, err = %v", out, err)
	}
	after, _ := os.ReadFile(final)
	if !bytes.Equal(before, after) {
		t.Error("the file changed")
	}
	if _, err := os.Stat(final + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp left: %v", err)
	}
	if len(logged) != 1 {
		t.Errorf("log = %v", logged)
	}
}

func TestPublishScoresOnly(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir()} // no MMDBBuilder, no HCPing: a convert or ping would fail or be seen
	sqlite := filepath.Join(p.DataDir, sqliteFileName)
	os.WriteFile(sqlite, []byte("sqlite"), 0o640)
	r2 := &fakeR2{status: publish.PutUploaded}
	rel := &fakeRelease{}
	out, err := p.publishScoresOnlyWith(context.Background(), r2, rel, Source{ScoresOnly: true, DateTag: "20261121"}, sqlite)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r2.keys, []string{"2026-11-21/asn.sqlite3"}) {
		t.Errorf("r2 keys = %v", r2.keys)
	}
	if !slices.Equal(rel.calls, []string{releaseSQLite}) || rel.bodies[releaseSQLite] != "" {
		t.Errorf("release calls = %v bodies = %v", rel.calls, rel.bodies)
	}
	if !out.ScoresOnly || out.ReleaseSQLiteURL == "" || out.ReleaseURL != "" || out.Bytes != int64(len("sqlite")) || len(out.R2) != 1 {
		t.Errorf("out = %+v", out)
	}

	// Unchanged release asset and a failing release call.
	rel = &fakeRelease{unchanged: map[string]bool{releaseSQLite: true}}
	out, err = p.publishScoresOnlyWith(context.Background(), &fakeR2{status: publish.PutUnchanged}, rel, Source{DateTag: "20261121"}, sqlite)
	if err != nil || !out.ReleaseSQLiteUnchanged || len(out.R2Unchanged) != 1 {
		t.Errorf("unchanged: %+v %v", out, err)
	}
	if _, err := p.publishScoresOnlyWith(context.Background(), &fakeR2{}, &fakeRelease{failOn: releaseSQLite}, Source{DateTag: "20261121"}, sqlite); err == nil {
		t.Error("release failure did not fail")
	}
}

// Publish with the scores-only Source sends no ping (HCPing would be called if it were reached)
// and never needs the builder; with settings missing it fails like a full run.
func TestPublishScoresOnlyNeedsSettingsAndNoPing(t *testing.T) {
	p := &Pipeline{HCPing: "http://127.0.0.1:1/ping"}
	if _, err := p.Publish(context.Background(), Source{ScoresOnly: true, DateTag: "20261121"}, "", "x.sqlite3"); err == nil {
		t.Error("expected a settings error")
	}
}
