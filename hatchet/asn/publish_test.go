package asn

import (
	"context"
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
