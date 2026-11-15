package geo

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"openipdata.org/hatchet/bgp"
	"openipdata.org/hatchet/publish"
)

// The BGP community city layer (25-bgp). Some networks tag the routes they learn with
// a community value that means "learned in city X". sync-bgp reads the values of every
// IPv4 prefix of a public RIPE RIS routing table (R2: bgp.CommunitiesKey). Here the
// value -> city meaning is learned on the prefixes whose city the geofeed layer already
// knows, and given to the other prefixes that carry the value. It is one more small
// piece of evidence and sits below 30-pdb on purpose: every later layer replaces it
// where it has rows, so it only shows where nothing else gives a city.
//
//  1. Ownership: an IPv4 address belongs to its most specific prefix of the communities
//     file; a prefix owns its addresses minus the more specific prefixes in it.
//  2. Truth: a prefix has a known city when the geofeed rows with a city cover at least
//     bgpTruthCover of its owned addresses and at least bgpTruthShare of those covered
//     addresses are in one city (country and normalised name). In the geofeed file a
//     more specific row wins over a broader one, and of a repeated prefix the last row.
//  3. Learn: for every value, over the known prefixes that carry it: n = their number;
//     it is a rule when n >= bgpMinPrefixes, its top city has at least bgpMinCityShare of
//     n and the top country at least bgpMinCountryShare of n. (sync-bgp already dropped
//     the values that every route carries and those of fewer than 3 prefixes.)
//  4. Apply: to every prefix with at least one rule among its values, when ALL of its
//     rules name the same city, its owned addresses get that city. A disagreement gives
//     nothing.
//  5. Rows: only the owned parts, split into prefixes, in address order.
const (
	// BGPLayer is the layer file in DataDir/geo/layers.
	BGPLayer = "25-bgp.csv.gz"

	bgpMinPrefixes     = 3
	bgpMinCityShare    = 0.80
	bgpMinCountryShare = 0.90
	bgpTruthCover      = 0.50
	bgpTruthShare      = 0.80

	bgpSource = "bgp"
)

// bgpInputError marks a problem with the communities file itself, which skips the
// layer (it is optional) instead of failing the build.
type bgpInputError struct{ err error }

func (e *bgpInputError) Error() string { return e.err.Error() }
func (e *bgpInputError) Unwrap() error { return e.err }

// bgpPrefix is one prefix of the communities file; its values are vals[vs:vs+vn].
type bgpPrefix struct {
	addr   uint32
	bits   uint8
	vs, vn uint32
}

func (p bgpPrefix) first() uint32 { return p.addr }
func (p bgpPrefix) last() uint32  { return p.addr | (1<<(32-p.bits) - 1) } // bits 0: all ones; shift by 32 is fine for uint32

// bgpTable is the parsed communities file: prefixes sorted by (address, length), the
// value ids of all prefixes in one slice (each prefix's ids sorted, no repeats).
type bgpTable struct {
	prefixes []bgpPrefix
	vals     []uint32
	nvals    int
}

// readBGPFile parses the communities file (see bgp.OutFile). Rows of another family,
// with a bad prefix or a bad value are skipped; a prefix repeated keeps its last row.
func readBGPFile(path string) (*bgpTable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &bgpInputError{err}
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, &bgpInputError{err}
	}
	defer gz.Close()
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	if !sc.Scan() {
		err := sc.Err()
		if err == nil {
			err = errors.New("empty file")
		}
		return nil, &bgpInputError{err}
	}
	if h := strings.TrimSpace(sc.Text()); h != "prefix,communities" {
		return nil, &bgpInputError{fmt.Errorf("header %q is not prefix,communities", h)}
	}
	t := &bgpTable{}
	ids := map[string]uint32{}
	for sc.Scan() {
		line := sc.Bytes()
		comma := bytes.IndexByte(line, ',')
		if comma < 0 {
			continue
		}
		p, err := netip.ParsePrefix(string(line[:comma]))
		if err != nil || !p.Addr().Is4() {
			continue
		}
		a := p.Masked().Addr().As4()
		entry := bgpPrefix{addr: uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]), bits: uint8(p.Bits()), vs: uint32(len(t.vals))}
		rest := line[comma+1:]
		for len(rest) > 0 {
			var tok []byte
			if sp := bytes.IndexByte(rest, ' '); sp >= 0 {
				tok, rest = rest[:sp], rest[sp+1:]
			} else {
				tok, rest = rest, nil
			}
			if len(tok) == 0 {
				continue
			}
			id, ok := ids[string(tok)]
			if !ok {
				id = uint32(len(ids))
				ids[string(tok)] = id
			}
			t.vals = append(t.vals, id)
		}
		own := t.vals[entry.vs:]
		slices.Sort(own)
		own = slices.Compact(own)
		t.vals = t.vals[:int(entry.vs)+len(own)]
		entry.vn = uint32(len(own))
		t.prefixes = append(t.prefixes, entry)
	}
	if err := sc.Err(); err != nil {
		return nil, &bgpInputError{err}
	}
	t.nvals = len(ids)
	// Sorted by (address, length); a repeated prefix keeps its last row (its values stay
	// in vals, unused).
	if !slices.IsSortedFunc(t.prefixes, cmpBGPPrefix) {
		slices.SortStableFunc(t.prefixes, cmpBGPPrefix)
	}
	out := t.prefixes[:0]
	for i, p := range t.prefixes {
		if i+1 < len(t.prefixes) && t.prefixes[i+1].addr == p.addr && t.prefixes[i+1].bits == p.bits {
			continue
		}
		out = append(out, p)
	}
	t.prefixes = out
	return t, nil
}

func cmpBGPPrefix(a, b bgpPrefix) int {
	switch {
	case a.addr != b.addr:
		if a.addr < b.addr {
			return -1
		}
		return 1
	case a.bits != b.bits:
		return int(a.bits) - int(b.bits)
	}
	return 0
}

// sweepOwned walks items sorted by (first address, length) that nest like prefixes (any
// two are disjoint or one holds the other) and calls emit(start, end, i) for every
// maximal address range that item i holds and no more specific item does, in address
// order. Two items with the same prefix must not occur.
func sweepOwned(n int, rng func(i int) (first, last uint32), emit func(start, end uint32, i int)) {
	type open struct {
		last uint32
		item int
	}
	var stack []open
	var cur uint64 // next address not yet emitted for the top of the stack
	closeTop := func() {
		top := stack[len(stack)-1]
		if cur <= uint64(top.last) {
			emit(uint32(cur), top.last, top.item)
		}
		cur = uint64(top.last) + 1
		stack = stack[:len(stack)-1]
	}
	for i := 0; i < n; i++ {
		first, last := rng(i)
		for len(stack) > 0 && stack[len(stack)-1].last < first {
			closeTop()
		}
		if len(stack) > 0 && cur < uint64(first) {
			top := stack[len(stack)-1]
			emit(uint32(cur), first-1, top.item)
		}
		stack = append(stack, open{last, i})
		cur = uint64(first)
	}
	for len(stack) > 0 {
		closeTop()
	}
}

// bgpFeedRow is one IPv4 row of the geofeed layer. key is the city key (1 and up), 0
// for a row without a city; country[0] == 0 marks a row with a bad country, which only
// deletes an earlier row of the same prefix (as the build does).
type bgpFeedRow struct {
	addr    uint32
	bits    uint8
	country [2]byte
	key     uint32
}

// bgpCities gives each (country, normalised city) one key and remembers the spellings.
type bgpCities struct {
	ids   map[cityKey]uint32
	keys  []cityKey // key-1 -> country and normalised name
	forms []map[string]int
}

func newBGPCities() *bgpCities { return &bgpCities{ids: map[cityKey]uint32{}} }

func (c *bgpCities) key(country, city string) uint32 {
	ck := cityKey{country, normCity(city)}
	id, ok := c.ids[ck]
	if !ok {
		c.keys = append(c.keys, ck)
		c.forms = append(c.forms, map[string]int{})
		id = uint32(len(c.keys))
		c.ids[ck] = id
	}
	c.forms[id-1][city]++
	return id
}

// spelling is the form of the city written most often (ties: the smallest string).
func (c *bgpCities) spelling(id uint32) string {
	var best string
	n := 0
	for f, k := range c.forms[id-1] {
		if k > n || (k == n && f < best) {
			best, n = f, k
		}
	}
	return best
}

// readBGPFeeds reads the IPv4 rows of the geofeed layer: sorted by (address, length),
// exact repeats merged (the last row wins), rows with a bad country removed.
func readBGPFeeds(path string) ([]bgpFeedRow, *bgpCities, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, err
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	header, err := r.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("read header: %w", err)
	}
	col := func(name string) (int, error) {
		if i := slices.Index(header, name); i >= 0 {
			return i, nil
		}
		return 0, fmt.Errorf("header %q lacks %s", strings.Join(header, ","), name)
	}
	prefixCol, err1 := col("prefix")
	countryCol, err2 := col("country")
	cityCol, err3 := col("city")
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, nil, err
	}
	var rows []bgpFeedRow
	cities := newBGPCities()
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			var parseErr *csv.ParseError
			if errors.As(err, &parseErr) {
				continue // the build skips such a row as well
			}
			return nil, nil, err
		}
		field := func(i int) string {
			if i < len(rec) {
				return rec[i]
			}
			return ""
		}
		p, ok := parseFeedPrefix(field(prefixCol))
		if !ok || !p.Addr().Is4() {
			continue
		}
		a := p.Addr().As4()
		row := bgpFeedRow{addr: uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]), bits: uint8(p.Bits())}
		country := strings.ToUpper(strings.TrimSpace(field(countryCol)))
		if validCountry(country) {
			row.country = [2]byte{country[0], country[1]}
			if city := strings.TrimSpace(field(cityCol)); city != "" {
				row.key = cities.key(country, city)
			}
		}
		rows = append(rows, row)
	}
	slices.SortStableFunc(rows, func(a, b bgpFeedRow) int {
		return cmpBGPPrefix(bgpPrefix{addr: a.addr, bits: a.bits}, bgpPrefix{addr: b.addr, bits: b.bits})
	})
	out := rows[:0]
	for i, row := range rows {
		if i+1 < len(rows) && rows[i+1].addr == row.addr && rows[i+1].bits == row.bits {
			continue
		}
		if row.country[0] != 0 {
			out = append(out, row)
		}
	}
	return out, cities, nil
}

// cityWeight is the number of addresses of a prefix that a geofeed city covers.
type cityWeight struct {
	key uint32
	w   uint64
}

// bgpStats is what bgpRows reports.
type bgpStats struct {
	Prefixes int // prefixes in the communities file
	Known    int // prefixes with a known city (the truth)
	Rules    int // values that are a rule
	Located  int // prefixes that got a city
	Rows     int
}

// bgpRows is the 25-bgp layer (see the top of this file): it reads the communities
// file and the geofeed layer file and writes the rows to w. Pure: no database, no
// network. A problem with the communities file is a *bgpInputError.
func bgpRows(communitiesPath, feedsPath string, w *csv.Writer) (bgpStats, error) {
	var st bgpStats
	tbl, err := readBGPFile(communitiesPath)
	if err != nil {
		return st, err
	}
	feeds, cities, err := readBGPFeeds(feedsPath)
	if err != nil {
		return st, err
	}
	n := len(tbl.prefixes)
	st.Prefixes = n

	// 1. Ownership.
	type seg struct {
		start, end uint32
		idx        int32
	}
	var segs []seg
	owned := make([]uint64, n)
	sweepOwned(n, func(i int) (uint32, uint32) { return tbl.prefixes[i].first(), tbl.prefixes[i].last() },
		func(start, end uint32, i int) {
			segs = append(segs, seg{start, end, int32(i)})
			owned[i] += uint64(end) - uint64(start) + 1
		})

	// 2. Truth: the geofeed rows with a city, a more specific row winning.
	tallies := make([][]cityWeight, n)
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
			cw := &tallies[segs[k].idx]
			if m := len(*cw); m > 0 && (*cw)[m-1].key == key {
				(*cw)[m-1].w += uint64(hi) - uint64(lo) + 1
				continue
			}
			found := false
			for x := range *cw {
				if (*cw)[x].key == key {
					(*cw)[x].w += uint64(hi) - uint64(lo) + 1
					found = true
					break
				}
			}
			if !found {
				*cw = append(*cw, cityWeight{key, uint64(hi) - uint64(lo) + 1})
			}
		}
	})
	feeds = nil
	known := make([]uint32, n) // city key, 0 unknown
	for i, cw := range tallies {
		var covered, top uint64
		var topKey uint32
		for _, c := range cw {
			covered += c.w
			if c.w > top || (c.w == top && c.key < topKey) {
				top, topKey = c.w, c.key
			}
		}
		if covered == 0 || owned[i] == 0 {
			continue
		}
		if float64(covered)/float64(owned[i]) >= bgpTruthCover && float64(top)/float64(covered) >= bgpTruthShare {
			known[i] = topKey
			st.Known++
		}
	}
	tallies = nil

	// 3. Learn. The known prefixes' cities per value, in one slice (CSR layout).
	count := make([]uint32, tbl.nvals)
	for i, p := range tbl.prefixes {
		if known[i] == 0 {
			continue
		}
		for _, v := range tbl.vals[p.vs : p.vs+p.vn] {
			count[v]++
		}
	}
	start := make([]uint32, tbl.nvals+1)
	for v, c := range count {
		if c < bgpMinPrefixes {
			c = 0
		}
		start[v+1] = start[v] + c
	}
	fill := make([]uint32, tbl.nvals)
	flat := make([]uint32, start[tbl.nvals])
	for i, p := range tbl.prefixes {
		if known[i] == 0 {
			continue
		}
		for _, v := range tbl.vals[p.vs : p.vs+p.vn] {
			if count[v] >= bgpMinPrefixes {
				flat[start[v]+fill[v]] = known[i]
				fill[v]++
			}
		}
	}
	rule := make([]uint32, tbl.nvals) // city key of the rule, 0 none
	countries := map[[2]byte]int{}
	for v := 0; v < tbl.nvals; v++ {
		c := flat[start[v]:start[v+1]]
		if len(c) < bgpMinPrefixes {
			continue
		}
		slices.Sort(c)
		var topKey uint32
		var top int
		clear(countries)
		var topCountry int
		for a := 0; a < len(c); {
			b := a
			for b < len(c) && c[b] == c[a] {
				b++
			}
			if b-a > top {
				top, topKey = b-a, c[a]
			}
			k := cities.keys[c[a]-1].country
			countries[[2]byte{k[0], k[1]}] += b - a
			a = b
		}
		for _, k := range countries {
			topCountry = max(topCountry, k)
		}
		if float64(top)/float64(len(c)) >= bgpMinCityShare && float64(topCountry)/float64(len(c)) >= bgpMinCountryShare {
			rule[v] = topKey
			st.Rules++
		}
	}
	flat = nil

	// 4. Apply to every prefix.
	city := make([]uint32, n)
	for i, p := range tbl.prefixes {
		var key uint32
		for _, v := range tbl.vals[p.vs : p.vs+p.vn] {
			r := rule[v]
			if r == 0 {
				continue
			}
			if key != 0 && key != r {
				key = 0
				break
			}
			key = r
		}
		if city[i] = key; key != 0 {
			st.Located++
		}
	}

	// 5. Rows: the owned parts, in address order.
	spell := map[uint32]string{}
	for _, s := range segs {
		key := city[s.idx]
		if key == 0 {
			continue
		}
		name, ok := spell[key]
		if !ok {
			name = cities.spelling(key)
			spell[key] = name
		}
		country := cities.keys[key-1].country
		for lo := uint64(s.start); lo <= uint64(s.end); {
			size := 32 // bits of the block: the largest aligned block that fits
			if lo != 0 {
				size = bits.TrailingZeros32(uint32(lo))
			}
			for size > 0 && lo+(1<<size)-1 > uint64(s.end) {
				size--
			}
			a := uint32(lo)
			addr := netip.AddrFrom4([4]byte{byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a)})
			w.Write([]string{netip.PrefixFrom(addr, 32-size).String(), country, "", name, bgpSource})
			st.Rows++
			lo += 1 << size
		}
	}
	return st, nil
}

// downloadBGP writes the 25-bgp layer. It fetches the communities file sync-bgp left in R2
// (the same ETag and saved-copy rule as the other R2 layers, in downloads) and
// computes the layer from it and the geofeed layer in the layers directory. The layer is
// optional: a missing or old object (older than rowsMaxAge), any R2 error, an unreadable
// communities file, no geofeed layer or no row skips it (src.SkippedLayers, a warning).
// Unlike 60-whois and 70-geofeeds it never adds to src.StaleLayers, so the health
// check ping is still sent. Only a cancelled context or a failure to write the layer fails.
func (p *Pipeline) downloadBGP(ctx context.Context, fetch r2Fetch, downloads, layers string, now time.Time, src *Source) error {
	skip := func(format string, args ...any) {
		src.SkippedLayers = append(src.SkippedLayers, BGPLayer)
		p.log("download: WARNING "+format+"; building without the %s layer", append(args, BGPLayer)...)
	}
	feeds := filepath.Join(layers, FeedsLayer)
	if _, err := os.Stat(feeds); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		skip("no %s", FeedsLayer)
		return nil
	}
	saved := filepath.Join(downloads, bgp.OutFile)
	info, downloaded, err := fetch(ctx, bgp.CommunitiesKey, saved, savedETag(saved))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, publish.ErrNotFound) {
			skip("%s is not in R2 (has sync-bgp uploaded it?)", bgp.CommunitiesKey)
		} else {
			skip("%s could not be fetched from R2: %v", bgp.CommunitiesKey, err)
		}
		return nil
	}
	if downloaded {
		p.saveETag(saved, info.ETag, bgp.OutFile)
	} else {
		src.R2Reused = append(src.R2Reused, BGPLayer)
	}
	src.R2Uploaded[BGPLayer] = info.LastModified.UTC().Format(time.RFC3339)
	if now.Sub(info.LastModified) > rowsMaxAge {
		skip("%s is from %s; is sync-bgp failing?", bgp.CommunitiesKey, src.R2Uploaded[BGPLayer])
		return nil
	}
	var stats bgpStats
	rows, err := writeLayer(filepath.Join(layers, BGPLayer), func(w *csv.Writer) error {
		var err error
		stats, err = bgpRows(saved, feeds, w)
		if err == nil && stats.Rows == 0 {
			err = ErrNoRows
		}
		return err
	})
	var inputErr *bgpInputError
	switch {
	case errors.As(err, &inputErr):
		// A bad copy must not be kept under a good ETag: fetch it again next run.
		os.Remove(saved + etagSuffix)
		os.Remove(saved)
		skip("%s is unreadable: %v", bgp.OutFile, err)
		return nil
	case errors.Is(err, ErrNoRows):
		skip("no prefix got a community city")
		return nil
	case err != nil:
		return fmt.Errorf("BGP community rows: %w", err)
	}
	src.BGPRules, src.BGPKnown, src.BGPPrefixes = stats.Rules, stats.Known, stats.Located
	src.Rows[BGPLayer] = rows
	p.log("download: BGP communities: %d prefixes, %d with a geofeed city, %d rules, %d prefixes located, %d rows", stats.Prefixes, stats.Known, stats.Rules, stats.Located, rows)
	return nil
}

// holdoutBGP recomputes the BGP community layer from the kept feeds file and the
// communities file Download saved, into out, and sets replace[BGPLayer] to it. When the
// saved file is missing or unreadable, or the result has no rows, the layer is left out
// of the holdout build (replace[...] = "") and the returned note says so.
func (p *Pipeline) holdoutBGP(kept, out string, replace map[string]string) (note string, err error) {
	replace[BGPLayer] = ""
	rows, err := writeLayer(out, func(w *csv.Writer) error {
		_, err := bgpRows(filepath.Join(p.dir(), "downloads", bgp.OutFile), kept, w)
		return err
	})
	var inputErr *bgpInputError
	if errors.As(err, &inputErr) {
		return "; " + BGPLayer + " left out: no saved communities", nil
	}
	if err != nil {
		return "", fmt.Errorf("holdout BGP rows: %w", err)
	}
	if rows == 0 {
		os.Remove(out)
		return "; " + BGPLayer + " left out: no rows from the kept feeds", nil
	}
	replace[BGPLayer] = out
	return "", nil
}
