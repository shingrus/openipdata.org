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
	"openipdata.org/hatchet/ipdata"
)

const userAgent = download.UserAgent

// errBlockedAddress stops requests to non-public addresses: feed URLs come
// from whois records anyone can write, and must not reach the VM's own
// network or the cloud metadata service.
var errBlockedAddress = errors.New("non-public address")

// publicOnly is a net.Dialer Control function: it refuses any address that is
// not public, after DNS resolution. Every dialer in this package uses it.
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if addr, err := netip.ParseAddr(host); err != nil || !ipdata.PublicAddr(addr) {
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
	statusFresh      = "fresh"         // young enough to use without asking the server
	statusFailed     = "failed"
)
