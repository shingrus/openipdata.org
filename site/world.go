package main

// The map data: country outlines of Natural Earth 1:50m (public domain), made by
// tools/mapdata into static/world.bin, embedded here and decoded once at start.
//
// File format "OIW1" (little-endian; the writer is tools/mapdata/main.go and
// must stay in step with this comment):
//
//	magic      4 bytes "OIW1"
//	countries  uint16
//	country, repeated:
//	  code       2 bytes, ISO 3166-1 alpha-2 in upper case, or 0x00 0x00 when
//	             the entry has no valid code (it is still drawn as land)
//	  label      int16 lon, int16 lat (units of 0.01 degree)
//	  bbox       int16 minLon, minLat, maxLon, maxLat (units of 0.01 degree)
//	  polygons   uint16
//	  polygon, repeated (outer ring only, counter-clockwise, no holes):
//	    points     uvarint, at least 3
//	    first      int16 lon, int16 lat (units of 0.01 degree)
//	    others     per point a zigzag varint dlon and a zigzag varint dlat, the
//	               difference from the point before (the ring is closed
//	               implicitly: the last point is not repeated)
//	crc32      uint32, IEEE, of every byte before it
//
// Longitudes are in -180..180 and latitudes in -90..90.

import (
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

//go:embed static/world.bin
var worldBin []byte

const worldUnit = 100.0 // file units per degree

// wpoly is one outer ring: its points are pts[start:end] (lon, lat pairs in
// file units) and its bounding box in degrees.
type wpoly struct {
	start, end       int32
	minX, minY, maxX float64
	maxY             float64
}

type wcountry struct {
	code         string // "" = no valid code
	labelX       float64
	labelY       float64 // the label point, degrees
	minX, minY   float64
	maxX, maxY   float64 // bounding box, degrees
	first, count int32   // polygons world.polys[first:first+count]
}

type world struct {
	countries []wcountry
	polys     []wpoly
	pts       []int16
	byCode    [26 * 26]int32 // index in countries plus 1; 0 = none
}

func codeSlot(code string) int {
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return -1
	}
	return int(code[0]-'A')*26 + int(code[1]-'A')
}

// country returns the entry with this code, or nil.
func (w *world) country(code string) *wcountry {
	if w == nil {
		return nil
	}
	if s := codeSlot(code); s >= 0 {
		if i := w.byCode[s]; i > 0 {
			return &w.countries[i-1]
		}
	}
	return nil
}

var errWorld = errors.New("world.bin is not valid")

// byteReader reads the file; the first error sticks and every later read gives zero.
type byteReader struct {
	b   []byte
	pos int
	err error
}

func (r *byteReader) fail() {
	if r.err == nil {
		r.err = errWorld
	}
}

func (r *byteReader) take(n int) []byte {
	if r.err != nil || n < 0 || r.pos+n > len(r.b) {
		r.fail()
		return nil
	}
	s := r.b[r.pos : r.pos+n]
	r.pos += n
	return s
}

func (r *byteReader) u16() int {
	if s := r.take(2); s != nil {
		return int(binary.LittleEndian.Uint16(s))
	}
	return 0
}

func (r *byteReader) i16() int16 { return int16(r.u16()) }

func (r *byteReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b[r.pos:])
	if n <= 0 {
		r.fail()
		return 0
	}
	r.pos += n
	return v
}

func (r *byteReader) varint() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.b[r.pos:])
	if n <= 0 {
		r.fail()
		return 0
	}
	r.pos += n
	return v
}

// parseWorld decodes the file. Any damage (wrong magic, checksum, truncation, a
// point outside the globe, a repeated code) is an error.
func parseWorld(b []byte) (*world, error) {
	if len(b) < 10 || string(b[:4]) != "OIW1" {
		return nil, fmt.Errorf("%w: wrong header", errWorld)
	}
	body := b[:len(b)-4]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(b[len(b)-4:]) {
		return nil, fmt.Errorf("%w: checksum", errWorld)
	}
	r := &byteReader{b: body, pos: 4}
	n := r.u16()
	w := &world{countries: make([]wcountry, 0, n)}
	for ; n > 0 && r.err == nil; n-- {
		c := wcountry{}
		if code := r.take(2); code != nil && code[0] != 0 {
			c.code = string(code)
			if codeSlot(c.code) < 0 {
				r.fail()
			}
		}
		c.labelX, c.labelY = float64(r.i16())/worldUnit, float64(r.i16())/worldUnit
		c.minX, c.minY = float64(r.i16())/worldUnit, float64(r.i16())/worldUnit
		c.maxX, c.maxY = float64(r.i16())/worldUnit, float64(r.i16())/worldUnit
		c.first = int32(len(w.polys))
		for p := r.u16(); p > 0 && r.err == nil; p-- {
			points := r.uvarint()
			if points < 3 || points > uint64(len(body)) {
				r.fail()
				break
			}
			poly := wpoly{start: int32(len(w.pts) / 2)}
			x, y := int64(r.i16()), int64(r.i16())
			for i := uint64(0); i < points && r.err == nil; i++ {
				if i > 0 {
					x += r.varint()
					y += r.varint()
				}
				if x < -180*worldUnit || x > 180*worldUnit || y < -90*worldUnit || y > 90*worldUnit {
					r.fail()
					break
				}
				w.pts = append(w.pts, int16(x), int16(y))
				fx, fy := float64(x)/worldUnit, float64(y)/worldUnit
				if i == 0 {
					poly.minX, poly.maxX, poly.minY, poly.maxY = fx, fx, fy, fy
				}
				poly.minX, poly.maxX = min(poly.minX, fx), max(poly.maxX, fx)
				poly.minY, poly.maxY = min(poly.minY, fy), max(poly.maxY, fy)
			}
			poly.end = int32(len(w.pts) / 2)
			w.polys = append(w.polys, poly)
		}
		c.count = int32(len(w.polys)) - c.first
		if c.code != "" {
			slot := codeSlot(c.code)
			if slot >= 0 && w.byCode[slot] != 0 {
				r.fail() // a code twice
			} else if slot >= 0 {
				w.byCode[slot] = int32(len(w.countries)) + 1
			}
		}
		w.countries = append(w.countries, c)
	}
	if r.err != nil {
		return nil, fmt.Errorf("%w: damaged data", errWorld)
	}
	if r.pos != len(body) {
		return nil, fmt.Errorf("%w: trailing bytes", errWorld)
	}
	if len(w.countries) == 0 {
		return nil, fmt.Errorf("%w: no countries", errWorld)
	}
	return w, nil
}
