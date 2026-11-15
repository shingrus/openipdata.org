// Command mapdata turns the Natural Earth 1:50m "Admin 0 - Countries" GeoJSON
// into site/static/world.bin, the one compact file of country outlines the
// website draws its map from (see README.md for the source, the licence and the
// command). Standard library only.
//
// File format "OIW1" (all numbers little-endian; the reader is site/world.go and
// must stay in step with this comment):
//
//	magic      4 bytes "OIW1"
//	countries  uint16
//	country, repeated:
//	  code       2 bytes, ISO 3166-1 alpha-2 in upper case, or 0x00 0x00 when the
//	             entry has no valid code (it is still drawn as land)
//	  label      int16 lon, int16 lat (the place for a name; units of 0.01 degree)
//	  bbox       int16 minLon, minLat, maxLon, maxLat (units of 0.01 degree; of
//	             the kept polygons, or of the label point when none is kept)
//	  polygons   uint16
//	  polygon, repeated (the outer ring only, holes are dropped):
//	    points     uvarint, at least 3
//	    first      int16 lon, int16 lat (units of 0.01 degree)
//	    others     per point a zigzag varint dlon and a zigzag varint dlat, the
//	               difference from the point before (the ring is closed
//	               implicitly: the last point is not repeated)
//	crc32      uint32, IEEE, of every byte before it
//
// Every ring is counter-clockwise (positive area in longitude/latitude), so one
// path with the non-zero fill rule never makes a hole where two meet.
// Longitudes are in -180..180 and latitudes in -90..90. A polygon that crosses
// the antimeridian is already split there by Natural Earth.
//
// Simplification, once, here: Douglas-Peucker in degrees with -tol (default
// 0.08, about one pixel at the frame the site draws: 46 x 18.2 degrees on about
// 500 pixels), then the points are rounded to 0.01 degree. Rings that collapse
// (under 3 distinct points) or are smaller than -minarea square degrees are
// dropped, except that a country always keeps its largest ring (simplified with
// a smaller tolerance when needed), so small island states still exist.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"sort"
	"strings"
)

const unit = 100 // file units per degree

type point struct{ x, y float64 }

type country struct {
	code     string // "" = none
	lx, ly   float64
	adm      string
	polygons [][]point // outer rings, closed implicitly, counter-clockwise
}

type feature struct {
	Properties struct {
		ISOA2EH string  `json:"ISO_A2_EH"`
		ISOA2   string  `json:"ISO_A2"`
		ADM0A3  string  `json:"ADM0_A3"`
		LabelX  float64 `json:"LABEL_X"`
		LabelY  float64 `json:"LABEL_Y"`
		Name    string  `json:"NAME"`
	} `json:"properties"`
	Geometry struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	} `json:"geometry"`
}

// outerRings returns the outer ring of every polygon of a geometry.
func outerRings(typ string, raw json.RawMessage) ([][]point, error) {
	var polys [][][][2]float64
	switch typ {
	case "Polygon":
		var p [][][2]float64
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		polys = [][][][2]float64{p}
	case "MultiPolygon":
		if err := json.Unmarshal(raw, &polys); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("geometry %q", typ)
	}
	var out [][]point
	for _, p := range polys {
		if len(p) == 0 {
			continue
		}
		var ring []point
		for _, c := range p[0] {
			ring = append(ring, point{c[0], c[1]})
		}
		if n := len(ring); n > 1 && ring[0] == ring[n-1] {
			ring = ring[:n-1]
		}
		out = append(out, ring)
	}
	return out, nil
}

func validCode(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// area is the signed area of the ring (positive = counter-clockwise).
func area(r []point) float64 {
	var a float64
	for i, p := range r {
		q := r[(i+1)%len(r)]
		a += p.x*q.y - q.x*p.y
	}
	return a / 2
}

func segDist(p, a, b point) float64 {
	dx, dy := b.x-a.x, b.y-a.y
	l := dx*dx + dy*dy
	if l == 0 {
		return math.Hypot(p.x-a.x, p.y-a.y)
	}
	t := ((p.x-a.x)*dx + (p.y-a.y)*dy) / l
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(p.x-(a.x+t*dx), p.y-(a.y+t*dy))
}

// dp keeps the points of line (an open polyline) that Douglas-Peucker keeps for
// the tolerance tol; the first and last point are always kept.
func dp(line []point, tol float64) []point {
	if len(line) <= 2 {
		return line
	}
	keep := make([]bool, len(line))
	keep[0], keep[len(line)-1] = true, true
	var rec func(lo, hi int)
	rec = func(lo, hi int) {
		worst, at := 0.0, -1
		for i := lo + 1; i < hi; i++ {
			if d := segDist(line[i], line[lo], line[hi]); d > worst {
				worst, at = d, i
			}
		}
		if at >= 0 && worst > tol {
			keep[at] = true
			rec(lo, at)
			rec(at, hi)
		}
	}
	rec(0, len(line)-1)
	var out []point
	for i, k := range keep {
		if k {
			out = append(out, line[i])
		}
	}
	return out
}

// simplifyRing simplifies a closed ring (given without the repeated first
// point): it is cut at its first point and at the point farthest from it, and
// both halves go through dp, so a ring never collapses to a line because of
// where it happened to start; a cut point that is not a corner is dropped again.
func simplifyRing(r []point, tol float64) []point {
	if len(r) < 4 {
		return r
	}
	far, best := 0, -1.0
	for i, p := range r {
		if d := math.Hypot(p.x-r[0].x, p.y-r[0].y); d > best {
			far, best = i, d
		}
	}
	a := dp(r[:far+1], tol)
	b := dp(append(append([]point{}, r[far:]...), r[0]), tol)
	out := append(append([]point{}, a[:len(a)-1]...), b[:len(b)-1]...)
	// The two cut points were kept only as ends of a half; drop each one that
	// lies on the line between its neighbours (the later one first).
	for _, i := range []int{len(a) - 1, 0} {
		if n := len(out); n > 3 && segDist(out[i], out[(i+n-1)%n], out[(i+1)%n]) <= tol {
			out = append(out[:i], out[i+1:]...)
		}
	}
	return out
}

// quantise rounds to file units, drops points that became equal to the one
// before (and to the first), and makes the ring counter-clockwise. It returns
// nil when fewer than 3 distinct points are left or the area is zero.
func quantise(r []point) []point {
	var out []point
	for _, p := range r {
		q := point{math.Round(p.x * unit), math.Round(p.y * unit)}
		if len(out) == 0 || out[len(out)-1] != q {
			out = append(out, q)
		}
	}
	for len(out) > 1 && out[0] == out[len(out)-1] {
		out = out[:len(out)-1]
	}
	if len(out) < 3 || area(out) == 0 {
		return nil
	}
	if area(out) < 0 {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// buildCountry simplifies the outer rings of one country: rings that collapse
// or have less than minArea square degrees are dropped, except the largest
// ring, which is kept whatever it takes (smaller tolerances until it survives).
func buildCountry(c *country, rings [][]point, tol, minArea float64) {
	largest, largestArea := -1, 0.0
	for i, r := range rings {
		if a := math.Abs(area(r)); a > largestArea || largest < 0 {
			largest, largestArea = i, a
		}
	}
	for i, r := range rings {
		q := quantise(simplifyRing(r, tol))
		if q != nil && area(q)/(unit*unit) < minArea {
			q = nil
		}
		if q == nil && i == largest {
			for t := tol / 2; q == nil && t > 1e-6; t /= 2 {
				q = quantise(simplifyRing(r, t))
			}
			if q == nil {
				q = quantise(r) // the original points, only rounded
			}
		}
		if q != nil {
			c.polygons = append(c.polygons, q)
		}
	}
}

func readCountries(data []byte, tol, minArea float64) ([]country, error) {
	var fc struct {
		Features []feature `json:"features"`
	}
	if err := json.Unmarshal(data, &fc); err != nil {
		return nil, err
	}
	if len(fc.Features) == 0 {
		return nil, errors.New("no features")
	}
	var out []country
	for _, f := range fc.Features {
		p := f.Properties
		code := p.ISOA2EH
		if !validCode(code) {
			code = ""
		}
		rings, err := outerRings(f.Geometry.Type, f.Geometry.Coordinates)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
		c := country{code: code, lx: p.LabelX, ly: p.LabelY, adm: p.ADM0A3}
		buildCountry(&c, rings, tol, minArea)
		out = append(out, c)
	}
	// A code used by several entries (the Indian Ocean Territories carry AU)
	// belongs to the entry whose own ADM0_A3 is that country's: the others lose
	// the code and stay as plain land.
	byCode := map[string][]int{}
	for i, c := range out {
		if c.code != "" {
			byCode[c.code] = append(byCode[c.code], i)
		}
	}
	for code, idx := range byCode {
		if len(idx) < 2 {
			continue
		}
		best := idx[0]
		for _, i := range idx {
			if strings.HasPrefix(out[i].adm, code) || (!strings.HasPrefix(out[best].adm, code) && len(out[i].polygons) > len(out[best].polygons)) {
				best = i
			}
		}
		for _, i := range idx {
			if i != best {
				out[i].code = ""
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].code < out[j].code })
	return out, nil
}

func i16(v float64) (int16, error) {
	r := math.Round(v * unit)
	if r < math.MinInt16 || r > math.MaxInt16 {
		return 0, fmt.Errorf("%v out of range", v)
	}
	return int16(r), nil
}

// encode writes the file (format at the top).
func encode(cs []country) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("OIW1")
	if len(cs) > math.MaxUint16 {
		return nil, errors.New("too many countries")
	}
	binary.Write(&b, binary.LittleEndian, uint16(len(cs)))
	tmp := make([]byte, binary.MaxVarintLen64)
	for _, c := range cs {
		if c.code == "" {
			b.Write([]byte{0, 0})
		} else {
			b.WriteString(c.code)
		}
		minx, miny, maxx, maxy := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
		for _, r := range c.polygons {
			for _, p := range r {
				minx, maxx = math.Min(minx, p.x), math.Max(maxx, p.x)
				miny, maxy = math.Min(miny, p.y), math.Max(maxy, p.y)
			}
		}
		if len(c.polygons) == 0 {
			minx, maxx, miny, maxy = c.lx*unit, c.lx*unit, c.ly*unit, c.ly*unit
		}
		var vals []int16
		for _, v := range []float64{c.lx, c.ly, minx / unit, miny / unit, maxx / unit, maxy / unit} {
			n, err := i16(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", c.code, err)
			}
			vals = append(vals, n)
		}
		binary.Write(&b, binary.LittleEndian, vals)
		if len(c.polygons) > math.MaxUint16 {
			return nil, errors.New("too many polygons")
		}
		binary.Write(&b, binary.LittleEndian, uint16(len(c.polygons)))
		for _, r := range c.polygons {
			b.Write(tmp[:binary.PutUvarint(tmp, uint64(len(r)))])
			binary.Write(&b, binary.LittleEndian, []int16{int16(r[0].x), int16(r[0].y)})
			for i := 1; i < len(r); i++ {
				b.Write(tmp[:binary.PutVarint(tmp, int64(r[i].x-r[i-1].x))])
				b.Write(tmp[:binary.PutVarint(tmp, int64(r[i].y-r[i-1].y))])
			}
		}
	}
	binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(b.Bytes()))
	return b.Bytes(), nil
}

func main() {
	in := flag.String("in", "", "Natural Earth ne_50m_admin_0_countries.geojson")
	out := flag.String("out", "", "world.bin to write")
	tol := flag.Float64("tol", 0.08, "Douglas-Peucker tolerance in degrees")
	minArea := flag.Float64("minarea", 0.02, "smallest ring kept, in square degrees (a country keeps its largest ring anyway)")
	flag.Parse()
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: mapdata -in countries.geojson -out world.bin [-tol 0.08] [-minarea 0.02]")
		os.Exit(2)
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mapdata:", err)
		os.Exit(1)
	}
	cs, err := readCountries(data, *tol, *minArea)
	if err == nil {
		var bin []byte
		if bin, err = encode(cs); err == nil {
			tmp := *out + ".tmp"
			if err = os.WriteFile(tmp, bin, 0o644); err == nil {
				err = os.Rename(tmp, *out)
			}
			sum := sha256.Sum256(data)
			polys, points := 0, 0
			for _, c := range cs {
				polys += len(c.polygons)
				for _, r := range c.polygons {
					points += len(r)
				}
			}
			fmt.Printf("input sha256 %s\n%d entries, %d polygons, %d points, %d bytes\n", hex.EncodeToString(sum[:]), len(cs), polys, points, len(bin))
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mapdata:", err)
		os.Exit(1)
	}
}
