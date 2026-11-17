package main

import (
	"fmt"
	"html/template"
	"math"
	"net/netip"
	"regexp"
	"strings"
	"testing"
)

func tiny(t *testing.T, cs ...tcountry) *world {
	t.Helper()
	w, err := parseWorld(writeWorld(cs))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// box is a country with one square, corners in degrees.
func box(code string, lon0, lat0, lon1, lat1 float64) tcountry {
	u := func(v float64) int { return int(math.Round(v * 100)) }
	return tcountry{code: code, label: [2]int{u((lon0 + lon1) / 2), u((lat0 + lat1) / 2)}, bbox: [4]int{u(lon0), u(lat0), u(lon1), u(lat1)},
		polygons: []tpoly{square(u(lon0), u(lat0), u(lon1), u(lat1))}}
}

// rings counts the rings (subpaths) of all paths of the svg.
func rings(svg string) int {
	n := 0
	for _, m := range regexp.MustCompile(`\sd="([^"]*)"`).FindAllStringSubmatch(svg, -1) {
		n += strings.Count(m[1], "M")
	}
	return n
}

func svgOf(t *testing.T, w *world, lat, lon float64, hl string, aim bool) string {
	t.Helper()
	m, ok := w.drawMap(lat, lon, hl, aim)
	if !ok {
		t.Fatal("no map")
	}
	return string(m)
}

func TestDrawPathEncoding(t *testing.T) {
	// A square of 2 x 2 degrees at the equator: tenths of a unit, an absolute M, then relative steps.
	w := tiny(t, box("AA", -1, -1, 1, 1))
	got := svgOf(t, w, 0, 0, "", false)
	want := `<path class="land" d="M-10 10l20 0 0-20-20 0z"/>`
	if !strings.Contains(got, want) {
		t.Errorf("got %s", got)
	}
	if !strings.HasPrefix(got, `<svg class="map" viewBox="-322 -55 460 182" preserveAspectRatio="xMidYMin slice" aria-hidden="true" focusable="false">`) || !strings.HasSuffix(got, "</svg>") {
		t.Errorf("svg wrapper: %s", got)
	}
	if strings.Contains(got, "aim") || strings.Contains(got, "circle") || strings.Contains(got, "here") || strings.Contains(got, "<text") {
		t.Errorf("unexpected parts: %s", got)
	}
}

func TestDrawStates(t *testing.T) {
	w := tiny(t, box("AA", -1, -1, 1, 1), box("BB", 3, -1, 5, 1))
	aim := svgOf(t, w, 0, 0, "", true)
	for _, x := range []string{`<path class="land"`, `<path class="aim" d="M-322 0H138M0-55V127"/>`, `<circle class="halo" r="10"/>`, `<circle class="pin" r="3.4"/>`} {
		if !strings.Contains(aim, x) {
			t.Errorf("city state lacks %s", x)
		}
	}
	if strings.Contains(aim, "here") {
		t.Error("city state highlights a country")
	}
	// A highlighted country has its own path and is not in the land path.
	hl := svgOf(t, w, 0, 4, "BB", false)
	if strings.Contains(hl, "aim") || strings.Contains(hl, "<circle") {
		t.Errorf("country state has an aim: %s", hl)
	}
	land := regexp.MustCompile(`class="land" d="([^"]*)"`).FindStringSubmatch(hl)
	here := regexp.MustCompile(`class="here" d="([^"]*)"`).FindStringSubmatch(hl)
	if land == nil || here == nil || strings.Count(land[1], "M") != 1 || strings.Count(here[1], "M") != 1 {
		t.Errorf("paths: %s", hl)
	}
	if land[1] == here[1] {
		t.Error("highlight drawn as land")
	}
	// Nothing to draw: unknown code, invalid centre, no data.
	for _, c := range []struct {
		w        *world
		lat, lon float64
		hl       string
	}{{w, 0, 0, "XX"}, {w, 0, 0, "aa"}, {w, 91, 0, ""}, {w, -91, 0, ""}, {w, 0, 181, ""}, {w, 0, -181, ""}, {w, math.NaN(), 0, ""}, {w, 0, math.NaN(), ""}, {nil, 0, 0, ""}} {
		if m, ok := c.w.drawMap(c.lat, c.lon, c.hl, false); ok || m != "" {
			t.Errorf("%v %v %q: got a map", c.lat, c.lon, c.hl)
		}
	}
	// (0, 0) is a real place.
	if _, ok := w.drawMap(0, 0, "", true); !ok {
		t.Error("(0,0) has no map")
	}
	// A frame with no land is still a map (the sea and the aim).
	if m := svgOf(t, w, 60, 100, "", true); strings.Contains(m, `class="land"`) || !strings.Contains(m, "aim") {
		t.Errorf("empty frame: %s", m)
	}
}

func TestProjectionScale(t *testing.T) {
	// A frame is 46 units wide; a unit is a degree of latitude and a degree of
	// longitude is cos(latitude) of it, never less than minCos.
	for _, c := range []struct{ lat, want float64 }{{0, 1}, {60, 0.5}, {-60, 0.5}, {89, minCos}, {-90, minCos}, {82.9, minCos}} {
		if got := math.Max(math.Cos(c.lat*math.Pi/180), minCos); math.Abs(got-c.want) > 1e-3 {
			t.Errorf("lat %v: scale %v, want %v", c.lat, got, c.want)
		}
	}
	// At latitude 60 a square of 8 x 2 degrees is 4 x 2 units: 40 x 20 tenths, around the point.
	w := tiny(t, box("AA", -4, 59, 4, 61))
	if got := svgOf(t, w, 60, 0, "", false); !strings.Contains(got, `d="M-20 10l40 0 0-20-40 0z"`) {
		t.Errorf("lat 60: %s", got)
	}
	// At the equator the same degrees are as wide as high.
	w = tiny(t, box("AA", -2, -1, 2, 1))
	if got := svgOf(t, w, 0, 0, "", false); !strings.Contains(got, `d="M-20 10l40 0 0-20-40 0z"`) {
		t.Errorf("equator: %s", got)
	}
	// The point is the origin: a square around it is around (0, 0).
	w = tiny(t, box("AA", 10, 20, 12, 22))
	if got := svgOf(t, w, 21, 11, "", false); !strings.Contains(got, `d="M-9 10l18 0 0-20-18 0z"`) {
		t.Errorf("not centred: %s", got)
	}
	// The frame never needs more than the whole world, however far north the centre is.
	for _, lat := range []float64{80, 85, 89.9, 90, -90} {
		c := math.Max(math.Cos(lat*math.Pi/180), minCos)
		if width := (frameW + 2*clipMargin) / c; width > 360+1e-9 {
			t.Errorf("lat %v: frame is %v degrees wide", lat, width)
		}
	}
	w = tiny(t, box("AA", -179, 80, 179, 89))
	if m, ok := w.drawMap(90, 0, "", true); !ok || !strings.Contains(string(m), `class="land"`) {
		t.Error("the pole has no map")
	}
}

// The point is at 70% of the width and 30% of the height of the frame, the frame
// reaches further left and below, and the clip follows (land 40 units to the
// left is drawn, land 20 units to the right is not).
func TestPointPosition(t *testing.T) {
	m := regexp.MustCompile(`viewBox="(-?\d+) (-?\d+) (\d+) (\d+)"`).FindStringSubmatch(svgOpen)
	if m == nil {
		t.Fatal(svgOpen)
	}
	var v [4]float64
	for i := range v {
		fmt.Sscan(m[i+1], &v[i])
	}
	if x, y := -v[0]/v[2], -v[1]/v[3]; math.Abs(x-pointX) > 0.005 || math.Abs(y-pointY) > 0.005 {
		t.Errorf("point at %.3f, %.3f of the frame, want %v, %v", x, y, pointX, pointY)
	}
	if v[2] != frameW*mapScale || v[3] != frameH*mapScale {
		t.Errorf("frame size %v x %v", v[2], v[3])
	}
	if !strings.Contains(svgOpen, "xMidYMin slice") {
		t.Error("alignment")
	}
	// The aim lines run across the whole frame and cross at the point.
	if want := fmt.Sprintf(`d="M%d 0H%dM0%dV%d"`, -boxL, boxR, -boxT, boxB); !strings.Contains(svgAim, want) {
		t.Errorf("aim %s, want %s", svgAim, want)
	}
	// The clip is the frame plus a unit: 33.2 to the left, 14.8 to the right,
	// 6.5 above and 13.7 below the point (y grows downwards, north is up).
	w := tiny(t,
		box("WA", -31, -1, -29, 1), box("WB", -40, -1, -38, 1), // west: drawn, outside
		box("EA", 13, -1, 14.5, 1), box("EB", 15.5, -1, 17, 1), // east: drawn, outside
		box("SA", -1, -13, 1, -12), box("SB", -1, -15, 1, -14), // south: drawn, outside
		box("NB", -1, 7, 1, 8)) // north: outside
	got := svgOf(t, w, 0, 0, "", false)
	if n := rings(got); n != 3 {
		t.Errorf("%d rings, want 3 (W1, E1, S1): %s", n, got)
	}
	for _, x := range []string{"M-310 10", "M130 10", "M-10 130"} {
		if !strings.Contains(got, x) {
			t.Errorf("lacks %s: %s", x, got)
		}
	}
}

func TestDrawAntimeridian(t *testing.T) {
	// One country on each side of the date line.
	w := tiny(t, box("WW", -180, -1, -178, 1), box("EE", 178, -1, 180, 1))
	for _, c := range []struct {
		lon float64
		x   []string // the first points of the squares, in tenths
	}{
		{179, []string{"M10 10", "M-10 10"}},   // WW is at x = 1..3 units east of the centre, EE is at -1..1
		{-179, []string{"M-30 10", "M-10 10"}}, // EE is at x = -3..-1, WW is at -1..1
	} {
		got := svgOf(t, w, 0, c.lon, "", false)
		for _, x := range c.x {
			if !strings.Contains(got, x) {
				t.Errorf("centre %v: no %s in %s", c.lon, x, got)
			}
		}
		if n := rings(got); n != 2 {
			t.Errorf("centre %v: %d rings, want 2", c.lon, n)
		}
	}
	// The frame is longer to the left of the point (33 units) than to the right (15),
	// across the date line too. Centre 170: WW is 10..12 units east, EE 8..10: both drawn.
	// Centre 164: WW would be 16..18 east, outside; only EE (cut at 14.8) is left.
	// Centre -164: EE is 14..16 west, inside (the left side is long): both drawn.
	for _, c := range []struct {
		lon     float64
		rings   int
		x, notx string
	}{{170, 2, "M100 10", ""}, {164, 1, "M140 10l8 ", "M160"}, {-164, 2, "M-160 10", ""}} {
		got := svgOf(t, w, 0, c.lon, "", false)
		if n := rings(got); n != c.rings || !strings.Contains(got, c.x) || (c.notx != "" && strings.Contains(got, c.notx)) {
			t.Errorf("centre %v: %d rings, want %d (%s): %s", c.lon, n, c.rings, c.x, got)
		}
	}
	// The highlighted country is found on the other side too.
	if got := svgOf(t, w, 0, 179, "WW", false); !strings.Contains(got, `class="here" d="M10 10`) {
		t.Errorf("highlight over the date line: %s", got)
	}
	// Far from both, nothing is drawn.
	if got := svgOf(t, w, 0, 90, "", false); strings.Contains(got, `class="land"`) {
		t.Errorf("far land drawn: %s", got)
	}
}

func TestClip(t *testing.T) {
	d := &mapDrawer{clipL: 14, clipR: 6, clipT: 2, clipB: 8}
	P := func(xy ...float64) []point {
		var out []point
		for i := 0; i < len(xy); i += 2 {
			out = append(out, point{xy[i], xy[i+1]})
		}
		return out
	}
	for _, c := range []struct {
		name string
		ring []point
		want float64 // area after clipping, -1: nothing left
		in   bool    // every point is inside the rectangle
	}{
		{"inside", P(-1, -1, 1, -1, 1, 1, -1, 1), 4, true},
		{"outside left", P(-30, -1, -20, -1, -20, 1, -30, 1), -1, true},
		{"outside above", P(-1, 20, 1, 20, 1, 30, -1, 30), -1, true},
		{"crossing the right edge", P(5, -1, 15, -1, 15, 1, 5, 1), 1 * 2, true},
		{"outside the short right side", P(8, -1, 12, -1, 12, 1, 8, 1), -1, true},
		{"crossing the long left edge", P(-20, -1, -10, -1, -10, 1, -20, 1), 4 * 2, true},
		{"crossing the short top edge", P(-1, -5, 1, -5, 1, 0, -1, 0), 2 * 2, true},
		{"crossing the long bottom edge", P(-1, 4, 1, 4, 1, 12, -1, 12), 4 * 2, true},
		{"crossing a corner", P(4, 6, 10, 6, 10, 12, 4, 12), 2 * 2, true},
		{"covers the frame", P(-50, -50, 50, -50, 50, 50, -50, 50), 20 * 10, true},
		{"triangle across the top", P(-5, 0, 5, 0, 0, 100), 0, true}, // area checked below
	} {
		got := d.clip(append([]point{}, c.ring...))
		if c.want < 0 {
			if len(got) != 0 {
				t.Errorf("%s: %v left", c.name, got)
			}
			continue
		}
		for _, p := range got {
			if p.x < -d.clipL-1e-9 || p.x > d.clipR+1e-9 || p.y < -d.clipT-1e-9 || p.y > d.clipB+1e-9 {
				t.Errorf("%s: %v is outside", c.name, p)
			}
		}
		if c.name == "triangle across the top" {
			if len(got) < 3 {
				t.Errorf("%s: %v", c.name, got)
			}
			continue
		}
		var a float64
		for i, p := range got {
			q := got[(i+1)%len(got)]
			a += p.x*q.y - q.x*p.y
		}
		if math.Abs(math.Abs(a/2)-c.want) > 1e-9 {
			t.Errorf("%s: area %v, want %v (%v)", c.name, math.Abs(a/2), c.want, got)
		}
	}
	// A ring that covers the frame becomes the rectangle (with the margin): four points.
	got := d.clip(P(-50, -50, 50, -50, 50, 50, -50, 50))
	if len(got) != 4 {
		t.Errorf("cover: %v", got)
	}
	// The work buffers are reused: a second call does not keep the first one's points.
	d.clip(P(-1, -1, 1, -1, 1, 1))
	if got := d.clip(P(-30, -1, -20, -1, -20, 1)); len(got) != 0 {
		t.Errorf("stale points: %v", got)
	}
}

func TestDrawClipsAtTheFrame(t *testing.T) {
	// A country that is much larger than the frame: its path stays inside the clip rectangle (frame plus margin).
	w := tiny(t, box("AA", -90, -60, 90, 60))
	got := svgOf(t, w, 0, 0, "", false)
	m := regexp.MustCompile(`class="land" d="([^"]*)"`).FindStringSubmatch(got)
	if m == nil {
		t.Fatal(got)
	}
	if want := fmt.Sprintf("M%d%dl0 %d %d 0 0%dz", -boxL-10, -boxT-10, boxT+boxB+20, boxL+boxR+20, -boxT-boxB-20); m[1] != want {
		t.Errorf("covering ring: %s", m[1])
	}
}

func TestTidyAndAppendRing(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []ipt
		want []ipt
	}{
		{"nothing to remove", []ipt{{0, 0}, {10, 0}, {10, 10}}, []ipt{{0, 0}, {10, 0}, {10, 10}}},
		{"repeated point", []ipt{{0, 0}, {0, 0}, {10, 0}, {10, 10}}, []ipt{{0, 0}, {10, 0}, {10, 10}}},
		{"point on a straight line", []ipt{{0, 0}, {5, 0}, {10, 0}, {10, 10}}, []ipt{{0, 0}, {10, 0}, {10, 10}}},
		{"diagonal line", []ipt{{0, 0}, {2, 2}, {4, 4}, {10, 0}}, []ipt{{0, 0}, {4, 4}, {10, 0}}},
		{"first point on a line", []ipt{{5, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0}}, []ipt{{10, 0}, {10, 10}, {0, 10}, {0, 0}}},
		{"last point equals the first", []ipt{{0, 0}, {10, 0}, {10, 10}, {0, 0}}, []ipt{{0, 0}, {10, 0}, {10, 10}}},
		{"spike", []ipt{{0, 0}, {10, 0}, {20, 0}, {10, 0}, {10, 10}}, []ipt{{0, 0}, {10, 0}, {10, 10}}},
		{"a line", []ipt{{0, 0}, {5, 5}, {10, 10}}, nil},
	} {
		got := tidy(append([]ipt{}, c.in...))
		if len(c.want) == 0 && len(got) >= 3 || len(c.want) > 0 && fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	for _, c := range []struct {
		r    []ipt
		want string
	}{
		{[]ipt{{0, 0}, {10, 0}, {10, 10}}, "M0 0l10 0 0 10z"},
		{[]ipt{{-5, -6}, {-1, 2}, {3, -4}}, "M-5-6l4 8 4-6z"},
		{[]ipt{{100, -200}, {101, -200}, {101, -190}}, "M100-200l1 0 0 10z"},
	} {
		if got := string(appendRing(nil, c.r, false)); got != c.want {
			t.Errorf("appendRing %v = %s, want %s", c.r, got, c.want)
		}
	}
}

func TestSmallRingsAreDropped(t *testing.T) {
	// A ring of 1 x 1 tenths is not drawn; one of 5 x 5 tenths is.
	w := tiny(t, box("AA", 0, 0, 0.1, 0.1), box("BB", 1, 0, 1.5, 0.5))
	got := svgOf(t, w, 0, 0, "", false)
	if n := rings(got); n != 1 || !strings.Contains(got, "M10 0l") {
		t.Errorf("rings: %s", got)
	}
}

func TestPlaceMap(t *testing.T) {
	w := tiny(t, box("PT", -9, 37, -6, 42), box("ES", -7, 36, 3, 43))
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		name string
		g    geoRecord
		want string // "aim", "here", "" (no map)
	}{
		{"city with coordinates", geoRecord{CountryCode: "PT", CityName: "Lisbon", Latitude: f(38.7), Longitude: f(-9.1)}, "aim"},
		{"city at 0, 0", geoRecord{CountryCode: "PT", CityName: "Null Island", Latitude: f(0), Longitude: f(0)}, "aim"},
		{"city without coordinates", geoRecord{CountryCode: "PT", CityName: "Lisbon"}, ""},
		{"city with only a latitude", geoRecord{CountryCode: "PT", CityName: "Lisbon", Latitude: f(38.7)}, ""},
		{"city with only a longitude", geoRecord{CountryCode: "PT", CityName: "Lisbon", Longitude: f(-9.1)}, ""},
		{"latitude out of range", geoRecord{CityName: "X", Latitude: f(91), Longitude: f(0)}, ""},
		{"latitude below range", geoRecord{CityName: "X", Latitude: f(-90.5), Longitude: f(0)}, ""},
		{"longitude out of range", geoRecord{CityName: "X", Latitude: f(0), Longitude: f(180.1)}, ""},
		{"longitude below range", geoRecord{CityName: "X", Latitude: f(0), Longitude: f(-181)}, ""},
		{"NaN", geoRecord{CityName: "X", Latitude: f(math.NaN()), Longitude: f(0)}, ""},
		{"Inf", geoRecord{CityName: "X", Latitude: f(0), Longitude: f(math.Inf(1))}, ""},
		{"edge of the globe", geoRecord{CityName: "X", Latitude: f(-90), Longitude: f(-180)}, "aim"},
		{"country only", geoRecord{CountryCode: "PT", CountryName: "Portugal"}, "here"},
		{"country only, no name", geoRecord{CountryCode: "ES"}, "here"},
		{"country code not in the data", geoRecord{CountryCode: "QQ", CountryName: "Nowhere"}, ""},
		{"coordinates without a city are not used", geoRecord{CountryCode: "PT", Latitude: f(1), Longitude: f(1)}, "here"},
		{"nothing", geoRecord{}, ""},
	} {
		m := string(w.placeMap(&c.g))
		switch c.want {
		case "aim":
			if !strings.Contains(m, `class="aim"`) || strings.Contains(m, `class="here"`) {
				t.Errorf("%s: %s", c.name, m)
			}
		case "here":
			if !strings.Contains(m, `class="here"`) || strings.Contains(m, `class="aim"`) || strings.Contains(m, "<circle") {
				t.Errorf("%s: %s", c.name, m)
			}
		default:
			if m != "" {
				t.Errorf("%s: got a map", c.name)
			}
		}
	}
	var none *world
	if m := none.placeMap(&geoRecord{CountryCode: "PT"}); m != "" {
		t.Error("map without data")
	}
}

// pages: the fixture Lisbon rows have coordinates, Spain has a country only.
func TestMapOnPages(t *testing.T) {
	s := fixtureServer(t)
	place := regexp.MustCompile(`<div class="place( none| mapped)?">`)
	for _, c := range []struct {
		name, target string
		want         string // "aim", "here", ""
	}{
		{"lookup, city with coordinates", "/?203.0.113.9", "aim"},
		{"lookup, IPv6 city", "/?2001:db8::1", "aim"},
		{"lookup, country only", "/?198.51.100.9", "here"},
		{"lookup, unknown", "/?192.0.2.9", ""},
	} {
		w := get(s, "GET", c.target)
		body := w.Body.String()
		if w.Code != 200 || !place.MatchString(body) {
			t.Errorf("%s: status %d", c.name, w.Code)
			continue
		}
		hasMap := strings.Contains(body, `<svg class="map"`)
		if mapped := strings.Contains(body, `<div class="place mapped">`); mapped != hasMap {
			t.Errorf("%s: class mapped is %v with a map %v", c.name, mapped, hasMap)
		}
		switch c.want {
		case "aim":
			if !hasMap || !strings.Contains(body, `class="aim"`) || strings.Contains(body, `class="here"`) {
				t.Errorf("%s: map state wrong", c.name)
			}
		case "here":
			if !hasMap || strings.Contains(body, `class="aim"`) || !strings.Contains(body, `class="here"`) {
				t.Errorf("%s: map state wrong", c.name)
			}
		default:
			if hasMap || strings.Contains(body, "<svg class") {
				t.Errorf("%s: unexpected map", c.name)
			}
		}
		// The map is the first child of the place block, decorative, with no text and no script.
		if hasMap {
			if !strings.Contains(body, `<div class="place mapped">`+`<svg class="map"`) || !strings.Contains(body, `aria-hidden="true" focusable="false"><path`) {
				t.Errorf("%s: map is not the first child or not decorative", c.name)
			}
			svg := body[strings.Index(body, `<svg class="map"`):]
			svg = svg[:strings.Index(svg, "</svg>")]
			if strings.Contains(svg, "<text") || strings.Contains(svg, "<script") || strings.Contains(svg, "style=") || strings.Contains(svg, "<title") {
				t.Errorf("%s: svg has text, script or style", c.name)
			}
		}
		if n := strings.Count(body, "<script"); n != 3 { // head, theme and panel (the copy button of the address)
			t.Errorf("%s: %d scripts", c.name, n)
		}
		if strings.Contains(body, "<head>") && strings.Contains(strings.SplitN(body, "</head>", 2)[0], "<svg class=\"map\"") {
			t.Errorf("%s: map in the head", c.name)
		}
		if got := w.Header().Get("Content-Security-Policy"); got != s.cspPnl[0] {
			t.Errorf("%s: CSP is not the policy of a page with panel.js: %s", c.name, got)
		}
	}
	// ASN pages and the search page have no map.
	for _, target := range []string{"/as64500", "/top-asn-by-ip-address"} {
		if b := get(s, "GET", target).Body.String(); strings.Contains(b, `<svg class="map"`) {
			t.Errorf("%s has a map", target)
		}
	}
}

// An MMDB without coordinates (the file published today) renders as before: a city has no map.
func TestMapNoCoordinatesMMDB(t *testing.T) {
	s, err := newServer("testdata/stamped.mmdb", "testdata/lookup-asn.mmdb", "X-Real-IP")
	if err != nil {
		t.Fatal(err)
	}
	s.public = func(a netip.Addr) bool { return true }
	body := get(s, "GET", "/?192.0.2.9").Body.String()
	if !strings.Contains(body, "<h1>Lisbon</h1>") || strings.Contains(body, "<svg class") {
		t.Errorf("a city without coordinates has a map or no name")
	}
}

// The place block has the same class and the same CSS box with and without a
// map: the map is absolute, so the page does not move.
func TestMapLayoutStable(t *testing.T) {
	s := fixtureServer(t)
	with := get(s, "GET", "/?203.0.113.9").Body.String()
	without := get(s, "GET", "/?192.0.2.9").Body.String()
	for _, b := range []string{with, without} {
		if !strings.Contains(b, `<div class="place`) {
			t.Fatal("no place block")
		}
	}
	re := regexp.MustCompile(`<div class="place( none| mapped)?">`)
	if re.FindString(with) != `<div class="place mapped">` || re.FindString(without) != `<div class="place none">` {
		t.Errorf("place classes: %q %q", re.FindString(with), re.FindString(without))
	}
	css := styleCSS + placeCSS + mapCSS
	for _, x := range []string{".place svg.map{position:absolute;", "height:100%;max-height:66.7cqw;max-width:100%", ".place{container-type:inline-size}", "pointer-events:none", "mask:linear-gradient", "vector-effect:non-scaling-stroke"} {
		if !strings.Contains(css, x) {
			t.Errorf("style.css lacks %q", x)
		}
	}
	// min-height of the block does not depend on the map (the rule is on .place, not on a map class).
	if !strings.Contains(css, ".place{position:relative;min-width:0;min-height:240px") || !strings.Contains(css, ".place{min-height:170px") {
		t.Error("the place block lost its fixed min-height")
	}
	// No grid behind a place block, with or without a map: only the glow (owner, 2026-11-17).
	if strings.Contains(css, "--grid") || strings.Contains(css, "linear-gradient(var(") || strings.Contains(css, ".place.mapped") {
		t.Error("the place block has a grid background, or a rule for the mapped class")
	}
	// Only existing colour tokens.
	for _, x := range []string{"fill:var(--raised)", "stroke:var(--line-2)", "stroke:var(--accent)", "fill:var(--accent-soft)", "fill:var(--accent);opacity"} {
		if !strings.Contains(css, x) {
			t.Errorf("style.css lacks %q", x)
		}
	}
}

// The cities and countries of the size budget: the map markup must stay small.
var sizeCases = []struct {
	name     string
	lat, lon float64
	country  string // set: the country-only map, centred on its label point
	typical  bool   // a typical place: at most 5 KB; the dense coasts may take up to 9 KB
}{
	{"Lisbon", 38.72, -9.14, "", true}, {"Singapore", 1.29, 103.85, "", true}, {"Hong Kong", 22.32, 114.17, "", true},
	{"London", 51.51, -0.13, "", false}, {"New York", 40.71, -74.01, "", true}, {"Tokyo", 35.68, 139.69, "", true},
	{"Sydney", -33.87, 151.21, "", true}, {"Oslo", 59.91, 10.75, "", false}, {"Athens", 37.98, 23.73, "", false},
	{"Jakarta", -6.21, 106.85, "", true}, {"Reykjavik", 64.15, -21.94, "", false}, {"Suva", -18.14, 178.44, "", true},
	{"PT", 0, 0, "PT", true}, {"US", 0, 0, "US", true}, {"RU", 0, 0, "RU", false}, {"ID", 0, 0, "ID", true},
	{"DE", 0, 0, "DE", false}, {"CA", 0, 0, "CA", false}, {"CN", 0, 0, "CN", false}, {"AU", 0, 0, "AU", true},
	{"BR", 0, 0, "BR", true},
}

func drawCase(w *world, c struct {
	name     string
	lat, lon float64
	country  string
	typical  bool
}) (string, bool) {
	if c.country == "" {
		m, ok := w.drawMap(c.lat, c.lon, "", true)
		return string(m), ok
	}
	m, ok := w.countryMap(c.country)
	return string(m), ok
}

// Since 2026-11-17 a map at the normal scale draws the other land from the
// coarse rings: London is 4.6 KB, Germany 5.6 KB (7.1 KB before; tolLand 1.5 gave 3.5 and 4.4 KB and looked too coarse), the biggest
// map is the zoomed-out Canada, 8.1 KB. The limits stay for the zoomed maps.
//
// Limits (raw bytes and gzip, measured 2026-11-15 with static/world.bin made at
// tolerance 0.08): a map is at most 9 KB raw (the biggest, London, is about 7.8
// KB; a typical city 4 to 5 KB) and 4 KB gzipped (the biggest is about 2.9 KB).
// A page with a map is at most 28672 raw and 10240 gzipped: the biggest is
// Canada on the fullest visitor page (all six scripts, the DNS row, copy and
// Share buttons), 28286 and 10086 on 2026-11-15 after the scripts lost their
// comments and indentation (29250 and 10454 before; 21974 and 8407 with the
// fill script only, before the copy, share and link work). The address-unknown
// page is at most 14 KB and 5 KB (13700 and 4814 now). The visitor page with
// the fill script: with a map (Lisbon) at most 19456 and 7168 (18667 and 6730
// now, 19849 and 7212 before), without a map at most 17408 and 6144 (16730 and
// 5808 now, 17967 and 6314 before).
const (
	mapMaxRaw, mapMaxGz         = 9 << 10, 4 << 10
	typicalMapMax               = 5 << 10
	pageMapMaxRaw, pageMapMaxGz = 43008, 14336 // the fullest visitor page with a map: all scripts, the DNS row, copy and Share buttons, the Browser row with the biggest icons (Firefox, Linux, both shrunk), a long Network panel, the API panel (42617 and 14111 now, CA, a zoomed-out map the coarse land does not change; it has every part, so the split of the style files saves nothing here; 41148 and 13745 with the one-line API hint before)
	pageMaxRaw, pageMaxGz       = 12288, 4608  // 11494 and 4221 now: it gets only the styles of its parts (homeCSS); 15271 and 5154 before the split
	// The visitor page (fill script, no beacon, no Redis) with a map (Lisbon) and without one.
	visitorMapMaxRaw, visitorMapMaxGz = 21504, 7424 // 21270 and 7335 now with the API panel
	visitorMaxRaw, visitorMaxGz       = 18432, 6272 // 18296 and 6114 now with the API panel (the limit of before the panel again)
)

func TestMapSizes(t *testing.T) {
	s := fixtureServer(t)
	for _, c := range sizeCases {
		m, ok := drawCase(s.world, c)
		if !ok {
			t.Errorf("%s: no map", c.name)
			continue
		}
		v := makeView(*s.scores.Load())
		v.Fill, v.Res, v.Pnl, v.Share = true, true, true, true // the fullest visitor page: all scripts, the DNS row, copy and Share buttons
		v.Panel = &panel{Title: "Your connection", Big: "Lisbon", Sub: "Portugal", Map: template.HTML(m), Copy: true,
			Share: true, Rows: []row{
				{K: "IPv4", V: "203.0.113.9", Copy: true}, {K: "IPv6", V: "—", Fam: "6", From: u6, Copy: true},
				{K: "DNS resolver", V: "—", Zone: "r.openipdata.org"},
				{K: "Organization", Parts: []part{{Text: "British Telecommunications Limited", Href: "/asn-british-telecommunications-limited"}}},
				{K: "Network", Parts: []part{{Text: "EXAMPLE-NET", Href: "/as64500"}, {Text: "AS64500", Href: "/as64500", Sp: true}}},
				{K: "Prefix", Parts: []part{{Text: "203.0.113.0/24", Href: "/prefix/203.0.113.0/24"}}},
				{K: "Domain", Parts: []part{{Text: "example-network-domain.net", Href: "https://example-network-domain.net/?utm_source=openipdata.org", Ext: true}}}},
			Hdr: template.JS(strings.Repeat("x", 600)), Br: browserRowFor("Mozilla/5.0 (X11; Linux x86_64; rv:146.0) Gecko/20100101 Firefox/146.0", "")} // the biggest icons: Firefox and Linux
		v.Net = &netPanel{Handle: "BT-UK-AS", ASN: "AS2856", Href: "/as2856",
			Type: "Education research", Country: "United Kingdom", CountryHref: "/united-kingdom-asn-list", Role: "Access provider",
			Domain: "example-network-domain.net", DomainHref: "https://example-network-domain.net/?utm_source=openipdata.org",
			Prefix: "2001:db8:1234::/48", PrefixHref: "/prefix/2001:db8:1234::/48", Addr: "12,402,688"} // a long Network panel
		v.API = s.apiPanelFor("/json", netip.MustParseAddr("203.0.113.9"))
		v.CSS = v.homeCSS()
		page, err := render(s.tmpl, "home.html", v)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%-10s map %5d raw %4d gz, page %5d raw %4d gz", c.name, len(m), gzipped([]byte(m)), len(page), gzipped(page))
		if len(m) > mapMaxRaw || gzipped([]byte(m)) > mapMaxGz {
			t.Errorf("%s: map is %d bytes (limit %d), %d gzipped (limit %d)", c.name, len(m), mapMaxRaw, gzipped([]byte(m)), mapMaxGz)
		}
		if c.typical && len(m) > typicalMapMax {
			t.Errorf("%s: a typical map is %d bytes (target %d)", c.name, len(m), typicalMapMax)
		}
		if len(page) > pageMapMaxRaw || gzipped(page) > pageMapMaxGz {
			t.Errorf("%s: page is %d bytes (limit %d), %d gzipped (limit %d)", c.name, len(page), pageMapMaxRaw, gzipped(page), pageMapMaxGz)
		}
	}
}

// BenchmarkDrawMap draws one map (a dense coast: Athens, and Tokyo).
func BenchmarkDrawMap(b *testing.B) {
	w, err := parseWorld(worldBin)
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		lat, lon float64
	}{{"Athens", 37.98, 23.73}, {"Tokyo", 35.68, 139.69}, {"Oslo", 59.91, 10.75}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, ok := w.drawMap(c.lat, c.lon, "", true); !ok {
					b.Fatal("no map")
				}
			}
		})
	}
}

func TestMainExtent(t *testing.T) {
	w, err := parseWorld(worldBin)
	if err != nil {
		t.Fatal(err)
	}
	ext := func(code string) *wcountry {
		c := w.country(code)
		if c == nil {
			t.Fatalf("%s missing", code)
		}
		return c
	}
	// France: the far departments (French Guiana, Reunion) do not count.
	if c := ext("FR"); c.extLon < -10 || c.extLon > 15 || c.extLat < 40 || c.extLat > 52 || c.extLonSpan > 20 || c.extLatSpan > 15 {
		t.Errorf("FR extent %+v", *c)
	}
	// Russia: the piece east of the antimeridian is joined the narrow way.
	if c := ext("RU"); c.extLonSpan > 180 || c.extLonSpan < 100 || c.extLon < 60 || c.extLon > 150 {
		t.Errorf("RU extent centre %v span %v", c.extLon, c.extLonSpan)
	}
	// The United States: Hawaii is small, the extent is North America.
	if c := ext("US"); c.extLon > -90 || c.extLon < -150 || c.extLatSpan > 50 {
		t.Errorf("US extent centre %v,%v span %vx%v", c.extLon, c.extLat, c.extLonSpan, c.extLatSpan)
	}
	// New Zealand and Fiji cross or touch the antimeridian: a small span, centre in range.
	for _, code := range []string{"NZ", "FJ"} {
		c := ext(code)
		if c.extLonSpan > 30 || c.extLon < -180 || c.extLon > 180 {
			t.Errorf("%s extent centre %v span %v", code, c.extLon, c.extLonSpan)
		}
	}
	if c := ext("FJ"); math.Abs(c.extLon) < 170 {
		t.Errorf("FJ centre %v is not near the antimeridian", c.extLon)
	}
	// every country with an outline has a sane extent
	for _, c := range w.countries {
		if c.count == 0 {
			continue
		}
		if c.extLonSpan <= 0 || c.extLonSpan > 360 || c.extLatSpan < 0 || c.extLon < -180 || c.extLon > 180 || c.extLat < -90 || c.extLat > 90 {
			t.Errorf("%q: extent centre %v,%v span %vx%v", c.code, c.extLon, c.extLat, c.extLonSpan, c.extLatSpan)
		}
	}
}

func TestCountryMapZoomsOut(t *testing.T) {
	w, err := parseWorld(worldBin)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"RU", "US", "CA", "CN", "AU", "BR"} {
		c := w.country(code)
		zoom, cs := c.fit()
		if zoom <= 1 {
			t.Errorf("%s: zoom %v, want zoomed out", code, zoom)
			continue
		}
		// the extent in frame units (centre of the frame at 0.5, 0.5)
		hw, hh := c.extLonSpan*cs/zoom/2, c.extLatSpan/zoom/2
		if hw > fitW*frameW/2+1e-9 || hh > fitH*frameH/2+1e-9 {
			t.Errorf("%s: the extent is %.1f x %.1f units in a frame of %v x %v", code, 2*hw, 2*hh, frameW, frameH)
		}
		if frameW*zoom/cs > 360 {
			t.Errorf("%s: the frame covers %.0f degrees of longitude", code, frameW*zoom/cs)
		}
		m, ok := w.countryMap(code)
		if !ok || !strings.Contains(string(m), `class="here"`) || strings.Contains(string(m), `class="aim"`) {
			t.Errorf("%s: no highlighted map without aim", code)
		}
		if string(m) != string(mustDraw(t, w, code)) {
			// placeMap goes through the same path
			t.Errorf("%s: placeMap differs from countryMap", code)
		}
	}
	// Countries that fit are drawn at the normal scale around their label point
	// (Japan a little north of it: TestCountryStaysInTheFrame).
	for _, code := range []string{"PT", "DE", "SG", "GB", "FR", "JP"} {
		c := w.country(code)
		if zoom, _ := c.fit(); zoom != 1 {
			t.Errorf("%s: zoom %v, want 1", code, zoom)
		}
		old, _ := w.drawMap(c.pointLat(), c.labelX, code, false)
		got, _ := w.countryMap(code)
		if old != got {
			t.Errorf("%s: the map changed", code)
		}
	}
}

func TestZoomedMapMeets(t *testing.T) {
	w, err := parseWorld(worldBin)
	if err != nil {
		t.Fatal(err)
	}
	ru, _ := w.countryMap("RU")
	if !strings.Contains(string(ru), `preserveAspectRatio="xMidYMid meet"`) || strings.Contains(string(ru), "slice") {
		t.Errorf("RU: %.120s", ru)
	}
	// the viewBox is the same as for the normal scale
	vb := regexp.MustCompile(`viewBox="[^"]*"`)
	if vb.FindString(string(ru)) != vb.FindString(svgOpen) {
		t.Error("the viewBox of a zoomed map changed")
	}
	pt, _ := w.countryMap("PT")
	if !strings.HasPrefix(string(pt), svgOpen) || strings.Contains(string(pt), "meet") {
		t.Errorf("PT: %.120s", pt)
	}
	// land is painted outside the viewBox for the free space of a tall or wide box: the
	// clip reaches beyond the frame, never past the world.
	for _, code := range []string{"RU", "US", "CA", "CN", "AU", "BR"} {
		c := w.country(code)
		zoom, cs := c.fit()
		if hw := math.Min(clipExtraW*frameW+clipMargin, 180*cs/zoom); 2*hw*zoom/cs > 360+1e-9 {
			t.Errorf("%s: clip covers %v degrees", code, 2*hw*zoom/cs)
		}
	}
}

func mustDraw(t *testing.T, w *world, code string) template.HTML {
	t.Helper()
	return w.placeMap(&geoRecord{CountryCode: code})
}

// A wide, short place block (a prefix page in a window of 500 to 700 px) used to
// scale the frame to its width and show only the top of it: Germany lost its
// lower half. The map is now never wider than mapAspect times its height, so
// at least frameW/mapAspect units of the frame's height show in every block.
func TestCountryShowsInAWideBlock(t *testing.T) {
	i := strings.Index(mapCSS, ".place svg.map{")
	if i < 0 {
		t.Fatal("map.css: no .place svg.map rule")
	}
	body := mapCSS[i+len(".place svg.map{"):]
	body = body[:strings.Index(body, "}")]
	decl := map[string]string{}
	for _, d := range strings.Split(body, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(d), ":"); ok {
			decl[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	for k, v := range map[string]string{"right": "0", "height": "100%", "max-width": "100%"} {
		if decl[k] != v {
			t.Errorf("map.css: the map has no largest width for its height (%s:%s), got %q", k, v, decl[k])
		}
	}
	for _, k := range []string{"left", "width"} {
		if _, ok := decl[k]; ok {
			t.Errorf("map.css: the map must not be stretched to the block (%s): %s", k, body)
		}
	}
	var aspect float64
	if _, err := fmt.Sscan(decl["aspect-ratio"], &aspect); err != nil || aspect < frameW/frameH {
		t.Fatalf("map.css: aspect-ratio %q", decl["aspect-ratio"])
	}
	shown := frameW / aspect // units of the frame's height that always show, from its top
	w, err := parseWorld(worldBin)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"DE", "FR", "PL", "IT", "GB", "NL", "ES", "JP"} {
		c := w.country(code)
		if zoom, _ := c.fit(); zoom > 1 {
			t.Errorf("%s: zoomed out, not a country for this test", code)
			continue
		}
		bottom := pointY*frameH + c.pointLat() - (c.extLat - c.extLatSpan/2)
		if bottom > shown {
			t.Errorf("%s: its south end is %.1f units below the top of the frame, a wide block shows %.1f", code, bottom, shown)
		}
	}
}

// A country at the normal scale used to be placed by its label point alone, so
// one that reaches far north of it (Japan, Finland, Iran) was cut at the top
// of the frame. Its main extent now stays inside the frame, top and bottom.
func TestCountryStaysInTheFrame(t *testing.T) {
	w, err := parseWorld(worldBin)
	if err != nil {
		t.Fatal(err)
	}
	moved := 0
	for i := range w.countries {
		c := &w.countries[i]
		if zoom, _ := c.fit(); c.code == "" || zoom > 1 {
			continue
		}
		lat := c.pointLat()
		above, below := c.extLat+c.extLatSpan/2-lat, lat-(c.extLat-c.extLatSpan/2)
		if above > pointY*frameH-fitEdge+1e-9 || below > (1-pointY)*frameH-fitEdge+1e-9 {
			t.Errorf("%s: %.1f units above the point and %.1f below, the frame has %.1f and %.1f", c.code, above, below, pointY*frameH, (1-pointY)*frameH)
		}
		if lat != c.labelY {
			moved++
		}
		m, ok := w.countryMap(c.code)
		want, _ := w.drawMap(lat, c.labelX, c.code, false)
		if !ok || m != want {
			t.Errorf("%s: the map is not drawn at its point", c.code)
		}
	}
	// Most countries fit around their label point and keep it.
	if moved == 0 || moved > 40 {
		t.Errorf("%d countries moved off their label point", moved)
	}
	for _, code := range []string{"DE", "FR", "PL", "GB", "NL", "ES", "IT"} {
		if c := w.country(code); c.pointLat() != c.labelY {
			t.Errorf("%s: moved off its label point", code)
		}
	}
	if c := w.country("JP"); c.pointLat() <= c.labelY {
		t.Error("JP: not moved north")
	}
}

// The land that is not highlighted is drawn from coarse rings made at start;
// the highlighted country keeps every point of the data.
func TestCoarseLand(t *testing.T) {
	w, err := parseWorld(worldBin)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.cpts) == 0 || len(w.cpts) > len(w.pts)*85/100 {
		t.Errorf("%d coarse numbers for %d: more than 85%%, the land is not coarse", len(w.cpts), len(w.pts))
	}
	for i, p := range w.polys {
		n := p.cend - p.cstart
		if n < 3 || n > p.end-p.start {
			t.Fatalf("ring %d: %d coarse points of %d", i, n, p.end-p.start)
		}
		// every coarse point is a point of the ring, in order
		j := p.start
		for k := p.cstart; k < p.cend; k++ {
			for ; j < p.end && (w.pts[2*j] != w.cpts[2*k] || w.pts[2*j+1] != w.cpts[2*k+1]); j++ {
			}
			if j == p.end {
				t.Fatalf("ring %d: coarse point %d is not in the ring", i, k-p.cstart)
			}
			j++
		}
	}
	here := regexp.MustCompile(`class="here" d="([^"]*)"`)
	m, _ := w.countryMap("DE")
	// The same frame with every country's fine rings: the highlighted path is the same, the land is bigger.
	fine := *w
	fine.polys = append([]wpoly(nil), w.polys...)
	for i := range fine.polys {
		fine.polys[i].cstart, fine.polys[i].cend = fine.polys[i].start, fine.polys[i].end
	}
	fine.cpts = fine.pts
	f, _ := fine.countryMap("DE")
	if a, b := here.FindStringSubmatch(string(m)), here.FindStringSubmatch(string(f)); a == nil || b == nil || a[1] != b[1] {
		t.Error("DE: the highlighted country is not drawn from all its points")
	}
	if len(m) > 6000 || len(m)*100 > len(f)*85 {
		t.Errorf("DE: %d bytes, %d with fine land", len(m), len(f))
	}
}

// A long thin ring that the line simplification reduces to its two ends keeps
// all its points in the coarse land instead of vanishing.
func TestCoarseKeepsThinRing(t *testing.T) {
	thin := tpoly{{0, 0}, {300, 2}, {600, 0}, {1000, 2}}
	w := tiny(t, tcountry{code: "AA", label: [2]int{500, 0}, bbox: [4]int{0, 0, 1000, 2}, polygons: []tpoly{thin}})
	p := w.polys[0]
	if n := p.cend - p.cstart; n != 4 {
		t.Fatalf("%d coarse points, want all 4", n)
	}
}
