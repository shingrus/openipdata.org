package arin

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"openipdata.org/hatchet/publish"
)

// uploadFunc is publish.R2.Put. Tests replace it.
type uploadFunc func(ctx context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error)

// backupEvery is how often the records file is copied to R2 (backup.json holds the last time).
const backupEvery = 24 * time.Hour

// Backup results (UploadOutput.Backup).
const (
	backupUploaded  = "uploaded"
	backupUnchanged = "unchanged" // R2 already holds these bytes
	backupRecent    = "recent"    // the last copy is younger than 24 hours
	backupRefused   = "refused"   // the file has less than half the records of the last copy
	backupNoFile    = "no records file"
)

// CheckUpload fails when the R2 setting is missing or invalid, so a run stops before the work.
func (p *Pipeline) CheckUpload() error {
	_, err := publish.NewR2(p.R2)
	return err
}

// UploadOutput is the upload task's result.
type UploadOutput struct {
	Skipped       string            `json:"skipped,omitempty"` // "no layer file"
	Status        publish.PutStatus `json:"status,omitempty"`  // the layer: uploaded, unchanged or refreshed
	Bytes         int64             `json:"bytes"`
	Backup        string            `json:"backup,omitempty"` // the records file: uploaded, unchanged, recent, refused or no records file
	BackupRecords int               `json:"backup_records,omitempty"`
	// HoldersBackup is the result of the daily copy of holders.csv: uploaded, unchanged, recent,
	// refused, failed, or empty when there is no file. A problem only shows here, it never fails the task.
	HoldersBackup   string  `json:"holders_backup,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// Upload puts the layer file at LayerKey with its sha256 in the object's metadata, as sync-bgp and
// sync-geofeeds do: an object that already holds the same bytes is not sent again, and a
// server-side copy renews its modified time. force always sends the bytes. With no layer file yet
// that part is skipped and the task succeeds. Then, independent of the layer, the records file is
// copied to RecordsKey once a day (see backup). No health check is pinged.
func (p *Pipeline) Upload(ctx context.Context, force bool) (UploadOutput, error) {
	started := time.Now()
	var out UploadOutput
	unlock, ok, err := p.lock()
	if err != nil {
		return out, err
	}
	if !ok {
		out.Skipped = skipBusy
		p.log("upload: skipped (another task or run-arin is working in the directory)")
		return out, nil
	}
	defer unlock()
	put := p.upload
	if put == nil {
		r2, err := publish.NewR2(p.R2)
		if err != nil {
			return out, err
		}
		put = r2.Put
	}
	fi, err := os.Stat(p.LayerPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		out.Skipped = skipNoLayer
		p.log("upload: layer skipped (%s)", skipNoLayer)
	case err != nil:
		return out, err
	default:
		_, status, err := put(ctx, LayerKey, p.LayerPath(), publish.PutOptions{Force: force, Refresh: true})
		if err != nil {
			return out, err
		}
		out.Status, out.Bytes = status, fi.Size()
		p.log("upload: %s (%d bytes)", status, out.Bytes)
	}
	if err := p.backup(ctx, put, force, &out); err != nil {
		return out, err
	}
	if err := p.backupHolders(ctx, put, force, &out); err != nil {
		return out, err
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	return out, nil
}

// backupHolders copies holders.csv, gzipped, to HoldersKey when there is none yet or the last copy is
// at least 24 hours old (force skips that wait). A file with less than half the rows of the last
// copy is refused, also with force (delete holders-backup.json to accept it). Any problem is only
// counted in the output; only a cancellation is returned.
func (p *Pipeline) backupHolders(ctx context.Context, put uploadFunc, force bool, out *UploadOutput) error {
	src := filepath.Join(p.dir(), holdersFile)
	if _, err := os.Stat(src); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			out.HoldersBackup = holdersFailed
		}
		return nil
	}
	now := p.clock().UTC()
	statePath := filepath.Join(p.dir(), holdersBackupFile)
	var last backupState
	haveLast := false
	if data, err := os.ReadFile(statePath); err == nil && json.Unmarshal(data, &last) == nil && !last.UploadedAt.IsZero() {
		haveLast = true
	}
	if haveLast && !force {
		if age := now.Sub(last.UploadedAt); age >= 0 && age < backupEvery {
			out.HoldersBackup = backupRecent
			return nil
		}
	}
	gzPath := filepath.Join(p.dir(), holdersGzFile)
	defer os.Remove(gzPath)
	n, err := gzipHolders(src, gzPath)
	if err != nil {
		out.HoldersBackup = holdersFailed
		p.log("upload: WARNING holders backup failed: the file cannot be read")
		return nil
	}
	if haveLast && n*2 < last.Records {
		out.HoldersBackup = backupRefused
		p.log("upload: WARNING holders backup refused: the file has %d rows, the last copy %d", n, last.Records)
		return nil
	}
	_, status, err := put(ctx, HoldersKey, gzPath, publish.PutOptions{Force: force})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out.HoldersBackup = holdersFailed
		p.log("upload: WARNING holders backup failed: R2 did not take the file")
		return nil
	}
	out.HoldersBackup = backupUploaded
	if status == publish.PutUnchanged {
		out.HoldersBackup = backupUnchanged
	}
	data, _ := json.Marshal(backupState{UploadedAt: now, Records: n})
	if err := writeFile(statePath, data); err != nil {
		p.log("upload: WARNING holders backup state not saved")
	}
	p.log("upload: holders backup %s (%d rows)", out.HoldersBackup, n)
	return nil
}

// gzipHolders writes src (plain CSV) gzipped to dst through a .tmp file and returns its row count.
// The gzip header holds no time, so the same rows give the same bytes (an identical object is not
// sent again).
func gzipHolders(src, dst string) (int, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	n := 0
	var recs []record
	if err := readHolderRows(bufio.NewReader(in), func(s string) string { return s }, func(r record) { recs = append(recs, r); n++ }); err != nil {
		return 0, err
	}
	// Only rows that parse are sent, in the file's order.
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	gz := gzip.NewWriter(f)
	w := csv.NewWriter(gz)
	w.Write(holderHeader)
	for _, r := range recs {
		w.Write(holderRowOf(r))
	}
	w.Flush()
	if err := errors.Join(w.Error(), gz.Close(), f.Close()); err != nil {
		return 0, err
	}
	return n, os.Rename(tmp, dst)
}

// backup copies the records file to R2 when there is none yet or the last copy is at least 24 hours
// old (force skips that wait). A file with less than half the records of the last copy is always
// refused, also with force: pruning does not shrink it that much, damage does. To accept a smaller
// file on purpose, delete backup.json.
func (p *Pipeline) backup(ctx context.Context, put uploadFunc, force bool, out *UploadOutput) error {
	base := filepath.Join(p.dir(), recordsFile)
	if _, err := os.Stat(base); errors.Is(err, os.ErrNotExist) {
		out.Backup = backupNoFile
		p.log("upload: records backup skipped (%s)", backupNoFile)
		return nil
	} else if err != nil {
		return err
	}
	last, haveLast := p.readBackup()
	now := p.clock().UTC()
	if haveLast && !force {
		if age := now.Sub(last.UploadedAt); age >= 0 && age < backupEvery {
			out.Backup, out.BackupRecords = backupRecent, last.Records
			return nil
		}
	}
	n, err := countBase(base)
	if err != nil {
		// Never copy a damaged file over the good one; the next export removes it and a lookup restores it.
		out.Backup = backupRefused
		p.log("upload: WARNING records backup refused: the records file cannot be read")
		return nil
	}
	if haveLast && n*2 < last.Records {
		out.Backup, out.BackupRecords = backupRefused, n
		p.log("upload: WARNING records backup refused: the file has %d records, the last copy %d", n, last.Records)
		return nil
	}
	_, status, err := put(ctx, RecordsKey, base, publish.PutOptions{Force: force})
	if err != nil {
		return err
	}
	out.Backup = backupUploaded
	if status == publish.PutUnchanged {
		out.Backup = backupUnchanged
	}
	out.BackupRecords = n
	data, _ := json.Marshal(backupState{UploadedAt: now, Records: n})
	if err := writeFile(filepath.Join(p.dir(), backupFile), data); err != nil {
		return err
	}
	p.log("upload: records backup %s (%d records)", out.Backup, n)
	return nil
}
