package geo

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"strings"
	"time"

	"openipdata.org/hatchet/download"
	"openipdata.org/hatchet/rirstats"
)

// rirStats are the five RIRs' delegated-extended files: the country each
// allocated or assigned block was delegated to.
var rirStats = []struct{ Name, URL string }{
	{"afrinic", "https://ftp.afrinic.net/pub/stats/afrinic/delegated-afrinic-extended-latest"},
	{"apnic", "https://ftp.apnic.net/stats/apnic/delegated-apnic-extended-latest"},
	{"arin", "https://ftp.arin.net/pub/stats/arin/delegated-arin-extended-latest"},
	{"lacnic", "https://ftp.lacnic.net/pub/stats/lacnic/delegated-lacnic-extended-latest"},
	{"ripencc", "https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-extended-latest"},
}

// geoIPBlocksURL is ipverse/geo-ip-blocks (CC0): the country in each network's
// registration record, including sub-allocations and reassignments that the
// RIR stats miss. Only its IPv4 prefixes are used: its IPv6 lists put
// unallocated APNIC space in AU and 6to4 (2002::/16) in US.
const geoIPBlocksURL = "https://codeload.github.com/ipverse/geo-ip-blocks/tar.gz/refs/heads/master"

// staleFor is how old a cached download may be when the source cannot be
// reached; an older one fails the task.
const staleFor = 3 * 24 * time.Hour

// newHTTPClient connects over IPv4 only: the VM's IPv6 egress stalls.
func newHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Minute,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp4", addr)
			},
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute,
			ForceAttemptHTTP2:     true,
		},
	}
}

// Download results.
const (
	statusDownloaded = download.Downloaded
	statusCached     = download.Cached // the server said the copy is current
	statusStale      = "stale"         // the server failed; a recent copy is used
)

// fetch downloads rawURL to path unless the copy there is current. The
// file's modification time holds the server's Last-Modified, <path>.etag its
// ETag, and the modification time of <path>.checked the last time the server
// gave or confirmed the copy. When the server fails, a copy it confirmed
// less than staleFor ago is used instead.
func fetch(ctx context.Context, client *http.Client, rawURL, path string, now time.Time) (string, error) {
	checked := path + ".checked"
	status, err := download.File(ctx, client, rawURL, path)
	if err == nil {
		if err := os.WriteFile(checked, nil, 0o640); err != nil {
			return "", err
		}
		return status, os.Chtimes(checked, now, now)
	}
	if ctx.Err() != nil {
		return "", err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		if info, statErr := os.Stat(checked); statErr == nil && now.Sub(info.ModTime()) < staleFor {
			return statusStale, nil
		}
	}
	return "", err
}

// layerRow writes one row of a layer file (prefix,country,region,city,source):
// region and source are left empty.
func layerRow(w *csv.Writer, prefix, country, city string) {
	w.Write([]string{prefix, country, "", city, ""})
}

// writeStatsRows writes one layer row per allocated or assigned block of a
// delegated-extended file and returns the number of blocks. A block without a
// real country is written with an empty one: it gives no country, but it
// still marks delegated space, which mmdbbuilder needs to judge geofeed rows.
func writeStatsRows(path string, w *csv.Writer) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	blocks := 0
	err = rirstats.Parse(f, func(country string, prefixes []netip.Prefix) {
		code := strings.ToUpper(country)
		// ZZ, EU and AP are not countries: ZZ is "unknown", EU and AP are the
		// regions some old RIR blocks are tagged with.
		if !isCountryCode(code) || code == "ZZ" || code == "EU" || code == "AP" {
			code = ""
		}
		for _, p := range prefixes {
			layerRow(w, p.String(), code, "")
		}
		blocks++
	})
	return blocks, err
}

func isCountryCode(code string) bool {
	return len(code) == 2 && code[0] >= 'A' && code[0] <= 'Z' && code[1] >= 'A' && code[1] <= 'Z'
}

// geoIPBlocksFile is the layout of country/<cc>/<cc>.json in geo-ip-blocks.
type geoIPBlocksFile struct {
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	Prefixes    struct {
		IPv4 []string `json:"ipv4"`
	} `json:"prefixes"`
}

// writeGeoIPBlocksRows writes the IPv4 prefixes of the geo-ip-blocks tarball
// as layer rows and returns the country names by code.
func writeGeoIPBlocksRows(tarball string, w *csv.Writer) (names map[string]string, prefixes int, err error) {
	f, err := os.Open(tarball)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return nil, 0, err
	}
	defer gz.Close()
	names = map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names, prefixes, nil
		}
		if err != nil {
			return nil, 0, err
		}
		// <repo>-<branch>/country/<cc>/<cc>.json
		dir, file := path.Split(hdr.Name)
		code := path.Base(dir)
		if hdr.Typeflag != tar.TypeReg || file != code+".json" || path.Base(path.Dir(path.Clean(dir))) != "country" {
			continue
		}
		var data geoIPBlocksFile
		if err := json.NewDecoder(tr).Decode(&data); err != nil {
			return nil, 0, fmt.Errorf("%s: %w", hdr.Name, err)
		}
		code = strings.ToUpper(data.CountryCode)
		if !isCountryCode(code) {
			continue
		}
		if name := strings.TrimSpace(data.Country); name != "" {
			names[code] = name
		}
		for _, text := range data.Prefixes.IPv4 {
			p, err := netip.ParsePrefix(strings.TrimSpace(text))
			if err != nil || !p.Addr().Is4() {
				continue
			}
			layerRow(w, p.Masked().String(), code, "")
			prefixes++
		}
	}
}
