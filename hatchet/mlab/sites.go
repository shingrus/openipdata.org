package mlab

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"openipdata.org/hatchet/download"
	"openipdata.org/hatchet/ipdata"
	"openipdata.org/hatchet/rdns"
)

// The site layer (35-speedtest-site). M-Lab's speed test (NDT) records the lowest round-trip time
// between its server and each client. A client /24 whose lowest reply time to a server is at most
// siteMaxRTT is very likely in the city of that server. The export is made from BigQuery by hand
// (see the README, "Reply-time export") and copied into DataDir/mlab/sites-inbox.
//
// SiteLayer reads the CSV (untrusted input, one row per client /24 and server site), keeps for each
// client /24 the one best candidate (the smallest reply time), keeps those within the limit whose
// city GeoNames places in the server's country, and writes the layer. It is the one task of the
// workflow import-mlab-sites and needs no database.

const (
	// SiteLayerFile and SiteLayerKey are in mlab.go.
	siteLayerSource = "speedtest-site" // the "source" column of the layer rows
	sitesInboxDir   = "sites-inbox"

	// siteMaxRTT (ms): a client /24 gets the site's city when its lowest reply time is at most this.
	// Measured on RIPE Atlas: above 5 ms an added answer is right only 27% to 40% of the time.
	siteMaxRTT = 5.0

	minValidSiteRows = 100_000 // fewer valid rows: not the right file
	minSiteLayerRows = 10_000  // fewer layer rows: fail, the old layer file stays
	maxSiteCities    = 5_000   // distinct (country, city) pairs kept
	maxSiteCityLen   = 64      // bytes
	maxSiteFields    = 32      // fields read from one line; more are ignored

	geoNamesURL      = "https://download.geonames.org/export/dump/cities1000.zip"
	geoNamesFile     = "cities1000.zip"
	geoNamesMaxBytes = 64 << 20
	geoNamesGate     = 7 * 24 * time.Hour
	checkedSuffix    = ".checked" // the time of the last request for a downloaded file is the stamp's mtime
)

var (
	errNoGeoNames  = errors.New("GeoNames is not available and there is no saved copy")
	errBadGeoNames = errors.New("the GeoNames file was damaged and has been removed")
)

// SiteLayerOptions are the inputs of SiteLayer.
type SiteLayerOptions struct {
	// Path is a local CSV (the run-mlab command); it wins over the name given to SiteLayer.
	Path        string
	SkipUpload  bool
	ForceUpload bool
}

// SiteLayerOutput counts a site layer run (counts only: no IP, no row, no city).
type SiteLayerOutput struct {
	File            string  `json:"file,omitempty"`
	Bytes           int64   `json:"bytes"`
	Rows            int     `json:"rows"`  // data lines read
	Valid           int     `json:"valid"` // rows that passed every check
	BadColumns      int     `json:"bad_columns"`
	BadTooLong      int     `json:"bad_too_long"`
	BadClient       int     `json:"bad_client"`
	NotPublicClient int     `json:"not_public_client"`
	NoSite          int     `json:"no_site"`
	BadCountry      int     `json:"bad_country"`
	BadCity         int     `json:"bad_city"`
	BadRTT          int     `json:"bad_rtt"`
	TooManyCities   int     `json:"too_many_cities"`
	Clients         int     `json:"clients"`    // distinct client /24s with a valid row
	OverLimit       int     `json:"over_limit"` // of them, best time above the limit
	Unplaced        int     `json:"unplaced"`   // within the limit, but GeoNames has no such city in the country
	Located         int     `json:"located"`    // rows of the layer
	Cities          int     `json:"cities"`     // distinct cities in the layer
	GeoNames        string  `json:"geonames,omitempty"`
	LayerBytes      int64   `json:"layer_bytes"`
	Upload          string  `json:"upload,omitempty"` // uploaded, unchanged (R2 has the same file; its date stays) or skipped
	Skipped         string  `json:"skipped,omitempty"`
	Seconds         float64 `json:"duration_seconds"`
}

// Bad is the number of rows that were not kept.
func (o SiteLayerOutput) Bad() int {
	return o.BadColumns + o.BadTooLong + o.BadClient + o.NotPublicClient + o.NoSite + o.BadCountry + o.BadCity + o.BadRTT + o.TooManyCities
}

// siteCity is a (country, city) pair of the export.
type siteCity struct{ country, city string }

// siteBest is the best candidate of one client /24.
type siteBest struct {
	rtt  float64
	city uint32 // index into the interned cities
}

// SiteLayer is the site-layer task. name is a base name in the sites inbox (empty: the newest CSV
// there); opts.Path wins. With no input file at all it succeeds with Skipped "no file" and does
// nothing.
func (p *Pipeline) SiteLayer(ctx context.Context, name string, opts SiteLayerOptions) (SiteLayerOutput, error) {
	started := time.Now()
	var out SiteLayerOutput
	path := opts.Path
	if path == "" {
		var found bool
		var err error
		if path, found, err = findInput(p.sitesInbox(), name); err != nil {
			return out, err
		}
		if !found {
			out.Skipped = "no file"
			p.log("site-layer: skipped (no file in the sites inbox)")
			return out, nil
		}
	}
	out.File = filepath.Base(path)
	f, err := os.Open(path)
	if err != nil {
		return out, permanent("cannot open the input file")
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil {
		out.Bytes = fi.Size()
	}
	minValid := p.minSiteRows
	if minValid == 0 {
		minValid = minValidSiteRows
	}
	minRows := p.minSiteLayerRows
	if minRows == 0 {
		minRows = minSiteLayerRows
	}

	cities, best, err := readSites(ctx, f, &out, minValid)
	if err != nil {
		return out, err
	}
	out.Clients = len(best)

	// The cities of the clients within the limit are the ones that need a place.
	wanted := map[string]bool{}
	keyOf := make([]string, len(cities))
	for _, b := range best {
		if b.rtt <= siteMaxRTT {
			c := cities[b.city]
			keyOf[b.city] = placeKey(c.country, c.city)
			wanted[keyOf[b.city]] = true
		}
	}
	if len(wanted) > 0 {
		if out.GeoNames, err = p.fetchGeoNames(ctx); err != nil {
			return out, err
		}
		if err := p.findPlaces(ctx, wanted); err != nil {
			return out, err
		}
	}

	clients := make([]uint32, 0, len(best))
	for c := range best {
		clients = append(clients, c)
	}
	slices.Sort(clients)
	var rows []layerRow
	used := map[uint32]bool{}
	for _, client := range clients {
		b := best[client]
		switch {
		case b.rtt > siteMaxRTT:
			out.OverLimit++
		case !wanted[keyOf[b.city]]:
			out.Unplaced++
		default:
			c := cities[b.city]
			rows = append(rows, layerRow{Client: client, Country: c.country, City: c.city})
			used[b.city] = true
		}
	}
	out.Located, out.Cities = len(rows), len(used)
	if len(rows) < minRows {
		return out, permanent("only %d layer rows, fewer than %d: is this the right file?", len(rows), minRows)
	}
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return out, err
	}
	if out.LayerBytes, err = writeLayer(p.SiteLayerPath(), rows, siteLayerSource); err != nil {
		return out, fmt.Errorf("write %s: %w", SiteLayerFile, err)
	}
	if opts.SkipUpload {
		out.Upload = "skipped"
	} else {
		// No refresh: the same export gives the same file, and an object that already holds it keeps
		// its date. So the age limit of build-geo-data counts from the last new export, also when
		// the workflow runs again for a new hop export only.
		status, err := p.put(ctx, SiteLayerKey, p.SiteLayerPath(), opts.ForceUpload, false)
		if err != nil {
			return out, err
		}
		out.Upload = string(status)
	}
	out.Seconds = time.Since(started).Round(time.Second).Seconds()
	p.log("site-layer: %d rows (%d bad, %d kept), %d client /24s, %d over the limit, %d unplaced, %d located in %d cities; %s is %d bytes; upload %s",
		out.Rows, out.Bad(), out.Valid, out.Clients, out.OverLimit, out.Unplaced, out.Located, out.Cities, SiteLayerFile, out.LayerBytes, out.Upload)
	return out, nil
}

// readSites streams the CSV, counts into out and returns the interned cities and the best
// candidate of each client /24. The header names the columns; the rows are untrusted.
func readSites(ctx context.Context, src io.Reader, out *SiteLayerOutput, minValid int) ([]siteCity, map[uint32]siteBest, error) {
	r, col, closeFn, err := openCSV(src)
	if err != nil {
		return nil, nil, err
	}
	defer closeFn()
	cClient, ok1 := col["client24"]
	cCountry, ok2 := col["site_country"]
	cCity, ok3 := col["site_city"]
	cRTT, ok4 := col["min_rtt"]
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil, nil, permanent("the header must have the columns client24, site_country, site_city and min_rtt")
	}
	need := max(cClient, cCountry, cCity, cRTT) + 1

	var cities []siteCity
	ids := map[string]uint32{}
	best := map[uint32]siteBest{}
	var fields [][]byte
	var key []byte
	lines := 0
	for {
		line, err := readLine(r)
		if err == io.EOF {
			break
		}
		// Every line counts for the two safety checks, empty ones too, so a file of empty lines
		// cannot run without end or without a look at the context.
		if lines++; lines > maxRows {
			return nil, nil, permanent("more than %d lines", maxRows)
		}
		if lines&0xFFFF == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		tooLong := errors.Is(err, errLineTooLong)
		if err != nil && !tooLong {
			return nil, nil, fmt.Errorf("read the input file: %w", err)
		}
		if !tooLong && len(line) == 0 {
			continue
		}
		out.Rows++
		if tooLong || len(line) > maxLine {
			out.BadTooLong++
			continue
		}
		var ok bool
		if fields, ok = splitQuoted(line, fields[:0]); !ok || len(fields) < need {
			out.BadColumns++
			continue
		}
		client, ok := parseClient(bytes.TrimSpace(fields[cClient]))
		if !ok {
			out.BadClient++
			continue
		}
		if !ipdata.PublicV4(addrOf(client<<8 | 1)) {
			out.NotPublicClient++
			continue
		}
		country, city := bytes.TrimSpace(fields[cCountry]), bytes.TrimSpace(fields[cCity])
		if len(country) == 0 || len(city) == 0 {
			out.NoSite++
			continue
		}
		if len(country) != 2 || !isASCIILetter(country[0]) || !isASCIILetter(country[1]) {
			out.BadCountry++
			continue
		}
		if len(city) > maxSiteCityLen || !utf8.Valid(city) || bytes.ContainsFunc(city, badCityRune) {
			out.BadCity++
			continue
		}
		rtt, err := strconv.ParseFloat(string(bytes.TrimSpace(fields[cRTT])), 64)
		if err != nil || math.IsNaN(rtt) || math.IsInf(rtt, 0) || rtt <= 0 {
			out.BadRTT++
			continue
		}
		key = append(append(append(key[:0], country...), '|'), city...)
		key[0] &^= 0x20 // upper-case the two letters of the country
		key[1] &^= 0x20
		id, seen := ids[string(key)]
		if !seen {
			if len(cities) >= maxSiteCities {
				out.TooManyCities++
				continue
			}
			id = uint32(len(cities))
			cities = append(cities, siteCity{string(key[:2]), string(city)})
			ids[string(key)] = id
		}
		out.Valid++
		cand := siteBest{rtt, id}
		if old, have := best[client]; !have || betterSite(cand, old, cities) {
			best[client] = cand
		}
	}
	if out.Valid < minValid {
		return nil, nil, permanent("only %d valid rows of %d, fewer than %d: is this the right file?", out.Valid, out.Rows, minValid)
	}
	return cities, best, nil
}

// betterSite says whether a beats b: the smaller time, then the smaller country, then the smaller
// city (byte order), so the result does not depend on the order of the lines.
func betterSite(a, b siteBest, cities []siteCity) bool {
	if a.rtt != b.rtt {
		return a.rtt < b.rtt
	}
	ca, cb := cities[a.city], cities[b.city]
	if ca.country != cb.country {
		return ca.country < cb.country
	}
	return ca.city < cb.city
}

func isASCIILetter(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

// badCityRune is true for what "mmdbbuilder geo" refuses in a city (cleanCity): a control character
// or an angle bracket. A row with such a city would be skipped by the build.
func badCityRune(r rune) bool { return unicode.IsControl(r) || r == '<' || r == '>' }

// splitQuoted splits one CSV line into fields (appended to dst). A field in double quotes may hold
// commas and "" for a quote. An unbalanced quote, a quote in the middle of an unquoted field or
// text after the closing quote is bad (ok false). At most maxSiteFields fields are read.
func splitQuoted(line []byte, dst [][]byte) (fields [][]byte, ok bool) {
	i := 0
	for {
		if len(dst) >= maxSiteFields {
			return dst, true
		}
		if i < len(line) && line[i] == '"' {
			i++
			var val []byte
			start := i
			escaped := false
			for {
				j := bytes.IndexByte(line[i:], '"')
				if j < 0 {
					return nil, false
				}
				i += j
				if i+1 < len(line) && line[i+1] == '"' {
					escaped = true
					i += 2
					continue
				}
				break
			}
			val = line[start:i]
			if escaped {
				val = bytes.ReplaceAll(val, []byte(`""`), []byte(`"`))
			}
			i++ // the closing quote
			dst = append(dst, val)
			if i == len(line) {
				return dst, true
			}
			if line[i] != ',' {
				return nil, false
			}
			i++
			if i == len(line) {
				return append(dst, nil), true
			}
			continue
		}
		j := bytes.IndexByte(line[i:], ',')
		end := len(line)
		if j >= 0 {
			end = i + j
		}
		field := line[i:end]
		if bytes.IndexByte(field, '"') >= 0 {
			return nil, false
		}
		dst = append(dst, field)
		if j < 0 {
			return dst, true
		}
		i = end + 1
		if i == len(line) {
			return append(dst, nil), true
		}
	}
}

// placeKey is the key of "mmdbbuilder geo -geonames" (geoNamesKey in mmdbbuilder/eval.go): the
// upper-case country, a bar and the lower-cased, trimmed name. hatchet must not import mmdbbuilder.
func placeKey(country, name string) string {
	return strings.ToUpper(strings.TrimSpace(country)) + "|" + strings.ToLower(strings.TrimSpace(name))
}

func hasLatin(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= 'a' && r <= 'z' })
}

// geoNamesPath is the workflow's own copy of cities1000.zip.
func (p *Pipeline) geoNamesPath() string { return filepath.Join(p.dir(), geoNamesFile) }

func (p *Pipeline) siteDownloadClient() *http.Client {
	if p.dlClient != nil {
		return p.dlClient
	}
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", addr) // IPv4 only: on the VM, IPv6 egress stalls for many hosts
	}
	transport.Proxy = nil
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute}
}

// fetchGeoNames downloads cities1000.zip unless it was asked for less than geoNamesGate ago (the
// stamp beside it) or the server says it is current. A failed download uses the saved file. The
// status is downloaded, cached, recent or saved copy; with no file at all the error is a fixed one.
func (p *Pipeline) fetchGeoNames(ctx context.Context) (string, error) {
	path := p.geoNamesPath()
	stamp := path + checkedSuffix
	now := p.clock()
	_, fileErr := os.Stat(path)
	if fileErr == nil {
		if si, err := os.Stat(stamp); err == nil && now.Sub(si.ModTime()) < geoNamesGate && si.ModTime().Before(now.Add(time.Minute)) {
			return "recent", nil
		}
	}
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return "", err
	}
	url := p.geoURL
	if url == "" {
		url = geoNamesURL
	}
	status, err := download.File(ctx, download.Capped(p.siteDownloadClient(), geoNamesMaxBytes), url, path)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if fileErr == nil {
			p.log("site-layer: WARNING download of %s failed: %s; using the saved copy", geoNamesFile, rdns.ScrubIPs(err.Error()))
			return "saved copy", nil
		}
		p.log("site-layer: WARNING download of %s failed: %s", geoNamesFile, rdns.ScrubIPs(err.Error()))
		return "", errNoGeoNames
	}
	if f, err := os.OpenFile(stamp, os.O_CREATE|os.O_WRONLY, 0o640); err == nil {
		f.Close()
		os.Chtimes(stamp, now, now)
	}
	return status, nil
}

// findPlaces reads cities1000.zip with the placing rule of "mmdbbuilder geo -geonames" (loadGeoNames
// in mmdbbuilder/eval.go): a place is indexed under its name and ascii name and each alternate name
// that has a Latin letter, by the key of placeKey. wanted holds the keys that are asked for; the
// value of each key found is set to true and every other is set to false, so the big index is
// never held. A copy that cannot be read, or has no place, is removed.
func (p *Pipeline) findPlaces(ctx context.Context, wanted map[string]bool) error {
	for k := range wanted {
		wanted[k] = false
	}
	places, err := scanPlaces(ctx, p.geoNamesPath(), wanted)
	if err != nil && ctx.Err() == nil || err == nil && places == 0 {
		path := p.geoNamesPath()
		os.Remove(path)
		os.Remove(path + ".etag")
		os.Remove(path + checkedSuffix)
		p.log("site-layer: WARNING %s could not be read and was removed", geoNamesFile)
		return errBadGeoNames
	}
	return err
}

// scanPlaces marks in wanted the keys that GeoNames has and returns the number of places read.
func scanPlaces(ctx context.Context, path string, wanted map[string]bool) (int, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	var f io.ReadCloser
	for _, zf := range zr.File {
		if zf.Name == "cities1000.txt" {
			if f, err = zf.Open(); err != nil {
				return 0, err
			}
			break
		}
	}
	if f == nil {
		return 0, errors.New("no cities1000.txt")
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	places, lines := 0, 0
	for sc.Scan() {
		if lines++; lines&0xFFFF == 0 {
			if err := ctx.Err(); err != nil {
				return places, err
			}
		}
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 15 {
			continue
		}
		_, latErr := strconv.ParseFloat(fields[4], 64)
		_, lonErr := strconv.ParseFloat(fields[5], 64)
		country := strings.ToUpper(strings.TrimSpace(fields[8]))
		if len(country) != 2 || latErr != nil || lonErr != nil {
			continue
		}
		places++
		mark := func(name string) {
			if k := placeKey(country, name); k != "" {
				if _, asked := wanted[k]; asked {
					wanted[k] = true
				}
			}
		}
		for _, name := range fields[1:3] {
			if strings.TrimSpace(name) != "" {
				mark(name)
			}
		}
		for _, name := range strings.Split(fields[3], ",") {
			if hasLatin(strings.ToLower(strings.TrimSpace(name))) {
				mark(name)
			}
		}
	}
	return places, sc.Err()
}
