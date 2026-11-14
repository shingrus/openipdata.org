package main

import (
	"bytes"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

func TestAppendControlSizes(t *testing.T) {
	for _, tc := range []struct {
		typ, size int
		want      []byte
	}{
		{typeString, 0, []byte{0x40}},
		{typeString, 28, []byte{0x40 | 28}},
		{typeString, 29, []byte{0x40 | 29, 0}},
		{typeString, 284, []byte{0x40 | 29, 255}},
		{typeString, 285, []byte{0x40 | 30, 0, 0}},
		{typeString, 65820, []byte{0x40 | 30, 255, 255}},
		{typeString, 65821, []byte{0x40 | 31, 0, 0, 0}},
		{typeString, 65821 + 0x010203, []byte{0x40 | 31, 1, 2, 3}},
		{typeMap, 9, []byte{0xe9}},
		{typeArray, 2, []byte{0x02, 4}},     // extended: type 11 = 7 + 4
		{typeArray, 30, []byte{0x1d, 4, 1}}, // extended type byte, then the 29 rule: 29 + 1
		{typeUint64, 3, []byte{0x03, 2}},
	} {
		got, err := appendControl(nil, tc.typ, tc.size)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("type %d size %d = %x, want %x", tc.typ, tc.size, got, tc.want)
		}
	}
	if _, err := appendControl(nil, typeString, 65821+1<<24); err == nil {
		t.Error("a size over the limit was accepted")
	}
}

func TestAppendUint(t *testing.T) {
	for _, tc := range []struct {
		typ  int
		v    uint64
		want []byte
	}{
		{typeUint16, 0, []byte{0xa0}},
		{typeUint16, 2, []byte{0xa1, 2}},
		{typeUint16, 256, []byte{0xa2, 1, 0}},
		{typeUint32, 70000, []byte{0xc3, 1, 0x11, 0x70}},
		{typeUint64, 0, []byte{0x00, 2}},
		{typeUint64, 1<<64 - 1, []byte{0x08, 2, 255, 255, 255, 255, 255, 255, 255, 255}},
	} {
		got, err := appendUint(nil, tc.typ, tc.v)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("type %d value %d = %x, want %x", tc.typ, tc.v, got, tc.want)
		}
	}
	if _, err := appendUint(nil, typeUint16, 65536); err == nil {
		t.Error("an overflowing uint16 was accepted")
	}
}

func openMeta(t *testing.T, path string) maxminddb.Metadata {
	t.Helper()
	r, err := maxminddb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Verify(); err != nil {
		t.Fatal(err)
	}
	return r.Metadata
}

func lookupString(t *testing.T, path, ip string) string {
	t.Helper()
	r, err := maxminddb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	res := r.Lookup(netip.MustParseAddr(ip))
	var rec map[string]any
	if res.Found() {
		if err := res.Decode(&rec); err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprint(res.Found(), res.Prefix(), rec)
}

func stampFixture(t *testing.T) (dir, db string) {
	dir = t.TempDir()
	return dir, lookupDB(t, dir)
}

func TestStamp(t *testing.T) {
	dir, db := stampFixture(t)
	meta := writeFile(t, filepath.Join(dir, "meta.json"),
		"{\n  \"date_tag\": \"2026-11-14\",\n  \"built_at\": \"2026-11-14T07:42:10Z\",\n  \"eval_id\": 22,\n  \"metrics\": {\"city\": 30.5}\n}\n")
	out := filepath.Join(dir, "stamped.mmdb")
	if err := stampMMDB(db, meta, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left: %v", err)
	}
	// Everything up to and including the marker is the same bytes.
	in, _ := os.ReadFile(db)
	got, _ := os.ReadFile(out)
	i, j := bytes.LastIndex(in, metadataMarker), bytes.LastIndex(got, metadataMarker)
	if i != j || !bytes.Equal(in[:i+len(metadataMarker)], got[:j+len(metadataMarker)]) {
		t.Error("search tree or data section changed")
	}

	a, b := openMeta(t, db), openMeta(t, out)
	if a.BuildEpoch != b.BuildEpoch || a.NodeCount != b.NodeCount || a.RecordSize != b.RecordSize ||
		a.IPVersion != b.IPVersion || a.DatabaseType != b.DatabaseType || !reflect.DeepEqual(a.Languages, b.Languages) ||
		a.BinaryFormatMajorVersion != b.BinaryFormatMajorVersion || a.BinaryFormatMinorVersion != b.BinaryFormatMinorVersion {
		t.Errorf("metadata changed: %+v -> %+v", a, b)
	}
	want := `{"built_at":"2026-11-14T07:42:10Z","date_tag":"2026-11-14","eval_id":22,"metrics":{"city":30.5}}`
	if b.Description[stampKey] != want {
		t.Errorf("openipdata = %q", b.Description[stampKey])
	}
	if b.Description["en"] != "openipdata.org ip2geo, built 2026-11-14 07:42 UTC" {
		t.Errorf("en = %q", b.Description["en"])
	}

	// Stamping the stamped file again replaces the description; in place works.
	meta2 := writeFile(t, filepath.Join(dir, "meta2.json"), `{"date_tag":"2026-11-15"}`)
	if err := stampMMDB(out, meta2, out); err != nil {
		t.Fatal(err)
	}
	c := openMeta(t, out)
	if len(c.Description) != 2 || !strings.HasPrefix(c.Description[stampKey], `{"built_at":"`) ||
		!strings.Contains(c.Description[stampKey], `"date_tag":"2026-11-15"`) {
		t.Errorf("second stamp: %v", c.Description)
	}
	if c.NodeCount != a.NodeCount {
		t.Error("node count changed")
	}
}

func TestStampLookupsUnchanged(t *testing.T) {
	dir, db := stampFixture(t)
	meta := writeFile(t, filepath.Join(dir, "meta.json"), `{"a":1}`)
	out := filepath.Join(dir, "s.mmdb")
	if err := stampMMDB(db, meta, out); err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"45.0.0.7", "45.0.5.9", "46.0.0.1", "47.0.0.1"} {
		if x, y := lookupString(t, db, ip), lookupString(t, out, ip); x != y {
			t.Errorf("%s: %s vs %s", ip, x, y)
		}
	}
}

func TestStampRejectsBadMeta(t *testing.T) {
	dir, db := stampFixture(t)
	for name, body := range map[string]string{
		"array":  `[1,2]`,
		"string": `"x"`,
		"null":   `null`,
		"broken": `{`,
		"big":    `{"x":"` + strings.Repeat("a", 70<<10) + `"}`,
	} {
		meta := writeFile(t, filepath.Join(dir, name+".json"), body)
		out := filepath.Join(dir, name+".mmdb")
		if err := stampMMDB(db, meta, out); err == nil {
			t.Errorf("%s: accepted", name)
		}
		for _, p := range []string{out, out + ".tmp"} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s: %s left behind", name, p)
			}
		}
	}
	// A big but allowed meta (over the 285 size step, under the limit) still reads back.
	meta := writeFile(t, filepath.Join(dir, "ok.json"), `{"x":"`+strings.Repeat("a", 60000)+`"}`)
	out := filepath.Join(dir, "ok.mmdb")
	if err := stampMMDB(db, meta, out); err != nil {
		t.Fatal(err)
	}
	if got := openMeta(t, out).Description[stampKey]; !strings.Contains(got, strings.Repeat("a", 60000)) {
		t.Error("long stamp not read back")
	}
}

// The lookup report shows description.openipdata, cut to 200 characters.
func TestLookupReportShowsStamp(t *testing.T) {
	dir, db := stampFixture(t)
	meta := writeFile(t, filepath.Join(dir, "meta.json"), `{"x":"`+strings.Repeat("a", 500)+`"}`)
	out := filepath.Join(dir, "s.mmdb")
	if err := stampMMDB(db, meta, out); err != nil {
		t.Fatal(err)
	}
	report := func(path string) string {
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		err := printLookups(path, "45.0.0.7", "")
		w.Close()
		os.Stdout = old
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(r)
		return string(data)
	}
	var line string
	for _, l := range strings.Split(report(out), "\n") {
		if strings.HasPrefix(l, "  description.openipdata: ") {
			line = l
		}
	}
	val := strings.TrimPrefix(line, "  description.openipdata: ")
	if line == "" || len(val) != 203 || !strings.HasSuffix(val, "...") || !strings.HasPrefix(val, `{"built_at":`) {
		t.Errorf("line = %q", line)
	}
	if strings.Contains(report(db), "description.openipdata") {
		t.Error("unstamped file shows the line")
	}
}
