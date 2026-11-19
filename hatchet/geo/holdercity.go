package geo

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"openipdata.org/hatchet/arin"
)

// The holder city guess (85-holder-city). sync-arin-whois keeps the registrant city of wide ARIN
// blocks (main records wider than a /20, holder rows wider than a /16) and uploads them, in the
// layer format, to the fixed R2 key arin.HolderCitiesKey. mmdbbuilder gives that city to address
// space that no network announces and that lies inside such a block (-holders <file>, only
// together with -asn-main, because it needs the announced prefixes). Like the other guesses it
// is made inside the builder after all layers: the file is not a layer file, it is not in
// layerRules or optionalLayers, and it is kept in the geo directory, outside the layers.
//
// A missing or older object (holderCitiesMaxAge), or any fetch error, only skips the guess: its
// name is then listed in Source.SkippedLayers, and it never adds to StaleLayers, so the health
// check ping is still sent.
//
// The holdout build passes the file as it is: it is not learned from the geofeed rows.
const (
	// HolderCitySource is the name mmdbbuilder prints ("layer 85-holder-city: ...") and writes
	// as city_source.
	HolderCitySource = "85-holder-city"

	// holdersFlag is the "mmdbbuilder geo" flag that gives the builder the holder blocks file.
	holdersFlag = "-holders"

	holderCitiesFile   = arin.HolderCitiesFile
	holderCitiesMaxAge = 14 * 24 * time.Hour
)

// downloadHolderCities puts the holder blocks file into the geo directory as it is
// (fetchOptionalLayer does the ETag and age rules, in downloads). A copy of an older run is
// removed first, so a file that is there always belongs to this run's fetch.
func (p *Pipeline) downloadHolderCities(ctx context.Context, fetch r2Fetch, downloads string, now time.Time, src *Source) error {
	dst := filepath.Join(p.dir(), holderCitiesFile)
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	saved := filepath.Join(downloads, holderCitiesFile)
	if ok, err := p.fetchOptionalLayer(ctx, fetch, saved, arin.HolderCitiesKey, HolderCitySource, "sync-arin-whois", holderCitiesMaxAge, now, src); err != nil || !ok {
		return err
	}
	if err := copyFile(saved, dst); err != nil {
		return err
	}
	p.log("download: %s uploaded %s", holderCitiesFile, src.R2Uploaded[HolderCitySource])
	return nil
}

// holderArgs is the flag that gives mmdbbuilder the holder blocks file, and the status for the
// output: "on", "skipped: no ASN prefixes" (the ASN main fill is not on; the builder refuses
// -holders without -asn-main), "skipped: no holder blocks" (no file) or
// "skipped: holder blocks unreadable" (not a gzip CSV with prefix, country and city columns, or no
// data row). A bad file never fails the build.
func (p *Pipeline) holderArgs(asnMainOn bool) (args []string, status string) {
	if !asnMainOn {
		return nil, "skipped: no ASN prefixes"
	}
	path := filepath.Join(p.dir(), holderCitiesFile)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, "skipped: no holder blocks"
	}
	if err := checkHolderCities(path); err != nil {
		p.log("mmdb: WARNING %s is unreadable or empty; building without the %s guess", holderCitiesFile, HolderCitySource)
		return nil, "skipped: holder blocks unreadable"
	}
	return []string{holdersFlag, path}, "on"
}

// checkHolderCities reads the whole file as the builder will: a gzipped CSV whose header has
// prefix, country and city, with at least one data row.
func checkHolderCities(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return err
	}
	for i := range header {
		header[i] = strings.TrimSpace(header[i]) // as the builder reads it
	}
	for _, col := range []string{"prefix", "country", "city"} {
		if !slices.Contains(header, col) {
			return errors.New("missing column")
		}
	}
	// To the end of the stream: a copy that is cut short would fail the builder.
	rows := 0
	for {
		_, err := r.Read()
		if err == io.EOF {
			break
		}
		var parseErr *csv.ParseError
		if errors.As(err, &parseErr) {
			continue // the builder skips such a row too
		}
		if err != nil {
			return err
		}
		rows++
	}
	if rows == 0 {
		return errors.New("no rows")
	}
	return nil
}
