package main

// The RTT country check (mmdbbuilder geo -rtt-country): a speed-of-light test of the country.
//
// M-Lab's speed test records the lowest round-trip time (min_rtt, ms) between each of its
// servers and each client /24. A client that answered a server in r ms is at most
// (r+1) * rttKmPerMs + rttBaseKm kilometres from it (light in fibre goes about 102 km per ms of
// round trip, plus 300 km for the path and the server's own position). If no GeoNames place of
// our record's country lies that close to the server, the country is impossible. The check then
// switches the /24 to the country of its fastest server, but only when that country passes the
// same test for every server of the /24.
//
// It runs after all layers and before the guesses (geo.go), so that the guesses fill cities in
// the corrected country. The file is another workflow's output (import-mlab-sites, a gzipped CSV
// with the columns prefix, country, city and rtt_ms) and is untrusted: a row that is not usable
// is skipped and counted. It needs the GeoNames index (the coordinates of the server's city and
// the places of a country). A country with no place in the index is never impossible. IPv4 only.

import (
	"bufio"
	"compress/gzip"
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

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/inserter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

const (
	rttKmPerMs = 102.1 // km one way per ms of round-trip time
	rttBaseKm  = 300.0 // added to every limit
	rttMaxMs   = 30.0  // rows above this are not used (the export holds none)
	earthKmDeg = 6371 * math.Pi / 180
)

// rttSite is a server city that GeoNames places.
type rttSite struct {
	country string
	city    string
	place   gnPlace
}

// rttRow is one usable row: a client /24 (its first address), a server and the time.
type rttRow struct {
	client uint32
	site   uint32 // index into the site table
	rtt    float32
}

// rttCounts is what the check did.
type rttCounts struct {
	rows, skipped int
	clients       int // distinct /24s with a usable row
	changed24     int // /24s where at least one part changed country
	partsChanged  int
}

func (c rttCounts) String() string {
	return fmt.Sprintf("rtt-country: %d rows, %d skipped, %d /24s, %d /24s changed, %d parts changed",
		c.rows, c.skipped, c.clients, c.changed24, c.partsChanged)
}

// readRTT reads the RTT file: usable rows sorted by client, then site, then time, and the table
// of sites. The header names the columns (prefix, country, city, rtt_ms).
func readRTT(path string, index geoNames) (rows []rttRow, sites []rttSite, skipped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return nil, nil, 0, err
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	header, err := r.Read()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[strings.TrimSpace(name)] = i
	}
	cPrefix, ok1 := col["prefix"]
	cCountry, ok2 := col["country"]
	cCity, ok3 := col["city"]
	cRTT, ok4 := col["rtt_ms"]
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil, nil, 0, fmt.Errorf("header %q lacks prefix, country, city, or rtt_ms", strings.Join(header, ","))
	}
	need := max(cPrefix, cCountry, cCity, cRTT) + 1
	ids := map[string]uint32{}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			var parseErr *csv.ParseError
			if errors.As(err, &parseErr) {
				skipped++
				continue
			}
			return nil, nil, 0, err
		}
		if len(rec) < need {
			skipped++
			continue
		}
		p, perr := netip.ParsePrefix(strings.TrimSpace(rec[cPrefix]))
		country := strings.ToUpper(strings.TrimSpace(rec[cCountry]))
		city := cleanCity(rec[cCity])
		rtt, rerr := strconv.ParseFloat(strings.TrimSpace(rec[cRTT]), 64)
		if perr != nil || !p.Addr().Is4() || p.Bits() != 24 || p != p.Masked() || !validCountries[country] || city == "" ||
			rerr != nil || math.IsNaN(rtt) || math.IsInf(rtt, 0) || rtt <= 0 || rtt > rttMaxMs {
			skipped++
			continue
		}
		key := country + "|" + city
		id, seen := ids[key]
		if !seen {
			place, ok := index.place(country, city)
			if !ok {
				skipped++
				continue
			}
			id = uint32(len(sites))
			sites = append(sites, rttSite{country, city, place})
			ids[key] = id
		}
		rows = append(rows, rttRow{addrNumber(p.Addr()), id, float32(rtt)})
	}
	slices.SortFunc(rows, cmpRTTRow)
	return rows, sites, skipped, nil
}

// cmpRTTRow orders rows by client, then site, then time.
func cmpRTTRow(a, b rttRow) int {
	if a.client != b.client {
		if a.client < b.client {
			return -1
		}
		return 1
	}
	if a.site != b.site {
		if a.site < b.site {
			return -1
		}
		return 1
	}
	switch {
	case a.rtt < b.rtt:
		return -1
	case a.rtt > b.rtt:
		return 1
	}
	return 0
}

// rttChecker answers "is this country impossible for this server and time".
type rttChecker struct {
	byCountry map[string][]gnPlace // places of a country, by latitude; built on first use
	built     bool
	dist      map[string]map[uint32]float64
	index     geoNames
	sites     []rttSite
}

func newRTTChecker(index geoNames, sites []rttSite) *rttChecker {
	return &rttChecker{byCountry: map[string][]gnPlace{}, dist: map[string]map[uint32]float64{}, index: index, sites: sites}
}

// places returns the places of a country, once, sorted by latitude then id. The index holds a
// place under several keys, so places are deduplicated by their GeoNames id.
func (c *rttChecker) places(country string) []gnPlace {
	// Build the lists of all countries in one pass: the index is a map and is walked once.
	if !c.built {
		c.built = true
		seen := map[uint32]bool{}
		for key, p := range c.index {
			cc, ok := plainKeyCountry(key)
			if !ok || seen[p.id] {
				continue
			}
			seen[p.id] = true
			c.byCountry[cc] = append(c.byCountry[cc], p)
		}
		for cc := range c.byCountry {
			slices.SortFunc(c.byCountry[cc], func(a, b gnPlace) int {
				if a.lat != b.lat {
					if a.lat < b.lat {
						return -1
					}
					return 1
				}
				if a.id < b.id {
					return -1
				}
				if a.id > b.id {
					return 1
				}
				return 0
			})
		}
	}
	return c.byCountry[country]
}

// nearest is the distance in km from the site to the nearest place of the country (cached);
// ok is false when the index has no place of that country.
func (c *rttChecker) nearest(country string, site uint32) (km float64, ok bool) {
	ps := c.places(country)
	if len(ps) == 0 {
		return 0, false
	}
	if d, hit := c.dist[country][site]; hit {
		return d, true
	}
	s := c.sites[site].place
	best := math.Inf(1)
	for _, p := range ps {
		if math.Abs(p.lat-s.lat)*earthKmDeg > best {
			continue // the latitude difference alone is already farther than the best so far
		}
		if d := haversineKm(s.lat, s.lon, p.lat, p.lon); d < best {
			best = d
		}
	}
	if c.dist[country] == nil {
		c.dist[country] = map[uint32]float64{}
	}
	c.dist[country][site] = best
	return best, true
}

// impossible says whether some row of the /24 rules the country out.
func (c *rttChecker) impossible(country string, rows []rttRow) bool {
	for _, r := range rows {
		d, ok := c.nearest(country, r.site)
		if !ok {
			return false // unknown country: never impossible
		}
		if d > (float64(r.rtt)+1)*rttKmPerMs+rttBaseKm {
			return true
		}
	}
	return false
}

// correct returns the country the /24 should have instead of x, or "" to keep x.
func (c *rttChecker) correct(x string, rows []rttRow) string {
	if !c.impossible(x, rows) {
		return ""
	}
	// The fastest server: the smallest time, then the smaller country, then the smaller city.
	best := rows[0]
	for _, r := range rows[1:] {
		if r.rtt != best.rtt {
			if r.rtt < best.rtt {
				best = r
			}
			continue
		}
		a, b := c.sites[r.site], c.sites[best.site]
		if a.country < b.country || a.country == b.country && a.city < b.city {
			best = r
		}
	}
	y := c.sites[best.site].country
	if y == x || c.impossible(y, rows) {
		return ""
	}
	return y
}

// applyRTTCountry runs the check on the tree. recordFor gives the shared country-only record of
// a country (the one any country-only layer row of it uses).
func applyRTTCountry(tree *mmdbwriter.Tree, path string, index geoNames, debug bool, recordFor func(place, *geoLayerStats) int, records func(int) mmdbtype.Map) (rttCounts, error) {
	var counts rttCounts
	rows, sites, skipped, err := readRTT(path, index)
	counts.rows, counts.skipped = len(rows), skipped
	if err != nil {
		return counts, err
	}
	checker := newRTTChecker(index, sites)
	var dummy geoLayerStats
	for lo := 0; lo < len(rows); {
		hi := lo
		for hi < len(rows) && rows[hi].client == rows[lo].client {
			hi++
		}
		group := rows[lo:hi]
		lo = hi
		counts.clients++
		prefix := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(group[0].client >> 24), byte(group[0].client >> 16), byte(group[0].client >> 8), 0}), 24)
		changed := 0
		decided := map[string]string{} // existing country -> replacement ("" keeps it)
		fn := func(existing mmdbtype.DataType) (mmdbtype.DataType, error) {
			m, ok := existing.(mmdbtype.Map)
			if !ok {
				return existing, nil
			}
			have, ok := m["country_code"].(mmdbtype.String)
			if !ok || have == "" {
				return existing, nil
			}
			x := string(have)
			y, done := decided[x]
			if !done {
				y = checker.correct(x, group)
				decided[x] = y
			}
			if y == "" {
				return existing, nil
			}
			record := records(recordFor(place{code: y}, &dummy))
			if debug {
				withNet := mmdbtype.Map{"network": mmdbtype.String(prefix.String())}
				for k, v := range record {
					withNet[k] = v
				}
				record = withNet
			}
			changed++
			return record, nil
		}
		if err := tree.InsertFunc(ipNet(prefix), inserter.Func(fn)); err != nil {
			return counts, fmt.Errorf("insert %s (rtt-country): %w", prefix, err)
		}
		if changed > 0 {
			counts.changed24++
			counts.partsChanged += changed
		}
	}
	return counts, nil
}
