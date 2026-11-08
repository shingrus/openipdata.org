package main

// ip2asn: as/<ASN>/aggregated.json files from ipverse/asn-ip -> asn, name, org,
// and country per prefix.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// asnFile is the layout of as/<ASN>/aggregated.json.
type asnFile struct {
	ASN      int64 `json:"asn"`
	Metadata struct {
		Handle      string `json:"handle"`
		Description string `json:"description"`
		CountryCode string `json:"countryCode"`
	} `json:"metadata"`
	Prefixes struct {
		IPv4 []string `json:"ipv4"`
		IPv6 []string `json:"ipv6"`
	} `json:"prefixes"`
}

// announcement is one prefix of one ASN.
type announcement struct {
	prefix netip.Prefix
	asn    int64
}

type asnStats struct {
	entries    int // ASNs written
	prefixes   int // networks written
	skipped    int // ASNs skipped (see warnings)
	duplicates int // prefixes also announced by a lower ASN, which wins
}

// buildASN builds the ip2asn MMDB from asDir and writes it to outPath atomically.
//
// Prefixes of different ASNs can overlap. Every prefix is inserted broadest
// first, so a more specific prefix always wins, as in routing; when several
// ASNs announce the identical prefix, the lowest ASN wins.
func buildASN(asDir, outPath string, debugMode bool) (asnStats, error) {
	stats := asnStats{}
	records, announcements, err := readASNDir(asDir, &stats)
	if err != nil {
		return stats, err
	}
	if len(records) == 0 {
		return stats, errors.New("no usable aggregated.json entries found")
	}

	sort.Slice(announcements, func(i, j int) bool {
		a, b := announcements[i], announcements[j]
		if a.prefix.Bits() != b.prefix.Bits() {
			return a.prefix.Bits() < b.prefix.Bits()
		}
		if c := a.prefix.Addr().Compare(b.prefix.Addr()); c != 0 {
			return c < 0
		}
		return a.asn < b.asn
	})

	writer, err := newWriter("ip2asn", "IP2ASN")
	if err != nil {
		return stats, err
	}
	written := map[int64]bool{}
	for i, a := range announcements {
		if i > 0 && announcements[i-1].prefix == a.prefix {
			stats.duplicates++
			continue
		}
		record := records[a.asn]
		if debugMode {
			record = mmdbtype.Map{"network": mmdbtype.String(a.prefix.String())}
			for k, v := range records[a.asn] {
				record[k] = v
			}
		}
		if err := writer.Insert(ipNet(a.prefix), record); err != nil {
			return stats, fmt.Errorf("insert %s (AS%d): %w", a.prefix, a.asn, err)
		}
		written[a.asn] = true
		stats.prefixes++
	}
	stats.entries = len(written)
	return stats, writeAtomic(writer, outPath)
}

// readASNDir returns one MMDB record per usable ASN and all their prefixes.
// Unusable files are skipped with a warning.
func readASNDir(asDir string, stats *asnStats) (map[int64]mmdbtype.Map, []announcement, error) {
	entries, err := os.ReadDir(asDir)
	if err != nil {
		return nil, nil, err
	}
	records := map[int64]mmdbtype.Map{}
	var announcements []announcement
	for _, entry := range entries {
		asn, err := strconv.ParseInt(entry.Name(), 10, 64)
		if !entry.IsDir() || err != nil || asn <= 0 {
			continue
		}
		path := filepath.Join(asDir, entry.Name(), "aggregated.json")
		prefixes, record, err := readASNFile(path, asn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v; skipping\n", path, err)
			stats.skipped++
			continue
		}
		records[asn] = record
		for _, p := range prefixes {
			announcements = append(announcements, announcement{prefix: p, asn: asn})
		}
	}
	return records, announcements, nil
}

func readASNFile(path string, dirASN int64) ([]netip.Prefix, mmdbtype.Map, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var f asnFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, nil, err
	}
	if f.ASN != dirASN {
		return nil, nil, fmt.Errorf("ASN mismatch (dir %d, json %d)", dirASN, f.ASN)
	}
	var prefixes []netip.Prefix
	for _, family := range []struct {
		values []string
		is4    bool
	}{{f.Prefixes.IPv4, true}, {f.Prefixes.IPv6, false}} {
		for _, text := range family.values {
			p, err := netip.ParsePrefix(strings.TrimSpace(text))
			if err != nil {
				return nil, nil, fmt.Errorf("invalid prefix %q: %v", text, err)
			}
			if p.Addr().Is4() != family.is4 {
				return nil, nil, fmt.Errorf("prefix %q in the wrong address family list", text)
			}
			// Host bits set ("8.8.8.1/24") keep the prefix length: 8.8.8.0/24.
			prefixes = append(prefixes, p.Masked())
		}
	}
	if len(prefixes) == 0 {
		return nil, nil, errors.New("no prefixes")
	}

	handle := strings.TrimSpace(f.Metadata.Handle)
	org := strings.TrimSpace(f.Metadata.Description)
	name := handle
	if name == "" {
		name = org
	}
	record := mmdbtype.Map{}
	setString(record, "asn", strconv.FormatInt(f.ASN, 10))
	setString(record, "name", name)
	setString(record, "org", org)
	setString(record, "country_code", strings.TrimSpace(f.Metadata.CountryCode))
	return prefixes, record, nil
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

// writeAtomic writes the tree to a temporary file next to outPath and renames
// it into place, so outPath is never left truncated or half written.
func writeAtomic(writer *mmdbwriter.Tree, outPath string) error {
	tmp, err := os.CreateTemp(filepath.Dir(outPath), filepath.Base(outPath)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := writer.WriteTo(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), outPath)
}
