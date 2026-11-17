package geo

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
	"openipdata.org/hatchet/schema"
)

// The three reads of public.asn take only the prefixes object; the fallback ones the whole json.
func TestQueriesReadReducedJSON(t *testing.T) {
	for name, q := range map[string]string{"announced": announcedQuery, "pdb": pdbQuery, "hosting": hostingQuery} {
		sel := q[:strings.Index(q, "FROM")]
		if !strings.Contains(sel, ipdata.PrefixesJSON) || strings.Contains(strings.ReplaceAll(sel, ipdata.PrefixesJSON, ""), "a.json") {
			t.Errorf("%s: SELECT list is not the reduced json: %s", name, sel)
		}
	}
	for name, c := range map[string]struct{ whole, reduced string }{
		"announced": {announcedWholeQuery, announcedQuery},
		"pdb":       {pdbWholeQuery, pdbQuery},
		"hosting":   {hostingWholeQuery, hostingQuery},
	} {
		sel := c.whole[:strings.Index(c.whole, "FROM")]
		if !strings.Contains(sel, "a.json") || strings.Contains(sel, "jsonb") {
			t.Errorf("%s: whole query does not select a.json: %s", name, sel)
		}
		// Only the json expression differs: WHERE, ORDER BY and the other columns are the same.
		if strings.ReplaceAll(c.reduced, ipdata.PrefixesJSON, "a.json") != c.whole {
			t.Errorf("%s: the two queries differ beyond the json column", name)
		}
	}
}

// writeASNPrefixRows against a throwaway database (OPENIPDATA_TEST_PGSQL): the reduced and the
// whole form write identical rows, and a damaged value makes the read repeat with the whole
// json without writing a row twice.
func TestWriteASNPrefixRowsPostgres(t *testing.T) {
	baseDSN := os.Getenv("OPENIPDATA_TEST_PGSQL")
	if baseDSN == "" {
		t.Skip("set OPENIPDATA_TEST_PGSQL to run disposable Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatal(schema.HideDSN(err))
	}
	name := fmt.Sprintf("geo_asnjson_it_%d", time.Now().UnixNano())
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
	defer conn.Close(ctx)
	setup := []string{
		`CREATE TABLE public.asn (asn bigint PRIMARY KEY, country text, json text NOT NULL DEFAULT '', seen_at timestamptz)`,
		`CREATE TABLE public.asn_geo_pdb (asn bigint PRIMARY KEY, country text, city text)`,
		`INSERT INTO public.asn VALUES
			(1,'DE','{"asn":1,"metadata":{"x":[1,2]},"prefixes":{"ipv4":["8.8.8.8/24"],"ipv6":["2a00::/32"]}}',now()),
			(2,'DE','{"prefixes":null}',now()),
			(3,'DE','{"prefixes":{}}',now()),
			(4,'DE','',now()),
			(5,'DE','[1,2]',now()),
			(6,'DE','{"asn":6}',now()),
			(7,'DE','{"prefixes":{"ipv4":["9.9.9.0/24"]}}',now())`,
		`INSERT INTO public.asn_geo_pdb SELECT asn, 'DE', 'Berlin' FROM public.asn`,
	}
	for _, stmt := range setup {
		if _, err = conn.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	rowsOf := func(reduced, whole string) (string, int, int) {
		var sb strings.Builder
		w := csv.NewWriter(&sb)
		asns, prefixes, err := writeASNPrefixRows(ctx, conn, reduced, whole, w)
		if err != nil {
			t.Fatal(err)
		}
		w.Flush()
		return sb.String(), asns, prefixes
	}
	reduced, rAsns, rPrefixes := rowsOf(pdbQuery, pdbWholeQuery)
	whole, wAsns, wPrefixes := rowsOf(pdbWholeQuery, pdbWholeQuery)
	if reduced != whole || rAsns != wAsns || rPrefixes != wPrefixes || rAsns != 2 || rPrefixes != 3 {
		t.Fatalf("reduced and whole differ:\n%s\n%d %d\n--\n%s\n%d %d", reduced, rAsns, rPrefixes, whole, wAsns, wPrefixes)
	}
	// A damaged value fails the reduced query; the repeat gives the rows of the whole query once.
	if _, err = conn.Exec(ctx, `INSERT INTO public.asn VALUES (8,'DE','{broken',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO public.asn_geo_pdb VALUES (8,'DE','Berlin')`); err != nil {
		t.Fatal(err)
	}
	again, aAsns, aPrefixes := rowsOf(pdbQuery, pdbWholeQuery)
	if again != reduced || aAsns != rAsns || aPrefixes != rPrefixes {
		t.Fatalf("after a damaged value:\n%s\n%d %d", again, aAsns, aPrefixes)
	}
	got, err := readAnnounced(ctx, u.String())
	if err != nil || len(got) != 3 {
		t.Fatalf("readAnnounced = %v, %v", got, err)
	}
	// Valid JSON that jsonb cannot store (a \u0000 escape, SQLSTATE 22P05): the repeat with the whole
	// json reads it, so its prefix is there.
	if _, err = conn.Exec(ctx, `DELETE FROM public.asn WHERE asn = 8`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO public.asn VALUES (9,'DE',$1,now())`, `{"name":"a\u0000b","prefixes":{"ipv4":["7.7.7.0/24"]}}`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO public.asn_geo_pdb VALUES (9,'DE','Berlin')`); err != nil {
		t.Fatal(err)
	}
	withNul, nAsns, nPrefixes := rowsOf(pdbQuery, pdbWholeQuery)
	if nAsns != 3 || nPrefixes != 4 || !strings.Contains(withNul, "7.7.7.0/24") {
		t.Fatalf("a document with a NUL escape:\n%s\n%d %d", withNul, nAsns, nPrefixes)
	}
	if got, err = readAnnounced(ctx, u.String()); err != nil || len(got) != 4 {
		t.Fatalf("readAnnounced with a NUL escape = %v, %v", got, err)
	}
}
