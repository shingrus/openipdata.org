package main

// Tests of the world.bin reader. The file is written here by test code that
// follows the format comment of world.go, not by the tool that makes it.

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"
)

type tpoly [][2]int // points in file units (0.01 degree)

type tcountry struct {
	code     string // "" = two zero bytes
	label    [2]int
	bbox     [4]int
	polygons []tpoly
}

// writeWorld writes the format of world.go.
func writeWorld(cs []tcountry) []byte {
	var b bytes.Buffer
	b.WriteString("OIW1")
	binary.Write(&b, binary.LittleEndian, uint16(len(cs)))
	tmp := make([]byte, binary.MaxVarintLen64)
	for _, c := range cs {
		if c.code == "" {
			b.Write([]byte{0, 0})
		} else {
			b.WriteString(c.code)
		}
		for _, v := range append(c.label[:], c.bbox[:]...) {
			binary.Write(&b, binary.LittleEndian, int16(v))
		}
		binary.Write(&b, binary.LittleEndian, uint16(len(c.polygons)))
		for _, p := range c.polygons {
			b.Write(tmp[:binary.PutUvarint(tmp, uint64(len(p)))])
			binary.Write(&b, binary.LittleEndian, []int16{int16(p[0][0]), int16(p[0][1])})
			for i := 1; i < len(p); i++ {
				b.Write(tmp[:binary.PutVarint(tmp, int64(p[i][0]-p[i-1][0]))])
				b.Write(tmp[:binary.PutVarint(tmp, int64(p[i][1]-p[i-1][1]))])
			}
		}
	}
	binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(b.Bytes()))
	return b.Bytes()
}

// withCRC makes the checksum right again after a test changed the body.
func withCRC(body []byte) []byte {
	return binary.LittleEndian.AppendUint32(append([]byte{}, body...), crc32.ChecksumIEEE(body))
}

func square(x0, y0, x1, y1 int) tpoly { return tpoly{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}} }

func TestParseWorldRoundTrip(t *testing.T) {
	cs := []tcountry{
		{code: "", label: [2]int{5, 6}, bbox: [4]int{0, 0, 10, 10}, polygons: []tpoly{square(0, 0, 10, 10)}},
		{code: "AA", label: [2]int{-1500, 2500}, bbox: [4]int{-18000, -9000, 18000, 9000},
			polygons: []tpoly{{{-18000, -9000}, {18000, -9000}, {0, 9000}}, square(-300, -250, 400, 350)}},
		{code: "ZZ", label: [2]int{1, 2}, bbox: [4]int{1, 2, 1, 2}}, // no polygon kept
	}
	w, err := parseWorld(writeWorld(cs))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.countries) != 3 || len(w.polys) != 3 || len(w.pts) != (4+3+4)*2 {
		t.Fatalf("countries %d polys %d pts %d", len(w.countries), len(w.polys), len(w.pts))
	}
	a := w.country("AA")
	if a == nil || a.code != "AA" || a.labelX != -15 || a.labelY != 25 || a.minX != -180 || a.maxY != 90 || a.count != 2 {
		t.Fatalf("AA: %+v", a)
	}
	// the second polygon of AA: its points and bounding box
	p := w.polys[a.first+1]
	if got := w.pts[p.start*2 : p.end*2]; len(got) != 8 || got[0] != -300 || got[1] != -250 || got[4] != 400 || got[5] != 350 {
		t.Errorf("points %v", got)
	}
	if p.minX != -3 || p.maxX != 4 || p.minY != -2.5 || p.maxY != 3.5 {
		t.Errorf("polygon bbox %+v", p)
	}
	if z := w.country("ZZ"); z == nil || z.count != 0 || z.labelX != 0.01 {
		t.Errorf("ZZ: %+v", z)
	}
	for _, code := range []string{"", "aa", "A", "AAA", "A1", "BB", "\x00\x00"} {
		if w.country(code) != nil {
			t.Errorf("country(%q) found", code)
		}
	}
	var nw *world
	if nw.country("AA") != nil {
		t.Error("nil world")
	}
}

func TestParseWorldRejects(t *testing.T) {
	good := writeWorld([]tcountry{{code: "AA", label: [2]int{0, 0}, bbox: [4]int{0, 0, 10, 10}, polygons: []tpoly{square(0, 0, 10, 10)}}})
	if _, err := parseWorld(good); err != nil {
		t.Fatal(err)
	}
	body := good[:len(good)-4]
	flip := append([]byte{}, good...)
	flip[12] ^= 0xff
	oneCountry := func(c tcountry) []byte { return writeWorld([]tcountry{c}) }
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"short", []byte("OIW1")},
		{"wrong magic", withCRC(append([]byte("OIW2"), body[4:]...))},
		{"flipped byte", flip},
		{"bad checksum", append(append([]byte{}, body...), 1, 2, 3, 4)},
		{"truncated", withCRC(body[:len(body)-3])},
		{"trailing bytes", withCRC(append(append([]byte{}, body...), 0, 0))},
		{"no countries", writeWorld(nil)},
		{"code twice", writeWorld([]tcountry{{code: "AA"}, {code: "AA"}})},
		{"lower case code", oneCountry(tcountry{code: "aa"})},
		{"digit in code", oneCountry(tcountry{code: "A1"})},
		{"two points", oneCountry(tcountry{code: "AA", polygons: []tpoly{{{0, 0}, {1, 1}}}})},
		{"longitude too big", oneCountry(tcountry{code: "AA", polygons: []tpoly{{{18001, 0}, {0, 1}, {1, 1}}}})},
		{"latitude too big", oneCountry(tcountry{code: "AA", polygons: []tpoly{{{0, 9001}, {0, 1}, {1, 1}}}})},
		{"delta leaves the globe", oneCountry(tcountry{code: "AA", polygons: []tpoly{{{17000, 0}, {18000, 0}, {19000, 5}}}})},
	} {
		if _, err := parseWorld(c.data); err == nil {
			t.Errorf("%s: no error", c.name)
		} else if !strings.Contains(err.Error(), "world.bin") {
			t.Errorf("%s: unclear error %v", c.name, err)
		}
	}
}

// The embedded file: it decodes, and small states are still there.
func TestEmbeddedWorld(t *testing.T) {
	w, err := parseWorld(worldBin)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.countries) < 235 {
		t.Errorf("%d entries", len(w.countries))
	}
	for _, code := range []string{"SG", "MT", "HK", "BH", "MC", "VA", "MV", "FR", "NO", "XK", "PT", "US", "RU", "ID", "FJ", "AU"} {
		if c := w.country(code); c == nil || c.count == 0 {
			t.Errorf("%s is missing or has no outline", code)
		}
	}
	if c := w.country("PT"); c.labelX < -10 || c.labelX > -6 || c.labelY < 37 || c.labelY > 42 {
		t.Errorf("PT label point %v %v", c.labelX, c.labelY)
	}
	// every polygon of every country lies inside the bounding box of its country
	for _, c := range w.countries {
		for _, p := range w.polys[c.first : c.first+c.count] {
			if p.minX < c.minX || p.maxX > c.maxX || p.minY < c.minY || p.maxY > c.maxY {
				t.Errorf("%q: polygon outside the country bbox", c.code)
			}
		}
	}
	// a damaged copy of the embedded file stops the start
	bad := append([]byte{}, worldBin...)
	bad[len(bad)/2] ^= 1
	if _, err := parseWorld(bad); err == nil {
		t.Error("damaged world.bin accepted")
	}
}

func BenchmarkParseWorld(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := parseWorld(worldBin); err != nil {
			b.Fatal(err)
		}
	}
}
