package geo

import (
	"archive/tar"
	"archive/zip"
	"bufio"
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
	"path"
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
	// MaxMind GeoLite2 City (GeoLite EULA, attribution required), a tar.gz with
	// GeoLite2-City_YYYYMMDD/GeoLite2-City.mmdb; the license key is added as a query.
	maxmindURL = "https://download.maxmind.com/app/geoip_download"
	// RIPE Atlas probe archive: today's dump, bzip2-compressed JSON
	// {"objects": [{"address_v4", "country_code", "latitude", "longitude", "status", ...}]}.
	atlasURL = "https://ftp.ripe.net/ripe/atlas/probes/archive/meta-latest"

	// GeoNames places with 1,000+ people (CC BY 4.0), a zip with cities1000.txt:
	// build-geo-data's own copy, used to place our city names in the Atlas city score.
	geonamesURL = "https://download.geonames.org/export/dump/cities1000.zip"

	atlasConnected = 1    // probe status "Connected"
	minAtlasProbes = 1000 // a dump with fewer public probes is broken
)

// evalURLs replaces the reference URLs in tests.
type evalURLs struct{ dbip, maxmind, atlas, geonames string }

func (p *Pipeline) urls() evalURLs {
	if p.refURLs != nil {
		return *p.refURLs
	}
	return evalURLs{dbipURLFormat, maxmindURL, atlasURL, geonamesURL}
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

// errMaxMind is all that is ever said about a failed request to MaxMind: the
// request URL holds the key, and the errors of Go's HTTP client repeat it.
var errMaxMind = errors.New("MaxMind download failed")

// refError turns any error of a request whose URL holds a secret into one that
// is safe to log and return. An HTTP status stays: it holds no URL.
func refError(name string, fixed, err error) error {
	var httpErr *download.StatusError
	if errors.As(err, &httpErr) {
		return fmt.Errorf("%s: %w", name, httpErr)
	}
	return fixed
}

// MaxMind limits its downloads (30 a day) and publishes rarely, so a copy
// checked this recently is used without a request.
const (
	maxMindMinAge = 3 * 24 * time.Hour

	maxMindMMDB = "GeoLite2-City.mmdb" // the entry of the MaxMind archive
	maxUnpacked = 2 << 30              // an entry larger than this is not a database
)

// recentlyChecked tells whether the archive exists and its last successful
// request (the mtime of <archive>.checked, our own clock: the file's mtime is
// the server's Last-Modified) is younger than maxAge.
func recentlyChecked(archive string, maxAge time.Duration, now time.Time) bool {
	if _, err := os.Stat(archive); err != nil {
		return false
	}
	info, err := os.Stat(archive + ".checked")
	return err == nil && now.Sub(info.ModTime()) < maxAge && !info.ModTime().After(now)
}

func markChecked(archive string, now time.Time) { stamp(archive+".checked", now) }

// stamp creates an empty file whose mtime is now (our clock).
func stamp(path string, now time.Time) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	f.Close()
	os.Chtimes(path, now, now)
}

// dropArchive removes a bad archive so that the next run downloads it again.
func dropArchive(archive string) {
	os.Remove(archive)
	os.Remove(archive + ".etag")
	os.Remove(archive + ".checked")
}

// fetchMaxMind downloads the GeoLite2 City archive (a tar.gz) and unpacks the
// MMDB; without MAXMIND_LICENSE_KEY it is skipped.
func (p *Pipeline) fetchMaxMind(ctx context.Context, client *http.Client, dir string, now time.Time) (string, string) {
	if p.MaxMindKey == "" {
		return "", "skipped: MAXMIND_LICENSE_KEY is not set"
	}
	archive, mmdb := filepath.Join(dir, "geolite2-city.tar.gz"), filepath.Join(dir, "geolite2-city.mmdb")
	status := download.Cached
	if !recentlyChecked(archive, maxMindMinAge, now) {
		rawURL := p.urls().maxmind + "?edition_id=GeoLite2-City&license_key=" + url.QueryEscape(p.MaxMindKey) + "&suffix=tar.gz"
		var err error
		if status, err = download.File(ctx, client, rawURL, archive); err != nil {
			var ok bool
			if status, ok = useCopy(archive, refError("MaxMind", errMaxMind, err)); !ok {
				return "", status
			}
		} else {
			markChecked(archive, now)
		}
	}
	if _, statErr := os.Stat(mmdb); status == download.Downloaded || statErr != nil {
		if err := untarMMDB(archive, mmdb); err != nil {
			dropArchive(archive)
			// An older unpacked copy is still better than no reference.
			if looksLikeMMDB(mmdb) {
				return mmdb, "stale: " + err.Error()
			}
			return "", "skipped: " + err.Error()
		}
	}
	return mmdb, status
}

// untarMMDB writes the GeoLite2-City.mmdb entry of a tar.gz archive to dst
// through a temporary file and checks that the result looks like an MMDB.
func untarMMDB(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return errors.New("not a gzip archive")
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return errors.New("archive has no " + maxMindMMDB)
		}
		if err != nil {
			return errors.New("bad tar archive")
		}
		if hdr.Typeflag == tar.TypeReg && path.Base(hdr.Name) == maxMindMMDB {
			return writeChecked(tr, dst, looksLikeMMDB, "not an MMDB file")
		}
	}
}

// writeChecked copies r to dst through a temporary file; check must accept the
// result.
func writeChecked(r io.Reader, dst string, check func(string) bool, bad string) error {
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	n, err := io.Copy(out, io.LimitReader(r, maxUnpacked+1))
	if err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if n > maxUnpacked || !check(tmp) {
		return errors.New(bad)
	}
	return os.Rename(tmp, dst)
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

const (
	// GeoNames publishes daily and the file changes little: a copy checked this recently is used.
	geonamesMinAge = 7 * 24 * time.Hour

	geonamesEntry   = "cities1000.txt" // the entry of the GeoNames archive
	geonamesMinCols = 15               // a line has 19 fields; the score reads up to the population (15th)
)

// looksLikeGeoNames checks the first line: enough tab-separated fields and a numeric geonameid.
func looksLikeGeoNames(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 1<<20).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	fields := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
	if len(fields) < geonamesMinCols {
		return false
	}
	_, err = strconv.ParseUint(fields[0], 10, 64)
	return err == nil
}

// fetchGeoNames downloads cities1000.zip (at most every 7 days) and unpacks
// cities1000.txt. It returns the path (empty if skipped) and the status.
func (p *Pipeline) fetchGeoNames(ctx context.Context, client *http.Client, dir string, now time.Time) (string, string) {
	archive, txt := filepath.Join(dir, "cities1000.zip"), filepath.Join(dir, geonamesEntry)
	status := download.Cached
	if !recentlyChecked(archive, geonamesMinAge, now) {
		var err error
		if status, err = download.File(ctx, client, p.urls().geonames, archive); err != nil {
			var ok bool
			if status, ok = useCopy(archive, err); !ok {
				return "", status
			}
		} else {
			markChecked(archive, now)
		}
	}
	if _, statErr := os.Stat(txt); status == download.Downloaded || statErr != nil {
		if err := unzipGeoNames(archive, txt); err != nil {
			dropArchive(archive)
			// An older unpacked copy is still better than no reference.
			if looksLikeGeoNames(txt) {
				return txt, "stale: " + err.Error()
			}
			return "", "skipped: " + err.Error()
		}
	}
	return txt, status
}

// unzipGeoNames writes the cities1000.txt entry of a zip archive to dst
// through a temporary file and checks its first line.
func unzipGeoNames(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return errors.New("not a zip archive")
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Mode().IsRegular() && path.Base(f.Name) == geonamesEntry {
			rc, err := f.Open()
			if err != nil {
				return errors.New("bad zip entry")
			}
			defer rc.Close()
			return writeChecked(rc, dst, looksLikeGeoNames, "not a GeoNames file")
		}
	}
	return errors.New("archive has no " + geonamesEntry)
}
