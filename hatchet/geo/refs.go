package geo

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"openipdata.org/hatchet/download"
)

// The reference data Evaluate scores against.
const (
	// DB-IP Lite city (CC BY 4.0), a new file each month, GeoLite2-style:
	// country.iso_code and city.names.en. The month goes in as 2006-01.
	dbipURLFormat = "https://download.db-ip.com/free/dbip-city-lite-%s.mmdb.gz"
	// IPinfo Lite (country only), updated daily; needs a token.
	ipinfoURL = "https://ipinfo.io/data/ipinfo_lite.mmdb"
	// RIPE Atlas probe archive: today's dump, bzip2-compressed JSON
	// {"objects": [{"address_v4", "country_code", "latitude", "longitude", "status", ...}]}.
	atlasURL = "https://ftp.ripe.net/ripe/atlas/probes/archive/meta-latest"

	atlasConnected = 1    // probe status "Connected"
	minAtlasProbes = 1000 // a dump with fewer public probes is broken
)

// evalURLs replaces the reference URLs in tests.
type evalURLs struct{ dbip, ipinfo, atlas string }

func (p *Pipeline) urls() evalURLs {
	if p.refURLs != nil {
		return *p.refURLs
	}
	return evalURLs{dbipURLFormat, ipinfoURL, atlasURL}
}

// useCopy is the fallback for a failed download: a copy kept from an earlier
// run is still better than no reference.
func useCopy(path string, err error) (status string, ok bool) {
	if _, statErr := os.Stat(path); statErr != nil {
		return "skipped: " + err.Error(), false
	}
	return "stale: " + err.Error(), true
}

// fetchDBIP downloads this month's DB-IP file, or last month's if this
// month's is not out yet, and unpacks it. It returns the MMDB path (empty if
// skipped) and the status.
func (p *Pipeline) fetchDBIP(ctx context.Context, client *http.Client, dir string, now time.Time) (string, string) {
	gz, mmdb := filepath.Join(dir, "dbip-city-lite.mmdb.gz"), filepath.Join(dir, "dbip-city-lite.mmdb")
	var status string
	var err error
	for i := 0; i < 2; i++ {
		month := time.Date(now.Year(), now.Month()-time.Month(i), 1, 0, 0, 0, 0, time.UTC)
		status, err = download.File(ctx, client, fmt.Sprintf(p.urls().dbip, month.Format("2006-01")), gz)
		var httpErr *download.StatusError
		if !errors.As(err, &httpErr) || httpErr.Code != http.StatusNotFound {
			break
		}
	}
	if err != nil {
		var ok bool
		if status, ok = useCopy(gz, err); !ok {
			return "", status
		}
	}
	if _, statErr := os.Stat(mmdb); status == download.Downloaded || statErr != nil {
		if err := gunzip(gz, mmdb); err != nil {
			os.Remove(gz) // the next run downloads it again
			os.Remove(gz + ".etag")
			// An older unpacked copy is still better than no reference.
			if looksLikeMMDB(mmdb) {
				return mmdb, "stale: " + err.Error()
			}
			return "", "skipped: " + err.Error()
		}
	}
	return mmdb, status
}

// gunzip writes the decompressed src to dst through a temporary file and
// checks that the result looks like an MMDB.
func gunzip(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := io.Copy(out, gz); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if !looksLikeMMDB(tmp) {
		return errors.New("not an MMDB file")
	}
	return os.Rename(tmp, dst)
}

// mmdbMarker starts the metadata section, which ends every MMDB file.
var mmdbMarker = []byte("\xab\xcd\xefMaxMind.com")

// looksLikeMMDB checks for the metadata marker in the last 128 KiB, so that
// an error page or a cut-off download is not passed on as a database.
func looksLikeMMDB(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	const tail = 128 << 10
	buf := make([]byte, min(info.Size(), tail))
	if _, err := f.ReadAt(buf, info.Size()-int64(len(buf))); err != nil && err != io.EOF {
		return false
	}
	return bytes.Contains(buf, mmdbMarker)
}

// errIPinfo is all that is ever said about a failed IPinfo request: the
// request URL holds the token, and the errors of Go's HTTP client repeat it.
var errIPinfo = errors.New("IPinfo download failed")

// ipinfoError turns any error of an IPinfo request into one that is safe to
// log and return. An HTTP status stays: it holds no URL.
func ipinfoError(err error) error {
	var httpErr *download.StatusError
	if errors.As(err, &httpErr) {
		return fmt.Errorf("IPinfo: %w", httpErr)
	}
	return errIPinfo
}

// fetchIPinfo downloads the IPinfo Lite MMDB; without IPINFO_TOKEN it is skipped.
func (p *Pipeline) fetchIPinfo(ctx context.Context, client *http.Client, dir string, _ time.Time) (string, string) {
	if p.IPinfoToken == "" {
		return "", "skipped: IPINFO_TOKEN is not set"
	}
	path := filepath.Join(dir, "ipinfo_lite.mmdb")
	status, err := download.File(ctx, client, p.urls().ipinfo+"?token="+url.QueryEscape(p.IPinfoToken), path)
	if err != nil {
		var ok bool
		if status, ok = useCopy(path, ipinfoError(err)); !ok {
			return "", status
		}
	}
	if !looksLikeMMDB(path) {
		os.Remove(path) // the next run downloads it again
		os.Remove(path + ".etag")
		return "", "skipped: not an MMDB file"
	}
	return path, status
}

// fetchAtlas downloads the RIPE Atlas probe dump and keeps the connected
// probes with a public IPv4 address and a country as a CSV (ip,country,lat,lon).
func (p *Pipeline) fetchAtlas(ctx context.Context, client *http.Client, dir string, _ time.Time) (string, string) {
	dump, csvPath := filepath.Join(dir, "atlas-probes.json.bz2"), filepath.Join(dir, "atlas-probes.csv")
	status, err := download.File(ctx, client, p.urls().atlas, dump)
	if err != nil {
		var ok bool
		if status, ok = useCopy(dump, err); !ok {
			return "", status
		}
	}
	if _, statErr := os.Stat(csvPath); status == download.Downloaded || statErr != nil {
		n, err := convertAtlas(dump, csvPath)
		if err != nil {
			os.Remove(dump) // the next run downloads it again
			os.Remove(dump + ".etag")
			return "", "skipped: " + err.Error()
		}
		status += ", " + strconv.Itoa(n) + " probes"
	}
	return csvPath, status
}

// convertAtlas writes the usable probes of a bzip2 dump to a CSV and returns
// their number.
func convertAtlas(dumpPath, csvPath string) (int, error) {
	in, err := os.Open(dumpPath)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	tmp := csvPath + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	n, err := writeAtlasCSV(bzip2.NewReader(in), out)
	if err == nil && n < minAtlasProbes {
		err = fmt.Errorf("only %d usable probes, fewer than %d", n, minAtlasProbes)
	}
	if err != nil {
		out.Close()
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	return n, os.Rename(tmp, csvPath)
}

// atlasProbe is the part of a probe object that is used.
type atlasProbe struct {
	AddressV4   *string  `json:"address_v4"`
	CountryCode string   `json:"country_code"`
	Status      int      `json:"status"`
	Latitude    *float64 `json:"latitude"`
	Longitude   *float64 `json:"longitude"`
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// writeAtlasCSV reads a probe dump ({"objects": [...]}) as a stream and writes
// ip,country,lat,lon for each connected probe with a public IPv4 address and
// a two-letter country.
func writeAtlasCSV(r io.Reader, w io.Writer) (int, error) {
	dec := json.NewDecoder(r)
	cw := csv.NewWriter(w)
	cw.Write([]string{"ip", "country", "lat", "lon"})
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return 0, errors.New("probe dump: not a JSON object")
	}
	n := 0
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, fmt.Errorf("probe dump: %w", err)
		}
		if key != "objects" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, fmt.Errorf("probe dump: %w", err)
			}
			continue
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			return 0, errors.New("probe dump: objects is not a list")
		}
		for dec.More() {
			var probe atlasProbe
			if err := dec.Decode(&probe); err != nil {
				return 0, fmt.Errorf("probe dump: %w", err)
			}
			if probe.Status != atlasConnected || probe.AddressV4 == nil || len(probe.CountryCode) != 2 {
				continue
			}
			addr, err := netip.ParseAddr(*probe.AddressV4)
			if err != nil || !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsPrivate() || cgnat.Contains(addr) {
				continue
			}
			row := []string{addr.String(), strings.ToUpper(probe.CountryCode), "", ""}
			if probe.Latitude != nil && probe.Longitude != nil {
				row[2] = strconv.FormatFloat(*probe.Latitude, 'f', -1, 64)
				row[3] = strconv.FormatFloat(*probe.Longitude, 'f', -1, 64)
			}
			cw.Write(row)
			n++
		}
		if _, err := dec.Token(); err != nil { // the closing ]
			return 0, fmt.Errorf("probe dump: %w", err)
		}
	}
	cw.Flush()
	return n, cw.Error()
}
