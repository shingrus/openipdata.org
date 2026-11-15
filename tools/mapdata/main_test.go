package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"strings"
	"testing"
)

func pts(xy ...float64) []point {
	var out []point
	for i := 0; i < len(xy); i += 2 {
		out = append(out, point{xy[i], xy[i+1]})
	}
	return out
}

func TestDouglasPeucker(t *testing.T) {
	// A nearly straight line keeps its ends; a real corner stays.
	line := pts(0, 0, 1, 0.01, 2, -0.01, 3, 0.005, 4, 0)
	if got := dp(line, 0.05); len(got) != 2 {
		t.Errorf("straight line: %v", got)
	}
	corner := pts(0, 0, 1, 0.01, 2, 0, 2, 1, 2, 2)
	if got := dp(corner, 0.05); len(got) != 3 || got[1] != (point{2, 0}) {
		t.Errorf("corner: %v", got)
	}
	// A tolerance of zero keeps every point that is off the line (not the one on it).
	if got := dp(corner, 0); len(got) != 4 {
		t.Errorf("tolerance 0: %v", got)
	}
	if got := dp(pts(0, 0, 1, 1), 1); len(got) != 2 {
		t.Errorf("two points: %v", got)
	}
}

func TestSimplifyRing(t *testing.T) {
	// A square with many points along its sides becomes four corners, whatever point the ring starts at.
	var ring []point
	for i := 0; i < 10; i++ {
		ring = append(ring, point{float64(i), 0})
	}
	for i := 0; i < 10; i++ {
		ring = append(ring, point{10, float64(i)})
	}
	for i := 10; i > 0; i-- {
		ring = append(ring, point{float64(i), 10})
	}
	for i := 10; i > 0; i-- {
		ring = append(ring, point{0, float64(i)})
	}
	for _, shift := range []int{0, 5, 17, 33} {
		r := append(append([]point{}, ring[shift:]...), ring[:shift]...)
		if got := simplifyRing(r, 0.05); len(got) != 4 {
			t.Errorf("start %d: %d points: %v", shift, len(got), got)
		}
	}
	if got := simplifyRing(pts(0, 0, 1, 0, 0, 1), 1); len(got) != 3 {
		t.Errorf("triangle: %v", got)
	}
}

func TestQuantise(t *testing.T) {
	// Rounded to 0.01 degree, made counter-clockwise, repeated points dropped.
	cw := pts(0, 0, 0, 1, 1, 1, 1, 0) // clockwise
	q := quantise(cw)
	if len(q) != 4 || area(q) <= 0 || q[0] == (point{0, 0}) && q[1] == (point{0, 100}) {
		t.Errorf("not counter-clockwise: %v", q)
	}
	if got := quantise(pts(0, 0, 0.001, 0.001, 1, 0, 1, 1)); len(got) != 3 {
		t.Errorf("repeated point: %v", got)
	}
	for name, r := range map[string][]point{
		"collapsed":  pts(0, 0, 0.001, 0, 0, 0.001),
		"a line":     pts(0, 0, 1, 1, 2, 2),
		"two points": pts(0, 0, 1, 1),
	} {
		if got := quantise(r); got != nil {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestBuildCountryKeepsLargest(t *testing.T) {
	big := pts(0, 0, 5, 0, 5, 5, 0, 5)
	tiny := pts(10, 10, 10.05, 10, 10.05, 10.05, 10, 10.05) // 0.0025 square degrees
	mid := pts(20, 20, 20.5, 20, 20.5, 20.5, 20, 20.5)      // 0.25 square degrees
	var c country
	buildCountry(&c, [][]point{big, tiny, mid}, 0.05, 0.02)
	if len(c.polygons) != 2 {
		t.Errorf("%d polygons, want the big and the mid one", len(c.polygons))
	}
	// A country made of one tiny ring (an island state) still has it.
	var s country
	buildCountry(&s, [][]point{tiny}, 0.05, 0.02)
	if len(s.polygons) != 1 {
		t.Errorf("island state lost its only ring: %v", s.polygons)
	}
	// Even when the tolerance is larger than the ring.
	var u country
	buildCountry(&u, [][]point{pts(103.6, 1.2, 104.0, 1.2, 104.0, 1.45, 103.6, 1.45)}, 0.5, 0.02)
	if len(u.polygons) != 1 {
		t.Errorf("Singapore-like ring lost: %v", u.polygons)
	}
	// A degenerate ring cannot be kept.
	var z country
	buildCountry(&z, [][]point{pts(0, 0, 1, 1, 2, 2)}, 0.05, 0.02)
	if len(z.polygons) != 0 {
		t.Errorf("a line became a polygon: %v", z.polygons)
	}
}

const sample = `{"type":"FeatureCollection","features":[
{"type":"Feature","properties":{"NAME":"Portugal","ISO_A2":"PT","ISO_A2_EH":"PT","ADM0_A3":"PRT","LABEL_X":-8.2,"LABEL_Y":39.6},
 "geometry":{"type":"Polygon","coordinates":[[[-9,37],[-6,37],[-6,42],[-9,42],[-9,37]],[[-8,38],[-7,38],[-7,39],[-8,38]]]}},
{"type":"Feature","properties":{"NAME":"France","ISO_A2":"-99","ISO_A2_EH":"FR","ADM0_A3":"FRA","LABEL_X":2.5,"LABEL_Y":46.7},
 "geometry":{"type":"MultiPolygon","coordinates":[[[[0,43],[5,43],[5,49],[0,49],[0,43]]],[[[8,41],[9.5,41],[9.5,43],[8,43],[8,41]]]]}},
{"type":"Feature","properties":{"NAME":"Somaliland","ISO_A2":"-99","ISO_A2_EH":"-99","ADM0_A3":"SOL","LABEL_X":46.7,"LABEL_Y":9.4},
 "geometry":{"type":"Polygon","coordinates":[[[44,8],[48,8],[48,11],[44,11],[44,8]]]}},
{"type":"Feature","properties":{"NAME":"Australia","ISO_A2":"AU","ISO_A2_EH":"AU","ADM0_A3":"AUS","LABEL_X":134,"LABEL_Y":-25},
 "geometry":{"type":"Polygon","coordinates":[[[113,-39],[153,-39],[153,-11],[113,-11],[113,-39]]]}},
{"type":"Feature","properties":{"NAME":"Indian Ocean Ter.","ISO_A2":"-99","ISO_A2_EH":"AU","ADM0_A3":"IOT","LABEL_X":105,"LABEL_Y":-10},
 "geometry":{"type":"Polygon","coordinates":[[[105,-11],[106,-11],[106,-10],[105,-10],[105,-11]]]}}
]}`

// decode reads the format of the comment at the top of main.go, independently of the writer.
func decode(t *testing.T, b []byte) []country {
	t.Helper()
	if string(b[:4]) != "OIW1" {
		t.Fatal("magic")
	}
	if crc32.ChecksumIEEE(b[:len(b)-4]) != binary.LittleEndian.Uint32(b[len(b)-4:]) {
		t.Fatal("crc")
	}
	r := bytes.NewReader(b[:len(b)-4])
	r.Seek(4, 0)
	var n uint16
	binary.Read(r, binary.LittleEndian, &n)
	var out []country
	for ; n > 0; n-- {
		var code [2]byte
		r.Read(code[:])
		var v [6]int16
		binary.Read(r, binary.LittleEndian, &v)
		c := country{lx: float64(v[0]) / 100, ly: float64(v[1]) / 100}
		if code[0] != 0 {
			c.code = string(code[:])
		}
		var np uint16
		binary.Read(r, binary.LittleEndian, &np)
		minx, miny, maxx, maxy := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
		for ; np > 0; np-- {
			cnt, _ := binary.ReadUvarint(r)
			var first [2]int16
			binary.Read(r, binary.LittleEndian, &first)
			p := []point{{float64(first[0]), float64(first[1])}}
			for i := uint64(1); i < cnt; i++ {
				dx, _ := binary.ReadVarint(r)
				dy, _ := binary.ReadVarint(r)
				p = append(p, point{p[i-1].x + float64(dx), p[i-1].y + float64(dy)})
			}
			for _, q := range p {
				minx, maxx = math.Min(minx, q.x), math.Max(maxx, q.x)
				miny, maxy = math.Min(miny, q.y), math.Max(maxy, q.y)
			}
			c.polygons = append(c.polygons, p)
		}
		if len(c.polygons) > 0 && (float64(v[2]) != minx || float64(v[3]) != miny || float64(v[4]) != maxx || float64(v[5]) != maxy) {
			t.Errorf("%q: bbox %v, points say %v %v %v %v", c.code, v[2:], minx, miny, maxx, maxy)
		}
		out = append(out, c)
	}
	if r.Len() != 0 {
		t.Errorf("%d bytes left", r.Len())
	}
	return out
}

func TestReadAndEncode(t *testing.T) {
	cs, err := readCountries([]byte(sample), 0.05, 0.02)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 5 {
		t.Fatalf("%d entries", len(cs))
	}
	var codes []string
	for _, c := range cs {
		codes = append(codes, c.code)
	}
	// sorted by code; no code first: Somaliland and the Indian Ocean Territories (which lost AU to Australia)
	if got := strings.Join(codes, ","); got != ",,AU,FR,PT" {
		t.Errorf("codes %q", got)
	}
	b, err := encode(cs)
	if err != nil {
		t.Fatal(err)
	}
	back := decode(t, b)
	if len(back) != len(cs) {
		t.Fatalf("round trip: %d entries", len(back))
	}
	for i, c := range cs {
		if back[i].code != c.code || len(back[i].polygons) != len(c.polygons) || back[i].lx != math.Round(c.lx*100)/100 {
			t.Errorf("entry %d: %+v vs %+v", i, back[i], c)
		}
		for j, p := range c.polygons {
			if fmt.Sprint(back[i].polygons[j]) != fmt.Sprint(p) {
				t.Errorf("%q polygon %d: %v vs %v", c.code, j, back[i].polygons[j], p)
			}
		}
	}
	// holes are dropped: Portugal has one outer ring
	for _, c := range cs {
		if c.code == "PT" && len(c.polygons) != 1 {
			t.Errorf("PT has %d polygons", len(c.polygons))
		}
		if c.code == "FR" && len(c.polygons) != 2 {
			t.Errorf("FR has %d polygons", len(c.polygons))
		}
		for _, p := range c.polygons {
			if area(p) <= 0 {
				t.Errorf("%q: ring is not counter-clockwise", c.code)
			}
		}
	}
}

func TestReadErrors(t *testing.T) {
	for name, in := range map[string]string{
		"not json":      "x",
		"no features":   `{"features":[]}`,
		"bad geometry":  `{"features":[{"properties":{"NAME":"A"},"geometry":{"type":"Point","coordinates":[1,2]}}]}`,
		"bad ring data": `{"features":[{"properties":{"NAME":"A"},"geometry":{"type":"Polygon","coordinates":"x"}}]}`,
	} {
		if _, err := readCountries([]byte(in), 0.05, 0.02); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A point outside the globe cannot be written.
	cs := []country{{code: "AA", polygons: [][]point{{{40000, 0}, {40001, 0}, {40001, 1}}}}}
	if _, err := encode(cs); err == nil {
		t.Error("encode accepted a point outside the file range")
	}
}
