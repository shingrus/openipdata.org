package arin

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"openipdata.org/hatchet/publish"
)

// The records live in files in DataDir/arin, not in a database: no other workflow reads them.
//
//	records.csv.gz       the base: the first line is "#position <cursor> <cycle_started>", then a CSV
//	                     with the header net_start,net_end,country,state,city,looked_up (Unix
//	                     seconds), sorted by start then end. The position line says: the records
//	                     are complete up to this walk position.
//	records.journal.csv  the rows of the batches stored since the last compaction (same columns, no
//	                     header), appended once per batch and synced.
//	progress.json        the walk position, written after the journal append is synced.
//	backup.json          what the last copy of the base in R2 sent (see Upload).
//
// A copy of the base is kept in the private R2 bucket (RecordsKey) once a day. When the base is
// lost, Progress restores it from there; when R2 cannot be read at that moment the lookup is
// skipped: the walk never starts again from 0 only because R2 was down.
const (
	// RecordsKey is the fixed key of the daily copy of the records in the private R2 bucket. It is
	// this workflow's own backup: no other workflow reads it.
	RecordsKey = "internal/arin/records.csv.gz"

	recordsFile  = "records.csv.gz"
	journalFile  = "records.journal.csv"
	progressFile = "progress.json"
	backupFile   = "backup.json"
	damagedFile  = "records.damaged" // a base whose content could not be read, moved aside; removed after a restore from R2
	lockFile     = "lock"            // held by a task while it works in the directory

	positionTag = "#position"

	skipNoRecords = "no records file and R2 cannot be read"
)

// errNoRecords is Progress's answer when the base is missing and R2 cannot say what it holds.
var errNoRecords = errors.New("no records file and R2 cannot be read")

// errDamaged is Compact's answer for a base that cannot be read to the end.
var errDamaged = errors.New("the records file was damaged and moved aside; the next run restores it from R2")

// errNoBase is Compact's answer when there is no base. Compact never makes one: only Progress
// does, after R2 said whether it has a copy (a base made here from the journal alone could be
// copied over the good one in R2).
var errNoBase = errors.New("no records file")

// recordsR2 is the part of publish.R2 the store uses to restore the base. Tests replace it.
type recordsR2 interface {
	Stat(ctx context.Context, key string) (publish.ObjectInfo, error)
	Download(ctx context.Context, key, path string) (time.Time, error)
}

// lookupStore is the records and the walk position, as Lookup and Export use them. Tests replace it.
type lookupStore interface {
	// Progress returns the walk position and when the cycle began. restored is true when the base
	// was lost and has been fetched from R2. errNoRecords: the base is missing and R2 cannot be read.
	Progress(ctx context.Context, now time.Time) (cursor int, started time.Time, restored bool, err error)
	// Save appends the records of a batch (looked up at now) and then moves the position.
	Save(ctx context.Context, recs []record, cursor int, started time.Time, now time.Time) error
	// Compact merges the journal into the base (the newest record of a range wins, records older than
	// pruneAfter go) and returns all records.
	Compact(ctx context.Context, now time.Time) (recs []record, pruned int, err error)
}

// fileStore is the lookupStore of the real workflow.
type fileStore struct {
	dir     string
	objects func() (recordsR2, error) // R2; an error: R2 cannot be used
	log     func(format string, args ...any)
}

func (s *fileStore) path(name string) string { return filepath.Join(s.dir, name) }

// position is the walk position and when the cycle began.
type position struct {
	Cursor       int       `json:"cursor"`
	CycleStarted time.Time `json:"cycle_started"`
}

// writeFile writes data through a .tmp file and renames it.
func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (s *fileStore) readProgress() (position, bool) {
	var p position
	data, err := os.ReadFile(s.path(progressFile))
	if err != nil || json.Unmarshal(data, &p) != nil || p.Cursor < 0 || p.CycleStarted.IsZero() {
		return position{}, false
	}
	return p, true
}

func (s *fileStore) writeProgress(p position) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeFile(s.path(progressFile), data)
}

// basePosition reads the first line of the base; ok is false when the file cannot be read or is
// not a records file (see readPosition for which of the two).
func basePosition(path string) (position, bool) {
	p, err := readPosition(path)
	return p, err == nil
}

// errNotRecords is readPosition's answer for a file whose first line is not a position line.
var errNotRecords = errors.New("not a records file")

// readPosition reads the first line of the base. A file-system error (isFileError) means the file
// could not be read; any other error means its content is not a records file.
func readPosition(path string) (position, error) {
	f, err := os.Open(path)
	if err != nil {
		return position{}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return position{}, err
	}
	line, err := bufio.NewReader(gz).ReadString('\n')
	if err != nil {
		return position{}, err
	}
	p, ok := parsePosition(line)
	if !ok {
		return position{}, errNotRecords
	}
	return p, nil
}

func parsePosition(line string) (position, bool) {
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != positionTag {
		return position{}, false
	}
	cursor, err := strconv.Atoi(f[1])
	started, err2 := time.Parse(time.RFC3339, f[2])
	if err != nil || err2 != nil || cursor < 0 {
		return position{}, false
	}
	return position{cursor, started}, true
}

func (s *fileStore) Progress(ctx context.Context, now time.Time) (int, time.Time, bool, error) {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return 0, now, false, err
	}
	base := s.path(recordsFile)
	bp, err := readPosition(base)
	switch {
	case err == nil:
		if p, ok := s.readProgress(); ok {
			return p.Cursor, p.CycleStarted, false, nil
		}
		// No progress file: the /24s since the last compaction are asked again, which is harmless.
		return bp.Cursor, bp.CycleStarted, false, nil
	case errors.Is(err, os.ErrNotExist):
		// Lost or never made: R2 says which.
	case isFileError(err):
		// A base that is there but cannot be opened or read (permissions, the disk) is not a lost
		// base: the task fails, and nothing is fetched or written over it.
		return 0, now, false, err
	default:
		// The content is not a records file: moved aside, never written over.
		if err := os.Rename(base, s.path(damagedFile)); err != nil {
			return 0, now, false, err
		}
		s.log("lookup: WARNING the records file cannot be read and was moved aside")
	}
	// The base is missing: R2 has the last copy.
	r2, err := s.objects()
	if err != nil {
		return 0, now, false, errNoRecords
	}
	if _, err := r2.Stat(ctx, RecordsKey); err != nil {
		if ctx.Err() != nil {
			return 0, now, false, ctx.Err()
		}
		if !errors.Is(err, publish.ErrNotFound) {
			return 0, now, false, errNoRecords
		}
		return s.startBase(now)
	}
	tmp := base + ".restore"
	defer os.Remove(tmp)
	// Part files of a download that was killed: the client would try to go on from them and fail.
	for _, pattern := range []string{"*.part.minio", recordsFile + ".restore*"} {
		if old, _ := filepath.Glob(filepath.Join(s.dir, pattern)); len(old) > 0 {
			for _, name := range old {
				os.Remove(name)
			}
		}
	}
	if _, err := r2.Download(ctx, RecordsKey, tmp); err != nil {
		if ctx.Err() != nil {
			return 0, now, false, ctx.Err()
		}
		if isFileError(err) {
			return 0, now, false, err // the disk, not R2: the task fails
		}
		return 0, now, false, errNoRecords
	}
	// The whole copy is read before it is used: a copy that is not a records file, or is cut
	// short, must not become the base.
	bp, ok := basePosition(tmp)
	n, err := countBase(tmp)
	if !ok || err != nil {
		s.log("lookup: WARNING the copy of the records file in R2 cannot be read")
		return 0, now, false, errNoRecords
	}
	if err := os.Chmod(tmp, 0o640); err != nil {
		return 0, now, false, err
	}
	// The position first, then the base: a crash in between leaves no base, and the next run
	// restores again. The other order could leave the restored base with a newer position, and
	// the /24s between the two would not be asked again.
	if err := s.writeProgress(bp); err != nil {
		return 0, now, false, err
	}
	if err := os.Rename(tmp, base); err != nil {
		return 0, now, false, err
	}
	// What R2 holds is now known: the next copy is refused if the file has shrunk to less than half.
	if data, err := json.Marshal(backupState{UploadedAt: now, Records: n}); err == nil {
		writeFile(s.path(backupFile), data)
	}
	os.Remove(s.path(damagedFile))
	s.log("lookup: restored the records file from R2: %d records, position %d", n, bp.Cursor)
	return bp.Cursor, bp.CycleStarted, true, nil
}

// startBase makes the first base: R2 has no copy. On the very first start that is an empty base at
// position 0. When a position was already saved, the base that went with it is gone (removed as
// damaged, or deleted) and with it its records, so the walk also starts again at 0, in the same
// cycle; the journal stays.
func (s *fileStore) startBase(now time.Time) (int, time.Time, bool, error) {
	started, lost := now, false
	if p, ok := s.readProgress(); ok {
		started, lost = p.CycleStarted, p.Cursor > 0
	}
	if _, err := os.Stat(s.path(damagedFile)); err == nil {
		lost = true
	}
	if _, err := os.Stat(s.path(backupFile)); err == nil {
		lost = true // a copy was sent once and R2 no longer has it
	}
	if lost { // before anything is written: after a crash the next run could no longer tell
		s.log("lookup: WARNING the records file is gone and R2 has no copy: the walk starts again")
	}
	// backup.json described a copy that is gone: with it the daily copy of the new, small base
	// would be refused. A file moved aside as damaged stays: it is the last copy of that data.
	if err := os.Remove(s.path(backupFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, now, false, err
	}
	pos := position{0, started}
	// The position first: see Progress.
	if err := s.writeProgress(pos); err != nil {
		return 0, now, false, err
	}
	if err := writeBase(s.path(recordsFile), pos, nil); err != nil {
		return 0, now, false, err
	}
	return 0, started, false, nil
}

var rowHeader = []string{"net_start", "net_end", "country", "state", "city", "looked_up"}

func rowOf(r record) []string {
	return []string{addrOf(r.Start).String(), addrOf(r.End).String(), r.Country, r.State, r.City, strconv.FormatInt(r.LookedUp, 10)}
}

// parseRow reads one row; ok is false for anything that is not a record.
func parseRow(f []string, intern func(string) string) (record, bool) {
	if len(f) != 6 {
		return record{}, false
	}
	s, e, ok := parseRange(f[0], f[1])
	at, err := strconv.ParseInt(f[5], 10, 64)
	if !ok || err != nil {
		return record{}, false
	}
	return record{Start: s, End: e, Country: intern(f[2]), State: intern(f[3]), City: intern(f[4]), LookedUp: at}, true
}

func (s *fileStore) Save(ctx context.Context, recs []record, cursor int, started time.Time, now time.Time) error {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return err
	}
	if len(recs) > 0 {
		var buf strings.Builder
		w := csv.NewWriter(&buf)
		for _, r := range recs {
			r.LookedUp = now.Unix()
			w.Write(rowOf(r))
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return err
		}
		f, err := os.OpenFile(s.path(journalFile), os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o640)
		if err != nil {
			return err
		}
		// A crash can leave half a row at the end. It is cut off: that batch was never confirmed
		// (its position was not saved), and a half row that ends inside a quoted city would
		// swallow every row written after it.
		if err := dropTornRow(f); err != nil {
			f.Close()
			return err
		}
		rows := buf.String()
		_, werr := io.WriteString(f, rows)
		if werr == nil {
			werr = f.Sync()
		}
		if err := errors.Join(werr, f.Close()); err != nil {
			return err
		}
	}
	// After the journal is synced: a crash in between only makes the next run ask the batch again.
	return s.writeProgress(position{cursor, started})
}

// isFileError reports whether err comes from the file system (a read, a write, a rename) and
// not from the content of a file or from R2.
func isFileError(err error) bool {
	var pe *os.PathError
	var le *os.LinkError
	return errors.As(err, &pe) || errors.As(err, &le)
}

// dropTornRow cuts the file back to the end of its last complete line.
func dropTornRow(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	end := fi.Size()
	buf := make([]byte, 4096)
	for pos := end; pos > 0; {
		n := int64(len(buf))
		if n > pos {
			n = pos
		}
		pos -= n
		if _, err := f.ReadAt(buf[:n], pos); err != nil {
			return err
		}
		if i := strings.LastIndexByte(string(buf[:n]), '\n'); i >= 0 {
			if keep := pos + int64(i) + 1; keep < end {
				return f.Truncate(keep)
			}
			return nil
		}
	}
	if end > 0 {
		return f.Truncate(0) // not one complete line
	}
	return nil
}

// readRows calls fn for each record of a CSV stream. With strict an unreadable row is an error;
// otherwise it is skipped (a torn last line of the journal).
func readRows(r io.Reader, strict, header bool, intern func(string) string, fn func(record)) error {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	if header {
		h, err := cr.Read()
		if err != nil || strings.Join(h, ",") != strings.Join(rowHeader, ",") {
			return errors.New("bad header")
		}
	}
	for {
		f, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		var pe *csv.ParseError
		if err != nil && !errors.As(err, &pe) {
			return err
		}
		if err != nil {
			if strict {
				return err
			}
			continue
		}
		rec, ok := parseRow(f, intern)
		if !ok {
			if strict {
				return errors.New("bad row")
			}
			continue
		}
		fn(rec)
	}
}

// readBase reads the base to its end (the gzip checksum covers every byte).
func readBase(path string, intern func(string) string, fn func(record)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return readBaseFrom(f, intern, fn)
}

func readBaseFrom(f io.Reader, intern func(string) string, fn func(record)) error {
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	br := bufio.NewReader(gz)
	line, err := br.ReadString('\n')
	if err != nil {
		return err
	}
	if _, ok := parsePosition(line); !ok {
		return errors.New("bad position line")
	}
	if err := readRows(br, true, true, intern, fn); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, gz) // reads to the end: a damaged stream is an error
	return err
}

// countBase counts the records of the base.
func countBase(path string) (int, error) {
	n := 0
	err := readBase(path, func(s string) string { return "" }, func(record) { n++ })
	return n, err
}

func (s *fileStore) Compact(ctx context.Context, now time.Time) ([]record, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	base := s.path(recordsFile)
	bf, err := os.Open(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, errNoBase
	}
	if err != nil {
		return nil, 0, err // not damage: a file that cannot be opened stays where it is
	}
	// One string for each distinct country, state and city.
	seen := map[string]string{}
	intern := func(v string) string {
		if c, ok := seen[v]; ok {
			return c
		}
		seen[v] = v
		return v
	}
	oldPos, haveOld := basePosition(base) // before the base is written again
	var recs []record
	err = readBaseFrom(bf, intern, func(r record) { recs = append(recs, r) })
	bf.Close()
	if isFileError(err) {
		return nil, 0, err // the disk could not be read: that says nothing about the content
	}
	if err != nil {
		// Moved aside in one step: there is never a moment with no base and no sign of the damage.
		if rerr := os.Rename(base, s.path(damagedFile)); rerr != nil {
			return nil, 0, rerr
		}
		return nil, 0, errDamaged
	}
	if f, err := os.Open(s.path(journalFile)); err == nil {
		err = readRows(bufio.NewReader(f), false, false, intern, func(r record) { recs = append(recs, r) })
		f.Close()
		if err != nil {
			return nil, 0, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}

	// The newest record of a range wins; a tie goes to the one read later.
	sort.SliceStable(recs, func(i, j int) bool {
		a, b := recs[i], recs[j]
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		if a.End != b.End {
			return a.End < b.End
		}
		return a.LookedUp < b.LookedUp
	})
	cutoff := now.Add(-pruneAfter).Unix()
	kept, pruned := recs[:0], 0
	for i, r := range recs {
		if i+1 < len(recs) && recs[i+1].Start == r.Start && recs[i+1].End == r.End {
			continue // a newer record of the range follows
		}
		if r.LookedUp < cutoff {
			pruned++
			continue
		}
		kept = append(kept, r)
	}
	recs = kept

	// The position line: progress.json; without it the line the base already had (never 0 for a
	// base that holds records, or the walk would start again); a first base starts at 0.
	pos, ok := s.readProgress()
	switch {
	case ok:
	case haveOld:
		pos = oldPos
	default:
		pos = position{0, now}
	}
	if err := writeBase(base, pos, recs); err != nil {
		return nil, 0, err
	}
	// The journal goes only after the base is in place: a crash before this applies the same rows again.
	if err := os.Remove(s.path(journalFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	return recs, pruned, nil
}

// writeBase writes the base through a .tmp file.
func writeBase(path string, pos position, recs []record) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	gz := gzip.NewWriter(f)
	fmt.Fprintf(gz, "%s %d %s\n", positionTag, pos.Cursor, pos.CycleStarted.UTC().Format(time.RFC3339))
	w := csv.NewWriter(gz)
	w.Write(rowHeader)
	for _, r := range recs {
		w.Write(rowOf(r))
	}
	w.Flush()
	if err := errors.Join(w.Error(), gz.Close(), f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// backupState is backup.json: what the last copy of the base in R2 sent.
type backupState struct {
	UploadedAt time.Time `json:"uploaded_at"`
	Records    int       `json:"records"`
}

func (p *Pipeline) readBackup() (backupState, bool) {
	var b backupState
	data, err := os.ReadFile(filepath.Join(p.dir(), backupFile))
	if err != nil || json.Unmarshal(data, &b) != nil || b.UploadedAt.IsZero() {
		return backupState{}, false
	}
	return b, true
}

// recordsObjects is the R2 client of the store: the test one, or one made from the R2 setting.
func (p *Pipeline) recordsObjects() (recordsR2, error) {
	if p.objects != nil {
		return p.objects, nil
	}
	return publish.NewR2(p.R2)
}

func (p *Pipeline) newStore() lookupStore {
	if p.db != nil {
		return p.db
	}
	return &fileStore{dir: p.dir(), objects: p.recordsObjects, log: p.log}
}
