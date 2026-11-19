package arin

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"openipdata.org/hatchet/publish"
)

// holders.csv keeps the answers whose holder class matters: every wide record (wider than a /16,
// at most a /8) with a usable city, and every record of at most a /16 whose holder is a school or
// university (edu) or a local government (gov; state governments and statewide bodies are other). Columns:
// net_start,net_end,country,state,city,class,looked_up (Unix seconds). Rows are appended and synced
// once per stored batch, BEFORE the journal and the walk position are written, so a crash can only
// repeat rows, never lose them behind the position. Export compacts it (newest row of a range
// wins, rows older than pruneAfter go). The holder's name is never stored: only its class.
const (
	holdersFile       = "holders.csv"
	holdersBackupFile = "holders-backup.json"
	holdersGzFile     = "holders.csv.gz" // the copy sent to R2; removed after the upload
)

var holderHeader = []string{"net_start", "net_end", "country", "state", "city", "class", "looked_up"}

func holderRowOf(r record) []string {
	return []string{addrOf(r.Start).String(), addrOf(r.End).String(), r.Country, r.State, r.City, r.Class, strconv.FormatInt(r.LookedUp, 10)}
}

// parseHolderRow reads one row; ok is false for anything that is not a holder row (the header too).
func parseHolderRow(f []string, intern func(string) string) (record, bool) {
	if len(f) != 7 {
		return record{}, false
	}
	switch f[5] {
	case classEdu, classGov, classMil, classOther:
	default:
		return record{}, false
	}
	s, e, ok := parseRange(f[0], f[1])
	at, err := strconv.ParseInt(f[6], 10, 64)
	if !ok || err != nil {
		return record{}, false
	}
	return record{Start: s, End: e, Country: intern(f[2]), State: intern(f[3]), City: intern(f[4]), Class: f[5], LookedUp: at}, true
}

// readHolderRows calls fn for each row of a holders CSV stream; a row that does not parse is skipped.
func readHolderRows(r io.Reader, intern func(string) string, fn func(record)) error {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
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
			continue
		}
		if rec, ok := parseHolderRow(f, intern); ok {
			fn(rec)
		}
	}
}

// holderSet is the ranges (at most a /16) whose newest row in holders.csv is edu or gov. A newer
// answer of such a range is always written, so it can switch the old row off.
type holderSet map[[2]uint32]struct{}

// loadHolderSet reads the file; a missing or unreadable file gives an empty set.
func loadHolderSet(dir string) holderSet {
	set := holderSet{}
	f, err := os.Open(filepath.Join(dir, holdersFile))
	if err != nil {
		return set
	}
	defer f.Close()
	newest := map[[2]uint32]record{}
	err = readHolderRows(bufio.NewReader(f), func(s string) string { return "" }, func(r record) {
		k := [2]uint32{r.Start, r.End}
		if old, ok := newest[k]; !ok || r.LookedUp >= old.LookedUp {
			newest[k] = r
		}
	})
	if err != nil {
		return holderSet{}
	}
	for k, r := range newest {
		if r.size() <= maxStoredBlock && (r.Class == classEdu || r.Class == classGov) {
			set[k] = struct{}{}
		}
	}
	return set
}

// holdersWanted picks, from the answers of a batch, the rows of the holders file: a wide answer
// with a usable country, state and city (its range is at most a /8); a found answer with a usable
// city whose class is edu or gov; and a found answer whose range is in set (an earlier edu or gov
// row), whatever it says now, also with no city: the newest row of a range wins in the compaction,
// so it switches the old row off. One row per range (the later answer wins), sorted by start then
// end.
func holdersWanted(answers []answer, set holderSet) []record {
	seen := map[[2]uint32]int{}
	var out []record
	for _, an := range answers {
		r := an.rec
		if r.size() > maxHolderBlock {
			continue
		}
		k := [2]uint32{r.Start, r.End}
		switch an.kind {
		case kindWide:
			if r.Country == "" || r.State == "" || r.City == "" {
				continue
			}
		case kindFound:
			_, known := set[k]
			if !known && (r.City == "" || (r.Class != classEdu && r.Class != classGov)) {
				continue
			}
			if !known && (r.Country == "" || r.State == "") {
				continue
			}
		default:
			continue
		}
		if i, ok := seen[k]; ok {
			out[i] = r
			continue
		}
		seen[k] = len(out)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].End < out[j].End
	})
	return out
}

// note updates the set after rows were appended: edu and gov rows join it, any other row of a
// range leaves it.
func (s holderSet) note(rows []record) {
	for _, r := range rows {
		k := [2]uint32{r.Start, r.End}
		if r.size() <= maxStoredBlock && (r.Class == classEdu || r.Class == classGov) && r.City != "" {
			s[k] = struct{}{}
		} else {
			delete(s, k)
		}
	}
}

// appendHolders appends the rows (looked up at now) and syncs the file. A half row left by a crash
// is cut off first, as in the journal. The header is written when the file is empty.
func appendHolders(dir string, recs []record, now time.Time) error {
	if len(recs) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, holdersFile), os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	if err := dropTornRow(f); err != nil {
		f.Close()
		return err
	}
	var buf strings.Builder
	w := csv.NewWriter(&buf)
	if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
		w.Write(holderHeader)
	}
	for _, r := range recs {
		r.LookedUp = now.Unix()
		w.Write(holderRowOf(r))
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	_, werr := io.WriteString(f, buf.String())
	if werr == nil {
		werr = f.Sync()
	}
	return errors.Join(werr, f.Close())
}

// compactHolders reads holders.csv, keeps one row per range (the newest), drops rows older than
// pruneAfter, writes the file again through a .tmp file, sorted by start then end, and returns the
// rows. A missing file is an empty set. An error (the file cannot be read or written) means the
// caller must go on without holders.
func compactHolders(dir string, now time.Time) (rows []record, pruned int, err error) {
	path := filepath.Join(dir, holdersFile)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	seen := map[string]string{}
	intern := func(v string) string {
		if c, ok := seen[v]; ok {
			return c
		}
		seen[v] = v
		return v
	}
	var recs []record
	err = readHolderRows(bufio.NewReader(f), intern, func(r record) { recs = append(recs, r) })
	f.Close()
	if err != nil {
		return nil, 0, err
	}
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
	kept := recs[:0]
	for i, r := range recs {
		if i+1 < len(recs) && recs[i+1].Start == r.Start && recs[i+1].End == r.End {
			continue // a newer row of the range follows
		}
		if r.LookedUp < cutoff {
			pruned++
			continue
		}
		kept = append(kept, r)
	}
	if err := writeHolders(path, kept); err != nil {
		return nil, 0, err
	}
	return kept, pruned, nil
}

// writeHolders writes the file through a .tmp file.
func writeHolders(path string, recs []record) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	w := csv.NewWriter(f)
	w.Write(holderHeader)
	for _, r := range recs {
		w.Write(holderRowOf(r))
	}
	w.Flush()
	if err := errors.Join(w.Error(), f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Results of the holders restore and backup (fixed words only).
const (
	holdersRestored = "restored"
	holdersFailed   = "failed"
)

// restoreHolders fetches holders.csv from R2 when the file is missing. It is best effort: any
// problem returns a fixed result and the task goes on with an empty set. "" means nothing was
// done (the file is there, R2 is not set up, or R2 has no copy).
func (p *Pipeline) restoreHolders(ctx context.Context) string {
	path := filepath.Join(p.dir(), holdersFile)
	if _, err := os.Stat(path); err == nil {
		return ""
	} else if !errors.Is(err, os.ErrNotExist) {
		p.log("lookup: WARNING the holders file cannot be checked")
		return holdersFailed
	}
	if p.objects == nil && strings.TrimSpace(p.R2) == "" {
		return "" // no R2 setting (local runs, tests)
	}
	r2, err := p.recordsObjects()
	if err != nil {
		p.log("lookup: WARNING holders restore skipped: R2 cannot be used")
		return holdersFailed
	}
	if _, err := r2.Stat(ctx, HoldersKey); err != nil {
		if ctx.Err() == nil && !errors.Is(err, publish.ErrNotFound) {
			p.log("lookup: WARNING holders restore skipped: R2 cannot be read")
			return holdersFailed
		}
		return ""
	}
	gzPath := filepath.Join(p.dir(), holdersGzFile+".restore")
	tmp := path + ".restore"
	// Part files of a download that was killed: the client would try to go on from them and fail.
	if old, _ := filepath.Glob(gzPath + "*"); len(old) > 0 {
		for _, name := range old {
			os.Remove(name)
		}
	}
	defer os.Remove(gzPath)
	defer os.Remove(tmp)
	if _, err := r2.Download(ctx, HoldersKey, gzPath); err != nil {
		p.log("lookup: WARNING holders restore failed: the copy cannot be downloaded")
		return holdersFailed
	}
	n, err := gunzipHolders(gzPath, tmp)
	if err != nil {
		p.log("lookup: WARNING holders restore failed: the copy cannot be read")
		return holdersFailed
	}
	if err := os.Rename(tmp, path); err != nil {
		p.log("lookup: WARNING holders restore failed: the file cannot be written")
		return holdersFailed
	}
	// The row count is what the next backup is compared with, so the refusal also holds after a wipe.
	if data, err := json.Marshal(backupState{UploadedAt: p.clock().UTC(), Records: n}); err == nil {
		writeFile(filepath.Join(p.dir(), holdersBackupFile), data)
	}
	p.log("lookup: restored the holders file from R2: %d rows", n)
	return holdersRestored
}

// gunzipHolders reads a gzipped holders file to its end and writes the plain CSV (only rows that
// parse) to dst; it returns the number of rows.
func gunzipHolders(src, dst string) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	var recs []record
	if err := readHolderRows(bufio.NewReader(gz), func(s string) string { return s }, func(r record) { recs = append(recs, r) }); err != nil {
		return 0, err
	}
	if _, err := io.Copy(io.Discard, gz); err != nil { // a damaged stream is an error
		return 0, err
	}
	return len(recs), writeHolders(dst, recs)
}
