package geofeeds

import (
	_ "embed"
	"strings"
)

// A text such as "descr: DALSTON" on a Virgin Media inetnum means Dalston in London,
// but GeoNames cities1000 has no districts (readPlaces skips PPLX), so the text layer
// named Dalston in Cumbria, a village of 1,285 people 400 km away. Measured on
// 2026-11-21: when the place a text names has fewer than districtMaxPop people and its name
// is also a district name near a city of districtCityPop people or more of the same country,
// the stronger layers agree with the village 14.5% of the time (44% for other small
// places). So such a match is dropped (dropDistrict), unless the place itself lies in a big city
// (the village of Bromley inside London is a place the text may well mean).
const (
	districtMaxPop  = 15_000  // only a place with fewer people can be a district's namesake
	districtCityPop = 100_000 // a big city: this many people
	districtKm      = 30.0    // a place within this distance of a big city is inside it
)

// dropDistrict is the reason an object is dropped when its text names a small place that is
// most likely a district of a big city. Counted with the other reasons of textReader.
const dropDistrict = "district_name"

// whois-districts.txt lists the names of city districts (GeoNames PPLX near a big city) that
// clash with the name of a small place of the same country. tools/whoisdistricts makes it
// (see its README); a line is a country code, a tab and a GeoNames name.
//
//go:embed whois-districts.txt
var whoisDistrictsList string

// districtEntries returns the country code and name of every line of the list. A line
// that is not "CC<TAB>name" with an upper-case two-letter code and a name is a
// programming error: ok is false (TestWhoisDistrictsList fails on it).
func districtEntries(text string) (entries [][2]string, ok bool) {
	ok = true
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cc, name, found := strings.Cut(line, "\t")
		if !found || len(cc) != 2 || cc[0] < 'A' || cc[0] > 'Z' || cc[1] < 'A' || cc[1] > 'Z' || strings.TrimSpace(name) == "" {
			ok = false
			continue
		}
		entries = append(entries, [2]string{cc, name})
	}
	return entries, ok
}

// districtSet holds the entries as keys of the name index: the country, "|" and the
// normalised words. A name the index would not hold (another script, a dropped letter,
// too many words) gives no key: no place could have been matched under it.
var districtSet = func() map[string]bool {
	out := map[string]bool{}
	entries, _ := districtEntries(whoisDistrictsList)
	for _, e := range entries {
		words, other, dropped := normText(e[1])
		if other || dropped || len(words) == 0 || len(words) > maxTextWords {
			continue
		}
		out[e[0]+"|"+strings.Join(words, " ")] = true
	}
	return out
}()

// isDistrictName says whether a text match (the place id and the key of the matched
// word group, "CC|words") is dropped: the place is small, not inside a big city, and the
// matched name is on the district list.
func (x *nameIndex) isDistrictName(id int32, key string) bool {
	p := x.places[id]
	return p.pop < districtMaxPop && !p.inCity && districtSet[key]
}

// markCities sets inCity of every place of fewer than districtMaxPop people: true when a
// place of districtCityPop people or more of the same country lies within districtKm.
// coords[i] and ccs[i] are the point and country of place i. Called once, after all places are loaded.
func (x *nameIndex) markCities(index placeIndex, coords [][2]float64, ccs []string) {
	for i := range x.places {
		if x.places[i].pop >= districtMaxPop {
			continue
		}
		if p, ok := index.biggestWithin(ccs[i], coords[i][0], coords[i][1], districtKm); ok && p.pop >= districtCityPop {
			x.places[i].inCity = true
		}
	}
}
