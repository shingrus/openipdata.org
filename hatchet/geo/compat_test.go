package geo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A failing "mmdbbuilder convert" gives the outcome "failed: convert failed" and no error, so
// Publish goes on; the GeoNames copy is passed only when it is there.
func TestPublishCompatConvertFailureIsNotAnError(t *testing.T) {
	date := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	for _, withGeoNames := range []bool{false, true} {
		p := evalPipeline(t, `echo "$@" > "$(dirname "$0")/args"; echo "boom 10.1.2.3" >&2; exit 1`)
		mmdb := filepath.Join(p.DataDir, "ip2geo-openipdata-20261120.mmdb")
		if withGeoNames {
			os.MkdirAll(p.evalDir(), 0o750)
			os.WriteFile(filepath.Join(p.evalDir(), geonamesEntry), []byte(strings.TrimSpace(geoNamesSample)+"\n"), 0o640)
		}
		var out PublishOutput
		// The conversion fails before any upload, so no R2 or release is needed.
		if err := p.publishCompat(context.Background(), nil, nil, date, mmdb, &out); err != nil {
			t.Fatalf("geonames %v: %v", withGeoNames, err)
		}
		if out.Compat != "failed: convert failed" {
			t.Errorf("Compat = %q", out.Compat)
		}
		wantGN := "skipped: no GeoNames copy"
		if withGeoNames {
			wantGN = "on"
		}
		if out.CompatGeoNames != wantGN {
			t.Errorf("CompatGeoNames = %q, want %q", out.CompatGeoNames, wantGN)
		}
		args, _ := os.ReadFile(filepath.Join(p.DataDir, "args"))
		wantArgs := "convert -db " + mmdb + " -out " + filepath.Join(p.DataDir, "openipdata-org-ip2geo-20261120.mmdb")
		if withGeoNames {
			wantArgs += " -geonames " + filepath.Join(p.evalDir(), geonamesEntry)
		}
		if strings.TrimSpace(string(args)) != wantArgs {
			t.Errorf("builder args = %q, want %q", args, wantArgs)
		}
		if strings.Contains(out.Compat, "10.1.2.3") {
			t.Error("outcome carries builder output")
		}
	}
}

// A cancelled context is returned, not turned into an outcome.
func TestPublishCompatCancelled(t *testing.T) {
	p := evalPipeline(t, `exit 1`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out PublishOutput
	err := p.publishCompat(ctx, nil, nil, time.Now(), filepath.Join(p.DataDir, "x.mmdb"), &out)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
