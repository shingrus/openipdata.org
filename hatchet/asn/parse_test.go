package asn

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"
)

func writeSource(t *testing.T, asDir, asn, body string) {
	t.Helper()
	dir := filepath.Join(asDir, asn)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aggregated.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseDir(t *testing.T) {
	asDir := t.TempDir()
	writeSource(t, asDir, "13335", `{
		"asn": 13335,
		"metadata": {"handle": " CLOUDFLARENET ", "description": "Cloudflare & Co.", "countryCode": "US",
			"country": "United States", "origin": "authoritative", "category": null},
		"prefixes": {"ipv4": ["1.1.1.0/24", "1.0.0.0/24"], "ipv6": ["2606:4700::/32"]}
	}`)
	writeSource(t, asDir, "64512", `{"asn": 64512, "metadata": {"countryCode": null}, "prefixes": {"ipv4": [], "ipv6": []}}`)
	writeSource(t, asDir, "100", `{"asn": 200, "metadata": {}, "prefixes": {"ipv4": [], "ipv6": []}}`)
	writeSource(t, asDir, "300", `{"asn": 300, "metadata": {}, "prefixes": {"ipv4": ["not-a-prefix"], "ipv6": []}}`)
	writeSource(t, asDir, "400", `{"asn": 400, "metadata": {}, "prefixes": {"ipv4": ["2001:db8::/32"], "ipv6": []}}`)

	var warnings []string
	result, err := ParseDir(asDir, func(msg string) { warnings = append(warnings, msg) })
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != 3 || len(warnings) != 3 {
		t.Fatalf("skipped=%d warnings=%v, want 3 skipped", result.Skipped, warnings)
	}
	if len(result.Records) != 2 || result.Records[0].ASN != 13335 || result.Records[1].ASN != 64512 {
		t.Fatalf("records = %+v", result.Records)
	}

	cf := result.Records[0]
	if *cf.Handle != "CLOUDFLARENET" || *cf.Organization != "Cloudflare & Co." || *cf.OrganizationSlug != "cloudflare-co" || *cf.Country != "US" {
		t.Errorf("fields = %q %q %q %q", *cf.Handle, *cf.Organization, *cf.OrganizationSlug, *cf.Country)
	}
	if cf.IPv4Amount != "512" || cf.IPv6Amount != "79228162514264337593543.95" {
		t.Errorf("amounts ipv4=%s ipv6=%s", cf.IPv4Amount, cf.IPv6Amount)
	}
	wantJSON := `{"asn":13335,"metadata":{"country":"United States","countryCode":"US","description":"Cloudflare & Co.","handle":"CLOUDFLARENET","origin":"authoritative"},"prefixes":{"ipv4":["1.1.1.0/24","1.0.0.0/24"],"ipv6":["2606:4700::/32"]},"country":"US"}`
	if cf.JSON != wantJSON {
		t.Errorf("json =\n%s\nwant\n%s", cf.JSON, wantJSON)
	}

	empty := result.Records[1]
	if empty.Handle != nil || empty.Country != nil || empty.IPAmount != "0" || empty.IPv6Amount != "0.00" {
		t.Errorf("empty record = %+v", empty)
	}
	if empty.JSON != `{"asn":64512,"metadata":{},"prefixes":{"ipv4":[],"ipv6":[]}}` {
		t.Errorf("empty json = %s", empty.JSON)
	}
}

func TestFormatIPv6Millions(t *testing.T) {
	cases := map[int64]string{0: "0.00", 4_999: "0.00", 5_000: "0.01", 1_234_567: "1.23", 1_995_000: "2.00"}
	for in, want := range cases {
		if got := formatIPv6Millions(big.NewInt(in)); got != want {
			t.Errorf("formatIPv6Millions(%d) = %s, want %s", in, got, want)
		}
	}
}
