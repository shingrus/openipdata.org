package main

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const upsertSQL = `INSERT INTO public.visitor_ips (ip, source)
SELECT ip, $2 FROM unnest($1::inet[]) AS ip
ON CONFLICT (ip) DO UPDATE SET last_seen_at = now()
WHERE visitor_ips.last_seen_at < now() - interval '1 hour'`

// Store writes visitor IPs over one pgx connection, reconnecting after a failure.
type Store struct {
	cfg    *pgx.ConnConfig
	source string
	conn   *pgx.Conn
}

// NewStore parses the DSN. pgx parse errors can echo the whole PGSQL value, so they are replaced.
func NewStore(dsn, source string) (*Store, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, hideDSN(err)
	}
	// Supabase poolers do not support prepared statements.
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.ConnectTimeout = 15 * time.Second
	return &Store{cfg: cfg, source: source}, nil
}

var errInvalidConfig = errors.New("PGSQL is not a valid connection string")

// hideDSN returns errInvalidConfig for a parse error and err otherwise.
func hideDSN(err error) error {
	var parseErr *pgconn.ParseConfigError
	if errors.As(err, &parseErr) {
		return errInvalidConfig
	}
	return err
}

// Write upserts the IPs in one statement. Returned errors are fixed texts (see describe).
func (s *Store) Write(ctx context.Context, ips []netip.Addr) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.conn == nil || s.conn.IsClosed() {
		conn, err := pgx.ConnectConfig(ctx, s.cfg)
		if err != nil {
			return describe("connect failed", err)
		}
		s.conn = conn
	}
	var sb strings.Builder
	sb.WriteByte('{')
	for i, ip := range ips {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(ip.String())
	}
	sb.WriteByte('}')
	if _, err := s.conn.Exec(ctx, upsertSQL, sb.String(), s.source); err != nil {
		s.conn.Close(context.Background())
		s.conn = nil
		return describe("write failed", err)
	}
	return nil
}

// Close closes the connection.
func (s *Store) Close() {
	if s.conn != nil {
		s.conn.Close(context.Background())
	}
}

// describe returns an error whose text is fixed plus the Postgres error code or a timeout marker:
// pgx errors can quote the DSN, and Postgres messages can quote an IP.
func describe(what string, err error) error {
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe):
		return errors.New(what + ", postgres code " + pe.Code)
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New(what + ", timeout")
	case errors.Is(err, context.Canceled):
		return errors.New(what + ", canceled")
	}
	return errors.New(what)
}
