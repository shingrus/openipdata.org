package geofeeds

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"openipdata.org/hatchet/download"
)

const userAgent = download.UserAgent

// errBlockedAddress stops requests to non-public addresses: feed URLs come
// from whois records anyone can write, and must not reach the VM's own
// network or the cloud metadata service.
var errBlockedAddress = errors.New("non-public address")

// reservedV4 lists the special-purpose IPv4 blocks (RFC 6890) that the netip
// checks below still count as global unicast.
var reservedV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // shared address space
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// globalV6 is IPv6 global unicast space. Everything outside 2000::/3 is
// special-purpose; ::/96 in particular is where an IPv6 MMDB tree keeps the
// IPv4 addresses.
var globalV6 = netip.MustParsePrefix("2000::/3")

// reservedV6 lists the special-purpose blocks inside 2000::/3.
var reservedV6 = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"),     // IETF protocol assignments, Teredo included
	netip.MustParsePrefix("2001:db8::/32"), // documentation
	netip.MustParsePrefix("3fff::/20"),     // documentation
	netip.MustParsePrefix("2002::/16"),     // 6to4: stands for IPv4 addresses
}

func publicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	if addr.Is6() {
		if !globalV6.Contains(addr) {
			return false
		}
		for _, p := range reservedV6 {
			if p.Contains(addr) {
				return false
			}
		}
		return true
	}
	for _, p := range reservedV4 {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// publicOnly is a net.Dialer Control function: it refuses any address that is
// not public, after DNS resolution. Every dialer in this package uses it.
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if addr, err := netip.ParseAddr(host); err != nil || !publicAddr(addr) {
		return errBlockedAddress
	}
	return nil
}

// newHTTPClient dials IPv4 only (on the VM, IPv6 egress stalls for many hosts)
// and only public addresses, checked after DNS resolution and on redirects.
// allowPrivate lifts the address check for tests.
func newHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		dialer.Control = publicOnly
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", addr)
	}
	transport.Proxy = nil
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// Fetch results.
const (
	statusDownloaded = download.Downloaded
	statusCached     = download.Cached // 304: the local copy is current
	statusStale      = "stale"         // download failed; using the previous copy
	statusFailed     = "failed"
)
