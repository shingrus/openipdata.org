// Package ping is the workflow sync-hosting-ping: once a month the worker pings the .1 address of
// every /24 announced by a hosting network from small Google Cloud VMs in about 27 regions and
// uploads the lowest round-trip time per region and /24 as hosting-rtt.csv.gz to the private R2
// bucket, where build-geo-data reads it and gives it to "mmdbbuilder geo -ping-country" (a
// speed-of-light check of the country).
//
// Three tasks: targets (the address list from public.asn, to R2), ping (one short-lived VM per
// region, created and deleted through the Compute Engine REST API; the VMs talk only to R2
// through presigned URLs and have no Google permissions) and publish (merge the regions, check
// the file, upload it).
package ping

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"openipdata.org/hatchet/publish"
)

const (
	// RTTFile is the name of the merged file and RTTKey its one fixed key in the private R2
	// bucket, replaced by each new run.
	RTTFile = "hosting-rtt.csv.gz"
	RTTKey  = "internal/ping/" + RTTFile

	// RunPrefix holds the objects of one run: the target list, the region results and the
	// progress notes of the VMs. It is emptied at the start of each run and after the upload.
	RunPrefix  = "internal/ping/run/"
	TargetsKey = RunPrefix + "targets.txt"

	skippedNoKey = "skipped: no GCP key"
)

// ResultKey is the key of the result of a region; ProgressKey the key of its progress note.
func ResultKey(region string) string   { return RunPrefix + "out/" + region + ".txt.gz" }
func ProgressKey(region string) string { return RunPrefix + "progress/" + region + ".txt" }

// ErrPermanent marks an error that no retry can fix (a bad setting, a refused list or file). The
// worker turns it into a non-retryable task error.
var ErrPermanent = errors.New("permanent")

// objectStore is the part of publish.R2 the tasks use. Tests replace it.
type objectStore interface {
	Stat(ctx context.Context, key string) (publish.ObjectInfo, error)
	Download(ctx context.Context, key, path string) (time.Time, error)
	Put(ctx context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error)
	DeletePrefix(ctx context.Context, prefix string) (int, error)
	PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error)
	PresignPut(ctx context.Context, key string, expiry time.Duration) (string, error)
}

// Pipeline runs sync-hosting-ping.
type Pipeline struct {
	DataDir    string
	R2         string // the setting of the private bucket (a secret)
	PGSQL      string // the Postgres DSN (a secret)
	GCPKey     string // GCP_PING_KEY: a service-account JSON key, base64 (a secret)
	GCPProject string // GCP_PING_PROJECT; empty = the project of the key
	HCPing     string // optional health-check URL (HC_PING_HOSTING); never logged
	Progress   func(string)

	// Tests only.
	store      objectStore
	hosting    hostingSource
	compute    computeAPI
	timing     *timing
	minTargets int
	maxTargets int
	minValid   int
	minRegions int
}

func (p *Pipeline) dir() string         { return filepath.Join(p.DataDir, "ping") }
func (p *Pipeline) runDir() string      { return filepath.Join(p.dir(), "run") }
func (p *Pipeline) targetsPath() string { return filepath.Join(p.runDir(), "targets.txt") }
func (p *Pipeline) outDir() string      { return filepath.Join(p.runDir(), "out") }
func (p *Pipeline) mergedPath() string  { return filepath.Join(p.dir(), RTTFile) }

// log sends a line to the progress function. Callers pass counts only.
func (p *Pipeline) log(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

func (p *Pipeline) objects() (objectStore, error) {
	if p.store != nil {
		return p.store, nil
	}
	return publish.NewR2(p.R2)
}

// Enabled says whether the workflow does anything: without GCP_PING_KEY every task skips.
func (p *Pipeline) Enabled() bool { return strings.TrimSpace(p.GCPKey) != "" }

// CheckSettings fails when a setting the tasks need is missing or invalid, so a run stops before
// the work and without retries. needDB is for the targets task. Without a GCP key nothing is
// checked: the tasks skip.
func (p *Pipeline) CheckSettings(needDB bool) error {
	if !p.Enabled() {
		return nil
	}
	if _, err := publish.NewR2(p.R2); err != nil && p.store == nil {
		return err
	}
	if _, err := parseGCPKey(p.GCPKey); err != nil {
		return err
	}
	if needDB && strings.TrimSpace(p.PGSQL) == "" && p.hosting == nil {
		return errors.New("PGSQL is not set")
	}
	return nil
}

// Fail pings the health check's /fail address (a failed run). A failed ping only logs.
func (p *Pipeline) Fail(ctx context.Context) {
	if p.HCPing == "" {
		return
	}
	if err := publish.Ping(ctx, strings.TrimRight(p.HCPing, "/")+"/fail"); err != nil {
		p.log("health check: %v", err)
	}
}
