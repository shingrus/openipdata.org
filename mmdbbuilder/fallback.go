package main

// The ladder of guessed cities (mmdbbuilder geo -asn-main and -fallback).
//
// Each address that has a country and no city gets the FIRST guess that applies:
//
//  1. 80-asn-main: the main city of its (ASN, country) group, when that city holds at
//     least 90% of the group's addresses with a city and the group has at least 256 of
//     them (asnmain.go).
//  2. 82-asn-likely: the same main city at a lower bar: at least 70% of the group's
//     addresses with a city, no minimum number of addresses.
//  3. 84-asn-near: the city of the nearest piece of the same group that has a city, when
//     that piece lies in the same /20. Decided per /24 chunk of the cityless part.
//  4. 85-holder-city: only for address space that no network announces (not even a prefix
//     announced by two ASNs): the city of the most specific holder block (-holders) that
//     covers it, when the block's country is the country of the address (holders.go).
//  5. 86-country-main: the main city of the country over all addresses with a city, with
//     or without an ASN, when it holds at least 30% of them.
//  6. 88-asn-far: only where the country has NO main city (rung 5 cannot apply): the city
//     of the nearest piece of the same group, like rung 3 but when the piece lies in the
//     same /16. Decided per /24 chunk.
//  7. 89-asn-anywhere: only for a chunk rung 6 gave nothing (the country has
//     no main city and the nearest piece is outside the same /16): the city of the nearest
//     piece of the same group, however far away in address space. A chunk whose
//     group has no piece at all goes on to rung 8.
//  8. 89-country-near: the last rung, for what rung 7 left in a country with no main city
//     (a part with no ASN, or in a group with no piece): the city of the nearest address
//     with a city in the same country, from any network, however far away. Decided per /24
//     chunk. A country with no address with a city stays without one.
//
// Rungs 80 to 84, 88, 89 and 89-country-near need the ASN prefixes (-asn-main); 82, 84, 86, 88, 89 and 89-country-near need
// -fallback; rung 85 needs -holders, which needs -asn-main (the announced prefixes say what is unannounced).
// Without -fallback and -holders only rung 80 runs, byte for byte as before. A part that
// has an ASN never gets rung 85.
//
// All evidence (group sums, pieces, country sums) comes from ONE snapshot of the tree
// taken after all layers and before anything is inserted, and every cityless part is
// given to its rung before the first insert, so a guess never counts as evidence for
// another guess and the stats count real fills. A city counts as evidence only when it
// "counts": with a GeoNames index when the index places it, without one always. Cities
// that do not count are ignored for the sums and are never overwritten. With an index a
// city is the place the index gives it, so every spelling and name of one place counts
// together (São Paulo and Sao Paulo); without one a city is its normalised name. IPv4 only.

import (
	"fmt"
	"sort"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

const (
	// asnLikelySource, asnNearSource, countryMainSource, asnFarSource,
	// asnAnywhereSource and countryNearSource are the city_sources of the six -fallback guesses.
	// countryNearSource carries an 8 like every guess (the comparison page says guessed
	// sources start with 8); it is the last rung.
	asnLikelySource   = "82-asn-likely"
	asnNearSource     = "84-asn-near"
	countryMainSource = "86-country-main"
	asnFarSource      = "88-asn-far"
	asnAnywhereSource = "89-asn-anywhere"
	countryNearSource = "89-country-near"

	// asnLikelyMinShare: the group's main city needs this share of its addresses with a
	// city (no minimum number of addresses).
	asnLikelyMinShare = 0.70
	// countryMainMinShare: the country's main city needs this share of the country's
	// addresses with a city.
	countryMainMinShare = 0.30
	// asnNearSameBlock: the nearest piece must be in the same block of 2^12 addresses
	// (a /20) as the chunk's middle address.
	asnNearSameBlock = 12
	// asnFarSameBlock: for rung 88 the nearest piece must be in the same block of 2^16
	// addresses (a /16) as the chunk's middle address.
	asnFarSameBlock = 16
	// asnAnywhereBlock: for rung 89 there is no distance limit. nearPiece compares
	// boundary>>block with x>>block, and in Go a shift of a uint32 by 32 or more gives 0,
	// so with block 32 every piece is in the same "block" as x.
	asnAnywhereBlock = 32
)

// fill is a range to write: addresses lo to hi (inclusive) get the city in the country.
type fill struct {
	lo, hi uint32
	code   string
	city   spelled // the city and its region (city_region)
}

// rung collects the fills of one guess, in address order.
type rung struct {
	stats geoLayerStats
	fills []fill
	addrs uint64
}

// add appends the range, merging it into the one before when that ends right before it
// with the same country and city.
func (r *rung) add(lo, hi uint32, code string, city spelled) {
	r.addrs += uint64(hi) - uint64(lo) + 1
	if k := len(r.fills); k > 0 && r.fills[k-1].code == code && r.fills[k-1].city == city && uint64(r.fills[k-1].hi)+1 == uint64(lo) {
		r.fills[k-1].hi = hi
		return
	}
	r.fills = append(r.fills, fill{lo, hi, code, city})
}

// piece is the part of a leaf with a counted city that lies in the ASN segment of its
// group: the evidence of rung 84.
type piece struct {
	lo, hi uint32
	rec    uint32 // index into the (country, city) pairs
}

// nearPiece decides rung 84 (block 12), rung 88 (block 16), or rung 89 and 89-country-near (block 32, no limit) for one chunk whose middle
// address is x: the nearest of the last piece starting at or before x and the first piece starting after x (pieces never
// overlap the chunk). On equal distance the bigger piece wins, then the earlier one. The
// piece counts only when its nearest edge (its end before x, its start after x) is in the
// same block of 2^block addresses as x.
func nearPiece(pieces []piece, x uint32, block uint) (rec uint32, ok bool) {
	i := sort.Search(len(pieces), func(i int) bool { return pieces[i].lo > x })
	var a, b *piece
	if i > 0 {
		a = &pieces[i-1]
	}
	if i < len(pieces) {
		b = &pieces[i]
	}
	var win *piece
	var boundary uint32
	switch {
	case a == nil && b == nil:
		return 0, false
	case b == nil:
		win, boundary = a, a.hi
	case a == nil:
		win, boundary = b, b.lo
	default:
		da, db := uint64(x)-uint64(a.hi), uint64(b.lo)-uint64(x)
		sizeA, sizeB := uint64(a.hi)-uint64(a.lo), uint64(b.hi)-uint64(b.lo)
		if da < db || da == db && sizeA >= sizeB {
			win, boundary = a, a.hi
		} else {
			win, boundary = b, b.lo
		}
	}
	if boundary>>block != x>>block {
		return 0, false
	}
	return win.rec, true
}

// fillGuesses runs the ladder on the tree and returns one stats entry per rung that ran,
// in order: 80-asn-main and, with fallback, 82-asn-likely and 84-asn-near (all three only
// when asnPath, the asn-prefixes file, is given), then 85-holder-city (only when
// holdersPath is given, which needs asnPath), then with fallback 86-country-main and,
// with asnPath too, 88-asn-far, 89-asn-anywhere and 89-country-near.
// recordFor makes the shared record of a place. It only fills: an address that has a
// city, or another country, is never touched. The result is deterministic. It also returns
// the ASN segments it read (nil without asnPath), which the RTT city check reuses.
func fillGuesses(tree *mmdbwriter.Tree, asnPath string, fallback bool, index geoNames, holdersPath string, debug bool, recordFor func(place, *geoLayerStats) int, records func(int) mmdbtype.Map) ([]geoLayerStats, []asnSeg, error) {
	useASN := asnPath != ""
	useHolders := holdersPath != ""
	if useHolders && !useASN {
		return nil, nil, fmt.Errorf("%s: holders need the ASN prefixes", holdersPath)
	}
	var segs []asnSeg
	var announced []addrRange
	if useASN {
		var err error
		if segs, announced, err = readASNData(asnPath); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", asnPath, err)
		}
	}
	var holders []holderSeg
	var holderBlocks, holderSkipped int
	if useHolders {
		var err error
		if holders, holderBlocks, holderSkipped, err = readHolders(holdersPath); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", holdersPath, err)
		}
	}
	leaves, pairs := treeLeaves(tree) // the snapshot

	// With a GeoNames index only the cities it places count (the measured definition);
	// the answer is cached per (country, city) pair, 0 unknown, 1 placed, 2 not placed.
	placedState := make([]uint8, len(pairs))
	var placeKeys []string // with an index: the place of each counted city (placeKey)
	if index != nil {
		placeKeys = make([]string, len(pairs))
	}
	counts := func(rec uint32) bool {
		if index == nil {
			return true
		}
		if placedState[rec] == 0 {
			placedState[rec] = 2
			if pl, ok := index.placeOf(place{code: pairs[rec].code, city: pairs[rec].city, region: pairs[rec].region}); ok {
				placedState[rec] = 1
				placeKeys[rec] = placeKey(pl)
			}
		}
		return placedState[rec] == 1
	}

	// Evidence: the addresses with a counted city per (ASN, country) group and per
	// country, and the pieces of every group.
	groups := map[groupKey]*asnGroup{}
	countries := map[string]*asnGroup{}
	pieces := map[groupKey][]piece{}
	// countryPieces: the pieces of each country over all networks (rung 89-country-near),
	// in address order because the leaves are. Only with -fallback and the ASN prefixes.
	var countryPieces map[string][]piece
	if fallback && useASN {
		countryPieces = map[string][]piece{}
	}
	newGroup := func() *asnGroup { return &asnGroup{cities: map[string]*cityCount{}} }
	for _, l := range leaves {
		p := pairs[l.rec]
		norm := normCity(p.city)
		if norm == "" || !counts(l.rec) {
			continue
		}
		// With GeoNames a city is its place, so two spellings or names of one place count
		// together; without it a city is its normalised name.
		city := norm
		if index != nil {
			city = placeKeys[l.rec]
		}
		if fallback {
			g := countries[p.code]
			if g == nil {
				g = newGroup()
				countries[p.code] = g
			}
			g.add(city, norm, spelled{p.city, p.region}, uint64(l.hi)-uint64(l.lo)+1)
			if useASN {
				countryPieces[p.code] = append(countryPieces[p.code], piece{l.lo, l.hi, l.rec})
			}
		}
		if !useASN {
			continue
		}
		overlaps(segs, l.lo, l.hi, func(seg asnSeg, a, b uint32) {
			key := groupKey{seg.asn, p.code}
			g := groups[key]
			if g == nil {
				g = newGroup()
				groups[key] = g
			}
			g.add(city, norm, spelled{p.city, p.region}, uint64(b)-uint64(a)+1)
			if fallback {
				pieces[key] = append(pieces[key], piece{a, b, l.rec})
			}
		})
	}
	qualified80, qualified82 := 0, 0
	for _, g := range groups {
		if g.main, g.ok = g.choose(asnMainMinShare, asnMainMinAddrs); g.ok {
			qualified80++
		}
		if fallback {
			if g.likely, g.likelyOK = g.choose(asnLikelyMinShare, 0); g.likelyOK && !g.ok {
				qualified82++
			}
		}
	}
	qualifiedCountries := 0
	for _, g := range countries {
		if g.main, g.ok = g.choose(countryMainMinShare, 0); g.ok {
			qualifiedCountries++
		}
	}

	// Give every cityless part to the first rung that applies, in address order.
	var r80, r82, r84, r85, r86, r88, r89, r89c rung
	r80.stats.name, r82.stats.name, r84.stats.name, r85.stats.name, r86.stats.name, r88.stats.name, r89.stats.name, r89c.stats.name = asnMainSource, asnLikelySource, asnNearSource, holderCitySource, countryMainSource, asnFarSource, asnAnywhereSource, countryNearSource
	r85.stats.skipped = holderSkipped
	for _, l := range leaves {
		p := pairs[l.rec]
		if normCity(p.city) != "" {
			continue // has a city (counted or not): never touched
		}
		// country is the last stop for a part nothing before it took: rung 86, or in a
		// country with no main city (with the ASN prefixes) 89-country-near.
		country := func(a, b uint32) {
			g := countries[p.code]
			if !fallback || g == nil {
				return
			}
			if g.ok {
				r86.add(a, b, p.code, g.main)
				return
			}
			// No main city: the nearest address with a city of the country, per /24 chunk.
			ps := countryPieces[p.code]
			if len(ps) == 0 {
				return
			}
			for c := a; ; {
				e := min(b, c|0xff)
				if rec, ok := nearPiece(ps, c+(e-c)/2, asnAnywhereBlock); ok {
					r89c.add(c, e, p.code, pairs[rec].spelled())
				}
				if e == b {
					break
				}
				c = e + 1
			}
		}
		part := func(a, b uint32, asn uint32, hasASN bool) {
			if !hasASN {
				if !useHolders {
					country(a, b)
					return
				}
				// Nobody announces the part, or two ASNs do: only the unannounced
				// addresses can get the holder city.
				splitRange(announced, a, b, country, func(c, d uint32) {
					holderParts(holders, c, d, p.code, func(e, f uint32, city, region string) {
						// The holder's region is the state the exporter placed the city in: the
						// city is placed in that state by the same rule as a layer row's.
						sp := spelled{city: city}
						if index != nil {
							_, sp.region, _ = index.placeIn(p.code, region, city)
						}
						r85.add(e, f, p.code, sp)
					}, country)
				})
				return
			}
			key := groupKey{asn, p.code}
			if g := groups[key]; g != nil {
				if g.ok {
					r80.add(a, b, p.code, g.main)
					return
				}
				if fallback && g.likelyOK {
					r82.add(a, b, p.code, g.likely)
					return
				}
			}
			ps := pieces[key]
			if !fallback || len(ps) == 0 {
				country(a, b)
				return
			}
			// Rungs 84, 88 and 89 are decided per /24 chunk, by the chunk's middle address. A
			// chunk rung 84 does not take gets the country's main city as before; only where
			// the country has none does rung 88 look farther, and then rung 89 without a limit.
			for c := a; ; {
				e := min(b, c|0xff)
				x := c + (e-c)/2
				if rec, ok := nearPiece(ps, x, asnNearSameBlock); ok {
					r84.add(c, e, p.code, pairs[rec].spelled())
				} else if g := countries[p.code]; g != nil && g.ok {
					r86.add(c, e, p.code, g.main)
				} else if rec, ok := nearPiece(ps, x, asnFarSameBlock); ok {
					r88.add(c, e, p.code, pairs[rec].spelled())
				} else if rec, ok := nearPiece(ps, x, asnAnywhereBlock); ok {
					r89.add(c, e, p.code, pairs[rec].spelled())
				}
				if e == b {
					break
				}
				c = e + 1
			}
		}
		pos := uint64(l.lo) // first address of the leaf not yet handed out
		if useASN {
			overlaps(segs, l.lo, l.hi, func(seg asnSeg, a, b uint32) {
				if uint64(a) > pos {
					part(uint32(pos), a-1, 0, false)
				}
				part(a, b, seg.asn, true)
				pos = uint64(b) + 1
			})
		}
		if pos <= uint64(l.hi) {
			part(uint32(pos), l.hi, 0, false)
		}
	}

	r80.stats.note = fmt.Sprintf("asn-main: %d groups qualified of %d, %d addresses filled", qualified80, len(groups), r80.addrs)
	r82.stats.note = fmt.Sprintf("asn-likely: %d groups qualified of %d, %d addresses filled", qualified82, len(groups), r82.addrs)
	r84.stats.note = fmt.Sprintf("asn-near: %d addresses filled", r84.addrs)
	r85.stats.note = fmt.Sprintf("holder-city: %d blocks, %d addresses filled", holderBlocks, r85.addrs)
	r86.stats.note = fmt.Sprintf("country-main: %d countries qualified of %d, %d addresses filled", qualifiedCountries, len(countries), r86.addrs)
	r88.stats.note = fmt.Sprintf("asn-far: %d addresses filled", r88.addrs)
	r89.stats.note = fmt.Sprintf("asn-anywhere: %d addresses filled", r89.addrs)
	r89c.stats.note = fmt.Sprintf("country-near: %d addresses filled", r89c.addrs)
	var ran []*rung
	if useASN {
		ran = append(ran, &r80)
		if fallback {
			ran = append(ran, &r82, &r84)
		}
	}
	if useHolders {
		ran = append(ran, &r85)
	}
	if fallback {
		ran = append(ran, &r86)
		if useASN {
			ran = append(ran, &r88, &r89, &r89c)
		}
	}
	stats := make([]geoLayerStats, 0, len(ran))
	for _, r := range ran {
		if err := r.insert(tree, debug, recordFor, records); err != nil {
			return stats, segs, err
		}
		stats = append(stats, r.stats)
	}
	return stats, segs, nil
}

// insert writes the fills into the tree, only where the address still has the fill's
// country and no city.
func (r *rung) insert(tree *mmdbwriter.Tree, debug bool, recordFor func(place, *geoLayerStats) int, records func(int) mmdbtype.Map) error {
	source := r.stats.name
	for _, f := range r.fills {
		record := records(recordFor(place{code: f.code, city: f.city.city, source: source, region: f.city.region}, &r.stats))
		for _, cidr := range rangePrefixes(f.lo, f.hi) {
			rec := record
			if debug {
				rec = mmdbtype.Map{"network": mmdbtype.String(cidr.String())}
				for k, v := range record {
					rec[k] = v
				}
			}
			if err := tree.InsertFunc(ipNet(cidr), whereCountryNoCity(rec, record["country_code"])); err != nil {
				return fmt.Errorf("insert %s (%s): %w", cidr, source, err)
			}
			r.stats.prefixes++
		}
		r.stats.rows++
	}
	return nil
}
