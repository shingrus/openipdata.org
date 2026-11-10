package main

// mmdbbuilder eval: scores a built ip2geo MMDB for completeness and
// correctness and prints the scores as one JSON object. IPv4 only.
//
// Address-weighted scores walk the IPv4 networks of two databases together
// (both sorted, so one pass), per-probe and per-prefix scores look addresses
// up. A reference that is not given leaves its scores out of the output.

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
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

// evalInputs are the files "eval" reads. Only db and rir are required.
type evalInputs struct {
	db           string // the MMDB to score
	rir          string // RIR layer: the IPv4 space that should have a country
	dbip         string // DB-IP city MMDB
	ipinfo       string // IPinfo country MMDB
	atlas        string // truth CSV ip,country,lat,lon
	holdoutDB    string // MMDB built without the held-out geofeeds
	holdoutTruth string // truth CSV prefix,country,city of the held-out rows
}

// evalMetrics is the JSON printed by "eval"; a score whose reference is
// missing (or shares no addresses with us) is nil and left out.
type evalMetrics struct {
	CountryPct *float64 `json:"country_pct,omitempty"`
	CityPct    *float64 `json:"city_pct,omitempty"`

	DBIPCountryAgreePct *float64 `json:"dbip_country_agree_pct,omitempty"`
	DBIPCityAgreePct    *float64 `json:"dbip_city_agree_pct,omitempty"`

	IPinfoCountryAgreePct    *float64 `json:"ipinfo_country_agree_pct,omitempty"`
	ConsensusCountryAgreePct *float64 `json:"consensus_country_agree_pct,omitempty"`

	AtlasProbes     *int     `json:"atlas_probes,omitempty"`
	AtlasCountryPct *float64 `json:"atlas_country_pct,omitempty"`

	HoldoutPrefixes   *int     `json:"holdout_prefixes,omitempty"`
	HoldoutCountryPct *float64 `json:"holdout_country_pct,omitempty"`
	HoldoutCityPct    *float64 `json:"holdout_city_pct,omitempty"`
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

// dbipFields: country.iso_code and city.names.en, like GeoLite2.
func dbipFields(res maxminddb.Result) (string, string, error) {
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

// ipinfoFields: country_code (IPinfo Lite has no city).
func ipinfoFields(res maxminddb.Result) (string, string, error) {
	var r struct {
		Country string `maxminddb:"country_code"`
	}
	err := res.Decode(&r)
	return r.Country, "", err
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

// openDB opens an MMDB and loads its IPv4 ranges.
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

// evaluate computes the scores for the inputs that are set.
func evaluate(in evalInputs) (*evalMetrics, error) {
	if in.db == "" || in.rir == "" {
		return nil, errors.New("eval needs -db and -rir")
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
	var withCountry, withCity uint64
	join(ours, space, func(lo, hi uint32, x, y *rng) {
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
	m.CountryPct, m.CityPct = ratio(withCountry, total), ratio(withCity, total)

	var dbip, ipinfo []rng
	if in.dbip != "" {
		if dbip, err = openRanges(in.dbip, ids, dbipFields); err != nil {
			return nil, err
		}
		var both, same, cityBoth, citySame uint64
		join(ours, dbip, func(lo, hi uint32, x, y *rng) {
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
		m.DBIPCountryAgreePct, m.DBIPCityAgreePct = ratio(same, both), ratio(citySame, cityBoth)
	}
	if in.ipinfo != "" {
		if ipinfo, err = openRanges(in.ipinfo, ids, ipinfoFields); err != nil {
			return nil, err
		}
		var both, same uint64
		join(ours, ipinfo, func(lo, hi uint32, x, y *rng) {
			if x == nil || y == nil || x.country == 0 || y.country == 0 {
				return
			}
			n := uint64(hi) - uint64(lo) + 1
			both += n
			if x.country == y.country {
				same += n
			}
		})
		m.IPinfoCountryAgreePct = ratio(same, both)
	}
	if dbip != nil && ipinfo != nil {
		// Where the two references agree with each other, do we agree too?
		var consensus []rng
		join(dbip, ipinfo, func(lo, hi uint32, x, y *rng) {
			if x != nil && y != nil && x.country != 0 && x.country == y.country {
				consensus = append(consensus, rng{lo: lo, hi: hi, country: x.country})
			}
		})
		var both, same uint64
		join(ours, consensus, func(lo, hi uint32, x, y *rng) {
			if x == nil || y == nil || x.country == 0 {
				return
			}
			n := uint64(hi) - uint64(lo) + 1
			both += n
			if x.country == y.country {
				same += n
			}
		})
		m.ConsensusCountryAgreePct = ratio(same, both)
	}

	if in.atlas != "" {
		reader, err := maxminddb.Open(in.db)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		probes, ok, err := scoreProbes(reader, in.atlas)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", in.atlas, err)
		}
		if probes > 0 {
			m.AtlasProbes, m.AtlasCountryPct = count(probes), ratio(uint64(ok), uint64(probes))
		}
	}
	if in.holdoutDB != "" || in.holdoutTruth != "" {
		if in.holdoutDB == "" || in.holdoutTruth == "" {
			return nil, errors.New("eval needs both -holdout-db and -holdout-truth")
		}
		reader, err := maxminddb.Open(in.holdoutDB)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		s, err := scoreHoldout(reader, in.holdoutTruth)
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

// lookupPlace returns the normalized country and city of one address.
func lookupPlace(reader *maxminddb.Reader, addr netip.Addr) (country, city string, err error) {
	res := reader.Lookup(addr)
	if !res.Found() {
		return "", "", res.Err()
	}
	country, city, err = ourFields(res)
	return normCountry(country), normCity(city), err
}

// scoreProbes counts the probes (ip,country,lat,lon) whose country we have right.
func scoreProbes(reader *maxminddb.Reader, path string) (probes, ok int, err error) {
	var lookupErr error
	err = readTruth(path, []string{"ip", "country"}, func(f []string) {
		addr, perr := netip.ParseAddr(strings.TrimSpace(f[0]))
		want := normCountry(f[1])
		if perr != nil || !addr.Unmap().Is4() || want == "" {
			return
		}
		got, _, lerr := lookupPlace(reader, addr.Unmap())
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
// (prefix,country,city) in the database built without its feed.
func scoreHoldout(reader *maxminddb.Reader, path string) (holdoutScore, error) {
	var s holdoutScore
	var lookupErr error
	err := readTruth(path, []string{"prefix", "country", "city"}, func(f []string) {
		p, perr := netip.ParsePrefix(strings.TrimSpace(f[0]))
		want, wantCity := normCountry(f[1]), normCity(f[2])
		if perr != nil || !p.Addr().Is4() || want == "" {
			return
		}
		got, gotCity, lerr := lookupPlace(reader, p.Masked().Addr())
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
