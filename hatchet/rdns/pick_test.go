package rdns

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Connection strings pgx cannot parse well enough to redact the password.
func TestConnectErrorHidesDSN(t *testing.T) {
	for _, dsn := range []string{"s3cr3t-password", "host=h pass=s3cr3t-password x"} {
		_, err := connect(context.Background(), dsn)
		if err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%q: err = %v", dsn, err)
		}
	}
	if _, err := connect(context.Background(), ""); err == nil {
		t.Error("no error without PGSQL")
	}
	if err := (&Pipeline{}).CheckPGSQL(); err == nil {
		t.Error("CheckPGSQL accepts an empty PGSQL")
	}
}

func TestBatchFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), batchFile)
	in := []batchEntry{{"5.6.7.0/24", "5.6.7.9"}, {"20.30.40.0/24", "20.30.40.1"}}
	if err := writeBatch(path, in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left: %v", err)
	}
	got, dropped, err := readBatch(path)
	if err != nil || dropped != 0 || len(got) != 2 ||
		got[0].Prefix != netip.MustParsePrefix("5.6.7.0/24") || got[0].Visitor != netip.MustParseAddr("5.6.7.9") {
		t.Fatalf("batch = %+v %d %v", got, dropped, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", info.Mode())
	}
}

// A line that is not a public /24 with its visitor inside is dropped and
// counted, never quoted; a missing file is an empty batch.
func TestReadBatchDropsBadLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), batchFile)
	lines := []string{
		`{"prefix":"5.6.7.0/24","visitor":"5.6.7.9"}`,
		`{"prefix":"5.6.7.0/24","visitor":"5.6.8.9"}`,   // visitor outside
		`{"prefix":"5.6.7.5/24","visitor":"5.6.7.9"}`,   // not a network address
		`{"prefix":"5.6.0.0/16","visitor":"5.6.7.9"}`,   // not a /24
		`{"prefix":"10.1.2.0/24","visitor":"10.1.2.3"}`, // private
		`{"prefix":"2001:db8::/24","visitor":"2001:db8::1"}`,
		`{"prefix":"x","visitor":"y"}`,
		`not json`,
		``,
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o640)
	got, dropped, err := readBatch(path)
	if err != nil || len(got) != 1 || dropped != 7 {
		t.Fatalf("batch = %+v dropped %d err %v", got, dropped, err)
	}
	got, dropped, err = readBatch(filepath.Join(t.TempDir(), "missing"))
	if err != nil || len(got) != 0 || dropped != 0 {
		t.Errorf("missing file: %+v %d %v", got, dropped, err)
	}
}

func TestPickQueryReadsOnly(t *testing.T) {
	for _, part := range []string{
		"family(ip) = 4", "interval '30 days'", "interval '60 days'", "public.network_traces", "ORDER BY count(*) DESC", "LIMIT $1", "$2::text[]::inet[]",
	} {
		if !strings.Contains(pickQuery, part) {
			t.Errorf("pickQuery lacks %q", part)
		}
	}
	for _, bad := range []string{"INSERT", "UPDATE", "ALTER", "DELETE", "CREATE"} {
		if strings.Contains(strings.ToUpper(pickQuery), bad) {
			t.Errorf("pickQuery contains %s", bad)
		}
	}
	if maxNetworksPerRun != 1000 {
		t.Errorf("maxNetworksPerRun = %d", maxNetworksPerRun)
	}
}
