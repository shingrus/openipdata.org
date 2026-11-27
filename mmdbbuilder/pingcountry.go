package main

// The ping country check (mmdbbuilder geo -ping-country): a speed-of-light test of the country
// of IPv4 /24s from ping times measured from cloud regions to the .1 of every /24 of hosting
// networks.
//
// Each usable time is a disk: the centre is the measuring site's GeoNames place, the radius is
// rtt * pingKmPerMs + pingMarginKm. The GeoNames places are grouped into cells of a quarter of a
// degree per country; a cell is possible when its point lies inside every disk of the /24. A
// country with cells but no possible cell is impossible for the /24. It is then replaced by the
// country that holds at least half of the weight of all possible cells (the weight of a cell is
// the population of its places). When no country is that clear, the country stays.
//
// It runs right after the RTT country check and before the guesses (geo.go), so that the guesses
// fill cities in the corrected country. The file is another tool's output (a gzipped CSV: the
// column prefix, then one column per site named <CC>/<City>, the round-trip time in ms or an
// empty cell) and is untrusted; it is streamed, never held in memory. A country with no cell is
// never impossible. IPv4 only.
//
// Speed: a point is inside a disk when the dot product of the two unit vectors is at least the
// cosine of the disk's angle, so no trigonometry runs per cell. The distance from each site to
// the nearest cell of each country is cached, which rules most impossible countries out without
// a scan; a country that may be possible is scanned only in the latitude band of the smallest
// disk, and the scan stops at the first possible cell.

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
	"sort"
	"strconv"
	"strings"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/inserter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

const (
	pingKmPerMs   = 100.0 // km one way per ms of round-trip time
	pingMarginKm  = 100.0 // added to every radius
	pingMaxMs     = 2000.0
	pingMinSites  = 3   // usable times a /24 needs
	pingMinShare  = 0.5 // the share of the possible weight the new country needs
	pingMinColumn = 3   // usable site columns the file needs
	pingEarthKm   = 6371.0
	pingUnset     = -3.0 // a dot product that is not computed yet
)

// pingCounts is what the check did.
type pingCounts struct {
	rows, skipped int
	changed24     int // /24s where at least one part changed country
	partsChanged  int
	ignored       int  // site columns that were not used
	skippedFile   bool // fewer than pingMinColumn usable site columns: nothing was checked
}

func (c pingCounts) String() string {
	return fmt.Sprintf("ping-country: %d rows, %d skipped, %d /24s, %d /24s changed, %d parts changed",
		c.rows, c.skipped, c.rows, c.changed24, c.partsChanged)
}

// pingCell is a quarter-degree cell of the places of one country.
type pingCell struct {
	lat, lon float64 // degrees
	x, y, z  float64 // unit vector of the point
	w        float64 // weight
	cc       int32
}

func unitVec(lat, lon float64) (x, y, z float64) {
	la, lo := lat*math.Pi/180, lon*math.Pi/180
	c := math.Cos(la)
	return c * math.Cos(lo), c * math.Sin(lo), math.Sin(la)
}

// pingSite is a measuring site that GeoNames places.
type pingSite struct {
	lat, lon float64
	x, y, z  float64
}

// pingWorld is the cell index.
type pingWorld struct {
	codes     []string         // country codes, sorted
	ccIdx     map[string]int32 // code -> index in codes
	cells     []pingCell       // sorted by country, then latitude
	start     []int32          // cells of country c are cells[start[c]:start[c+1]]
	gridStart []int32          // grid of pingGrid degree squares: bucket b holds gridCells[gridStart[b]:gridStart[b+1]]
	gridCells []int32          // indexes into cells
	nrst      []float64        // [country*len(sites)+site]: largest dot product to a cell, or pingUnset
	sites     []pingSite
	// scratch of one pingTop call
	sums    []float64
	touched []int32
}

// newPingWorld builds the cells from the index once. The index holds a place under several keys,
// so places are deduplicated by their GeoNames id. A cell's point is its most populous place
// (ties: the smaller id) and its weight the sum of the populations, at least 1.
func newPingWorld(index geoNames, sites []pingSite) *pingWorld {
	type ckey struct {
		cc     string
		la, lo int32
	}
	type acc struct {
		sum, bestPop int64
		bestID       uint32
		lat, lon     float64
	}
	cells := map[ckey]*acc{}
	seen := map[uint32]bool{}
	for key, p := range index {
		// The cells are made from the plain keys only, so they are the same with or without
		// the regional places.
		cc, ok := plainKeyCountry(key)
		if !ok || seen[p.id] {
			continue
		}
		seen[p.id] = true
		k := ckey{cc, int32(math.Round(p.lat * 4)), int32(math.Round(p.lon * 4))}
		a := cells[k]
		if a == nil {
			cells[k] = &acc{sum: p.pop, bestPop: p.pop, bestID: p.id, lat: p.lat, lon: p.lon}
			continue
		}
		a.sum += p.pop
		if p.pop > a.bestPop || p.pop == a.bestPop && p.id < a.bestID {
			a.bestPop, a.bestID, a.lat, a.lon = p.pop, p.id, p.lat, p.lon
		}
	}
	w := &pingWorld{ccIdx: map[string]int32{}, sites: sites}
	for k := range cells {
		if _, ok := w.ccIdx[k.cc]; !ok {
			w.ccIdx[k.cc] = 0
			w.codes = append(w.codes, k.cc)
		}
	}
	slices.Sort(w.codes)
	for i, c := range w.codes {
		w.ccIdx[c] = int32(i)
	}
	w.cells = make([]pingCell, 0, len(cells))
	for k, a := range cells {
		x, y, z := unitVec(a.lat, a.lon)
		w.cells = append(w.cells, pingCell{lat: a.lat, lon: a.lon, x: x, y: y, z: z, w: float64(max(a.sum, 1)), cc: w.ccIdx[k.cc]})
	}
	slices.SortFunc(w.cells, func(a, b pingCell) int {
		switch {
		case a.cc != b.cc:
			return int(a.cc - b.cc)
		case a.lat != b.lat:
			if a.lat < b.lat {
				return -1
			}
			return 1
		case a.x != b.x:
			if a.x < b.x {
				return -1
			}
			return 1
		case a.y != b.y:
			if a.y < b.y {
				return -1
			}
			return 1
		}
		return 0
	})
	w.start = make([]int32, len(w.codes)+1)
	for _, c := range w.cells {
		w.start[c.cc+1]++
	}
	for i := 1; i < len(w.start); i++ {
		w.start[i] += w.start[i-1]
	}
	w.gridStart = make([]int32, pingRows*pingCols+1)
	for _, c := range w.cells {
		w.gridStart[gridBucket(c.lat, c.lon)+1]++
	}
	for i := 1; i < len(w.gridStart); i++ {
		w.gridStart[i] += w.gridStart[i-1]
	}
	w.gridCells = make([]int32, len(w.cells))
	fill := slices.Clone(w.gridStart[:len(w.gridStart)-1])
	for i, c := range w.cells {
		b := gridBucket(c.lat, c.lon)
		w.gridCells[fill[b]] = int32(i)
		fill[b]++
	}
	w.nrst = make([]float64, len(w.codes)*len(sites))
	for i := range w.nrst {
		w.nrst[i] = pingUnset
	}
	w.sums = make([]float64, len(w.codes))
	return w
}

// The grid of squares of pingGrid degrees that finds the cells near a site.
const (
	pingGrid = 2.0
	pingRows = 90
	pingCols = 180
)

func gridRow(lat float64) int { return min(max(int(math.Floor((lat+90)/pingGrid)), 0), pingRows-1) }

func gridBucket(lat, lon float64) int {
	col := min(max(int(math.Floor((lon+180)/pingGrid)), 0), pingCols-1)
	return gridRow(lat)*pingCols + col
}

// pingDisk is one usable time: a site and the angle of the disk (the time is kept for the
// ping city check).
type pingDisk struct {
	site  int32
	theta float64 // radians, at most pi
	cosT  float64 // cos(theta); -2 when the disk is the whole earth
	rtt   float64 // ms
}

func newDisk(site int32, rtt float64) pingDisk {
	theta := (rtt*pingKmPerMs + pingMarginKm) / pingEarthKm
	if theta >= math.Pi {
		return pingDisk{site, math.Pi, -2, rtt}
	}
	return pingDisk{site, theta, math.Cos(theta), rtt}
}

// inside says whether the cell's point lies in every disk.
func (w *pingWorld) inside(c *pingCell, disks []pingDisk) bool {
	for i := range disks {
		s := &w.sites[disks[i].site]
		if c.x*s.x+c.y*s.y+c.z*s.z < disks[i].cosT {
			return false
		}
	}
	return true
}

// nearest is the largest dot product of the site and a cell of the country (the nearest cell),
// cached. The cells are sorted by latitude, so each direction stops as soon as the latitude
// difference alone is farther than the best so far.
func (w *pingWorld) nearest(c int32, site int32) float64 {
	slot := int(c)*len(w.sites) + int(site)
	if v := w.nrst[slot]; v != pingUnset {
		return v
	}
	cells := w.cells[w.start[c]:w.start[c+1]]
	s := &w.sites[site]
	j := sort.Search(len(cells), func(i int) bool { return cells[i].lat >= s.lat })
	best, limit := -2.0, math.Pi
	scan := func(i int) bool { // false: stop this direction
		if math.Abs(cells[i].lat-s.lat)*math.Pi/180 > limit {
			return false
		}
		if d := cells[i].x*s.x + cells[i].y*s.y + cells[i].z*s.z; d > best {
			best = d
			limit = math.Acos(math.Min(1, best)) + 1e-9
		}
		return true
	}
	for i := j; i < len(cells) && scan(i); i++ {
	}
	for i := j - 1; i >= 0 && scan(i); i-- {
	}
	w.nrst[slot] = best
	return best
}

// band returns the latitude limits in degrees of the smallest disk (disks are sorted by size).
func (w *pingWorld) band(disks []pingDisk) (lo, hi float64) {
	d := disks[0]
	if d.theta >= math.Pi {
		return -91, 91
	}
	deg := d.theta*180/math.Pi + 1e-6
	s := w.sites[d.site]
	return s.lat - deg, s.lat + deg
}

// possible says whether a cell of the country lies inside every disk.
func (w *pingWorld) possible(c int32, disks []pingDisk) bool {
	cells := w.cells[w.start[c]:w.start[c+1]]
	lo, hi := w.band(disks)
	for i := sort.Search(len(cells), func(i int) bool { return cells[i].lat >= lo }); i < len(cells) && cells[i].lat <= hi; i++ {
		if w.inside(&cells[i], disks) {
			return true
		}
	}
	return false
}

// top finds the country that holds the most possible weight (ties: the smaller code). It is ""
// when no cell is possible or the best country holds less than pingMinShare of the weight.
func (w *pingWorld) top(disks []pingDisk) string {
	w.touched = w.touched[:0]
	total := 0.0
	// Only the grid squares that the smallest disk touches are scanned.
	d := disks[0]
	s := w.sites[d.site]
	latLo, latHi := s.lat-d.theta*180/math.Pi-1e-6, s.lat+d.theta*180/math.Pi+1e-6
	allLon := d.theta >= math.Pi/2 || latLo <= -90 || latHi >= 90
	var dLon float64
	if !allLon {
		if r := math.Sin(d.theta) / math.Cos(s.lat*math.Pi/180); r < 1 {
			dLon = math.Asin(r)*180/math.Pi + 1e-6
		} else {
			allLon = true
		}
	}
	rowLo, rowHi := gridRow(max(latLo, -90)), gridRow(min(latHi, 90))
	colLo, cols := 0, pingCols
	if !allLon && dLon*2 < 360-2*pingGrid {
		colLo = int(math.Floor((s.lon - dLon + 180) / pingGrid))
		cols = int(math.Floor((s.lon+dLon+180)/pingGrid)) - colLo + 1
	}
	for row := rowLo; row <= rowHi; row++ {
		for k := 0; k < cols; k++ {
			b := row*pingCols + ((colLo+k)%pingCols+pingCols)%pingCols
			for _, ci := range w.gridCells[w.gridStart[b]:w.gridStart[b+1]] {
				c := &w.cells[ci]
				if !w.inside(c, disks) {
					continue
				}
				if w.sums[c.cc] == 0 {
					w.touched = append(w.touched, c.cc)
				}
				w.sums[c.cc] += c.w
				total += c.w
			}
		}
	}
	best := int32(-1)
	for _, c := range w.touched {
		if best < 0 || w.sums[c] > w.sums[best] || w.sums[c] == w.sums[best] && c < best {
			best = c
		}
	}
	top := ""
	if best >= 0 && w.sums[best] >= pingMinShare*total {
		top = w.codes[best]
	}
	for _, c := range w.touched {
		w.sums[c] = 0
	}
	return top
}

// pingChecker decides the countries of one /24. Disks are sorted by size, smallest first.
type pingChecker struct {
	w      *pingWorld
	disks  []pingDisk
	top    string
	topSet bool
}

func (k *pingChecker) reset(disks []pingDisk) { k.disks, k.topSet = disks, false }

// correct returns the country the /24 should have instead of x, or "" to keep x.
func (k *pingChecker) correct(x string) string {
	c, ok := k.w.ccIdx[x]
	if !ok {
		return "" // no cell: never impossible
	}
	possible := true
	for _, d := range k.disks {
		if k.w.nearest(c, d.site) < d.cosT {
			possible = false // no cell of the country is even inside this one disk
			break
		}
	}
	if possible && k.w.possible(c, k.disks) {
		return ""
	}
	if !k.topSet {
		k.top, k.topSet = k.w.top(k.disks), true
	}
	if k.top == x {
		return ""
	}
	return k.top
}

// sortDisks puts the smallest disk first (ties: the smaller site).
func sortDisks(disks []pingDisk) {
	slices.SortFunc(disks, func(a, b pingDisk) int {
		switch {
		case a.theta < b.theta:
			return -1
		case a.theta > b.theta:
			return 1
		}
		return int(a.site - b.site)
	})
}

// errFewPingSites: fewer than pingMinColumn site columns are usable; the check is skipped.
var errFewPingSites = errors.New("too few usable site columns")

// pingReader streams the file.
type pingReader struct {
	f       *os.File
	gz      *gzip.Reader
	r       *csv.Reader
	prefix  int   // column of the prefix
	cols    []int // column of each site
	sites   []pingSite
	width   int
	skipped int
	ignored int
	disks   []pingDisk
}

func (p *pingReader) Close() {
	p.gz.Close()
	p.f.Close()
}

// openPing reads the header: the column prefix and the site columns <CC>/<City> that GeoNames
// places. Any other column is ignored and counted.
func openPing(path string, index geoNames) (*pingReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		f.Close()
		return nil, err
	}
	p := &pingReader{f: f, gz: gz, prefix: -1}
	fail := func(err error) (*pingReader, error) { p.Close(); return nil, err }
	p.r = csv.NewReader(gz)
	p.r.FieldsPerRecord = -1
	p.r.ReuseRecord = true
	header, err := p.r.Read()
	if err != nil {
		return fail(fmt.Errorf("read header: %w", err))
	}
	p.width = len(header)
	seen := map[string]bool{}
	for i, name := range header {
		name = strings.TrimSpace(name)
		if name == "prefix" && p.prefix < 0 {
			p.prefix = i
			continue
		}
		cc, city, ok := strings.Cut(name, "/")
		cc, city = strings.ToUpper(strings.TrimSpace(cc)), cleanCity(city)
		var place gnPlace
		if ok && len(cc) == 2 && validCountries[cc] && city != "" && !seen[cc+"/"+city] {
			place, ok = index.place(cc, city)
		} else {
			ok = false
		}
		if !ok {
			p.ignored++
			continue
		}
		seen[cc+"/"+city] = true
		x, y, z := unitVec(place.lat, place.lon)
		p.cols = append(p.cols, i)
		p.sites = append(p.sites, pingSite{place.lat, place.lon, x, y, z})
	}
	if p.prefix < 0 {
		return fail(fmt.Errorf("header %q has no prefix column", strings.Join(header, ",")))
	}
	if len(p.sites) < pingMinColumn {
		// Not an error: a site name GeoNames no longer places must not fail the whole build.
		p.Close()
		return nil, errFewPingSites
	}
	return p, nil
}

// next returns the next usable row: the /24 (its first address) and its disks, smallest first
// (valid until the next call). ok is false at the end of the file.
func (p *pingReader) next() (client uint32, disks []pingDisk, ok bool, err error) {
	for {
		rec, err := p.r.Read()
		if err == io.EOF {
			return 0, nil, false, nil
		}
		if err != nil {
			var parseErr *csv.ParseError
			if errors.As(err, &parseErr) {
				p.skipped++
				continue
			}
			return 0, nil, false, err
		}
		if len(rec) != p.width {
			p.skipped++
			continue
		}
		pr, perr := netip.ParsePrefix(strings.TrimSpace(rec[p.prefix]))
		if perr != nil || !pr.Addr().Is4() || pr.Bits() != 24 || pr != pr.Masked() {
			p.skipped++
			continue
		}
		p.disks = p.disks[:0]
		for i, col := range p.cols {
			cell := strings.TrimSpace(rec[col])
			if cell == "" {
				continue
			}
			v, verr := strconv.ParseFloat(cell, 64)
			if verr != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > pingMaxMs {
				continue
			}
			p.disks = append(p.disks, newDisk(int32(i), v))
		}
		if len(p.disks) < pingMinSites {
			p.skipped++
			continue
		}
		sortDisks(p.disks)
		return addrNumber(pr.Addr()), p.disks, true, nil
	}
}

// applyPingCountry runs the check on the tree. recordFor gives the shared country-only record of
// a country (the one any country-only layer row of it uses).
func applyPingCountry(tree *mmdbwriter.Tree, path string, index geoNames, debug bool, recordFor func(place, *geoLayerStats) int, records func(int) mmdbtype.Map) (pingCounts, error) {
	var counts pingCounts
	p, err := openPing(path, index)
	if errors.Is(err, errFewPingSites) {
		counts.skippedFile = true
		return counts, nil
	}
	if err != nil {
		return counts, err
	}
	defer p.Close()
	counts.ignored = p.ignored
	checker := &pingChecker{w: newPingWorld(index, p.sites)}
	var dummy geoLayerStats
	for {
		client, disks, ok, err := p.next()
		counts.skipped = p.skipped
		if err != nil {
			return counts, err
		}
		if !ok {
			return counts, nil
		}
		counts.rows++
		checker.reset(disks)
		prefix := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(client >> 24), byte(client >> 16), byte(client >> 8), 0}), 24)
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
				y = checker.correct(x)
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
			return counts, fmt.Errorf("insert %s (ping-country): %w", prefix, err)
		}
		if changed > 0 {
			counts.changed24++
			counts.partsChanged += changed
		}
	}
}
