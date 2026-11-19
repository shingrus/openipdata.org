package main

// The gap list of an ip2geo MMDB (mmdbbuilder gaps): the /24s that have a country
// but no city. A city of a weak source (city_source 80-asn-main, 82-asn-likely, 84-asn-near, 85-holder-city, 86-country-main, 88-asn-far, 25-bgp, 36-mlab, 37-whois-arin, 37-whois-text or 39-github-geofeeds; see
// noSampleSources) does not count as a city, so those /24s stay in the list. resolve-ips
// samples customer PTR names in them.

import (
	"bufio"
	"fmt"
	"net/netip"
	"os"

	"github.com/oschwald/maxminddb-golang/v2"
)

// gapStats counts what writeGaps found.
type gapStats struct {
	networks int // /24s written
	runs     int // lines written
	split    int // /24s skipped: parts of them have different countries
	hasCity  int // /24s skipped: a part of them has a city (smaller networks only)
}

// gapWriter merges consecutive /24s of one country into runs and writes them.
type gapWriter struct {
	w     *bufio.Writer
	stats *gapStats
	err   error

	open  bool
	first uint32 // first /24 of the open run, as address >> 8
	count uint32
	cc    string
}

// add appends n consecutive /24s starting at /24 number first (address >> 8).
func (g *gapWriter) add(first, n uint32, country string) {
	if g.open && g.cc == country && g.first+g.count == first {
		g.count += n
		return
	}
	g.flush()
	g.open, g.first, g.count, g.cc = true, first, n, country
}

func (g *gapWriter) flush() {
	if !g.open {
		return
	}
	g.open = false
	if g.err != nil {
		return
	}
	a := netip.AddrFrom4([4]byte{byte(g.first >> 16), byte(g.first >> 8), byte(g.first), 0})
	_, g.err = fmt.Fprintf(g.w, "%s/24 %d %s\n", a, g.count, g.cc)
	g.stats.runs++
	g.stats.networks += int(g.count)
}

// part is the state of one /24 that smaller networks share.
type part struct {
	active  bool
	idx     uint32 // address >> 8
	country string // the one country seen so far; "" with no country yet
	gap     bool   // a part has a country and no city
	hasCity bool
	split   bool
}

// writeGaps walks the IPv4 networks of the MMDB at mmdbPath in address order and
// writes the /24s whose record has a country and no city (a city of a weak source counts as none), as runs
// "<first /24> <count of consecutive /24s> <country>" (merged when consecutive with the
// same country), through a temporary file and a rename. A network smaller than a /24
// gives its /24 only when no part of it has a city and all its parts agree on the country.
func writeGaps(mmdbPath, outPath string) (gapStats, error) {
	var stats gapStats
	reader, err := maxminddb.Open(mmdbPath)
	if err != nil {
		return stats, err
	}
	defer reader.Close()
	tmp := outPath + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return stats, err
	}
	defer os.Remove(tmp) // no-op after a successful rename
	g := &gapWriter{w: bufio.NewWriter(out), stats: &stats}

	var cur part
	endPart := func() {
		if !cur.active {
			return
		}
		switch {
		case cur.split:
			stats.split++
		case cur.hasCity:
			stats.hasCity++
		case cur.gap:
			g.add(cur.idx, 1, cur.country)
		}
		cur = part{}
	}
	for res := range reader.NetworksWithin(allIPv4) {
		if err := res.Err(); err != nil {
			out.Close()
			return stats, err
		}
		p := res.Prefix()
		if a := p.Addr().Unmap(); !a.Is4() {
			continue
		} else if a != p.Addr() {
			p = netip.PrefixFrom(a, p.Bits()-96)
		}
		country, city, err := gapFields(res)
		if err != nil {
			out.Close()
			return stats, err
		}
		country = normCountry(country)
		lo, hi := prefixBounds(p)
		idx := lo >> 8
		if cur.active && cur.idx != idx {
			endPart()
		}
		if p.Bits() <= 24 {
			if country != "" && normCity(city) == "" {
				g.add(idx, hi>>8-idx+1, country)
			}
			continue
		}
		// A network inside one /24.
		cur.active, cur.idx = true, idx
		if country == "" {
			if normCity(city) != "" {
				cur.hasCity = true
			}
			continue
		}
		if cur.country != "" && cur.country != country {
			cur.split = true
		}
		cur.country = country
		if normCity(city) == "" {
			cur.gap = true
		} else {
			cur.hasCity = true
		}
	}
	endPart()
	g.flush()
	if g.err != nil {
		out.Close()
		return stats, g.err
	}
	if err := g.w.Flush(); err != nil {
		out.Close()
		return stats, err
	}
	if err := out.Close(); err != nil {
		return stats, err
	}
	return stats, os.Rename(tmp, outPath)
}
