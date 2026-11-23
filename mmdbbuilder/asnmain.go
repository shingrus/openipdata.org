package main

// The ASN main city fill (mmdbbuilder geo -asn-main): a guess that gives the
// cityless addresses of an ASN the one city where the rest of its addresses in
// that country are. With -fallback three more guesses follow it, and -holders adds a
// fifth (fallback.go, holders.go).
//
// It is the one derived fill done inside the builder, because it needs the
// merged result of all layers. It is not a layer file.

import (
	"bufio"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/inserter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang/v2"
)

const (
	// asnMainSource is the city_source of the cities this fill writes: the single
	// name of the source. A city from it is a guess.
	asnMainSource = "80-asn-main"
	// asnMainMinShare: the main city must hold at least this share of the
	// addresses of the group (ASN and country) that have a city.
	asnMainMinShare = 0.90
	// asnMainMinAddrs: the addresses with a city a group needs, so that one
	// stray /32 does not decide for a whole network.
	asnMainMinAddrs = 256
)

// guessSources are the city_sources of the guessed cities, the one list: the ASN main
// city (80-asn-main), the six -fallback guesses (82-asn-likely, 84-asn-near,
// 86-country-main, 88-asn-far, 89-asn-anywhere, 89-country-near; see fallback.go) and the holder city (85-holder-city, -holders).
var guessSources = []string{asnMainSource, asnLikelySource, asnNearSource, holderCitySource, countryMainSource, asnFarSource, asnAnywhereSource, countryNearSource}

// isGuessSource reports whether a city_source names a guessed city. Guesses
// must not count as evidence for anything we build from our own MMDB (the router
// hop votes of lookup -in).
func isGuessSource(source string) bool { return slices.Contains(guessSources, source) }

// noSampleSources are the city_sources whose city is weak evidence: a /24 whose city comes
// from one of them still counts as having no city in the gap list (mmdbbuilder gaps), so
// sample-names keeps sampling customer PTR names there (a customer name is far more precise).
// They are the eight guesses (guessSources), the BGP community layer (25-bgp), the M-Lab speed
// test site layer (35-speedtest-site: right about 64% of the time, below a customer name), the
// M-Lab hop layer (36-mlab), the whois text layer (37-whois-text: right about 68% of the time, a
// customer name about 82%) and the ARIN registrant city layer (37-whois-arin: right about 61% of
// the time; sync-arin-whois walks the gap list, so the /24s it fills must stay in it) and the
// GitHub geofeed layer (39-github-geofeeds: right about 61% of the time, below a customer name),
// and since 2026-11-21 the PeeringDB layer (30-pdb: about 51%) and the router name layer
// (35-rdns-wide: about 36%), so sample-names reaches the networks those two cover (AT&T and
// Orange customer names carry a place code).
// lookup -in keeps only the guesses out (isGuessSource).
var noSampleSources = slices.Concat(guessSources, []string{"25-bgp", "30-pdb", "35-rdns-wide", "35-speedtest-site", "36-mlab", "37-whois-arin", "37-whois-text", "39-github-geofeeds"})

// isWeakCitySource reports whether a city_source is one of noSampleSources.
func isWeakCitySource(source string) bool { return slices.Contains(noSampleSources, source) }

// evidenceFields is ourFields for what feeds our own evidence: the city of a
// guessed record is left out, so the record counts as a country with no city.
func evidenceFields(res maxminddb.Result) (country, city string, err error) {
	country, city, _, err = evidenceRecord(res, isGuessSource)
	return country, city, err
}

// gapFields is evidenceFields for the gap list: the city of a weak source (see
// noSampleSources) is left out as well.
func gapFields(l geoLayout, res maxminddb.Result) (country, city string, err error) {
	country, city, _, err = l.evidence(res, isWeakCitySource)
	return country, city, err
}

// evidenceRecord decodes country, city and city_source of a record of the flat file
// (the build output "mmdbbuilder geo" writes); the city is left out (country only)
// when drop(city_source) is true. source is the record's own, also when dropped.
func evidenceRecord(res maxminddb.Result, drop func(string) bool) (country, city, source string, err error) {
	return layoutFlat.evidence(res, drop)
}

// geoLayout is the record layout of an ip2geo MMDB: the flat build output, or the
// published GeoLite2-City layout (mmdbbuilder convert).
type geoLayout int

const (
	layoutFlat geoLayout = iota // database_type ip2geo: country_code, city_name, city_source
	layoutCity                  // database_type with "City" (OpenIPData-City): country.iso_code, city.names.en, city_source
)

// detectLayout reads the layout from the file's database_type, once per run; any
// other type is an error.
func detectLayout(r *maxminddb.Reader) (geoLayout, error) {
	switch t := r.Metadata.DatabaseType; {
	case t == "ip2geo":
		return layoutFlat, nil
	case strings.Contains(t, "City"):
		return layoutCity, nil
	default:
		return 0, fmt.Errorf("database_type %q is neither ip2geo nor a City layout", t)
	}
}

// evidence decodes country, city and city_source in either layout, with the rule of
// evidenceRecord: the output is the same for the same data in both.
func (l geoLayout) evidence(res maxminddb.Result, drop func(string) bool) (country, city, source string, err error) {
	if l == layoutCity {
		var r struct {
			Country struct {
				ISOCode string `maxminddb:"iso_code"`
			} `maxminddb:"country"`
			City struct {
				Names struct {
					EN string `maxminddb:"en"`
				} `maxminddb:"names"`
			} `maxminddb:"city"`
			Source string `maxminddb:"city_source"`
		}
		if err = res.Decode(&r); err != nil {
			return "", "", "", err
		}
		country, city, source = r.Country.ISOCode, r.City.Names.EN, r.Source
	} else {
		var r struct {
			Country string `maxminddb:"country_code"`
			City    string `maxminddb:"city_name"`
			Source  string `maxminddb:"city_source"`
		}
		if err = res.Decode(&r); err != nil {
			return "", "", "", err
		}
		country, city, source = r.Country, r.City, r.Source
	}
	if drop(source) {
		city = ""
	}
	return country, city, source, nil
}

// asnSeg is a range of IPv4 addresses (inclusive) that belongs to one ASN.
type asnSeg struct{ lo, hi, asn uint32 }

// readASNPrefixes reads an asn-prefixes file: a gzipped CSV, header asn,prefix
// (columns found by name), one announced prefix of one ASN per row. Only IPv4
// rows are used (the measurement behind the fill was IPv4 only); an
// unparsable row, ASN 0 and IPv6 rows are skipped. It returns the segments:
// sorted, not overlapping, each address with the ASN of the longest announced
// prefix containing it. A prefix announced by more than one ASN belongs to
// nobody, but it still shadows the broader prefixes under it, as in the ASN
// geofeed layer: its addresses are never given to a broader prefix.
func readASNPrefixes(path string) ([]asnSeg, error) {
	segs, _, err := readASNData(path)
	return segs, err
}

// addrRange is a range of IPv4 addresses (inclusive).
type addrRange struct{ lo, hi uint32 }

// readASNData is readASNPrefixes that also returns the announced ranges: the merged union
// of ALL IPv4 prefixes of the file, a prefix announced by more than one ASN included (it
// has no segment, but it is announced). Sorted, not overlapping, neighbours merged.
func readASNData(path string) ([]asnSeg, []addrRange, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return nil, nil, err
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	header, err := r.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[strings.TrimSpace(name)] = i
	}
	asnCol, ok1 := col["asn"]
	prefixCol, ok2 := col["prefix"]
	if !ok1 || !ok2 {
		return nil, nil, fmt.Errorf("header %q lacks asn or prefix", strings.Join(header, ","))
	}
	// owner 0 marks an ambiguous prefix.
	owner := map[netip.Prefix]uint32{}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			var parseErr *csv.ParseError
			if errors.As(err, &parseErr) {
				continue
			}
			return nil, nil, err
		}
		if len(rec) <= max(asnCol, prefixCol) {
			continue
		}
		asn, err1 := strconv.ParseUint(strings.TrimSpace(rec[asnCol]), 10, 32)
		p, err2 := netip.ParsePrefix(strings.TrimSpace(rec[prefixCol]))
		if err1 != nil || err2 != nil || asn == 0 || !p.Addr().Is4() {
			continue
		}
		p = p.Masked()
		if have, ok := owner[p]; ok {
			if have != uint32(asn) {
				owner[p] = 0
			}
			continue
		}
		owner[p] = uint32(asn)
	}
	type entry struct{ lo, hi, asn uint32 }
	entries := make([]entry, 0, len(owner))
	for p, asn := range owner {
		lo, hi := prefixBounds(p)
		entries = append(entries, entry{lo, hi, asn})
	}
	// Broader first at the same start; the order is total, so the result is stable.
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.lo != b.lo {
			return a.lo < b.lo
		}
		return a.hi > b.hi
	})
	var segs []asnSeg
	var stack []entry
	var pos uint64 // first address not yet written
	emit := func(lo, hi uint64, asn uint32) {
		if lo > hi || asn == 0 {
			return
		}
		if n := len(segs); n > 0 && segs[n-1].asn == asn && uint64(segs[n-1].hi)+1 == lo {
			segs[n-1].hi = uint32(hi)
			return
		}
		segs = append(segs, asnSeg{uint32(lo), uint32(hi), asn})
	}
	pop := func() {
		t := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		emit(pos, uint64(t.hi), t.asn)
		pos = uint64(t.hi) + 1
	}
	for _, e := range entries {
		for len(stack) > 0 && stack[len(stack)-1].hi < e.lo {
			pop()
		}
		if len(stack) > 0 && pos < uint64(e.lo) {
			emit(pos, uint64(e.lo)-1, stack[len(stack)-1].asn)
		}
		pos = uint64(e.lo)
		stack = append(stack, e)
	}
	for len(stack) > 0 {
		pop()
	}
	// entries are sorted by start, so the union is one pass.
	var announced []addrRange
	for _, e := range entries {
		if n := len(announced); n > 0 && uint64(e.lo) <= uint64(announced[n-1].hi)+1 {
			announced[n-1].hi = max(announced[n-1].hi, e.hi)
			continue
		}
		announced = append(announced, addrRange{e.lo, e.hi})
	}
	return segs, announced, nil
}

// overlaps calls fn for each part of [lo, hi] that lies in a segment.
func overlaps(segs []asnSeg, lo, hi uint32, fn func(s asnSeg, a, b uint32)) {
	i := sort.Search(len(segs), func(i int) bool { return segs[i].hi >= lo })
	for ; i < len(segs) && segs[i].lo <= hi; i++ {
		fn(segs[i], max(lo, segs[i].lo), min(hi, segs[i].hi))
	}
}

// leaf is a range of IPv4 addresses of the merged tree with a country.
type leaf struct {
	lo, hi uint32
	rec    uint32 // index into the list of (country, city) pairs
}

type cityPair struct{ code, city string }

// treeLeaves lists, in address order, the IPv4 networks of the tree that have a
// country, with their (country, city): what the tree would hold if written now.
// The tree is read as it is (Tree.Get), never written, so the build is unchanged.
func treeLeaves(tree *mmdbwriter.Tree) ([]leaf, []cityPair) {
	var leaves []leaf
	var pairs []cityPair
	index := map[cityPair]uint32{}
	for addr := netip.IPv4Unspecified(); ; {
		n, value := tree.Get(addr.AsSlice())
		q := treePrefix(n, addr)
		lo, hi := prefixBounds(q)
		if m, ok := value.(mmdbtype.Map); ok {
			code, _ := m["country_code"].(mmdbtype.String)
			city, _ := m["city_name"].(mmdbtype.String)
			if code != "" {
				pair := cityPair{string(code), strings.TrimSpace(string(city))}
				id, ok := index[pair]
				if !ok {
					id = uint32(len(pairs))
					index[pair] = id
					pairs = append(pairs, pair)
				}
				if k := len(leaves); k > 0 && leaves[k-1].rec == id && leaves[k-1].hi+1 == lo {
					leaves[k-1].hi = hi
				} else {
					leaves = append(leaves, leaf{lo, hi, id})
				}
			}
		}
		if hi == 1<<32-1 {
			return leaves, pairs
		}
		addr = netip.AddrFrom4([4]byte{byte((hi + 1) >> 24), byte((hi + 1) >> 16), byte((hi + 1) >> 8), byte(hi + 1)})
	}
}

// asnGroup is the addresses of one ASN in one country that have a city.
type asnGroup struct {
	total  uint64
	cities map[string]*cityCount // by city key: the normalised name, or with GeoNames the place (placeKey)
	// set by choose when the group qualifies
	main string // 80-asn-main: the city to write
	ok   bool
	// 82-asn-likely: the same, at a lower share and with no minimum of addresses
	likely   string
	likelyOK bool
}

// add counts n addresses of a city: key says which city it is (the normalised name norm, or
// with GeoNames the place, so that every spelling and name of one place counts together), and
// spell is how the record writes it.
func (g *asnGroup) add(key, norm, spell string, n uint64) {
	g.total += n
	c := g.cities[key]
	if c == nil {
		c = &cityCount{name: norm, spells: map[string]uint64{}}
		g.cities[key] = c
	}
	if norm < c.name {
		c.name = norm
	}
	c.n += n
	c.spells[spell] += n
}

type cityCount struct {
	n      uint64
	name   string // the smallest normalised name counted here: the tie-break between cities
	spells map[string]uint64
}

// placeKey is the city key of a place GeoNames gave: its coordinates. Two names that the index
// places at the same place (São Paulo and Sao Paulo, Kyiv and Kiev) get the same key.
func placeKey(p gnPlace) string {
	return strconv.FormatUint(math.Float64bits(p.lat), 16) + "," + strconv.FormatUint(math.Float64bits(p.lon), 16)
}

type groupKey struct {
	asn  uint32
	code string
}

// choose finds the main city of the group and whether it qualifies: the group needs at
// least minAddrs addresses with a city and its main city at least minShare of them. Ties
// are broken by name, so the result does not depend on the order of the addresses. The
// city is the spelling with most addresses (ties: the smallest spelling).
func (g *asnGroup) choose(minShare float64, minAddrs uint64) (main string, ok bool) {
	var best *cityCount
	var bestKey string
	for key, c := range g.cities {
		if best == nil || c.n > best.n || c.n == best.n && (c.name < best.name || c.name == best.name && key < bestKey) {
			best, bestKey = c, key
		}
	}
	if best == nil || g.total < minAddrs || float64(best.n)/float64(g.total) < minShare {
		return "", false
	}
	return best.spelling(), true
}

// spelling is the most common spelling of the city, ties by the smallest spelling.
func (c *cityCount) spelling() string {
	var spell string
	var spellN uint64
	for s, n := range c.spells {
		if n > spellN || n == spellN && s < spell {
			spell, spellN = s, n
		}
	}
	return spell
}

// rangePrefixes splits the IPv4 range lo..hi into the fewest CIDR prefixes.
func rangePrefixes(lo, hi uint32) []netip.Prefix {
	var out []netip.Prefix
	for cur := uint64(lo); cur <= uint64(hi); {
		size := uint64(1) << bits.TrailingZeros64(cur|1<<32) // largest block aligned at cur
		for cur+size-1 > uint64(hi) {
			size >>= 1
		}
		a := uint32(cur)
		out = append(out, netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a)}), 32-bits.TrailingZeros64(size)))
		cur += size
	}
	return out
}

// whereCountryNoCity writes record over the networks that have the country
// code and no city, and leaves the others as they are: the fill only fills.
func whereCountryNoCity(record mmdbtype.Map, code mmdbtype.DataType) inserter.Func {
	return func(existing mmdbtype.DataType) (mmdbtype.DataType, error) {
		m, ok := existing.(mmdbtype.Map)
		if !ok {
			return existing, nil
		}
		if have, ok := m["country_code"]; !ok || !have.Equal(code) {
			return existing, nil
		}
		if city, ok := m["city_name"].(mmdbtype.String); ok && strings.TrimSpace(string(city)) != "" {
			return existing, nil
		}
		return record, nil
	}
}
