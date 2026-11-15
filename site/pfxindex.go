package main

// The index of announced prefixes. The ip2asn MMDB cannot say whether a prefix
// is announced exactly (a more specific prefix splits its parent in the tree),
// so the truth is the prefix lists in the JSON of the SQLite file. They are read
// once per load into two sorted slices of small records, one per address
// family, never strings or a map: about 12 bytes per IPv4 and 24 per IPv6
// prefix. The same prefix announced by several networks has one record each.

import (
	"context"
	"database/sql"
	"encoding/binary"
	"net/netip"
	"slices"
	"sort"
)

type pfx4 struct {
	addr, asn uint32
	bits      uint8
}

type pfx6 struct {
	hi, lo uint64
	asn    uint32
	bits   uint8
}

// pfxIndex is sorted by (address, length, AS number) with no duplicates, and
// never changed after the load.
type pfxIndex struct {
	v4 []pfx4
	v6 []pfx6
}

// annPrefix is one announced prefix and the network that announces it.
type annPrefix struct {
	Prefix netip.Prefix
	ASN    uint32
}

func (x *pfxIndex) size() int { return len(x.v4) + len(x.v6) }

func cmp4(a pfx4, addr uint32, bits uint8) int {
	switch {
	case a.addr != addr:
		if a.addr < addr {
			return -1
		}
		return 1
	case a.bits != bits:
		if a.bits < bits {
			return -1
		}
		return 1
	}
	return 0
}

func cmp6(a pfx6, hi, lo uint64, bits uint8) int {
	switch {
	case a.hi != hi:
		if a.hi < hi {
			return -1
		}
		return 1
	case a.lo != lo:
		if a.lo < lo {
			return -1
		}
		return 1
	case a.bits != bits:
		if a.bits < bits {
			return -1
		}
		return 1
	}
	return 0
}

func split6(a netip.Addr) (hi, lo uint64) {
	b := a.As16()
	return binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
}

func join6(hi, lo uint64) netip.Addr {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], hi)
	binary.BigEndian.PutUint64(b[8:], lo)
	return netip.AddrFrom16(b)
}

func addr4(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

func mask6(hi, lo uint64, bits int) (uint64, uint64) {
	switch {
	case bits <= 0:
		return 0, 0
	case bits < 64:
		return hi &^ (1<<(64-bits) - 1), 0
	case bits == 64:
		return hi, 0
	case bits < 128:
		return hi, lo &^ (1<<(128-bits) - 1)
	}
	return hi, lo
}

// first4 is the index of the first record not before (addr, bits).
func (x *pfxIndex) first4(addr uint32, bits uint8) int {
	return sort.Search(len(x.v4), func(i int) bool { return cmp4(x.v4[i], addr, bits) >= 0 })
}

func (x *pfxIndex) first6(hi, lo uint64, bits uint8) int {
	return sort.Search(len(x.v6), func(i int) bool { return cmp6(x.v6[i], hi, lo, bits) >= 0 })
}

// exact returns the networks that announce exactly p (a masked prefix), by number.
func (x *pfxIndex) exact(p netip.Prefix) []uint32 {
	var out []uint32
	bits := uint8(p.Bits())
	if p.Addr().Is4() {
		a := addr4(p.Addr())
		for i := x.first4(a, bits); i < len(x.v4) && x.v4[i].addr == a && x.v4[i].bits == bits; i++ {
			out = append(out, x.v4[i].asn)
		}
		return out
	}
	hi, lo := split6(p.Addr())
	for i := x.first6(hi, lo, bits); i < len(x.v6) && x.v6[i].hi == hi && x.v6[i].lo == lo && x.v6[i].bits == bits; i++ {
		out = append(out, x.v6[i].asn)
	}
	return out
}

// covering returns the announced prefixes that strictly contain p, the most
// specific first, one entry per announcing network.
func (x *pfxIndex) covering(p netip.Prefix) []annPrefix {
	var out []annPrefix
	for l := p.Bits() - 1; l >= 1; l-- {
		q, err := p.Addr().Prefix(l)
		if err != nil {
			continue
		}
		for _, asn := range x.exact(q) {
			out = append(out, annPrefix{q, asn})
		}
	}
	return out
}

// longest returns the most specific announced prefix that contains a, and its networks.
func (x *pfxIndex) longest(a netip.Addr) (netip.Prefix, []uint32, bool) {
	for l := a.BitLen(); l >= 1; l-- {
		q, err := a.Prefix(l)
		if err != nil {
			continue
		}
		if asns := x.exact(q); len(asns) > 0 {
			return q, asns, true
		}
	}
	return netip.Prefix{}, nil, false
}

// inside returns the announced prefixes that lie strictly inside p, in address
// order (one entry per announcing network), at most limit of them, and how
// many there are in all.
func (x *pfxIndex) inside(p netip.Prefix, limit int) (rows []annPrefix, total int) {
	bits := uint8(p.Bits())
	if p.Addr().Is4() {
		first := addr4(p.Addr())
		last := first | uint32(1<<(32-p.Bits())-1)
		for i := x.first4(first, 0); i < len(x.v4) && x.v4[i].addr <= last; i++ {
			e := x.v4[i]
			if e.bits <= bits {
				continue
			}
			total++
			if len(rows) < limit {
				a := netip.AddrFrom4([4]byte{byte(e.addr >> 24), byte(e.addr >> 16), byte(e.addr >> 8), byte(e.addr)})
				rows = append(rows, annPrefix{netip.PrefixFrom(a, int(e.bits)), e.asn})
			}
		}
		return rows, total
	}
	hi, lo := split6(p.Addr())
	lastHi, lastLo := hi, lo
	if b := p.Bits(); b < 64 {
		lastHi |= 1<<(64-b) - 1
		lastLo = ^uint64(0)
	} else if b < 128 {
		lastLo |= 1<<(128-b) - 1
	}
	for i := x.first6(hi, lo, 0); i < len(x.v6); i++ {
		e := x.v6[i]
		if e.hi > lastHi || (e.hi == lastHi && e.lo > lastLo) {
			break
		}
		if e.bits <= bits {
			continue
		}
		total++
		if len(rows) < limit {
			rows = append(rows, annPrefix{netip.PrefixFrom(join6(e.hi, e.lo), int(e.bits)), e.asn})
		}
	}
	return rows, total
}

// loadPrefixSQL reads one family's prefix strings with their network. The JSON
// functions are guarded by json_valid: a malformed row has no prefixes, it does
// not stop the load. Only the elements of an array count (a bare string is not a list).
const loadPrefixSQL = `SELECT a.asn, j.value FROM asn a,
 json_each(CASE WHEN json_valid(a.json) THEN a.json ELSE '{}' END, ?) j WHERE j.type = 'text' AND typeof(j.key) = 'integer'`

// loadPrefixes reads every announced prefix of the table into a pfxIndex. Masked,
// invalid or wrong-family entries are skipped.
func loadPrefixes(ctx context.Context, db *sql.DB) (pfxIndex, error) {
	var x pfxIndex
	for _, fam := range []string{"ipv4", "ipv6"} {
		rows, err := db.QueryContext(ctx, loadPrefixSQL, "$.prefixes."+fam)
		if err != nil {
			return pfxIndex{}, err
		}
		for rows.Next() {
			var asn int64
			var s string
			if err := rows.Scan(&asn, &s); err != nil {
				rows.Close()
				return pfxIndex{}, err
			}
			if asn < 1 || asn > maxASN {
				continue
			}
			p, err := netip.ParsePrefix(s)
			if err != nil || p.Bits() < 1 || p != p.Masked() || p.Addr().Zone() != "" || p.Addr().Is4In6() {
				continue
			}
			if fam == "ipv4" {
				if p.Addr().Is4() {
					x.v4 = append(x.v4, pfx4{addr: addr4(p.Addr()), asn: uint32(asn), bits: uint8(p.Bits())})
				}
			} else if p.Addr().Is6() {
				hi, lo := split6(p.Addr())
				x.v6 = append(x.v6, pfx6{hi: hi, lo: lo, asn: uint32(asn), bits: uint8(p.Bits())})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return pfxIndex{}, err
		}
	}
	slices.SortFunc(x.v4, func(a, b pfx4) int {
		if c := cmp4(a, b.addr, b.bits); c != 0 {
			return c
		}
		return cmpU32(a.asn, b.asn)
	})
	slices.SortFunc(x.v6, func(a, b pfx6) int {
		if c := cmp6(a, b.hi, b.lo, b.bits); c != 0 {
			return c
		}
		return cmpU32(a.asn, b.asn)
	})
	x.v4 = slices.Clip(slices.Compact(x.v4))
	x.v6 = slices.Clip(slices.Compact(x.v6))
	return x, nil
}

func cmpU32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// The text of a prefix and its page.
func prefixHref(p netip.Prefix) string { return prefixPath + p.String() }

const prefixPath = "/prefix/"
