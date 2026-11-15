package bgp

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"math/rand"
	"net/netip"
	"os"
	"slices"
	"testing"
)

// Hand-built MRT data.

func mrtRecord(typ, sub uint16, body []byte) []byte {
	b := make([]byte, 12, 12+len(body))
	binary.BigEndian.PutUint16(b[4:], typ)
	binary.BigEndian.PutUint16(b[6:], sub)
	binary.BigEndian.PutUint32(b[8:], uint32(len(body)))
	return append(b, body...)
}

// peerIndex is a PEER_INDEX_TABLE with n peers (alternating IPv4 and IPv6 addresses, 2 and 4 byte AS numbers).
func peerIndex(n int) []byte {
	body := []byte{0, 0, 0, 0, 0, 3, 'v', 'i', 'w'}
	body = binary.BigEndian.AppendUint16(body, uint16(n))
	for i := 0; i < n; i++ {
		pt := byte(0)
		if i%2 == 1 {
			pt = 1 | 2 // IPv6 address, 4-byte AS
		}
		body = append(body, pt, 0, 0, 0, byte(i))
		if pt&1 != 0 {
			body = append(body, make([]byte, 16)...)
		} else {
			body = append(body, 10, 0, 0, byte(i))
		}
		if pt&2 != 0 {
			body = append(body, 0, 0, 0xfd, 0xe8)
		} else {
			body = append(body, 0xfd, 0xe8)
		}
	}
	return mrtRecord(mrtTableDumpV2, subPeerIndexTable, body)
}

func attr(flags, typ byte, v []byte) []byte {
	if flags&0x10 != 0 {
		return append(binary.BigEndian.AppendUint16([]byte{flags, typ}, uint16(len(v))), v...)
	}
	return append([]byte{flags, typ, byte(len(v))}, v...)
}

// asPath is an attribute the reader ignores.
func asPath() []byte { return attr(0x40, 2, []byte{2, 1, 0, 0, 0xfd, 0xe8}) }

func comms(ext bool, vals ...uint32) []byte {
	var v []byte
	for _, x := range vals {
		v = binary.BigEndian.AppendUint32(v, x)
	}
	flags := byte(0xc0)
	if ext {
		flags |= 0x10
	}
	return attr(flags, 8, v)
}

func large(vals ...[3]uint32) []byte {
	var v []byte
	for _, x := range vals {
		v = binary.BigEndian.AppendUint32(v, x[0])
		v = binary.BigEndian.AppendUint32(v, x[1])
		v = binary.BigEndian.AppendUint32(v, x[2])
	}
	return attr(0xd0, 32, v) // optional, transitive, extended length
}

// entry is one RIB entry of the peer with the attributes.
func entry(peer int, attrs ...[]byte) []byte {
	var a []byte
	for _, x := range attrs {
		a = append(a, x...)
	}
	b := binary.BigEndian.AppendUint16(nil, uint16(peer))
	b = append(b, 0, 0, 0, 0) // originated time
	b = binary.BigEndian.AppendUint16(b, uint16(len(a)))
	return append(b, a...)
}

func rib(sub uint16, p string, entries ...[]byte) []byte {
	pf := netip.MustParsePrefix(p)
	body := []byte{0, 0, 0, 1, byte(pf.Bits())}
	a := pf.Addr().As16()
	if pf.Addr().Is4() {
		a4 := pf.Addr().As4()
		body = append(body, a4[:(pf.Bits()+7)/8]...)
	} else {
		body = append(body, a[:(pf.Bits()+7)/8]...)
	}
	body = binary.BigEndian.AppendUint16(body, uint16(len(entries)))
	for _, e := range entries {
		body = append(body, e...)
	}
	return mrtRecord(mrtTableDumpV2, sub, body)
}

func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

type seenPrefix struct {
	prefix string
	values []string
}

func scan(t *testing.T, data []byte) ([]seenPrefix, Stats, error) {
	t.Helper()
	s := NewScanner()
	var out []seenPrefix
	st, err := s.Scan(bytes.NewReader(data), func(p netip.Prefix, ids []uint32) error {
		var vals []string
		for _, id := range ids {
			vals = append(vals, s.Values[id].String())
		}
		slices.Sort(vals)
		out = append(out, seenPrefix{p.String(), vals})
		return nil
	})
	return out, st, err
}

func TestScanValues(t *testing.T) {
	data := join(
		peerIndex(3),
		// Several peers: the prefix's set is the union; a repeated value counts once.
		rib(subRIBIPv4Unicast, "192.0.2.0/24",
			entry(0, asPath(), comms(false, 64500<<16|100, 64500<<16|200)),
			entry(1, comms(true, 64500<<16|100, 64501<<16|7), large([3]uint32{4200000000, 1, 2})),
			entry(2, asPath())), // an entry with no community adds nothing
		// Standard values with the extended-length flag only, 4-byte values at the edge.
		rib(subRIBIPv4Unicast, "10.0.0.0/8", entry(0, comms(true, 0xffffffff, 0))),
		// No entry has a value: the prefix is still reported.
		rib(subRIBIPv4Unicast, "198.51.100.0/24", entry(0, asPath())),
	)
	got, st, err := scan(t, data)
	if err != nil {
		t.Fatal(err)
	}
	want := []seenPrefix{
		{"192.0.2.0/24", []string{"4200000000:1:2", "64500:100", "64500:200", "64501:7"}},
		{"10.0.0.0/8", []string{"0:0", "65535:65535"}},
		{"198.51.100.0/24", nil},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].prefix != want[i].prefix || !slices.Equal(got[i].values, want[i].values) {
			t.Errorf("record %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	if st.Peers != 3 || st.Prefixes != 3 || st.Entries != 5 || st.EntriesWithCom != 3 || st.Records != 4 {
		t.Errorf("stats %+v", st)
	}
}

func TestScanIgnoresOtherData(t *testing.T) {
	data := join(
		mrtRecord(16, 4, []byte{1, 2, 3}), // BGP4MP: another type
		peerIndex(1),
		rib(4, "2001:db8::/32", entry(0, comms(false, 1<<16|1))), // RIB_IPV6_UNICAST: another subtype
		mrtRecord(mrtTableDumpV2, 3, []byte{9, 9}),               // RIB_IPV4_MULTICAST
		rib(subRIBIPv4Unicast, "203.0.113.0/24", entry(0, comms(false, 1<<16|1))),
	)
	got, st, err := scan(t, data)
	if err != nil || len(got) != 1 || got[0].prefix != "203.0.113.0/24" {
		t.Fatalf("%v %v", got, err)
	}
	if st.Ignored != 3 || st.Prefixes != 1 {
		t.Errorf("stats %+v", st)
	}
}

func TestScanPrefixLengths(t *testing.T) {
	var recs [][]byte
	for _, p := range []string{"0.0.0.0/0", "10.0.0.0/7", "10.0.0.0/8", "10.1.0.0/24", "10.1.1.0/25", "10.1.1.1/32"} {
		recs = append(recs, rib(subRIBIPv4Unicast, p, entry(0, comms(false, 1<<16|1))))
	}
	got, st, err := scan(t, join(append([][]byte{peerIndex(1)}, recs...)...))
	if err != nil || len(got) != 2 || got[0].prefix != "10.0.0.0/8" || got[1].prefix != "10.1.0.0/24" || st.OutOfRange != 4 {
		t.Fatalf("%v %+v %v", got, st, err)
	}
	// Host bits after the prefix length are cleared.
	rec := rib(subRIBIPv4Unicast, "10.1.2.0/24", entry(0))
	rec[12+5+2] = 0x03 // the third byte: 10.1.3 as /23 has a stray bit
	rec[12+4] = 23
	got, _, err = scan(t, join(peerIndex(1), rec))
	if err != nil || len(got) != 1 || got[0].prefix != "10.1.2.0/23" {
		t.Fatalf("masked: %v %v", got, err)
	}
}

func TestScanDamaged(t *testing.T) {
	good := join(
		peerIndex(2),
		rib(subRIBIPv4Unicast, "192.0.2.0/24", entry(0, comms(false, 1<<16|1)), entry(1, large([3]uint32{1, 2, 3}))),
		rib(subRIBIPv4Unicast, "10.0.0.0/8", entry(1, comms(true, 5))),
	)
	if _, _, err := scan(t, good); err != nil {
		t.Fatal(err)
	}
	// Cut anywhere: an error, never a panic (a cut exactly between records is a shorter, valid file).
	boundaries := map[int]bool{0: true}
	off := 0
	for off < len(good) {
		off += 12 + int(binary.BigEndian.Uint32(good[off+8:]))
		boundaries[off] = true
	}
	for n := 0; n < len(good); n++ {
		_, _, err := scan(t, good[:n])
		if boundaries[n] {
			if err != nil {
				t.Errorf("cut at record boundary %d: %v", n, err)
			}
		} else if !errors.Is(err, ErrDamaged) {
			t.Errorf("cut at %d: %v", n, err)
		}
	}
	// Specific damage.
	ribBody := func(mutate func(b []byte) []byte) []byte {
		rec := rib(subRIBIPv4Unicast, "192.0.2.0/24", entry(0, comms(false, 1<<16|1)))
		body := mutate(slices.Clone(rec[12:]))
		return join(peerIndex(1), mrtRecord(mrtTableDumpV2, subRIBIPv4Unicast, body))
	}
	cases := map[string][]byte{
		"RIB before peer index":  rib(subRIBIPv4Unicast, "192.0.2.0/24", entry(0)),
		"prefix length 33":       ribBody(func(b []byte) []byte { b[4] = 33; return b }),
		"peer out of range":      ribBody(func(b []byte) []byte { b[5+3+2+1] = 7; return b }),
		"entry count too big":    ribBody(func(b []byte) []byte { b[5+3+1] = 9; return b }),
		"attribute too long":     ribBody(func(b []byte) []byte { b[5+3+2+6+1] = 200; return b }),
		"attribute header cut":   ribBody(func(b []byte) []byte { b[5+3+2+6+1] = 2; return b[:len(b)-4] }),
		"record too large":       join(mrtRecord(mrtTableDumpV2, subPeerIndexTable, nil)[:8], binary.BigEndian.AppendUint32(nil, maxRecord+1)),
		"peer table short":       mrtRecord(mrtTableDumpV2, subPeerIndexTable, []byte{1, 2}),
		"peer table lies":        mrtRecord(mrtTableDumpV2, subPeerIndexTable, []byte{0, 0, 0, 0, 0, 0, 0, 5, 0}),
		"extended attribute cut": join(peerIndex(1), rib(subRIBIPv4Unicast, "192.0.2.0/24", entry(0, []byte{0x10, 8}))),
	}
	for name, data := range cases {
		if _, _, err := scan(t, data); !errors.Is(err, ErrDamaged) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Random garbage and random corruption of a good file never panic.
func TestScanGarbage(t *testing.T) {
	good := join(
		peerIndex(3),
		rib(subRIBIPv4Unicast, "192.0.2.0/24", entry(0, comms(false, 1<<16|1)), entry(2, large([3]uint32{1, 2, 3}))),
		rib(subRIBIPv4Unicast, "10.0.0.0/8", entry(1, comms(true, 5), asPath())),
	)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 3000; i++ {
		b := slices.Clone(good)
		for j := rng.Intn(4) + 1; j > 0; j-- {
			b[rng.Intn(len(b))] = byte(rng.Intn(256))
		}
		scan(t, b[:rng.Intn(len(b)+1)])
	}
	for i := 0; i < 1000; i++ {
		b := make([]byte, rng.Intn(200))
		rng.Read(b)
		scan(t, b)
	}
}

func FuzzScan(f *testing.F) {
	f.Add(join(peerIndex(2), rib(subRIBIPv4Unicast, "192.0.2.0/24", entry(0, comms(false, 1<<16|1)), entry(1, large([3]uint32{1, 2, 3})))))
	f.Fuzz(func(t *testing.T, data []byte) {
		s := NewScanner()
		s.Scan(bytes.NewReader(data), func(netip.Prefix, []uint32) error { return nil })
	})
}

func gzipTo(t *testing.T, path string, data []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(data)
	gz.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
}
