package geo

import (
	"context"
	"path/filepath"
	"time"

	"openipdata.org/hatchet/arin"
)

// The ARIN registrant city layer (37-whois-arin). sync-arin-whois asks ARIN's RDAP service about
// the IPv4 /24s where our own database has a country but only a weak city or none, and uploads the
// cities of the small records (at most a /20) that GeoNames places in the record's own state to the
// fixed R2 key arin.LayerKey. build-geo-data puts the file in as it is, with -same-country: the
// registrant's address names a place, but the record's country may be the holder's, so the layer
// adds cities and never changes a country. It sorts between 36-mlab and 37-whois-text, so the
// later layers replace it.
//
// arinMaxAge: sync-arin-whois writes the file every 6 hours and renews the object on every run
// (every 30 minutes), so a copy up to 14 days old is still used. A missing or older object, or any fetch error, skips the layer with a
// warning and never skips the health check ping: the layer is optional and may be switched off
// on purpose.
//
// The holdout build passes the layer as it is: it is not learned from the geofeed rows.
const (
	// ARINLayer is the layer file in DataDir/geo/layers.
	ARINLayer = arin.LayerFile

	arinMaxAge = 14 * 24 * time.Hour
)

// downloadARIN puts the 37-whois-arin layer into the layers directory as it is
// (fetchOptionalLayer does the ETag and age rules, in downloads).
func (p *Pipeline) downloadARIN(ctx context.Context, fetch r2Fetch, downloads, layers string, now time.Time, src *Source) error {
	saved := filepath.Join(downloads, ARINLayer)
	if ok, err := p.fetchOptionalLayer(ctx, fetch, saved, arin.LayerKey, ARINLayer, "sync-arin-whois", arinMaxAge, now, src); err != nil || !ok {
		return err
	}
	if err := copyFile(saved, filepath.Join(layers, ARINLayer)); err != nil {
		return err
	}
	p.log("download: %s uploaded %s", ARINLayer, src.R2Uploaded[ARINLayer])
	return nil
}
