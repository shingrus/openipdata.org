package asn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

func TestPublishNeedsSettings(t *testing.T) {
	src := Source{Commit: "abc", DateTag: "20261109"}
	cases := []struct {
		name string
		p    Pipeline
		want string
	}{
		{"no R2", Pipeline{GitHubToken: "tok", GitHubRepo: "o/r"}, "R2 is not set"},
		{"bad R2", Pipeline{R2: "s3cr3t", GitHubToken: "tok", GitHubRepo: "o/r"}, "invalid R2 configuration"},
		{"no token", Pipeline{R2: "https://k:s3cr3t@acct.r2.cloudflarestorage.com/b", GitHubRepo: "o/r"}, "GITHUB_TOKEN is not set"},
	}
	for _, tc := range cases {
		_, err := tc.p.Publish(context.Background(), src, "a.mmdb", "asn.sqlite3")
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

type fakeRelease struct {
	unchanged map[string]bool // asset name -> digest matches
	failOn    string
	calls     []string
	bodies    map[string]string
}

func (f *fakeRelease) Replace(_ context.Context, path, asset, body string) (string, bool, error) {
	f.calls = append(f.calls, asset)
	if f.bodies == nil {
		f.bodies = map[string]string{}
	}
	f.bodies[asset] = body
	if asset == f.failOn {
		return "", false, errors.New("upload failed")
	}
	return "https://example.test/" + asset, !f.unchanged[asset], nil
}

type fakeR2 struct {
	status publish.PutStatus
	err    error
	keys   []string
}

func (f *fakeR2) Put(_ context.Context, key, _ string, _ publish.PutOptions) (string, publish.PutStatus, error) {
	f.keys = append(f.keys, key)
	if f.err != nil {
		return "", "", f.err
	}
	return "r2://b/" + key, f.status, nil
}

// builderPipeline has a fake mmdbbuilder (a shell script) in a temp dir.
func builderPipeline(t *testing.T, script string) (*Pipeline, string) {
	t.Helper()
	dir := t.TempDir()
	builder := filepath.Join(dir, "mmdbbuilder")
	if err := os.WriteFile(builder, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Pipeline{DataDir: dir, MMDBBuilder: builder}, filepath.Join(dir, "ip2asn-openipdata-20261121.mmdb")
}

var compatDate = time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)

func TestPublishCompatOutcomes(t *testing.T) {
	const ok = `echo "$@" > "$(dirname "$0")/args"; echo "convert: 3 networks, 3 with an organization, 0 skipped"; for last; do :; done; echo data > "$last"`
	cases := []struct {
		name, script string
		r2           *fakeR2
		rel          *fakeRelease
		want         string
		wantCalls    int
	}{
		{"convert fails", `echo "boom 10.1.2.3" >&2; exit 1`, &fakeR2{}, &fakeRelease{}, "failed: convert failed", 0},
		{"uploaded", ok, &fakeR2{status: publish.PutUploaded}, &fakeRelease{}, "uploaded", 1},
		{"r2 unchanged, release replaced", ok, &fakeR2{status: publish.PutUnchanged}, &fakeRelease{}, "uploaded", 1},
		{"both unchanged", ok, &fakeR2{status: publish.PutUnchanged}, &fakeRelease{unchanged: map[string]bool{compatAsset: true}}, "unchanged", 1},
		{"r2 fails", ok, &fakeR2{err: errors.New("r2 down")}, &fakeRelease{}, "failed: R2 upload failed", 0},
		{"release fails", ok, &fakeR2{status: publish.PutUploaded}, &fakeRelease{failOn: compatAsset}, "failed: release upload failed", 1},
	}
	for _, tc := range cases {
		p, mmdb := builderPipeline(t, tc.script)
		var out PublishOutput
		if err := p.publishCompat(context.Background(), tc.r2, tc.rel, compatDate, mmdb, &out); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if out.Compat != tc.want || len(tc.rel.calls) != tc.wantCalls || strings.Contains(out.Compat, "10.1.2.3") {
			t.Errorf("%s: Compat = %q, release calls %v", tc.name, out.Compat, tc.rel.calls)
		}
		if tc.wantCalls > 0 {
			if tc.rel.bodies[compatAsset] != "" || tc.r2.keys[0] != "2026-11-21/openipdata-org-ip2asn-20261121.mmdb" {
				t.Errorf("%s: body %q, keys %v", tc.name, tc.rel.bodies[compatAsset], tc.r2.keys)
			}
			args, _ := os.ReadFile(filepath.Join(p.DataDir, "args"))
			want := "convert -db " + mmdb + " -out " + filepath.Join(p.DataDir, "openipdata-org-ip2asn-20261121.mmdb")
			if strings.TrimSpace(string(args)) != want {
				t.Errorf("%s: args %q, want %q", tc.name, args, want)
			}
			if out.CompatBytes == 0 {
				t.Errorf("%s: CompatBytes = 0", tc.name)
			}
		}
	}
}

func TestPublishCompatCancelled(t *testing.T) {
	p, mmdb := builderPipeline(t, `exit 1`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out PublishOutput
	if err := p.publishCompat(ctx, &fakeR2{}, &fakeRelease{}, compatDate, mmdb, &out); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestPublishRelease(t *testing.T) {
	cases := []struct {
		name                         string
		unchanged                    map[string]bool
		failOn                       string
		wantErr                      bool
		wantMMDBSame, wantSQLiteSame bool
		wantCalls                    int
	}{
		{"both uploaded", nil, "", false, false, false, 2},
		{"sqlite unchanged, mmdb changed", map[string]bool{releaseSQLite: true}, "", false, false, true, 2},
		{"mmdb unchanged, sqlite changed", map[string]bool{releaseAsset: true}, "", false, true, false, 2},
		{"both unchanged", map[string]bool{releaseAsset: true, releaseSQLite: true}, "", false, true, true, 2},
		{"sqlite fails after mmdb", nil, releaseSQLite, true, false, false, 2},
		{"mmdb fails, sqlite not tried", nil, releaseAsset, true, false, false, 1},
	}
	for _, tc := range cases {
		f := &fakeRelease{unchanged: tc.unchanged, failOn: tc.failOn}
		var out PublishOutput
		p := &Pipeline{}
		err := p.publishRelease(context.Background(), f, "a.mmdb", "asn.sqlite3", "text", &out)
		if (err != nil) != tc.wantErr || len(f.calls) != tc.wantCalls {
			t.Errorf("%s: err = %v, calls = %v", tc.name, err, f.calls)
			continue
		}
		if f.calls[0] != "ip2asn-openipdata.mmdb" || (len(f.calls) > 1 && f.calls[1] != "asn-openipdata.sqlite3") {
			t.Errorf("%s: calls = %v", tc.name, f.calls)
		}
		if f.bodies[releaseAsset] != "text" || (len(f.calls) > 1 && f.bodies[releaseSQLite] != "") {
			t.Errorf("%s: bodies = %v", tc.name, f.bodies)
		}
		if tc.wantErr {
			if tc.failOn == releaseSQLite && (out.ReleaseURL == "" || out.ReleaseSQLiteURL != "" || !strings.Contains(err.Error(), "MMDB asset is already updated")) {
				t.Errorf("%s: out = %+v, err = %v", tc.name, out, err)
			}
			continue
		}
		if out.ReleaseUnchanged != tc.wantMMDBSame || out.ReleaseSQLiteUnchanged != tc.wantSQLiteSame || out.ReleaseSQLiteURL == "" {
			t.Errorf("%s: out = %+v", tc.name, out)
		}
	}
}
