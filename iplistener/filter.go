package main

import (
	"net/netip"
	"regexp"
)

// Result tells what became of one log line.
type Result int

const (
	NoMatch  Result = iota // FILTER did not match
	Excluded               // EXCLUDE matched
	BadIP                  // first field is not a public IPv4 address
	Kept
)

// reserved lists IPv4 ranges that are not global unicast although netip says so.
var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// Filter turns a log line into a visitor IP.
type Filter struct {
	keep, exclude *regexp.Regexp // exclude may be nil
}

// NewFilter compiles FILTER and the optional EXCLUDE ("" means none).
func NewFilter(keep, exclude string) (*Filter, error) {
	f := &Filter{}
	var err error
	if f.keep, err = regexp.Compile(keep); err != nil {
		return nil, err
	}
	if exclude != "" {
		if f.exclude, err = regexp.Compile(exclude); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// Check applies FILTER, EXCLUDE and the IP rules to one line.
func (f *Filter) Check(line string) (netip.Addr, Result) {
	if !f.keep.MatchString(line) {
		return netip.Addr{}, NoMatch
	}
	if f.exclude != nil && f.exclude.MatchString(line) {
		return netip.Addr{}, Excluded
	}
	field := line
	for i := 0; i < len(line); i++ {
		if line[i] == ' ' || line[i] == '\t' {
			field = line[:i]
			break
		}
	}
	ip, err := netip.ParseAddr(field)
	if err != nil {
		return netip.Addr{}, BadIP
	}
	ip = ip.Unmap()
	if !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return netip.Addr{}, BadIP
	}
	for _, p := range reserved {
		if p.Contains(ip) {
			return netip.Addr{}, BadIP
		}
	}
	return ip, Kept
}
