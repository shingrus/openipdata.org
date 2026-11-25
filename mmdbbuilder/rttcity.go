package main

// The RTT city check (mmdbbuilder geo -rtt-city): the speed-of-light test of rttcountry.go
// applied to cities.
//
// Each client /24 of the RTT file (the same file as -rtt-country) has, per M-Lab server, a
// lowest round-trip time r ms. The client is at most r * 100 km + 100 km from the server, so
// the /24 lies inside one disk per row. A part of the tree inside the /24 whose city has a
// point outside any disk has an impossible city, unless its city_source is 70-geofeeds (the
// operator's own statement). It is replaced, in the same country, by the first of:
//
//	B: the city with most addresses in the (ASN, country) group of the /24 that lies inside
//	   every disk (cities counted as the guesses count them: placed by GeoNames, not a guess);
//	   city_source 87-rtt-network;
//	C: the most populous city of the country (any source) that lies inside every disk;
//	   city_source 87-rtt-country;
//	else the record stays.
//
// Both are guesses (guessSources). The check runs after all layers and after the guesses, just
// before the write; evidence comes from one snapshot of the tree taken before it changes
// anything, and a guessed city never counts as evidence for B. It needs -geonames; without
// -asn-main only C is tried. IPv4 only; the output without the flag is unchanged.

import (
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strings"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/inserter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

const (
	// The city_sources of the two replacements, both guesses.
	rttNetworkSource = "87-rtt-network"
	rttCountrySource = "87-rtt-country"

	rttCityKmPerMs  = 100.0 // km one way per ms of round-trip time
	rttCityMarginKm = 100.0 // added to every disk
	rttCityKeep     = "70-geofeeds"
)

type rttCityCounts struct {
	rows, skipped, clients, impossible, byNet, byCountry, kept int
}

func (c rttCityCounts) String() string {
	return fmt.Sprintf("rtt-city: %d rows, %d skipped, %d /24s, %d impossible parts, %d 87-rtt-network, %d 87-rtt-country, %d kept",
		c.rows, c.skipped, c.clients, c.impossible, c.byNet, c.byCountry, c.kept)
}

// cityLeaf is a range of the snapshot with a country, a city and its source.
type cityLeaf struct {
	lo, hi uint32
	rec    uint32
}

type cityRec struct{ code, city, source string }

// snapshotCities lists the IPv4 networks of the tree that have a country, in address order
// (like treeLeaves, but with the city_source). The tree is only read.
func snapshotCities(tree *mmdbwriter.Tree) ([]cityLeaf, []cityRec) {
	var leaves []cityLeaf
	var recs []cityRec
	index := map[cityRec]uint32{}
	for addr := netip.IPv4Unspecified(); ; {
		n, value := tree.Get(addr.AsSlice())
		q := treePrefix(n, addr)
		lo, hi := prefixBounds(q)
		if m, ok := value.(mmdbtype.Map); ok {
			code, _ := m["country_code"].(mmdbtype.String)
			city, _ := m["city_name"].(mmdbtype.String)
			src, _ := m["city_source"].(mmdbtype.String)
			if code != "" {
				r := cityRec{string(code), strings.TrimSpace(string(city)), string(src)}
				id, ok := index[r]
				if !ok {
					id = uint32(len(recs))
					index[r] = id
					recs = append(recs, r)
				}
				if k := len(leaves); k > 0 && leaves[k-1].rec == id && leaves[k-1].hi+1 == lo {
					leaves[k-1].hi = hi
				} else {
					leaves = append(leaves, cityLeaf{lo, hi, id})
				}
			}
		}
		if hi == 1<<32-1 {
			return leaves, recs
		}
		addr = netip.AddrFrom4([4]byte{byte((hi + 1) >> 24), byte((hi + 1) >> 16), byte((hi + 1) >> 8), byte(hi + 1)})
	}
}

// cityEvidence holds the counted cities of the snapshot.
type cityEvidence struct {
	groups    map[groupKey]*asnGroup // not-guess cities per (ASN, country)
	countries map[string]*asnGroup   // every counted city per country
}

func buildCityEvidence(leaves []cityLeaf, recs []cityRec, segs []asnSeg, index geoNames) *cityEvidence {
	ev := &cityEvidence{groups: map[groupKey]*asnGroup{}, countries: map[string]*asnGroup{}}
	newGroup := func() *asnGroup { return &asnGroup{cities: map[string]*cityCount{}} }
	keys := make([]string, len(recs))
	state := make([]uint8, len(recs)) // 0 unknown, 1 placed, 2 not placed
	for _, l := range leaves {
		r := recs[l.rec]
		norm := normCity(r.city)
		if norm == "" {
			continue
		}
		if state[l.rec] == 0 {
			state[l.rec] = 2
			if pl, ok := index.place(r.code, r.city); ok {
				state[l.rec] = 1
				keys[l.rec] = placeKey(pl)
			}
		}
		if state[l.rec] != 1 {
			continue
		}
		g := ev.countries[r.code]
		if g == nil {
			g = newGroup()
			ev.countries[r.code] = g
		}
		g.add(keys[l.rec], norm, r.city, uint64(l.hi)-uint64(l.lo)+1)
		if isGuessSource(r.source) {
			continue
		}
		overlaps(segs, l.lo, l.hi, func(seg asnSeg, a, b uint32) {
			key := groupKey{seg.asn, r.code}
			g := ev.groups[key]
			if g == nil {
				g = newGroup()
				ev.groups[key] = g
			}
			g.add(keys[l.rec], norm, r.city, uint64(b)-uint64(a)+1)
		})
	}
	return ev
}

type disk struct{ lat, lon, km float64 }

func insideAll(disks []disk, lat, lon float64) bool {
	for _, d := range disks {
		if haversineKm(d.lat, d.lon, lat, lon) > d.km {
			return false
		}
	}
	return true
}

// pickCity chooses the replacement among the cities of g that lie inside every disk: with
// byCount the one with most addresses (B), else the most populous (C). Ties: smaller name.
// It returns the written spelling.
func pickCity(g *asnGroup, code string, index geoNames, disks []disk, byCount bool) (string, bool) {
	var best *cityCount
	var bestPop int64
	var bestSpell string
	for _, c := range g.cities {
		spell := c.spelling()
		pl, ok := index.place(code, spell)
		if !ok || !insideAll(disks, pl.lat, pl.lon) {
			continue
		}
		var better bool
		switch {
		case best == nil:
			better = true
		case byCount:
			better = c.n > best.n || c.n == best.n && c.name < best.name
		default:
			better = pl.pop > bestPop || pl.pop == bestPop && c.name < best.name
		}
		if better {
			best, bestPop, bestSpell = c, pl.pop, spell
		}
	}
	return bestSpell, best != nil
}

func segASN(segs []asnSeg, x uint32) (uint32, bool) {
	i := sort.Search(len(segs), func(i int) bool { return segs[i].hi >= x })
	if i < len(segs) && segs[i].lo <= x {
		return segs[i].asn, true
	}
	return 0, false
}

// applyRTTCity runs the check on the tree. segs are the ASN segments fillGuesses read (nil
// without -asn-main). It returns the counts and the stats of the two sources.
func applyRTTCity(tree *mmdbwriter.Tree, path string, segs []asnSeg, index geoNames, debug bool, recordFor func(place, *geoLayerStats) int, records func(int) mmdbtype.Map) (rttCityCounts, []geoLayerStats, error) {
	var counts rttCityCounts
	stNet, stCountry := geoLayerStats{name: rttNetworkSource}, geoLayerStats{name: rttCountrySource}
	rows, sites, skipped, err := readRTT(path, index)
	counts.rows, counts.skipped = len(rows), skipped
	if err != nil {
		return counts, nil, err
	}
	leaves, recs := snapshotCities(tree)
	ev := buildCityEvidence(leaves, recs, segs, index)
	type decision struct {
		city string // "" keeps the record
		net  bool
	}
	for lo := 0; lo < len(rows); {
		hi := lo
		for hi < len(rows) && rows[hi].client == rows[lo].client {
			hi++
		}
		group := rows[lo:hi]
		lo = hi
		counts.clients++
		prefix := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(group[0].client >> 24), byte(group[0].client >> 16), byte(group[0].client >> 8), 0}), 24)
		disks := make([]disk, len(group))
		for i, r := range group {
			p := sites[r.site].place
			disks[i] = disk{p.lat, p.lon, float64(r.rtt)*rttCityKmPerMs + rttCityMarginKm}
		}
		asn, hasASN := segASN(segs, group[0].client)
		decided := map[cityRec]decision{}
		fn := func(existing mmdbtype.DataType) (mmdbtype.DataType, error) {
			m, ok := existing.(mmdbtype.Map)
			if !ok {
				return existing, nil
			}
			code, _ := m["country_code"].(mmdbtype.String)
			city, _ := m["city_name"].(mmdbtype.String)
			src, _ := m["city_source"].(mmdbtype.String)
			lat, ok1 := m["latitude"].(mmdbtype.Float64)
			lon, ok2 := m["longitude"].(mmdbtype.Float64)
			if code == "" || city == "" || !ok1 || !ok2 || string(src) == rttCityKeep ||
				math.IsNaN(float64(lat)) || math.IsNaN(float64(lon)) || insideAll(disks, float64(lat), float64(lon)) {
				return existing, nil
			}
			counts.impossible++
			key := cityRec{string(code), string(city), string(src)}
			d, done := decided[key]
			if !done {
				if hasASN {
					if g := ev.groups[groupKey{asn, key.code}]; g != nil {
						if c, ok := pickCity(g, key.code, index, disks, true); ok {
							d = decision{c, true}
						}
					}
				}
				if d.city == "" {
					if g := ev.countries[key.code]; g != nil {
						if c, ok := pickCity(g, key.code, index, disks, false); ok {
							d = decision{city: c}
						}
					}
				}
				decided[key] = d
			}
			if d.city == "" {
				counts.kept++
				return existing, nil
			}
			st, source := &stCountry, rttCountrySource
			if d.net {
				counts.byNet++
				st, source = &stNet, rttNetworkSource
			} else {
				counts.byCountry++
			}
			st.rows++
			st.prefixes++
			record := records(recordFor(place{code: key.code, city: d.city, source: source}, st))
			if debug {
				withNet := mmdbtype.Map{"network": mmdbtype.String(prefix.String())}
				for k, v := range record {
					withNet[k] = v
				}
				record = withNet
			}
			return record, nil
		}
		if err := tree.InsertFunc(ipNet(prefix), inserter.Func(fn)); err != nil {
			return counts, nil, fmt.Errorf("insert %s (rtt-city): %w", prefix, err)
		}
	}
	return counts, []geoLayerStats{stNet, stCountry}, nil
}
