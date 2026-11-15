package bgp

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func v(a, b uint32) uint32 { return a<<16 | b }

// dump builds a file of n prefixes whose values come from f.
func dump(n int, f func(i int) []uint32) []byte {
	parts := [][]byte{peerIndex(1)}
	for i := 0; i < n; i++ {
		parts = append(parts, rib(subRIBIPv4Unicast, fmt.Sprintf("10.%d.%d.0/24", i/256, i%256), entry(0, comms(false, f(i)...))))
	}
	return join(parts...)
}

func readOut(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	sc := bufio.NewScanner(gz)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

func TestExtractFilters(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.gz"), filepath.Join(dir, "out.csv.gz")
	// 100 prefixes. 1:1 in 6 prefixes (6%, more than 5%: common); 1:2 in 5 (exactly 5%: kept);
	// 1:3 in 3 (kept); 1:4 in 2 and 1:5 in 1 (fewer than 3: dropped); one large value in 4.
	gzipTo(t, in, dump(100, func(i int) []uint32 {
		var vals []uint32
		if i < 6 {
			vals = append(vals, v(1, 1))
		}
		if i >= 10 && i < 15 {
			vals = append(vals, v(1, 2))
		}
		if i >= 20 && i < 23 {
			vals = append(vals, v(1, 3))
		}
		if i >= 30 && i < 32 {
			vals = append(vals, v(1, 4))
		}
		if i == 40 {
			vals = append(vals, v(1, 5))
		}
		return vals
	}))
	res, err := ExtractFile(context.Background(), in, out, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Prefixes != 100 || res.Peers != 1 || res.ValuesSeen != 5 || res.ValuesKept != 2 || res.ValuesCommon != 1 || res.ValuesRare != 2 || res.Pairs != 8 {
		t.Errorf("%+v", res)
	}
	lines := readOut(t, out)
	if len(lines) != 101 || lines[0] != "prefix,communities" {
		t.Fatalf("%d lines, first %q", len(lines), lines[0])
	}
	// Every prefix has a row, also one with no value left; the values are in numeric order.
	for i, want := range map[int]string{0: "10.0.0.0/24,", 10: "10.0.10.0/24,1:2", 22: "10.0.22.0/24,1:3", 30: "10.0.30.0/24,", 99: "10.0.99.0/24,"} {
		if lines[i+1] != want {
			t.Errorf("row %d = %q, want %q", i, lines[i+1], want)
		}
	}
	if res.Bytes == 0 {
		t.Error("no size")
	}
	if _, err := os.Stat(out + ".tmp"); err == nil {
		t.Error("temporary file left")
	}
}

func TestExtractFormat(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.gz"), filepath.Join(dir, "out.csv.gz")
	// Standard values come before large ones, each group in numeric order, a value once per row.
	parts := [][]byte{peerIndex(2)}
	for i := 0; i < 3; i++ {
		parts = append(parts, rib(subRIBIPv4Unicast, fmt.Sprintf("192.0.%d.0/24", i),
			entry(0, comms(false, v(200, 1), v(100, 9)), large([3]uint32{7, 7, 7}, [3]uint32{7, 7, 1})),
			entry(1, comms(false, v(100, 9)))))
	}
	// Add filler so the values are not common (3 of 40 is 7.5%: use more prefixes without values).
	for i := 0; i < 57; i++ {
		parts = append(parts, rib(subRIBIPv4Unicast, fmt.Sprintf("198.51.%d.0/24", i), entry(0)))
	}
	gzipTo(t, in, join(parts...))
	if _, err := ExtractFile(context.Background(), in, out, 1); err != nil {
		t.Fatal(err)
	}
	lines := readOut(t, out)
	if lines[1] != "192.0.0.0/24,100:9 200:1 7:7:1 7:7:7" {
		t.Errorf("row = %q", lines[1])
	}
	for _, l := range lines[1:] {
		if strings.Count(l, ",") != 1 {
			t.Errorf("row %q", l)
		}
	}
}

func TestExtractErrors(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.gz"), filepath.Join(dir, "out.csv.gz")
	gzipTo(t, in, dump(10, func(int) []uint32 { return nil }))
	// Too few prefixes: no output.
	if _, err := ExtractFile(context.Background(), in, out, 11); !errors.Is(err, ErrTooFewPrefixes) {
		t.Errorf("few prefixes: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("output written for a bad dump")
	}
	// A damaged MRT stream inside a good gzip.
	gzipTo(t, in, dump(10, func(int) []uint32 { return nil })[:200])
	if _, err := ExtractFile(context.Background(), in, out, 0); !errors.Is(err, ErrDamaged) {
		t.Errorf("truncated MRT: %v", err)
	}
	// A truncated gzip file and a file that is no gzip at all.
	gzipTo(t, in, dump(10, func(int) []uint32 { return nil }))
	data, _ := os.ReadFile(in)
	os.WriteFile(in, data[:len(data)-12], 0o640)
	if _, err := ExtractFile(context.Background(), in, out, 0); err == nil {
		t.Error("truncated gzip accepted")
	}
	os.WriteFile(in, []byte("<html>not a dump</html>"), 0o640)
	if _, err := ExtractFile(context.Background(), in, out, 0); !errors.Is(err, ErrDamaged) {
		t.Errorf("no gzip: %v", err)
	}
	if _, err := ExtractFile(context.Background(), filepath.Join(dir, "missing"), out, 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("output written after errors")
	}
	if _, err := os.Stat(out + ".tmp"); err == nil {
		t.Error("temporary file left")
	}
	// A cancelled run stops.
	gzipTo(t, in, dump(10000, func(int) []uint32 { return nil }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExtractFile(ctx, in, out, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}
