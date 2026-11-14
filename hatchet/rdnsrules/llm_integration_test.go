package rdnsrules

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"openipdata.org/hatchet/schema"
)

func llmDB(t *testing.T, prefix string) (context.Context, *pgx.Conn, string) {
	t.Helper()
	conn, dsn := testDB(t, prefix)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := schema.EnsureRDNSCityRules(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := schema.EnsureRDNSLLMAnswers(ctx, conn); err != nil {
		t.Fatal(err)
	}
	return ctx, conn, dsn
}

// The rank check of a table made before rank 4 existed is widened once, and only then.
func TestRankCheckWidenedOncePostgres(t *testing.T) {
	conn, _ := testDB(t, "rdnsrules_widen_it")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The table as the learner created it before rank 4 (check 1 to 3).
	if _, err := conn.Exec(ctx, `CREATE TABLE public.rdns_city_rules (
		domain TEXT NOT NULL, rank SMALLINT NOT NULL DEFAULT 1 CHECK (rank BETWEEN 1 AND 3),
		pattern TEXT NOT NULL, dictionary TEXT NOT NULL, codes JSONB NOT NULL DEFAULT '{}'::jsonb, places JSONB NOT NULL DEFAULT '{}'::jsonb,
		version TEXT NOT NULL, names BIGINT NOT NULL DEFAULT 0, decoded BIGINT NOT NULL DEFAULT 0, distinct_places BIGINT NOT NULL DEFAULT 0,
		country_agree DOUBLE PRECISION, truth_hits BIGINT, truth_misses BIGINT, precision DOUBLE PRECISION,
		status TEXT NOT NULL DEFAULT 'candidate', origin TEXT NOT NULL DEFAULT 'learned', learned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		measured_at TIMESTAMPTZ, rejected_versions TEXT[] NOT NULL DEFAULT '{}', PRIMARY KEY (domain, rank))`); err != nil {
		t.Fatal(err)
	}
	insert := func(rank int) error {
		_, err := conn.Exec(ctx, `INSERT INTO public.rdns_city_rules(domain,rank,pattern,dictionary,version) VALUES ('a.net',$1,'p','llm','v')`, rank)
		return err
	}
	if insert(4) == nil {
		t.Fatal("rank 4 accepted before the widening")
	}
	for i := 0; i < 3; i++ { // idempotent
		if err := schema.EnsureRDNSCityRules(ctx, conn); err != nil {
			t.Fatal(err)
		}
	}
	if err := insert(4); err != nil {
		t.Fatalf("rank 4 after widening: %v", err)
	}
	if insert(5) == nil {
		t.Fatal("rank 5 accepted")
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid='public.rdns_city_rules'::regclass AND contype='c'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("check constraints: %d %v", n, err)
	}
	// A new table has the wide check from the start and Ensure leaves it alone.
	fresh, _ := testDB(t, "rdnsrules_fresh_it")
	if err := schema.EnsureRDNSCityRules(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Exec(ctx, `INSERT INTO public.rdns_city_rules(domain,rank,pattern,dictionary,version) VALUES ('a.net',4,'p','llm','v')`); err != nil {
		t.Fatalf("fresh table rank 4: %v", err)
	}
}

// Rank 4 belongs to propose-codes: the learner never touches, retires or deletes it, a
// rejection withdraws it like every rank, and a withdrawn version cannot come back.
func TestLLMRuleLifecyclePostgres(t *testing.T) {
	ctx, conn, dsn := llmDB(t, "rdnsrules_llmrule_it")
	st := &pgProposeStore{conn: conn}
	domain := "carrier.net"
	w := llmRuleWrite{Regex: terraRegex, Names: 8, Decoded: 6, Distinct: 3,
		Codes:  map[string]Code{"fnt": {Country: "DE", City: "Frankfurt am Main", Origin: "llm", Scope: Scope{Dictionary: "llm"}, GeoNameID: 1}},
		Places: map[string]Place{"fnt": {"DE", "Frankfurt am Main"}}}
	if why, err := st.StoreLLMRule(ctx, domain, w); err != nil || why != "" {
		t.Fatalf("store: %q %v", why, err)
	}
	var origin, dict, status string
	var rank int
	var places map[string][2]string
	var codes map[string]Code
	read := func() {
		t.Helper()
		if err := conn.QueryRow(ctx, `SELECT rank,origin,dictionary,status,places,codes FROM public.rdns_city_rules WHERE domain=$1 AND rank=4`, domain).Scan(&rank, &origin, &dict, &status, &places, &codes); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if origin != "llm" || dict != "llm" || status != "candidate" || places["fnt"] != [2]string{"DE", "Frankfurt am Main"} || codes["fnt"].GeoNameID != 1 {
		t.Fatalf("row %s %s %s %v %v", origin, dict, status, places, codes)
	}
	// The learner writes a rank 1 row for the domain: rank 4 stays, and its codes are not
	// copied into the learned row.
	learned := Candidate{Domain: domain, Rank: 1, Pattern: `^(?:[a-z]+\.)*([a-z]+)\.carrier\.net$`, Dictionary: "iata", Names: 3, Decoded: 3, Distinct: 2,
		Places: map[string]Place{"fnt": {"US", "Flint"}, "muc": {"DE", "Munich"}}, Scope: Scope{LabelFromRight: 1, TokenFromLeft: 1, Dictionary: "iata"}}
	out := Output{}
	if err := storeRules(ctx, conn, []Candidate{learned}, map[string]bool{domain: true}, &out); err != nil {
		t.Fatal(err)
	}
	read()
	var learnedCodes map[string]Code
	if err := conn.QueryRow(ctx, `SELECT codes FROM public.rdns_city_rules WHERE domain=$1 AND rank=1`, domain).Scan(&learnedCodes); err != nil || len(learnedCodes) != 0 {
		t.Fatalf("learned row codes %v %v", learnedCodes, err)
	}
	// Observed with no learned candidate at all: the learned row is retired, rank 4 is not.
	out = Output{}
	if err := storeRules(ctx, conn, nil, map[string]bool{domain: true}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Retired != 1 {
		t.Fatalf("output %+v", out)
	}
	read()
	if status != "candidate" {
		t.Fatalf("learner retired rank 4: %s", status)
	}
	// The same rule again keeps the row; an override of a learned row needs its version.
	if why, err := st.StoreLLMRule(ctx, domain, w); err != nil || why != "" {
		t.Fatal(why, err)
	}
	// Reject withdraws rank 4 too.
	rej, err := (&Pipeline{PGSQL: dsn}).Reject(ctx, []string{domain})
	if err != nil || rej.Rejected != 1 {
		t.Fatalf("%+v %v", rej, err)
	}
	read()
	var rejected []string
	if err := conn.QueryRow(ctx, `SELECT rejected_versions FROM public.rdns_city_rules WHERE domain=$1 AND rank=4`, domain).Scan(&rejected); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || len(rejected) != 1 || rejected[0] != version(w.Regex, "llm", w.Places) {
		t.Fatalf("not withdrawn: %s %v", status, rejected)
	}
	if why, err := st.StoreLLMRule(ctx, domain, w); err != nil || why != rejRejectedVersion {
		t.Fatalf("a withdrawn version came back: %q %v", why, err)
	}
	// A different answer replaces the row as a candidate and keeps the withdrawal history.
	w2 := w
	w2.Places = map[string]Place{"fnt": {"DE", "Frankfurt (Oder)"}}
	w2.Codes = map[string]Code{"fnt": {Country: "DE", City: "Frankfurt (Oder)", Origin: "llm", GeoNameID: 2}}
	if why, err := st.StoreLLMRule(ctx, domain, w2); err != nil || why != "" {
		t.Fatalf("%q %v", why, err)
	}
	read()
	if err := conn.QueryRow(ctx, `SELECT rejected_versions FROM public.rdns_city_rules WHERE domain=$1 AND rank=4`, domain).Scan(&rejected); err != nil {
		t.Fatal(err)
	}
	if status != "candidate" || places["fnt"][1] != "Frankfurt (Oder)" || len(rejected) != 1 {
		t.Fatalf("%s %v %v", status, places, rejected)
	}
}

func TestLLMAnswersStatusPostgres(t *testing.T) {
	ctx, conn, _ := llmDB(t, "rdnsrules_answers_it")
	st := &pgProposeStore{conn: conn}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	save := func(domain, kind, status, hash string, ago time.Duration, versions ...string) {
		t.Helper()
		err := st.SaveAnswer(ctx, answerRecord{AskedAt: at.Add(-ago), Kind: kind, Domain: domain, RequestID: "r", Model: "m", Hosts: 3, Patterns: []string{"p.0." + domain},
			Hash: hash, RuleVersions: versions, Answer: map[string]any{"a": 1}, Status: status, Verdict: verdictAccepted, TokensIn: 1, TokensOut: 2, Cost: 0.01})
		if err != nil {
			t.Fatal(err)
		}
	}
	save("a.net", kindPropose, statusComplete, "h-old", 40*24*time.Hour, "v0")
	save("a.net", kindPropose, statusComplete, "h-new", 10*24*time.Hour, "v1", "v2")
	save("a.net", kindPropose, statusIncomplete, "h-error", time.Hour) // newer, but incomplete: not an answer, but it must be retried
	save("a.net", kindSecond, statusComplete, "h-second", time.Minute) // a second opinion is no answer
	save("b.net", kindProposeSmall, statusIncomplete, "h", time.Hour)
	save("c.net", kindProposeSmall, statusComplete, "hc", time.Hour, "c1")
	save("e.net", kindPropose, statusIncomplete, "h", 2*time.Hour)
	save("e.net", kindPropose, statusComplete, "he", time.Hour, "e1") // the newest is complete again
	last, err := st.LastAnswers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := last["a.net"]
	if !a.HasComplete || a.Hash != "h-new" || !a.AskedAt.Equal(at.Add(-10*24*time.Hour)) || strings.Join(a.RuleVersions, ",") != "v1,v2" || !a.NewestIncomplete {
		t.Fatalf("a.net %+v", a)
	}
	if c := last["c.net"]; !c.HasComplete || c.Hash != "hc" || c.NewestIncomplete || len(c.RuleVersions) != 1 {
		t.Fatalf("c.net %+v", c)
	}
	if b := last["b.net"]; b.HasComplete || !b.NewestIncomplete {
		t.Fatalf("an incomplete answer counted as an answer: %+v", b)
	}
	if e := last["e.net"]; !e.HasComplete || e.NewestIncomplete || e.Hash != "he" {
		t.Fatalf("e.net %+v", e)
	}
	// A nil answer and nil slices are stored.
	if err := st.SaveAnswer(ctx, answerRecord{Kind: kindPropose, Domain: "d.net", RequestID: "r", Model: "m", Status: statusIncomplete, Verdict: verdictStopped, Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	// Learned versions: ranks 1-3 that are not withdrawn; rank 4 and rejected rows are left out.
	for _, q := range []string{
		`INSERT INTO public.rdns_city_rules(domain,rank,pattern,dictionary,version,status) VALUES ('v.net',1,'p','iata','l1','candidate'),('v.net',2,'p','iata','l2','rejected'),('v.net',3,'p','iata','l3','active'),('v.net',4,'p','llm','l4','candidate')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := st.LearnedVersions(ctx)
	if err != nil || len(versions["v.net"]) != 2 {
		t.Fatalf("%v %v", versions, err)
	}
	got := map[string]bool{}
	for _, v := range versions["v.net"] {
		got[v] = true
	}
	if !got["l1"] || !got["l3"] {
		t.Fatalf("%v", versions)
	}
}

func TestUpdateLearnedRowNeedsItsVersionPostgres(t *testing.T) {
	ctx, conn, _ := llmDB(t, "rdnsrules_update_it")
	st := &pgProposeStore{conn: conn}
	c := Candidate{Domain: "carrier.net", Rank: 1, Pattern: `^(?:[a-z]+\.)*([a-z]+)\.carrier\.net$`, Dictionary: "iata", Names: 3, Decoded: 3, Distinct: 2,
		Places: map[string]Place{"fnt": {"US", "Flint"}, "muc": {"DE", "Munich"}}, Scope: Scope{LabelFromRight: 1, TokenFromLeft: 1, Dictionary: "iata"}}
	out := Output{}
	if err := storeRules(ctx, conn, []Candidate{c}, nil, &out); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Rules(ctx, "carrier.net")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	old := rows[0]
	if _, err := conn.Exec(ctx, `UPDATE public.rdns_city_rules SET status='active',truth_hits=5,measured_at=now() WHERE domain='carrier.net'`); err != nil {
		t.Fatal(err)
	}
	newPlaces := map[string]Place{"fnt": {"DE", "Frankfurt am Main"}, "muc": {"DE", "Munich"}}
	override := Code{Country: "DE", City: "Frankfurt am Main", Origin: "llm", Scope: Scope{LabelFromRight: 1, TokenFromLeft: 1, Dictionary: "iata"}, GeoNameID: 1}
	u := rowUpdate{Rank: 1, OldVersion: "stale", NewVersion: version(old.Pattern, "iata", newPlaces), Codes: map[string]Code{"fnt": override}, Places: newPlaces}
	if ok, err := st.UpdateLearnedRow(ctx, "carrier.net", u); err != nil || ok {
		t.Fatalf("a stale version must not update: %v %v", ok, err)
	}
	u.OldVersion = old.Version
	if ok, err := st.UpdateLearnedRow(ctx, "carrier.net", u); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	rows, _ = st.Rules(ctx, "carrier.net")
	var hits *int64
	if err := conn.QueryRow(ctx, `SELECT truth_hits FROM public.rdns_city_rules WHERE domain='carrier.net'`).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	r := rows[0]
	if r.Version != u.NewVersion || r.Status != "candidate" || hits != nil || r.Places["fnt"] != [2]string{"DE", "Frankfurt am Main"} || r.Codes["fnt"].Origin != "llm" || r.Codes["fnt"].GeoNameID != 1 {
		t.Fatalf("row %+v hits %v", r, hits)
	}
	// The learner reproduces the same content with the override in force: unchanged.
	c.Places = map[string]Place{"fnt": {"US", "Flint"}, "muc": {"DE", "Munich"}}
	out = Output{}
	if err := storeRules(ctx, conn, []Candidate{c}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Unchanged != 1 || out.Updated != 0 {
		t.Fatalf("the learner undid the override: %+v", out)
	}
}

// ProposeCodes end to end: Postgres, a fake OpenRouter, a learned rule that the LLM corrects.
func TestProposeCodesEndToEndPostgres(t *testing.T) {
	conn, dsn := testDB(t, "rdnsrules_propose_it")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, stmt := range []string{
		`CREATE TABLE public.network_traces (target inet NOT NULL, candidates inet[], traced_at timestamptz NOT NULL)`,
		`CREATE TABLE public.ip_ptr_names (ip inet PRIMARY KEY, ptr text, status text NOT NULL)`,
		`CREATE TABLE public.asn (asn integer PRIMARY KEY, country text, json text, seen_at timestamptz)`,
		`INSERT INTO public.asn VALUES (1,'DE','{"prefixes":{"ipv4":["8.8.8.0/24"]}}',now())`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	tokens := []string{"fnt", "fnt", "muc", "muc", "ber", "ber", "ham", "ham"}
	var ips []string
	for i, tok := range tokens {
		ip := fmt.Sprintf("8.8.8.%d", i+1)
		ips = append(ips, "'"+ip+"'::inet")
		if _, err := conn.Exec(ctx, `INSERT INTO public.ip_ptr_names(ip,ptr,status) VALUES($1,$2,'ok')`, ip, fmt.Sprintf("r%d.%s.carrier.net", i+1, tok)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO public.network_traces VALUES ('8.8.8.1', ARRAY[`+strings.Join(ips, ",")+`], now())`); err != nil {
		t.Fatal(err)
	}
	// The learner decodes fnt as Flint (wrong for a German carrier).
	dict := Dictionaries{"iata": {"fnt": {Country: "US", City: "Flint"}, "muc": {Country: "DE", City: "Munich"}, "ber": {Country: "DE", City: "Berlin"}, "ham": {Country: "DE", City: "Hamburg"}}}
	g := testGeoIndex(t)
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/key") {
			fmt.Fprint(w, `{"data":{"limit_remaining":null}}`)
			return
		}
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		prompts = append(prompts, body)
		if strings.Contains(body, "m/luna") {
			fmt.Fprint(w, completion(`{"tokens": {"fnt": {"country": "DE", "region": "Hesse", "city": "Frankfurt am Main"}}}`, 0.001))
			return
		}
		fmt.Fprint(w, completion(`{"location": true, "regex": "^[a-z0-9-]+\\.([a-z]+)\\.carrier\\.net$", "codes": {
			"fnt": {"country": "DE", "region": "Hesse", "city": "Frankfurt"}, "muc": {"country": "DE", "city": "Munich"},
			"ber": {"country": "DE", "city": "Berlin"}, "ham": {"country": "DE", "city": "Hamburg"}}}`, 0.01))
	}))
	defer srv.Close()
	p := Pipeline{PGSQL: dsn, dictionaries: dict, OpenRouterKey: "sk-test", OpenRouterModel: "m/terra", OpenRouterSecondModel: "m/luna",
		OpenRouterURL: srv.URL, testGeo: g, llmBackoff: time.Millisecond}
	if _, err := p.Learn(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := p.ProposeCodes(ctx, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Eligible != 1 || out.Accepted != 1 || out.Overrides != 1 || out.Disagreements != 1 || out.Requests != 2 || out.Incomplete != 0 {
		t.Fatalf("%+v", out)
	}
	var places map[string][2]string
	var origin string
	if err = conn.QueryRow(ctx, `SELECT origin,places FROM public.rdns_city_rules WHERE domain='carrier.net' AND rank=4`).Scan(&origin, &places); err != nil || origin != "llm" || places["fnt"] != [2]string{"DE", "Frankfurt am Main"} {
		t.Fatalf("rank 4 %s %v %v", origin, places, err)
	}
	var codes map[string]Code
	if err = conn.QueryRow(ctx, `SELECT places,codes FROM public.rdns_city_rules WHERE domain='carrier.net' AND rank=1`).Scan(&places, &codes); err != nil || places["fnt"] != [2]string{"DE", "Frankfurt am Main"} || codes["fnt"].Origin != "llm" {
		t.Fatalf("learned row not overridden: %v %v %v", places, codes, err)
	}
	var complete int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM public.rdns_llm_answers WHERE status='complete'`).Scan(&complete); err != nil || complete != 2 {
		t.Fatalf("answers: %d %v", complete, err)
	}
	// Nothing was sent that holds a real name or address.
	for _, body := range prompts {
		if strings.Contains(body, "8.8.8.") || strings.Contains(body, "r1.fnt") {
			t.Fatalf("request leaks names: %s", body)
		}
	}
	// The override changed the learned version; the answer recorded the new one: a second run asks nothing.
	before := len(prompts)
	out, err = p.ProposeCodes(ctx, ProposeOptions{})
	if err != nil || out.Eligible != 0 || len(prompts) != before {
		t.Fatalf("second run %+v %v", out, err)
	}
	// force asks again whatever the record says.
	out, err = p.ProposeCodes(ctx, ProposeOptions{Force: true, Domains: []string{"carrier.net"}})
	if err != nil || out.Eligible != 1 || out.Accepted != 1 {
		t.Fatalf("force %+v %v", out, err)
	}
	// The learner runs again: the override and rank 4 survive.
	lout, err := p.Learn(ctx)
	if err != nil || lout.Updated != 0 {
		t.Fatalf("learn %+v %v", lout, err)
	}
	var n int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM public.rdns_city_rules WHERE domain='carrier.net' AND status='candidate'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows after the learner: %d %v", n, err)
	}
	// A learned row that changes (a re-learned version) is asked again at once, without the 30-day wait.
	if _, err = conn.Exec(ctx, `UPDATE public.rdns_city_rules SET version='changed-by-the-learner' WHERE domain='carrier.net' AND rank=1`); err != nil {
		t.Fatal(err)
	}
	out, err = p.ProposeCodes(ctx, ProposeOptions{})
	if err != nil || out.Eligible != 1 {
		t.Fatalf("a changed learned version must be asked again: %+v %v", out, err)
	}
	// No key: skipped before anything is read.
	noKey := Pipeline{PGSQL: dsn}
	if o, err := noKey.ProposeCodes(ctx, ProposeOptions{}); err != nil || o.Skipped != "no key" {
		t.Fatalf("%+v %v", o, err)
	}
}
