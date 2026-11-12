package rdns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

// originQuery reads the announced IPv4 prefixes (prefixes.ipv4 of the json) of every
// current ASN, so the rest of each document is never sent. public.asn.json is
// TEXT: a value that is not a JSON object (the column defaults to ”) has no
// prefixes, as for ipdata.ASNPrefixes; the CASE keeps the cast to jsonb away from it.
// Read only.
const originQuery = `SELECT a.asn,
		ARRAY(SELECT t.e FROM jsonb_array_elements_text(l.list) AS t(e) WHERE t.e IS NOT NULL)
	FROM public.asn a
	CROSS JOIN LATERAL (SELECT CASE WHEN a.json LIKE '{%' THEN a.json::jsonb #> '{prefixes,ipv4}' END AS list) l
	WHERE jsonb_typeof(l.list) = 'array' AND ` + ipdata.SeenAtFilter + ` ORDER BY a.asn`

// addPrefixes adds the IPv4 prefixes among texts (as written in the json: spaces
// around them are ignored, unparsable ones are left out) to o and returns how many.
func (o *originTable) addPrefixes(asn int64, texts []string) int {
	n := 0
	for _, text := range texts {
		if p, err := netip.ParsePrefix(strings.TrimSpace(text)); err == nil && p.Addr().Is4() {
			o.add(p, asn)
			n++
		}
	}
	return n
}

// originFallbackQuery reads every current ASN's whole json, for the case that
// originQuery cannot cast one of them to jsonb. Read only.
const originFallbackQuery = `SELECT a.asn, a.json FROM public.asn a WHERE ` + ipdata.SeenAtFilter + ` ORDER BY a.asn`

// loadOrigin reads the announced IPv4 prefixes of public.asn. A json value that
// starts like an object but is not valid JSON fails originQuery (SQLSTATE 22P02);
// ipdata.ASNPrefixes ignores such a value, so the load then goes through
// originFallbackQuery and one damaged row does not stop locate.
func loadOrigin(ctx context.Context, conn *pgx.Conn) (*originTable, int, error) {
	o, prefixes, err := loadOriginWith(ctx, conn, originQuery, scanPrefixList)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == invalidTextRepresentation {
		o, prefixes, err = loadOriginWith(ctx, conn, originFallbackQuery, scanWholeJSON)
	}
	if err != nil {
		return nil, 0, err
	}
	if prefixes == 0 {
		return nil, 0, fmt.Errorf("public.asn has no announced IPv4 prefixes")
	}
	return o, prefixes, nil
}

// invalidTextRepresentation is Postgres' SQLSTATE for an invalid json value.
const invalidTextRepresentation = "22P02"

// scanPrefixList reads a row of originQuery: the ASN and its IPv4 prefixes as text.
func scanPrefixList(rows pgx.Rows) (int64, []string, error) {
	var asn int64
	var texts []string
	err := rows.Scan(&asn, &texts)
	return asn, texts, err
}

// scanWholeJSON reads a row of originFallbackQuery: the ASN and its json, from
// which the IPv4 prefixes are taken.
func scanWholeJSON(rows pgx.Rows) (int64, []string, error) {
	var asn int64
	var raw string
	if err := rows.Scan(&asn, &raw); err != nil {
		return 0, nil, err
	}
	var texts []string
	for _, p := range ipdata.ASNPrefixes(raw) {
		if p.Addr().Is4() {
			texts = append(texts, p.String())
		}
	}
	return asn, texts, nil
}

func loadOriginWith(ctx context.Context, conn *pgx.Conn, query string, scan func(pgx.Rows) (int64, []string, error)) (*originTable, int, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	o := newOriginTable()
	prefixes := 0
	for rows.Next() {
		asn, texts, err := scan(rows)
		if err != nil {
			return nil, 0, err
		}
		prefixes += o.addPrefixes(asn, texts)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return o, prefixes, nil
}
