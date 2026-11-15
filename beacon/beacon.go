package main

import (
	"net/netip"
	"strings"

	dnstap "github.com/dnstap/golang-dnstap"
	"github.com/miekg/dns"
	"google.golang.org/protobuf/proto"
)

const tokenLen = 32

// tokenFromName returns the token of a query name: the name must be exactly
// <token>.<zone>, with the token 32 hex characters. The name is folded to lower
// case first (resolvers may send mixed case, "0x20"). zone is lower case, no dot.
func tokenFromName(name, zone string) (string, bool) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	token, ok := strings.CutSuffix(name, "."+zone)
	if !ok || len(token) != tokenLen {
		return "", false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return token, true
}

// resolverAddr turns the address bytes of a dnstap message (4 or 16 bytes) into
// canonical text; an IPv4-mapped IPv6 address becomes plain IPv4.
func resolverAddr(b []byte) (string, bool) {
	if len(b) != 4 && len(b) != 16 {
		return "", false
	}
	a, ok := netip.AddrFromSlice(b)
	if !ok {
		return "", false
	}
	return a.Unmap().String(), true
}

// parseFrame reads one dnstap frame. It returns the token and the address of the
// resolver only for an authoritative query for <token>.<zone>.
func parseFrame(frame []byte, zone string) (token, resolver string, ok bool) {
	var dt dnstap.Dnstap
	if proto.Unmarshal(frame, &dt) != nil {
		return "", "", false
	}
	msg := dt.GetMessage()
	if msg == nil || msg.GetType() != dnstap.Message_AUTH_QUERY {
		return "", "", false
	}
	resolver, ok = resolverAddr(msg.GetQueryAddress())
	if !ok {
		return "", "", false
	}
	var m dns.Msg
	if m.Unpack(msg.GetQueryMessage()) != nil || len(m.Question) != 1 {
		return "", "", false
	}
	token, ok = tokenFromName(m.Question[0].Name, zone)
	if !ok {
		return "", "", false
	}
	return token, resolver, true
}
