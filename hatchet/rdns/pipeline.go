// Package rdns runs the resolve-ips workflow: it traces a few hosts in each
// IPv4 /24 that visitors came from, names the routers near them (PTR), and turns
// the names that a rule of public.generated_rules recognizes into a city for the
// /24 (scope traced) and for its announced prefix (scope wide). build-geo-data
// writes those rows as the layers 65-rdns and 35-rdns-wide.
//
// Each Pipeline method is one task; the Hatchet workflow and the local run-rdns
// command call them in the same order:
//
//	PickNetworks  /24s with a visitor and no recent trace -> the batch file
//	Trace         scamper traceroutes of three hosts per /24 -> public.network_traces
//	ResolveNames  PTR names of the targets and the last hops -> public.ip_ptr_names
//	Locate        rules + names + announced prefixes -> public.rdns_geo_layer
//
// Visitor IPs (and every traced IP) stay in Postgres and in the batch file on the
// VM: never in Hatchet task inputs, outputs, log lines or returned errors. Tasks
// report counts only, and errors that could carry an IP are replaced or scrubbed.
package rdns

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"openipdata.org/hatchet/schema"
)

// Pipeline runs resolve-ips.
type Pipeline struct {
	DataDir  string
	PGSQL    string
	Progress func(string)
	Scamper  string // path of the scamper binary (the worker passes the one next to itself)

	// Tests only.
	scamper  scamperFunc
	intn     func(n int) int
	resolver *ptrResolver
}

func (p *Pipeline) dir() string { return filepath.Join(p.DataDir, "rdns") }

func (p *Pipeline) log(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

func (p *Pipeline) randInt(n int) int {
	if p.intn != nil {
		return p.intn(n)
	}
	return rand.IntN(n)
}

// CheckPGSQL fails when PGSQL is not set, so a run stops before the first task works.
func (p *Pipeline) CheckPGSQL() error {
	if p.PGSQL == "" {
		return errors.New("PGSQL is not set")
	}
	return nil
}

// connect opens a connection and creates the tables resolve-ips writes. Only
// CREATE TABLE IF NOT EXISTS runs here, never schema.Ensure: its ALTER TABLE
// statements lock the ASN tables, and visitor_ips belongs to the external websites.
func connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	if dsn == "" {
		return nil, errors.New("PGSQL is not set")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	if err := schema.EnsureVisitorIPs(ctx, conn); err != nil {
		conn.Close(ctx)
		return nil, err
	}
	if err := schema.EnsureRDNS(ctx, conn); err != nil {
		conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}

// dbError replaces a Postgres error by its code: the message of a failed
// statement can quote the values it was given, and those are visitor IPs here.
func dbError(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("%s: Postgres error %s", op, pgErr.Code)
	}
	return fmt.Errorf("%s: %w", op, err)
}
