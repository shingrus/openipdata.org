package domains

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// maxLookupsPerRun caps RDAP lookups per run (about 1 s each).
const maxLookupsPerRun = 3000

// Pipeline runs the domain update. Each method is one task; the Hatchet
// workflow and the local run-domains command call them in the same order.
type Pipeline struct {
	PGSQL    string
	Progress func(string)
}

func (p *Pipeline) log(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

// PDBStats describes the PeeringDB pass.
type PDBStats struct {
	PDBDomains int `json:"pdb_domains"` // known ASNs with a PeeringDB domain
	Set        int `json:"set"`         // rows written (new or changed)
	Unchanged  int `json:"unchanged"`
	Removed    int `json:"removed"` // PeeringDB domain disappeared; RDAP looks them up next
}

// PDBDomains stores the PeeringDB domain of every ASN that has one, replacing
// RDAP values, and drops PeeringDB rows whose domain disappeared.
func (p *Pipeline) PDBDomains(ctx context.Context) (PDBStats, error) {
	conn, err := connect(ctx, p.PGSQL)
	if err != nil {
		return PDBStats{}, err
	}
	defer conn.Close(ctx)
	wanted, stored, err := loadPDBState(ctx, conn)
	if err != nil {
		return PDBStats{}, err
	}
	plan := planPDB(stored, wanted)
	if err := applyPDB(ctx, conn, plan); err != nil {
		return PDBStats{}, err
	}
	stats := PDBStats{PDBDomains: len(wanted), Set: len(plan.set), Unchanged: plan.same, Removed: len(plan.remove)}
	p.log("pdb: %d ASNs with a PeeringDB domain: set=%d unchanged=%d removed=%d",
		stats.PDBDomains, stats.Set, stats.Unchanged, stats.Removed)
	return stats, nil
}

// RDAPStats describes the RDAP pass.
type RDAPStats struct {
	Due                int     `json:"due"` // ASNs due before this run
	Checked            int     `json:"checked"`
	Found              int     `json:"found"`
	NotFound           int     `json:"not_found"`
	Failed             int     `json:"failed"` // transient errors; retried next run
	RejectedCandidates int     `json:"rejected_candidates"`
	Remaining          int     `json:"remaining"` // still due after this run
	DurationSeconds    float64 `json:"duration_seconds"`
}

// RDAPDomains looks up the most urgent due ASNs, up to maxLookupsPerRun.
func (p *Pipeline) RDAPDomains(ctx context.Context) (RDAPStats, error) {
	opts := defaultRDAPOptions()
	started := time.Now()
	stats := RDAPStats{}
	conn, err := connect(ctx, p.PGSQL)
	if err != nil {
		return stats, err
	}
	defer conn.Close(ctx)
	due, err := dueASNs(ctx, conn)
	if err != nil {
		return stats, fmt.Errorf("list due ASNs: %w", err)
	}
	stats.Due = len(due)
	if len(due) > maxLookupsPerRun {
		due = due[:maxLookupsPerRun]
	}
	p.log("rdap: %d ASNs due, looking up %d", stats.Due, len(due))

	client := &http.Client{Timeout: opts.timeout}
	lastReport := time.Now()
	for _, asn := range due {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		result := lookupRDAP(ctx, client, opts, asn, func(msg string) { p.log("%s", msg) })
		if err := ctx.Err(); err != nil {
			return stats, err // stopped mid-lookup: not a failure of this ASN
		}
		stats.Checked++
		stats.RejectedCandidates += result.rejected
		var err error
		switch result.status {
		case statusFound:
			stats.Found++
			err = saveRDAP(ctx, conn, asn, result.domain)
		case statusNotFound:
			stats.NotFound++
			err = saveRDAP(ctx, conn, asn, "")
		case statusFailed:
			stats.Failed++
			err = saveRDAPFailure(ctx, conn, asn)
		}
		if err != nil {
			return stats, fmt.Errorf("store result for AS%d: %w", asn, err)
		}
		if time.Since(lastReport) >= 30*time.Second {
			p.log("rdap: %d/%d (found=%d not_found=%d failed=%d)", stats.Checked, len(due), stats.Found, stats.NotFound, stats.Failed)
			lastReport = time.Now()
		}
		if err := sleep(ctx, opts.delay); err != nil {
			return stats, err
		}
	}
	stats.Remaining = stats.Due - stats.Found - stats.NotFound
	stats.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("rdap: done checked=%d found=%d not_found=%d failed=%d rejected_candidates=%d remaining=%d",
		stats.Checked, stats.Found, stats.NotFound, stats.Failed, stats.RejectedCandidates, stats.Remaining)
	return stats, nil
}
