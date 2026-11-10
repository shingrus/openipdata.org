package geofeeds

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"openipdata.org/hatchet/rirstats"
)

// attr is one RPSL attribute; continuation lines are joined with a space.
type attr struct {
	key, value string
}

const (
	maxObjectAttrs = 2000    // larger objects are truncated (huge remarks lists)
	maxLine        = 1 << 20 // longer lines fail the scan
)

// scanObjects calls fn for each RPSL object in r. Objects are separated by
// blank lines; % and # lines are comments.
func scanObjects(r io.Reader, fn func([]attr)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	var obj []attr
	flush := func() {
		if len(obj) > 0 {
			fn(obj)
		}
		obj = obj[:0]
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case line[0] == '%' || line[0] == '#':
		case line[0] == ' ' || line[0] == '\t' || line[0] == '+':
			if n := len(obj); n > 0 {
				obj[n-1].value = strings.TrimSpace(obj[n-1].value + " " + strings.TrimSpace(line[1:]))
			}
		default:
			key, value, ok := strings.Cut(line, ":")
			if ok && len(obj) < maxObjectAttrs {
				obj = append(obj, attr{strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)})
			}
		}
	}
	flush()
	return sc.Err()
}

var urlPattern = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)

// feedURLs returns the geofeed URLs an object points to: the geofeed attribute
// (RFC 9632), and any URL in a value that mentions "geofeed" or whose path
// does, which covers "remarks: Geofeed <url>" and the non-standard variants.
// Discovery is greedy: fetching rejects URLs that are not geofeeds.
func feedURLs(obj []attr) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range obj {
		mentions := a.key == "geofeed" || strings.Contains(strings.ToLower(a.value), "geofeed")
		for _, raw := range urlPattern.FindAllString(a.value, -1) {
			u, ok := cleanURL(raw)
			if !ok || seen[u] || !(mentions || strings.Contains(strings.ToLower(u), "geofeed")) {
				continue
			}
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// cleanURL trims punctuation that ends a sentence and checks the URL has an
// http(s) scheme and a host. The scheme and host are lowercased and an empty
// query or a fragment is dropped, so one feed written two ways counts once;
// the path keeps its case, which servers may care about.
func cleanURL(raw string) (string, bool) {
	u := strings.TrimRight(strings.TrimSpace(raw), ".,;:!)]}>\"'`")
	parsed, err := url.Parse(u)
	if err != nil {
		return "", false
	}
	parsed.Scheme, parsed.Host = strings.ToLower(parsed.Scheme), strings.ToLower(parsed.Host)
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return "", false
	}
	parsed.ForceQuery, parsed.Fragment, parsed.RawFragment = false, "", ""
	return parsed.String(), true
}

// objectPrefixes returns the address block of an inetnum, inet6num, route, or
// route6 object; ok is false for other classes.
func objectPrefixes(obj []attr) (prefixes []netip.Prefix, ok bool, err error) {
	if len(obj) == 0 {
		return nil, false, nil
	}
	class, value := obj[0].key, obj[0].value
	switch class {
	case "inetnum":
		if strings.Contains(value, "-") {
			from, to, err := parseRange(value)
			if err != nil {
				return nil, true, err
			}
			return rirstats.RangeToPrefixes(from, to), true, nil
		}
		p, err := parsePrefix(value) // LACNIC writes inetnums as prefixes
		if err != nil {
			return nil, true, err
		}
		return []netip.Prefix{p}, true, nil
	case "inet6num", "route", "route6":
		p, err := parsePrefix(value)
		if err != nil {
			return nil, true, err
		}
		return []netip.Prefix{p}, true, nil
	}
	return nil, false, nil
}

// parseRange reads the IPv4 range of an inetnum written as "first - last".
func parseRange(value string) (from, to netip.Addr, err error) {
	start, end, _ := strings.Cut(value, "-")
	from, err1 := netip.ParseAddr(strings.TrimSpace(start))
	to, err2 := netip.ParseAddr(strings.TrimSpace(end))
	if err1 != nil || err2 != nil || !from.Is4() || !to.Is4() || to.Less(from) {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("bad inetnum %q", value)
	}
	return from, to, nil
}

// parsePrefix accepts abbreviated IPv4 prefixes such as 200.3.12/22.
func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if addr, bits, ok := strings.Cut(s, "/"); ok && !strings.Contains(addr, ":") {
		for strings.Count(addr, ".") < 3 {
			addr += ".0"
		}
		s = addr + "/" + bits
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("bad prefix %q", s)
	}
	return p.Masked(), nil
}
