// Package rirstats reads the RIRs' delegated-extended stats files and splits
// address ranges into CIDR prefixes. sync-geofeeds and build-geo-data share
// this code; each keeps its own downloaded copies of the files.
package rirstats

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
	"net/netip"
	"strconv"
	"strings"
)

// Parse calls fn for each allocated or assigned IPv4 or IPv6 block of a
// delegated-extended file (registry|cc|type|start|value|date|status|...),
// with the country code as written and the block as prefixes. An IPv4 value
// is an address count, which need not be a power of two; an IPv6 value is a
// prefix length. Callers filter by country.
func Parse(r io.Reader, fn func(country string, prefixes []netip.Prefix)) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Split(strings.TrimSpace(sc.Text()), "|")
		if len(f) < 7 || strings.HasPrefix(f[0], "#") || (f[6] != "allocated" && f[6] != "assigned") {
			continue
		}
		start, err := netip.ParseAddr(f[3])
		if err != nil {
			continue
		}
		value, err := strconv.ParseUint(f[4], 10, 64)
		if err != nil || value == 0 {
			continue
		}
		switch {
		case f[2] == "ipv4" && start.Is4():
			b := start.As4()
			last := uint64(binary.BigEndian.Uint32(b[:])) + value - 1
			if last > math.MaxUint32 {
				continue
			}
			var end [4]byte
			binary.BigEndian.PutUint32(end[:], uint32(last))
			fn(f[1], RangeToPrefixes(start, netip.AddrFrom4(end)))
		case f[2] == "ipv6" && start.Is6() && value <= 128:
			fn(f[1], []netip.Prefix{netip.PrefixFrom(start, int(value)).Masked()})
		}
	}
	return sc.Err()
}

// RangeToPrefixes splits an inclusive address range into the fewest prefixes.
func RangeToPrefixes(from, to netip.Addr) []netip.Prefix {
	var out []netip.Prefix
	for from.IsValid() && !to.Less(from) {
		p := netip.PrefixFrom(from, from.BitLen())
		for bits := 0; bits <= from.BitLen(); bits++ {
			c := netip.PrefixFrom(from, bits)
			if c.Masked().Addr() == from && !to.Less(LastAddr(c)) {
				p = c
				break
			}
		}
		out = append(out, p)
		from = LastAddr(p).Next() // invalid after the last address: ends the loop
	}
	return out
}

// LastAddr returns the highest address in p.
func LastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As16()
	hostBits := p.Addr().BitLen() - p.Bits()
	for i := 15; hostBits > 0; i-- {
		n := min(hostBits, 8)
		b[i] |= byte(1<<n - 1)
		hostBits -= n
	}
	addr := netip.AddrFrom16(b)
	if p.Addr().Is4() {
		addr = addr.Unmap()
	}
	return addr
}
