package main

// ip2geo: layered CSV files (RIR stats, registration data, PeeringDB rows,
// geofeed rows) -> country and city per prefix.

import (
	"bufio"
	"compress/gzip"
	_ "embed"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/maxmind/mmdbwriter/inserter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// countriesTab is tzdata's iso3166.tab: the country name for codes that the
// -names file does not cover.
//
//go:embed countries.tab
var countriesTab string

type geoLayerStats struct {
	name     string
	rows     int // usable rows
	prefixes int // networks written (repeated prefixes count once)
	skipped  int // unusable rows (see warnings)
	noCity   int // rows whose city was left out (too long or not plain text)
}

type geoEntry struct {
	prefix netip.Prefix
	place  int // index into the record list
}

// place is the value written for a prefix.
type place struct{ code, city string }

// buildGeo builds the ip2geo MMDB from the layer files, in order, and writes
// it to outPath atomically.
//
// A layer replaces whatever the layers before it put under its prefixes.
// Within a layer every prefix is inserted broadest first, so a more specific
// prefix wins; for a repeated prefix the last row wins.
//
// A layer named in sameCountry (by file name) only refines: its rows apply
// where the layers before it gave the same country or none, so a broad row
// does not erase more specific networks that are somewhere else.
//
// A layer named in withinFirst (by file name) is used only for rows no
// broader than one prefix of the first layer, so a broad row cannot change
// the country of networks that belong to someone else. A first-layer row with
// an empty country writes nothing, but still counts as a prefix for this.
func buildGeo(layers []string, namesPath, outPath string, debugMode bool, sameCountry, withinFirst map[string]bool) ([]geoLayerStats, error) {
	names, err := loadNames(namesPath)
	if err != nil {
		return nil, err
	}
	writer, err := newWriter("ip2geo", "IP2GEO")
	if err != nil {
		return nil, err
	}
	places := map[place]int{}
	var records []mmdbtype.Map
	var stats []geoLayerStats
	first := newBlockSet()
	for n, path := range layers {
		s := geoLayerStats{name: filepath.Base(path)}
		limited := n > 0 && withinFirst[s.name] && s.name != stats[0].name
		var entries []geoEntry
		err := readLayer(path, func(line int, prefix, code, city string) {
			p, err := parseGeoPrefix(prefix)
			if err != nil {
				warnf(&s, "%s:%d: invalid prefix %q", s.name, line, prefix)
				return
			}
			code = strings.ToUpper(strings.TrimSpace(code))
			if n == 0 {
				first.add(p)
				if code == "" {
					return // delegated space with no country: it only marks a block
				}
			}
			if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
				warnf(&s, "%s:%d: invalid country code %q", s.name, line, code)
				return
			}
			if limited && !first.covers(p) {
				warnf(&s, "%s:%d: %s is broader than any block of %s", s.name, line, prefix, stats[0].name)
				return
			}
			key := place{code, cleanCity(city)}
			if key.city == "" && strings.TrimSpace(city) != "" {
				s.noCity++
			}
			idx, ok := places[key]
			if !ok {
				record := mmdbtype.Map{}
				setString(record, "country_code", key.code)
				setString(record, "country_name", names[key.code])
				setString(record, "city_name", key.city)
				idx = len(records)
				places[key] = idx
				records = append(records, record)
			}
			entries = append(entries, geoEntry{p, idx})
			s.rows++
		})
		if err != nil {
			return stats, fmt.Errorf("%s: %w", s.name, err)
		}
		if s.rows == 0 {
			return stats, fmt.Errorf("%s: no usable rows", s.name)
		}

		// Stable, so repeated prefixes stay in file order next to each other.
		sort.SliceStable(entries, func(i, j int) bool {
			a, b := entries[i].prefix, entries[j].prefix
			if a.Bits() != b.Bits() {
				return a.Bits() < b.Bits()
			}
			return a.Addr().Less(b.Addr())
		})
		for i, e := range entries {
			if i+1 < len(entries) && entries[i+1].prefix == e.prefix {
				continue // the last row of a repeated prefix wins
			}
			record := records[e.place]
			if debugMode {
				record = mmdbtype.Map{"network": mmdbtype.String(e.prefix.String())}
				for k, v := range records[e.place] {
					record[k] = v
				}
			}
			insert := inserter.ReplaceWith(record)
			if sameCountry[s.name] {
				insert = whereCountry(record, records[e.place]["country_code"])
			}
			if err := writer.InsertFunc(ipNet(e.prefix), insert); err != nil {
				return stats, fmt.Errorf("insert %s (%s): %w", e.prefix, s.name, err)
			}
			s.prefixes++
		}
		stats = append(stats, s)
	}
	return stats, writeAtomic(writer, outPath)
}

// blockSet holds the prefixes of one layer, to test whether a prefix lies
// inside one of them.
type blockSet struct {
	prefixes map[netip.Prefix]bool
	lens     [2]map[int]bool // prefix lengths seen: IPv4, IPv6
}

func newBlockSet() *blockSet {
	return &blockSet{prefixes: map[netip.Prefix]bool{}, lens: [2]map[int]bool{{}, {}}}
}

func famIndex(a netip.Addr) int {
	if a.Is4() {
		return 0
	}
	return 1
}

func (b *blockSet) add(p netip.Prefix) {
	b.prefixes[p] = true
	b.lens[famIndex(p.Addr())][p.Bits()] = true
}

// covers reports whether p is one of the prefixes or lies inside one.
func (b *blockSet) covers(p netip.Prefix) bool {
	for bits := range b.lens[famIndex(p.Addr())] {
		if bits <= p.Bits() && b.prefixes[netip.PrefixFrom(p.Addr(), bits).Masked()] {
			return true
		}
	}
	return false
}

// whereCountry writes record over the networks whose country is code or
// unknown, and leaves the others as they are.
func whereCountry(record mmdbtype.Map, code mmdbtype.DataType) inserter.Func {
	return func(existing mmdbtype.DataType) (mmdbtype.DataType, error) {
		if m, ok := existing.(mmdbtype.Map); ok {
			if have, ok := m["country_code"]; ok && !have.Equal(code) {
				return existing, nil
			}
		}
		return record, nil
	}
}

const maxCityLen = 100 // bytes

// cleanCity returns the city if it is plain text: short, valid UTF-8, with no
// control characters or markup. Anything else is left out; the names come
// from files that third parties write.
func cleanCity(city string) string {
	city = strings.TrimSpace(city)
	if len(city) > maxCityLen || !utf8.ValidString(city) {
		return ""
	}
	for _, r := range city {
		if unicode.IsControl(r) || r == '<' || r == '>' {
			return ""
		}
	}
	return city
}

// warnf counts a skipped row and prints the first few warnings of a layer.
func warnf(s *geoLayerStats, format string, args ...any) {
	s.skipped++
	if s.skipped <= 10 {
		fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...)
	}
}

// globalV6 is IPv6 global unicast space. An IPv6 prefix outside it is refused:
// the tree keeps the IPv4 addresses at ::/96, so such a row could write over
// IPv4 space.
var globalV6 = netip.MustParsePrefix("2000::/3")

// parseGeoPrefix reads a CIDR prefix or a single address. Host bits are
// cleared ("8.8.8.1/24" is 8.8.8.0/24).
func parseGeoPrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	var p netip.Prefix
	if !strings.Contains(s, "/") {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		addr = addr.Unmap()
		p = netip.PrefixFrom(addr, addr.BitLen())
	} else {
		var err error
		if p, err = netip.ParsePrefix(s); err != nil {
			return netip.Prefix{}, err
		}
		if p.Addr().Is4In6() {
			return netip.Prefix{}, errors.New("IPv4-mapped prefix")
		}
		p = p.Masked()
	}
	if p.Addr().Is6() && (p.Bits() < globalV6.Bits() || !globalV6.Contains(p.Addr())) {
		return netip.Prefix{}, errors.New("IPv6 prefix outside 2000::/3")
	}
	return p, nil
}

// readLayer calls fn for each data row of a CSV file (gzipped if it ends in
// .gz). The header row names the columns: prefix, country, and optionally
// city; other columns are ignored.
func readLayer(path string, fn func(line int, prefix, code, city string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var in io.Reader = bufio.NewReader(f)
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(in)
		if err != nil {
			return err
		}
		defer gz.Close()
		in = gz
	}
	r := csv.NewReader(in)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[strings.TrimSpace(name)] = i
	}
	prefixCol, ok1 := col["prefix"]
	countryCol, ok2 := col["country"]
	cityCol, hasCity := col["city"]
	if !ok1 || !ok2 {
		return fmt.Errorf("header %q lacks prefix or country", strings.Join(header, ","))
	}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// No field position here: a row that fails to parse may have none.
			var parseErr *csv.ParseError
			if errors.As(err, &parseErr) {
				fmt.Fprintf(os.Stderr, "warning: %s: %v\n", filepath.Base(path), err)
				continue
			}
			return err
		}
		line, _ := r.FieldPos(0)
		if len(rec) <= prefixCol || len(rec) <= countryCol {
			fn(line, "", "", "")
			continue
		}
		city := ""
		if hasCity && len(rec) > cityCol {
			city = rec[cityCol]
		}
		fn(line, rec[prefixCol], rec[countryCol], city)
	}
}

// loadNames returns country names by code: the names file (code<TAB>name)
// first, then the embedded tzdata list for codes it lacks.
func loadNames(path string) (map[string]string, error) {
	names := map[string]string{}
	addTab := func(text string) {
		for _, line := range strings.Split(text, "\n") {
			code, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
			code, name = strings.ToUpper(strings.TrimSpace(code)), strings.TrimSpace(name)
			if !ok || strings.HasPrefix(code, "#") || len(code) != 2 || name == "" {
				continue
			}
			if _, seen := names[code]; !seen {
				names[code] = name
			}
		}
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		addTab(string(data))
	}
	addTab(countriesTab)
	return names, nil
}
