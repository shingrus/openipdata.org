package geo

import (
	"context"
	"path/filepath"
	"slices"
	"time"

	"openipdata.org/hatchet/geofeeds"
)

// The whois text layer (37-whois-text). sync-geofeeds reads the free text of the RIPE inetnum
// objects (descr and netname lines), names the city a text mentions (a place of GeoNames in the
// object's own country) and uploads the rows to the fixed R2 key geofeeds.WhoisTextKey.
// build-geo-data puts the file in as it is, with -same-country: the description names a place,
// but the record's country may be the holder's, so the layer adds cities and never changes a
// country. It sits between 36-mlab and 38-asn-geofeed, so the strong layers replace it and it
// only shows where nothing stronger gives a city.
//
// whoisTextMaxAge: the text of whois records changes slowly, so a copy up to 14 days old is
// better than none and is still used. But sync-geofeeds renews the object every night, so one
// older than rowsMaxAge means it is failing: the layer goes into src.StaleLayers (no health
// check ping, the owner is alerted) while the copy is still used (or, older than 14 days,
// skipped). A missing object or a fetch error only skips the layer with a warning and never
// the ping: the layer is optional and may be switched off on purpose. (To switch it off after
// it was uploaded once, also delete the object: one that is left in R2 turns stale.)
//
// The holdout build passes the layer as it is (no code: it takes every file of the layers
// directory and replaces only the geofeed layer and the layers learned from it). The layer is
// not learned from the geofeed rows, so held-out feeds do not reach it.
const (
	// WhoisTextLayer is the layer file in DataDir/geo/layers.
	WhoisTextLayer = geofeeds.WhoisTextFile

	whoisTextMaxAge = 14 * 24 * time.Hour
)

// downloadWhoisText puts the 37-whois-text layer into the layers directory as it is
// (fetchOptionalLayer does the ETag and age rules, in downloads).
func (p *Pipeline) downloadWhoisText(ctx context.Context, fetch r2Fetch, downloads, layers string, now time.Time, src *Source) error {
	saved := filepath.Join(downloads, WhoisTextLayer)
	ok, err := p.fetchOptionalLayer(ctx, fetch, saved, geofeeds.WhoisTextKey, WhoisTextLayer, "sync-geofeeds", whoisTextMaxAge, now, src)
	// R2Uploaded is set only when the object exists, so a missing one or a fetch error is not stale.
	if up, found := src.R2Uploaded[WhoisTextLayer]; found {
		if t, perr := time.Parse(time.RFC3339, up); perr == nil && now.Sub(t) > rowsMaxAge && !slices.Contains(src.StaleLayers, WhoisTextLayer) {
			src.StaleLayers = append(src.StaleLayers, WhoisTextLayer)
			p.log("download: WARNING %s is from %s; is sync-geofeeds failing?", WhoisTextLayer, up)
		}
	}
	if err != nil || !ok {
		return err
	}
	if err := copyFile(saved, filepath.Join(layers, WhoisTextLayer)); err != nil {
		return err
	}
	p.log("download: %s uploaded %s", WhoisTextLayer, src.R2Uploaded[WhoisTextLayer])
	return nil
}
