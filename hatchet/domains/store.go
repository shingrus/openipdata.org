package domains

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/schema"
)

// Sources recorded in asn_domain.source.
const (
	SourcePDB  = "pdb"
	SourceRDAP = "rdap"
)

// Re-check periods for RDAP results, and the pause after a failed lookup.
const (
	recheckFound    = "180 days"
	recheckNotFound = "30 days"
	retryFailed     = "1 day"
)

func connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	if dsn == "" {
		return nil, fmt.Errorf("PGSQL is not set")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	if err := schema.Ensure(ctx, conn); err != nil {
		conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}

type storedDomain struct {
	domain string // "" when NULL or blank
	source string // "" when NULL
}

// pdbPlan is what the PeeringDB pass changes in asn_domain.
type pdbPlan struct {
	set    map[int64]string // ASN -> PeeringDB domain to store
	remove []int64          // ASNs whose PeeringDB domain disappeared
	same   int
}

// planPDB compares the stored rows with the wanted PeeringDB domains.
func planPDB(stored map[int64]storedDomain, wanted map[int64]string) pdbPlan {
	plan := pdbPlan{set: map[int64]string{}}
	for asn, domain := range wanted {
		if cur, ok := stored[asn]; ok && cur.source == SourcePDB && cur.domain == domain {
			plan.same++
			continue
		}
		plan.set[asn] = domain
	}
	for asn, cur := range stored {
		if _, ok := wanted[asn]; !ok && cur.source == SourcePDB {
			plan.remove = append(plan.remove, asn)
		}
	}
	sort.Slice(plan.remove, func(i, j int) bool { return plan.remove[i] < plan.remove[j] })
	return plan
}

// loadPDBState reads PeeringDB domains for known ASNs and the stored asn_domain rows.
func loadPDBState(ctx context.Context, conn *pgx.Conn) (map[int64]string, map[int64]storedDomain, error) {
	wanted := map[int64]string{}
	rows, err := conn.Query(ctx, `SELECT g.asn, g.domain FROM public.asn_geo_pdb g
		JOIN public.asn a ON a.asn = g.asn
		WHERE g.domain IS NOT NULL`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var asn int64
		var raw string
		if err := rows.Scan(&asn, &raw); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if d := Normalize(raw); d != "" {
			wanted[asn] = d
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	stored := map[int64]storedDomain{}
	rows, err = conn.Query(ctx, `SELECT asn, coalesce(trim(domain), ''), coalesce(source, '') FROM public.asn_domain`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var asn int64
		var s storedDomain
		if err := rows.Scan(&asn, &s.domain, &s.source); err != nil {
			rows.Close()
			return nil, nil, err
		}
		stored[asn] = s
	}
	return wanted, stored, rows.Err()
}

// applyPDB writes a plan in one transaction.
func applyPDB(ctx context.Context, conn *pgx.Conn, plan pdbPlan) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if len(plan.set) > 0 {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE pdb_stage (asn BIGINT, domain TEXT) ON COMMIT DROP`); err != nil {
			return err
		}
		rows := make([][]any, 0, len(plan.set))
		for asn, domain := range plan.set {
			rows = append(rows, []any{asn, domain})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"pdb_stage"}, []string{"asn", "domain"}, pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("copy PeeringDB domains: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.asn_domain (asn, domain, source, checked_at, failed_at)
			SELECT asn, domain, 'pdb', now(), NULL FROM pdb_stage
			ON CONFLICT (asn) DO UPDATE SET
				domain = excluded.domain, source = excluded.source,
				checked_at = excluded.checked_at, failed_at = NULL`); err != nil {
			return fmt.Errorf("store PeeringDB domains: %w", err)
		}
	}
	if len(plan.remove) > 0 {
		// Deleted rows have no domain at all, so RDAP looks them up first.
		if _, err := tx.Exec(ctx, `DELETE FROM public.asn_domain WHERE source = 'pdb' AND asn = ANY($1)`, plan.remove); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// dueASNs lists ASNs that need an RDAP lookup, most urgent first:
// no row at all, then older rows with a rejected domain, then other older rows,
// then RDAP results past their re-check period. PeeringDB rows are never due,
// and ASNs whose last lookup failed wait retryFailed before they are due again.
func dueASNs(ctx context.Context, conn *pgx.Conn) ([]int64, error) {
	rejectedList := make([]string, 0, len(rejected))
	for d := range rejected {
		rejectedList = append(rejectedList, d)
	}
	rows, err := conn.Query(ctx, `SELECT a.asn
		FROM public.asn a
		LEFT JOIN public.asn_domain d ON d.asn = a.asn
		WHERE d.asn IS NULL
		   OR (d.source IS DISTINCT FROM 'pdb'
		       AND (d.failed_at IS NULL OR d.failed_at < now() - $4::interval)
		       AND (d.checked_at IS NULL
		            OR (NULLIF(trim(d.domain), '') IS NOT NULL AND d.checked_at < now() - $2::interval)
		            OR (NULLIF(trim(d.domain), '') IS NULL AND d.checked_at < now() - $3::interval)))
		ORDER BY
		   CASE WHEN d.asn IS NULL THEN 0
		        WHEN d.checked_at IS NULL AND lower(trim(d.domain)) = ANY($1) THEN 1
		        WHEN d.checked_at IS NULL THEN 2
		        ELSE 3 END,
		   d.failed_at NULLS FIRST, d.checked_at NULLS FIRST, a.asn`,
		rejectedList, recheckFound, recheckNotFound, retryFailed)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// saveRDAP stores one lookup result right away, so a long run resumes where it
// stopped. domain "" records "looked up, nothing found". PeeringDB rows win.
func saveRDAP(ctx context.Context, conn *pgx.Conn, asn int64, domain string) error {
	var value any
	if domain != "" {
		value = domain
	}
	_, err := conn.Exec(ctx, `INSERT INTO public.asn_domain (asn, domain, source, checked_at, failed_at)
		VALUES ($1, $2, 'rdap', now(), NULL)
		ON CONFLICT (asn) DO UPDATE SET
			domain = excluded.domain, source = excluded.source,
			checked_at = excluded.checked_at, failed_at = NULL
		WHERE public.asn_domain.source IS DISTINCT FROM 'pdb'`, asn, value)
	return err
}

// saveRDAPFailure records a temporary lookup failure, keeping any stored domain,
// so the ASN waits retryFailed instead of heading every following run.
func saveRDAPFailure(ctx context.Context, conn *pgx.Conn, asn int64) error {
	_, err := conn.Exec(ctx, `INSERT INTO public.asn_domain (asn, failed_at) VALUES ($1, now())
		ON CONFLICT (asn) DO UPDATE SET failed_at = excluded.failed_at
		WHERE public.asn_domain.source IS DISTINCT FROM 'pdb'`, asn)
	return err
}
