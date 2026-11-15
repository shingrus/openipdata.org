package main

// The outline map of the place block, drawn on the server as one small inline
// SVG: no script, no request. Pure functions of the embedded country outlines
// (world.go), a centre and an optional country to highlight.
//
// The frame is frameW x frameH units, a unit being one degree of latitude;
// longitude is scaled by cos(latitude of the point), the simple projection of
// the design mockup. The point is not in the middle: it sits at pointX of the
// width and pointY of the height from the top left, so the place name drawn
// bottom left of the block does not cover it. At very high
// latitudes the scale is clamped so the frame never needs more than the whole
// world. The SVG viewBox is the frame in tenths of a unit (mapScale); the land
// is clipped a little outside it (clipMargin), so the edge of a clipped
// country never shows as a line at the border of the frame. All land is one
// path of integer coordinates: an absolute M, then relative l steps, per ring.

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
)

const (
	frameW     = 46.0 // frame width in units (degrees of latitude)
	frameH     = 18.2 // frame height in units
	mapScale   = 10   // SVG units per unit (tenths)
	clipMargin = 1.0  // land is clipped this many units outside the frame
	minRing    = 2    // rings with less area than this (SVG units squared) are not drawn
)

// Where the point sits in the frame, as a share of its width from the left and
// of its height from the top: upper right, clear of the name at the bottom left.
const (
	pointX = 0.7
	pointY = 0.3
)

// The frame in SVG units around the point (0, 0): x in -boxL..boxR, y in -boxT..boxB
// (rounded to a tenth, so the viewBox, the aim lines and the clip agree).
var (
	boxL = int(math.Round(pointX * frameW * mapScale))
	boxR = int(math.Round(frameW*mapScale)) - boxL
	boxT = int(math.Round(pointY * frameH * mapScale))
	boxB = int(math.Round(frameH*mapScale)) - boxT
)

var (
	// minCos keeps the frame inside one world width plus its margin.
	minCos  = (frameW + 2*clipMargin) / 360
	svgOpen = fmt.Sprintf(`<svg class="map" viewBox="%d %d %d %d" preserveAspectRatio="xMidYMin slice" aria-hidden="true" focusable="false">`, -boxL, -boxT, boxL+boxR, boxT+boxB)
	svgAim  = fmt.Sprintf(`<path class="aim" d="M%d 0H%dM0%dV%d"/><circle class="halo" r="10"/><circle class="pin" r="3.4"/>`, -boxL, boxR, -boxT, boxB)
)

type point struct{ x, y float64 }

type ipt struct{ x, y int }

// mapDrawer holds the state and the work buffers of one drawing.
type mapDrawer struct {
	lat0, lon0   float64
	c            float64 // longitude scale, cos of the latitude, clamped
	lonLo, lonHi float64 // the clip rectangle in degrees (before shifting by a world width)
	latLo, latHi float64
	clipL, clipR float64 // the clip rectangle in units around the point: x in -clipL..clipR
	clipT, clipB float64 // y in -clipT..clipB (y grows downwards)
	highlight    *wcountry
	a, s1, s2    []point
	ints         []ipt
	land, here   []byte
}

// drawMap returns the SVG of the map with the point (lat, lon) in the upper right of the frame, or false when there
// is nothing to draw: no data, an invalid point, or a highlight code that is
// not in the data. With aim, two dashed lines cross at the point and a dot
// marks it. A non-empty highlight draws that country in the accent colour.
func (w *world) drawMap(lat, lon float64, highlight string, aim bool) (template.HTML, bool) {
	if w == nil || math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return "", false
	}
	d := &mapDrawer{lat0: lat, lon0: lon}
	if highlight != "" {
		if d.highlight = w.country(highlight); d.highlight == nil {
			return "", false
		}
	}
	d.c = math.Max(math.Cos(lat*math.Pi/180), minCos)
	d.clipL, d.clipR = float64(boxL)/mapScale+clipMargin, float64(boxR)/mapScale+clipMargin
	d.clipT, d.clipB = float64(boxT)/mapScale+clipMargin, float64(boxB)/mapScale+clipMargin
	d.lonLo, d.lonHi = lon-d.clipL/d.c, lon+d.clipR/d.c
	d.latLo, d.latHi = lat-d.clipB, lat+d.clipT
	for i := range w.countries {
		d.country(w, &w.countries[i])
	}
	out := make([]byte, 0, len(svgOpen)+len(d.land)+len(d.here)+len(svgAim)+80)
	out = append(out, svgOpen...)
	if len(d.land) > 0 {
		out = append(out, `<path class="land" d="`...)
		out = append(out, d.land...)
		out = append(out, `"/>`...)
	}
	if len(d.here) > 0 {
		out = append(out, `<path class="here" d="`...)
		out = append(out, d.here...)
		out = append(out, `"/>`...)
	}
	if aim {
		out = append(out, svgAim...)
	}
	out = append(out, "</svg>"...)
	return template.HTML(out), true
}

func (d *mapDrawer) country(w *world, c *wcountry) {
	if c.count == 0 || c.maxY < d.latLo || c.minY > d.latHi {
		return
	}
	target := &d.land
	if c == d.highlight {
		target = &d.here
	}
	for _, p := range w.polys[c.first : c.first+c.count] {
		if p.maxY < d.latLo || p.minY > d.latHi {
			continue
		}
		// The world repeats every 360 degrees: a polygon at -179 is a
		// neighbour of a centre at 179.
		for _, shift := range [3]float64{-360, 0, 360} {
			if p.maxX+shift < d.lonLo || p.minX+shift > d.lonHi {
				continue
			}
			d.poly(w.pts[p.start*2:p.end*2], shift, target)
		}
	}
}

// poly projects, clips and appends one ring to the path in *dst.
func (d *mapDrawer) poly(pts []int16, shift float64, dst *[]byte) {
	d.a = d.a[:0]
	inside := true
	for i := 0; i < len(pts); i += 2 {
		p := point{(float64(pts[i])/worldUnit + shift - d.lon0) * d.c, d.lat0 - float64(pts[i+1])/worldUnit}
		if p.x < -d.clipL || p.x > d.clipR || p.y < -d.clipT || p.y > d.clipB {
			inside = false
		}
		d.a = append(d.a, p)
	}
	ring := d.a
	if !inside {
		ring = d.clip(d.a)
		if len(ring) < 3 {
			return
		}
	}
	d.ints = d.ints[:0]
	for _, p := range ring {
		d.ints = append(d.ints, ipt{int(math.Round(p.x * mapScale)), int(math.Round(p.y * mapScale))})
	}
	r := tidy(d.ints)
	if len(r) < 3 || abs(area2(r)) < 2*minRing {
		return
	}
	*dst = appendRing(*dst, r)
}

// clip cuts the ring to the clip rectangle (Sutherland-Hodgman, edge by edge,
// alternating between the two work buffers). The result is only good until the
// next call.
func (d *mapDrawer) clip(in []point) []point {
	d.s1 = clipEdge(d.s1[:0], in, true, -d.clipL, true)
	d.s2 = clipEdge(d.s2[:0], d.s1, true, d.clipR, false)
	d.s1 = clipEdge(d.s1[:0], d.s2, false, -d.clipT, true)
	d.s2 = clipEdge(d.s2[:0], d.s1, false, d.clipB, false)
	return d.s2
}

// clipEdge keeps the part of the ring on one side of a vertical (x) or
// horizontal (y) line: coordinate >= bound with keepHigh, else <= bound. out
// must not share memory with in.
func clipEdge(out, in []point, x bool, bound float64, keepHigh bool) []point {
	if len(in) == 0 {
		return out
	}
	coord := func(p point) float64 {
		if x {
			return p.x
		}
		return p.y
	}
	ok := func(p point) bool {
		if keepHigh {
			return coord(p) >= bound
		}
		return coord(p) <= bound
	}
	prev := in[len(in)-1]
	for _, cur := range in {
		pi, ci := ok(prev), ok(cur)
		if pi != ci {
			t := (bound - coord(prev)) / (coord(cur) - coord(prev))
			cut := point{prev.x + t*(cur.x-prev.x), prev.y + t*(cur.y-prev.y)}
			if x {
				cut.x = bound
			} else {
				cut.y = bound
			}
			out = append(out, cut)
		}
		if ci {
			out = append(out, cur)
		}
		prev = cur
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// area2 is twice the signed area of the ring.
func area2(r []ipt) int {
	a := 0
	for i, p := range r {
		q := r[(i+1)%len(r)]
		a += p.x*q.y - q.x*p.y
	}
	return a
}

func cross(a, b, c ipt) int { return (b.x-a.x)*(c.y-b.y) - (b.y-a.y)*(c.x-b.x) }

// tidy removes repeated points and points on a straight line (or at the tip of
// a zero-width spike) from a closed ring, in place.
func tidy(p []ipt) []ipt {
	out := p[:0]
	for _, c := range p {
		if n := len(out); n > 0 && out[n-1] == c {
			continue
		}
		for n := len(out); n >= 2 && cross(out[n-2], out[n-1], c) == 0; n = len(out) {
			out = out[:n-1]
		}
		out = append(out, c)
	}
	for changed := true; changed && len(out) >= 3; {
		changed = false
		n := len(out)
		switch {
		case out[n-1] == out[0], cross(out[n-2], out[n-1], out[0]) == 0:
			out, changed = out[:n-1], true
		case cross(out[n-1], out[0], out[1]) == 0:
			out, changed = out[1:], true
		}
	}
	return out
}

func appendNum(b []byte, n int, sep bool) []byte {
	if sep && n >= 0 {
		b = append(b, ' ')
	}
	return strconv.AppendInt(b, int64(n), 10)
}

// appendRing writes "M x y l dx dy ... z" with the shortest separators.
func appendRing(b []byte, r []ipt) []byte {
	b = append(b, 'M')
	b = appendNum(b, r[0].x, false)
	b = appendNum(b, r[0].y, true)
	b = append(b, 'l')
	for i := 1; i < len(r); i++ {
		b = appendNum(b, r[i].x-r[i-1].x, i > 1)
		b = appendNum(b, r[i].y-r[i-1].y, true)
	}
	return append(b, 'z')
}
