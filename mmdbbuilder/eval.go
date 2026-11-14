package main

// mmdbbuilder eval: scores a built ip2geo MMDB for completeness and
// correctness and prints the scores as one JSON object. IPv4 only.
//
// Address-weighted scores walk the IPv4 networks of two databases together
// (both sorted, so one pass), per-probe and per-prefix scores look addresses
// up. A reference that is not given leaves its scores out of the output.
//
// The reference databases (DB-IP, MaxMind GeoLite2 City, IP2Location LITE DB11)
// are scored like ours: coverage of the RIR space, the RIPE Atlas probes and
// the holdout rows, each looked up in the reference itself.

import (
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
	ip2location  string // IP2Location LITE DB11 CSV (IPv4)
	atlas        string // truth CSV ip,country,lat,lon
	holdoutDB    string // MMDB built without the held-out geofeeds
	holdoutTruth string // truth CSV prefix,country,city of the held-out rows
}

// evalMetrics is the JSON printed by "eval"; a score whose reference is
// missing (or shares no addresses with us) is nil and left out.
type evalMetrics struct {
	CountryPct *float64 `json:"country_pct,omitempty"`
	CityPct    *float64 `json:"city_pct,omitempty"`

	// How often our place equals each reference's, over the addresses both have.
	DBIPCountryAgreePct        *float64 `json:"dbip_country_agree_pct,omitempty"`
	DBIPCityAgreePct           *float64 `json:"dbip_city_agree_pct,omitempty"`
	MaxMindCountryAgreePct     *float64 `json:"maxmind_country_agree_pct,omitempty"`
	MaxMindCityAgreePct        *float64 `json:"maxmind_city_agree_pct,omitempty"`
	IP2LocationCountryAgreePct *float64 `json:"ip2location_country_agree_pct,omitempty"`
	IP2LocationCityAgreePct    *float64 `json:"ip2location_city_agree_pct,omitempty"`
	// Where a strict majority of the references (at least two with a country) agree, how often we do.
	VendorConsensusCountryAgreePct *float64 `json:"vendor_consensus_country_agree_pct,omitempty"`

	AtlasProbes     *int     `json:"atlas_probes,omitempty"`
	AtlasCountryPct *float64 `json:"atlas_country_pct,omitempty"`

	HoldoutPrefixes   *int     `json:"holdout_prefixes,omitempty"`
	HoldoutCountryPct *float64 `json:"holdout_country_pct,omitempty"`
	HoldoutCityPct    *float64 `json:"holdout_city_pct,omitempty"`

	// The reference databases scored like ours (the holdout rows are looked up in the reference itself).
	DBIPCountryPct        *float64 `json:"dbip_country_pct,omitempty"`
	DBIPCityPct           *float64 `json:"dbip_city_pct,omitempty"`
	DBIPAtlasCountryPct   *float64 `json:"dbip_atlas_country_pct,omitempty"`
	DBIPHoldoutCountryPct *float64 `json:"dbip_holdout_country_pct,omitempty"`
	DBIPHoldoutCityPct    *float64 `json:"dbip_holdout_city_pct,omitempty"`

	MaxMindCountryPct        *float64 `json:"maxmind_country_pct,omitempty"`
	MaxMindCityPct           *float64 `json:"maxmind_city_pct,omitempty"`
	MaxMindAtlasCountryPct   *float64 `json:"maxmind_atlas_country_pct,omitempty"`
	MaxMindHoldoutCountryPct *float64 `json:"maxmind_holdout_country_pct,omitempty"`
	MaxMindHoldoutCityPct    *float64 `json:"maxmind_holdout_city_pct,omitempty"`

	IP2LocationCountryPct        *float64 `json:"ip2location_country_pct,omitempty"`
	IP2LocationCityPct           *float64 `json:"ip2location_city_pct,omitempty"`
	IP2LocationAtlasCountryPct   *float64 `json:"ip2location_atlas_country_pct,omitempty"`
	IP2LocationHoldoutCountryPct *float64 `json:"ip2location_holdout_country_pct,omitempty"`
	IP2LocationHoldoutCityPct    *float64 `json:"ip2location_holdout_city_pct,omitempty"`
}

// refSlots are the places in evalMetrics where the scores of one reference go.
type refSlots struct {
	countryAgree, cityAgree, country, city, atlas, holdoutCountry, holdoutCity **float64
}

func (m *evalMetrics) slots(key string) refSlots {
	switch key {
	case "dbip":
		return refSlots{&m.DBIPCountryAgreePct, &m.DBIPCityAgreePct, &m.DBIPCountryPct, &m.DBIPCityPct, &m.DBIPAtlasCountryPct, &m.DBIPHoldoutCountryPct, &m.DBIPHoldoutCityPct}
	case "maxmind":
		return refSlots{&m.MaxMindCountryAgreePct, &m.MaxMindCityAgreePct, &m.MaxMindCountryPct, &m.MaxMindCityPct, &m.MaxMindAtlasCountryPct, &m.MaxMindHoldoutCountryPct, &m.MaxMindHoldoutCityPct}
	case "ip2location":
		return refSlots{&m.IP2LocationCountryAgreePct, &m.IP2LocationCityAgreePct, &m.IP2LocationCountryPct, &m.IP2LocationCityPct, &m.IP2LocationAtlasCountryPct, &m.IP2LocationHoldoutCountryPct, &m.IP2LocationHoldoutCityPct}
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

// loadIP2Location reads the IPv4 ranges of an IP2Location LITE DB11 CSV: no
// header, quoted columns ip_from, ip_to (decimal numbers), country_code,
// country_name, region_name, city_name, latitude, longitude, ... "-" means
// unknown. Rows that do not parse (or overlap the row before) are skipped.
func loadIP2Location(path string, ids names) ([]rng, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	var out []rng
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(rec) < 6 {
			continue
		}
		lo, err1 := strconv.ParseUint(strings.TrimSpace(rec[0]), 10, 32)
		hi, err2 := strconv.ParseUint(strings.TrimSpace(rec[1]), 10, 32)
		if err1 != nil || err2 != nil || lo > hi {
			continue
		}
		city := ""
		if strings.TrimSpace(rec[5]) != "-" {
			city = rec[5]
		}
		x := rng{lo: uint32(lo), hi: uint32(hi), country: ids.id(normCountry(rec[2])), city: ids.id(normCity(city))}
		if x.country == 0 && x.city == 0 {
			continue
		}
		out = append(out, x)
	}
	slices.SortStableFunc(out, func(a, b rng) int { return cmpU32(a.lo, b.lo) })
	merged := out[:0]
	for _, x := range out {
		if n := len(merged); n > 0 {
			prev := &merged[n-1]
			if x.lo <= prev.hi {
				continue // overlaps the row before
			}
			if prev.hi+1 == x.lo && prev.country == x.country && prev.city == x.city && prev.hi != math.MaxUint32 {
				prev.hi = x.hi
				continue
			}
		}
		merged = append(merged, x)
	}
	if len(merged) == 0 {
		return nil, fmt.Errorf("%s: no usable rows", path)
	}
	return merged, nil
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
		refs = append(refs, reference{"dbip", ranges})
	}
	if in.maxmind != "" {
		ranges, err := openRanges(in.maxmind, ids, geoipFields)
		if err != nil {
			return nil, err
		}
		refs = append(refs, reference{"maxmind", ranges})
	}
	if in.ip2location != "" {
		ranges, err := loadIP2Location(in.ip2location, ids)
		if err != nil {
			return nil, err
		}
		refs = append(refs, reference{"ip2location", ranges})
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

	// Lookups of ours and of each reference, for the probes and the holdout rows.
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
			for _, ref := range refs {
				rs, err := scoreHoldout(rangeLookup(ref.ranges, reverse), in.holdoutTruth)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", in.holdoutTruth, err)
				}
				slots := m.slots(ref.key)
				*slots.holdoutCountry = ratio(uint64(rs.countryOK), uint64(rs.rows))
				*slots.holdoutCity = ratio(uint64(rs.cityOK), uint64(rs.cityRows))
			}
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
