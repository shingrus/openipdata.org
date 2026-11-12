package rdns

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"openipdata.org/hatchet/outputs"
	"openipdata.org/hatchet/publish"
)

// The MMDB fallback: a /24 that no rule matches takes the city of one of its
// router hops from our own published ip2geo MMDB (build-geo-data's output). locate
// keeps its own copy of the newest one from R2, never build-geo-data's files on
// the VM, and asks the mmdbbuilder binary next to the worker to read it.
const (
	mmdbFile       = "ip2geo.mmdb" // the saved copy, in the rdns dir; the ETag of the object is in mmdbFile + mmdbETagSuffix
	mmdbETagSuffix = ".etag"
	hopsInFile     = "mmdb-hops.txt" // the router IPs to look up, one per line; removed after the run
	hopsOutFile    = "mmdb-hops.tsv" // mmdbbuilder lookup's answer: ip, country, city; removed after the run

	// The key shape build-geo-data publishes under: <YYYY-MM-DD>/ip2geo-openipdata-<YYYYMMDD>.mmdb.
	mmdbKeyFormat    = "%s/ip2geo-openipdata-%s.mmdb"
	mmdbFolderLayout = "2006-01-02"
	mmdbTagLayout    = "20060102"

	// lookupTimeout bounds the mmdbbuilder lookup process.
	lookupTimeout = 5 * time.Minute

	// noMMDB is the ETag in the fingerprint of a run that has no MMDB.
	noMMDB = "none"
)

// Why the fallback was skipped (LocateOutput.MMDBReason): fixed texts, never an error's text.
const (
	reasonNoR2      = "R2 is not set"
	reasonBadR2     = "R2 setting is invalid"
	reasonStat      = "R2 stat failed"
	reasonNoObject  = "no MMDB in R2 for today or yesterday"
	reasonDownload  = "download failed"
	reasonNoBuilder = "mmdbbuilder path unknown"
	reasonLookup    = "mmdbbuilder lookup failed"
)

// LocateOutput.MMDB values.
const (
	statusUsed      = "used"       // the object of R2 (downloaded, or the saved copy of the same ETag)
	statusSavedCopy = "saved copy" // R2 could not be used, so the last saved copy was (MMDBReason says why)
	statusSkipped   = "skipped"    // no usable MMDB: the fallback was not applied
)

// skipLogFormat logs the reason and the detail of a skipped fallback; the others
// are for a run that falls back on the saved copy.
const (
	skipLogFormat      = "locate: WARNING MMDB hop fallback skipped (%s): %v"
	statLogFormat      = "locate: WARNING MMDB stat in R2 failed: %v"
	savedNoR2LogFormat = "locate: WARNING no usable MMDB in R2 (%s); using the saved copy"
	savedDownLogFormat = "locate: WARNING MMDB download failed: %v; using the saved copy"
)

// objectStore is the part of publish.R2 that locate uses. Tests replace it.
type objectStore interface {
	Stat(ctx context.Context, key string) (publish.ObjectInfo, error)
	DownloadIfChanged(ctx context.Context, key, path, etag string) (info publish.ObjectInfo, downloaded bool, err error)
}

// lookupFunc runs "mmdbbuilder lookup" on the MMDB at db for the IPs in inFile and
// writes ip<TAB>country<TAB>city rows to outFile. Tests replace it.
type lookupFunc func(ctx context.Context, db, inFile, outFile string) error

// mmdbRef is what the Stat at the start of locate found.
type mmdbRef struct {
	store objectStore
	Key   string
	// ETag is the object's, or with Saved the ETag of the saved copy; empty: no MMDB at all.
	ETag string
	// Reason is non-empty when R2 gave no object (and why). The saved copy, if it has an ETag, is
	// then the MMDB (Saved): an R2 outage must not change what locate builds.
	Reason string
	Saved  bool
}

// usable reports whether there is an MMDB to read this run.
func (r mmdbRef) usable() bool { return r.Reason == "" || r.Saved }

// etag is the ETag for the fingerprint: that of the MMDB locate reads, noMMDB when there is none.
func (r mmdbRef) etag() string {
	if !r.usable() || r.ETag == "" {
		return noMMDB
	}
	return r.ETag
}

// savedPath is the saved copy of the MMDB.
func (p *Pipeline) savedPath() string { return filepath.Join(p.dir(), mmdbFile) }

// mmdbKey is the key of the MMDB build-geo-data published on day.
func mmdbKey(day time.Time) string {
	day = day.UTC()
	return fmt.Sprintf(mmdbKeyFormat, day.Format(mmdbFolderLayout), day.Format(mmdbTagLayout))
}

// objects returns the R2 client: the test one, or one made from the R2 setting.
// Errors are fixed texts: the setting holds the secret key.
func (p *Pipeline) objects() (objectStore, string) {
	if p.store != nil {
		return p.store, ""
	}
	if strings.TrimSpace(p.R2) == "" {
		return nil, reasonNoR2
	}
	r2, err := publish.NewR2(p.R2)
	if err != nil {
		return nil, reasonBadR2 // publish.ErrInvalidR2; never the value
	}
	return r2, ""
}

// statMMDB looks for the MMDB of today (UTC), else of yesterday, in R2, reading only
// its headers. It never fails: a reference with a Reason means R2 gave no MMDB this
// run; it then is the saved copy when there is one with its ETag (Saved), else none.
func (p *Pipeline) statMMDB(ctx context.Context) mmdbRef {
	ref := p.statR2(ctx)
	if ref.Reason != "" {
		if etag := savedMMDBETag(p.savedPath()); etag != "" {
			ref.ETag, ref.Saved = etag, true
		}
	}
	return ref
}

// statR2 is statMMDB's look at R2.
func (p *Pipeline) statR2(ctx context.Context) mmdbRef {
	store, reason := p.objects()
	if store == nil {
		return mmdbRef{Reason: reason}
	}
	now := p.clock().UTC()
	for _, day := range []time.Time{now, now.AddDate(0, 0, -1)} {
		key := mmdbKey(day)
		info, err := store.Stat(ctx, key)
		if errors.Is(err, publish.ErrNotFound) {
			continue
		}
		if err != nil {
			p.log(statLogFormat, scrubIPs(err.Error()))
			return mmdbRef{Reason: reasonStat}
		}
		return mmdbRef{store: store, Key: key, ETag: info.ETag}
	}
	return mmdbRef{Reason: reasonNoObject}
}

// fetchMMDB makes <dir>/ip2geo.mmdb the object of ref: it is downloaded only when
// its ETag differs from the one saved beside the copy (or there is no copy). It
// returns the path and whether it downloaded.
func (p *Pipeline) fetchMMDB(ctx context.Context, ref mmdbRef) (string, bool, error) {
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return "", false, err
	}
	path := p.savedPath()
	info, downloaded, err := ref.store.DownloadIfChanged(ctx, ref.Key, path, savedMMDBETag(path))
	if err != nil {
		return "", false, err
	}
	if downloaded {
		// The ETag is written after the file: a crash in between only costs one more download.
		etagPath := path + mmdbETagSuffix
		err := os.WriteFile(etagPath+".tmp", []byte(info.ETag), 0o640)
		if err == nil {
			err = os.Rename(etagPath+".tmp", etagPath)
		}
		if err != nil {
			os.Remove(etagPath + ".tmp")
			os.Remove(etagPath) // the copy has no ETag: the next run downloads it again
			p.log("locate: could not save the ETag of %s: %v", mmdbFile, err)
		}
	}
	return path, downloaded, nil
}

// savedMMDBETag is the ETag saved for the copy at path, or "" when the copy or
// its ETag is missing (then the object is downloaded).
func savedMMDBETag(path string) string {
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	data, err := os.ReadFile(path + mmdbETagSuffix)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// dropMMDBCopy removes the saved copy and its ETag (a copy that cannot be read).
func (p *Pipeline) dropMMDBCopy() {
	path := filepath.Join(p.dir(), mmdbFile)
	os.Remove(path)
	os.Remove(path + mmdbETagSuffix)
}

// runLookup is the default lookupFunc: the mmdbbuilder next to the worker. Its
// stderr counts invalid lines and failed lookups, never the lines, but the text
// still goes through scrubIPs.
func (p *Pipeline) runLookup(ctx context.Context, db, inFile, outFile string) error {
	if p.MMDBBuilder == "" {
		return errors.New("mmdbbuilder: binary path unknown")
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.MMDBBuilder, "lookup", "-db", db, "-in", inFile, "-out", outFile)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mmdbbuilder lookup: %w: %s", err, scrubIPs(outputs.LastLines(stderr.String(), 3)))
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		p.log("locate: mmdbbuilder lookup: %s", scrubIPs(outputs.LastLines(msg, 3)))
	}
	return nil
}

// lookupHops asks the MMDB at db for the city of each IP: the IPs go to a run file
// in the rdns dir, mmdbbuilder lookup writes its answer to another, and both
// are removed afterwards. IPs without a city are not in the result.
func (p *Pipeline) lookupHops(ctx context.Context, db string, ips []netip.Addr) (map[netip.Addr]cityKey, error) {
	if len(ips) == 0 {
		return map[netip.Addr]cityKey{}, nil
	}
	inFile, outFile := filepath.Join(p.dir(), hopsInFile), filepath.Join(p.dir(), hopsOutFile)
	for _, f := range []string{inFile, inFile + ".tmp", outFile, outFile + ".tmp"} {
		defer os.Remove(f)
		os.Remove(f) // left by an earlier run that was killed
	}
	var list strings.Builder
	for _, ip := range ips {
		list.WriteString(ip.String())
		list.WriteByte('\n')
	}
	if err := os.WriteFile(inFile+".tmp", []byte(list.String()), 0o640); err != nil {
		return nil, fmt.Errorf("write %s: %w", hopsInFile, err)
	}
	if err := os.Rename(inFile+".tmp", inFile); err != nil {
		return nil, fmt.Errorf("write %s: %w", hopsInFile, err)
	}
	run := p.lookup
	if run == nil {
		run = p.runLookup
	}
	if err := run(ctx, db, inFile, outFile); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", hopsOutFile, err)
	}
	return parseHopCities(data), nil
}

// parseHopCities reads mmdbbuilder lookup's rows, ip<TAB>country<TAB>city. A row that
// is not an IP and two non-empty fields is skipped.
func parseHopCities(data []byte) map[netip.Addr]cityKey {
	cities := map[netip.Addr]cityKey{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		f := strings.Split(scanner.Text(), "\t")
		if len(f) != 3 {
			continue
		}
		ip, err := netip.ParseAddr(strings.TrimSpace(f[0]))
		country, city := strings.ToUpper(strings.TrimSpace(f[1])), strings.TrimSpace(f[2])
		if err != nil || len(country) != 2 || city == "" {
			continue
		}
		cities[ip.Unmap()] = cityKey{country, city}
	}
	return cities
}

// mmdbResult is what mmdbHops found.
type mmdbResult struct {
	Hops       map[netip.Addr]cityKey
	Status     string // statusUsed, statusSavedCopy or statusSkipped
	Reason     string // why R2 was not used or the fallback skipped; empty for statusUsed
	Downloaded bool
	// ETag is the ETag of the MMDB that was read (noMMDB when none), for the fingerprint to save.
	ETag string
}

func skipped(reason string) mmdbResult {
	return mmdbResult{Status: statusSkipped, Reason: reason, ETag: noMMDB}
}

// mmdbHops is the fallback step of Locate: the cities of ips are read from the MMDB
// of ref. Normally the saved copy is first brought up to date from R2. When R2 gave
// no object (ref.Saved) or the download fails, the saved copy that is there is read
// instead, so an outage changes nothing. Any other problem, or no saved copy, makes the
// fallback skipped. Problems are logged and returned as fixed reasons, never as errors:
// locate goes on without the fallback.
func (p *Pipeline) mmdbHops(ctx context.Context, ref mmdbRef, ips []netip.Addr) mmdbResult {
	if p.lookup == nil && p.MMDBBuilder == "" {
		p.log(skipLogFormat, reasonNoBuilder, "no path")
		return skipped(reasonNoBuilder)
	}
	res := mmdbResult{Status: statusUsed, ETag: ref.ETag}
	path := p.savedPath()
	if ref.Saved {
		p.log(savedNoR2LogFormat, ref.Reason)
		res.Status, res.Reason = statusSavedCopy, ref.Reason
	} else {
		var err error
		if path, res.Downloaded, err = p.fetchMMDB(ctx, ref); err != nil {
			saved := savedMMDBETag(p.savedPath())
			if saved == "" {
				p.log(skipLogFormat, reasonDownload, scrubIPs(err.Error()))
				return skipped(reasonDownload)
			}
			p.log(savedDownLogFormat, scrubIPs(err.Error()))
			path = p.savedPath()
			res.Status, res.Reason, res.ETag = statusSavedCopy, reasonDownload, saved
		}
	}
	hops, err := p.lookupHops(ctx, path, ips)
	if err != nil {
		p.log(skipLogFormat, reasonLookup, scrubIPs(err.Error()))
		// The copy may be what is wrong (mmdbbuilder ran and said so): fetch it again next time.
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			p.dropMMDBCopy()
		}
		return skipped(reasonLookup)
	}
	res.Hops = hops
	return res
}
