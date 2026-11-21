package asn

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

var publishDate = time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)

func TestPublishFiles(t *testing.T) {
	const ok = `echo "$@" > "$(dirname "$0")/args"; echo "convert: 3 networks, 3 with an organization, 0 skipped"; for last; do :; done; echo data > "$last"`
	cases := []struct {
		name, script string
		r2           *fakeR2
		rel          *fakeRelease
		wantErr      string
		wantKeys     int
		wantCalls    []string
	}{
		{"convert fails: nothing uploaded", `echo "boom" >&2; exit 1`, &fakeR2{}, &fakeRelease{}, "convert to the published layout failed", 0, nil},
		{"uploaded", ok, &fakeR2{status: publish.PutUploaded}, &fakeRelease{}, "", 2, []string{releaseAsset, releaseSQLite}},
		{"r2 fails: release untouched", ok, &fakeR2{err: errors.New("r2 down")}, &fakeRelease{}, "r2 down", 1, nil},
		{"mmdb release fails", ok, &fakeR2{status: publish.PutUploaded}, &fakeRelease{failOn: releaseAsset}, "upload failed", 2, []string{releaseAsset}},
		{"sqlite release fails after mmdb", ok, &fakeR2{status: publish.PutUploaded}, &fakeRelease{failOn: releaseSQLite}, "the MMDB asset is already updated", 2, []string{releaseAsset, releaseSQLite}},
	}
	for _, tc := range cases {
		p, mmdb := builderPipeline(t, tc.script)
		sqlite := filepath.Join(p.DataDir, "asn.sqlite3")
		os.WriteFile(sqlite, []byte("sqlite"), 0o640)
		var out PublishOutput
		err := p.publishFiles(context.Background(), tc.r2, tc.rel, Source{Commit: "abc"}, publishDate, mmdb, sqlite, &out)
		if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
			continue
		}
		if tc.wantKeys == 0 && !errors.Is(err, errConvert) {
			t.Errorf("%s: err = %v, want errConvert", tc.name, err)
		}
		if len(tc.r2.keys) != tc.wantKeys || !slices.Equal(tc.rel.calls, tc.wantCalls) {
			t.Errorf("%s: r2 keys %v, release calls %v", tc.name, tc.r2.keys, tc.rel.calls)
		}
		for _, k := range tc.r2.keys {
			if strings.Contains(k, "ip2asn-openipdata") {
				t.Errorf("%s: the flat file was uploaded: %s", tc.name, k)
			}
		}
		if tc.name == "uploaded" {
			if tc.r2.keys[0] != "2026-11-21/openipdata-org-ip2asn-20261121.mmdb" || tc.r2.keys[1] != "2026-11-21/asn.sqlite3" {
				t.Errorf("keys %v", tc.r2.keys)
			}
			if !strings.Contains(tc.rel.bodies[releaseAsset], "Built: 2026-11-21") || tc.rel.bodies[releaseSQLite] != "" {
				t.Errorf("bodies %v", tc.rel.bodies)
			}
			args, _ := os.ReadFile(filepath.Join(p.DataDir, "args"))
			want := "convert -db " + mmdb + " -out " + filepath.Join(p.DataDir, "openipdata-org-ip2asn-20261121.mmdb")
			if strings.TrimSpace(string(args)) != want {
				t.Errorf("args %q, want %q", args, want)
			}
			if out.Bytes != int64(len("data\n")+len("sqlite")) || len(out.R2) != 2 {
				t.Errorf("out = %+v", out)
			}
		}
	}
}

func TestPublishFilesCancelled(t *testing.T) {
	p, mmdb := builderPipeline(t, `exit 1`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out PublishOutput
	if err := p.publishFiles(ctx, &fakeR2{}, &fakeRelease{}, Source{}, publishDate, mmdb, "x.sqlite3", &out); !errors.Is(err, context.Canceled) {
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
		if f.calls[0] != "openipdata-org-ip2asn.mmdb" || (len(f.calls) > 1 && f.calls[1] != "asn-openipdata.sqlite3") {
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
