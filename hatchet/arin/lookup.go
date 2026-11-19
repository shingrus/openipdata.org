package arin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"openipdata.org/hatchet/download"
	"openipdata.org/hatchet/outputs"
	"openipdata.org/hatchet/rdns"
	"openipdata.org/hatchet/rirstats"
)

// LookupOutput counts a lookup run (counts only: no IP, no name).
type LookupOutput struct {
	// Skipped is "no ARIN stats", "cycle done" (every /24 was walked; the next cycle starts 180
	// days after the last began), "no records file and R2 cannot be read", "busy" (another task or
	// run-arin holds the directory) or a fixed reason the MMDB or the gap list is not usable; every
	// count is then zero.
	Skipped    string `json:"skipped,omitempty"`
	MMDB       string `json:"mmdb,omitempty"` // "used", "saved copy" or "skipped"
	MMDBReason string `json:"mmdb_reason,omitempty"`
	GapsBuilt  bool   `json:"gaps_built,omitempty"` // mmdbbuilder gaps ran (the MMDB's ETag had changed)
	Stats      string `json:"stats,omitempty"`      // downloaded, cached, recent or saved copy
	Restored   bool   `json:"restored,omitempty"`   // the records file was lost and fetched from R2
	// HoldersRestore is "restored" or "failed" when holders.csv was missing and R2 was asked.
	HoldersRestore string `json:"holders_restore,omitempty"`

	ARINRanges int `json:"arin_ranges"` // IPv4 ranges of ARIN's stats file (merged)
	Gaps       int `json:"gaps"`        // /24s in the gap
	Steps      int `json:"steps"`       // positions of the walk looked at
	Targets    int `json:"targets"`     // gap /24s in ARIN's space (lookups wanted)
	Batches    int `json:"batches"`     // batches stored

	Requests    int `json:"requests"`     // requests started
	Retries     int `json:"retries"`      // tries after a failed one
	RateLimited int `json:"rate_limited"` // answers of 429
	Found       int `json:"found"`        // a record of at most a /16
	WithCity    int `json:"with_city"`    // of them, with a usable city
	Wide        int `json:"wide"`         // a record wider than a /16 (not stored)
	NotARIN     int `json:"not_arin"`     // a redirect
	NotFound    int `json:"not_found"`    // 404
	Bad         int `json:"bad"`          // an answer that could not be used
	Failed      int `json:"failed"`       // no answer after the tries
	Stored      int `json:"stored"`       // distinct records written

	HoldersStored int `json:"holders_stored"` // rows appended to holders.csv
	HoldersEdu    int `json:"holders_edu"`    // of them, class edu
	HoldersGov    int `json:"holders_gov"`    // class gov
	HoldersMil    int `json:"holders_mil"`    // class mil
	HoldersOther  int `json:"holders_other"`  // class other
	// HoldersSkipped counts holder rows not appended because the restore of holders.csv failed.
	HoldersSkipped int `json:"holders_skipped"`

	CycleDone bool   `json:"cycle_done,omitempty"` // the walk reached the end
	Stopped   string `json:"stopped,omitempty"`    // "time", "rate limited" or "failing"
}

type gapsFunc func(ctx context.Context, db, outFile string) error

// arinRanges are ARIN's allocated and assigned IPv4 ranges, sorted and merged: [start, end] pairs.
type arinRanges [][2]uint32

// readARINRanges parses the stats file.
func readARINRanges(path string) (arinRanges, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r arinRanges
	err = rirstats.Parse(f, func(_ string, prefixes []netip.Prefix) {
		for _, p := range prefixes {
			if p.Addr().Is4() {
				r = append(r, [2]uint32{u32(p.Addr()), u32(rirstats.LastAddr(p))})
			}
		}
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(r, func(i, j int) bool { return r[i][0] < r[j][0] })
	merged := r[:0]
	for _, x := range r {
		if n := len(merged); n > 0 && uint64(x[0]) <= uint64(merged[n-1][1])+1 {
			merged[n-1][1] = max(merged[n-1][1], x[1])
		} else {
			merged = append(merged, x)
		}
	}
	return merged, nil
}

// containsBlock reports whether [start, end] lies inside one range.
func (r arinRanges) containsBlock(start, end uint32) bool {
	i := sort.Search(len(r), func(k int) bool { return r[k][1] >= start })
	return i < len(r) && r[i][0] <= start && r[i][1] >= end
}

// target is a gap /24 to look up: the address asked and its position in the walk.
type target struct {
	Addr netip.Addr
	Pos  int
}

// hostOf is the host of a /24 that is asked, chosen from the /24 index only (1 to 254, never .0
// or .255), so a rerun asks the same address.
func hostOf(idx uint32) byte {
	h := uint64(idx) * 0x9E3779B97F4A7C15
	return byte((h>>32)%254) + 1
}

// walk looks at positions from cursor on and takes the /24s that are in the gap, inside ARIN's
// space and public, until it has want of them or after maxSteps positions. end is the position
// after the last one looked at.
func walk(gap rdns.GapList, ranges arinRanges, at func(uint32) uint32, public func(netip.Addr) bool, size, cursor, want, maxSteps int) (picked []target, end int) {
	i := cursor
	for steps := 0; i < size && steps < maxSteps && len(picked) < want; steps++ {
		idx := at(uint32(i))
		if _, ok := gap.Contains(idx); ok && ranges.containsBlock(idx<<8, idx<<8|255) {
			addr := addrOf(idx<<8 | uint32(hostOf(idx)))
			if public(addr) {
				picked = append(picked, target{addr, i})
			}
		}
		i++
	}
	return picked, i
}

// fetchGated downloads rawURL to path unless the file was asked for less than gate ago (the
// stamp beside it) or the server says it is current. A failed download uses the saved file.
// The status is downloaded, cached, recent or saved copy; with no file at all the error is
// returned.
func (p *Pipeline) fetchGated(ctx context.Context, rawURL, path string, gate time.Duration) (string, error) {
	stamp := path + checkedSuffix
	now := p.clock()
	_, fileErr := os.Stat(path)
	if fileErr == nil {
		if si, err := os.Stat(stamp); err == nil && now.Sub(si.ModTime()) < gate && si.ModTime().Before(now.Add(time.Minute)) {
			return "recent", nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", err
	}
	status, err := download.File(ctx, p.downloadClient(), rawURL, path)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if fileErr == nil {
			p.log("WARNING download of %s failed: %s; using the saved copy", filepath.Base(path), rdns.ScrubIPs(err.Error()))
			return "saved copy", nil
		}
		return "", err
	}
	if f, err := os.OpenFile(stamp, os.O_CREATE|os.O_WRONLY, 0o640); err == nil {
		f.Close()
		os.Chtimes(stamp, now, now)
	}
	return status, nil
}

// ensureGaps makes the gap list match the MMDB. It is made again only when the ETag of the MMDB
// differs from the one saved beside the list. A fixed reason is returned when it cannot be made.
func (p *Pipeline) ensureGaps(ctx context.Context, mm rdns.MMDBState, mmcopy *rdns.MMDBCopy) (built bool, reason string) {
	path := filepath.Join(p.dir(), gapsFile)
	if _, err := os.Stat(path); err == nil && mm.ETag != "" && savedETag(path) == mm.ETag {
		return false, ""
	}
	run := p.gaps
	if run == nil {
		if p.MMDBBuilder == "" {
			p.log("lookup: WARNING gap list not made (%s)", reasonNoBuild)
			return false, reasonNoBuild
		}
		run = p.runGaps
	}
	tmp := path + ".tmp"
	if err := run(ctx, mm.Path, tmp); err != nil {
		os.Remove(tmp)
		p.log("lookup: WARNING gap list not made: %s", rdns.ScrubIPs(err.Error()))
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			mmcopy.Drop() // the copy may be what is wrong
		}
		return false, reasonGaps
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, reasonGaps
	}
	// The ETag after the list: a crash in between only costs one more run of mmdbbuilder gaps.
	etag := path + etagSuffix
	err := os.WriteFile(etag+".tmp", []byte(mm.ETag), 0o640)
	if err == nil {
		err = os.Rename(etag+".tmp", etag)
	}
	if err != nil {
		os.Remove(etag + ".tmp")
		os.Remove(etag)
		p.log("lookup: could not save the ETag of %s", gapsFile)
	}
	return true, ""
}

func savedETag(path string) string {
	data, err := os.ReadFile(path + etagSuffix)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// runGaps is the default gaps function: the mmdbbuilder next to the worker.
func (p *Pipeline) runGaps(ctx context.Context, db, outFile string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.MMDBBuilder, "gaps", "-db", db, "-out", outFile)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mmdbbuilder gaps: %w: %s", err, rdns.ScrubIPs(outputs.LastLines(stderr.String(), 3)))
	}
	return nil
}

// Lookup walks the /24s on from the saved position and asks ARIN's RDAP service about one address
// of each gap /24 inside ARIN's space (see the package comment), a batch at a time: the records of
// a batch and the new position are stored together, so the position moves only past stored
// batches. It stops when the cycle ends, the soft budget is used, or ARIN rate limits or fails
// (the unfinished batch is then not stored). Only a file error or a cancellation fails the
// task, never a problem of ARIN, the MMDB or the files.
func (p *Pipeline) Lookup(ctx context.Context) (LookupOutput, error) {
	var out LookupOutput
	// The budget counts from the start of the task: the downloads and mmdbbuilder gaps take
	// time too, and the task's own timeout is only a little above the budget.
	budget := p.budget
	if budget == 0 {
		budget = lookupBudget
	}
	deadline := time.Now().Add(budget)
	unlock, ok, err := p.lock()
	if err != nil {
		return out, err
	}
	if !ok {
		out.Skipped = skipBusy
		p.log("lookup: skipped (another task or run-arin is working in the directory)")
		return out, nil
	}
	defer unlock()
	st := p.newStore()
	size := rdns.WalkSize
	if p.size > 0 {
		size = p.size
	}
	now := p.clock().UTC()
	cursor, started, restored, err := st.Progress(ctx, now)
	if errors.Is(err, errNoRecords) {
		// The records are lost and R2 cannot say whether it has a copy: the walk must not start
		// again from 0 only because R2 was down. Nothing is written.
		out.Skipped = skipNoRecords
		p.log("lookup: WARNING skipped (%s); nothing is asked until R2 answers", skipNoRecords)
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Restored = restored
	// Best effort and independent of the records: a problem only shows in the output.
	out.HoldersRestore = p.restoreHolders(ctx)
	if cursor >= size {
		if now.Before(started.Add(cycleLength)) {
			out.Skipped = skipCycleDone
			p.log("lookup: skipped (cycle done, the next starts %s)", started.Add(cycleLength).Format("2006-01-02"))
			return out, nil
		}
		cursor, started = 0, now
	}
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return out, err
	}

	// ARIN's address space.
	statsPath := filepath.Join(p.dir(), statsFile)
	url := p.statsURL
	if url == "" {
		url = statsURL
	}
	if out.Stats, err = p.fetchGated(ctx, url, statsPath, statsGate); err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		out.Skipped = skipNoStats
		p.log("lookup: skipped (%s)", skipNoStats)
		return out, nil
	}
	ranges, err := readARINRanges(statsPath)
	if err != nil || len(ranges) == 0 {
		os.Remove(statsPath) // a damaged file must not stay
		os.Remove(statsPath + checkedSuffix)
		out.Skipped = skipNoStats
		p.log("lookup: skipped (%s)", skipNoStats)
		return out, nil
	}
	out.ARINRanges = len(ranges)

	// Our MMDB and its gap list.
	mmcopy := &rdns.MMDBCopy{Dir: p.dir(), R2: p.R2, Progress: p.Progress, Store: p.mmdbStore, Now: p.now}
	mm := mmcopy.Prepare(ctx, "lookup")
	out.MMDB, out.MMDBReason = mm.Status, mm.Reason
	if !mm.Usable() {
		out.Skipped = "MMDB: " + mm.Reason
		p.log("lookup: skipped (%s)", out.Skipped)
		return out, nil
	}
	if out.GapsBuilt, out.Skipped = p.ensureGaps(ctx, mm, mmcopy); out.Skipped != "" {
		return out, nil
	}
	data, err := os.ReadFile(filepath.Join(p.dir(), gapsFile))
	if err != nil {
		out.Skipped = reasonGaps
		return out, nil
	}
	gap := rdns.ParseGaps(data)
	out.Gaps = gap.Total()
	if out.Gaps == 0 {
		out.Skipped = reasonNoGaps
		p.log("lookup: skipped (%s)", reasonNoGaps)
		return out, nil
	}

	if err := p.walkAndLook(ctx, st, gap, ranges, cursor, started, size, deadline, &out); err != nil {
		return out, err
	}
	return out, ctx.Err()
}
