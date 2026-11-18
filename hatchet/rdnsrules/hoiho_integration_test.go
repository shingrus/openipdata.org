package rdnsrules

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"openipdata.org/hatchet/schema"
)

// hoihoTestDB creates an empty database (OPENIPDATA_TEST_PGSQL, as the other integration tests) and
// returns its DSN; it is dropped at the end of the test.
func hoihoTestDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("OPENIPDATA_TEST_PGSQL")
	if base == "" {
		t.Skip("set OPENIPDATA_TEST_PGSQL to run disposable Postgres integration test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	name := fmt.Sprintf("rdnsrules_hoiho_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	t.Cleanup(func() {
		admin.Exec(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`)
		admin.Close(context.Background())
	})
	return u.String()
}

// hoihoSpec is the content of a test file: 400 records under example.net, 199 under example.org and
// one under badexample.org (which is not under example.org).
type hoihoSpec struct {
	first string   // the hint of the first record: "chi" or "dal"
	omit  []string // domains left out
	code  string   // the hint code of the example.org records ("chi" by default)
	extra []string // more domains (each gets the usual regex and a Chicago hint)
}

func (sp hoihoSpec) file() []byte {
	var lines []string
	omit := map[string]bool{}
	for _, d := range sp.omit {
		omit[d] = true
	}
	add := func(domain string, hint testHint) {
		if !omit[domain] {
			lines = append(lines, recordLine(domain, []string{`^([a-z]{3})\.` + strings.ReplaceAll(domain, ".", `\.`) + `$`}, [][]string{{"iata"}}, hint))
		}
	}
	dallas := testHint{code: "dal", typ: "iata", lat: "32.8", lng: "-96.8", tp: 1, loc: `{"cc":"US","place":"Dallas"}`}
	for i := 0; i < 400; i++ {
		hint := chicago("chi", "iata", 1)
		if i == 0 && sp.first == "dal" {
			hint = dallas
		}
		add(fmt.Sprintf("x%03d.example.net", i), hint)
	}
	code := sp.code
	if code == "" {
		code = "chi"
	}
	for i := 0; i < 199; i++ {
		add(fmt.Sprintf("x%03d.example.org", i), chicago(code, "iata", 1))
	}
	add("badexample.org", chicago("chi", "iata", 1))
	for _, d := range sp.extra {
		add(d, chicago("chi", "iata", 1))
	}
	return []byte(strings.Join(lines, "\n"))
}

func TestImportHoihoPostgres(t *testing.T) {
	dsn := hoihoTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	defer conn.Close(ctx)
	g := hoihoTestGeo(t)
	imp := func(sp hoihoSpec) HoihoOutput {
		t.Helper()
		p := &Pipeline{DataDir: t.TempDir(), PGSQL: dsn, testHoiho: sp.file(), testGeo: g}
		out, err := p.ImportHoiho(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	count := func(where string) (n int) {
		t.Helper()
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM public.rdns_hoiho_rules WHERE `+where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	city := func(domain string) (c string) {
		t.Helper()
		if err := conn.QueryRow(ctx, `SELECT coalesce((places->'chi'->>1), (places->'dal'->>1)) FROM public.rdns_hoiho_rules WHERE domain = $1`, domain).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	setRejected := func(rejected bool, domains ...string) HoihoRejectOutput {
		t.Helper()
		out, err := (&Pipeline{PGSQL: dsn}).SetHoihoRejected(ctx, domains, rejected)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	const rejectedOrg = "status = 'rejected' AND domain LIKE '%.example.org'"

	// The first import writes.
	out := imp(hoihoSpec{})
	if !out.Written || out.Unchanged || out.Rows != 600 || out.Skipped != "" {
		t.Fatalf("first import: %+v", out)
	}
	if count("true") != 600 || count("status = 'candidate'") != 600 || count("seq = 1 AND plan = 'iata' AND class = 'good' AND length(version) = 64") != 600 {
		t.Fatal("rows not as written")
	}
	if city("x000.example.net") != "Chicago" {
		t.Fatalf("places %q", city("x000.example.net"))
	}
	// The second is unchanged and leaves the table alone.
	var stamp, stamp2 time.Time
	if err = conn.QueryRow(ctx, `SELECT max(imported_at) FROM public.rdns_hoiho_rules`).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if out = imp(hoihoSpec{}); out.Written || !out.Unchanged || out.Rows != 600 {
		t.Fatalf("second import: %+v", out)
	}
	if err = conn.QueryRow(ctx, `SELECT max(imported_at) FROM public.rdns_hoiho_rules`).Scan(&stamp2); err != nil || !stamp.Equal(stamp2) {
		t.Fatalf("table touched: %v %v %v", stamp, stamp2, err)
	}
	// A file with too few records, and one with records but too few rows: refused, the table stays.
	few := []byte(strings.Join(strings.Split(string(hoihoSpec{}.file()), "\n")[:10], "\n"))
	p := &Pipeline{DataDir: t.TempDir(), PGSQL: dsn, testHoiho: few, testGeo: g}
	if out, err = p.ImportHoiho(ctx); err != nil || out.Skipped != "too few records" || out.Written || count("true") != 600 {
		t.Fatalf("few records: %+v %v", out, err)
	}
	p = &Pipeline{DataDir: t.TempDir(), PGSQL: dsn, testHoiho: []byte(hoihoRecords(hoihoMinRecords, false)), testGeo: g}
	if out, err = p.ImportHoiho(ctx); err != nil || out.Skipped != "too few rows" || out.Written || count("true") != 600 {
		t.Fatalf("few rows: %+v %v", out, err)
	}
	// A changed file replaces.
	if out = imp(hoihoSpec{first: "dal"}); !out.Written || out.Unchanged || city("x000.example.net") != "Dallas" {
		t.Fatalf("changed import: %+v %q", out, city("x000.example.net"))
	}
	// A file that only changes score.class is not unchanged.
	p = &Pipeline{DataDir: t.TempDir(), PGSQL: dsn, testHoiho: []byte(strings.Replace(string(hoihoSpec{first: "dal"}.file()), `"good"`, `"promising"`, 1)), testGeo: g}
	if out, err = p.ImportHoiho(ctx); err != nil || !out.Written || count("class = 'promising'") != 1 {
		t.Fatalf("class change: %+v %v", out, err)
	}
	imp(hoihoSpec{first: "dal"}) // back to all good

	// Rejecting a domain switches it and its sub-suffixes off, not a name that only ends the same way.
	rej := setRejected(true, "Example.Org.")
	if rej.Domains != 1 || rej.DomainsChanged != 1 || rej.RowsChanged != 199 || rej.Rejected != 1 {
		t.Fatalf("reject: %+v", rej)
	}
	if count(rejectedOrg) != 199 || count("status = 'rejected'") != 199 {
		t.Fatalf("rejected rows: %d", count("status = 'rejected'"))
	}
	if rej = setRejected(true, "example.org"); rej.DomainsChanged != 0 || rej.RowsChanged != 0 || rej.Rejected != 1 {
		t.Fatalf("second reject: %+v", rej)
	}
	// A rejection survives an import of the same file (unchanged), a changed file ...
	if out = imp(hoihoSpec{first: "dal"}); !out.Unchanged || out.DomainsRejected != 199 {
		t.Fatalf("import after reject: %+v", out)
	}
	// ... a rejected domain's own rows changing ...
	if out = imp(hoihoSpec{first: "dal", code: "chx"}); !out.Written || count(rejectedOrg) != 199 || count("status = 'rejected'") != 199 {
		t.Fatalf("content change: %+v, rejected %d", out, count("status = 'rejected'"))
	}
	// ... a domain that leaves the file and returns in a later import ...
	var gone []string
	for i := 0; i < 50; i++ {
		gone = append(gone, fmt.Sprintf("x%03d.example.org", i))
	}
	if out = imp(hoihoSpec{first: "dal", omit: gone}); !out.Written || count("true") != 550 || count("status = 'rejected'") != 149 {
		t.Fatalf("leave: %+v", out)
	}
	if out = imp(hoihoSpec{first: "dal"}); !out.Written || count("true") != 600 || count(rejectedOrg) != 199 || count("status = 'rejected'") != 199 {
		t.Fatalf("return: %+v, rejected %d", out, count("status = 'rejected'"))
	}
	// ... and a new sub-suffix of a rejected domain, which arrives rejected.
	if out = imp(hoihoSpec{first: "dal", extra: []string{"new.sub.example.org"}}); !out.Written || count("domain = 'new.sub.example.org' AND status = 'rejected'") != 1 || count("status = 'rejected'") != 200 {
		t.Fatalf("new sub-suffix: %+v", out)
	}
	// Rejecting an unknown domain changes no row but is remembered.
	if rej = setRejected(true, "unknown.example.com"); rej.DomainsChanged != 1 || rej.RowsChanged != 0 || rej.Rejected != 2 {
		t.Fatalf("unknown domain: %+v", rej)
	}
	if out = imp(hoihoSpec{first: "dal", extra: []string{"new.sub.example.org", "a.unknown.example.com"}}); !out.Written || count("domain = 'a.unknown.example.com' AND status = 'rejected'") != 1 {
		t.Fatalf("remembered: %+v", out)
	}
	// -reject (our own rules) no longer touches the Hoiho table.
	before := count("status = 'rejected'")
	if _, err = (&Pipeline{PGSQL: dsn}).Reject(ctx, []string{"example.net"}); err != nil {
		t.Fatal(err)
	}
	if count("status = 'rejected'") != before || count("status = 'candidate' AND domain LIKE '%.example.net'") != 400 {
		t.Fatal("-reject touched the Hoiho rules")
	}
	// Accepting brings the rows back, and the next import keeps them.
	rej = setRejected(false, "example.org", "unknown.example.com")
	if rej.DomainsChanged != 2 || rej.RowsChanged != before || rej.Rejected != 0 || count("status = 'rejected'") != 0 {
		t.Fatalf("accept: %+v, rejected %d, want %d rows", rej, count("status = 'rejected'"), before)
	}
	spec := hoihoSpec{first: "dal", extra: []string{"new.sub.example.org", "a.unknown.example.com"}}
	if out = imp(spec); !out.Unchanged || out.DomainsRejected != 0 || count("status = 'rejected'") != 0 {
		t.Fatalf("import after accept: %+v", out)
	}
	// A bad domain is a fixed error that does not echo it.
	if _, err = (&Pipeline{PGSQL: dsn}).SetHoihoRejected(ctx, []string{"not a host!"}, true); err == nil || strings.Contains(err.Error(), "not a host") {
		t.Fatalf("bad domain: %v", err)
	}
	// Reject before the tables exist works too.
	fresh := hoihoTestDB(t)
	if r, err := (&Pipeline{PGSQL: fresh}).Reject(ctx, []string{"example.org"}); err != nil || r.Rejected != 0 || r.Unchanged != 1 {
		t.Fatalf("reject without table: %+v %v", r, err)
	}
	if r, err := (&Pipeline{PGSQL: fresh}).SetHoihoRejected(ctx, []string{"example.org"}, true); err != nil || r.DomainsChanged != 1 || r.RowsChanged != 0 {
		t.Fatalf("hoiho reject without table: %+v %v", r, err)
	}
}
