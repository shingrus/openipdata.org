package main

import (
	"net"
	"testing"

	dnstap "github.com/dnstap/golang-dnstap"
	"github.com/miekg/dns"
	"google.golang.org/protobuf/proto"
)

const (
	zone  = "r.openipdata.org"
	token = "0123456789abcdef0123456789abcdef"
)

func TestTokenFromName(t *testing.T) {
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{token + ".r.openipdata.org.", token, true},
		{token + ".r.openipdata.org", token, true},
		{"0123456789ABCDEF0123456789abcdef.R.OpenIPData.Org.", token, true}, // 0x20 mixed case
		{token + ".x.r.openipdata.org.", "", false},                         // extra label
		{"a." + token + ".r.openipdata.org.", "", false},
		{"r.openipdata.org.", "", false},
		{".r.openipdata.org.", "", false},
		{token + ".openipdata.org.", "", false},            // wrong zone
		{token + ".r.openipdata.org.evil.com.", "", false}, // zone only inside
		{token + "r.openipdata.org.", "", false},           // no dot before the zone
		{token[:31] + ".r.openipdata.org.", "", false},     // too short
		{token + "0.r.openipdata.org.", "", false},         // too long
		{token[:31] + "g.r.openipdata.org.", "", false},    // not hex
		{token[:31] + "-.r.openipdata.org.", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := tokenFromName(c.name, zone)
		if got != c.want || ok != c.ok {
			t.Errorf("tokenFromName(%q) = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestResolverAddr(t *testing.T) {
	cases := []struct {
		in   []byte
		want string
		ok   bool
	}{
		{net.ParseIP("203.0.113.7").To4(), "203.0.113.7", true},
		{net.ParseIP("203.0.113.7").To16(), "203.0.113.7", true}, // IPv4-mapped
		{net.ParseIP("2001:db8::1"), "2001:db8::1", true},
		{net.ParseIP("2001:DB8:0:0:0:0:0:1"), "2001:db8::1", true},
		{nil, "", false},
		{[]byte{1, 2, 3}, "", false},
		{make([]byte, 5), "", false},
	}
	for _, c := range cases {
		got, ok := resolverAddr(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("resolverAddr(%v) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func query(t *testing.T, name string, qtype uint16) []byte {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	// An EDNS client subnet must not matter.
	o := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}}
	o.Option = append(o.Option, &dns.EDNS0_SUBNET{Family: 1, SourceNetmask: 24, Address: net.ParseIP("198.51.100.0").To4()})
	m.Extra = append(m.Extra, o)
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func frame(t *testing.T, typ dnstap.Message_Type, addr []byte, wire []byte) []byte {
	t.Helper()
	dt := &dnstap.Dnstap{
		Type: dnstap.Dnstap_MESSAGE.Enum(),
		Message: &dnstap.Message{
			Type:         typ.Enum(),
			QueryAddress: addr,
			QueryMessage: wire,
		},
	}
	b, err := proto.Marshal(dt)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseFrame(t *testing.T) {
	v4 := net.ParseIP("203.0.113.7").To4()
	v6 := net.ParseIP("2001:db8::53")
	good := query(t, token+".r.openipdata.org.", dns.TypeA)

	cases := []struct {
		name        string
		frame       []byte
		token, addr string
		ok          bool
	}{
		{"ipv4 resolver", frame(t, dnstap.Message_AUTH_QUERY, v4, good), token, "203.0.113.7", true},
		{"ipv6 resolver", frame(t, dnstap.Message_AUTH_QUERY, v6, good), token, "2001:db8::53", true},
		{"mapped resolver", frame(t, dnstap.Message_AUTH_QUERY, net.ParseIP("203.0.113.7").To16(), good), token, "203.0.113.7", true},
		{"AAAA query", frame(t, dnstap.Message_AUTH_QUERY, v4, query(t, token+".r.openipdata.org.", dns.TypeAAAA)), token, "203.0.113.7", true},
		{"mixed case", frame(t, dnstap.Message_AUTH_QUERY, v4, query(t, "0123456789AbCdEf0123456789aBcDeF.R.openipdata.ORG.", dns.TypeA)), token, "203.0.113.7", true},
		{"response type", frame(t, dnstap.Message_AUTH_RESPONSE, v4, good), "", "", false},
		{"resolver query type", frame(t, dnstap.Message_RESOLVER_QUERY, v4, good), "", "", false},
		{"wrong zone", frame(t, dnstap.Message_AUTH_QUERY, v4, query(t, token+".example.org.", dns.TypeA)), "", "", false},
		{"extra label", frame(t, dnstap.Message_AUTH_QUERY, v4, query(t, "www."+token+".r.openipdata.org.", dns.TypeA)), "", "", false},
		{"apex", frame(t, dnstap.Message_AUTH_QUERY, v4, query(t, "r.openipdata.org.", dns.TypeNS)), "", "", false},
		{"short token", frame(t, dnstap.Message_AUTH_QUERY, v4, query(t, "abcd.r.openipdata.org.", dns.TypeA)), "", "", false},
		{"bad address", frame(t, dnstap.Message_AUTH_QUERY, []byte{1, 2}, good), "", "", false},
		{"no address", frame(t, dnstap.Message_AUTH_QUERY, nil, good), "", "", false},
		{"garbage query", frame(t, dnstap.Message_AUTH_QUERY, v4, []byte{1, 2, 3}), "", "", false},
		{"no query", frame(t, dnstap.Message_AUTH_QUERY, v4, nil), "", "", false},
		{"garbage frame", []byte("not protobuf \xff\xff\xff"), "", "", false},
		{"empty frame", nil, "", "", false},
	}
	for _, c := range cases {
		tok, addr, ok := parseFrame(c.frame, zone)
		if tok != c.token || addr != c.addr || ok != c.ok {
			t.Errorf("%s: got %q, %q, %v; want %q, %q, %v", c.name, tok, addr, ok, c.token, c.addr, c.ok)
		}
	}
	// A dnstap frame with no message at all.
	b, _ := proto.Marshal(&dnstap.Dnstap{Type: dnstap.Dnstap_MESSAGE.Enum()})
	if _, _, ok := parseFrame(b, zone); ok {
		t.Error("frame without message was accepted")
	}
}
