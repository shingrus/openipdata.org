package geo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EvalOutput is what Evaluate scored. Metrics holds the numbers
// "mmdbbuilder eval" printed; a score whose reference was not available is
// absent. References tells how each reference stands (never a URL: the IPinfo
// URL holds a secret).
type EvalOutput struct {
	ID              int64              `json:"id"` // the row in public.geo_eval
	Metrics         map[string]float64 `json:"metrics"`
	References      map[string]string  `json:"references"`
	DurationSeconds float64            `json:"duration_seconds"`
	// Error is set when the evaluation itself failed (holdout, mmdbbuilder
	// eval, Postgres). The task still succeeds so that the run goes on to
	// publish, which refuses unless force_publish is set.
	Error string `json:"error,omitempty"`
}

func (p *Pipeline) evalDir() string { return filepath.Join(p.dir(), "eval") }

// Evaluate scores the MMDB that BuildMMDB built and stores the scores in
// public.geo_eval (published = false; Publish flips it).
//
// It refreshes the reference databases in DataDir/geo/eval/ (DB-IP city,
// IPinfo country, RIPE Atlas probes), builds a second MMDB without a fixed
// tenth of the geofeeds (holdout.mmdb) to score the lower layers on the
// held-out rows, and runs "mmdbbuilder eval". A reference that cannot be
// fetched falls back to its cached copy; with no copy it is skipped, which
// only leaves its scores out.
//
// A problem with the evaluation itself (the holdout, mmdbbuilder eval,
// Postgres) does not fail the task: it goes into EvalOutput.Error, and Publish
// refuses such a result unless forced. Only cancellation and a local file
// error in the eval directory return an error.
func (p *Pipeline) Evaluate(ctx context.Context, src Source, mmdbPath string) (EvalOutput, error) {
	started := time.Now()
	out := EvalOutput{References: map[string]string{}}
	dir := p.evalDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return out, err
	}
	client := newHTTPClient()
	now := time.Now().UTC()
	// failed records a problem with the evaluation and ends it normally.
	failed := func(err error) (EvalOutput, error) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, ctxErr
		}
		out.Error = err.Error()
		out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
		p.log("eval: failed: %v", err)
		return out, nil
	}
	args := []string{"eval", "-db", mmdbPath, "-rir", filepath.Join(p.layersDir(), RIRLayer)}

	for _, ref := range []struct {
		name, flag string
		fetch      func(ctx context.Context, client *http.Client, dir string, now time.Time) (string, string)
	}{
		{"dbip", "-dbip", p.fetchDBIP},
		{"ipinfo", "-ipinfo", p.fetchIPinfo},
		{"atlas", "-atlas", p.fetchAtlas},
	} {
		path, status := ref.fetch(ctx, client, dir, now)
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out.References[ref.name] = status
		p.log("eval: %s: %s", ref.name, status)
		if path != "" {
			args = append(args, ref.flag, path)
		}
	}

	holdoutDB, truth, status, err := p.prepareHoldout(ctx, dir)
	if err != nil {
		return failed(err)
	}
	out.References["holdout"] = status
	p.log("eval: holdout: %s", status)
	if holdoutDB != "" {
		args = append(args, "-holdout-db", holdoutDB, "-holdout-truth", truth)
	}

	stdout, err := p.runBuilder(ctx, args)
	if err != nil {
		return failed(err)
	}
	if err := json.Unmarshal([]byte(stdout), &out.Metrics); err != nil {
		out.Metrics = nil
		return failed(fmt.Errorf("mmdbbuilder eval: unreadable output: %w", err))
	}
	p.log("eval: %s", formatMetrics(out.Metrics))

	if out.ID, err = p.storeEval(ctx, src.DateTag, out.Metrics); err != nil {
		out.ID = 0
		return failed(fmt.Errorf("store scores: %w", err))
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	return out, nil
}

// storeEval inserts the scores as a new, unpublished row and returns its id.
// The task is retried, and a retry must not leave duplicates: unpublished rows
// of the same date_tag are deleted first, in the same transaction as the insert.
func (p *Pipeline) storeEval(ctx context.Context, dateTag string, metrics map[string]float64) (int64, error) {
	data, err := json.Marshal(metrics)
	if err != nil {
		return 0, err
	}
	conn, err := p.connect(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM public.geo_eval WHERE date_tag = $1 AND NOT published`, dateTag); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO public.geo_eval (date_tag, metrics) VALUES ($1, $2::jsonb) RETURNING id`,
		dateTag, string(data)).Scan(&id); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func formatMetrics(metrics map[string]float64) string {
	names := make([]string, 0, len(metrics))
	for name := range metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s=%g", name, metrics[name])
	}
	return strings.Join(parts, " ")
}
