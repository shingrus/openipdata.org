package publish

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestR2Presign(t *testing.T) {
	r, err := newR2("https://KEYID123:s3cr3tkey@abc123.r2.cloudflarestorage.com/openipdata-dbs", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(context.Context, string, time.Duration) (string, error){"get": r.PresignGet, "put": r.PresignPut} {
		u, err := f(context.Background(), "internal/ping/run/x.txt", 6*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"https://abc123.r2.cloudflarestorage.com/openipdata-dbs/internal/ping/run/x.txt", "X-Amz-Expires=21600", "X-Amz-Signature="} {
			if !strings.Contains(u, want) {
				t.Errorf("%s: %q lacks %q", name, u, want)
			}
		}
		if strings.Contains(u, "s3cr3tkey") {
			t.Errorf("%s: the secret is in the URL", name)
		}
	}
}
