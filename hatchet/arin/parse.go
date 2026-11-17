package arin

import (
	"encoding/binary"
	"encoding/json"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"
)

// record is what is kept of an RDAP answer: the network's range and, when the registrant's
// address gives a usable city, its country (US or CA), state or province and city. Never a handle
// or a name.
type record struct {
	Start, End uint32
	Country    string
	State      string
	City       string
	LookedUp   int64 // Unix seconds; set when the record is stored
}

func (r record) size() uint64 { return uint64(r.End) - uint64(r.Start) + 1 }

// Answer kinds of one lookup.
const (
	kindFound    = "found"     // a record of at most a /16 (stored, with or without a usable city)
	kindWide     = "wide"      // a record wider than a /16 (not stored)
	kindNotARIN  = "not_arin"  // a redirect: another registry's address
	kindNotFound = "not_found" // 404
	kindBad      = "bad"       // an answer that cannot be used
	kindFailed   = "failed"    // no answer after the tries
)

// maxBody caps the body of an answer.
const maxBody = 1 << 20

// maxCity is the longest city kept, in bytes.
const maxCity = 64

// rdapAnswer is the part of an RDAP "ip network" answer that is read.
type rdapAnswer struct {
	IPVersion    string `json:"ipVersion"`
	StartAddress string `json:"startAddress"`
	EndAddress   string `json:"endAddress"`
	Entities     []struct {
		Roles      []string          `json:"roles"`
		VcardArray []json.RawMessage `json:"vcardArray"`
	} `json:"entities"`
}

// parseAnswer reads the body of a 200 answer to a lookup of asked. The body is untrusted. It
// returns kindFound or kindWide with the record, or kindBad when the JSON is broken, the range is
// not IPv4, is upside down or does not contain the address asked.
func parseAnswer(body []byte, asked netip.Addr) (record, string) {
	var a rdapAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		return record{}, kindBad
	}
	if a.IPVersion != "" && a.IPVersion != "v4" {
		return record{}, kindBad
	}
	start, err1 := netip.ParseAddr(a.StartAddress)
	end, err2 := netip.ParseAddr(a.EndAddress)
	if err1 != nil || err2 != nil || !start.Is4() || !end.Is4() || !asked.Is4() {
		return record{}, kindBad
	}
	rec := record{Start: u32(start), End: u32(end)}
	at := u32(asked)
	if rec.Start > rec.End || at < rec.Start || at > rec.End {
		return record{}, kindBad
	}
	if rec.size() > maxStoredBlock {
		return record{}, kindWide
	}
	// The first top-level registrant entity; a nested entity is never read.
	for _, e := range a.Entities {
		if !hasRole(e.Roles, "registrant") {
			continue
		}
		if label, ok := adrLabel(e.VcardArray); ok {
			rec.Country, rec.State, rec.City = parseLabel(label)
		}
		break
	}
	return rec, kindFound
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if strings.EqualFold(r, want) {
			return true
		}
	}
	return false
}

// adrLabel returns the "label" parameter of the first "adr" property of a vcardArray
// (["vcard", [[name, params, type, value], ...]]).
func adrLabel(vcard []json.RawMessage) (string, bool) {
	if len(vcard) != 2 {
		return "", false
	}
	var props []json.RawMessage
	if json.Unmarshal(vcard[1], &props) != nil {
		return "", false
	}
	for _, raw := range props {
		var prop []json.RawMessage
		if json.Unmarshal(raw, &prop) != nil || len(prop) < 2 {
			continue
		}
		var name string
		if json.Unmarshal(prop[0], &name) != nil || name != "adr" {
			continue
		}
		var params map[string]json.RawMessage
		if json.Unmarshal(prop[1], &params) != nil {
			return "", false
		}
		var label string
		if json.Unmarshal(params["label"], &label) != nil {
			return "", false
		}
		return label, true // only the first adr property
	}
	return "", false
}

// parseLabel reads an address label: lines, the last is the country name ("United States" or
// "Canada"). From the second-last line upwards (never the first), the first line that is exactly
// two upper-case ASCII letters is the state or province, and the line before it is the city. It
// returns empty strings when there is no usable city: fewer than 3 lines, another country, no
// state, an empty or over-long city, or a city that is not valid UTF-8 or has a control character
// (Postgres refuses a NUL byte in a text value, and one bad city would fail its whole batch).
func parseLabel(label string) (country, state, city string) {
	var lines []string
	for _, l := range strings.Split(label, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < 3 {
		return "", "", ""
	}
	switch lines[len(lines)-1] {
	case "United States":
		country = "US"
	case "Canada":
		country = "CA"
	default:
		return "", "", ""
	}
	for i := len(lines) - 2; i >= 1; i-- {
		if !isStateCode(lines[i]) {
			continue
		}
		city = lines[i-1]
		if city == "" || len(city) > maxCity || !utf8.ValidString(city) || strings.ContainsFunc(city, unicode.IsControl) {
			return "", "", ""
		}
		return country, lines[i], city
	}
	return "", "", ""
}

func isStateCode(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

func u32(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

func addrOf(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// parseRange reads the two addresses of a stored range; ok is false for anything but IPv4 with
// start <= end.
func parseRange(start, end string) (s, e uint32, ok bool) {
	sa, err1 := netip.ParseAddr(start)
	ea, err2 := netip.ParseAddr(end)
	if err1 != nil || err2 != nil || !sa.Is4() || !ea.Is4() || u32(sa) > u32(ea) {
		return 0, 0, false
	}
	return u32(sa), u32(ea), true
}
