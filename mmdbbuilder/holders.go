package main

// The holder city guess (mmdbbuilder geo -holders): rung 85 of the ladder in fallback.go.
//
// The file is another workflow's output (a gzipped CSV in the layer layout, one row per
// address block of a holder with the holder's country and city), so every row is untrusted.
// Address space that no network announces, and that lies inside such a block, gets the
// block's city when the block's country is the country of the address.

import (
	"net/netip"
	"sort"
	"strings"
)

// holderCitySource is the city_source of the cities rung 85 writes. A city from it is a guess.
const holderCitySource = "85-holder-city"

// holderMinBits: a block broader than a /8 is not a holder block.
const holderMinBits = 8

// holderSeg is a range of IPv4 addresses (inclusive) with the country and city of the
// most specific holder block that contains it.
type holderSeg struct {
	lo, hi                uint32
	country, city, region string
}

// readHolders reads a holders file. It returns the segments (sorted, not overlapping:
// each address has the most specific block containing it; for the same prefix twice the
// last row wins), the number of usable rows and the number of skipped rows. A row is
// usable when its prefix parses, is IPv4 and no broader than a /8, its country is two
// letters A-Z and its city is not empty after cleanCity. A file that cannot be read or
// has no usable header is an error; no usable row is not.
func readHolders(path string) (segs []holderSeg, usable, skipped int, err error) {
	type block struct{ country, city, region string }
	blocks := map[netip.Prefix]block{}
	err = readLayerRegions(path, func(_ int, prefix, code, region, city string) {
		p, perr := parseGeoPrefix(prefix)
		code = strings.ToUpper(strings.TrimSpace(code))
		city = cleanCity(city)
		if perr != nil || !p.Addr().Is4() || p.Bits() < holderMinBits || city == "" || len(code) != 2 ||
			code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
			skipped++
			return
		}
		usable++
		blocks[p] = block{code, city, strings.TrimSpace(region)}
	})
	if err != nil {
		return nil, 0, 0, err
	}
	type entry struct {
		lo, hi uint32
		block
	}
	entries := make([]entry, 0, len(blocks))
	for p, b := range blocks {
		lo, hi := prefixBounds(p)
		entries = append(entries, entry{lo, hi, b})
	}
	// Broader first at the same start; the order is total, so the result is stable.
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.lo != b.lo {
			return a.lo < b.lo
		}
		return a.hi > b.hi
	})
	var stack []entry
	var pos uint64 // first address not yet written
	emit := func(lo, hi uint64, b block) {
		if lo > hi {
			return
		}
		segs = append(segs, holderSeg{uint32(lo), uint32(hi), b.country, b.city, b.region})
	}
	pop := func() {
		t := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		emit(pos, uint64(t.hi), t.block)
		pos = uint64(t.hi) + 1
	}
	for _, e := range entries {
		for len(stack) > 0 && stack[len(stack)-1].hi < e.lo {
			pop()
		}
		if len(stack) > 0 && pos < uint64(e.lo) {
			emit(pos, uint64(e.lo)-1, stack[len(stack)-1].block)
		}
		pos = uint64(e.lo)
		stack = append(stack, e)
	}
	for len(stack) > 0 {
		pop()
	}
	return segs, usable, skipped, nil
}

// splitRange calls in for each part of [lo, hi] that lies in one of the ranges (sorted,
// not overlapping) and out for each part that lies in none, in address order.
func splitRange(rs []addrRange, lo, hi uint32, in func(a, b uint32), out func(a, b uint32)) {
	i := sort.Search(len(rs), func(i int) bool { return rs[i].hi >= lo })
	pos := uint64(lo)
	for ; i < len(rs) && rs[i].lo <= hi; i++ {
		a, b := max(lo, rs[i].lo), min(hi, rs[i].hi)
		if uint64(a) > pos {
			out(uint32(pos), a-1)
		}
		in(a, b)
		pos = uint64(b) + 1
	}
	if pos <= uint64(hi) {
		out(uint32(pos), hi)
	}
}

// holderParts splits [lo, hi] of a leaf in country code by the holder segments: fill gets
// each part that a segment of the same country covers, rest every other part.
func holderParts(segs []holderSeg, lo, hi uint32, code string, fill func(a, b uint32, city, region string), rest func(a, b uint32)) {
	i := sort.Search(len(segs), func(i int) bool { return segs[i].hi >= lo })
	pos := uint64(lo)
	for ; i < len(segs) && segs[i].lo <= hi; i++ {
		s := segs[i]
		a, b := max(lo, s.lo), min(hi, s.hi)
		if uint64(a) > pos {
			rest(uint32(pos), a-1)
		}
		if s.country == code {
			fill(a, b, s.city, s.region)
		} else {
			rest(a, b)
		}
		pos = uint64(b) + 1
	}
	if pos <= uint64(hi) {
		rest(uint32(pos), hi)
	}
}
