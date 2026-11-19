package geo

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"openipdata.org/hatchet/ipdata"
	"openipdata.org/hatchet/rirstats"
)

// The cloud region layer (68-cloud-regions). Amazon publishes its address ranges with a region
// code each (ip-ranges.json: eu-west-1) but without a country or a city. Its geofeed, already in
// the geofeed layer, names a city for most of the same addresses. Here the city of a region is
// LEARNED from those geofeed rows (nothing is written by hand) and given to every range of the
// region, which also covers the ranges the geofeed misses and corrects the country of those that
// the registry data puts in the operator's home country.
//
//  1. Parse (untrusted, streamed, never more than cloudRegionsMaxEntries entries read): IPv4
//     prefixes of "prefixes" only (the IPv6 list is ignored). The LABEL of an entry is its
//     network_border_group when that is a non-empty string, else its region: Local Zones and
//     Wavelength zones carry the parent region in "region" and the real place only in the border
//     group (us-east-1-lim-1 is Lima), so a zone is its own group and never gets the parent's
//     city. Entries that are not an IPv4 prefix between /8 and /32 with no host bits, or not
//     public, or with a label outside [a-z0-9-]{1,48}, are skipped and counted.
//  2. Disjoint: an address belongs to the most specific prefix that holds it. The same prefix
//     twice: the first one in the file, but the pseudo label "global" never wins over a real
//     one. A "global" range gets no row, but a more specific one inside a regional prefix still
//     takes its addresses out of that region. Ranges of neighbouring prefixes are not merged, so
//     a row is never broader than an entry of the file.
//  3. Learn: the addresses that a geofeed row with a city OWNS (its prefix minus the more specific
//     geofeed rows inside it; of a repeated prefix the last row) and that lie inside a label's
//     ranges are counted per (country, city, compared case-insensitively). A label qualifies
//     when it has at least cloudRegionsMinAddrs of them and its main city holds at least
//     cloudRegionsMinShare of them (a small zone simply gets no row).
//  4. Rows: every range of a qualifying label, split into prefixes, in address order, with the
//     main city (the most common form that has a lower-case letter) and its country and the
//     source cloud-regions:aws:<label>. The layer rule -within-first drops a row broader than
//     one prefix of the first layer; the builder counts it as skipped.
//
// The layer file also holds the rows of a second operator, Oracle (oracleregions.go), whose
// cities come from a table; cloudregionsdl.go puts the two together.
const (
	// cloudRegionsURL is Amazon's region file.
	cloudRegionsURL = "https://ip-ranges.amazonaws.com/ip-ranges.json"
	// cloudRegionsFile is the saved copy in the downloads directory.
	cloudRegionsFile = "aws-ip-ranges.json"
	// cloudRegionsMaxFile caps the download (download.Capped).
	cloudRegionsMaxFile = 32 << 20
	// cloudRegionsMaxAge is how old the last confirmed copy may be when the fetch fails: the
	// copy is confirmed (the modification time of <file>.checked) at each successful fetch.
	cloudRegionsMaxAge = 14 * 24 * time.Hour

	cloudRegionsMaxEntries  = 200000 // more entries and the file is not the file
	cloudRegionsMinPrefixes = 1000   // fewer valid entries and it is not the file either
	cloudRegionsMinBits     = 8      // an entry broader than this is not used
	cloudRegionsMinAddrs    = 4096   // geofeed addresses with a city a region needs
	cloudRegionsMinShare    = 80     // percent of them the main city needs
	cloudRegionsGlobal      = "global"
	cloudRegionsSource      = "cloud-regions:aws:"
)

// cloudInputError marks a problem with the region file itself: the layer is skipped and the bad
// copy is not kept.
type cloudInputError struct{ err error }

func (e *cloudInputError) Error() string { return e.err.Error() }
func (e *cloudInputError) Unwrap() error { return e.err }

// cloudRange is one valid entry of the region file.
type cloudRange struct {
	addr   uint32
	bits   uint8
	region string // the label: network_border_group, else region; lower case
}

func (r cloudRange) last() uint32 { return r.addr | (1<<(32-r.bits) - 1) }

// cloudParse counts what parseCloudRegions saw.
type cloudParse struct {
	Entries int // entries in "prefixes"
	Valid   int
	Bad     int
}

// validRegion reports whether s is 1 to 48 characters of a-z, 0-9 and "-".
func validRegion(s string) bool {
	if len(s) < 1 || len(s) > 48 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// cloudEntry is the range of one entry of a region file. ok is false unless the label is valid and
// p is a public IPv4 prefix of minBits to 32 bits with no host bits (the zero prefix of a failed
// parse is not).
func cloudEntry(p netip.Prefix, label string, minBits int) (r cloudRange, ok bool) {
	if !validRegion(label) || !p.Addr().Is4() || p.Bits() < minBits || p.Masked() != p || !ipdata.PublicV4(p.Addr()) {
		return r, false
	}
	a := p.Addr().As4()
	return cloudRange{addr: uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]), bits: uint8(p.Bits()), region: label}, true
}

// skipJSONValue reads one JSON value from dec token by token (nothing is kept).
func skipJSONValue(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// parseCloudRegions reads the region file with a streaming decoder: the top-level keys other than
// "prefixes" are skipped token by token and the entries of "prefixes" are decoded one at a time,
// so a body with too many entries is rejected as soon as the limit is passed. A problem with the
// file as a whole (not JSON, a wrong type, too many entries, too few valid ones) is a
// *cloudInputError; an entry without a prefix or a valid label is only counted as bad.
func parseCloudRegions(data []byte) ([]cloudRange, cloudParse, error) {
	var st cloudParse
	notFile := &cloudInputError{errors.New("not a region file")}
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, st, notFile
	}
	var out []cloudRange
	for dec.More() {
		t, err := dec.Token()
		key, ok := t.(string)
		if err != nil || !ok {
			return nil, st, notFile
		}
		if key != "prefixes" {
			if err := skipJSONValue(dec); err != nil {
				return nil, st, notFile
			}
			continue
		}
		if t, err := dec.Token(); err != nil || t != json.Delim('[') {
			return nil, st, notFile
		}
		for dec.More() {
			var e struct {
				IPPrefix string `json:"ip_prefix"`
				Region   string `json:"region"`
				Border   string `json:"network_border_group"`
			}
			if err := dec.Decode(&e); err != nil {
				return nil, st, notFile
			}
			if st.Entries++; st.Entries > cloudRegionsMaxEntries {
				return nil, st, &cloudInputError{fmt.Errorf("more than %d entries", cloudRegionsMaxEntries)}
			}
			label := strings.ToLower(strings.TrimSpace(e.Border))
			if label == "" {
				label = strings.ToLower(strings.TrimSpace(e.Region))
			}
			p, _ := netip.ParsePrefix(strings.TrimSpace(e.IPPrefix))
			r, ok := cloudEntry(p, label, cloudRegionsMinBits)
			if !ok {
				st.Bad++
				continue
			}
			out = append(out, r)
		}
		if t, err := dec.Token(); err != nil || t != json.Delim(']') {
			return nil, st, notFile
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, st, notFile
	}
	st.Valid = len(out)
	if st.Valid < cloudRegionsMinPrefixes {
		return nil, st, &cloudInputError{fmt.Errorf("%d valid entries, fewer than %d", st.Valid, cloudRegionsMinPrefixes)}
	}
	return out, st, nil
}

// cloudSeg is an address range (inclusive) that belongs to one region (an index into the region
// names).
type cloudSeg struct {
	start, end uint32
	region     int
}

// cloudSegments makes the ranges disjoint (see the top of this file) and returns the address
// ranges of every real region in address order (each range lies inside one entry of the file),
// and the sorted names of the regions (the pseudo region left out).
func cloudSegments(ranges []cloudRange) ([]cloudSeg, []string) {
	ranges = slices.Clone(ranges)
	// Sorted by (address, length); equal prefixes keep the order of the file.
	slices.SortStableFunc(ranges, func(a, b cloudRange) int {
		return cmpBGPPrefix(bgpPrefix{addr: a.addr, bits: a.bits}, bgpPrefix{addr: b.addr, bits: b.bits})
	})
	// One range per prefix: the first, but never the pseudo region over a real one.
	uniq := ranges[:0]
	for i := 0; i < len(ranges); {
		j := i
		for j < len(ranges) && ranges[j].addr == ranges[i].addr && ranges[j].bits == ranges[i].bits {
			j++
		}
		pick := ranges[i]
		for _, r := range ranges[i:j] {
			if r.region != cloudRegionsGlobal {
				pick = r
				break
			}
		}
		uniq = append(uniq, pick)
		i = j
	}
	var names []string
	for _, r := range uniq {
		if r.region != cloudRegionsGlobal {
			names = append(names, r.region)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)

	var segs []cloudSeg
	sweepOwned(len(uniq), func(i int) (uint32, uint32) { return uniq[i].addr, uniq[i].last() },
		func(start, end uint32, i int) {
			if uniq[i].region == cloudRegionsGlobal {
				return
			}
			// Ranges of neighbouring prefixes are NOT merged: a merged range could become a
			// prefix broader than the operator's own entries, and so broader than one
			// registry delegation, which the layer rule -within-first drops.
			region, _ := slices.BinarySearch(names, uniq[i].region)
			segs = append(segs, cloudSeg{start, end, region})
		})
	return segs, names
}

// cloudStats is what cloudRegionRows reports.
type cloudStats struct {
	Parse     cloudParse
	Seen      int // labels in the file (regions and zones; the pseudo label left out)
	Qualified int // regions that got a city
	Rows      int
}

// cloudRow is one row of the layer.
type cloudRow struct {
	prefix                netip.Prefix
	country, city, source string
}

// writeCloudRows writes the rows of all operators to w sorted by prefix (address, then the broader
// prefix first). The sort is stable, so for the same prefix the rows of the operator given first
// come first.
func writeCloudRows(w *csv.Writer, operators ...[]cloudRow) {
	all := slices.Concat(operators...)
	slices.SortStableFunc(all, func(a, b cloudRow) int {
		if c := a.prefix.Addr().Compare(b.prefix.Addr()); c != 0 {
			return c
		}
		return a.prefix.Bits() - b.prefix.Bits()
	})
	for _, r := range all {
		w.Write([]string{r.prefix.String(), r.country, "", r.city, r.source})
	}
}

// cloudRegionRows is the 68-cloud-regions layer of Amazon (see the top of this file): it reads the
// region file (data) and the geofeed layer file and returns the rows in address order. Pure: no
// database, no network. A problem with the region file is a *cloudInputError.
func cloudRegionRows(data []byte, feedsPath string) ([]cloudRow, cloudStats, error) {
	var st cloudStats
	ranges, parse, err := parseCloudRegions(data)
	st.Parse = parse
	if err != nil {
		return nil, st, err
	}
	segs, regions := cloudSegments(ranges)
	st.Seen = len(regions)
	feeds, cities, err := readBGPFeeds(feedsPath)
	if err != nil {
		return nil, st, err
	}

	// Learn: the owned addresses of the geofeed rows with a city, inside each region.
	tallies := make([]map[uint32]uint64, len(regions))
	totals := make([]uint64, len(regions))
	for i := range tallies {
		tallies[i] = map[uint32]uint64{}
	}
	j := 0
	sweepOwned(len(feeds), func(i int) (uint32, uint32) {
		return feeds[i].addr, bgpPrefix{addr: feeds[i].addr, bits: feeds[i].bits}.last()
	}, func(start, end uint32, i int) {
		key := feeds[i].key
		if key == 0 {
			return
		}
		for j < len(segs) && segs[j].end < start {
			j++
		}
		for k := j; k < len(segs) && segs[k].start <= end; k++ {
			lo, hi := max(segs[k].start, start), min(segs[k].end, end)
			n := uint64(hi) - uint64(lo) + 1
			tallies[segs[k].region][key] += n
			totals[segs[k].region] += n
		}
	})

	// The main city of each region. Ties: the smaller country, then the smaller (normalised) name.
	main := make([]uint32, len(regions)) // city key, 0 none
	for r, tally := range tallies {
		if totals[r] < cloudRegionsMinAddrs {
			continue
		}
		var top uint64
		var topKey uint32
		for key, n := range tally {
			if n > top || (n == top && cmpCityKey(cities.keys[key-1], cities.keys[topKey-1]) < 0) {
				top, topKey = n, key
			}
		}
		if top*100 >= totals[r]*cloudRegionsMinShare {
			main[r] = topKey
			st.Qualified++
		}
	}

	// Rows: every range of a qualifying region, in address order.
	var rows []cloudRow
	spell := map[uint32]string{}
	for _, s := range segs {
		key := main[s.region]
		if key == 0 {
			continue
		}
		name, ok := spell[key]
		if !ok {
			name = cloudSpelling(cities, key)
			spell[key] = name
		}
		country := cities.keys[key-1].country
		source := cloudRegionsSource + regions[s.region]
		for _, p := range rirstats.RangeToPrefixes(addrFrom32(s.start), addrFrom32(s.end)) {
			rows = append(rows, cloudRow{p, country, name, source})
		}
	}
	st.Rows = len(rows)
	return rows, st, nil
}

// cloudSpelling is the form of the city written most often in the geofeed layer among the forms
// that have a lower-case letter (ties: the smallest string), so that a feed which writes its rows
// in capitals (COLUMBUS) does not decide how the operator's city is written; when every form is in
// capitals, the most common one.
func cloudSpelling(cities *bgpCities, key uint32) string {
	var best string
	n := 0
	for form, k := range cities.forms[key-1] {
		if form == strings.ToUpper(form) {
			continue
		}
		if k > n || (k == n && form < best) {
			best, n = form, k
		}
	}
	if n == 0 {
		return cities.spelling(key)
	}
	return best
}

func cmpCityKey(a, b cityKey) int {
	if c := strings.Compare(a.country, b.country); c != 0 {
		return c
	}
	return strings.Compare(a.city, b.city)
}

func addrFrom32(a uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a)})
}
