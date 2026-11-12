package rdns

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
)

// originTable answers "which ASN announces this address, and under which
// prefix" from the announced IPv4 prefixes of public.asn, in memory. When two ASNs
// announce the same prefix, the lower ASN wins.
type originTable struct {
	byLen [33]map[uint32]int64 // masked address -> ASN, per prefix length
}

func newOriginTable() *originTable { return &originTable{} }

func addrKey(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// add records an announced IPv4 prefix. A prefix shorter than /8 is not a real
// announcement and is left out.
func (o *originTable) add(p netip.Prefix, asn int64) {
	if !p.Addr().Is4() || p.Bits() < 8 {
		return
	}
	p = p.Masked()
	if o.byLen[p.Bits()] == nil {
		o.byLen[p.Bits()] = map[uint32]int64{}
	}
	if _, ok := o.byLen[p.Bits()][addrKey(p.Addr())]; !ok {
		o.byLen[p.Bits()][addrKey(p.Addr())] = asn
	}
}

// lookup returns the longest announced prefix containing addr and its ASN.
func (o *originTable) lookup(addr netip.Addr) (netip.Prefix, int64, bool) {
	return o.longest(addr, 32)
}

// containing returns the longest announced prefix of at most p's length that
// contains all of p, and its ASN.
func (o *originTable) containing(p netip.Prefix) (netip.Prefix, int64, bool) {
	return o.longest(p.Addr(), p.Bits())
}

func (o *originTable) longest(addr netip.Addr, maxBits int) (netip.Prefix, int64, bool) {
	addr = addr.Unmap()
	if !addr.Is4() {
		return netip.Prefix{}, 0, false
	}
	for bits := maxBits; bits >= 8; bits-- {
		m := o.byLen[bits]
		if m == nil {
			continue
		}
		p, err := addr.Prefix(bits)
		if err != nil {
			continue
		}
		if asn, ok := m[addrKey(p.Addr())]; ok {
			return p, asn, true
		}
	}
	return netip.Prefix{}, 0, false
}

// originQuery reads every current ASN's json. Read only.
const originQuery = `SELECT a.asn, a.json FROM public.asn a WHERE ` + ipdata.SeenAtFilter + ` ORDER BY a.asn`

// loadOrigin reads the announced IPv4 prefixes of public.asn.
func loadOrigin(ctx context.Context, conn *pgx.Conn) (*originTable, int, error) {
	rows, err := conn.Query(ctx, originQuery)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	o := newOriginTable()
	prefixes := 0
	for rows.Next() {
		var asn int64
		var raw string
		if err := rows.Scan(&asn, &raw); err != nil {
			return nil, 0, err
		}
		for _, p := range ipdata.ASNPrefixes(raw) {
			if p.Addr().Is4() {
				o.add(p, asn)
				prefixes++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if prefixes == 0 {
		return nil, 0, fmt.Errorf("public.asn has no announced IPv4 prefixes")
	}
	return o, prefixes, nil
}
