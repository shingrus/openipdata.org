package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const secret = "s3cr3tpass"

func TestNoLeaks(t *testing.T) {
	// A parse error that echoes the DSN is replaced.
	for _, dsn := range []string{"postgres://u:" + secret + "@h:5432/db?sslmode=bogus", "host=h user=u password=" + secret + " port=x"} {
		_, err := NewStore(dsn, "src")
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("NewStore(%q) error = %v", dsn, err)
		}
	}
	_, raw := pgx.ParseConfig("host=h user=u password=" + secret + " port=x")
	if raw == nil || !strings.Contains(raw.Error(), secret) {
		t.Skip("pgx no longer echoes the DSN; hideDSN has nothing to prove")
	}

	// Postgres and network errors that quote an IP or the DSN come out as fixed texts.
	pgErr := &pgconn.PgError{Code: "23505", Message: `duplicate key value (ip)=(8.8.8.8)`, Detail: "8.8.8.8"}
	errs := []error{
		pgErr,
		fmt.Errorf("wrap: %w", pgErr),
		errors.New("dial tcp 8.8.8.8:5432 password=" + secret),
		context.DeadlineExceeded,
	}
	for _, err := range errs {
		text := describe("write failed", err).Error()
		if strings.Contains(text, "8.8.8.8") || strings.Contains(text, secret) {
			t.Errorf("describe leaked: %q", text)
		}
	}
	if got := describe("write failed", fmt.Errorf("x: %w", pgErr)).Error(); got != "write failed, postgres code 23505" {
		t.Errorf("describe = %q", got)
	}

	// Batcher log lines and the summary hold no IP.
	b, w, _, logs := newTest()
	w.err = describe("write failed", pgErr)
	for i := 0; i < 100; i++ {
		b.Add(ctx, ipn(i))
	}
	line := summaryLine(Counts{Lines: 5}, b.Stats, 1)
	for _, s := range []string{logs.String(), line} {
		if strings.Contains(s, "8.") && strings.Contains(s, ".0.") || strings.Contains(s, secret) {
			t.Errorf("log text leaks: %q", s)
		}
	}
	if !strings.HasPrefix(line, "summary: lines 5,") {
		t.Errorf("summary = %q", line)
	}
}
