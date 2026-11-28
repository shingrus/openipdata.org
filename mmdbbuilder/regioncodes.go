package main

// Region codes outside the US, learned from the layer rows themselves (mmdbbuilder geo
// -geonames; no flag of its own).
//
// A row of another country carries a region such as DE-SN, IT-25 or CA-ON, an ISO 3166-2 code
// that is NOT the GeoNames admin1 code (DE-SN is admin1 13), and there is no table. But the rows
// teach the mapping: a city name with exactly one place in its country has that place's admin1,
// so when most of the unambiguous cities that rows write under DE-SN lie in admin1 13, DE-SN is
// admin1 13. With the mapping, the region places a namesake by the row's own region exactly like a
// US state (geoNames.placeIn), and settles a name the full list leaves ambiguous.
//
//  1. scanLayers (fullplaces.go) reads the layer files once, with the per-row steps of the build,
//     and notes for every row with a region "CC-R" (CC the row's own country, R one to three
//     ASCII letters or digits, not the US) the city name under (CC, R).
//  2. applyRegionCodes reads cities1000.txt once more, for those names only, and keeps their
//     candidate places (id, admin1, people, coordinates, time zone). A name is unambiguous when
//     it has one candidate. A (CC, R) is learned when at least regionMinCities distinct cities
//     voted and at least regionMinSharePct percent of them agree on one admin1.
//  3. The regional places of the wanted names in the learned admin1 values go into the index under
//     the keys of the US ones ("DE:13|neustadt"), so placeOf and everything after it work as for
//     a US state.
//
// The learned mapping lives in the index itself, under keys no country code can equal and that
// contain no "|" ("DE#SN" holds the admin1 in the tz field of its value, "DE=13" marks an admin1
// as learned), so every loop that takes the country of a key with plainKeyCountry skips them,
// and no package-level state exists.

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

const (
	// regionMinCities is the number of distinct unambiguous cities that must vote for a mapping.
	regionMinCities = 5
	// regionMinSharePct is the percent of the voters that must agree on one admin1.
	regionMinSharePct = 90
	// regionMaxCodeLen is the longest region code R of "CC-R".
	regionMaxCodeLen = 3
)

// isoRegionCode returns the code R of a row's region "CC-R" for a country other than the US, in
// upper case: CC must be the row's own country and R one to regionMaxCodeLen ASCII letters or
// digits. A bare code, another country's prefix or any other shape gives "".
func isoRegionCode(country, region string) string {
	c := normCountry(country)
	if c == "" || c == "US" {
		return ""
	}
	r := strings.ToUpper(strings.TrimSpace(region))
	rest, ok := strings.CutPrefix(r, c+"-")
	if !ok || rest == "" || len(rest) > regionMaxCodeLen {
		return ""
	}
	for i := 0; i < len(rest); i++ {
		b := rest[i]
		if !(b >= 'A' && b <= 'Z' || b >= '0' && b <= '9') {
			return ""
		}
	}
	return rest
}

func learnedKey(cc, r string) string    { return cc + "#" + r }
func admin1SetKey(cc, a1 string) string { return cc + "=" + a1 }

// learnedAdmin1 is the admin1 learned for the region code r of the country cc ("" for none).
func (g geoNames) learnedAdmin1(cc, r string) string { return g[learnedKey(cc, r)].tz }

// regionUsable says whether the admin1 a1 of the country cc is one a row's region can name: any
// US state, or an admin1 some learned mapping of cc points to.
func (g geoNames) regionUsable(cc, a1 string) bool {
	if a1 == "" {
		return false
	}
	if cc == "US" {
		return true
	}
	_, ok := g[admin1SetKey(cc, a1)]
	return ok
}

// admin1Of turns a row's region into the admin1 that may change where its city is placed: the
// US rule (regionAdmin1) for the US, the learned mapping for another country, "" for none.
func (g geoNames) admin1Of(country, region string) string {
	if a := regionAdmin1(country, region); a != "" {
		return a
	}
	if r := isoRegionCode(country, region); r != "" {
		return g.learnedAdmin1(normCountry(country), r)
	}
	return ""
}

type regionCode struct{ cc, r string }

// regionScan is what the pre-pass over the layers notes about the regions: the city names of
// every (country, region code), as ids into names (the keys "CC|name" of the main index).
type regionScan struct {
	ids   map[string]int32
	names []string
	votes map[regionCode]map[int32]struct{}
}

func newRegionScan() *regionScan {
	return &regionScan{ids: map[string]int32{}, votes: map[regionCode]map[int32]struct{}{}}
}

// add notes that a row of the country cc with the region code r wrote the city key.
func (s *regionScan) add(cc, r, key string) {
	id, ok := s.ids[key]
	if !ok {
		id = int32(len(s.names))
		s.ids[key] = id
		s.names = append(s.names, key)
	}
	rc := regionCode{cc, r}
	set := s.votes[rc]
	if set == nil {
		set = map[int32]struct{}{}
		s.votes[rc] = set
	}
	set[id] = struct{}{}
}

// regionStats are the counts printed as "region-codes:".
type regionStats struct {
	learned int // mappings (country, region code) -> admin1
	seen    int // region codes seen on rows with a city
}

// regionCand is a place of cities1000.txt that carries a wanted name.
type regionCand struct {
	lat, lon float64
	pop      int64
	id       uint32
	admin1   string // field 10, upper case
	primary  bool   // the name is the place's own name or ascii name
	tz       string
}

// readRegionCands reads cities1000.txt and returns, for every wanted key ("CC|name", the main
// index's name rule), its places in file order, each place once.
func readRegionCands(path string, want func(string) bool) (map[string][]regionCand, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string][]regionCand{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var keys []string
	var prim []bool
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 15 {
			continue
		}
		lat, latErr := strconv.ParseFloat(fields[4], 64)
		lon, lonErr := strconv.ParseFloat(fields[5], 64)
		country := normCountry(fields[8])
		id64, idErr := strconv.ParseUint(fields[0], 10, 32)
		if idErr != nil || latErr != nil || lonErr != nil || country == "" {
			continue
		}
		c := regionCand{lat: lat, lon: lon, id: uint32(id64), admin1: strings.ToUpper(strings.TrimSpace(fields[10]))}
		c.pop, _ = strconv.ParseInt(fields[14], 10, 64)
		if len(fields) > 17 {
			c.tz = strings.TrimSpace(fields[17])
		}
		keys, prim = keys[:0], prim[:0]
		note := func(name string, primary bool) {
			key := geoNamesKey(country, name)
			if !want(key) {
				return
			}
			for i := range keys {
				if keys[i] == key {
					prim[i] = prim[i] || primary
					return
				}
			}
			keys, prim = append(keys, key), append(prim, primary)
		}
		for _, name := range fields[1:3] {
			if normCity(name) != "" {
				note(name, true)
			}
		}
		for _, name := range strings.Split(fields[3], ",") {
			if hasLatin(normCity(name)) {
				note(name, false)
			}
		}
		for i, key := range keys {
			cc := c
			cc.primary = prim[i]
			out[key] = append(out[key], cc)
		}
	}
	return out, sc.Err()
}

// bestInAdmin1 is the place of the candidates that lies in the admin1 a1, by the collision rule
// of the main index: a primary name beats an alternate, then more people, the first in file order.
func bestInAdmin1(list []regionCand, a1 string) (regionCand, bool) {
	var best regionCand
	found := false
	for _, c := range list {
		if c.admin1 != a1 {
			continue
		}
		if !found || c.primary && !best.primary || c.primary == best.primary && c.pop > best.pop {
			best, found = c, true
		}
	}
	return best, found
}

// learnRegions decides the mappings from the votes and records them in the index. Only the
// unambiguous cities (one candidate place in the country, with an admin1) vote, once per name.
func learnRegions(index geoNames, cands map[string][]regionCand, scan *regionScan) regionStats {
	st := regionStats{seen: len(scan.votes)}
	for rc, ids := range scan.votes {
		counts := map[string]int{}
		total := 0
		for id := range ids {
			list := cands[scan.names[id]]
			if len(list) != 1 || list[0].admin1 == "" {
				continue
			}
			counts[list[0].admin1]++
			total++
		}
		if total < regionMinCities {
			continue
		}
		top, topN := "", 0
		for a1, n := range counts {
			if n > topN || n == topN && a1 < top {
				top, topN = a1, n
			}
		}
		if topN*100 < total*regionMinSharePct {
			continue
		}
		index[learnedKey(rc.cc, rc.r)] = gnPlace{tz: top}
		index[admin1SetKey(rc.cc, top)] = gnPlace{}
		st.learned++
	}
	return st
}

// addRegionalPlaces adds, for every candidate name outside the US, its place in each admin1 that
// a row's region can name (regionUsable), under the regional key.
func addRegionalPlaces(index geoNames, cands map[string][]regionCand) {
	for key, list := range cands {
		cc := key[:2]
		if cc == "US" {
			continue // the main pass of loadGeoNamesOpts has them
		}
		done := map[string]bool{}
		for _, c := range list {
			if done[c.admin1] || !index.regionUsable(cc, c.admin1) {
				continue
			}
			done[c.admin1] = true
			if best, ok := bestInAdmin1(list, c.admin1); ok {
				index[cc+":"+c.admin1+key[2:]] = gnPlace{best.lat, best.lon, best.pop, best.primary, best.id, best.tz}
			}
		}
	}
}

// applyRegionCodes learns the region codes of the scan and adds the regional places of the
// names its rows wrote. It reads cities1000.txt once more, for those names only, and nothing
// when no row had a region code.
func applyRegionCodes(index geoNames, citiesPath string, scan *regionScan) (regionStats, error) {
	if scan == nil || len(scan.votes) == 0 {
		return regionStats{}, nil
	}
	cands, err := readRegionCands(citiesPath, func(key string) bool { _, ok := scan.ids[key]; return ok })
	if err != nil {
		return regionStats{}, err
	}
	st := learnRegions(index, cands, scan)
	addRegionalPlaces(index, cands)
	return st, nil
}
