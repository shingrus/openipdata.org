package rdns

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
	"openipdata.org/hatchet/outputs"
)

// sample-names: customer PTR names in the gap. The gap is every /24 that our published
// ip2geo MMDB gives a country but no city (about 69% of IPv4). Each run walks a fixed shuffled
// order of all 2^24 /24 indexes from a saved cursor (sample-state.json), takes the next
// sampleNetworks /24s that are in the gap, looks up three hosts of each (the .1 and two random
// ones) with the resolver of resolve-names, and records the /24s that have at least one name
// in public.customer_name_networks (the hosts and their names; nothing goes to ip_ptr_names).
// learn-rdns-rules learns from the names (the country of a name is the row's), and locate turns a
// /24 whose names decode to one city into a row of scope named (see named.go).
//
// One pass over the 2^24 indexes is a cycle; the next one starts 180 days after the last began, so
// every /24 is sampled about every 180 days. The state file only holds the cursor and two times:
// losing it restarts the cycle from 0, which is harmless (some /24s are sampled again early).
const (
	sampleNetworks = 2000 // gap /24s per run
	sampleMaxSteps = 100000
	sampleMinGap   = 25 * time.Minute // a run less than this after the last one does nothing
	cycleLength    = 180 * 24 * time.Hour
	// customerWindow is how old a row may be for locate and the learner to use it, and when
	// sample-names deletes it: longer than a cycle, so a /24 stays until its next sample.
	customerWindow = "200 days"
	sampleBatch    = 160 // /24s looked up and recorded together
	// sampleBudget is how long the lookups may take; the task's own timeout is 30 minutes. A run
	// that reaches it records the /24s it finished and stops, it does not fail.
	sampleBudget = 25 * time.Minute

	walkSize  = 1 << 24 // the /24 indexes
	walkMul   = 0x9E3779
	walkAdd   = 0x5A5A5B
	stateFile = "sample-state.json"

	gapsFile     = "gaps.txt" // the output of mmdbbuilder gaps; its MMDB's ETag is in gapsFile + mmdbETagSuffix
	gapsTimeout  = 10 * time.Minute
	reasonGaps   = "mmdbbuilder gaps failed"
	reasonNoGaps = "no gap /24s"
)

// SampleOutput counts a sample-names run (counts only: no IP, no name).
type SampleOutput struct {
	// Skipped is "recent" (the last run was less than 25 minutes ago) or "cycle done" (every /24 was
	// walked and the next cycle starts 180 days after the last began); every count is then zero.
	Skipped string `json:"skipped,omitempty"`
	// MMDB is "used", "saved copy" or "skipped" (with MMDBReason: a fixed text); when it is skipped
	// every count is zero.
	MMDB       string `json:"mmdb,omitempty"`
	MMDBReason string `json:"mmdb_reason,omitempty"`
	GapsBuilt  bool   `json:"gaps_built,omitempty"` // mmdbbuilder gaps ran (the MMDB's ETag had changed)

	Gaps          int  `json:"gaps"`                 // /24s in the gap
	Steps         int  `json:"steps"`                // indexes of the walk looked at
	Picked        int  `json:"picked"`               // gap /24s the walk found
	Queried       int  `json:"queried"`              // hosts looked up
	OK            int  `json:"ok"`                   // got a name
	NotFound      int  `json:"not_found"`            // no PTR record
	Errors        int  `json:"errors"`               // failed lookups
	DoH           int  `json:"doh"`                  // of the lookups, answered through the DoH fallback
	Networks      int  `json:"networks"`             // /24s done (the cursor moved past them)
	NamedNetworks int  `json:"named_networks"`       // of them, with at least one name: rows of customer_name_networks
	Pruned        int  `json:"pruned"`               // rows older than 200 days deleted
	CycleDone     bool `json:"cycle_done,omitempty"` // the walk reached the end of the 2^24 indexes

	Stopped string `json:"stopped,omitempty"` // "time": the budget ran out before every picked /24 was done
}

// sampleState is the content of sample-state.json in the rdns dir.
type sampleState struct {
	Cursor       int       `json:"cursor"`        // position in the walk; walkSize or more: the cycle is done
	CycleStarted time.Time `json:"cycle_started"` // when position 0 was taken
	LastRun      time.Time `json:"last_run"`
}

func (p *Pipeline) statePath() string { return filepath.Join(p.dir(), stateFile) }

// readSampleState reads the state file; a missing or damaged one is a new cycle from 0.
func readSampleState(path string, now time.Time) sampleState {
	var s sampleState
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &s) != nil || s.Cursor < 0 || s.CycleStarted.IsZero() {
		return sampleState{CycleStarted: now}
	}
	return s
}

// writeSampleState writes the state file through a temporary file.
func writeSampleState(path string, s sampleState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
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

// walkAt is the /24 index (address >> 8) at position i of the walk: a bijection on 0..2^24-1
// ((i*A + C) mod 2^24 with A odd), so the walk visits every /24 once and spreads neighbours apart.
func walkAt(i uint32) uint32 { return (i*walkMul + walkAdd) & (walkSize - 1) }

// gapRun is a run of consecutive gap /24s of one country.
type gapRun struct {
	first   uint32 // the first /24, as address >> 8
	count   uint32
	cum     uint32 // /24s in the runs before this one
	country [2]byte
}

// gapList is the parsed output of mmdbbuilder gaps.
type gapList struct {
	runs  []gapRun
	total int
}

// parseGaps reads the lines "<a.b.c.0/24> <count> <country>" of mmdbbuilder gaps. A line that
// is not one, or whose run would pass the end of IPv4, is skipped.
func parseGaps(data []byte) gapList {
	var g gapList
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		f := strings.Fields(scanner.Text())
		if len(f) != 3 || len(f[2]) != 2 {
			continue
		}
		p, err := netip.ParsePrefix(f[0])
		n, err2 := strconv.ParseUint(f[1], 10, 32)
		if err != nil || err2 != nil || n == 0 || p.Bits() != 24 || !p.Addr().Is4() || p.Addr() != p.Masked().Addr() {
			continue
		}
		b := p.Addr().As4()
		first := uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
		if uint64(first)+n > 1<<24 {
			continue
		}
		g.runs = append(g.runs, gapRun{first: first, count: uint32(n), cum: uint32(g.total), country: [2]byte{f[2][0], f[2][1]}})
		g.total += int(n)
	}
	return g
}

// contains returns the country of the gap run that holds the /24 index idx (address >> 8).
func (g gapList) contains(idx uint32) (string, bool) {
	r := sort.Search(len(g.runs), func(k int) bool { return g.runs[k].first+g.runs[k].count > idx })
	if r == len(g.runs) || g.runs[r].first > idx {
		return "", false
	}
	return strings.ToUpper(string(g.runs[r].country[:])), true
}

// sampled is a gap /24 found by the walk with its hosts and its position in the walk.
type sampled struct {
	Prefix  netip.Prefix
	Country string
	Hosts   [3]netip.Addr
	Pos     int
}

// walkGap walks from position cursor, takes the /24s that are in the gap until it has want of them or
// after maxSteps indexes, and returns them with the position after the last index looked at.
func walkGap(g gapList, cursor, want, maxSteps int, intn func(int) int) (picked []sampled, end int) {
	i := cursor
	for steps := 0; i < walkSize && steps < maxSteps && len(picked) < want; steps++ {
		idx := walkAt(uint32(i))
		if c, ok := g.contains(idx); ok {
			a := netip.AddrFrom4([4]byte{byte(idx >> 16), byte(idx >> 8), byte(idx), 0})
			p := netip.PrefixFrom(a, 24)
			hosts := sampleHosts(p, intn)
			if ipdata.PublicV4(hosts[0]) { // a special range the RIR data lists: nothing to name
				picked = append(picked, sampled{p, c, hosts, i})
			}
		}
		i++
	}
	return picked, i
}

// sampleHosts returns the three hosts of a /24 that are named: the .1 and two distinct hosts from
// .2 to .254 chosen with intn (never .0 or .255).
func sampleHosts(p netip.Prefix, intn func(int) int) [3]netip.Addr {
	b := p.Masked().Addr().As4()
	host := func(last int) netip.Addr { return netip.AddrFrom4([4]byte{b[0], b[1], b[2], byte(last)}) }
	a := 2 + intn(253) // 2..254
	c := 2 + intn(252) // one of the other 252 values
	if c >= a {
		c++
	}
	return [3]netip.Addr{host(1), host(a), host(c)}
}

// gapsPath and gapsETagPath are the gap list and the ETag of the MMDB it was made from.
func (p *Pipeline) gapsPath() string { return filepath.Join(p.dir(), gapsFile) }

// runGaps is the default gaps function: the mmdbbuilder next to the worker.
func (p *Pipeline) runGaps(ctx context.Context, db, outFile string) error {
	if p.MMDBBuilder == "" {
		return errors.New("mmdbbuilder: binary path unknown")
	}
	ctx, cancel := context.WithTimeout(ctx, gapsTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.MMDBBuilder, "gaps", "-db", db, "-out", outFile)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mmdbbuilder gaps: %w: %s", err, scrubIPs(outputs.LastLines(stderr.String(), 3)))
	}
	return nil
}

// ensureGaps makes the gap list match the MMDB of ref. It is made again only when the ETag of
// the MMDB that is read differs from the one saved beside the list. The result is skipped (a
// fixed reason) when there is no usable MMDB or mmdbbuilder gaps fails; the old list stays.
func (p *Pipeline) ensureGaps(ctx context.Context, ref mmdbRef) (res mmdbResult, built bool) {
	if !ref.usable() {
		return skipped(ref.Reason), false
	}
	path := p.gapsPath()
	if etag := savedMMDBETag(path); etag != "" && ref.ETag != "" && etag == ref.ETag {
		res = mmdbResult{Status: statusUsed, ETag: etag}
		if ref.Saved {
			res.Status, res.Reason = statusSavedCopy, ref.Reason
		}
		return res, false
	}
	run := p.gaps
	if run == nil {
		if p.MMDBBuilder == "" {
			p.log(skipLogFormat, whoSample, reasonNoBuilder, "no path")
			return skipped(reasonNoBuilder), false
		}
		run = p.runGaps
	}
	db, res := p.useMMDB(ctx, ref, whoSample)
	if res.Status == statusSkipped {
		return res, false
	}
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		p.log(skipLogFormat, whoSample, reasonGaps, err)
		return skipped(reasonGaps), false
	}
	if err := run(ctx, db, path); err != nil {
		p.log(skipLogFormat, whoSample, reasonGaps, scrubIPs(err.Error()))
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			p.dropMMDBCopy() // the copy may be what is wrong
		}
		return skipped(reasonGaps), false
	}
	// The ETag after the list: a crash in between only costs one more run of mmdbbuilder gaps.
	tmp := path + mmdbETagSuffix + ".tmp"
	err := os.WriteFile(tmp, []byte(res.ETag), 0o640)
	if err == nil {
		err = os.Rename(tmp, path+mmdbETagSuffix)
	}
	if err != nil {
		os.Remove(tmp)
		os.Remove(path + mmdbETagSuffix)
		p.log("sample-names: could not save the ETag of %s: %v", gapsFile, err)
	}
	return res, true
}

// upsertSampledSQL records one sampled /24 with the hosts that have a name ($2 country, $3 hosts, $4 names).
const upsertSampledSQL = `INSERT INTO public.customer_name_networks (prefix, country, hosts, names, sampled_at)
	VALUES ($1::text::cidr, $2, $3::text[]::inet[], $4::text[], now())
	ON CONFLICT (prefix) DO UPDATE SET country = excluded.country, hosts = excluded.hosts,
		names = excluded.names, sampled_at = excluded.sampled_at`

// SampleNames names customer addresses in the gap (see the top of this file): it does nothing when
// the last run was less than 25 minutes ago or the cycle is done and the next is not due; otherwise it makes
// sure the gap list matches the current ip2geo MMDB, walks on from the saved cursor to up to
// sampleNetworks gap /24s, looks up three hosts of each with the resolver of resolve-names (its
// lookups only: nothing goes to ip_ptr_names) and records the /24s that have a name in
// public.customer_name_networks. The cursor moves only past /24s that were done. A DNS problem never
// fails the task; only a database error or a stop does. With no usable MMDB the task reports it as
// skipped and succeeds.
func (p *Pipeline) SampleNames(ctx context.Context) (SampleOutput, error) {
	var out SampleOutput
	now := p.clock().UTC()
	statePath := p.statePath()
	state := readSampleState(statePath, now)
	if !state.LastRun.IsZero() && now.Sub(state.LastRun) >= 0 && now.Sub(state.LastRun) < sampleMinGap {
		out.Skipped = "recent"
		p.log("sample-names: skipped (the last run was less than %s ago)", sampleMinGap)
		return out, nil
	}
	if state.Cursor >= walkSize {
		if now.Before(state.CycleStarted.Add(cycleLength)) {
			out.Skipped = "cycle done"
			p.log("sample-names: skipped (cycle done, the next starts %s)", state.CycleStarted.Add(cycleLength).Format("2006-01-02"))
			return out, nil
		}
		state = sampleState{CycleStarted: now}
	}
	conn, err := connect(ctx, p.PGSQL)
	if err != nil {
		return out, err
	}
	defer conn.Close(ctx)

	ref := p.statMMDBFor(ctx, whoSample)
	res, built := p.ensureGaps(ctx, ref)
	out.MMDB, out.MMDBReason, out.GapsBuilt = res.Status, res.Reason, built
	if res.Status == statusSkipped {
		p.log("sample-names: skipped (%s)", res.Reason)
		return out, nil
	}
	data, err := os.ReadFile(p.gapsPath())
	if err != nil {
		out.MMDB, out.MMDBReason = statusSkipped, reasonGaps
		p.log(skipLogFormat, whoSample, reasonGaps, "no gap list")
		return out, nil
	}
	gap := parseGaps(data)
	out.Gaps = gap.total
	if gap.total == 0 {
		out.MMDB, out.MMDBReason = statusSkipped, reasonNoGaps
		p.log("sample-names: skipped (%s)", reasonNoGaps)
		return out, nil
	}

	steps := sampleMaxSteps
	if p.walkSteps > 0 {
		steps = p.walkSteps
	}
	picked, end := walkGap(gap, state.Cursor, sampleNetworks, steps, p.randInt)
	out.Picked, out.Steps = len(picked), end-state.Cursor
	state.LastRun = now
	save := func(cursor int) {
		state.Cursor = cursor
		if err := writeSampleState(statePath, state); err != nil {
			p.log("sample-names: could not save %s: %v", stateFile, err)
		}
	}

	resolver := p.resolver
	if resolver == nil {
		resolver = newPTRResolver(p.PTRResolver)
	}
	rctx, cancel := context.WithTimeout(ctx, sampleBudget)
	defer cancel()
	done := true
	for start := 0; start < len(picked); start += sampleBatch {
		batch := picked[start:min(start+sampleBatch, len(picked))]
		if rctx.Err() != nil {
			done = false
			break
		}
		ok, err := p.sampleBatch(ctx, rctx, conn, resolver, batch, &out)
		if err != nil {
			return out, err
		}
		if !ok {
			done = false
			break
		}
		save(batch[len(batch)-1].Pos + 1) // moves only past /24s that are recorded
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if done {
		save(end)
		out.CycleDone = end >= walkSize
	} else {
		out.Stopped = "time"
	}

	tag, err := conn.Exec(ctx, `DELETE FROM public.customer_name_networks WHERE sampled_at < now() - $1::text::interval`, customerWindow)
	if err != nil {
		return out, dbError("prune customer name networks", err)
	}
	out.Pruned = int(tag.RowsAffected())
	p.log("sample-names: %d gap /24s, walked %d indexes, %d found; %d hosts looked up: %d names, %d not found, %d errors, %d through DoH; %d /24s done, %d recorded; %d old removed; cycle done=%v; stopped=%q",
		out.Gaps, out.Steps, out.Picked, out.Queried, out.OK, out.NotFound, out.Errors, out.DoH, out.Networks, out.NamedNetworks, out.Pruned, out.CycleDone, out.Stopped)
	return out, nil
}

// sampleBatch looks up the hosts of batch with the resolver (no store: the answers are only used here)
// and records the /24s that have at least one name, with those hosts and their names. ok is false when
// the time budget ran out during the lookups: the batch is then not recorded and the cursor does not move
// past it. A cancellation of ctx is an error.
func (p *Pipeline) sampleBatch(ctx, rctx context.Context, conn *pgx.Conn, resolver *ptrResolver, batch []sampled, out *SampleOutput) (ok bool, err error) {
	hosts := make([]netip.Addr, 0, len(batch)*3)
	for _, s := range batch {
		hosts = append(hosts, s.Hosts[:]...)
	}
	answers, doh := resolveAll(rctx, resolver, hosts, resolveWorkers)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if rctx.Err() != nil {
		return false, nil // answers cut short by the budget are not real errors
	}
	out.DoH += doh
	out.Queried += len(hosts)
	for _, a := range answers {
		switch a.Status {
		case statusOK:
			out.OK++
		case statusNotFound:
			out.NotFound++
		default:
			out.Errors++
		}
	}
	rows := &pgx.Batch{}
	named := 0
	for i, s := range batch {
		var ips, names []string
		for k, h := range s.Hosts {
			if a := answers[i*3+k]; a.Status == statusOK && a.PTR != "" {
				ips, names = append(ips, h.String()), append(names, a.PTR)
			}
		}
		if len(ips) == 0 {
			continue // no name: no row
		}
		rows.Queue(upsertSampledSQL, s.Prefix.String(), s.Country, ips, names)
		named++
	}
	if named > 0 {
		br := conn.SendBatch(ctx, rows)
		for i := 0; i < named; i++ {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return false, dbError("record customer name networks", err)
			}
		}
		if err := br.Close(); err != nil {
			return false, dbError("record customer name networks", err)
		}
	}
	out.Networks += len(batch)
	out.NamedNetworks += named
	return true, nil
}
