// Package asn is the build-asn-data workflow: it parses ipverse/asn-ip, stores
// rows in Postgres, fills domains, and builds the ip2asn MMDB and asn.sqlite3.
package asn

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Record is one row of the asn table.
type Record struct {
	ASN              int64
	Handle           *string
	Organization     *string
	OrganizationSlug *string
	Country          *string
	IPAmount         string
	IPv4Amount       string
	IPv6Amount       string
	JSON             string
}

// ParseResult is the outcome of parsing an asn-ip "as" directory.
type ParseResult struct {
	Records []Record
	Skipped int
}

// sourceFile is the layout of as/<ASN>/aggregated.json.
type sourceFile struct {
	ASN      int64          `json:"asn"`
	Metadata map[string]any `json:"metadata"`
	Prefixes struct {
		IPv4 []string `json:"ipv4"`
		IPv6 []string `json:"ipv6"`
	} `json:"prefixes"`
}

// rowJSON is what the json column stores.
type rowJSON struct {
	ASN      int64          `json:"asn"`
	Metadata map[string]any `json:"metadata"`
	Prefixes rowPrefixes    `json:"prefixes"`
	Country  string         `json:"country,omitempty"`
}

type rowPrefixes struct {
	IPv4 []string `json:"ipv4"`
	IPv6 []string `json:"ipv6"`
}

// ParseDir parses every as/<ASN>/aggregated.json under asDir, sorted by ASN.
// Files that cannot be used are skipped and reported through warn.
func ParseDir(asDir string, warn func(string)) (ParseResult, error) {
	result := ParseResult{}
	entries, err := os.ReadDir(asDir)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		asn, err := strconv.ParseInt(entry.Name(), 10, 64)
		if !entry.IsDir() || err != nil || asn <= 0 {
			continue
		}
		path := filepath.Join(asDir, entry.Name(), "aggregated.json")
		record, err := parseFile(path, asn)
		if err != nil {
			warn(fmt.Sprintf("warning: %s: %v; skipping", path, err))
			result.Skipped++
			continue
		}
		result.Records = append(result.Records, record)
	}
	sort.Slice(result.Records, func(i, j int) bool {
		return result.Records[i].ASN < result.Records[j].ASN
	})
	return result, nil
}

func parseFile(path string, dirASN int64) (Record, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var src sourceFile
	if err := json.Unmarshal(raw, &src); err != nil {
		return Record{}, err
	}
	if src.ASN != dirASN {
		return Record{}, fmt.Errorf("ASN mismatch (dir %d, json %d)", dirASN, src.ASN)
	}

	metadata := map[string]any{}
	for key, value := range src.Metadata {
		if s, ok := value.(string); ok {
			value = strings.TrimSpace(s)
		}
		if value != nil {
			metadata[key] = value
		}
	}
	ipv4, err := cleanPrefixes(src.Prefixes.IPv4)
	if err != nil {
		return Record{}, err
	}
	ipv6, err := cleanPrefixes(src.Prefixes.IPv6)
	if err != nil {
		return Record{}, err
	}
	ipv4Amount, err := countAddresses(ipv4, true)
	if err != nil {
		return Record{}, err
	}
	ipv6Amount, err := countAddresses(ipv6, false)
	if err != nil {
		return Record{}, err
	}

	country := metadataString(metadata, "countryCode")
	row := rowJSON{ASN: src.ASN, Metadata: metadata, Prefixes: rowPrefixes{IPv4: ipv4, IPv6: ipv6}}
	if country != nil {
		row.Country = *country
	}
	jsonText, err := encodeJSON(row)
	if err != nil {
		return Record{}, err
	}
	organization := metadataString(metadata, "description")
	return Record{
		ASN:              src.ASN,
		Handle:           metadataString(metadata, "handle"),
		Organization:     organization,
		OrganizationSlug: slugifyOrganization(organization),
		Country:          country,
		IPAmount:         new(big.Int).Add(ipv4Amount, ipv6Amount).String(),
		IPv4Amount:       ipv4Amount.String(),
		IPv6Amount:       formatIPv6Millions(ipv6Amount),
		JSON:             jsonText,
	}, nil
}

func cleanPrefixes(prefixes []string) ([]string, error) {
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("empty prefix entry")
		}
		out = append(out, p)
	}
	return out, nil
}

// countAddresses sums the addresses covered by prefixes of one family.
func countAddresses(prefixes []string, wantV4 bool) (*big.Int, error) {
	total := new(big.Int)
	for _, text := range prefixes {
		prefix, err := netip.ParsePrefix(text)
		if err != nil {
			return nil, fmt.Errorf("invalid prefix %q: %v", text, err)
		}
		if prefix.Addr().Is4() != wantV4 {
			return nil, fmt.Errorf("prefix %q in the wrong address family list", text)
		}
		hostBits := prefix.Addr().BitLen() - prefix.Bits()
		total.Add(total, new(big.Int).Lsh(big.NewInt(1), uint(hostBits)))
	}
	return total, nil
}

func metadataString(metadata map[string]any, key string) *string {
	s, ok := metadata[key].(string)
	if !ok || s == "" {
		return nil
	}
	return &s
}

// encodeJSON is json.Marshal without HTML escaping, so "AT&T" stays readable.
func encodeJSON(value any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// formatIPv6Millions formats an address count in millions with 2 decimals, rounding half up.
func formatIPv6Millions(value *big.Int) string {
	million := big.NewInt(1_000_000)
	whole, remainder := new(big.Int).QuoRem(value, million, new(big.Int))
	rounded := new(big.Int).Mul(remainder, big.NewInt(100))
	rounded.Add(rounded, big.NewInt(500_000))
	rounded.Quo(rounded, million)
	if rounded.Cmp(big.NewInt(100)) >= 0 {
		whole.Add(whole, big.NewInt(1))
		rounded.SetInt64(0)
	}
	return fmt.Sprintf("%s.%02d", whole.String(), rounded.Int64())
}

var (
	slugInvalid    = regexp.MustCompile(`[^a-z0-9 ]+`)
	slugWhitespace = regexp.MustCompile(`\s+`)
)

func slugifyOrganization(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := slugInvalid.ReplaceAllString(strings.ToLower(*value), "")
	cleaned = strings.TrimSpace(slugWhitespace.ReplaceAllString(cleaned, " "))
	if cleaned == "" {
		return nil
	}
	slug := strings.ReplaceAll(cleaned, " ", "-")
	return &slug
}
