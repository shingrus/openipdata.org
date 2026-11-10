package geofeeds

import (
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"openipdata.org/hatchet/download"
	"openipdata.org/hatchet/outputs"
	"openipdata.org/hatchet/rirstats"
)

// The whois geoloc layer: some inetnum objects carry "geoloc: <lat> <lon>".
// While discover reads the RIPE, APNIC and AFRINIC dumps it collects them, then
// names the nearest big place with GeoNames and writes WhoisRowsFile, a layer
// file in the same format as RowsFile. export-rows uploads it to WhoisRowsKey.
// build-geo-data applies it below the operator geofeeds.
const (
	WhoisRowsFile = "60-whois.csv.gz"
	WhoisRowsKey  = "internal/layers/" + WhoisRowsFile

	whoisUploadFile     = "whois-upload.json" // what the last whois upload sent
	minWhoisFirstUpload = 20_000              // with no previous upload, refuse fewer rows than this
	maxGeolocBlock      = 65536               // larger blocks (broader than a /16) say nothing about one place
	maxPlaceKm          = 25.0                // the place must lie this close to the geoloc point

	geonamesURL = "https://download.geonames.org/export/dump/cities1000.zip"
	// GeoNames rebuilds the file daily, so a conditional request would fetch it
	// every run: a copy younger than this is used without asking the server.
	geonamesMaxAge = 7 * 24 * time.Hour
	geonamesFile   = "cities1000.zip"
	geonamesText   = "cities1000.txt" // inside the zip
)

// geolocDumps are the whois dumps whose inetnum objects are read for geoloc
// (by dump name): not LACNIC, not the IRR dumps, and no inet6num.
var geolocDumps = map[string]bool{"ripe-inetnum": true, "apnic-inetnum": true, "afrinic": true}

// Reasons whois blocks are dropped, counted in WhoisOutput.Dropped.
const (
	dropTooLarge   = "too_large"   // more than maxGeolocBlock addresses
	dropBadCountry = "bad_country" // no country, or one that is not a country (EU, ZZ, AP)
	dropNoPlace    = "no_place"    // no GeoNames place of the country within maxPlaceKm
)

// geolocBlock is an IPv4 inetnum with a usable geoloc.
type geolocBlock struct {
	registry string
	from, to netip.Addr
	country  string // upper case as written, "" if the object has none
	lat, lon float64
}

// parseGeoloc reads the value of a geoloc attribute: latitude and longitude
// in degrees, any further fields ignored. 0 0 is a placeholder, not a place.
func parseGeoloc(value string) (lat, lon float64, ok bool) {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return 0, 0, false
	}
	lat, err1 := strconv.ParseFloat(fields[0], 64)
	lon, err2 := strconv.ParseFloat(fields[1], 64)
	// Written so that NaN fails.
	if err1 != nil || err2 != nil || !(lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180) || lat == 0 && lon == 0 {
		return 0, 0, false
	}
	return lat, lon, true
}

// geolocBlockOf returns the block of an inetnum object that has a valid
// geoloc; ok is false for any other object.
func geolocBlockOf(obj []attr, registry string) (b geolocBlock, ok bool) {
	if len(obj) == 0 || obj[0].key != "inetnum" {
		return b, false
	}
	var haveGeoloc, haveCountry bool
	for _, a := range obj[1:] {
		switch {
		case a.key == "geoloc" && !haveGeoloc:
			haveGeoloc = true
			if b.lat, b.lon, ok = parseGeoloc(a.value); !ok {
				return b, false
			}
		case a.key == "country" && !haveCountry:
			haveCountry = true
			b.country = strings.ToUpper(strings.TrimSpace(a.value))
		}
	}
	if !haveGeoloc {
		return b, false
	}
	from, to, err := parseRange(obj[0].value)
	if err != nil {
		return b, false
	}
	b.registry, b.from, b.to = registry, from, to
	return b, true
}

// size is the number of addresses in the block.
func (b geolocBlock) size() uint64 {
	first, last := b.from.As4(), b.to.As4()
	return uint64(binary.BigEndian.Uint32(last[:])) - uint64(binary.BigEndian.Uint32(first[:])) + 1
}

// reject returns the reason the block cannot be used, "" if it can.
func (b geolocBlock) reject() string {
	switch {
	case b.size() > maxGeolocBlock:
		return dropTooLarge
	case !countries[b.country]:
		return dropBadCountry
	}
	return ""
}

// place is a GeoNames city.
type place struct {
	name     string
	lat, lon float64
	pop      int
}

// A grid of 0.5 degree cells per country finds the places near a point
// without comparing against all of them.
const (
	cellsPerDegree = 2
	lonCells       = 360 * cellsPerDegree
	earthKm        = 6371.0088
)

type cell struct {
	country  string
	lat, lon int // lon is wrapped into [0, lonCells)
}

type placeIndex map[cell][]place

func latCell(lat float64) int { return int(math.Floor((lat + 90) * cellsPerDegree)) }

// lonCell is not wrapped: callers wrap it with wrapLon after stepping to neighbours.
func lonCell(lon float64) int { return int(math.Floor((lon + 180) * cellsPerDegree)) }
func wrapLon(c int) int       { return (c%lonCells + lonCells) % lonCells }

func (x placeIndex) add(country string, p place) {
	key := cell{country, latCell(p.lat), wrapLon(lonCell(p.lon))}
	x[key] = append(x[key], p)
}

// distanceKm is the great-circle distance (haversine).
func distanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	const rad = math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthKm * math.Asin(math.Min(1, math.Sqrt(a)))
}

// biggestWithin returns the most populous place of the country within km of
// the point; of equal ones, the closer, then the first by name. The search
// covers every cell the circle can touch, however far north the point is,
// and wraps at the antimeridian.
func (x placeIndex) biggestWithin(country string, lat, lon, km float64) (best place, ok bool) {
	delta := km / earthKm * 180 / math.Pi // degrees of latitude
	dLon := 180.0                         // degrees of longitude the circle spans; all of them near a pole
	if s := math.Sin(km/earthKm) / math.Cos(lat*math.Pi/180); s > 0 && s < 1 {
		dLon = math.Asin(s) * 180 / math.Pi
	}
	loLon, hiLon := lonCell(lon-dLon), lonCell(lon+dLon)
	if hiLon-loLon+1 >= lonCells {
		loLon, hiLon = 0, lonCells-1
	}
	bestKm := 0.0
	for la := latCell(lat - delta); la <= latCell(lat+delta); la++ {
		for lo := loLon; lo <= hiLon; lo++ {
			for _, p := range x[cell{country, la, wrapLon(lo)}] {
				d := distanceKm(lat, lon, p.lat, p.lon)
				if d > km {
					continue
				}
				if !ok || p.pop > best.pop || p.pop == best.pop && (d < bestKm || d == bestKm && p.name < best.name) {
					best, bestKm, ok = p, d, true
				}
			}
		}
	}
	return best, ok
}

// skipFeature lists the GeoNames feature codes that are not a place to name.
var skipFeature = map[string]bool{"PPLX": true, "PPLH": true, "PPLQ": true, "PPLW": true, "PPLCH": true}

// readPlaces reads cities1000.txt from the GeoNames zip: tab-separated, with
// name, latitude, longitude, feature code, country code and population in
// columns 1, 4, 5, 7, 8 and 14. Sections of a populated place (PPLX, such as a
// city district) and places that no longer exist (PPLH, PPLQ, PPLW, PPLCH) are
// left out, so a city is never named by its district or by a ghost.
func readPlaces(path string) (placeIndex, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var text io.ReadCloser
	for _, f := range zr.File {
		if f.Name == geonamesText {
			if text, err = f.Open(); err != nil {
				return nil, err
			}
			break
		}
	}
	if text == nil {
		return nil, fmt.Errorf("%s not in the zip", geonamesText)
	}
	defer text.Close()
	index, n := placeIndex{}, 0
	sc := bufio.NewScanner(text)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) < 15 || len(f[8]) != 2 || skipFeature[f[7]] {
			continue
		}
		lat, err1 := strconv.ParseFloat(f[4], 64)
		lon, err2 := strconv.ParseFloat(f[5], 64)
		pop, _ := strconv.Atoi(f[14])
		if err1 != nil || err2 != nil || f[1] == "" || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
			continue
		}
		index.add(f[8], place{name: f[1], lat: lat, lon: lon, pop: pop})
		n++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("no places in %s", geonamesText)
	}
	return index, nil
}

// loadPlaces refreshes the GeoNames zip in the dumps directory and reads it.
// A copy younger than geonamesMaxAge is used without a request. Otherwise the
// download is conditional; if it fails, the copy there is used, however old.
// It returns the status (downloaded, cached, fresh, stale) or an error when
// there is no usable copy.
func (p *Pipeline) loadPlaces(ctx context.Context) (placeIndex, string, error) {
	dir := filepath.Join(p.dir(), "dumps")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, geonamesFile)
	status := statusFresh
	if info, err := os.Stat(path); err != nil || p.clock().Sub(info.ModTime()) >= geonamesMaxAge {
		url := p.geonames
		if url == "" {
			url = geonamesURL
		}
		var err error
		if status, err = download.File(ctx, newHTTPClient(10*time.Minute, p.allowPrivate), url, path); err != nil {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			if _, statErr := os.Stat(path); statErr != nil {
				return nil, statusFailed, fmt.Errorf("GeoNames download failed and there is no earlier copy: %w", err)
			}
			p.log("discover: WARNING GeoNames download failed, using the earlier copy: %v", err)
			status = statusStale
		}
	}
	index, err := readPlaces(path)
	if err != nil {
		os.Remove(path) // downloaded again next run
		return nil, status, fmt.Errorf("read %s: %w", geonamesFile, err)
	}
	return index, status, nil
}

// WhoisOutput is the whois geoloc part of the discover result.
type WhoisOutput struct {
	Blocks   int            `json:"blocks"`            // inetnums with a valid geoloc
	Dropped  map[string]int `json:"dropped,omitempty"` // blocks, by reason
	Rows     int            `json:"rows"`              // prefixes written
	Bytes    int64          `json:"bytes,omitempty"`
	GeoNames string         `json:"geonames,omitempty"` // downloaded, cached, fresh, stale
	// Skipped says why no layer file was written this run (the previous one
	// stays). A problem here never fails discover or touches the geofeed rows.
	Skipped string `json:"skipped,omitempty"`
}

// buildWhois writes WhoisRowsFile from the inetnums with a geoloc: one row per
// prefix of each block that passes reject and has a GeoNames place near it.
// It never fails: a problem ends in the result's Skipped.
func (p *Pipeline) buildWhois(ctx context.Context, blocks []geolocBlock) WhoisOutput {
	out := WhoisOutput{Blocks: len(blocks), Dropped: map[string]int{}}
	skip := func(format string, args ...any) WhoisOutput {
		out.Skipped = fmt.Sprintf(format, args...)
		p.log("discover: WARNING whois geoloc layer not built: %s", out.Skipped)
		return out
	}
	usable := make([]geolocBlock, 0, len(blocks))
	for _, b := range blocks {
		if reason := b.reject(); reason != "" {
			out.Dropped[reason]++
			continue
		}
		usable = append(usable, b)
	}
	if len(usable) == 0 {
		return skip("no usable blocks with a geoloc")
	}
	places, status, err := p.loadPlaces(ctx)
	out.GeoNames = status
	if err != nil {
		return skip("%v", err)
	}
	outDir := filepath.Join(p.dir(), OutDir)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return skip("%v", err)
	}
	path := filepath.Join(outDir, WhoisRowsFile)
	if out.Rows, err = writeWhoisRows(path, usable, places, out.Dropped); err != nil {
		return skip("%v", err)
	}
	out.Bytes = outputs.FileSize(path)
	if len(out.Dropped) == 0 {
		out.Dropped = nil
	}
	p.log("discover: whois geoloc: %d blocks, dropped %v, %d rows, %d bytes, GeoNames %s",
		out.Blocks, out.Dropped, out.Rows, out.Bytes, out.GeoNames)
	return out
}

// writeWhoisRows writes one row per prefix of each block, naming the most
// populous place within maxPlaceKm in the block's country, through a
// temporary file. A block with no such place is counted in dropped. With no
// rows at all it fails and leaves the file there.
func writeWhoisRows(path string, blocks []geolocBlock, places placeIndex, dropped map[string]int) (rows int, err error) {
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	defer file.Close()
	gz := gzip.NewWriter(file)
	w := csv.NewWriter(gz)
	w.Write(layerHeader)
	for _, b := range blocks {
		city, ok := places.biggestWithin(b.country, b.lat, b.lon, maxPlaceKm)
		if !ok {
			dropped[dropNoPlace]++
			continue
		}
		for _, prefix := range rirstats.RangeToPrefixes(b.from, b.to) {
			w.Write([]string{prefix.String(), b.country, "", city.name, "whois:" + b.registry})
			rows++
		}
	}
	if rows == 0 {
		return 0, errors.New("no rows")
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	if err := file.Close(); err != nil {
		return 0, err
	}
	return rows, os.Rename(tmp, path)
}

// WhoisUploadOutput is the whois geoloc part of the export-rows result.
type WhoisUploadOutput struct {
	Rows         int  `json:"rows"` // rows in the file
	Uploaded     bool `json:"uploaded"`
	PreviousRows int  `json:"previous_rows,omitempty"`
	// Skipped says why the file was not uploaded. The geofeed rows are not
	// affected: a problem with the whois rows never fails export-rows.
	Skipped string `json:"skipped,omitempty"`
}

// uploadFunc is publish.R2.Upload.
type uploadFunc func(ctx context.Context, key, path string) (string, error)

// uploadWhois puts the whois rows file that discover wrote at WhoisRowsKey,
// if upload is not nil. It never fails: a problem ends in the result's
// Skipped. A file that discover did not rewrite since the last upload is not
// sent again (its upload time would hide that discover could not build it).
// The gate is the geofeed rows': more than 10% fewer rows than the last
// upload is refused, as are fewer than minWhoisFirstUpload rows on the first
// upload; force skips both checks.
func (p *Pipeline) uploadWhois(ctx context.Context, upload uploadFunc, force bool) WhoisUploadOutput {
	var out WhoisUploadOutput
	skip := func(format string, args ...any) WhoisUploadOutput {
		out.Skipped = fmt.Sprintf(format, args...)
		p.log("export: WARNING whois geoloc rows not uploaded: %s", out.Skipped)
		return out
	}
	path := filepath.Join(p.dir(), OutDir, WhoisRowsFile)
	info, err := os.Stat(path)
	if err != nil {
		return skip("no %s (has discover built it?)", WhoisRowsFile)
	}
	if out.Rows, err = countRows(path); err != nil {
		return skip("%v", err)
	}
	if upload == nil {
		return out
	}
	if err := p.checkWhoisUpload(info.ModTime(), force, &out); err != nil {
		return skip("%v", err)
	}
	location, err := upload(ctx, WhoisRowsKey, path)
	if err != nil {
		return skip("%v", err)
	}
	p.log("export: uploaded %s", location)
	out.Uploaded = true
	data, _ := json.Marshal(uploaded{Date: p.clock().UTC().Format(time.RFC3339), Rows: out.Rows})
	if err := writeFileAtomic(filepath.Join(p.dir(), whoisUploadFile), data); err != nil {
		return skip("%v", err)
	}
	return out
}

// checkWhoisUpload applies the checks of uploadWhois; the error says why
// nothing should be uploaded.
func (p *Pipeline) checkWhoisUpload(modified time.Time, force bool, out *WhoisUploadOutput) error {
	var last uploaded
	data, err := os.ReadFile(filepath.Join(p.dir(), whoisUploadFile))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &last); err != nil && !force {
			return fmt.Errorf("%s is damaged (%v); run once with force_upload", whoisUploadFile, err)
		}
	case !errors.Is(err, os.ErrNotExist) && !force:
		return err
	}
	out.PreviousRows = last.Rows
	if out.Rows == 0 {
		return errors.New("no rows")
	}
	if force {
		p.log("export: whois gate skipped (force_upload)")
		return nil
	}
	if date, err := time.Parse(time.RFC3339, last.Date); err == nil && !modified.After(date) {
		return fmt.Errorf("not rebuilt since the last upload on %s (could discover build it?)", last.Date)
	}
	switch {
	case last.Rows > 0 && float64(out.Rows) < float64(last.Rows)*(1-maxRowsDrop):
		return fmt.Errorf("%d rows, down from %d on %s", out.Rows, last.Rows, last.Date)
	case last.Rows == 0 && out.Rows < minWhoisFirstUpload:
		return fmt.Errorf("%d rows, fewer than %d", out.Rows, minWhoisFirstUpload)
	}
	return nil
}

// countRows counts the data rows of a gzipped layer file.
func countRows(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	gz, err := gzip.NewReader(bufio.NewReader(file))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	rows := -1 // the header
	for {
		if _, err := r.Read(); err == io.EOF {
			return max(rows, 0), nil
		} else if err != nil {
			return 0, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		rows++
	}
}
