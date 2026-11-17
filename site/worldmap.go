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
	"strings"
)

const (
	frameW     = 46.0 // frame width in units (degrees of latitude)
	frameH     = 18.2 // frame height in units
	mapScale   = 10   // SVG units per unit (tenths)
	clipMargin = 1.0  // land is clipped this many units outside the frame
	clipExtraW = 0.7  // zoomed-out maps: the clip reaches this many frame widths left and right of the centre
	clipExtraH = 1.1  // and this many frame heights above and below it
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
	minCos      = (frameW + 2*clipMargin) / 360
	svgOpen     = fmt.Sprintf(`<svg class="map" viewBox="%d %d %d %d" preserveAspectRatio="xMidYMin slice" aria-hidden="true" focusable="false">`, -boxL, -boxT, boxL+boxR, boxT+boxB)
	svgOpenMeet = strings.Replace(svgOpen, "xMidYMin slice", "xMidYMid meet", 1)
	svgAim      = fmt.Sprintf(`<path class="aim" d="M%d 0H%dM0%dV%d"/><circle class="halo" r="10"/><circle class="pin" r="3.4"/>`, -boxL, boxR, -boxT, boxB)
)

type point struct{ x, y float64 }

type ipt struct{ x, y int }

// mapDrawer holds the state and the work buffers of one drawing.
type mapDrawer struct {
	lat0, lon0   float64
	c            float64 // longitude scale, cos of the latitude, clamped
	tolHere      float64 // line simplification tolerance in SVG units for the highlighted country; 0 at normal scale
	tolOther     float64 // the same for the other land
	sx, sy       []float64
	keep         []bool
	stack        [][2]int
	zoom         float64 // the frame covers zoom times more degrees; SVG coordinates are divided by it
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
	return w.draw(lat, lon, math.Max(math.Cos(lat*math.Pi/180), minCos), 1, highlight, aim)
}

// Zoomed-out country maps: the main extent of the country must fit these
// shares of the frame, else the map zooms out just enough and centres it.
const (
	fitW = 0.9 // the zoomed-out map keeps the whole frame in view (preserveAspectRatio meet), so the frame is used well
	fitH = 0.84
	// A country at the normal scale keeps this many units between its main extent
	// and the top and bottom of the frame: the label point alone does not say
	// where the outline ends (Japan's is far south of its middle).
	fitEdge = 1.0
)

// countryMap draws the country alone (no aim). A country whose main extent
// fits the frame at the normal scale keeps the label point at the point of the
// frame, moved north or south only as far as its main extent needs to stay
// inside the frame; a bigger one is shown whole, zoomed out, its extent centred.
func (w *world) countryMap(code string) (template.HTML, bool) {
	c := w.country(code)
	if c == nil {
		return "", false
	}
	zoom, cs := c.fit()
	if zoom <= 1 {
		return w.drawMap(c.pointLat(), c.labelX, code, false)
	}
	// The point of the frame is (pointX-0.5, pointY-0.5) of it away from the centre.
	lon := c.extLon + (pointX-0.5)*frameW*zoom/cs
	lat := c.extLat + (0.5-pointY)*frameH*zoom
	return w.draw(lat, lon, cs, zoom, code, false)
}

// pointLat is the latitude to put at the point of the frame for a country at
// the normal scale: the label point, or the nearest latitude that keeps the
// main extent fitEdge inside the top and the bottom of the frame (the top wins).
func (c *wcountry) pointLat() float64 {
	lat := c.labelY
	top, bottom := c.extLat+c.extLatSpan/2, c.extLat-c.extLatSpan/2
	if below := (1-pointY)*frameH - fitEdge; lat-bottom > below {
		lat = bottom + below
	}
	if above := pointY*frameH - fitEdge; top-lat > above {
		lat = top - above
	}
	return lat
}

// fit returns the zoom (1 = the normal scale) at which the main extent fits
// the frame, and the longitude scale to draw at that zoom (clamped so the
// frame never needs more than the whole world).
func (c *wcountry) fit() (zoom, cs float64) {
	cs = math.Max(math.Cos(c.extLat*math.Pi/180), minCos)
	zoom = math.Max(c.extLonSpan*cs/(fitW*frameW), c.extLatSpan/(fitH*frameH))
	if !(zoom > 1) {
		return 1, cs
	}
	return zoom, math.Max(math.Cos(c.extLat*math.Pi/180), minCos*zoom)
}

// draw draws the frame whose point (lat, lon) sits at pointX, pointY, with
// the longitude scale c and the zoom.
func (w *world) draw(lat, lon, c, zoom float64, highlight string, aim bool) (template.HTML, bool) {
	d := &mapDrawer{lat0: lat, lon0: lon, c: c, zoom: zoom}
	if zoom > 1 {
		d.tolHere, d.tolOther = tolHere*zoom, tolOther*zoom
	}
	if highlight != "" {
		if d.highlight = w.country(highlight); d.highlight == nil {
			return "", false
		}
	}
	d.clipL, d.clipR = float64(boxL)/mapScale+clipMargin, float64(boxR)/mapScale+clipMargin
	d.clipT, d.clipB = float64(boxT)/mapScale+clipMargin, float64(boxB)/mapScale+clipMargin
	open := svgOpen
	if zoom > 1 {
		// "meet" shows the whole frame in any box; land is also painted outside the
		// viewBox, to fill the free space of a tall or wide box: the clip rectangle is
		// wider and taller than the frame, around the frame centre, never more than
		// the world wide.
		cx, cy := float64(boxR-boxL)/2/mapScale, float64(boxB-boxT)/2/mapScale
		hw := math.Min(clipExtraW*frameW+clipMargin, 180*c/zoom)
		hh := clipExtraH*frameH + clipMargin
		d.clipL, d.clipR = hw-cx, hw+cx
		d.clipT, d.clipB = hh-cy, hh+cy
		open = svgOpenMeet
	}
	d.lonLo, d.lonHi = lon-d.clipL*zoom/d.c, lon+d.clipR*zoom/d.c
	d.latLo, d.latHi = lat-d.clipB*zoom, lat+d.clipT*zoom
	for i := range w.countries {
		d.country(w, &w.countries[i])
	}
	out := make([]byte, 0, len(open)+len(d.land)+len(d.here)+len(svgAim)+80)
	out = append(out, open...)
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
	// At the normal scale the other land is drawn from the coarse rings.
	coarse := d.zoom <= 1 && c != d.highlight
	for _, p := range w.polys[c.first : c.first+c.count] {
		if p.maxY < d.latLo || p.minY > d.latHi || coarse && p.cstart == p.cend {
			continue
		}
		// The world repeats every 360 degrees: a polygon at -179 is a
		// neighbour of a centre at 179.
		for _, shift := range [3]float64{-360, 0, 360} {
			if p.maxX+shift < d.lonLo || p.minX+shift > d.lonHi {
				continue
			}
			if coarse {
				d.poly(w.cpts[p.cstart*2:p.cend*2], shift, target)
			} else {
				d.poly(w.pts[p.start*2:p.end*2], shift, target)
			}
		}
	}
}

// poly projects, clips and appends one ring to the path in *dst.
func (d *mapDrawer) poly(pts []int16, shift float64, dst *[]byte) {
	d.a = d.a[:0]
	inside := true
	for i := 0; i < len(pts); i += 2 {
		p := point{(float64(pts[i])/worldUnit + shift - d.lon0) * d.c / d.zoom, (d.lat0 - float64(pts[i+1])/worldUnit) / d.zoom}
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
	if d.tolHere > 0 {
		tol := d.tolOther
		if dst == &d.here {
			tol = d.tolHere
		}
		ring = d.simplify(ring, tol*tol)
		if len(ring) < 3 {
			return
		}
	}
	// The highlighted country of a map zoomed out this far is written in half units: its lines
	// are short there, and whole units would turn them into visible steps.
	half := d.zoom >= halfZoom && dst == &d.here
	f := 1.0
	if half {
		f = 2
	}
	ringZoom := minRingZoom
	if dst != &d.here {
		ringZoom = minRingOther
	}
	d.ints = d.ints[:0]
	for _, p := range ring {
		d.ints = append(d.ints, ipt{int(math.Round(p.x * mapScale * f)), int(math.Round(p.y * mapScale * f))})
	}
	r := tidy(d.ints)
	if len(r) < 3 || float64(abs(area2(r))) < f*f*2*minRing*math.Max(1, d.zoom*d.zoom*ringZoom) {
		return
	}
	*dst = appendRing(*dst, r, half)
}

// Zoomed-out maps (zoom > 1) drop points instead of coarsening the grid:
// Douglas-Peucker with a tolerance of tol * zoom SVG units, a larger one for
// the land that is not the highlighted country. minRingZoom scales the
// minimum ring area with the square of the zoom. At the normal scale nothing is
// simplified while drawing: the land that is not highlighted is drawn from the
// coarse rings made once at start with tolLand (world.coarsen), about a pixel
// and a half, which makes a map a third lighter and quicker to draw; the
// highlighted country keeps every point.
const (
	tolLand      = 1.5
	tolHere      = 0.45
	tolOther     = 2.0
	minRingZoom  = 1.5
	minRingOther = 3.0
	halfZoom     = 1.8 // from this zoom the highlighted country is written in half units
)

// simplify is Douglas-Peucker on a closed ring (in units; the tolerance is in
// SVG units, tol2 its square): the ring is split at its first point and the
// point farthest from it, each half is simplified. The result is only good
// until the next call.
func (d *mapDrawer) simplify(r []point, tol2 float64) []point {
	if len(r) < 4 {
		return r
	}
	d.mark(r, tol2)
	out := d.s1[:0]
	for i, k := range d.keep {
		if k {
			out = append(out, r[i])
		}
	}
	d.s1 = out
	return out
}

// mark sets d.keep[i] for the points of the ring (at least 4) that simplify keeps.
func (d *mapDrawer) mark(r []point, tol2 float64) {
	n := len(r)
	d.sx = d.sx[:0]
	for _, p := range r {
		d.sx = append(d.sx, p.x*mapScale, p.y*mapScale)
	}
	pt := func(i int) (float64, float64) { return d.sx[2*(i%n)], d.sx[2*(i%n)+1] }
	d.keep = append(d.keep[:0], make([]bool, n)...)
	far, fd := 0, -1.0
	x0, y0 := pt(0)
	for i := 1; i < n; i++ {
		x, y := pt(i)
		if dd := (x-x0)*(x-x0) + (y-y0)*(y-y0); dd > fd {
			far, fd = i, dd
		}
	}
	d.keep[0], d.keep[far] = true, true
	d.stack = append(d.stack[:0], [2]int{0, far}, [2]int{far, n})
	for len(d.stack) > 0 {
		s := d.stack[len(d.stack)-1]
		d.stack = d.stack[:len(d.stack)-1]
		ax, ay := pt(s[0])
		bx, by := pt(s[1])
		dx, dy := bx-ax, by-ay
		l2 := dx*dx + dy*dy
		best, bi := -1.0, -1
		for i := s[0] + 1; i < s[1]; i++ {
			x, y := pt(i)
			var dd float64
			if l2 == 0 {
				dd = (x-ax)*(x-ax) + (y-ay)*(y-ay)
			} else {
				t := math.Max(0, math.Min(1, ((x-ax)*dx+(y-ay)*dy)/l2))
				ex, ey := x-ax-t*dx, y-ay-t*dy
				dd = ex*ex + ey*ey
			}
			if dd > best {
				best, bi = dd, i
			}
		}
		if bi >= 0 && best > tol2 {
			d.keep[bi] = true
			d.stack = append(d.stack, [2]int{s[0], bi}, [2]int{bi, s[1]})
		}
	}
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

// appendNum writes n, or n/2 ("3", "-.5", "1.5") when the numbers are half units.
func appendNum(b []byte, n int, sep, half bool) []byte {
	if sep && n >= 0 {
		b = append(b, ' ')
	}
	if !half {
		return strconv.AppendInt(b, int64(n), 10)
	}
	if n < 0 {
		b = append(b, '-')
		n = -n
	}
	if n/2 > 0 || n%2 == 0 {
		b = strconv.AppendInt(b, int64(n/2), 10)
	}
	if n%2 == 1 {
		b = append(b, '.', '5')
	}
	return b
}

// appendRing writes "M x y l dx dy ... z" with the shortest separators; half
// means the coordinates are in half units.
func appendRing(b []byte, r []ipt, half bool) []byte {
	b = append(b, 'M')
	b = appendNum(b, r[0].x, false, half)
	b = appendNum(b, r[0].y, true, half)
	b = append(b, 'l')
	for i := 1; i < len(r); i++ {
		b = appendNum(b, r[i].x-r[i-1].x, i > 1, half)
		b = appendNum(b, r[i].y-r[i-1].y, true, half)
	}
	return append(b, 'z')
}
