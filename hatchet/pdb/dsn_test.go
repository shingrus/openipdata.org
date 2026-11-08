package pdb

import (
	"context"
	"strings"
	"testing"
)

// Connection strings pgx cannot parse well enough to redact the password.
func TestConnectErrorHidesDSN(t *testing.T) {
	for _, dsn := range []string{"s3cr3t-password", "host=h pass=s3cr3t-password x"} {
		_, err := Replace(context.Background(), dsn, nil)
		if err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%q: err = %v", dsn, err)
		}
	}
}
