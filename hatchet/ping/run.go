package ping

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	"openipdata.org/hatchet/outputs"
	"openipdata.org/hatchet/publish"
)

const (
	labelKey   = "openipdata"
	labelValue = "hosting-ping"
	vmPrefix   = "hosting-ping-"

	machineType = "e2-micro"
	imageFamily = "projects/debian-cloud/global/images/family/debian-12"
	maxRunHours = 4 // the VM deletes itself after this long, even if the worker is gone

	presignExpiry = 6 * time.Hour
	minRegionsOK  = 20 // fewer regions with a result: the task fails
)

// timing holds the waits of the ping task. Tests shorten them.
type timing struct {
	Poll          time.Duration // between two looks at R2
	CreateRetry   time.Duration // before a VM that could not be created is tried again
	Lost          time.Duration // a created VM that is not in the list after this long is taken for lost
	RegionTimeout time.Duration // give up on a region this long after the start of the run
	ProgressEvery time.Duration // between two progress lines
	Cleanup       time.Duration // the budget of the final deletion of the VMs
}

var defaultTiming = timing{
	Poll: time.Minute, CreateRetry: 10 * time.Minute, Lost: 3 * time.Minute,
	RegionTimeout: 3 * time.Hour, ProgressEvery: 5 * time.Minute, Cleanup: 3 * time.Minute,
}

// instanceSpec is the body of the instances.insert request.
type instanceSpec struct {
	Name              string            `json:"name"`
	MachineType       string            `json:"machineType"`
	Labels            map[string]string `json:"labels"`
	Disks             []diskSpec        `json:"disks"`
	NetworkInterfaces []nicSpec         `json:"networkInterfaces"`
	// ServiceAccounts is sent empty on purpose (no omitempty): the VM gets no Google permissions.
	ServiceAccounts []struct{}     `json:"serviceAccounts"`
	Scheduling      schedulingSpec `json:"scheduling"`
	Metadata        metadataSpec   `json:"metadata"`
}

type diskSpec struct {
	Boot             bool `json:"boot"`
	AutoDelete       bool `json:"autoDelete"`
	InitializeParams struct {
		SourceImage string `json:"sourceImage"`
	} `json:"initializeParams"`
}

type nicSpec struct {
	Network       string `json:"network"`
	AccessConfigs []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"accessConfigs"`
}

type schedulingSpec struct {
	MaxRunDuration struct {
		Seconds string `json:"seconds"`
	} `json:"maxRunDuration"`
	InstanceTerminationAction string `json:"instanceTerminationAction"`
}

type metadataSpec struct {
	Items []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"items"`
}

// newInstanceSpec describes the VM of one region.
func newInstanceSpec(zone, name, script string) instanceSpec {
	s := instanceSpec{
		Name:            name,
		MachineType:     "zones/" + zone + "/machineTypes/" + machineType,
		Labels:          map[string]string{labelKey: labelValue},
		ServiceAccounts: []struct{}{},
	}
	var d diskSpec
	d.Boot, d.AutoDelete = true, true
	d.InitializeParams.SourceImage = imageFamily
	s.Disks = []diskSpec{d}
	var n nicSpec
	n.Network = "global/networks/default"
	n.AccessConfigs = []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}{{Type: "ONE_TO_ONE_NAT", Name: "External NAT"}}
	s.NetworkInterfaces = []nicSpec{n}
	s.Scheduling.MaxRunDuration.Seconds = fmt.Sprint(maxRunHours * 3600)
	s.Scheduling.InstanceTerminationAction = "DELETE"
	s.Metadata.Items = []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}{{Key: "startup-script", Value: script}}
	return s
}

// shQuote quotes a value for bash in single quotes.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var startupTemplate = template.Must(template.New("startup").Funcs(template.FuncMap{"sh": shQuote}).Parse(`#!/bin/bash
# Pings the .1 of every target /24 once and uploads the answers. The URLs are presigned and
# expire; nothing here prints them.
set -u
REGION={{sh .Region}}
TARGETS_URL={{sh .TargetsURL}}
RESULT_URL={{sh .ResultURL}}
PROGRESS_URL={{sh .ProgressURL}}
progress() { curl -fsS -m 30 -X PUT --data-binary "$REGION $*" "$PROGRESS_URL" >/dev/null 2>&1; }
progress "installing fping"
for _ in 1 2 3 4 5; do
  apt-get update -qq && apt-get install -y -qq fping && break
  sleep 30
done
command -v fping >/dev/null || { progress "failed: fping did not install"; exit 1; }
mkdir -p /var/tmp/chunks
cd /var/tmp || exit 1
got=0
for _ in 1 2 3 4 5; do
  curl -fsS -m 600 -o targets.txt "$TARGETS_URL" >/dev/null 2>&1 && got=1 && break
  sleep 20
done
[ "$got" = 1 ] || { progress "failed: target list"; exit 1; }
# Each VM pings in its own random order, so the pings of an address (and a network's traffic)
# are spread over the run instead of arriving together. The answers are sorted below.
shuf targets.txt -o targets.txt
split -l 200000 targets.txt chunks/c.
: > raw.txt
n=$(find chunks -name 'c.*' | wc -l)
i=0
for c in chunks/c.*; do
  progress "pinging, chunk $i of $n done"
  fping -C 1 -q -r 0 -t 1000 -i 1 -f "$c" 2>> raw.txt
  i=$((i + 1))
done
progress "sorting and uploading"
grep -v duplicate raw.txt | LC_ALL=C sort -T /var/tmp -S 200M -t. -k1,1n -k2,2n -k3,3n -k4,4n | gzip > "$REGION.txt.gz"
up=0
for _ in 1 2 3 4 5; do
  curl -fsS -m 900 -T "$REGION.txt.gz" "$RESULT_URL" >/dev/null 2>&1 && up=1 && break
  sleep 20
done
[ "$up" = 1 ] || { progress "failed: upload"; exit 1; }
progress "done"
`))

// startupScript fills the template for one region.
func startupScript(region, targetsURL, resultURL, progressURL string) (string, error) {
	var b bytes.Buffer
	err := startupTemplate.Execute(&b, map[string]string{
		"Region": region, "TargetsURL": targetsURL, "ResultURL": resultURL, "ProgressURL": progressURL,
	})
	return b.String(), err
}

// PingOutput counts the ping task (counts only).
type PingOutput struct {
	Skipped       string  `json:"skipped,omitempty"`
	Regions       int     `json:"regions"`
	RegionsDone   int     `json:"regions_done"`
	RegionsFailed int     `json:"regions_failed"`
	TimedOut      int     `json:"timed_out"`
	CreateRetries int     `json:"create_retries"`
	Leftovers     int     `json:"leftover_vms"`
	Seconds       float64 `json:"duration_seconds"`
}

type regionPhase int

const (
	phasePending regionPhase = iota // no VM yet
	phaseRunning
	phaseDone
	phaseGaveUp // timed out or failed: no result, no VM
)

type regionRun struct {
	region
	phase   regionPhase
	zone    string
	created time.Time
	retryAt time.Time
}

func vmName(region string) string { return vmPrefix + region }

// Ping is the ping task: one short-lived VM per region pings the target list and uploads its
// answers to R2. It always deletes every VM it made (or found) before it returns.
func (p *Pipeline) Ping(ctx context.Context) (PingOutput, error) {
	started := time.Now()
	var out PingOutput
	if !p.Enabled() {
		out.Skipped = "no GCP key"
		p.log("ping: skipped (no GCP key)")
		return out, nil
	}
	store, err := p.objects()
	if err != nil {
		return out, err
	}
	c, err := p.computeAPI()
	if err != nil {
		return out, err
	}
	tim := defaultTiming
	if p.timing != nil {
		tim = *p.timing
	}
	minRegions := p.minRegions
	if minRegions == 0 {
		minRegions = minRegionsOK
	}
	r := &runner{p: p, s: store, c: c, tim: tim}
	out, err = r.run(ctx)
	out.Seconds = time.Since(started).Round(time.Second).Seconds()
	if err != nil {
		return out, err
	}
	if out.RegionsDone < minRegions {
		return out, fmt.Errorf("only %d of %d regions have a result", out.RegionsDone, out.Regions)
	}
	return out, nil
}

// computeAPI is the Compute Engine client of the GCP key (tests set p.compute).
func (p *Pipeline) computeAPI() (computeAPI, error) {
	if p.compute != nil {
		return p.compute, nil
	}
	key, err := parseGCPKey(p.GCPKey)
	if err != nil {
		return nil, err
	}
	project := strings.TrimSpace(p.GCPProject)
	if project == "" {
		project = key.ProjectID
	}
	if project == "" {
		return nil, permanent("no Google Cloud project (set GCP_PING_PROJECT)")
	}
	return newComputeClient(project, newTokenSource(key, newHTTPClient()), newHTTPClient()), nil
}

// deleteLeftovers deletes every VM with our label (a crashed earlier run) and returns how many.
func deleteLeftovers(ctx context.Context, c computeAPI) (int, error) {
	left, err := c.ListInstances(ctx)
	if err != nil {
		return 0, fmt.Errorf("list the VMs: %w", err)
	}
	for _, ref := range left {
		if err := c.DeleteInstance(ctx, ref.Zone, ref.Name); err != nil {
			return 0, fmt.Errorf("delete a leftover VM: %w", err)
		}
	}
	return len(left), nil
}

type runner struct {
	p   *Pipeline
	s   objectStore
	c   computeAPI
	tim timing

	regions []*regionRun
	out     PingOutput
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *runner) hasResult(ctx context.Context, region string) (bool, error) {
	_, err := r.s.Stat(ctx, ResultKey(region))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, publish.ErrNotFound) {
		return false, nil
	}
	return false, err
}

// cleanup deletes every VM with our label (and, when the list fails, every VM this run made),
// with a fresh context: the run's own may be cancelled.
func (r *runner) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), r.tim.Cleanup)
	defer cancel()
	refs, err := r.c.ListInstances(ctx)
	if err != nil {
		refs = nil
		for _, rs := range r.regions {
			if rs.phase == phaseRunning {
				refs = append(refs, vmRef{rs.zone, vmName(rs.Name)})
			}
		}
	}
	failed := 0
	for _, ref := range refs {
		if err := r.c.DeleteInstance(ctx, ref.Zone, ref.Name); err != nil {
			failed++
		}
	}
	if failed > 0 || err != nil {
		r.p.log("cleanup: %d of %d VMs could not be deleted, list ok: %t (they delete themselves after %d hours)", failed, len(refs), err == nil, maxRunHours)
	}
}

func (r *runner) run(ctx context.Context) (out PingOutput, err error) {
	p := r.p
	for _, rg := range regions {
		r.regions = append(r.regions, &regionRun{region: rg})
	}
	r.out.Regions = len(regions)
	defer func() {
		r.cleanup()
		out = r.out
		out.RegionsDone, out.RegionsFailed = 0, 0
		cctx, cancel := context.WithTimeout(context.Background(), r.tim.Cleanup)
		defer cancel()
		for _, rs := range r.regions {
			if ok, e := r.hasResult(cctx, rs.Name); e == nil && ok {
				out.RegionsDone++
			} else {
				out.RegionsFailed++
			}
		}
	}()

	// A VM of a crashed earlier run is deleted first.
	left, err := deleteLeftovers(ctx, r.c)
	if err != nil {
		return r.out, err
	}
	r.out.Leftovers = left

	todo := 0
	for _, rs := range r.regions {
		ok, err := r.hasResult(ctx, rs.Name)
		if err != nil {
			return r.out, fmt.Errorf("check the results in R2: %w", err)
		}
		if ok {
			rs.phase = phaseDone
		} else {
			todo++
		}
	}
	p.log("ping: %d regions, %d already have a result, %d leftover VMs deleted", len(r.regions), len(r.regions)-todo, left)

	start := time.Now()
	pace := outputs.Pace{Every: r.tim.ProgressEvery}
	for {
		now := time.Now()
		r.startPending(ctx, now)
		if err := ctx.Err(); err != nil {
			return r.out, err
		}
		list, listErr := r.c.ListInstances(ctx)
		inList := map[string]bool{}
		for _, ref := range list {
			inList[ref.Name] = true
		}
		for _, rs := range r.regions {
			switch rs.phase {
			case phasePending:
				if now.Sub(start) > r.tim.RegionTimeout {
					rs.phase = phaseGaveUp
					r.out.TimedOut++
				}
			case phaseRunning:
				if ok, e := r.hasResult(ctx, rs.Name); e == nil && ok {
					r.drop(ctx, rs)
					rs.phase = phaseDone
				} else if now.Sub(rs.created) > r.tim.RegionTimeout {
					r.drop(ctx, rs)
					rs.phase = phaseGaveUp
					r.out.TimedOut++
				} else if listErr == nil && !inList[vmName(rs.Name)] && now.Sub(rs.created) > r.tim.Lost {
					// The creation was accepted but the VM never appeared (a quota or zone problem
					// that shows only in the operation): try again later.
					rs.phase, rs.retryAt = phasePending, now.Add(r.tim.CreateRetry)
					r.out.CreateRetries++
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return r.out, err
		}
		if pace.Due() {
			r.progressLine(ctx)
		}
		open := false
		for _, rs := range r.regions {
			if rs.phase == phasePending || rs.phase == phaseRunning {
				open = true
			}
		}
		if !open {
			break
		}
		if err := sleep(ctx, r.tim.Poll); err != nil {
			return r.out, err
		}
	}
	return r.out, nil
}

// drop deletes the VM of a region; a failure is repeated by the final cleanup.
func (r *runner) drop(ctx context.Context, rs *regionRun) {
	if err := r.c.DeleteInstance(ctx, rs.zone, vmName(rs.Name)); err != nil && ctx.Err() == nil {
		r.p.log("ping: a VM could not be deleted yet (the cleanup tries again)")
	}
}

// startPending creates the VM of every region that waits and whose time has come. A failure is
// tried again after CreateRetry.
func (r *runner) startPending(ctx context.Context, now time.Time) {
	for _, rs := range r.regions {
		if rs.phase != phasePending || now.Before(rs.retryAt) || ctx.Err() != nil {
			continue
		}
		if err := r.create(ctx, rs); err != nil {
			if ctx.Err() != nil {
				return
			}
			rs.retryAt = now.Add(r.tim.CreateRetry)
			r.out.CreateRetries++
			continue
		}
		rs.phase, rs.created = phaseRunning, now
	}
}

func (r *runner) create(ctx context.Context, rs *regionRun) error {
	zone, err := r.c.UpZone(ctx, rs.Name)
	if err != nil {
		return err
	}
	targets, err := r.s.PresignGet(ctx, TargetsKey, presignExpiry)
	if err != nil {
		return err
	}
	result, err := r.s.PresignPut(ctx, ResultKey(rs.Name), presignExpiry)
	if err != nil {
		return err
	}
	progress, err := r.s.PresignPut(ctx, ProgressKey(rs.Name), presignExpiry)
	if err != nil {
		return err
	}
	script, err := startupScript(rs.Name, targets, result, progress)
	if err != nil {
		return err
	}
	if err := r.c.CreateInstance(ctx, zone, newInstanceSpec(zone, vmName(rs.Name), script)); err != nil {
		return err
	}
	rs.zone = zone
	return nil
}

// progressLine reads the progress notes of the running VMs and logs counts. A VM whose note says
// it failed is deleted and its region is given up.
func (r *runner) progressLine(ctx context.Context) {
	done, pinging, starting, failed, waiting := 0, 0, 0, 0, 0
	for _, rs := range r.regions {
		switch rs.phase {
		case phaseDone:
			done++
		case phaseGaveUp:
			failed++
		case phasePending:
			waiting++
		case phaseRunning:
			switch r.note(ctx, rs.Name) {
			case "failed":
				r.drop(ctx, rs)
				rs.phase = phaseGaveUp
				failed++
			case "pinging":
				pinging++
			default:
				starting++
			}
		}
	}
	r.p.log("ping: done %d of %d, pinging %d, starting %d, waiting %d, failed %d",
		done, len(r.regions), pinging, starting, waiting, failed)
}

// note classifies the progress note of a region: failed, pinging or starting (no note yet, or
// installing). Only the class leaves this function.
func (r *runner) note(ctx context.Context, region string) string {
	f, err := os.CreateTemp(r.p.runDirEnsured(), "note-*")
	if err != nil {
		return "starting"
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)
	if _, err := r.s.Download(ctx, ProgressKey(region), path); err != nil {
		return "starting"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "starting"
	}
	if len(b) > 256 {
		b = b[:256]
	}
	text := string(b)
	switch {
	case strings.Contains(text, "failed"):
		return "failed"
	case strings.Contains(text, "installing"):
		return "starting"
	default:
		return "pinging"
	}
}

func (p *Pipeline) runDirEnsured() string {
	_ = os.MkdirAll(p.runDir(), 0o750)
	return p.runDir()
}
