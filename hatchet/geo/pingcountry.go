package geo

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"openipdata.org/hatchet/ping"
)

// The ping country check (mmdbbuilder geo -ping-country). sync-hosting-ping makes hosting-rtt.csv.gz
// monthly: ping times from 27 Google Cloud regions to the .1 of every /24 of hosting networks.
// It checks the file and uploads it to the fixed R2 key ping.RTTKey. mmdbbuilder uses it as a speed-of-light test of the country, with the same rules
// of use as the RTT country check (rttcountry.go): it needs the GeoNames copy, runs after all
// layers and before the guesses, and is not a layer (it is not in layerRules or optionalLayers
// and is kept in the geo directory).
//
// A missing or older object (pingMaxAge, counted from the last new export) or any fetch error only
// skips the check: its name is then listed in Source.SkippedLayers, and it never adds to
// StaleLayers, so the health check ping is still sent. The holdout build and the retry without
// GeoNames do not pass the flag. The same file also goes to the city check as -ping-city (pingCityFlag):
// same status, same rules of use.
const (
	// PingCountrySource is the name listed in Source.SkippedLayers and R2Uploaded.
	PingCountrySource = "ping-country"

	// pingCountryFlag is the "mmdbbuilder geo" flag that gives the builder the ping file.
	pingCountryFlag = "-ping-country"

	// pingCityFlag gives the builder the same file for the city check (rttcity.go in mmdbbuilder):
	// its times of at most 30 ms are more disks next to the M-Lab reply times.
	pingCityFlag = "-ping-city"

	pingCountryFile = ping.RTTFile

	// pingMaxAge: sync-hosting-ping runs monthly.
	pingMaxAge = 45 * 24 * time.Hour

	// pingMinSiteColumns is the number of site columns a usable file has besides "prefix".
	pingMinSiteColumns = 3
)

// downloadPingCountry puts the ping file into the geo directory as it is (fetchOptionalLayer does
// the ETag and age rules, in downloads). A copy of an older run is removed first, so a file that
// is there always belongs to this run's fetch.
func (p *Pipeline) downloadPingCountry(ctx context.Context, fetch r2Fetch, downloads string, now time.Time, src *Source) error {
	dst := filepath.Join(p.dir(), pingCountryFile)
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	saved := filepath.Join(downloads, pingCountryFile)
	if ok, err := p.fetchOptionalLayer(ctx, fetch, saved, ping.RTTKey, PingCountrySource, "sync-hosting-ping", pingMaxAge, now, src); err != nil || !ok {
		return err
	}
	if err := copyFile(saved, dst); err != nil {
		return err
	}
	p.log("download: %s uploaded %s", pingCountryFile, src.R2Uploaded[PingCountrySource])
	return nil
}

// pingCountryArgs is the flag that gives mmdbbuilder the ping file, and the status for the output:
// "on", "skipped: no GeoNames" (the build has no GeoNames copy; the builder refuses the flag
// without -geonames), "skipped: no ping times" (no file) or "skipped: ping times unreadable" (not
// a gzip CSV with a prefix column and at least 3 other columns, or no data row). A bad file never
// fails the build.
func (p *Pipeline) pingCountryArgs(geonamesOn bool) (args []string, status string) {
	if !geonamesOn {
		return nil, "skipped: no GeoNames"
	}
	path := filepath.Join(p.dir(), pingCountryFile)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, "skipped: no ping times"
	}
	if err := checkGzipCSV(path, "prefix"); err != nil || !pingHasSites(path) {
		p.log("mmdb: WARNING %s is unreadable or empty; building without the ping country check", pingCountryFile)
		return nil, "skipped: ping times unreadable"
	}
	return []string{pingCountryFlag, path}, "on"
}

// pingHasSites says whether the header has "prefix" and at least pingMinSiteColumns more columns.
func pingHasSites(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return false
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return false
	}
	for i := range header {
		header[i] = strings.TrimSpace(header[i]) // as the builder reads it
	}
	return slices.Contains(header, "prefix") && len(header) >= 1+pingMinSiteColumns
}
