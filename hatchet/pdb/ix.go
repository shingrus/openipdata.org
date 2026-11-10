package pdb

import (
	"net/netip"
	"strings"
)

// IXRow is one geo_ix_pdb row: the peering LAN prefix of an internet exchange
// and where that exchange is.
type IXRow struct {
	Prefix  string // masked CIDR text
	IXID    int64
	Country string // upper case, 2 letters
	City    *string
}

type ixlanRow struct {
	ID   int64 `json:"id"`
	IXID int64 `json:"ix_id"`
}

type ixpfxRow struct {
	IXLANID int64  `json:"ixlan_id"`
	Prefix  string `json:"prefix"`
}

// Peering LANs are never shorter than these; a shorter prefix is a data error
// that would paint a large block with one exchange's city.
const (
	minIXBits4 = 16
	minIXBits6 = 32
)

// notGlobal4 and notGlobal6 hold special-purpose ranges that netip's
// IsGlobalUnicast and IsPrivate do not exclude. Private (RFC 1918, ULA) and
// link-local ranges are caught by those methods.
var (
	notGlobal4 = mustPrefixes("100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4")
	notGlobal6 = mustPrefixes("2001:db8::/32", "2002::/16", "3fff::/20")
	global6    = netip.MustParsePrefix("2000::/3")
)

func mustPrefixes(texts ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(texts))
	for i, t := range texts {
		out[i] = netip.MustParsePrefix(t)
	}
	return out
}

// globalPrefix returns the masked prefix when text is a globally routable
// unicast network of a plausible size for a peering LAN.
func globalPrefix(text string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(strings.TrimSpace(text))
	if err != nil {
		return netip.Prefix{}, false
	}
	p = p.Masked()
	addr := p.Addr()
	if addr.Is4In6() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return netip.Prefix{}, false
	}
	special, minBits := notGlobal4, minIXBits4
	if addr.Is6() {
		if !global6.Contains(addr) {
			return netip.Prefix{}, false
		}
		special, minBits = notGlobal6, minIXBits6
	}
	if p.Bits() < minBits {
		return netip.Prefix{}, false
	}
	for _, s := range special {
		if s.Overlaps(p) {
			return netip.Prefix{}, false
		}
	}
	return p, true
}

// validCountry reports whether code is two ASCII letters.
func validCountry(code string) bool {
	if len(code) != 2 {
		return false
	}
	for _, c := range code {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// ComputeIX derives one row per exchange prefix from the downloaded datasets
// in dir: the prefix (ixpfx) belongs to a peering LAN (ixlan) of an exchange
// (ix), whose country and city it gets. Prefixes that are not global unicast,
// rows whose exchange has no 2-letter country, and repeated prefixes (the
// first in file order wins) are left out.
func ComputeIX(dir string) ([]IXRow, error) {
	exchanges := map[int64]placeRow{}
	if err := readJSONL(datasetPath(dir, "ix"), func(p placeRow) { exchanges[p.ID] = p }); err != nil {
		return nil, err
	}
	lanToIX := map[int64]int64{}
	if err := readJSONL(datasetPath(dir, "ixlan"), func(l ixlanRow) { lanToIX[l.ID] = l.IXID }); err != nil {
		return nil, err
	}
	var rows []IXRow
	seen := map[string]bool{}
	err := readJSONL(datasetPath(dir, "ixpfx"), func(r ixpfxRow) {
		ixID, ok := lanToIX[r.IXLANID]
		if !ok {
			return
		}
		ix, ok := exchanges[ixID]
		if !ok {
			return
		}
		country := strings.ToUpper(strings.TrimSpace(ix.Country))
		if !validCountry(country) {
			return
		}
		p, ok := globalPrefix(r.Prefix)
		if !ok || seen[p.String()] {
			return
		}
		seen[p.String()] = true
		rows = append(rows, IXRow{Prefix: p.String(), IXID: ixID, Country: country, City: optional(normCity(ix.City))})
	})
	return rows, err
}
