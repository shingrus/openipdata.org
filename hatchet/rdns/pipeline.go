// Package rdns runs the resolve-ips workflow: it traces a few hosts in each
// IPv4 /24 that visitors came from, names the routers near them (PTR), and turns
// the names that a rule of public.generated_rules recognizes into a city for the
// /24 (scope traced) and for its announced prefix (scope wide). build-geo-data
// writes those rows as the layers 65-rdns and 35-rdns-wide.
//
// Each Pipeline method is one task; the Hatchet workflow and the local run-rdns
// command call them in the same order:
//
//	PickNetworks  the /24s that are due (never traced with a visitor, schedule over, or brought
//	              back by a visitor; see schedule.go) -> the batch file (also the only task that
//	              runs DDL: it creates and upgrades the tables for the whole run)
//	Trace         scamper traceroutes of up to three hosts per /24 -> public.network_traces and
//	              the schedule public.traced_networks; deletes visitor IPs it no longer needs
//	ResolveNames  PTR names of the targets and the last hops (and the old names of located
//	              /24s) -> public.ip_ptr_names
//	Locate        rules + names + announced prefixes (+ router hop cities from our own
//	              published ip2geo MMDB) -> public.rdns_geo_layer, which keeps the rows of
//	              located /24s that no trace locates again (skipped while nothing it reads
//	              has changed)
//
// A /24 that no rule matches may still vote for its announced prefix with the city of
// one of its router hops in our own published ip2geo MMDB (a copy kept from R2, read
// by mmdbbuilder lookup); see mmdb.go.
//
// A Hatchet run that picked its full limit starts the next run once it has succeeded (see
// ShouldChain and main.go). resolve-ips is also the deleter of public.visitor_ips.
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
	"time"

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

	// MMDBBuilder is the mmdbbuilder binary next to the worker, and R2 the setting of the
	// private bucket that holds the published ip2geo MMDB: locate reads the cities of
	// router hops from it. Without either the fallback is skipped, not an error.
	MMDBBuilder string
	R2          string

	// Tests only.
	store    objectStore
	lookup   lookupFunc
	scamper  scamperFunc
	intn     func(n int) int
	resolver *ptrResolver
	now      func() time.Time
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

// connect opens a connection and runs no DDL: ALTER TABLE takes an ACCESS EXCLUSIVE
// lock even when the column exists, and build-geo-data reads rdns_geo_layer, so the
// tables are created and upgraded once per run (see connectEnsure), not by every task.
func connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	if dsn == "" {
		return nil, errors.New("PGSQL is not set")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	return conn, nil
}

// connectEnsure is connect, then creates the tables resolve-ips writes. It is used by
// the first task of a run only (PickNetworks, which the run-rdns command also starts
// with); trace, resolve-names and locate follow it in the same run and use connect. Only the
// statements of schema.EnsureRDNS (its own tables, plus the ALTERs of network_traces and
// rdns_geo_layer) and CREATE TABLE IF NOT EXISTS of visitor_ips run here, never
// schema.Ensure: its ALTER TABLE statements lock the ASN tables, and visitor_ips
// belongs to the external websites.
func connectEnsure(ctx context.Context, dsn string) (*pgx.Conn, error) {
	conn, err := connect(ctx, dsn)
	if err != nil {
		return nil, err
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
