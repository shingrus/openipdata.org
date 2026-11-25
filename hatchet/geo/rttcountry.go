package geo

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"openipdata.org/hatchet/mlab"
)

// The RTT country check (mmdbbuilder geo -rtt-country). import-mlab-sites keeps, besides the
// layer 35-speedtest-site, the reply times of M-Lab's speed test servers (every row within 30 ms
// whose server city GeoNames places) and uploads them to the fixed R2 key mlab.SiteRTTKey.
// mmdbbuilder uses them as a speed-of-light test of the country: a /24 that answered a server in
// r ms cannot be farther than (r+1) * 102.1 + 300 km from it, so when no place of its record's
// country is that close, the /24 takes the country of its fastest server (if that country passes
// the same test). The check runs after all layers and before the guesses, and it needs the
// GeoNames copy (the coordinates of the servers and the places of a country). The file is not a
// layer: it is not in layerRules or optionalLayers and is kept in the geo directory.
//
// A missing or older object (mlabMaxAge, the same 45 days as the other M-Lab files, counted from
// the last new export), or any fetch error, only skips the check: its name is then listed in
// Source.SkippedLayers, and it never adds to StaleLayers, so the health check ping is still sent.
//
// The holdout build has no check: it passes no -geonames (it does not place cities), and the flag
// needs it.
const (
	// RTTCountrySource is the name listed in Source.SkippedLayers and R2Uploaded.
	RTTCountrySource = "rtt-country"

	// rttCountryFlag is the "mmdbbuilder geo" flag that gives the builder the RTT file.
	rttCountryFlag = "-rtt-country"

	// rttCityFlag gives the builder the same file for the city check (rttcity.go in mmdbbuilder),
	// which runs after the guesses and is passed exactly when rttCountryFlag is.
	rttCityFlag = "-rtt-city"

	rttCountryFile = mlab.SiteRTTFile
)

// downloadRTTCountry puts the RTT file into the geo directory as it is (fetchOptionalLayer does
// the ETag and age rules, in downloads). A copy of an older run is removed first, so a file that
// is there always belongs to this run's fetch.
func (p *Pipeline) downloadRTTCountry(ctx context.Context, fetch r2Fetch, downloads string, now time.Time, src *Source) error {
	dst := filepath.Join(p.dir(), rttCountryFile)
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	saved := filepath.Join(downloads, rttCountryFile)
	if ok, err := p.fetchOptionalLayer(ctx, fetch, saved, mlab.SiteRTTKey, RTTCountrySource, "import-mlab-sites", mlabMaxAge, now, src); err != nil || !ok {
		return err
	}
	if err := copyFile(saved, dst); err != nil {
		return err
	}
	p.log("download: %s uploaded %s", rttCountryFile, src.R2Uploaded[RTTCountrySource])
	return nil
}

// rttCountryArgs is the flag that gives mmdbbuilder the RTT file, and the status for the output:
// "on", "skipped: no GeoNames" (the build has no GeoNames copy; the builder refuses the flag
// without -geonames), "skipped: no reply times" (no file) or "skipped: reply times unreadable"
// (not a gzip CSV with prefix, country, city and rtt_ms columns, or no data row). A bad file never
// fails the build.
func (p *Pipeline) rttCountryArgs(geonamesOn bool) (args []string, status string) {
	if !geonamesOn {
		return nil, "skipped: no GeoNames"
	}
	path := filepath.Join(p.dir(), rttCountryFile)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, "skipped: no reply times"
	}
	if err := checkGzipCSV(path, "prefix", "country", "city", "rtt_ms"); err != nil {
		p.log("mmdb: WARNING %s is unreadable or empty; building without the RTT country check", rttCountryFile)
		return nil, "skipped: reply times unreadable"
	}
	return []string{rttCountryFlag, path}, "on"
}
