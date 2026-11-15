package main

// The ASN main city fill (mmdbbuilder geo -asn-main): a guess that gives the
// cityless addresses of an ASN the one city where the rest of its addresses in
// that country are.
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
	"math/bits"
	"net/netip"
	"os"
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

// isGuessSource reports whether a city_source names a guessed city. Guesses
// must not count as evidence for anything we build from our own MMDB (the gap
// list, the router hop votes of lookup -in).
func isGuessSource(source string) bool { return source == asnMainSource }

// evidenceFields is ourFields for what feeds our own evidence: the city of a
// guessed record is left out, so the record counts as a country with no city.
func evidenceFields(res maxminddb.Result) (country, city string, err error) {
	var r struct {
		Country string `maxminddb:"country_code"`
		City    string `maxminddb:"city_name"`
		Source  string `maxminddb:"city_source"`
	}
	if err = res.Decode(&r); err != nil {
		return "", "", err
	}
	if isGuessSource(r.Source) {
		r.City = ""
	}
	return r.Country, r.City, nil
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
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[strings.TrimSpace(name)] = i
	}
	asnCol, ok1 := col["asn"]
	prefixCol, ok2 := col["prefix"]
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("header %q lacks asn or prefix", strings.Join(header, ","))
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
			return nil, err
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
	return segs, nil
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
	cities map[string]*cityCount // by normalised city
	// set when the group qualifies
	main string // city to write (the spelling with most addresses)
	ok   bool
}

type cityCount struct {
	n      uint64
	spells map[string]uint64
}

type groupKey struct {
	asn  uint32
	code string
}

// choose finds the main city and whether the group qualifies. Ties are broken
// by name, so the result does not depend on the order of the addresses.
func (g *asnGroup) choose() {
	var best *cityCount
	var bestName string
	for name, c := range g.cities {
		if best == nil || c.n > best.n || c.n == best.n && name < bestName {
			best, bestName = c, name
		}
	}
	if best == nil || g.total < asnMainMinAddrs || float64(best.n)/float64(g.total) < asnMainMinShare {
		return
	}
	var spell string
	var spellN uint64
	for s, n := range best.spells {
		if n > spellN || n == spellN && s < spell {
			spell, spellN = s, n
		}
	}
	g.main, g.ok = spell, true
}

// fillASNMain gives the main city of each qualifying (ASN, country) group to
// the addresses of that ASN in that country that have no city, in the tree. A
// group qualifies when it has at least asnMainMinAddrs addresses with a city
// and one city holds at least asnMainMinShare of them. It only fills: an
// address that has a city, or another country, is never touched. recordFor
// makes the shared record of a place. The result is deterministic.
//
// With a GeoNames index (nil: none) a city counts toward a group's total and its
// per-city sums only when the index places it, as in the measurement behind the
// fill; cities it cannot place are ignored for the count (and still never
// overwritten). Without an index every city counts.
func fillASNMain(tree *mmdbwriter.Tree, path string, index geoNames, debug bool, recordFor func(place, *geoLayerStats) int, records func(int) mmdbtype.Map) (geoLayerStats, error) {
	s := geoLayerStats{name: asnMainSource}
	segs, err := readASNPrefixes(path)
	if err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	leaves, pairs := treeLeaves(tree)

	// With a GeoNames index only the cities it places count (the measured definition);
	// the answer is cached per (country, city) pair, 0 unknown, 1 placed, 2 not placed.
	placedState := make([]uint8, len(pairs))
	counts := func(rec uint32) bool {
		if index == nil {
			return true
		}
		if placedState[rec] == 0 {
			placedState[rec] = 2
			if _, ok := index.place(pairs[rec].code, pairs[rec].city); ok {
				placedState[rec] = 1
			}
		}
		return placedState[rec] == 1
	}
	groups := map[groupKey]*asnGroup{}
	for _, l := range leaves {
		p := pairs[l.rec]
		if normCity(p.city) == "" || !counts(l.rec) {
			continue
		}
		overlaps(segs, l.lo, l.hi, func(seg asnSeg, a, b uint32) {
			key := groupKey{seg.asn, p.code}
			g := groups[key]
			if g == nil {
				g = &asnGroup{cities: map[string]*cityCount{}}
				groups[key] = g
			}
			n := uint64(b) - uint64(a) + 1
			g.total += n
			norm := normCity(p.city)
			c := g.cities[norm]
			if c == nil {
				c = &cityCount{spells: map[string]uint64{}}
				g.cities[norm] = c
			}
			c.n += n
			c.spells[p.city] += n
		})
	}
	qualified := 0
	for _, g := range groups {
		if g.choose(); g.ok {
			qualified++
		}
	}

	// The ranges to fill, in address order; neighbours of one group are one range.
	type fill struct {
		lo, hi uint32
		code   string
		city   string
	}
	var fills []fill
	var addrs uint64
	for _, l := range leaves {
		p := pairs[l.rec]
		if normCity(p.city) != "" {
			continue
		}
		overlaps(segs, l.lo, l.hi, func(seg asnSeg, a, b uint32) {
			g := groups[groupKey{seg.asn, p.code}]
			if g == nil || !g.ok {
				return
			}
			addrs += uint64(b) - uint64(a) + 1
			if k := len(fills); k > 0 && fills[k-1].code == p.code && fills[k-1].city == g.main && uint64(fills[k-1].hi)+1 == uint64(a) {
				fills[k-1].hi = b
				return
			}
			fills = append(fills, fill{a, b, p.code, g.main})
		})
	}
	for _, f := range fills {
		idx := recordFor(place{f.code, f.city, asnMainSource}, &s)
		record := records(idx)
		for _, cidr := range rangePrefixes(f.lo, f.hi) {
			rec := record
			if debug {
				rec = mmdbtype.Map{"network": mmdbtype.String(cidr.String())}
				for k, v := range record {
					rec[k] = v
				}
			}
			if err := tree.InsertFunc(ipNet(cidr), whereCountryNoCity(rec, record["country_code"])); err != nil {
				return s, fmt.Errorf("insert %s (%s): %w", cidr, asnMainSource, err)
			}
			s.prefixes++
		}
		s.rows++
	}
	s.note = fmt.Sprintf("asn-main: %d groups qualified of %d, %d addresses filled", qualified, len(groups), addrs)
	return s, nil
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
