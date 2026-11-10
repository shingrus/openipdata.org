package pdb

import (
	"reflect"
	"testing"
)

func TestComputeIX(t *testing.T) {
	dir := t.TempDir()
	writeDataset(t, dir, "ix",
		`{"id": 1, "city": "  Amsterdam ", "country": "nl"}`,
		`{"id": 2, "city": "", "country": "DE"}`,
		`{"id": 3, "city": "Nowhere", "country": ""}`,
		`{"id": 4, "city": "Paris", "country": "FRA"}`,
	)
	writeDataset(t, dir, "ixlan",
		`{"id": 10, "ix_id": 1}`,
		`{"id": 20, "ix_id": 2}`,
		`{"id": 30, "ix_id": 3}`,
		`{"id": 40, "ix_id": 4}`,
		`{"id": 50, "ix_id": 99}`,
	)
	writeDataset(t, dir, "ixpfx",
		`{"id": 1, "ixlan_id": 10, "prefix": "80.249.208.0/21", "protocol": "IPv4"}`,
		`{"id": 2, "ixlan_id": 10, "prefix": "2001:7f8:1::/64", "protocol": "IPv6"}`,
		`{"id": 3, "ixlan_id": 20, "prefix": "80.81.192.5/22", "protocol": "IPv4"}`, // host bits are masked
		`{"id": 4, "ixlan_id": 10, "prefix": "192.168.0.0/24", "protocol": "IPv4"}`, // RFC 1918
		`{"id": 5, "ixlan_id": 10, "prefix": "10.1.0.0/16", "protocol": "IPv4"}`,
		`{"id": 6, "ixlan_id": 10, "prefix": "fd00::/64", "protocol": "IPv6"}`,        // ULA
		`{"id": 7, "ixlan_id": 10, "prefix": "fe80::/64", "protocol": "IPv6"}`,        // link-local
		`{"id": 8, "ixlan_id": 10, "prefix": "2001:db8::/64", "protocol": "IPv6"}`,    // documentation
		`{"id": 9, "ixlan_id": 10, "prefix": "100.64.0.0/24", "protocol": "IPv4"}`,    // shared address space
		`{"id": 10, "ixlan_id": 10, "prefix": "not a prefix", "protocol": "IPv4"}`,    // bad
		`{"id": 11, "ixlan_id": 10, "prefix": "", "protocol": "IPv4"}`,                // empty
		`{"id": 12, "ixlan_id": 10, "prefix": "8.0.0.0/8", "protocol": "IPv4"}`,       // far too broad
		`{"id": 13, "ixlan_id": 30, "prefix": "198.32.118.0/24", "protocol": "IPv4"}`, // exchange without country
		`{"id": 14, "ixlan_id": 40, "prefix": "198.32.119.0/24", "protocol": "IPv4"}`, // 3-letter country
		`{"id": 15, "ixlan_id": 50, "prefix": "198.32.120.0/24", "protocol": "IPv4"}`, // unknown exchange
		`{"id": 16, "ixlan_id": 77, "prefix": "198.32.121.0/24", "protocol": "IPv4"}`, // unknown ixlan
		`{"id": 17, "ixlan_id": 20, "prefix": "80.249.208.0/21", "protocol": "IPv4"}`, // repeated: first wins
	)
	rows, err := ComputeIX(dir)
	if err != nil {
		t.Fatal(err)
	}
	ams := "Amsterdam"
	want := []IXRow{
		{Prefix: "80.249.208.0/21", IXID: 1, Country: "NL", City: &ams},
		{Prefix: "2001:7f8:1::/64", IXID: 1, Country: "NL", City: &ams},
		{Prefix: "80.81.192.0/22", IXID: 2, Country: "DE", City: nil},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v\nwant   %+v", rows, want)
	}
}

func TestGlobalPrefix(t *testing.T) {
	for text, ok := range map[string]bool{
		"80.249.208.0/21":     true,
		"2001:7f8:1::/64":     true,
		"198.32.118.0/24":     true,
		"10.0.0.0/24":         false,
		"172.16.0.0/16":       false,
		"192.168.1.0/24":      false,
		"169.254.0.0/24":      false,
		"127.0.0.0/24":        false,
		"0.0.0.0/0":           false,
		"192.0.2.0/24":        false,
		"198.18.0.0/24":       false,
		"203.0.113.0/24":      false,
		"fc00::/64":           false,
		"::1/128":             false,
		"2002:c000:204::/48":  false,
		"::ffff:10.0.0.0/104": false,
		"1.2.3.4":             false,
	} {
		if _, got := globalPrefix(text); got != ok {
			t.Errorf("globalPrefix(%q) = %v, want %v", text, got, ok)
		}
	}
}

func TestComputeKeepsScopeAndType(t *testing.T) {
	dir := t.TempDir()
	writeDataset(t, dir, "org", `{"id": 1, "city": "Berlin", "country": "DE", "website": ""}`)
	writeDataset(t, dir, "net",
		`{"id": 1, "asn": 64500, "org_id": 1, "website": "", "info_scope": "Global", "info_type": "Content"}`,
		`{"id": 2, "asn": 64501, "org_id": 1, "website": "", "info_scope": "", "info_type": " NSP "}`,
	)
	for _, name := range []string{"fac", "ix", "netfac", "netixlan"} {
		writeDataset(t, dir, name)
	}
	rows, err := Compute(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || str(rows[0].InfoScope) != "Global" || str(rows[0].InfoType) != "Content" ||
		rows[1].InfoScope != nil || str(rows[1].InfoType) != "NSP" {
		t.Errorf("rows = %+v", rows)
	}
}
