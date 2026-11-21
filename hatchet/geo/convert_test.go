package geo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

// A failing "mmdbbuilder convert" is an error (Publish stops before any upload) with a fixed
// text and the builder's last stderr lines; the GeoNames copy is passed only when it is there.
func TestConvertPublishedFailureIsAnError(t *testing.T) {
	date := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	for _, withGeoNames := range []bool{false, true} {
		p := evalPipeline(t, `echo "$@" > "$(dirname "$0")/args"; echo "boom" >&2; exit 1`)
		mmdb := filepath.Join(p.DataDir, "ip2geo-openipdata-20261120.mmdb")
		if withGeoNames {
			os.MkdirAll(p.evalDir(), 0o750)
			os.WriteFile(filepath.Join(p.evalDir(), geonamesEntry), []byte(strings.TrimSpace(geoNamesSample)+"\n"), 0o640)
		}
		var out PublishOutput
		_, err := p.convertPublished(context.Background(), date, mmdb, &out)
		if err == nil || !errors.Is(err, errConvert) || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("geonames %v: err = %v", withGeoNames, err)
		}
		wantGN := "skipped: no GeoNames copy"
		if withGeoNames {
			wantGN = "skipped: convert failed with GeoNames" // the second run (no GeoNames) wrote args last
		}
		if out.GeoNames != wantGN {
			t.Errorf("GeoNames = %q, want %q", out.GeoNames, wantGN)
		}
		args, _ := os.ReadFile(filepath.Join(p.DataDir, "args"))
		wantArgs := "convert -db " + mmdb + " -out " + filepath.Join(p.DataDir, "openipdata-org-ip2geo-20261120.mmdb")
		if strings.TrimSpace(string(args)) != wantArgs {
			t.Errorf("builder args = %q, want %q", args, wantArgs)
		}
	}
}

// A cancelled context is returned as it is.
func TestConvertPublishedCancelled(t *testing.T) {
	p := evalPipeline(t, `exit 1`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out PublishOutput
	_, err := p.convertPublished(ctx, time.Now(), filepath.Join(p.DataDir, "x.mmdb"), &out)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

// A good conversion returns the published path next to the flat file.
func TestConvertPublishedPath(t *testing.T) {
	p := evalPipeline(t, `echo "convert: ok"`)
	var out PublishOutput
	date := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	got, err := p.convertPublished(context.Background(), date, filepath.Join(p.DataDir, "ip2geo-openipdata-20261120.mmdb"), &out)
	if err != nil || got != filepath.Join(p.DataDir, "openipdata-org-ip2geo-20261120.mmdb") {
		t.Errorf("got %q, %v", got, err)
	}
}

// With a GeoNames copy that the first convert cannot use, the convert is run again without
// it (geonames "skipped: convert failed with GeoNames"); only a failure of that run is an error.
func TestConvertPublishedRetriesWithoutGeoNames(t *testing.T) {
	date := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	script := `echo "$@" >> "$(dirname "$0")/calls"; case "$*" in *-geonames*) echo "bad geonames" >&2; exit 1;; esac; for last; do :; done; echo data > "$last"; echo ok`
	p := evalPipeline(t, script)
	os.MkdirAll(p.evalDir(), 0o750)
	os.WriteFile(filepath.Join(p.evalDir(), geonamesEntry), []byte(strings.TrimSpace(geoNamesSample)+"\n"), 0o640)
	var out PublishOutput
	mmdb := filepath.Join(p.DataDir, "ip2geo-openipdata-20261120.mmdb")
	got, err := p.convertPublished(context.Background(), date, mmdb, &out)
	if err != nil || got != filepath.Join(p.DataDir, "openipdata-org-ip2geo-20261120.mmdb") {
		t.Fatalf("got %q, %v", got, err)
	}
	if out.GeoNames != "skipped: convert failed with GeoNames" {
		t.Errorf("GeoNames = %q", out.GeoNames)
	}
	calls, _ := os.ReadFile(filepath.Join(p.DataDir, "calls"))
	if lines := strings.Split(strings.TrimSpace(string(calls)), "\n"); len(lines) != 2 || !strings.Contains(lines[0], "-geonames") || strings.Contains(lines[1], "-geonames") {
		t.Errorf("calls = %q", calls)
	}

	// Both runs fail: an error.
	p = evalPipeline(t, `echo boom >&2; exit 1`)
	os.MkdirAll(p.evalDir(), 0o750)
	os.WriteFile(filepath.Join(p.evalDir(), geonamesEntry), []byte(strings.TrimSpace(geoNamesSample)+"\n"), 0o640)
	if _, err := p.convertPublished(context.Background(), date, mmdb, &out); !errors.Is(err, errConvert) {
		t.Errorf("err = %v", err)
	}
}

type fakeR2 struct {
	err  error
	keys []string
}

func (f *fakeR2) Put(_ context.Context, key, _ string, _ publish.PutOptions) (string, publish.PutStatus, error) {
	f.keys = append(f.keys, key)
	if f.err != nil {
		return "", "", f.err
	}
	return "r2://b/" + key, publish.PutUploaded, nil
}

type fakeRelease struct {
	calls []string
}

func (f *fakeRelease) Replace(_ context.Context, _, asset, _ string) (string, bool, error) {
	f.calls = append(f.calls, asset)
	return "https://example.test/" + asset, true, nil
}

// The order of publishFiles: a failed convert uploads nothing; otherwise R2 first, then the
// release, both with the new names; a failed R2 put stops before the release.
func TestPublishFilesOrder(t *testing.T) {
	date := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	ok := `for last; do :; done; echo data > "$last"; echo ok`
	t.Run("convert fails", func(t *testing.T) {
		p := evalPipeline(t, `echo boom >&2; exit 1`)
		r2, rel := &fakeR2{}, &fakeRelease{}
		var out PublishOutput
		err := p.publishFiles(context.Background(), r2, rel, date, filepath.Join(p.DataDir, "flat.mmdb"), &out)
		if !errors.Is(err, errConvert) || len(r2.keys) != 0 || len(rel.calls) != 0 {
			t.Errorf("err %v, keys %v, calls %v", err, r2.keys, rel.calls)
		}
	})
	t.Run("success", func(t *testing.T) {
		p := evalPipeline(t, ok)
		r2, rel := &fakeR2{}, &fakeRelease{}
		var out PublishOutput
		if err := p.publishFiles(context.Background(), r2, rel, date, filepath.Join(p.DataDir, "flat.mmdb"), &out); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(r2.keys, []string{"2026-11-20/openipdata-org-ip2geo-20261120.mmdb"}) || !slices.Equal(rel.calls, []string{"openipdata-org-ip2geo.mmdb"}) || out.Bytes == 0 {
			t.Errorf("keys %v, calls %v, out %+v", r2.keys, rel.calls, out)
		}
	})
	t.Run("r2 fails", func(t *testing.T) {
		p := evalPipeline(t, ok)
		r2, rel := &fakeR2{err: errors.New("r2 down")}, &fakeRelease{}
		var out PublishOutput
		if err := p.publishFiles(context.Background(), r2, rel, date, filepath.Join(p.DataDir, "flat.mmdb"), &out); err == nil || len(rel.calls) != 0 {
			t.Errorf("err %v, calls %v", err, rel.calls)
		}
	})
}
