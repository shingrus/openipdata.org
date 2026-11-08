package main

// Lookup report for a built MMDB (mmdbbuilder lookup).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

var builtinTestIPs = []string{
	"1.1.1.1",
	"8.8.8.8",
	"8.8.4.4",
	"9.9.9.9",
	"208.67.222.222",
	"2606:4700:4700::1111",
	"2001:4860:4860::8888",
	"2620:fe::fe",
}

// printLookups looks up testIP, or the IPs in ipsPath plus the builtin IPs,
// and prints each record and a summary.
func printLookups(mmdbPath, testIP, ipsPath string) error {
	reader, err := maxminddb.Open(mmdbPath)
	if err != nil {
		return err
	}
	defer reader.Close()

	fileIPs := []string{}
	if testIP == "" && ipsPath != "" {
		fileIPs, err = readIPFile(ipsPath)
		if err != nil {
			return err
		}
	}
	ips := []string{testIP}
	if testIP == "" {
		ips = mergeUniqueIPs(fileIPs, builtinTestIPs)
	}

	found, notFound, invalid, lookupErrors := 0, 0, 0, 0
	countryCounts := map[string]int{}
	for _, ipStr := range ips {
		addr, err := netip.ParseAddr(ipStr)
		if err != nil {
			invalid++
			fmt.Printf("%s: invalid IP\n", ipStr)
			continue
		}
		result := reader.Lookup(addr)
		if err := result.Err(); err != nil {
			lookupErrors++
			fmt.Printf("%s: lookup error: %v\n", ipStr, err)
			continue
		}
		if !result.Found() {
			notFound++
			fmt.Printf("%s: not found\n", ipStr)
			continue
		}
		var record map[string]any
		if err := result.Decode(&record); err != nil {
			lookupErrors++
			fmt.Printf("%s: decode error: %v\n", ipStr, err)
			continue
		}
		if code := extractCountryISO(record); code != "" {
			countryCounts[code]++
		}
		found++
		encoded, err := json.Marshal(record)
		if err != nil {
			fmt.Printf("%s: %v\n", ipStr, record)
			continue
		}
		fmt.Printf("%s: %s\n", ipStr, encoded)
	}

	total := len(ips)
	lookups := total - invalid
	meta := reader.Metadata
	fmt.Println("summary:")
	fmt.Printf("  mmdb: %s (type %s, built %s, %d nodes)\n", mmdbPath, meta.DatabaseType,
		meta.BuildTime().UTC().Format("2006-01-02 15:04:05 UTC"), meta.NodeCount)
	if testIP != "" {
		fmt.Printf("  total IPs: %d (from flag)\n", total)
	} else {
		fmt.Printf("  total IPs: %d (from file: %d, builtin: %d)\n", total, len(fileIPs), len(builtinTestIPs))
	}
	fmt.Printf("  invalid IPs: %d (%.1f%% of total)\n", invalid, percent(invalid, total))
	fmt.Printf("  lookups: %d\n", lookups)
	fmt.Printf("  found: %d (%.1f%% of lookups)\n", found, percent(found, lookups))
	fmt.Printf("  not found: %d (%.1f%% of lookups)\n", notFound, percent(notFound, lookups))
	fmt.Printf("  errors: %d (%.1f%% of lookups)\n", lookupErrors, percent(lookupErrors, lookups))

	if len(countryCounts) > 0 {
		codes := make([]string, 0, len(countryCounts))
		for code := range countryCounts {
			codes = append(codes, code)
		}
		sort.Slice(codes, func(i, j int) bool {
			if countryCounts[codes[i]] != countryCounts[codes[j]] {
				return countryCounts[codes[i]] > countryCounts[codes[j]]
			}
			return codes[i] < codes[j]
		})
		fmt.Printf("  country stats: %d\n", len(codes))
		for _, code := range codes {
			fmt.Printf("    %s: %d\n", code, countryCounts[code])
		}
	}
	return nil
}

// readIPFile reads one IP per line; blank lines and # comments are ignored.
func readIPFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	ips := []string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ips = append(ips, line)
	}
	return ips, scanner.Err()
}

func mergeUniqueIPs(primary []string, secondary []string) []string {
	seen := make(map[string]struct{}, len(primary)+len(secondary))
	merged := []string{}
	for _, values := range [][]string{primary, secondary} {
		for _, value := range values {
			if _, ok := seen[value]; ok || value == "" {
				continue
			}
			seen[value] = struct{}{}
			merged = append(merged, value)
		}
	}
	return merged
}

func percent(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return (float64(part) * 100) / float64(total)
}

// extractCountryISO finds a two-letter country code in an ASN or geo record.
func extractCountryISO(record map[string]any) string {
	for _, key := range []string{"country_code", "countryCode", "iso_code", "country", "registered_country"} {
		value := record[key]
		if nested, ok := value.(map[string]any); ok {
			value = nested["iso_code"]
		}
		if code, ok := value.(string); ok {
			code = strings.TrimSpace(code)
			if len(code) == 2 {
				return strings.ToUpper(code)
			}
		}
	}
	return ""
}
