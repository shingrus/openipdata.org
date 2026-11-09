package schema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// leakyDSNs are connection strings pgx cannot parse well enough to redact.
var leakyDSNs = []string{
	"s3cr3t-password",               // a bare value, e.g. a password pasted into PGSQL
	"host=h pass=s3cr3t-password x", // keyword typo ("pass" for "password") in an unparsable string
}

func TestHideDSN(t *testing.T) {
	for _, dsn := range leakyDSNs {
		_, connectErr := pgx.Connect(context.Background(), dsn)
		_, poolErr := pgxpool.ParseConfig(dsn)
		for _, err := range []error{connectErr, poolErr} {
			if err == nil || !strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("%q: want a parse error that echoes the password, got %v", dsn, err)
			}
			hidden := HideDSN(err)
			if !errors.Is(hidden, ErrInvalidConfig) || strings.Contains(hidden.Error(), "s3cr3t") {
				t.Errorf("%q: HideDSN = %v", dsn, hidden)
			}
		}
	}
	other := errors.New("connection refused")
	if HideDSN(other) != other {
		t.Error("HideDSN changed a non-parse error")
	}
}

func TestEnsureAddsSeenAt(t *testing.T) {
	for _, stmt := range statements {
		if strings.Contains(stmt, "public.asn ADD COLUMN IF NOT EXISTS seen_at") {
			return
		}
	}
	t.Error("no idempotent statement adds public.asn.seen_at")
}
