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
	for _, dsn := range []string{"s3cr3t-password", "host=h pass=s3cr3t-password x"} {
		_, err := connectEnsure(context.Background(), dsn)
		if err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("connectEnsure %q: err = %v", dsn, err)
		}
	}
	if _, err := connect(context.Background(), ""); err == nil {
		t.Error("no error without PGSQL")
	}
	if _, err := connectEnsure(context.Background(), ""); err == nil {
		t.Error("connectEnsure: no error without PGSQL")
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

func TestPickQueriesReadOnly(t *testing.T) {
	for name, q := range map[string]string{"new": pickNewQuery, "due": pickDueQuery, "revived": pickRevivedQuery} {
		for _, part := range []string{"family(ip) = 4", "interval '30 days'", "public.traced_networks", "LIMIT $1", "$2::text[]::inet[]"} {
			if !strings.Contains(q, part) {
				t.Errorf("%s query lacks %q", name, part)
			}
		}
		for _, bad := range []string{"INSERT", "UPDATE", "ALTER", "DELETE", "CREATE"} {
			if strings.Contains(strings.ToUpper(q), bad) {
				t.Errorf("%s query contains %s", name, bad)
			}
		}
	}
	for _, part := range []string{"ORDER BY recent DESC", "NOT EXISTS (SELECT 1 FROM public.traced_networks"} {
		if !strings.Contains(pickNewQuery, part) {
			t.Errorf("pickNewQuery lacks %q", part)
		}
	}
	for _, part := range []string{"t.next_trace_at <= now()", "ORDER BY t.next_trace_at", "LEFT JOIN newest"} {
		if !strings.Contains(pickDueQuery, part) {
			t.Errorf("pickDueQuery lacks %q", part)
		}
	}
	for _, part := range []string{"t.next_trace_at IS NULL", "t.status <> 'located'", "interval '60 days'", "n.seen_at > t.last_traced_at", "JOIN newest"} {
		if !strings.Contains(pickRevivedQuery, part) {
			t.Errorf("pickRevivedQuery lacks %q", part)
		}
	}
	if maxNetworksPerRun != 1000 {
		t.Errorf("maxNetworksPerRun = %d", maxNetworksPerRun)
	}
}

func row(prefix string, eligible int) pickRow {
	return pickRow{Prefix: prefix, Visitor: "9.9.9.9", Recent: 1, Eligible: eligible}
}

// Never-traced /24s come first, then the due ones, then those a visitor brought back; each
// tier keeps its own order, and the limit cuts the later tiers first.
func TestMergePicksOrdering(t *testing.T) {
	newRows := []pickRow{row("1.0.0.0/24", 2), row("1.0.1.0/24", 2)}
	dueRows := []pickRow{row("2.0.0.0/24", 3), row("2.0.1.0/24", 3), row("2.0.2.0/24", 3)}
	revived := []pickRow{row("3.0.0.0/24", 1)}
	var order func(b []pickRow) string = func(b []pickRow) string {
		var parts []string
		for _, r := range b {
			parts = append(parts, r.Prefix)
		}
		return strings.Join(parts, " ")
	}

	got, eligible := mergePicks(10, newRows, dueRows, revived)
	if order(got) != "1.0.0.0/24 1.0.1.0/24 2.0.0.0/24 2.0.1.0/24 2.0.2.0/24 3.0.0.0/24" || eligible != 6 {
		t.Errorf("got %s eligible %d", order(got), eligible)
	}
	for i, tier := range []int{tierNew, tierNew, tierDue, tierDue, tierDue, tierRevived} {
		if got[i].Tier != tier {
			t.Errorf("row %d tier = %d, want %d", i, got[i].Tier, tier)
		}
	}
	// The limit: the revived ones are cut first, then the due ones; eligible still counts everything.
	got, eligible = mergePicks(4, newRows, dueRows, revived)
	if order(got) != "1.0.0.0/24 1.0.1.0/24 2.0.0.0/24 2.0.1.0/24" || eligible != 6 {
		t.Errorf("limit 4: %s eligible %d", order(got), eligible)
	}
	got, _ = mergePicks(1, newRows, dueRows, revived)
	if order(got) != "1.0.0.0/24" {
		t.Errorf("limit 1: %s", order(got))
	}
	// A /24 in two tiers is picked once, as the earlier tier.
	got, _ = mergePicks(10, []pickRow{row("1.0.0.0/24", 1)}, []pickRow{row("1.0.0.0/24", 1), row("2.0.0.0/24", 1)})
	if order(got) != "1.0.0.0/24 2.0.0.0/24" || got[0].Tier != tierNew {
		t.Errorf("duplicate: %s", order(got))
	}
	// A tier with no rows adds nothing; with none at all the batch is empty.
	if got, eligible = mergePicks(10, nil, nil, nil); len(got) != 0 || eligible != 0 {
		t.Errorf("empty: %v %d", got, eligible)
	}
	// A full limit of the first tier leaves nothing for the others.
	many := make([]pickRow, 5)
	for i := range many {
		many[i] = row("1.0."+string(rune('0'+i))+".0/24", 5)
	}
	if got, _ = mergePicks(5, many, dueRows); len(got) != 5 || got[4].Tier != tierNew {
		t.Errorf("full first tier: %v", got)
	}
}

// The visitor of a batch line is optional: a recheck of a located /24 has none.
func TestBatchWithoutVisitor(t *testing.T) {
	path := filepath.Join(t.TempDir(), batchFile)
	in := []batchEntry{{"5.6.7.0/24", ""}, {"20.30.40.0/24", "20.30.40.1"}}
	if err := writeBatch(path, in); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(strings.Split(string(data), "\n")[0], "visitor") {
		t.Errorf("an entry without a visitor writes none: %s", data)
	}
	got, dropped, err := readBatch(path)
	if err != nil || dropped != 0 || len(got) != 2 || got[0].Visitor.IsValid() || !got[1].Visitor.IsValid() {
		t.Fatalf("batch = %+v %d %v", got, dropped, err)
	}
	// A prefix with a bad visitor, or a private prefix without one, is still dropped.
	os.WriteFile(path, []byte(`{"prefix":"5.6.7.0/24","visitor":"zzz"}`+"\n"+`{"prefix":"10.1.2.0/24"}`+"\n"+`{"prefix":"5.6.7.0/24"}`+"\n"), 0o640)
	if got, dropped, _ := readBatch(path); len(got) != 1 || dropped != 2 {
		t.Errorf("batch = %+v dropped %d", got, dropped)
	}
}
