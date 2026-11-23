package main

// ip2geo: layer files (gzipped CSV in the geofeed layout) -> country and city
// per prefix.

import (
	"bufio"
	"compress/gzip"
	_ "embed"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/maxmind/mmdbwriter"
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
	rows     int    // usable rows
	prefixes int    // networks written (repeated prefixes count once)
	skipped  int    // unusable rows (see warnings)
	noCity   int    // rows whose city was left out (too long or not plain text)
	placed   int    // new records that got coordinates (only with GeoNames)
	unplaced int    // new records with a city that GeoNames could not place
	renamed  int    // rows whose city the name cleanup replaced (cityclean.go)
	note     string // one more line to print after the layer line (the ASN main city fill)
}

type geoEntry struct {
	prefix netip.Prefix
	place  int            // index into the record list
	parts  []netip.Prefix // for a firstCountry layer: where the row applies; nil means all of prefix
}

// place is the value written for a prefix. source is the layer that wrote the
// city (empty when there is no city), so records are shared per (country,
// city, source).
type place struct{ code, city, source string }

// buildGeo builds the ip2geo MMDB from the layer files, in order, and writes
// it to outPath atomically.
//
// A layer replaces whatever the layers before it put under its prefixes (one exception: an IPv4
// row with a country and no city, see below).
// Within a layer every prefix is inserted broadest first, so a more specific
// prefix wins; for a repeated prefix the last row wins.
//
// An IPv4 row with a country and no city (in any layer) never erases a city of the same country
// that another layer wrote: such a network is left as it is (keepCity). Where the country differs,
// or the city came from a broader row of the same layer, the row replaces the record as usual.
// IPv6 rows are not changed by this: there the rule was not measured, and no guess fills in.
//
// A layer named in sameCountry (by file name) only refines: its rows apply
// where the layers before it gave the same country or none, so a broad row
// does not erase more specific networks that are somewhere else.
//
// A layer named in withinFirst (by file name) is used only for rows no
// broader than one prefix of the first layer, so a broad row cannot change
// the country of networks that belong to someone else. A first-layer row with
// an empty country writes nothing, but still counts as a prefix for this.
//
// A layer named in firstCountry (by file name) applies a row only to the
// addresses where the country of the first layer equals the row's country, so
// it can only change a country back to the one the first layer gives. This is
// not the same as sameCountry, which compares with whatever the layers before
// it left. A row broader than the first-layer blocks is split: it is written
// over the parts where the first layer has its country, and the rest is left
// alone. A row with no such part is skipped and counted in the skipped total.
// Addresses the first layer has no country for never match.
//
// A record has country_code, country_name, city_name and, when it has a city,
// city_source: the layer file name without .csv.gz (or .csv) of the layer that
// wrote the city. A row replaces the whole record or leaves it as it is, so
// city_source always names the layer behind the city; a record without a city has none. With
// debug the record also has network.
//
// With a GeoNames index (nil: none), a record whose city the index places
// (the placing rule of the evaluation, geoNames.place) also has latitude and
// longitude, doubles rounded to 4 decimals. A city that is not placed, and a
// record without a city, have neither. A record is made once per (country,
// city, source), so every prefix of a place shares the values; the counts in
// the layer stats are per new record, in the layer that first made it.
//
// With asnMain (the path of an asn-prefixes file, "" for none), after all
// layers the cities of the ASNs that sit in one city are given to their
// addresses that have a country and no city; see asnmain.go. It is a guess,
// not a layer file, and it only fills.
func buildGeo(layers []string, namesPath, outPath string, debugMode bool, sameCountry, withinFirst, firstCountry map[string]bool, index geoNames, asnMain string) ([]geoLayerStats, error) {
	return buildGeoGuesses(layers, namesPath, outPath, debugMode, sameCountry, withinFirst, firstCountry, index, asnMain, false, "", "")
}

// buildGeoGuesses is buildGeo with the fallback switch: with fallback, more guesses
// follow the ASN main city fill (82-asn-likely, 84-asn-near, 86-country-main, 88-asn-far, 89-asn-anywhere, 89-country-near; see
// fallback.go), and with fallback and no asnMain only the country one runs. With holders
// (the path of a holders file, "" for none; it needs asnMain) the holder city guess runs
// as well (85-holder-city, holders.go). With rttCountry (the path of the RTT file, "" for none; it
// needs the GeoNames index) the speed-of-light country check runs after all layers and before
// the guesses (rttcountry.go). Without them the output is exactly buildGeo's.
func buildGeoGuesses(layers []string, namesPath, outPath string, debugMode bool, sameCountry, withinFirst, firstCountry map[string]bool, index geoNames, asnMain string, fallback bool, holders string, rttCountry string) ([]geoLayerStats, error) {
	return buildGeoNames(layers, namesPath, outPath, debugMode, sameCountry, withinFirst, firstCountry, index, nil, asnMain, fallback, holders, rttCountry)
}

// buildGeoNames is buildGeoGuesses with the folded GeoNames index (nil: none; it needs index):
// a layer city the index does not place exactly is cleaned first (cityclean.go), and the number
// of rows renamed is in the stats (geoLayerStats.renamed).
func buildGeoNames(layers []string, namesPath, outPath string, debugMode bool, sameCountry, withinFirst, firstCountry map[string]bool, index geoNames, folded foldedNames, asnMain string, fallback bool, holders string, rttCountry string) ([]geoLayerStats, error) {
	var cleaner *cityCleaner
	if index != nil && folded != nil {
		cleaner = &cityCleaner{index: index, folded: folded}
	}
	if rttCountry != "" && index == nil {
		return nil, errors.New("-rtt-country needs -geonames")
	}
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
	// recordFor returns the index of the record of a place, making it on first use
	// (counted in the stats of the layer that asks).
	recordFor := func(key place, s *geoLayerStats) int {
		if idx, ok := places[key]; ok {
			return idx
		}
		record := mmdbtype.Map{}
		setString(record, "country_code", key.code)
		setString(record, "country_name", names[key.code])
		setString(record, "city_name", key.city)
		setString(record, "city_source", key.source)
		if index != nil && key.city != "" {
			if p, ok := index.place(key.code, key.city); ok {
				record["latitude"] = mmdbtype.Float64(round4(p.lat))
				record["longitude"] = mmdbtype.Float64(round4(p.lon))
				s.placed++
			} else {
				s.unplaced++
			}
		}
		idx := len(records)
		places[key] = idx
		records = append(records, record)
		return idx
	}
	first := newBlockSet()
	var firstTree *mmdbwriter.Tree // country of the first layer, only when a layer needs it
	if len(firstCountry) > 0 {
		if firstTree, err = newWriter("first", "FIRST"); err != nil {
			return nil, err
		}
	}
	for n, path := range layers {
		s := geoLayerStats{name: filepath.Base(path)}
		limited := n > 0 && withinFirst[s.name] && s.name != stats[0].name
		matching := n > 0 && firstCountry[s.name] && s.name != stats[0].name
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
			if !validCountries[code] {
				warnf(&s, "%s:%d: invalid country code %q", s.name, line, code)
				return
			}
			if limited && !first.covers(p) {
				warnf(&s, "%s:%d: %s is broader than any block of %s", s.name, line, prefix, stats[0].name)
				return
			}
			var parts []netip.Prefix
			if matching {
				if parts = countryParts(firstTree, p, code); len(parts) == 0 {
					warnf(&s, "%s:%d: %s has no address where %s gives %s", s.name, line, prefix, stats[0].name, code)
					return
				}
			}
			if cleaner != nil {
				var renamed bool
				if city, renamed = cleaner.clean(code, city); renamed {
					s.renamed++
				}
			}
			key := place{code: code, city: cleanCity(city)}
			if key.city != "" {
				key.source = layerSource(s.name)
			}
			if key.city == "" && strings.TrimSpace(city) != "" {
				s.noCity++
			}
			idx := recordFor(key, &s)
			entries = append(entries, geoEntry{p, idx, parts})
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
			if _, hasCity := records[e.place]["city_name"]; !hasCity && e.prefix.Addr().Is4() {
				insert = keepCity(insert, records[e.place]["country_code"], layerSource(s.name))
			}
			parts := e.parts
			if parts == nil {
				parts = []netip.Prefix{e.prefix}
			}
			for _, part := range parts {
				if err := writer.InsertFunc(ipNet(part), insert); err != nil {
					return stats, fmt.Errorf("insert %s (%s): %w", part, s.name, err)
				}
				s.prefixes++
			}
			if n == 0 && firstTree != nil {
				if err := firstTree.Insert(ipNet(e.prefix), records[e.place]["country_code"]); err != nil {
					return stats, fmt.Errorf("insert %s (%s): %w", e.prefix, s.name, err)
				}
			}
		}
		stats = append(stats, s)
	}
	if rttCountry != "" {
		// After all layers, before the guesses, so that they fill cities in the corrected country.
		// One line, never a "layer" line: it is not a layer and has no stats entry.
		counts, err := applyRTTCountry(writer, rttCountry, index, debugMode, recordFor, func(i int) mmdbtype.Map { return records[i] })
		if err != nil {
			return stats, fmt.Errorf("%s: %w", filepath.Base(rttCountry), err)
		}
		fmt.Println(counts)
	}
	if asnMain != "" || fallback || holders != "" {
		guesses, err := fillGuesses(writer, asnMain, fallback, index, holders, debugMode, recordFor, func(i int) mmdbtype.Map { return records[i] })
		stats = append(stats, guesses...)
		if err != nil {
			return stats, err
		}
	}
	return stats, writeAtomic(writer, outPath)
}

// round4 rounds a coordinate to 4 decimals (about 10 m).
func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }

// layerSource is a layer file's name without the .csv.gz or .csv suffix.
func layerSource(name string) string {
	name = strings.TrimSuffix(name, ".gz")
	return strings.TrimSuffix(name, ".csv")
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

// keepCity is the rule for an IPv4 row with a country and no city: it leaves a network as it is
// when the network has that country and a city that another layer wrote, and otherwise does what
// insert does. So a row that only names the country cannot erase a city of the same country
// (measured: such rows of the geofeed layer erased the city of 419 RIPE Atlas probes, and the
// guesses that filled in were right less often). A city that a broader row of the same layer
// wrote is replaced: within one layer the more specific row is the layer's own statement. The
// builder does not see a row's region, so a row with a region and no city is treated the same.
func keepCity(insert inserter.Func, code mmdbtype.DataType, source string) inserter.Func {
	return func(existing mmdbtype.DataType) (mmdbtype.DataType, error) {
		if m, ok := existing.(mmdbtype.Map); ok {
			have, hasCountry := m["country_code"]
			city, _ := m["city_name"].(mmdbtype.String)
			src, _ := m["city_source"].(mmdbtype.String)
			if hasCountry && have.Equal(code) && city != "" && string(src) != source {
				return existing, nil
			}
		}
		return insert(existing)
	}
}

// countryParts returns the parts of p where first (the first layer, as a tree
// of country codes) has the country code. They are the networks of the tree
// inside p, or p itself when one tree network holds all of p.
func countryParts(first *mmdbwriter.Tree, p netip.Prefix, code string) []netip.Prefix {
	var parts []netip.Prefix
	end := lastAddr(p)
	for addr := p.Addr(); ; {
		network, value := first.Get(addr.AsSlice())
		q := treePrefix(network, addr)
		if q.Bits() <= p.Bits() {
			q = p // the network holds all of p
		}
		if have, ok := value.(mmdbtype.String); ok && string(have) == code {
			parts = append(parts, q)
		}
		addr = lastAddr(q).Next()
		if !addr.IsValid() || addr.Compare(end) > 0 {
			return parts
		}
	}
}

// treePrefix converts a network that Tree.Get returns. For an IPv4 address it
// is the IPv4-mapped form, with the prefix length counted in 128 bits.
func treePrefix(n *net.IPNet, addr netip.Addr) netip.Prefix {
	ones, _ := n.Mask.Size()
	if addr.Is4() {
		if ones < 96 {
			return netip.PrefixFrom(netip.IPv4Unspecified(), 0)
		}
		ones -= 96
	}
	a, _ := netip.AddrFromSlice(n.IP)
	return netip.PrefixFrom(a.Unmap(), ones).Masked()
}

// lastAddr is the last address of p.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
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

// readLayer calls fn for each data row of a layer file: a gzipped CSV in the
// geofeed (RFC 8805) layout with a header line, prefix,country,region,city,source.
// The columns are found by name; region and source are not used.
func readLayer(path string, fn func(line int, prefix, code, city string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return err
	}
	defer gz.Close()
	r := csv.NewReader(gz)
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
	cityCol, ok3 := col["city"]
	if !ok1 || !ok2 || !ok3 {
		return fmt.Errorf("header %q lacks prefix, country, or city", strings.Join(header, ","))
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
		if len(rec) <= max(prefixCol, countryCol) {
			fn(line, "", "", "")
			continue
		}
		// RFC 8805 allows leaving out trailing fields: a row may end before the city.
		city := ""
		if len(rec) > cityCol {
			city = rec[cityCol]
		}
		fn(line, rec[prefixCol], rec[countryCol], city)
	}
}

// validCountries are the codes of countries today: tzdata's ISO 3166 list plus
// XK (Kosovo). A layer row with any other code (a retired one such as YU,
// Yugoslavia) is skipped like an invalid code, so the country of a lower layer stays.
var validCountries = func() map[string]bool {
	valid := map[string]bool{"XK": true}
	for _, line := range strings.Split(countriesTab, "\n") {
		code, _, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && len(code) == 2 && code[0] >= 'A' && code[0] <= 'Z' && code[1] >= 'A' && code[1] <= 'Z' {
			valid[code] = true
		}
	}
	return valid
}()

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
