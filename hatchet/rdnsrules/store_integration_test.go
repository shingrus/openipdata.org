package rdnsrules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"openipdata.org/hatchet/schema"
)

func TestStoreLifecyclePostgres(t *testing.T) {
	baseDSN := os.Getenv("OPENIPDATA_TEST_PGSQL")
	if baseDSN == "" {
		t.Skip("set OPENIPDATA_TEST_PGSQL to run disposable Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	name := fmt.Sprintf("rdnsrules_it_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()
	t.Cleanup(func() {
		admin.Exec(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`)
		admin.Close(context.Background())
	})
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	defer conn.Close(ctx)
	if err = schema.EnsureRDNSCityRules(ctx, conn); err != nil {
		t.Fatal(err)
	}
	domain := "rdnsrules-test.com"
	first := Candidate{Domain: domain, Pattern: `^(?:[a-z]+\.)*([a-z]+)\.example$`, Dictionary: "codes", Names: 3, Decoded: 3, Distinct: 1, Places: map[string]Place{"foo": {Country: "US", City: "Example City"}}}
	first.Version = version(first.Pattern, first.Dictionary, first.Places)
	out := Output{}
	if err := storeCandidates(ctx, conn, []Candidate{first}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Updated != 1 || out.Unchanged != 0 {
		t.Fatalf("first output: %+v", out)
	}
	var status, storedVersion string
	var places map[string][2]string
	if err := conn.QueryRow(ctx, `SELECT status,version,places FROM public.rdns_city_rules WHERE domain=$1`, domain).Scan(&status, &storedVersion, &places); err != nil {
		t.Fatal(err)
	}
	if status != "candidate" || storedVersion != first.Version || places["foo"] != [2]string{"US", "Example City"} {
		t.Fatalf("stored rule status=%s version=%s places=%v", status, storedVersion, places)
	}
	if _, err = conn.Exec(ctx, `UPDATE public.rdns_city_rules SET status='active',truth_hits=9,truth_misses=1,precision=0.9,measured_at=now() WHERE domain=$1`, domain); err != nil {
		t.Fatal(err)
	}
	out = Output{}
	if err = storeCandidates(ctx, conn, []Candidate{first}, &out); err != nil {
		t.Fatal(err)
	}
	var hits, misses int
	var precision float64
	if err = conn.QueryRow(ctx, `SELECT status,truth_hits,truth_misses,precision FROM public.rdns_city_rules WHERE domain=$1`, domain).Scan(&status, &hits, &misses, &precision); err != nil {
		t.Fatal(err)
	}
	if status != "active" || hits != 9 || misses != 1 || precision != 0.9 || out.Unchanged != 1 {
		t.Fatalf("unchanged rule lost its measurements: %s %d %d %.2f %+v", status, hits, misses, precision, out)
	}
	pipe := Pipeline{PGSQL: dsn}
	rejected, err := pipe.Reject(ctx, []string{domain, domain})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Rejected != 1 {
		t.Fatalf("rejection output: %+v", rejected)
	}
	out = Output{}
	if err = storeCandidates(ctx, conn, []Candidate{first}, &out); err != nil {
		t.Fatal(err)
	}
	var rejectedVersions []string
	if err = conn.QueryRow(ctx, `SELECT status,rejected_versions FROM public.rdns_city_rules WHERE domain=$1`, domain).Scan(&status, &rejectedVersions); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || len(rejectedVersions) == 0 || rejectedVersions[0] != first.Version {
		t.Fatalf("withdrawal history missing: status=%s versions=%v", status, rejectedVersions)
	}
	changed := first
	changed.Pattern += `(?:-changed)?`
	changed.Places = map[string]Place{"foo": {Country: "US", City: "Changed City"}}
	changed.Version = version(changed.Pattern, changed.Dictionary, changed.Places)
	out = Output{}
	if err = storeCandidates(ctx, conn, []Candidate{changed}, &out); err != nil {
		t.Fatal(err)
	}
	var hitsNull, missesNull, precisionNull, measuredNull bool
	if err = conn.QueryRow(ctx, `SELECT status,version,truth_hits IS NULL,truth_misses IS NULL,precision IS NULL,measured_at IS NULL,rejected_versions FROM public.rdns_city_rules WHERE domain=$1`, domain).Scan(&status, &storedVersion, &hitsNull, &missesNull, &precisionNull, &measuredNull, &rejectedVersions); err != nil {
		t.Fatal(err)
	}
	if status != "candidate" || storedVersion != changed.Version || !hitsNull || !missesNull || !precisionNull || !measuredNull || len(rejectedVersions) != 1 || rejectedVersions[0] != first.Version {
		t.Fatalf("changed rule did not reset measurements or preserve history: status=%s version=%s rejected=%v nulls=%v/%v/%v/%v", status, storedVersion, rejectedVersions, hitsNull, missesNull, precisionNull, measuredNull)
	}
	// Returning to a previously rejected version must not revive it or replace
	// the newer candidate row.
	out = Output{}
	if err = storeCandidates(ctx, conn, []Candidate{first}, &out); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `SELECT status,version FROM public.rdns_city_rules WHERE domain=$1`, domain).Scan(&status, &storedVersion); err != nil {
		t.Fatal(err)
	}
	if status != "candidate" || storedVersion != changed.Version || out.Unchanged != 1 {
		t.Fatalf("rejected version revived: status=%s version=%s output=%+v", status, storedVersion, out)
	}
	// The stored JSON remains the runtime array contract after content changes.
	var raw []byte
	if err = conn.QueryRow(ctx, `SELECT places FROM public.rdns_city_rules WHERE domain=$1`, domain).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var decoded map[string][2]string
	if err = json.Unmarshal(raw, &decoded); err != nil || decoded["foo"] != [2]string{"US", "Changed City"} {
		t.Fatalf("runtime places JSON: %s, %v", raw, err)
	}
}

func TestStoreKeepsOverridesOfOtherScopesPostgres(t *testing.T) {
	dsn := os.Getenv("OPENIPDATA_TEST_PGSQL")
	if dsn == "" {
		t.Skip("set OPENIPDATA_TEST_PGSQL to run disposable Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	defer conn.Close(ctx)
	if err = schema.EnsureRDNSCityRules(ctx, conn); err != nil {
		t.Fatal(err)
	}
	domain := fmt.Sprintf("codes-scope-%d.example", time.Now().UnixNano())
	defer conn.Exec(context.Background(), `DELETE FROM public.rdns_city_rules WHERE domain=$1`, domain)
	elsewhere := Code{Country: "RU", City: "Moscow", Origin: "manual", Scope: Scope{LabelFromRight: 3, TokenFromLeft: 1, Dictionary: "iata"}}
	here := Code{Country: "NL", City: "Amsterdam", Origin: "manual", Scope: Scope{LabelFromRight: 2, TokenFromLeft: 1, Dictionary: "iata"}}
	oldCodes, _ := json.Marshal(map[string]Code{"msk": elsewhere, "ams": here})
	if _, err = conn.Exec(ctx, `INSERT INTO public.rdns_city_rules(domain,pattern,dictionary,codes,version) VALUES($1,'x','iata',$2::jsonb,'old')`, domain, string(oldCodes)); err != nil {
		t.Fatal(err)
	}
	c := Candidate{Domain: domain, Pattern: `^([a-z]+)\.` + domain + `$`, Dictionary: "iata", Scope: here.Scope, Names: 3, Decoded: 3, Distinct: 1, Places: map[string]Place{"fra": {Country: "DE", City: "Frankfurt"}}}
	out := Output{}
	if err = storeCandidates(ctx, conn, []Candidate{c}, &out); err != nil {
		t.Fatal(err)
	}
	var codes map[string]Code
	var places map[string][2]string
	if err = conn.QueryRow(ctx, `SELECT codes,places FROM public.rdns_city_rules WHERE domain=$1`, domain).Scan(&codes, &places); err != nil {
		t.Fatal(err)
	}
	if codes["msk"] != elsewhere || codes["ams"] != here {
		t.Fatalf("overrides were not kept: %+v", codes)
	}
	if _, ok := places["msk"]; ok || places["ams"] != [2]string{"NL", "Amsterdam"} || places["fra"] != [2]string{"DE", "Frankfurt"} {
		t.Fatalf("only the override of the chosen extraction applies: %v", places)
	}
}

func TestLearnPipelineFiltersAndPublishesAtomicallyPostgres(t *testing.T) {
	baseDSN := os.Getenv("OPENIPDATA_TEST_PGSQL")
	if baseDSN == "" {
		t.Skip("set OPENIPDATA_TEST_PGSQL to run disposable Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	name := fmt.Sprintf("rdnsrules_learn_it_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()
	t.Cleanup(func() {
		admin.Exec(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`)
		admin.Close(context.Background())
	})
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	defer conn.Close(ctx)
	for _, stmt := range []string{
		`CREATE TABLE public.network_traces (target inet NOT NULL, candidates inet[], traced_at timestamptz NOT NULL)`,
		`CREATE TABLE public.ip_ptr_names (ip inet PRIMARY KEY, ptr text, status text NOT NULL)`,
		`CREATE TABLE public.asn (asn integer PRIMARY KEY, country text, json text, seen_at timestamptz)`,
		`INSERT INTO public.asn VALUES (1,'DE','{"prefixes":{"ipv4":["8.8.8.0/24"]}}',now())`,
		// A damaged json value must not stop the run (the fallback read ignores it).
		`INSERT INTO public.asn VALUES (2,'US','{broken',now())`,
	} {
		if _, err = conn.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	// Three distinct live router addresses support FRA. The target's own name counts as
	// input too (a different city, LON); a private address is excluded.
	for _, row := range []struct{ ip, ptr string }{
		{"8.8.8.1", "fra1.router.example.net"},
		{"8.8.8.2", "fra2.router.example.net"},
		{"8.8.8.3", "fra3.router.example.net"},
		{"8.8.8.9", "lon9.router.example.net"},
		{"10.0.0.1", "fra4.router.example.net"},
		{"8.8.8.4", "fra_4.router.example.net"}, // a label outside [a-z0-9-]: skipped and counted
	} {
		if _, err = conn.Exec(ctx, `INSERT INTO public.ip_ptr_names(ip,ptr,status) VALUES($1,$2,'ok')`, row.ip, row.ptr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = conn.Exec(ctx, `INSERT INTO public.network_traces VALUES
		('8.8.8.9',ARRAY['8.8.8.9'::inet,'8.8.8.1'::inet,'8.8.8.2'::inet,'8.8.8.3'::inet,'10.0.0.1'::inet,'8.8.8.4'::inet],now())`); err != nil {
		t.Fatal(err)
	}
	dict := Dictionaries{"iata": {"fra": {Country: "DE", City: "Frankfurt"}, "lon": {Country: "GB", City: "London"}}}
	// No hop_ips column yet (resolve-ips has not run its DDL): the learner reads the candidates only.
	p := Pipeline{PGSQL: dsn, dictionaries: dict}
	out, err := p.Learn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Domains != 1 || out.Candidates != 1 || out.Updated != 1 || out.Unsupported != 0 || out.LostSupport != 0 || out.NamesSkipped != 1 {
		t.Fatalf("learn output: %+v", out)
	}
	var names, decoded int
	var places map[string][2]string
	var pattern string
	if err = conn.QueryRow(ctx, `SELECT names,decoded,places,pattern FROM public.rdns_city_rules WHERE domain='example.net'`).Scan(&names, &decoded, &places, &pattern); err != nil {
		t.Fatal(err)
	}
	if names != 4 || decoded != 4 || places["fra"] != [2]string{"DE", "Frankfurt"} || places["lon"] != [2]string{"GB", "London"} || strings.Contains(pattern, "\\.") == false {
		t.Fatalf("unexpected learned row names=%d decoded=%d places=%v pattern=%q", names, decoded, places, pattern)
	}
	var count int
	if _, err = conn.Exec(ctx, `UPDATE public.network_traces SET traced_at=now()-interval '181 days'`); err != nil {
		t.Fatal(err)
	}
	out, err = p.Learn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Domains != 0 || out.Unsupported != 0 || out.LostSupport != 1 || out.Updated != 0 || out.Unchanged != 0 {
		t.Fatalf("lost-support output: %+v", out)
	}
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM public.rdns_city_rules WHERE domain='example.net'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("lost support removed rule: count=%d err=%v", count, err)
	}
	// A failed refresh is rejected before any rule-table write.
	p.dictionaries = nil
	p.load = func(context.Context) (Dictionaries, error) {
		return nil, fmt.Errorf("source contained sensitive details")
	}
	if _, err = p.Learn(ctx); err == nil || strings.Contains(err.Error(), "sensitive details") {
		t.Fatalf("refresh error was not safely reported: %v", err)
	}
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM public.rdns_city_rules`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed refresh changed rules: count=%d err=%v", count, err)
	}
}

// testDB creates a disposable database for one test and returns a connection and its DSN.
func testDB(t *testing.T, prefix string) (*pgx.Conn, string) {
	t.Helper()
	baseDSN := os.Getenv("OPENIPDATA_TEST_PGSQL")
	if baseDSN == "" {
		t.Skip("set OPENIPDATA_TEST_PGSQL to run disposable Postgres integration test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	name := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	t.Cleanup(func() {
		admin.Exec(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`)
		admin.Close(context.Background())
	})
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn, u.String()
}

// Up to three rows per domain, keyed by (domain, rank): measurements and status stay with
// their content when it moves to another rank, a withdrawn version stays withdrawn at any
// rank, overrides of any rank are kept, and a rejection withdraws every rank.
func TestStoreRanksAndRejectionPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, dsn := testDB(t, "rdnsrules_rank_it")
	if err := schema.EnsureRDNSCityRules(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := schema.EnsureRDNSCityRules(ctx, conn); err != nil { // idempotent
		t.Fatal(err)
	}
	domain := "ranks.example"
	mk := func(rank int, pattern, token, city string) Candidate {
		c := Candidate{Domain: domain, Rank: rank, Pattern: pattern, Dictionary: "iata", Scope: Scope{LabelFromRight: rank, TokenFromLeft: 1, Dictionary: "iata"},
			Names: 5, Decoded: 3, Distinct: 1, Places: map[string]Place{token: {Country: "DE", City: city}}}
		c.Version = version(c.Pattern, c.Dictionary, c.Places)
		return c
	}
	r1, r2, r3 := mk(1, `^a([a-z]+)\.ranks\.example$`, "fra", "Frankfurt"), mk(2, `^b([a-z]+)\.ranks\.example$`, "muc", "Munich"), mk(3, `^c([a-z]+)\.ranks\.example$`, "ber", "Berlin")
	out := Output{}
	if err := storeCandidates(ctx, conn, []Candidate{r1, r2, r3}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Updated != 3 {
		t.Fatalf("first store: %+v", out)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM public.rdns_city_rules WHERE domain=$1`, domain).Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows: %d %v", n, err)
	}
	// A fifth rank is refused by the table (rank 4 is the LLM rule of propose-codes).
	if _, err := conn.Exec(ctx, `INSERT INTO public.rdns_city_rules(domain,rank,pattern,dictionary,version) VALUES($1,5,'x','iata','x')`, domain); err == nil {
		t.Fatal("rank 5 accepted")
	}
	// Measurements of rank 2 follow its content to rank 1; a manual override on rank 3 is kept.
	manual := Code{Country: "AT", City: "Vienna", Origin: "manual", Scope: Scope{LabelFromRight: 1, TokenFromLeft: 1, Dictionary: "iata"}}
	codesJSON, _ := json.Marshal(map[string]Code{"vie": manual})
	if _, err := conn.Exec(ctx, `UPDATE public.rdns_city_rules SET status='active',truth_hits=7 WHERE domain=$1 AND rank=2`, domain); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE public.rdns_city_rules SET codes=$2::jsonb WHERE domain=$1 AND rank=3`, domain, string(codesJSON)); err != nil {
		t.Fatal(err)
	}
	moved := r2
	moved.Rank, moved.Scope = 1, Scope{LabelFromRight: 2, TokenFromLeft: 1, Dictionary: "iata"}
	moved.Version = version(moved.Pattern, moved.Dictionary, moved.Places)
	out = Output{}
	if err := storeCandidates(ctx, conn, []Candidate{moved}, &out); err != nil {
		t.Fatal(err)
	}
	var status, ver string
	var hits *int64
	if err := conn.QueryRow(ctx, `SELECT status,version,truth_hits FROM public.rdns_city_rules WHERE domain=$1 AND rank=1`, domain).Scan(&status, &ver, &hits); err != nil {
		t.Fatal(err)
	}
	if status != "active" || ver != moved.Version || hits == nil || *hits != 7 || out.Unchanged != 1 {
		t.Fatalf("moved content lost its status: status=%s hits=%v out=%+v", status, hits, out)
	}
	var codes map[string]Code
	if err := conn.QueryRow(ctx, `SELECT codes FROM public.rdns_city_rules WHERE domain=$1 AND rank=1`, domain).Scan(&codes); err != nil || codes["vie"] != manual {
		t.Fatalf("override of rank 3 not applied to rank 1 row: %+v %v", codes, err)
	}

	// Rejection withdraws every rank and remembers each version in its row.
	pipe := Pipeline{PGSQL: dsn}
	rej, err := pipe.Reject(ctx, []string{domain, "unknown-domain.example"})
	if err != nil {
		t.Fatal(err)
	}
	if rej.Rejected != 1 || rej.Unchanged != 1 {
		t.Fatalf("reject output: %+v", rej)
	}
	rows, err := conn.Query(ctx, `SELECT rank,status,version,rejected_versions FROM public.rdns_city_rules WHERE domain=$1 ORDER BY rank`, domain)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for rows.Next() {
		var rank int
		var st, v string
		var rv []string
		if err := rows.Scan(&rank, &st, &v, &rv); err != nil {
			t.Fatal(err)
		}
		if st != "rejected" || len(rv) != 1 || rv[0] != v {
			t.Errorf("rank %d: status=%s version=%s rejected=%v", rank, st, v, rv)
		}
		seen++
	}
	rows.Close()
	if seen != 3 {
		t.Fatalf("rejected %d rows", seen)
	}
	// Rejecting again changes nothing.
	if rej, err = pipe.Reject(ctx, []string{domain}); err != nil || rej.Rejected != 0 || rej.Unchanged != 1 {
		t.Fatalf("second rejection: %+v %v", rej, err)
	}
	// The rank 3 version cannot come back as rank 1: the domain's withdrawn versions count.
	back := r3
	back.Rank = 1
	out = Output{}
	if err := storeCandidates(ctx, conn, []Candidate{back}, &out); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT status,version FROM public.rdns_city_rules WHERE domain=$1 AND rank=1`, domain).Scan(&status, &ver); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || ver != moved.Version || out.Unchanged != 1 || out.Updated != 0 {
		t.Fatalf("withdrawn version revived at another rank: status=%s version=%s out=%+v", status, ver, out)
	}
}

// The learner end to end on the full schema: target names and every hop are inputs, a
// domain gets a rank 2 rule from the names rank 1 does not decode, and a second run changes nothing.
func TestLearnRanksTargetsAndHopsPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, dsn := testDB(t, "rdnsrules_ranks_learn_it")
	if err := schema.EnsureRDNS(ctx, conn); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE public.asn (asn integer PRIMARY KEY, country text, json text, seen_at timestamptz)`,
		`INSERT INTO public.asn VALUES (1,'DE','{"prefixes":{"ipv4":["8.8.8.0/24"]}}',now())`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil { // the learner never runs the ASN DDL
			t.Fatal(err)
		}
	}
	// Rank 1: router.example.net names "xx<city>N"; rank 2: customer pool names where the city
	// is the first token of the first label; both share the registered domain. The pool
	// names are the targets, the routers are hops that are not in candidates (hop_ips only).
	type row struct{ ip, ptr string }
	for _, r := range []row{
		{"8.8.8.1", "fra1.core.example.net"}, {"8.8.8.2", "fra2.core.example.net"}, {"8.8.8.3", "muc3.core.example.net"},
		{"8.8.8.4", "ber4.core.example.net"},
		{"8.8.8.101", "pool-ber.example.net"}, {"8.8.8.102", "pool-ham.example.net"}, {"8.8.8.103", "pool-muc.example.net"},
	} {
		if _, err := conn.Exec(ctx, `INSERT INTO public.ip_ptr_names(ip,ptr,status) VALUES($1,$2,'ok')`, r.ip, r.ptr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO public.network_traces(prefix,target_kind,target,candidates,hop_ips,hops) VALUES
		('8.8.8.0/24','visitor','8.8.8.101',ARRAY['8.8.8.101'::inet],ARRAY['8.8.8.1'::inet,'8.8.8.2'::inet,'8.8.8.3'::inet,'8.8.8.4'::inet],'[]'),
		('8.8.8.0/24','gateway','8.8.8.102',ARRAY['8.8.8.102'::inet],NULL,'[]'),
		('8.8.8.0/24','random','8.8.8.103',ARRAY['8.8.8.103'::inet],ARRAY[]::inet[],'[]')`); err != nil {
		t.Fatal(err)
	}
	dict := Dictionaries{"iata": {"fra": {Country: "DE", City: "Frankfurt"}, "muc": {Country: "DE", City: "Munich"}, "ber": {Country: "DE", City: "Berlin"}, "ham": {Country: "DE", City: "Hamburg"}}}
	p := Pipeline{PGSQL: dsn, dictionaries: dict}
	out, err := p.Learn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Domains != 1 || out.Candidates != 2 || out.ExtraRanks != 1 || out.Updated != 2 {
		t.Fatalf("learn output: %+v", out)
	}
	for rank, want := range map[int]string{1: "fra", 2: "ham"} {
		var places map[string][2]string
		var names, decoded int
		if err := conn.QueryRow(ctx, `SELECT places,names,decoded FROM public.rdns_city_rules WHERE domain='example.net' AND rank=$1`, rank).Scan(&places, &names, &decoded); err != nil {
			t.Fatalf("rank %d: %v", rank, err)
		}
		if _, ok := places[want]; !ok || decoded < MinSupport {
			t.Errorf("rank %d: places=%v names=%d decoded=%d", rank, places, names, decoded)
		}
	}
	out, err = p.Learn(ctx)
	if err != nil || out.Updated != 0 || out.Unchanged != 2 {
		t.Fatalf("second run: %+v %v", out, err)
	}
}

// A table created with the old per-domain key is reported, never altered or dropped, and the
// learner and rejection fail with the same fixed message.
func TestOldRuleTableLayoutDetectedPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, dsn := testDB(t, "rdnsrules_old_layout_it")
	if _, err := conn.Exec(ctx, `CREATE TABLE public.rdns_city_rules (domain TEXT PRIMARY KEY, pattern TEXT NOT NULL, dictionary TEXT NOT NULL, version TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO public.rdns_city_rules VALUES ('a.example','p','iata','v')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := schema.EnsureRDNSCityRules(ctx, conn); !errors.Is(err, schema.ErrOldRuleLayout) {
			t.Fatalf("EnsureRDNSCityRules = %v", err)
		}
	}
	p := Pipeline{PGSQL: dsn, dictionaries: Dictionaries{"iata": {}}}
	if _, err := p.Reject(ctx, []string{"a.example"}); !errors.Is(err, schema.ErrOldRuleLayout) {
		t.Fatalf("Reject = %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM public.rdns_city_rules`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("table was touched: %d %v", n, err)
	}
	// After the manual drop the same call creates the new layout.
	if _, err := conn.Exec(ctx, `DROP TABLE public.rdns_city_rules`); err != nil {
		t.Fatal(err)
	}
	if err := schema.EnsureRDNSCityRules(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := schema.EnsureRDNSCityRules(ctx, conn); err != nil {
		t.Fatal(err)
	}
}

// Superseded learned rows are retired only for domains that have current names; manual rows
// and rows of domains without names stay; a retired version cannot come back unchanged; a row
// whose content moved to another rank is a stale duplicate and is deleted.
func TestStoreRetiresSupersededLearnedRowsPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, _ := testDB(t, "rdnsrules_retire_it")
	if err := schema.EnsureRDNSCityRules(ctx, conn); err != nil {
		t.Fatal(err)
	}
	mk := func(domain string, rank int, token, city string) Candidate {
		c := Candidate{Domain: domain, Rank: rank, Pattern: `^([a-z]+)\.` + strings.ReplaceAll(domain, ".", `\.`) + `$`, Dictionary: "iata",
			Scope: Scope{LabelFromRight: 1, TokenFromLeft: rank, Dictionary: "iata"}, Names: 5, Decoded: 3, Distinct: 2,
			Places: map[string]Place{token: {Country: "US", City: city}, token + "x": {Country: "US", City: city + "x"}}}
		c.Version = version(c.Pattern, c.Dictionary, c.Places)
		return c
	}
	junk, nonames, manual, moved1 := mk("junk.example", 1, "sub", "Surabaya"), mk("nonames.example", 1, "ip", "Ip"), mk("manual.example", 1, "ma", "Halle"), mk("move.example", 1, "aaa", "A")
	moved2 := mk("move.example", 2, "bbb", "B")
	out := Output{}
	if err := storeCandidates(ctx, conn, []Candidate{junk, nonames, manual, moved1, moved2}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE public.rdns_city_rules SET origin='manual' WHERE domain='manual.example'`); err != nil {
		t.Fatal(err)
	}
	good := mk("junk.example", 1, "fra", "Frankfurt")
	// move.example: the content of rank 2 is now the best (rank 1); rank 1's old content is gone.
	nowRank1 := moved2
	nowRank1.Rank = 1
	observed := map[string]bool{"junk.example": true, "manual.example": true, "move.example": true} // nonames.example is not observed
	out = Output{}
	if err := storeRules(ctx, conn, []Candidate{good, nowRank1}, observed, &out); err != nil {
		t.Fatal(err)
	}
	if out.Retired != 0 {
		t.Fatalf("a replaced rank is not retired: %+v", out)
	}
	count := func(domain string) int {
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM public.rdns_city_rules WHERE domain=$1`, domain).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("move.example"); n != 1 {
		t.Fatalf("stale duplicate of the moved row kept: %d rows", n)
	}
	// A domain with names whose learned row is no longer found: retired, and it stays out.
	out = Output{}
	if err := storeRules(ctx, conn, nil, observed, &out); err != nil {
		t.Fatal(err)
	}
	var status string
	var rejected []string
	check := func(domain, wantStatus string, wantRejected int) {
		t.Helper()
		if err := conn.QueryRow(ctx, `SELECT status,rejected_versions FROM public.rdns_city_rules WHERE domain=$1 AND rank=1`, domain).Scan(&status, &rejected); err != nil {
			t.Fatal(err)
		}
		if status != wantStatus || len(rejected) != wantRejected {
			t.Fatalf("%s: status=%s rejected=%v", domain, status, rejected)
		}
	}
	if out.Retired != 2 { // junk.example (the good row) and move.example
		t.Fatalf("retired: %+v", out)
	}
	check("junk.example", "rejected", 1)
	check("move.example", "rejected", 1)
	check("manual.example", "candidate", 0)  // manual origin, never retired
	check("nonames.example", "candidate", 0) // no current names: kept
	// Retiring again does nothing, and the retired version cannot return unchanged.
	out = Output{}
	if err := storeRules(ctx, conn, []Candidate{good}, observed, &out); err != nil {
		t.Fatal(err)
	}
	check("junk.example", "rejected", 1)
	if out.Retired != 0 || out.Updated != 0 {
		t.Fatalf("revived or re-retired: %+v", out)
	}
	// An unobserved domain with a stored learned row stays whatever the run produced for others.
	out = Output{}
	if err := storeRules(ctx, conn, nil, map[string]bool{"other.example": true}, &out); err != nil || out.Retired != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	check("nonames.example", "candidate", 0)
}
