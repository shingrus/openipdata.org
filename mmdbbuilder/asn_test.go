package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

func writeASN(t *testing.T, asDir, name, body string) {
	t.Helper()
	dir := filepath.Join(asDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aggregated.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildASN(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, "as")
	// A more specific prefix of a lower ASN inside a broader one of a higher ASN.
	writeASN(t, asDir, "1", `{"asn": 1, "metadata": {"handle": "ONE", "description": "One Inc.", "countryCode": "US"},
		"prefixes": {"ipv4": ["8.8.8.0/24"], "ipv6": ["2001:db8:1::/48"]}}`)
	writeASN(t, asDir, "2", `{"asn": 2, "metadata": {"handle": "TWO", "description": "Two LLC", "countryCode": "DE"},
		"prefixes": {"ipv4": ["8.8.0.0/16"], "ipv6": ["2001:db8::/32"]}}`)
	// The identical prefix announced by two ASNs: the lowest (1) wins.
	writeASN(t, asDir, "3", `{"asn": 3, "metadata": {"handle": "THREE"}, "prefixes": {"ipv4": ["8.8.8.0/24"], "ipv6": []}}`)
	// Host bits set: keeps the /24 coverage instead of shrinking to a /32.
	writeASN(t, asDir, "4", `{"asn": 4, "metadata": {"handle": null, "description": "Four", "countryCode": null},
		"prefixes": {"ipv4": ["9.9.9.1/24"], "ipv6": []}}`)
	writeASN(t, asDir, "5", `{"asn": 5, "metadata": {}, "prefixes": {"ipv4": [], "ipv6": []}}`)             // no prefixes
	writeASN(t, asDir, "6", `{"asn": 7, "metadata": {}, "prefixes": {"ipv4": ["10.0.0.0/8"], "ipv6": []}}`) // ASN mismatch
	writeASN(t, asDir, "8", `{"asn": 8, "metadata": {}, "prefixes": {"ipv4": ["not-a-prefix"], "ipv6": []}}`)
	writeASN(t, asDir, "9", `{"asn": 9, "metadata": {}, "prefixes": {"ipv4": ["2001:db8::/32"], "ipv6": []}}`)

	out := filepath.Join(root, "ip2asn.mmdb")
	if err := os.WriteFile(out, []byte("previous database"), 0o644); err != nil {
		t.Fatal(err)
	}
	stats, err := buildASN(asDir, out, false)
	if err != nil {
		t.Fatal(err)
	}
	want := asnStats{entries: 3, prefixes: 5, skipped: 4, duplicates: 1}
	if stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}

	reader, err := maxminddb.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	cases := map[string]string{
		"8.8.8.8":         "1", // more specific wins over AS2's /16
		"8.8.4.4":         "2",
		"9.9.9.200":       "4",
		"2001:db8:1::1":   "1",
		"2001:db8:ffff::": "2",
		"10.1.2.3":        "",
	}
	for ip, wantASN := range cases {
		var record struct {
			ASN     string `maxminddb:"asn"`
			Name    string `maxminddb:"name"`
			Country string `maxminddb:"country_code"`
		}
		if err := reader.Lookup(netip.MustParseAddr(ip)).Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record.ASN != wantASN {
			t.Errorf("%s: asn %q, want %q (%+v)", ip, record.ASN, wantASN, record)
		}
	}

	matches, _ := filepath.Glob(filepath.Join(root, "*.tmp"))
	if len(matches) != 0 {
		t.Errorf("temporary files left: %v", matches)
	}
}

func TestBuildASNFailureKeepsOldFile(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, "as")
	writeASN(t, asDir, "5", `{"asn": 5, "metadata": {}, "prefixes": {"ipv4": [], "ipv6": []}}`)
	out := filepath.Join(root, "ip2asn.mmdb")
	if err := os.WriteFile(out, []byte("previous database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := buildASN(asDir, out, false); err == nil {
		t.Fatal("want an error when nothing is usable")
	}
	if data, _ := os.ReadFile(out); string(data) != "previous database" {
		t.Errorf("existing file changed: %q", data)
	}
}
