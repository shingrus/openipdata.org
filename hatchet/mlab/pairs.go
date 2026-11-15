package mlab

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"slices"
	"strconv"
)

// pair is one (client /24, hop IP) of the pairs file: key is the /24 (first three octets as a
// 24-bit number) shifted left by 2 with from_end (0 closest to the client .. 2) in the low bits;
// hop is the IPv4 address; traces is how many traces saw the hop there. 12 bytes, so 25M pairs
// are 300 MB.
type pair struct {
	key    uint32
	hop    uint32
	traces uint32
}

func (p pair) client() uint32 { return p.key >> 2 }
func (p pair) fromEnd() uint8 { return uint8(p.key & 3) }

func makePair(client uint32, fromEnd uint8, hop, traces uint32) pair {
	return pair{key: client<<2 | uint32(fromEnd), hop: hop, traces: traces}
}

// cmpPair orders by client /24, hop, then from_end.
func cmpPair(a, b pair) int {
	switch {
	case a.key>>2 != b.key>>2:
		if a.key>>2 < b.key>>2 {
			return -1
		}
		return 1
	case a.hop != b.hop:
		if a.hop < b.hop {
			return -1
		}
		return 1
	case a.key != b.key:
		if a.key < b.key {
			return -1
		}
		return 1
	}
	return 0
}

// compactPairs sorts pairs and merges the rows of one (client, hop): the smallest from_end and
// the sum of the traces (saturating). It works in place and returns the shortened slice.
func compactPairs(pairs []pair) []pair {
	slices.SortFunc(pairs, cmpPair)
	return mergeSorted(pairs)
}

func mergeSorted(pairs []pair) []pair {
	out := pairs[:0]
	for _, p := range pairs {
		if n := len(out); n > 0 && out[n-1].key>>2 == p.key>>2 && out[n-1].hop == p.hop {
			t := uint64(out[n-1].traces) + uint64(p.traces)
			out[n-1].traces = uint32(min(t, math.MaxUint32))
			continue // sorted by key: the first row has the smallest from_end
		}
		out = append(out, p)
	}
	return out
}

// distinctHops returns the hop IPs of pairs, sorted, each once.
func distinctHops(pairs []pair) []uint32 {
	hops := make([]uint32, len(pairs))
	for i, p := range pairs {
		hops[i] = p.hop
	}
	slices.Sort(hops)
	return slices.Compact(hops)
}

func addrOf(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func u32Of(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// appendClient writes the /24 as "a.b.c".
func appendClient(b []byte, c uint32) []byte {
	b = strconv.AppendUint(b, uint64(c>>16&255), 10)
	b = append(b, '.')
	b = strconv.AppendUint(b, uint64(c>>8&255), 10)
	b = append(b, '.')
	return strconv.AppendUint(b, uint64(c&255), 10)
}

func appendHop(b []byte, h uint32) []byte {
	b = appendClient(b, h>>8)
	b = append(b, '.')
	return strconv.AppendUint(b, uint64(h&255), 10)
}

// parseOctets reads "a.b.c" (three decimal octets, each 1 to 3 digits, no signs or spaces).
func parseClient(b []byte) (uint32, bool) {
	var v uint32
	for i := 0; i < 3; i++ {
		n, digits := 0, 0
		for len(b) > 0 && b[0] >= '0' && b[0] <= '9' && digits < 3 {
			n = n*10 + int(b[0]-'0')
			b = b[1:]
			digits++
		}
		if digits == 0 || n > 255 {
			return 0, false
		}
		v = v<<8 | uint32(n)
		if i < 2 {
			if len(b) == 0 || b[0] != '.' {
				return 0, false
			}
			b = b[1:]
		}
	}
	return v, len(b) == 0
}

// parseUint reads a decimal number of at most 18 digits.
func parseUint(b []byte) (uint64, bool) {
	if len(b) == 0 || len(b) > 18 {
		return 0, false
	}
	var n uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, true
}

const pairsHeader = "client24,hop_ip,from_end,traces"

// writePairs writes the pairs file (gzipped CSV, sorted by client /24 then hop) through a
// temporary file and a rename; the file it replaces is whole until then.
func writePairs(path string, pairs []pair) (int64, error) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp) // no-op after the rename
	gz, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	w := bufio.NewWriterSize(gz, 1<<20)
	w.WriteString(pairsHeader + "\n")
	var line []byte
	for _, p := range pairs {
		line = appendClient(line[:0], p.client())
		line = append(line, ',')
		line = appendHop(line, p.hop)
		line = append(line, ',')
		line = strconv.AppendUint(line, uint64(p.fromEnd()), 10)
		line = append(line, ',')
		line = strconv.AppendUint(line, uint64(p.traces), 10)
		line = append(line, '\n')
		if _, err := w.Write(line); err != nil {
			f.Close()
			return 0, err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return 0, err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		return 0, err
	}
	return fi.Size(), os.Rename(tmp, path)
}

// errNoPairs is returned when the pairs file is missing.
var errNoPairs = permanent("no %s (has the import task run?)", pairsFile)

// readPairs reads the pairs file. A damaged line is an error (the file is ours). A file whose
// rows are not in order is sorted and merged.
func readPairs(path string) ([]pair, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, errNoPairs
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", pairsFile, err)
	}
	defer gz.Close()
	r := bufio.NewReaderSize(gz, 1<<20)
	header, err := readLine(r)
	if err != nil || string(header) != pairsHeader {
		return nil, fmt.Errorf("%s: unexpected header", pairsFile)
	}
	var pairs []pair
	ordered := true
	for {
		line, err := readLine(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pairsFile, err)
		}
		p, ok := parsePairLine(line)
		if !ok {
			return nil, fmt.Errorf("%s: damaged line %d", pairsFile, len(pairs)+2)
		}
		if n := len(pairs); n > 0 && cmpPair(pairs[n-1], p) >= 0 {
			ordered = false
		}
		pairs = append(pairs, p)
	}
	if !ordered {
		pairs = compactPairs(pairs)
	}
	return pairs, nil
}

var errLineTooLong = errors.New("line too long")

// readLine returns the next line without its newline (and without a carriage return). The slice
// is valid until the next call. io.EOF comes only when there is no more data.
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		for err == bufio.ErrBufferFull {
			_, err = r.ReadSlice('\n')
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		return nil, errLineTooLong
	}
	if err == io.EOF && len(line) > 0 {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	return bytes.TrimSuffix(line, []byte{'\r'}), nil
}

func parsePairLine(line []byte) (pair, bool) {
	f := bytes.Split(line, []byte{','})
	if len(f) != 4 {
		return pair{}, false
	}
	client, ok1 := parseClient(f[0])
	hop, err := netip.ParseAddr(string(f[1]))
	from, ok2 := parseUint(f[2])
	traces, ok3 := parseUint(f[3])
	if !ok1 || err != nil || !hop.Is4() || !ok2 || from > 2 || !ok3 || traces == 0 || traces > math.MaxUint32 {
		return pair{}, false
	}
	return makePair(client, uint8(from), u32Of(hop), uint32(traces)), true
}
