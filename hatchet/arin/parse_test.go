package arin

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParseLabel(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := []struct {
		name, label          string
		country, state, city string
	}{
		{"US 6 lines", "123 Example Street\nSuite 4\nSpringfield\nIL\n62701\nUnited States", "US", "IL", "Springfield"},
		{"US 5 lines", "1 Example Road\nSpringfield\nIL\n62701\nUnited States", "US", "IL", "Springfield"},
		{"US 4 lines", "Springfield\nIL\n62701\nUnited States", "US", "IL", "Springfield"},
		{"US 3 lines, the city is the first line", "Springfield\nIL\nUnited States", "US", "IL", "Springfield"},
		{"Canada", "1 Example Avenue\nToronto\nON\nM5V 1A1\nCanada", "CA", "ON", "Toronto"},
		{"8 lines", "Example Inc\nBuilding 1\nFloor 2\n1 Example Road\nSpringfield\nIL\n62701\nUnited States", "US", "IL", "Springfield"},
		{"blank lines and spaces", " 1 Example Road \n\n Springfield \r\n IL\n62701\n United States \n", "US", "IL", "Springfield"},
		{"two lines", "IL\nUnited States", "", "", ""},
		{"no state", "1 Example Road\nSpringfield\n62701\nUnited States", "", "", ""},
		{"state first line only", "IL\nExample Road\nSpringfield\nUnited States", "", "", ""},
		{"another country", "1 Example Road\nBerlin\nBE\n10115\nGermany", "", "", ""},
		{"lower-case state", "1 Example Road\nSpringfield\nil\n62701\nUnited States", "", "", ""},
		{"a NUL byte in the city", "1 Example Road\nSpring\x00field\nIL\n62701\nUnited States", "", "", ""},
		{"a tab in the city", "1 Example Road\nSpring\tfield\nIL\n62701\nUnited States", "", "", ""},
		{"state with a digit", "1 Example Road\nSpringfield\nI1\n62701\nUnited States", "", "", ""},
		{"three letters", "1 Example Road\nSpringfield\nILL\n62701\nUnited States", "", "", ""},
		{"over-long city", "1 Example Road\n" + long + "\nIL\n62701\nUnited States", "", "", ""},
		{"city of 64 bytes", "1 Example Road\n" + long[:64] + "\nIL\n62701\nUnited States", "US", "IL", long[:64]},
		{"invalid UTF-8", "1 Example Road\nSpring\xfffield\nIL\n62701\nUnited States", "", "", ""},
		{"empty", "", "", "", ""},
	}
	for _, c := range cases {
		country, state, city := parseLabel(c.label)
		if country != c.country || state != c.state || city != c.city {
			t.Errorf("%s: got %q %q %q, want %q %q %q", c.name, country, state, city, c.country, c.state, c.city)
		}
	}
}

// rdapJSON builds a made-up RDAP answer (documentation addresses, made-up names).
func rdapJSON(start, end, extra string) []byte {
	return []byte(`{"objectClassName":"ip network","handle":"NET-X","startAddress":"` + start + `","endAddress":"` + end + `","ipVersion":"v4"` + extra + `}`)
}

func entity(roles, label string) string {
	return `{"objectClassName":"entity","handle":"C1","roles":[` + roles + `],"vcardArray":["vcard",[["version",{},"text","4.0"],["fn",{},"text","Example Customer"],` +
		`["adr",{"label":` + label + `},"text",["","","","","","",""]],["kind",{},"text","org"]]]}`
}

const goodLabel = `"123 Example Street\nSuite 4\nSpringfield\nIL\n62701\nUnited States"`

func TestParseAnswer(t *testing.T) {
	asked := netip.MustParseAddr("192.0.2.77")
	reg := entity(`"registrant"`, goodLabel)
	abuse := entity(`"abuse"`, `"9 Other Road\nOther Town\nTX\n75001\nUnited States"`)

	rec, kind := parseAnswer(rdapJSON("192.0.2.0", "192.0.2.255", `,"entities":[`+abuse+`,`+reg+`]`), asked)
	if kind != kindFound || rec.Start != u32(netip.MustParseAddr("192.0.2.0")) || rec.End != u32(netip.MustParseAddr("192.0.2.255")) ||
		rec.Country != "US" || rec.State != "IL" || rec.City != "Springfield" {
		t.Errorf("good: %v %+v", kind, rec)
	}
	// A registrant that is nested inside another entity is never read.
	nested := `{"roles":["technical"],"entities":[` + reg + `]}`
	if rec, kind = parseAnswer(rdapJSON("192.0.2.0", "192.0.2.255", `,"entities":[`+nested+`]`), asked); kind != kindFound || rec.City != "" {
		t.Errorf("nested: %v %+v", kind, rec)
	}
	// No registrant, or no adr: stored with no city.
	if rec, kind = parseAnswer(rdapJSON("192.0.2.0", "192.0.2.255", `,"entities":[`+abuse+`]`), asked); kind != kindFound || rec.City != "" {
		t.Errorf("no registrant: %v %+v", kind, rec)
	}
	noAdr := `{"roles":["registrant"],"vcardArray":["vcard",[["fn",{},"text","Example"]]]}`
	if rec, kind = parseAnswer(rdapJSON("192.0.2.0", "192.0.2.255", `,"entities":[`+noAdr+`]`), asked); kind != kindFound || rec.City != "" || rec.End == 0 {
		t.Errorf("no adr: %v %+v", kind, rec)
	}
	// The first registrant counts; a second one is not read.
	reg2 := entity(`"registrant"`, `"1 Road\nOther Town\nTX\n75001\nUnited States"`)
	if rec, _ = parseAnswer(rdapJSON("192.0.2.0", "192.0.2.255", `,"entities":[`+reg+`,`+reg2+`]`), asked); rec.City != "Springfield" {
		t.Errorf("two registrants: %+v", rec)
	}
	// A /16 is stored, a /15 is wide.
	if _, kind = parseAnswer(rdapJSON("192.0.0.0", "192.0.255.255", ""), asked); kind != kindFound {
		t.Errorf("/16: %v", kind)
	}
	if rec, kind = parseAnswer(rdapJSON("192.0.0.0", "192.1.255.255", `,"entities":[`+reg+`]`), asked); kind != kindWide || rec.City != "" {
		t.Errorf("/15: %v %+v", kind, rec)
	}
	bad := map[string][]byte{
		"IPv6":              []byte(`{"ipVersion":"v6","startAddress":"2001:db8::","endAddress":"2001:db8::ffff"}`),
		"IPv6 addresses":    rdapJSON("2001:db8::", "2001:db8::ffff", ""),
		"not containing":    rdapJSON("192.0.3.0", "192.0.3.255", ""),
		"upside down":       rdapJSON("192.0.2.255", "192.0.2.0", ""),
		"broken JSON":       []byte(`{"startAddress":`),
		"not JSON":          []byte("<html>"),
		"empty":             nil,
		"missing addresses": []byte(`{"ipVersion":"v4"}`),
		"mapped":            rdapJSON("::ffff:192.0.2.0", "::ffff:192.0.2.255", ""),
		"bad address":       rdapJSON("192.0.2.x", "192.0.2.255", ""),
	}
	for name, body := range bad {
		if _, kind := parseAnswer(body, asked); kind != kindBad {
			t.Errorf("%s: kind %v", name, kind)
		}
	}
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }
