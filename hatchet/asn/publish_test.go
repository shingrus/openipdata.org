package asn

import (
	"context"
	"errors"
	"strings"
	"testing"
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
