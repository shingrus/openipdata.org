package bgp

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
)

// MRT (RFC 6396) TABLE_DUMP_V2 reader, only as far as the community extraction needs it.
const (
	mrtTableDumpV2    = 13
	subPeerIndexTable = 1
	subRIBIPv4Unicast = 2

	// maxRecord is the largest MRT record body read into memory; a bigger one is damage.
	// A RIB record holds one prefix and the entries of every peer: a few hundred KB at most.
	maxRecord = 16 << 20

	attrCommunities      = 8
	attrLargeCommunities = 32

	// Prefix lengths kept: shorter ones are not announcements of real networks,
	// longer ones are filtered by most networks, so they carry no community of a city.
	MinPrefixLen = 8
	MaxPrefixLen = 24
)

// ErrDamaged is wrapped by every error about the content of an MRT file.
var ErrDamaged = errors.New("damaged MRT data")

func damaged(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrDamaged, fmt.Sprintf(format, args...))
}

// Value is one BGP community: standard (A:B, 16 + 16 bits) or large (A:B:C).
type Value struct {
	A, B, C uint32
	Large   bool
}

// String is the text form used in the file: a:b or a:b:c.
func (v Value) String() string {
	if v.Large {
		return fmt.Sprintf("%d:%d:%d", v.A, v.B, v.C)
	}
	return fmt.Sprintf("%d:%d", v.A, v.B)
}

func (v Value) less(w Value) bool {
	if v.Large != w.Large {
		return !v.Large
	}
	if v.A != w.A {
		return v.A < w.A
	}
	if v.B != w.B {
		return v.B < w.B
	}
	return v.C < w.C
}

// Stats counts what a Scan read.
type Stats struct {
	Peers          int    // entries of the PEER_INDEX_TABLE
	Records        uint64 // TABLE_DUMP_V2 records
	Ignored        uint64 // records of other types or subtypes
	Prefixes       uint64 // IPv4 unicast prefixes of /8 to /24 handed to the callback
	OutOfRange     uint64 // IPv4 unicast prefixes of other lengths, skipped
	Entries        uint64 // RIB entries of the prefixes handed to the callback
	EntriesWithCom uint64 // of those, entries with a community attribute
}

// Scanner reads TABLE_DUMP_V2 files. It keeps one id per distinct community value
// (Values[id]); the ids stay the same over several Scan calls, so a first pass can
// count and a second pass can use the counts.
type Scanner struct {
	Values []Value

	std   map[uint32]uint32
	large map[[3]uint32]uint32
	seen  []uint32 // per id: the number of the last record that had it
	rec   uint32
	ids   []uint32
	buf   []byte
}

// NewScanner returns a Scanner with no values yet.
func NewScanner() *Scanner {
	return &Scanner{std: map[uint32]uint32{}, large: map[[3]uint32]uint32{}}
}

func (s *Scanner) newID(v Value) uint32 {
	id := uint32(len(s.Values))
	s.Values = append(s.Values, v)
	s.seen = append(s.seen, 0)
	return id
}

// add puts id into the current record's set once.
func (s *Scanner) add(id uint32) {
	if s.seen[id] != s.rec {
		s.seen[id] = s.rec
		s.ids = append(s.ids, id)
	}
}

// Scan reads the (already decompressed) file and calls fn for every IPv4 unicast
// prefix of /8 to /24, in file order, with the ids of the distinct community values
// (standard and large) of all its entries. The ids slice is reused: fn must copy
// what it keeps. Anything else in the file is ignored. Every length is checked:
// damaged input gives an error wrapping ErrDamaged (or a read error), never a panic.
func (s *Scanner) Scan(r io.Reader, fn func(p netip.Prefix, ids []uint32) error) (Stats, error) {
	var st Stats
	br := bufio.NewReaderSize(r, 1<<20)
	var hdr [12]byte
	peers := -1 // number of peers of the table; -1: no PEER_INDEX_TABLE yet
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF {
				return st, nil
			}
			if err == io.ErrUnexpectedEOF {
				return st, damaged("truncated record header")
			}
			return st, err
		}
		typ := binary.BigEndian.Uint16(hdr[4:])
		sub := binary.BigEndian.Uint16(hdr[6:])
		length := binary.BigEndian.Uint32(hdr[8:])
		if typ != mrtTableDumpV2 || (sub != subPeerIndexTable && sub != subRIBIPv4Unicast) {
			if _, err := io.CopyN(io.Discard, br, int64(length)); err != nil {
				return st, truncated(err)
			}
			if typ == mrtTableDumpV2 {
				st.Records++
			}
			st.Ignored++
			continue
		}
		if length > maxRecord {
			return st, damaged("record of %d bytes", length)
		}
		if cap(s.buf) < int(length) {
			s.buf = make([]byte, int(length)+int(length)/4)
		}
		body := s.buf[:length]
		if _, err := io.ReadFull(br, body); err != nil {
			return st, truncated(err)
		}
		st.Records++
		switch sub {
		case subPeerIndexTable:
			n, err := peerCount(body)
			if err != nil {
				return st, err
			}
			peers = n
			st.Peers = n
		case subRIBIPv4Unicast:
			if peers < 0 {
				return st, damaged("RIB record before the peer index table")
			}
			if err := s.rib(body, peers, &st, fn); err != nil {
				return st, err
			}
		}
	}
}

func truncated(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return damaged("truncated record")
	}
	return err
}

func be16(b []byte) int { return int(binary.BigEndian.Uint16(b)) }

// peerCount reads the number of peers of a PEER_INDEX_TABLE and checks that all
// peer entries fit in the record.
func peerCount(b []byte) (int, error) {
	if len(b) < 6 {
		return 0, damaged("short peer index table")
	}
	o := 4 // collector BGP ID
	nameLen := be16(b[o:])
	o += 2 + nameLen
	if len(b) < o+2 {
		return 0, damaged("short peer index table")
	}
	n := be16(b[o:])
	o += 2
	for i := 0; i < n; i++ {
		if len(b) < o+1 {
			return 0, damaged("short peer entry")
		}
		pt := b[o]
		o += 5 // peer type and BGP ID
		if pt&1 != 0 {
			o += 16
		} else {
			o += 4
		}
		if pt&2 != 0 {
			o += 4
		} else {
			o += 2
		}
		if len(b) < o {
			return 0, damaged("short peer entry")
		}
	}
	return n, nil
}

// rib reads one RIB_IPV4_UNICAST record.
func (s *Scanner) rib(body []byte, peers int, st *Stats, fn func(netip.Prefix, []uint32) error) error {
	if len(body) < 5 {
		return damaged("short RIB record")
	}
	plen := int(body[4])
	if plen > 32 {
		return damaged("IPv4 prefix length %d", plen)
	}
	nb := (plen + 7) / 8
	o := 5 + nb
	if len(body) < o+2 {
		return damaged("short RIB record")
	}
	var a [4]byte
	copy(a[:], body[5:5+nb])
	count := be16(body[o:])
	o += 2
	keep := plen >= MinPrefixLen && plen <= MaxPrefixLen
	if !keep {
		st.OutOfRange++
	}
	s.rec++
	s.ids = s.ids[:0]
	for e := 0; e < count; e++ {
		if len(body)-o < 8 {
			return damaged("short RIB entry")
		}
		peer := be16(body[o:])
		alen := be16(body[o+6:])
		o += 8
		if len(body)-o < alen {
			return damaged("short attributes")
		}
		attrs := body[o : o+alen]
		o += alen
		if peer >= peers {
			return damaged("peer index %d of %d", peer, peers)
		}
		if !keep {
			continue
		}
		st.Entries++
		has, err := s.attributes(attrs)
		if err != nil {
			return err
		}
		if has {
			st.EntriesWithCom++
		}
	}
	if !keep {
		return nil
	}
	st.Prefixes++
	return fn(netip.PrefixFrom(netip.AddrFrom4(a), plen).Masked(), s.ids)
}

// attributes reads the path attributes of one entry and adds its communities.
func (s *Scanner) attributes(a []byte) (hasCommunities bool, err error) {
	for len(a) > 0 {
		if len(a) < 3 {
			return false, damaged("short attribute")
		}
		flags, typ := a[0], a[1]
		var l, hl int
		if flags&0x10 != 0 { // extended length
			if len(a) < 4 {
				return false, damaged("short attribute")
			}
			l, hl = be16(a[2:]), 4
		} else {
			l, hl = int(a[2]), 3
		}
		if len(a) < hl+l {
			return false, damaged("attribute of %d bytes does not fit", l)
		}
		v := a[hl : hl+l]
		a = a[hl+l:]
		switch typ {
		case attrCommunities:
			for ; len(v) >= 4; v = v[4:] {
				x := binary.BigEndian.Uint32(v)
				id, ok := s.std[x]
				if !ok {
					id = s.newID(Value{A: x >> 16, B: x & 0xffff})
					s.std[x] = id
				}
				s.add(id)
				hasCommunities = true
			}
		case attrLargeCommunities:
			for ; len(v) >= 12; v = v[12:] {
				k := [3]uint32{binary.BigEndian.Uint32(v), binary.BigEndian.Uint32(v[4:]), binary.BigEndian.Uint32(v[8:])}
				id, ok := s.large[k]
				if !ok {
					id = s.newID(Value{A: k[0], B: k[1], C: k[2], Large: true})
					s.large[k] = id
				}
				s.add(id)
				hasCommunities = true
			}
		}
	}
	return hasCommunities, nil
}
