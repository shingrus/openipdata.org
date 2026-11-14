package main

// mmdbbuilder eval: scores a built ip2geo MMDB for completeness and
// correctness and prints the scores as one JSON object. IPv4 only.
//
// Address-weighted scores walk the IPv4 networks of two databases together
// (both sorted, so one pass), per-probe and per-prefix scores look addresses
// up. A reference that is not given leaves its scores out of the output.
//
// The reference databases (DB-IP, MaxMind GeoLite2 City) are scored like ours:
// coverage of the RIR space and the RIPE Atlas probes (country, and city by
// distance), each looked up in the reference itself. The holdout is scored
// for ours only: vendors ingest the same geofeeds, so it is not held out from them.

import (
	"bufio"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

// evalInputs are the files "eval" reads. Only db and rir are required.
type evalInputs struct {
	db           string // the MMDB to score
	rir          string // RIR layer: the IPv4 space that should have a country
	dbip         string // DB-IP city MMDB
	maxmind      string // MaxMind GeoLite2 City MMDB
	atlas        string // truth CSV ip,country,lat,lon
	geonames     string // GeoNames cities1000.txt: places our city names for the Atlas city score
	holdoutDB    string // MMDB built without the held-out geofeeds
	holdoutTruth string // truth CSV prefix,country,city of the held-out rows
}

// evalMetrics is the JSON printed by "eval"; a score whose reference is
// missing (or shares no addresses with us) is nil and left out.
type evalMetrics struct {
	CountryPct *float64 `json:"country_pct,omitempty"`
	CityPct    *float64 `json:"city_pct,omitempty"`

	// How often our place equals each reference's, over the addresses both have.
	DBIPCountryAgreePct    *float64 `json:"dbip_country_agree_pct,omitempty"`
	DBIPCityAgreePct       *float64 `json:"dbip_city_agree_pct,omitempty"`
	MaxMindCountryAgreePct *float64 `json:"maxmind_country_agree_pct,omitempty"`
	MaxMindCityAgreePct    *float64 `json:"maxmind_city_agree_pct,omitempty"`
	// Where a strict majority of the references (at least two with a country) agree, how often we do.
	VendorConsensusCountryAgreePct *float64 `json:"vendor_consensus_country_agree_pct,omitempty"`

	AtlasProbes     *int     `json:"atlas_probes,omitempty"`
	AtlasCountryPct *float64 `json:"atlas_country_pct,omitempty"`

	// Atlas city score: the probes with coordinates whose database names a city
	// within atlasCityKm of the probe. For ours the city is placed with GeoNames
	// (-geonames), and AtlasCityUnresolved counts the probes where we named a
	// city that GeoNames could not place.
	AtlasCityProbes     *int     `json:"atlas_city_probes,omitempty"`
	AtlasCityPct        *float64 `json:"atlas_city_pct,omitempty"`
	AtlasCityUnresolved *int     `json:"atlas_city_unresolved,omitempty"`
	// Coverage versus accuracy: the probes where the database named a city with
	// coordinates, and the share of those within atlasCityKm.
	AtlasCityAnswered     *int     `json:"atlas_city_answered,omitempty"`
	AtlasCityPrecisionPct *float64 `json:"atlas_city_precision_pct,omitempty"`

	HoldoutPrefixes   *int     `json:"holdout_prefixes,omitempty"`
	HoldoutCountryPct *float64 `json:"holdout_country_pct,omitempty"`
	HoldoutCityPct    *float64 `json:"holdout_city_pct,omitempty"`

	// The reference databases scored like ours.
	DBIPCountryPct      *float64 `json:"dbip_country_pct,omitempty"`
	DBIPCityPct         *float64 `json:"dbip_city_pct,omitempty"`
	DBIPAtlasCountryPct *float64 `json:"dbip_atlas_country_pct,omitempty"`
	DBIPAtlasCityPct    *float64 `json:"dbip_atlas_city_pct,omitempty"`

	DBIPAtlasCityAnswered     *int     `json:"dbip_atlas_city_answered,omitempty"`
	DBIPAtlasCityPrecisionPct *float64 `json:"dbip_atlas_city_precision_pct,omitempty"`

	MaxMindCountryPct      *float64 `json:"maxmind_country_pct,omitempty"`
	MaxMindCityPct         *float64 `json:"maxmind_city_pct,omitempty"`
	MaxMindAtlasCountryPct *float64 `json:"maxmind_atlas_country_pct,omitempty"`
	MaxMindAtlasCityPct    *float64 `json:"maxmind_atlas_city_pct,omitempty"`

	MaxMindAtlasCityAnswered     *int     `json:"maxmind_atlas_city_answered,omitempty"`
	MaxMindAtlasCityPrecisionPct *float64 `json:"maxmind_atlas_city_precision_pct,omitempty"`
}

// refSlots are the places in evalMetrics where the scores of one reference go.
type refSlots struct {
	countryAgree, cityAgree, country, city, atlas, atlasCity **float64
	atlasCityAnswered                                        **int
	atlasCityPrecisionPct                                    **float64
}

func (m *evalMetrics) slots(key string) refSlots {
	switch key {
	case "dbip":
		return refSlots{&m.DBIPCountryAgreePct, &m.DBIPCityAgreePct, &m.DBIPCountryPct, &m.DBIPCityPct, &m.DBIPAtlasCountryPct, &m.DBIPAtlasCityPct, &m.DBIPAtlasCityAnswered, &m.DBIPAtlasCityPrecisionPct}
	case "maxmind":
		return refSlots{&m.MaxMindCountryAgreePct, &m.MaxMindCityAgreePct, &m.MaxMindCountryPct, &m.MaxMindCityPct, &m.MaxMindAtlasCountryPct, &m.MaxMindAtlasCityPct, &m.MaxMindAtlasCityAnswered, &m.MaxMindAtlasCityPrecisionPct}
	}
	panic("eval: unknown reference " + key)
}

// rng is a block of IPv4 addresses lo..hi with its country and city, as ids
// from one names table (0 is "none"). The ranges of a list are sorted and
// do not overlap.
type rng struct {
	lo, hi        uint32
	country, city uint32
}

// names gives every distinct string an id, so countries and cities of
// different databases compare as integers.
type names map[string]uint32

// reverse is the table the other way round: reverse()[id] is the string.
func (n names) reverse() []string {
	out := make([]string, len(n)+1)
	for s, id := range n {
		out[id] = s
	}
	return out
}

func (n names) id(s string) uint32 {
	if s == "" {
		return 0
	}
	id, ok := n[s]
	if !ok {
		id = uint32(len(n)) + 1
		n[s] = id
	}
	return id
}

// normCountry returns an upper-case two-letter code, or "".
func normCountry(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 2 {
		return ""
	}
	return code
}

// normCity is the form in which city names are compared: trimmed, lower case.
func normCity(city string) string { return strings.ToLower(strings.TrimSpace(city)) }

// Each database keeps its country and city in its own fields.
type placeFields func(res maxminddb.Result) (country, city string, err error)

// ourFields: country_code and city_name, as written by "mmdbbuilder geo".
func ourFields(res maxminddb.Result) (string, string, error) {
	var r struct {
		Country string `maxminddb:"country_code"`
		City    string `maxminddb:"city_name"`
	}
	err := res.Decode(&r)
	return r.Country, r.City, err
}

// geoipFields: country.iso_code and city.names.en, as in DB-IP and GeoLite2.
func geoipFields(res maxminddb.Result) (string, string, error) {
	var r struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
		City struct {
			Names struct {
				EN string `maxminddb:"en"`
			} `maxminddb:"names"`
		} `maxminddb:"city"`
	}
	err := res.Decode(&r)
	return r.Country.ISOCode, r.City.Names.EN, err
}

var allIPv4 = netip.MustParsePrefix("0.0.0.0/0")

// prefixBounds returns the first and last address of an IPv4 prefix.
func prefixBounds(p netip.Prefix) (lo, hi uint32) {
	a := p.Addr().As4()
	lo = binary.BigEndian.Uint32(a[:])
	hi = uint32(uint64(lo) + 1<<(32-p.Bits()) - 1)
	return lo, hi
}

func addrNumber(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

// loadRanges reads the IPv4 networks of an MMDB that have a country or a
// city. Neighbours with the same place are merged.
func loadRanges(reader *maxminddb.Reader, ids names, fields placeFields) ([]rng, error) {
	var out []rng
	for res := range reader.NetworksWithin(allIPv4) {
		if err := res.Err(); err != nil {
			return nil, err
		}
		p := res.Prefix()
		if a := p.Addr().Unmap(); !a.Is4() {
			continue
		} else if a != p.Addr() {
			p = netip.PrefixFrom(a, p.Bits()-96)
		}
		country, city, err := fields(res)
		if err != nil {
			return nil, err
		}
		r := rng{country: ids.id(normCountry(country)), city: ids.id(normCity(city))}
		if r.country == 0 && r.city == 0 {
			continue
		}
		r.lo, r.hi = prefixBounds(p)
		if n := len(out); n > 0 && out[n-1].hi+1 == r.lo && out[n-1].country == r.country && out[n-1].city == r.city && out[n-1].hi != math.MaxUint32 {
			out[n-1].hi = r.hi
			continue
		}
		out = append(out, r)
	}
	if !slices.IsSortedFunc(out, func(a, b rng) int { return cmpU32(a.lo, b.lo) }) {
		slices.SortFunc(out, func(a, b rng) int { return cmpU32(a.lo, b.lo) })
	}
	return out, nil
}

func cmpU32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// loadSpace returns the IPv4 space of a layer file as sorted, merged ranges.
func loadSpace(path string) ([]rng, error) {
	var out []rng
	err := readLayer(path, func(_ int, prefix, _, _ string) {
		p, err := parseGeoPrefix(prefix)
		if err != nil || !p.Addr().Is4() {
			return
		}
		lo, hi := prefixBounds(p)
		out = append(out, rng{lo: lo, hi: hi})
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b rng) int { return cmpU32(a.lo, b.lo) })
	merged := out[:0]
	for _, r := range out {
		if n := len(merged); n > 0 && uint64(r.lo) <= uint64(merged[n-1].hi)+1 {
			merged[n-1].hi = max(merged[n-1].hi, r.hi)
			continue
		}
		merged = append(merged, r)
	}
	return merged, nil
}

// join walks two sorted lists of ranges together and calls fn for each run of
// addresses that lies in the same range of each list (x and y are nil where a
// list has no range). Runs where neither list has a range are not reported.
func join(a, b []rng, fn func(lo, hi uint32, x, y *rng)) {
	i, j := 0, 0
	var pos uint64 // the first address not reported yet
	for i < len(a) || j < len(b) {
		var x, y *rng
		if i < len(a) {
			x = &a[i]
		}
		if j < len(b) {
			y = &b[j]
		}
		next := uint64(math.MaxUint32) + 1 // the nearest start of a range not yet reached
		if x != nil {
			next = min(next, uint64(x.lo))
		}
		if y != nil {
			next = min(next, uint64(y.lo))
		}
		lo := max(pos, next)
		if x != nil && uint64(x.lo) > lo {
			x = nil
		}
		if y != nil && uint64(y.lo) > lo {
			y = nil
		}
		// The run ends where an active range ends or an inactive one starts.
		hi := uint64(math.MaxUint32)
		for _, r := range []*rng{x, y} {
			if r != nil {
				hi = min(hi, uint64(r.hi))
			}
		}
		if x == nil && i < len(a) {
			hi = min(hi, uint64(a[i].lo)-1)
		}
		if y == nil && j < len(b) {
			hi = min(hi, uint64(b[j].lo)-1)
		}
		fn(uint32(lo), uint32(hi), x, y)
		pos = hi + 1
		if x != nil && uint64(x.hi) == hi {
			i++
		}
		if y != nil && uint64(y.hi) == hi {
			j++
		}
	}
}

// sweep walks any number of sorted lists of ranges together and calls fn for
// each run of addresses that lies in the same range (or none) of every list;
// active[k] is the range of list k, nil where it has none. Runs where no list
// has a range are not reported. active is reused between calls.
func sweep(lists [][]rng, fn func(lo, hi uint32, active []*rng)) {
	idx := make([]int, len(lists))
	active := make([]*rng, len(lists))
	var pos uint64 // the first address not reported yet
	for {
		start := uint64(math.MaxUint32) + 1
		for k, l := range lists {
			for idx[k] < len(l) && uint64(l[idx[k]].hi) < pos {
				idx[k]++
			}
			if idx[k] < len(l) {
				start = min(start, max(pos, uint64(l[idx[k]].lo)))
			}
		}
		if start > math.MaxUint32 {
			return
		}
		end := uint64(math.MaxUint32)
		for k, l := range lists {
			active[k] = nil
			if idx[k] >= len(l) {
				continue
			}
			r := &l[idx[k]]
			if uint64(r.lo) <= start {
				active[k] = r
				end = min(end, uint64(r.hi))
			} else {
				end = min(end, uint64(r.lo)-1)
			}
		}
		fn(uint32(start), uint32(end), active)
		pos = end + 1
	}
}

// ratio is 100*part/total rounded to two decimals; nil when total is 0, so a
// score without data is left out instead of reading as 0.
func ratio(part, total uint64) *float64 {
	if total == 0 {
		return nil
	}
	v := math.Round(float64(part)/float64(total)*10000) / 100
	return &v
}

func count(n int) *int { return &n }

// openRanges opens an MMDB and loads its IPv4 ranges.
func openRanges(path string, ids names, fields placeFields) ([]rng, error) {
	reader, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	ranges, err := loadRanges(reader, ids, fields)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ranges, nil
}

// reference is one reference database loaded as ranges.
type reference struct {
	key    string
	path   string
	ranges []rng
}

// agreement is how often ours has the same country (and, where both have one,
// the same city) as the reference, over the addresses where both have a country.
func agreement(ours, ref []rng) (country, city *float64) {
	var both, same, cityBoth, citySame uint64
	join(ours, ref, func(lo, hi uint32, x, y *rng) {
		if x == nil || y == nil || x.country == 0 || y.country == 0 {
			return
		}
		n := uint64(hi) - uint64(lo) + 1
		both += n
		if x.country != y.country {
			return
		}
		same += n
		if x.city != 0 && y.city != 0 {
			cityBoth += n
			if x.city == y.city {
				citySame += n
			}
		}
	})
	return ratio(same, both), ratio(citySame, cityBoth)
}

// coverage is the share of the delegated space that has a country, and a city.
func coverage(db, space []rng, total uint64) (country, city *float64) {
	var withCountry, withCity uint64
	join(db, space, func(lo, hi uint32, x, y *rng) {
		if x == nil || y == nil {
			return
		}
		n := uint64(hi) - uint64(lo) + 1
		if x.country != 0 {
			withCountry += n
		}
		if x.city != 0 {
			withCity += n
		}
	})
	return ratio(withCountry, total), ratio(withCity, total)
}

// consensus returns the ranges where at least two of the references have a
// country and a strict majority of those agree, with that country.
func consensus(refs []reference) []rng {
	lists := make([][]rng, len(refs))
	for i, r := range refs {
		lists[i] = r.ranges
	}
	var out []rng
	votes := map[uint32]int{}
	sweep(lists, func(lo, hi uint32, active []*rng) {
		clear(votes)
		n := 0
		for _, r := range active {
			if r != nil && r.country != 0 {
				votes[r.country]++
				n++
			}
		}
		if n < 2 {
			return
		}
		for country, v := range votes {
			if v*2 > n {
				out = append(out, rng{lo: lo, hi: hi, country: country})
			}
		}
	})
	return out
}

// evaluate computes the scores for the inputs that are set.
func evaluate(in evalInputs) (*evalMetrics, error) {
	if in.db == "" || in.rir == "" {
		return nil, errors.New("eval needs -db and -rir")
	}
	if (in.holdoutDB == "") != (in.holdoutTruth == "") {
		return nil, errors.New("eval needs both -holdout-db and -holdout-truth")
	}
	ids := names{}
	space, err := loadSpace(in.rir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", in.rir, err)
	}
	var total uint64
	for _, r := range space {
		total += uint64(r.hi) - uint64(r.lo) + 1
	}
	if total == 0 {
		return nil, fmt.Errorf("%s: no IPv4 space", in.rir)
	}
	ours, err := openRanges(in.db, ids, ourFields)
	if err != nil {
		return nil, err
	}
	m := &evalMetrics{}

	// Completeness: how much of the delegated space has a country or a city.
	m.CountryPct, m.CityPct = coverage(ours, space, total)

	var refs []reference
	if in.dbip != "" {
		ranges, err := openRanges(in.dbip, ids, geoipFields)
		if err != nil {
			return nil, err
		}
		refs = append(refs, reference{"dbip", in.dbip, ranges})
	}
	if in.maxmind != "" {
		ranges, err := openRanges(in.maxmind, ids, geoipFields)
		if err != nil {
			return nil, err
		}
		refs = append(refs, reference{"maxmind", in.maxmind, ranges})
	}
	for _, ref := range refs {
		slots := m.slots(ref.key)
		*slots.countryAgree, *slots.cityAgree = agreement(ours, ref.ranges)
		*slots.country, *slots.city = coverage(ref.ranges, space, total)
	}
	if len(refs) >= 2 {
		// Where most of the references agree with each other, do we agree too?
		var both, same uint64
		join(ours, consensus(refs), func(lo, hi uint32, x, y *rng) {
			if x == nil || y == nil || x.country == 0 {
				return
			}
			n := uint64(hi) - uint64(lo) + 1
			both += n
			if x.country == y.country {
				same += n
			}
		})
		m.VendorConsensusCountryAgreePct = ratio(same, both)
	}

	// Lookups of ours and of each reference, for the probes and the holdout rows
	// (the holdout only for ours).
	reverse := ids.reverse()
	if in.atlas != "" {
		reader, err := maxminddb.Open(in.db)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		probes, ok, err := scoreProbes(mmdbLookup(reader), in.atlas)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", in.atlas, err)
		}
		if probes > 0 {
			m.AtlasProbes, m.AtlasCountryPct = count(probes), ratio(uint64(ok), uint64(probes))
			for _, ref := range refs {
				_, refOK, err := scoreProbes(rangeLookup(ref.ranges, reverse), in.atlas)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", in.atlas, err)
				}
				*m.slots(ref.key).atlas = ratio(uint64(refOK), uint64(probes))
			}
		}
		if err := scoreAtlasCities(m, in, reader, refs); err != nil {
			return nil, err
		}
	}
	if in.holdoutDB != "" {
		reader, err := maxminddb.Open(in.holdoutDB)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		s, err := scoreHoldout(mmdbLookup(reader), in.holdoutTruth)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", in.holdoutTruth, err)
		}
		if s.rows > 0 {
			m.HoldoutPrefixes = count(s.rows)
			m.HoldoutCountryPct = ratio(uint64(s.countryOK), uint64(s.rows))
			m.HoldoutCityPct = ratio(uint64(s.cityOK), uint64(s.cityRows))
		}
	}
	return m, nil
}

// readTruth calls fn with the named columns of each row of a truth CSV.
func readTruth(path string, columns []string, fn func(fields []string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	pos := make([]int, len(columns))
	for i, name := range columns {
		pos[i] = slices.Index(header, name)
		if pos[i] < 0 {
			return fmt.Errorf("header %q lacks %s", strings.Join(header, ","), name)
		}
	}
	fields := make([]string, len(columns))
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			continue // a row that does not parse is not a truth row
		}
		ok := true
		for i, p := range pos {
			if p >= len(rec) {
				ok = false
				break
			}
			fields[i] = rec[p]
		}
		if ok {
			fn(fields)
		}
	}
}

// lookupFunc returns the normalized country and city of one IPv4 address.
type lookupFunc func(netip.Addr) (country, city string, err error)

// mmdbLookup looks addresses up in one of our MMDBs (country_code, city_name).
func mmdbLookup(reader *maxminddb.Reader) lookupFunc {
	return func(addr netip.Addr) (string, string, error) {
		res := reader.Lookup(addr)
		if !res.Found() {
			return "", "", res.Err()
		}
		country, city, err := ourFields(res)
		return normCountry(country), normCity(city), err
	}
}

// rangeLookup looks addresses up in sorted ranges (a reference database);
// reverse turns their ids back into strings.
func rangeLookup(ranges []rng, reverse []string) lookupFunc {
	return func(addr netip.Addr) (string, string, error) {
		n := addrNumber(addr)
		i, found := slices.BinarySearchFunc(ranges, n, func(r rng, n uint32) int {
			switch {
			case r.hi < n:
				return -1
			case r.lo > n:
				return 1
			}
			return 0
		})
		if !found {
			return "", "", nil
		}
		return reverse[ranges[i].country], reverse[ranges[i].city], nil
	}
}

// scoreProbes counts the probes (ip,country,lat,lon) and those whose country the database has right.
func scoreProbes(lookup lookupFunc, path string) (probes, ok int, err error) {
	var lookupErr error
	err = readTruth(path, []string{"ip", "country"}, func(f []string) {
		addr, perr := netip.ParseAddr(strings.TrimSpace(f[0]))
		want := normCountry(f[1])
		if perr != nil || !addr.Unmap().Is4() || want == "" {
			return
		}
		got, _, lerr := lookup(addr.Unmap())
		if lerr != nil {
			lookupErr = lerr
			return
		}
		probes++
		if got == want {
			ok++
		}
	})
	return probes, ok, errors.Join(err, lookupErr)
}

type holdoutScore struct {
	rows, countryOK  int // every held-out prefix
	cityRows, cityOK int // those with a city
}

// scoreHoldout looks up the first address of each held-out prefix
// (prefix,country,city) in a database.
func scoreHoldout(lookup lookupFunc, path string) (holdoutScore, error) {
	var s holdoutScore
	var lookupErr error
	err := readTruth(path, []string{"prefix", "country", "city"}, func(f []string) {
		p, perr := netip.ParsePrefix(strings.TrimSpace(f[0]))
		want, wantCity := normCountry(f[1]), normCity(f[2])
		if perr != nil || !p.Addr().Is4() || want == "" {
			return
		}
		got, gotCity, lerr := lookup(p.Masked().Addr())
		if lerr != nil {
			lookupErr = lerr
			return
		}
		s.rows++
		if got == want {
			s.countryOK++
		}
		if wantCity != "" {
			s.cityRows++
			if gotCity == wantCity {
				s.cityOK++
			}
		}
	})
	return s, errors.Join(err, lookupErr)
}

// atlasCityKm is how close a database's city must be to the probe to count.
const atlasCityKm = 50

// haversineKm is the great-circle distance between two points in kilometres
// (Earth radius 6371 km).
func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const rad = math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * 6371 * math.Asin(math.Min(1, math.Sqrt(a)))
}

// cityProbe is an Atlas probe with coordinates.
type cityProbe struct {
	addr     netip.Addr
	lat, lon float64
}

// readCityProbes returns the probes of the CSV (ip,country,lat,lon) that have
// an IPv4 address and valid coordinates.
func readCityProbes(path string) ([]cityProbe, error) {
	var out []cityProbe
	err := readTruth(path, []string{"ip", "lat", "lon"}, func(f []string) {
		addr, err := netip.ParseAddr(strings.TrimSpace(f[0]))
		lat, latErr := strconv.ParseFloat(strings.TrimSpace(f[1]), 64)
		lon, lonErr := strconv.ParseFloat(strings.TrimSpace(f[2]), 64)
		if err != nil || !addr.Unmap().Is4() || latErr != nil || lonErr != nil ||
			math.Abs(lat) > 90 || math.Abs(lon) > 180 {
			return
		}
		out = append(out, cityProbe{addr.Unmap(), lat, lon})
	})
	return out, err
}

// How a database answered for a probe.
type cityAnswer int

const (
	noCity     cityAnswer = iota // no answer, or no city
	unplaced                     // a city, but its coordinates are unknown
	placedCity                   // a city with coordinates
)

// cityPlacer returns the city of an address with its coordinates.
type cityPlacer func(netip.Addr) (lat, lon float64, answer cityAnswer, err error)

// scoreCities counts the probes whose database city lies within atlasCityKm,
// those where a city was named but could not be placed, and those answered
// with a city that has coordinates.
func scoreCities(probes []cityProbe, place cityPlacer) (ok, unresolved, answered int, err error) {
	for _, p := range probes {
		lat, lon, answer, err := place(p.addr)
		if err != nil {
			return 0, 0, 0, err
		}
		switch answer {
		case unplaced:
			unresolved++
		case placedCity:
			answered++
			if haversineKm(p.lat, p.lon, lat, lon) <= atlasCityKm {
				ok++
			}
		}
	}
	return ok, unresolved, answered, nil
}

// scoreAtlasCities sets the Atlas city scores: ours (placed with GeoNames) and
// each reference (its own coordinates).
func scoreAtlasCities(m *evalMetrics, in evalInputs, ours *maxminddb.Reader, refs []reference) error {
	probes, err := readCityProbes(in.atlas)
	if err != nil {
		return fmt.Errorf("%s: %w", in.atlas, err)
	}
	if len(probes) == 0 {
		return nil
	}
	total := uint64(len(probes))
	m.AtlasCityProbes = count(len(probes))
	if in.geonames != "" {
		index, err := loadGeoNames(in.geonames)
		if err != nil {
			return fmt.Errorf("%s: %w", in.geonames, err)
		}
		ok, unresolved, answered, err := scoreCities(probes, geoNamesPlacer(ours, index))
		if err != nil {
			return err
		}
		m.AtlasCityPct, m.AtlasCityUnresolved = ratio(uint64(ok), total), count(unresolved)
		m.AtlasCityAnswered, m.AtlasCityPrecisionPct = count(answered), ratio(uint64(ok), uint64(answered))
	}
	for _, ref := range refs {
		reader, err := maxminddb.Open(ref.path)
		if err != nil {
			return err
		}
		ok, _, answered, err := scoreCities(probes, referencePlacer(reader))
		reader.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", ref.path, err)
		}
		slots := m.slots(ref.key)
		*slots.atlasCity = ratio(uint64(ok), total)
		*slots.atlasCityAnswered, *slots.atlasCityPrecisionPct = count(answered), ratio(uint64(ok), uint64(answered))
	}
	return nil
}

// gnPlace is a GeoNames place.
type gnPlace struct {
	lat, lon float64
	pop      int64
	primary  bool // the key is the place's own name or ascii name, not an alternate
}

// geoNames maps country code + normalized name to the most populous place of that name.
type geoNames map[string]gnPlace

func geoNamesKey(country, city string) string { return country + "|" + normCity(city) }

// loadGeoNames reads a GeoNames dump (cities1000.txt, tab-separated) and
// indexes every place by country and by its name, ascii name and each
// alternate name. On a collision the most populous place wins (the first on a tie).
func loadGeoNames(path string) (geoNames, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	index := geoNames{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 15 {
			continue
		}
		lat, latErr := strconv.ParseFloat(fields[4], 64)
		lon, lonErr := strconv.ParseFloat(fields[5], 64)
		country := normCountry(fields[8])
		if latErr != nil || lonErr != nil || country == "" {
			continue
		}
		pop, _ := strconv.ParseInt(fields[14], 10, 64)
		set := func(name string, primary bool) {
			key := geoNamesKey(country, name)
			old, ok := index[key]
			// A primary name beats any alternate; otherwise the more populous place wins.
			if !ok || (primary && !old.primary) || (primary == old.primary && pop > old.pop) {
				index[key] = gnPlace{lat, lon, pop, primary}
			}
		}
		for _, name := range fields[1:3] {
			if normCity(name) != "" {
				set(name, true)
			}
		}
		for _, name := range strings.Split(fields[3], ",") {
			if hasLatin(normCity(name)) {
				set(name, false)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(index) == 0 {
		return nil, errors.New("no places")
	}
	return index, nil
}

// geoNamesPlacer looks addresses up in our MMDB (country_code, city_name) and
// places the city with GeoNames.
func geoNamesPlacer(reader *maxminddb.Reader, index geoNames) cityPlacer {
	return func(addr netip.Addr) (float64, float64, cityAnswer, error) {
		res := reader.Lookup(addr)
		if !res.Found() {
			return 0, 0, noCity, res.Err()
		}
		country, city, err := ourFields(res)
		country, city = normCountry(country), normCity(city)
		if err != nil || city == "" {
			return 0, 0, noCity, err
		}
		p, ok := index[geoNamesKey(country, city)]
		if !ok {
			return 0, 0, unplaced, nil
		}
		return p.lat, p.lon, placedCity, nil
	}
}

// referencePlacer looks addresses up in a DB-IP or GeoLite2 City MMDB and uses
// its own coordinates, but only for a record with a city: without one they are
// a country's centroid.
func referencePlacer(reader *maxminddb.Reader) cityPlacer {
	return func(addr netip.Addr) (float64, float64, cityAnswer, error) {
		res := reader.Lookup(addr)
		if !res.Found() {
			return 0, 0, noCity, res.Err()
		}
		var r struct {
			City struct {
				Names struct {
					EN string `maxminddb:"en"`
				} `maxminddb:"names"`
			} `maxminddb:"city"`
			Location struct {
				Latitude  *float64 `maxminddb:"latitude"`
				Longitude *float64 `maxminddb:"longitude"`
			} `maxminddb:"location"`
		}
		if err := res.Decode(&r); err != nil {
			return 0, 0, noCity, err
		}
		if normCity(r.City.Names.EN) == "" || r.Location.Latitude == nil || r.Location.Longitude == nil {
			return 0, 0, noCity, nil
		}
		return *r.Location.Latitude, *r.Location.Longitude, placedCity, nil
	}
}

// hasLatin tells whether a normalized name has a letter a-z.
func hasLatin(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= 'a' && r <= 'z' })
}
