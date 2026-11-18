package geo

import (
	"context"
	"path/filepath"
	"time"

	"openipdata.org/hatchet/geofeeds"
)

// The GitHub geofeed layer (39-github-geofeeds). sync-geofeeds searches public GitHub
// repositories whose name contains "geofeed", keeps the files in them that are geofeeds and that
// no whois record links, and uploads their rows to the fixed R2 key geofeeds.GitHubRowsKey.
// build-geo-data puts the file in as it is, with -same-country: a repository is the operator's
// own claim, but nothing says the record's country is theirs, so the layer adds cities and never
// changes a country. It sits between 38-asn-geofeed and 40-hosting, so 40-hosting, 50-ix,
// 60-whois, 65-rdns, 67-rdns-names and 70-geofeeds replace it and it only shows where nothing
// stronger gives a city.
//
// githubFeedsMaxAge: sync-geofeeds writes the file with every export-rows run and renews the
// object, so a copy up to 14 days old is still used. A missing or older object, or any fetch
// error, skips the layer with a warning and never skips the health check ping: the layer is
// optional and may be switched off on purpose.
//
// The holdout build leaves the layer out: export-rows drops a feed that is read under another URL
// (same repository or same rows), but a copy of a held-out feed that differs by a row or by row
// order passes both rules and would bring the held-out cities back.
const (
	// GitHubFeedsLayer is the layer file in DataDir/geo/layers.
	GitHubFeedsLayer = geofeeds.GitHubRowsFile

	githubFeedsMaxAge = 14 * 24 * time.Hour
)

// downloadGitHubFeeds puts the 39-github-geofeeds layer into the layers directory as it is
// (fetchOptionalLayer does the ETag and age rules, in downloads).
func (p *Pipeline) downloadGitHubFeeds(ctx context.Context, fetch r2Fetch, downloads, layers string, now time.Time, src *Source) error {
	saved := filepath.Join(downloads, GitHubFeedsLayer)
	if ok, err := p.fetchOptionalLayer(ctx, fetch, saved, geofeeds.GitHubRowsKey, GitHubFeedsLayer, "sync-geofeeds", githubFeedsMaxAge, now, src); err != nil || !ok {
		return err
	}
	if err := copyFile(saved, filepath.Join(layers, GitHubFeedsLayer)); err != nil {
		return err
	}
	p.log("download: %s uploaded %s", GitHubFeedsLayer, src.R2Uploaded[GitHubFeedsLayer])
	return nil
}
