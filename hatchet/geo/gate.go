package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/schema"
)

// ErrGate means the new database scores clearly worse than the last published
// one. Retrying cannot help; a run with force_publish skips the gate.
var ErrGate = errors.New("publish gate")

// The publish gate compares these scores with the last published row and
// refuses a drop of more than gateMaxDrop points. They are the ones that do
// not depend on how complete the reference databases are.
var gatedMetrics = []string{"atlas_country_pct", "holdout_country_pct"}

const gateMaxDrop = 2.0

// gateDecision returns an ErrGate error if a gated score present in both
// previous and current fell by more than gateMaxDrop points.
func gateDecision(previous, current map[string]float64) error {
	var drops []string
	for _, name := range gatedMetrics {
		before, hadBefore := previous[name]
		now, hasNow := current[name]
		// The scores have two decimals: round, so that exactly 2.00 passes.
		if hadBefore && hasNow && math.Round((before-now)*100)/100 > gateMaxDrop {
			drops = append(drops, fmt.Sprintf("%s fell from %.2f to %.2f", name, before, now))
		}
	}
	if len(drops) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s (more than %.1f points below the last published database); check the build, then run once with force_publish", ErrGate, strings.Join(drops, ", "), gateMaxDrop)
}

// connect opens a Postgres connection with public.geo_eval in place. It runs
// EnsureGeoEval, not Ensure: the other tables are only read here, and Ensure
// would take the writers' table locks.
func (p *Pipeline) connect(ctx context.Context) (*pgx.Conn, error) {
	if p.PGSQL == "" {
		return nil, errors.New("PGSQL is not set")
	}
	conn, err := pgx.Connect(ctx, p.PGSQL)
	if err != nil {
		return nil, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	if err := schema.EnsureGeoEval(ctx, conn); err != nil {
		conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}

// CheckGate compares ev with the latest published scores in public.geo_eval.
// With no published row there is no baseline and the check passes.
func (p *Pipeline) CheckGate(ctx context.Context, ev EvalOutput) error {
	conn, err := p.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var raw []byte
	err = conn.QueryRow(ctx, `SELECT metrics FROM public.geo_eval WHERE published ORDER BY run_at DESC, id DESC LIMIT 1`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		p.log("publish: gate passed, no published baseline yet")
		return nil
	}
	if err != nil {
		return err
	}
	var previous map[string]float64
	if err := json.Unmarshal(raw, &previous); err != nil {
		return fmt.Errorf("last published scores: %w", err)
	}
	if err := gateDecision(previous, ev.Metrics); err != nil {
		return err
	}
	p.log("publish: gate passed")
	return nil
}

// markPublished flags a row of public.geo_eval as published.
func (p *Pipeline) markPublished(ctx context.Context, id int64) error {
	conn, err := p.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	tag, err := conn.Exec(ctx, `UPDATE public.geo_eval SET published = true WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("public.geo_eval row %d not found", id)
	}
	return nil
}
