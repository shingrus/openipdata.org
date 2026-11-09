package asn

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }

func TestExportSQLiteDomains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asn.sqlite3")
	records := []Record{{ASN: 1, IPAmount: "256", IPv4Amount: "256", IPv6Amount: "0.00", JSON: "{}"}}
	domainRows := []DomainRow{
		{ASN: 1, Domain: strPtr("www.cloudflare.com"), Source: strPtr("pdb")},
		{ASN: 2, Domain: strPtr("yandex.ru"), Source: strPtr("pdb")},   // PeeringDB: kept
		{ASN: 3, Domain: strPtr("yandex.ru"), Source: strPtr("rdap")},  // RDAP free mail: dropped
		{ASN: 4, Domain: strPtr("nic.or.kr"), Source: nil},             // older row, registry: dropped
		{ASN: 5, Domain: strPtr(" "), Source: nil},                     // old "nothing found" marker
		{ASN: 6, Domain: nil, Source: strPtr("rdap")},                  // looked up, nothing found
		{ASN: 7, Domain: strPtr("noc.ops.example.co.uk"), Source: nil}, // older row: cut to registered domain
		{ASN: 8, Domain: strPtr("wholesale.telekom.com"), Source: strPtr("rdap")},
	}
	stats, err := ExportSQLite(context.Background(), path, records, domainRows, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ASNs != 1 || stats.Domains != 4 {
		t.Errorf("stats = %+v, want 1 ASN and 4 domains", stats)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT asn, domain, coalesce(source, '') FROM asn_domain ORDER BY asn`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		asn            int64
		domain, source string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.asn, &r.domain, &r.source); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	want := []row{
		{1, "cloudflare.com", "pdb"},
		{2, "yandex.ru", "pdb"},
		{7, "example.co.uk", ""},
		{8, "telekom.com", "rdap"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("asn_domain = %v, want %v", got, want)
	}
}

// Every ASN of a run is stamped, including rows that did not change.
func TestMarkSeenSQL(t *testing.T) {
	if !strings.Contains(markSeenSQL, "UPDATE public.asn SET seen_at = now() WHERE asn = ANY($1)") {
		t.Errorf("markSeenSQL = %s", markSeenSQL)
	}
}
