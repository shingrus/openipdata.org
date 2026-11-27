package geo

import (
	"archive/zip"
	"context"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"openipdata.org/hatchet/download"
)

// The full GeoNames list (mmdbbuilder geo -geonames-all, and convert with the same flag). The main
// copy, cities1000.txt, holds the places of 1,000 people or more; a city name it lacks gets no
// coordinates. GeoNames' allCountries.zip also has the smaller places and the administrative
// divisions, and the builder looks the names that stay unplaced up in it (see the rule in
// AGENTS.md). It is build-geo-data's own copy next to cities1000, with its own .checked stamp and
// the same 7-day gate, and it is passed to the builder as the zip: the builder reads the entry as a
// stream, nothing is unpacked here. A missing, failed or unreadable list never fails the build.
const (
	// geonamesAllURL is GeoNames' file of every place (CC BY 4.0), about 400 MB as a zip.
	geonamesAllURL = "https://download.geonames.org/export/dump/allCountries.zip"

	geonamesAllFile = "allCountries.zip" // the copy in the eval directory
	geonamesAllText = "allCountries.txt" // the entry of the archive
	geonamesAllFlag = "-geonames-all"    // the flag of "mmdbbuilder geo" and "convert"
	geonamesAllMax  = 1 << 30            // download.Capped limit: 1 GB

	// geonamesAllWait bounds the download of the build. The main list waits 5 minutes; this file is
	// about 400 MB, so it gets 10. The .checked stamp is written only after a success, so a stalled
	// server costs this wait on every build, and build-mmdb has one hour in all.
	geonamesAllWait = 10 * time.Minute

	// geonamesAllRejected is the status of a copy that is there but is not the archive.
	geonamesAllRejected = "skipped: not a GeoNames archive"
)

// fetchGeoNamesAll downloads allCountries.zip (at most every 7 days) into dir and checks that it is
// a zip with the entry. It returns the path ("" if skipped) and the status.
func (p *Pipeline) fetchGeoNamesAll(ctx context.Context, client *http.Client, dir string, now time.Time) (string, string) {
	archive := filepath.Join(dir, geonamesAllFile)
	status := download.Cached
	if !recentlyChecked(archive, geonamesMinAge, now) {
		var err error
		if status, err = download.File(ctx, download.Capped(client, geonamesAllMax), p.urls().geonamesAll, archive); err != nil {
			var ok bool
			if status, ok = useCopy(archive, err); !ok {
				return "", status
			}
		} else {
			markChecked(archive, now)
		}
	}
	if !looksLikeGeoNamesAll(archive) {
		dropArchive(archive)
		return "", geonamesAllRejected
	}
	return archive, status
}

// looksLikeGeoNamesAll checks that a file is a zip with the allCountries.txt entry (the central
// directory only: the builder reads the lines).
func looksLikeGeoNamesAll(file string) bool {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return false
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Mode().IsRegular() && path.Base(f.Name) == geonamesAllText {
			return true
		}
	}
	return false
}

// prepareGeoNamesAll makes the copy available. It returns the path, or "" and the reason; a problem
// here never fails the build.
func (p *Pipeline) prepareGeoNamesAll(ctx context.Context) (file, status string) {
	dir := p.evalDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "skipped: " + err.Error()
	}
	fetchCtx, cancel := context.WithTimeout(ctx, geonamesAllWait)
	defer cancel()
	file, status = p.fetchGeoNamesAll(fetchCtx, newHTTPClient(), dir, time.Now().UTC())
	p.log("mmdb: geonames-all: %s", status)
	return file, status
}
