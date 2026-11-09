package geofeeds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"syscall"
	"time"
)

const userAgent = "openipdata-worker/1.0 (+https://github.com/shingrus/openipdata.org)"

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

func publicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, p := range reservedV4 {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// newHTTPClient dials IPv4 only (on the VM, IPv6 egress stalls for many hosts)
// and only public addresses, checked after DNS resolution and on redirects.
// allowPrivate lifts the address check for tests.
func newHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if addr, err := netip.ParseAddr(host); err != nil || !publicAddr(addr) {
				return errBlockedAddress
			}
			return nil
		}
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
	statusDownloaded = "downloaded"
	statusCached     = "cached" // 304: the local copy is current
	statusStale      = "stale"  // download failed; using the previous copy
	statusFailed     = "failed"
)

// fetchFile downloads rawURL to path unless the local copy is current. The
// file's modification time holds the server's Last-Modified, so one rename
// replaces the data and its cache validator together.
func fetchFile(ctx context.Context, client *http.Client, rawURL, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	info, statErr := os.Stat(path)
	if statErr == nil {
		req.Header.Set("If-Modified-Since", info.ModTime().UTC().Format(http.TimeFormat))
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && statErr == nil:
		return statusCached, nil
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	if _, err := io.Copy(file, resp.Body); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if modified, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		if err := os.Chtimes(tmp, modified, modified); err != nil {
			return "", err
		}
	}
	return statusDownloaded, os.Rename(tmp, path)
}
