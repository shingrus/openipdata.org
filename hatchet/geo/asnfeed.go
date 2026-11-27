package geo

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
)

// The ASN dominant geofeed city layer (38-asn-geofeed): an ASN whose own
// geofeed rows put (nearly) all of its addresses in one city is very likely to
// have its other announced prefixes there too. The geofeed rows are the best
// evidence we have (the geofeed layer is right about 80% of the time), the
// PeeringDB city of an ASN (30-pdb) only about 46%.
//
// For every ASN and address family separately, the feed rows inside the ASN's
// announced prefixes are weighed by the addresses they cover; the ASN has a
// dominant city in that family when one (country, city) has at least
// domMinShare of the weight and comes from at least domMinRows distinct feed
// rows, and the feed rows together cover at least domMinCoverage of the ASN's
// announced addresses in that family. Every announced prefix of the ASN in
// that family then gets that city.
const (
	// domMinShare is the share of the family's city weight the top city needs.
	domMinShare = 0.8
	// domMinCoverage is the share of the ASN's announced addresses (in that
	// family) that its attributed feed rows, with or without a city, must
	// cover (their weight over the announced weight; overlapping rows can push
	// it above 1). A feed that describes a small part of an ASN says little
	// about the rest.
	domMinCoverage = 0.5
	// domMinRows is the number of distinct feed rows the top city needs; one
	// row is an anecdote, not a pattern.
	domMinRows = 2

	// asnPrefixesFile is the ASN-prefix list Download saves in the geo directory
	// (outside the layers directory, so the build ignores it): the holdout build
	// recomputes the layer from the kept feeds and needs the same prefixes.
	asnPrefixesFile = "asn-prefixes.csv.gz"
)

// asnPrefix is one announced prefix of one ASN (public.asn).
type asnPrefix struct {
	ASN    int64
	Prefix netip.Prefix
}

// announcedQuery reads the announced prefixes of every current ASN (only the prefixes object
// of its json, ipdata.PrefixesJSON); announcedWholeQuery sends the whole json, for the case that
// one damaged value fails the first. Read only.
var (
	announcedQuery      = announcedSelect(ipdata.PrefixesJSON)
	announcedWholeQuery = announcedSelect("a.json")
)

func announcedSelect(doc string) string {
	return `SELECT a.asn, ` + doc + ` FROM public.asn a WHERE ` + ipdata.SeenAtFilter + ` ORDER BY a.asn`
}

// readAnnounced returns the announced prefixes (masked) of the current ASNs.
func readAnnounced(ctx context.Context, dsn string) ([]asnPrefix, error) {
	conn, err := connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	var out []asnPrefix
	// Each pass collects into its own slice, so a failed first pass adds nothing.
	err = ipdata.WithJSONFallback(announcedQuery, announcedWholeQuery, func(query string) error {
		pass, err := readAnnouncedPass(ctx, conn, query)
		if err == nil {
			out = pass
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func readAnnouncedPass(ctx context.Context, conn *pgx.Conn, query string) ([]asnPrefix, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []asnPrefix
	for rows.Next() {
		var asn int64
		var raw string
		if err := rows.Scan(&asn, &raw); err != nil {
			return nil, err
		}
		for _, p := range ipdata.ASNPrefixes(raw) {
			out = append(out, asnPrefix{asn, p.Masked()})
		}
	}
	return out, rows.Err()
}

// writeAnnouncedFile saves the prefixes as a gzipped CSV (asn,prefix) through a temporary file.
func writeAnnouncedFile(path string, prefixes []asnPrefix) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	defer f.Close()
	gz := gzip.NewWriter(f)
	w := csv.NewWriter(gz)
	w.Write([]string{"asn", "prefix"})
	for _, p := range prefixes {
		w.Write([]string{strconv.FormatInt(p.ASN, 10), p.Prefix.String()})
	}
	w.Flush()
	if err := errors.Join(w.Error(), gz.Close(), f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readAnnouncedFile reads what writeAnnouncedFile saved.
func readAnnouncedFile(path string) ([]asnPrefix, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	asnCol, prefixCol := slices.Index(header, "asn"), slices.Index(header, "prefix")
	if asnCol < 0 || prefixCol < 0 {
		return nil, fmt.Errorf("header %q lacks asn or prefix", strings.Join(header, ","))
	}
	var out []asnPrefix
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(rec) <= max(asnCol, prefixCol) {
			continue
		}
		asn, err1 := strconv.ParseInt(rec[asnCol], 10, 64)
		p, err2 := netip.ParsePrefix(rec[prefixCol])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, asnPrefix{asn, p.Masked()})
	}
}

// announcedEntry is what an announced prefix maps to. An ambiguous prefix (the
// same masked prefix announced by more than one ASN) belongs to nobody, but it
// stays in the index: a feed row whose longest containing announced prefix is
// ambiguous is ignored, not given to a shorter prefix that the ambiguous one
// overrides in routing.
type announcedEntry struct {
	asn       int64
	ambiguous bool
}

type announcedIndex struct {
	byPrefix     map[netip.Prefix]announcedEntry
	lens4, lens6 []int // the prefix lengths in use, longest first
}

func newAnnouncedIndex(announced []asnPrefix) *announcedIndex {
	idx := &announcedIndex{byPrefix: make(map[netip.Prefix]announcedEntry, len(announced))}
	var used4, used6 [129]bool
	for _, a := range announced {
		p := a.Prefix.Masked()
		if !p.IsValid() || p.Addr().Is4In6() {
			continue
		}
		if e, ok := idx.byPrefix[p]; ok {
			if e.asn != a.ASN {
				idx.byPrefix[p] = announcedEntry{ambiguous: true}
			}
			continue
		}
		idx.byPrefix[p] = announcedEntry{asn: a.ASN}
		if p.Addr().Is4() {
			used4[p.Bits()] = true
		} else {
			used6[p.Bits()] = true
		}
	}
	for l := 128; l >= 0; l-- {
		if used4[l] {
			idx.lens4 = append(idx.lens4, l)
		}
		if used6[l] {
			idx.lens6 = append(idx.lens6, l)
		}
	}
	return idx
}

// lookup finds the longest announced prefix that contains p (never one longer
// than p: a row broader than the announcement says nothing about all of it).
func (idx *announcedIndex) lookup(p netip.Prefix) (announcedEntry, bool) {
	lens := idx.lens6
	if p.Addr().Is4() {
		lens = idx.lens4
	}
	for _, l := range lens {
		if l > p.Bits() {
			continue
		}
		q, err := p.Addr().Prefix(l)
		if err != nil {
			continue
		}
		if e, ok := idx.byPrefix[q]; ok {
			return e, true
		}
	}
	return announcedEntry{}, false
}

// insideOwn reports whether a shorter non-ambiguous announced prefix of the
// same ASN contains p.
func (idx *announcedIndex) insideOwn(p netip.Prefix, asn int64) bool {
	lens := idx.lens6
	if p.Addr().Is4() {
		lens = idx.lens4
	}
	for _, l := range lens {
		if l >= p.Bits() {
			continue
		}
		q, err := p.Addr().Prefix(l)
		if err != nil {
			continue
		}
		if e, ok := idx.byPrefix[q]; ok && !e.ambiguous && e.asn == asn {
			return true
		}
	}
	return false
}

// prefixWeight is the number of addresses a feed row stands for: IPv4
// addresses, or /64s for IPv6 (a row narrower than a /64 counts one).
func prefixWeight(p netip.Prefix) float64 {
	if p.Addr().Is4() {
		return math.Ldexp(1, 32-p.Bits())
	}
	if p.Bits() > 64 {
		return 1
	}
	return math.Ldexp(1, 64-p.Bits())
}

// attributed is a feed row given to an ASN. Compact on purpose (millions are
// kept): the city is an id in the city table (0: the row has no city) and the
// weight is computed from the prefix when needed.
type attributed struct {
	asn     int64
	city    uint32
	country [2]byte
	state   [2]byte // the US state of the row's region (feedState), or zero
}

// cityTable gives each distinct city spelling one id (ids start at 1).
type cityTable struct {
	names []string
	ids   map[string]uint32
}

func (t *cityTable) id(name string) uint32 {
	if id, ok := t.ids[name]; ok {
		return id
	}
	t.names = append(t.names, name)
	id := uint32(len(t.names))
	t.ids[name] = id
	return id
}

func (t *cityTable) name(id uint32) string { return t.names[id-1] }

// parseFeedPrefix reads a prefix or a single address of either family.
func parseFeedPrefix(text string) (netip.Prefix, bool) {
	text = strings.TrimSpace(text)
	p, err := netip.ParsePrefix(text)
	if err != nil {
		addr, aerr := netip.ParseAddr(text)
		if aerr != nil {
			return netip.Prefix{}, false
		}
		p = netip.PrefixFrom(addr, addr.BitLen())
	}
	if !p.IsValid() || p.Addr().Is4In6() {
		return netip.Prefix{}, false
	}
	return p.Masked(), true
}

func validCountry(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// regionMinShare is the share of a city's weight that one state needs for a derived layer
// (38-asn-geofeed, 68-cloud-regions) to write that state in the region column: the city is the
// layer's decision, the state only says which place of that name the geofeed rows meant.
const regionMinShare = 0.8

// feedState turns the region column of a geofeed row into the US state that tells which place of
// the city's name is meant: for the country US a region "US-XX" or a bare "XX" (two ASCII letters,
// any case, white space around it ignored) gives XX in upper case; every other country, and any
// other text, gives "". It is the rule of regionAdmin1 in mmdbbuilder, which places a layer row's
// city in that state, and the two must stay equal (hatchet does not import mmdbbuilder). The
// region is untrusted row data: nothing but this result is ever used.
func feedState(country, region string) string {
	if strings.ToUpper(strings.TrimSpace(country)) != "US" {
		return ""
	}
	r := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(region)), "US-")
	if len(r) != 2 || r[0] < 'A' || r[0] > 'Z' || r[1] < 'A' || r[1] > 'Z' {
		return ""
	}
	return r
}

// stateBytes packs a state of feedState (or "") into the two bytes the row structs keep.
func stateBytes(state string) [2]byte {
	if len(state) != 2 {
		return [2]byte{}
	}
	return [2]byte{state[0], state[1]}
}

// topState is the state a derived layer writes for a city: the state with the most weight when
// it holds at least regionMinShare of whole, the city's weight in that decision (rows without a
// state are part of it, so they count against every state); else "". Ties: the smaller code.
func topState[N uint64 | float64](states map[[2]byte]N, whole N) string {
	var best [2]byte
	var top N
	for st, n := range states {
		if n > top || (n == top && string(st[:]) < string(best[:])) {
			best, top = st, n
		}
	}
	if top <= 0 || whole <= 0 || float64(top)/float64(whole) < regionMinShare {
		return ""
	}
	return string(best[:])
}

// regionColumn is the text of the region column for a state of topState: "US-XX", or "".
func regionColumn(state string) string {
	if state == "" {
		return ""
	}
	return "US-" + state
}

// normCity makes the spelling of a city comparable: case and spacing only.
func normCity(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

type cityKey struct{ country, city string }

type cityTally struct {
	weight float64
	rows   int
	forms  map[string]int      // spellings as written
	states map[[2]byte]float64 // weight of the rows that name a US state
}

type familyTally struct {
	total  float64
	cities map[cityKey]*cityTally
}

type asnFamily struct {
	asn int64
	v6  bool
}

// asnFeedRows is the 38-asn-geofeed layer: it reads the geofeed layer file
// (prefix,country,region,city,source; columns found by header), gives each
// row with a 2-letter country to the ASN of the longest announced
// prefix that contains it, finds each ASN's dominant city per address family
// (see domMinShare and domMinRows; rows without a city count in the total weight) and writes one row per announced prefix of
// that ASN in that family (source asn:<number>; region column US-XX when the rows of
// the city name that state, see regionMinShare, else empty). It returns the number of
// ASNs and rows written and the number of decisions (an ASN and a family) that got a
// state. A prefix announced by more than one ASN is ignored
// entirely. Exact duplicate feed prefixes count once, the last row wins (as in
// the build); rows that overlap otherwise are counted as they are. Pure: no
// database, the announced prefixes are an argument.
func asnFeedRows(feedsPath string, announced []asnPrefix, w *csv.Writer) (asns, rows, states int, err error) {
	idx := newAnnouncedIndex(announced)
	feeds, cities, err := readAttributed(feedsPath, idx)
	if err != nil {
		return 0, 0, 0, err
	}

	tallies := map[asnFamily]*familyTally{}
	for p, a := range feeds {
		key := asnFamily{a.asn, p.Addr().Is6()}
		t := tallies[key]
		if t == nil {
			t = &familyTally{cities: map[cityKey]*cityTally{}}
			tallies[key] = t
		}
		weight := prefixWeight(p)
		t.total += weight // rows without a city count too: the share of "no city"
		if a.city == 0 {
			continue
		}
		city := cities.name(a.city)
		ck := cityKey{string(a.country[:]), normCity(city)}
		ct := t.cities[ck]
		if ct == nil {
			ct = &cityTally{forms: map[string]int{}}
			t.cities[ck] = ct
		}
		ct.weight += weight
		ct.rows++
		ct.forms[city]++
		if a.state != ([2]byte{}) {
			if ct.states == nil {
				ct.states = map[[2]byte]float64{}
			}
			ct.states[a.state] += weight
		}
	}

	// The announced weight of each (ASN, family): the union of its non-ambiguous prefixes
	// (one inside another prefix of the same ASN counts once, through the outer one).
	announcedWeight := map[asnFamily]float64{}
	for p, e := range idx.byPrefix {
		if !e.ambiguous && !idx.insideOwn(p, e.asn) {
			announcedWeight[asnFamily{e.asn, p.Addr().Is6()}] += prefixWeight(p)
		}
	}

	type verdict struct{ country, city, state string }
	dominant := map[asnFamily]verdict{}
	for key, t := range tallies {
		var best cityKey
		var bestTally *cityTally
		for ck, ct := range t.cities {
			if bestTally == nil || ct.weight > bestTally.weight ||
				(ct.weight == bestTally.weight && (ck.country < best.country || (ck.country == best.country && ck.city < best.city))) {
				best, bestTally = ck, ct
			}
		}
		if bestTally == nil || bestTally.rows < domMinRows || t.total <= 0 || bestTally.weight/t.total < domMinShare {
			continue
		}
		if aw := announcedWeight[key]; aw <= 0 || t.total/aw < domMinCoverage {
			continue
		}
		// The spelling written most often (ties: the smallest string).
		var form string
		for f, n := range bestTally.forms {
			if form == "" || n > bestTally.forms[form] || (n == bestTally.forms[form] && f < form) {
				form = f
			}
		}
		state := topState(bestTally.states, bestTally.weight)
		if state != "" {
			states++
		}
		dominant[key] = verdict{best.country, form, state}
	}
	if len(dominant) == 0 {
		return 0, 0, 0, nil
	}

	// One row per non-ambiguous announced prefix of each dominant (ASN, family).
	var out []asnPrefix
	for p, e := range idx.byPrefix {
		if !e.ambiguous {
			if _, ok := dominant[asnFamily{e.asn, p.Addr().Is6()}]; ok {
				out = append(out, asnPrefix{e.asn, p})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ASN != b.ASN {
			return a.ASN < b.ASN
		}
		if c := a.Prefix.Addr().Compare(b.Prefix.Addr()); c != 0 {
			return c < 0
		}
		return a.Prefix.Bits() < b.Prefix.Bits()
	})
	var last int64 = -1
	for _, a := range out {
		v := dominant[asnFamily{a.ASN, a.Prefix.Addr().Is6()}]
		w.Write([]string{a.Prefix.String(), v.country, regionColumn(v.state), v.city, "asn:" + strconv.FormatInt(a.ASN, 10)})
		if a.ASN != last {
			asns++
			last = a.ASN
		}
		rows++
	}
	return asns, rows, states, nil
}

// readAttributed reads the feed file and returns the rows that belong to an
// ASN, one per exact prefix (the last row of a prefix wins; one without a city
// is kept with city id 0, one with a bad country is left out).
func readAttributed(feedsPath string, idx *announcedIndex) (map[netip.Prefix]attributed, *cityTable, error) {
	f, err := os.Open(feedsPath)
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
	regionCol := slices.Index(header, "region") // optional: without it no row has a state
	feeds := map[netip.Prefix]attributed{}
	cities := &cityTable{ids: map[string]uint32{}}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return feeds, cities, nil
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
		if !ok {
			continue
		}
		country := strings.ToUpper(strings.TrimSpace(field(countryCol)))
		city := strings.TrimSpace(field(cityCol))
		if !validCountry(country) {
			delete(feeds, p)
			continue
		}
		e, found := idx.lookup(p)
		if !found || e.ambiguous {
			delete(feeds, p)
			continue
		}
		var id uint32
		if city != "" {
			id = cities.id(city)
		}
		var state [2]byte
		if id != 0 && regionCol >= 0 {
			state = stateBytes(feedState(country, field(regionCol)))
		}
		feeds[p] = attributed{e.asn, id, [2]byte{country[0], country[1]}, state}
	}
}
