package asn

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"openipdata.org/hatchet/schema"
)

// Store writes public.asn and reads public.asn_domain and public.asn_geo_pdb.
type Store struct {
	pool *pgxpool.Pool
}

// OpenStore connects to Postgres. Use Supabase's session pooler DSN (port 5432).
func OpenStore(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, schema.HideDSN(err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// EnsureSchema creates missing tables and columns.
func (s *Store) EnsureSchema(ctx context.Context) error {
	return schema.Ensure(ctx, s.pool)
}

// Row digests are computed identically in Go and in SQL so unchanged rows are
// detected without reading the large json column back out of Postgres.
const (
	digestSep  = "\x1e"
	digestNull = `\N`
)

const rowDigestSQL = `md5(concat_ws(chr(30),
	asn::text,
	coalesce(handle, '\N'),
	coalesce(organization, '\N'),
	coalesce(organization_slug, '\N'),
	coalesce(country, '\N'),
	coalesce(ip_amount::text, '\N'),
	coalesce(ipv4_amount::text, '\N'),
	coalesce(ipv6_amount::text, '\N'),
	json))`

func optional(value *string) string {
	if value == nil {
		return digestNull
	}
	return *value
}

// Digest matches rowDigestSQL for this record.
func (r Record) Digest() string {
	parts := []string{
		strconv.FormatInt(r.ASN, 10),
		optional(r.Handle),
		optional(r.Organization),
		optional(r.OrganizationSlug),
		optional(r.Country),
		r.IPAmount,
		r.IPv4Amount,
		r.IPv6Amount,
		r.JSON,
	}
	sum := md5.Sum([]byte(strings.Join(parts, digestSep)))
	return hex.EncodeToString(sum[:])
}

// IngestStats counts what an ingest changed.
type IngestStats struct {
	Total     int `json:"total"`
	Inserted  int `json:"inserted"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	// Stale rows exist in Postgres but not in the source; they are kept.
	Stale int `json:"stale"`
}

// Ingest upserts only records whose digest differs from the stored row.
func (s *Store) Ingest(ctx context.Context, records []Record) (IngestStats, error) {
	stats := IngestStats{Total: len(records)}
	existing, err := s.rowDigests(ctx)
	if err != nil {
		return stats, err
	}

	var changed [][]any
	seen := make(map[int64]struct{}, len(records))
	for _, r := range records {
		seen[r.ASN] = struct{}{}
		digest, ok := existing[r.ASN]
		switch {
		case !ok:
			stats.Inserted++
		case digest != r.Digest():
			stats.Updated++
		default:
			stats.Unchanged++
			continue
		}
		changed = append(changed, []any{
			r.ASN, r.Handle, r.Organization, r.OrganizationSlug, r.Country,
			r.IPAmount, r.IPv4Amount, r.IPv6Amount, r.JSON,
		})
	}
	for asn := range existing {
		if _, ok := seen[asn]; !ok {
			stats.Stale++
		}
	}
	if len(changed) == 0 {
		return stats, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return stats, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE asn_stage (
		asn BIGINT, handle TEXT, organization TEXT, organization_slug TEXT, country TEXT,
		ip_amount TEXT, ipv4_amount TEXT, ipv6_amount TEXT, json TEXT
	) ON COMMIT DROP`); err != nil {
		return stats, err
	}
	columns := []string{"asn", "handle", "organization", "organization_slug", "country",
		"ip_amount", "ipv4_amount", "ipv6_amount", "json"}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"asn_stage"}, columns, pgx.CopyFromRows(changed)); err != nil {
		return stats, fmt.Errorf("copy changed rows: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.asn
		(asn, handle, organization, organization_slug, country, ip_amount, ipv4_amount, ipv6_amount, json)
		SELECT asn, handle, organization, organization_slug, country,
		       ip_amount::numeric, ipv4_amount::numeric, ipv6_amount::numeric, json
		  FROM asn_stage
		ON CONFLICT (asn) DO UPDATE SET
			handle = excluded.handle,
			organization = excluded.organization,
			organization_slug = excluded.organization_slug,
			country = excluded.country,
			ip_amount = excluded.ip_amount,
			ipv4_amount = excluded.ipv4_amount,
			ipv6_amount = excluded.ipv6_amount,
			json = excluded.json`); err != nil {
		return stats, fmt.Errorf("upsert asn: %w", err)
	}
	return stats, tx.Commit(ctx)
}

func (s *Store) rowDigests(ctx context.Context) (map[int64]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT asn, `+rowDigestSQL+` FROM public.asn`)
	if err != nil {
		return nil, fmt.Errorf("read row digests: %w", err)
	}
	digests := map[int64]string{}
	for rows.Next() {
		var asn int64
		var digest string
		if err := rows.Scan(&asn, &digest); err != nil {
			rows.Close()
			return nil, err
		}
		digests[asn] = digest
	}
	return digests, rows.Err()
}

// StaleRecords returns rows present in Postgres but absent from current, so the
// SQLite export keeps them.
func (s *Store) StaleRecords(ctx context.Context, current []Record) ([]Record, error) {
	ids := make([]int64, len(current))
	for i, r := range current {
		ids[i] = r.ASN
	}
	rows, err := s.pool.Query(ctx, `SELECT asn, handle, organization, organization_slug, country,
		ip_amount::text, ipv4_amount::text, ipv6_amount::text, json
		FROM public.asn WHERE asn <> ALL($1) ORDER BY asn`, ids)
	if err != nil {
		return nil, fmt.Errorf("read stale rows: %w", err)
	}
	defer rows.Close()
	var stale []Record
	for rows.Next() {
		var r Record
		var ip, ipv4, ipv6 *string
		if err := rows.Scan(&r.ASN, &r.Handle, &r.Organization, &r.OrganizationSlug, &r.Country,
			&ip, &ipv4, &ipv6, &r.JSON); err != nil {
			return nil, err
		}
		r.IPAmount, r.IPv4Amount, r.IPv6Amount = deref(ip), deref(ipv4), deref(ipv6)
		stale = append(stale, r)
	}
	return stale, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// DomainRow is one asn_domain row as stored; update-asn-domains writes them.
type DomainRow struct {
	ASN    int64
	Domain *string
	Source *string
}

// Domains returns every asn_domain row.
func (s *Store) Domains(ctx context.Context) ([]DomainRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT asn, domain, source FROM public.asn_domain ORDER BY asn`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DomainRow, error) {
		var d DomainRow
		err := row.Scan(&d.ASN, &d.Domain, &d.Source)
		return d, err
	})
}

// GeoPDBRow is one asn_geo_pdb row; the PeeringDB workflow fills this table.
type GeoPDBRow struct {
	ASN       int64
	Country   *string
	City      *string
	Dominance *float64
	Domain    *string
}

// GeoPDB returns every asn_geo_pdb row.
func (s *Store) GeoPDB(ctx context.Context) ([]GeoPDBRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT asn, country, city, dominance, domain
		FROM public.asn_geo_pdb ORDER BY asn`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (GeoPDBRow, error) {
		var g GeoPDBRow
		err := row.Scan(&g.ASN, &g.Country, &g.City, &g.Dominance, &g.Domain)
		return g, err
	})
}
