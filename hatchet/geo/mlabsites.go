package geo

import (
	"context"
	"path/filepath"
	"time"

	"openipdata.org/hatchet/mlab"
)

// The M-Lab speed test site layer (35-speedtest-site). import-mlab-sites (task site-layer) turns the
// reply time export that is copied into its sites inbox by hand into this layer and uploads it to
// the fixed R2 key mlab.SiteLayerKey: a client /24 whose lowest reply time to an M-Lab speed test
// server is at most 5 ms gets the city of that server. build-geo-data puts the file in as it is,
// with -same-country (it adds cities, never changes a country). It sorts between 35-rdns-wide and
// 36-mlab, so 36-mlab and every later layer replace it.
//
// The age limit is mlabMaxAge, as for 36-mlab: the export is made by hand about monthly. The age
// counts from the last new export: import-mlab-sites does not renew the date of an object that already
// holds the same file. A missing or older object, or any fetch error, skips the layer with a
// warning and never skips the health check ping.
//
// The holdout build passes the layer as it is: it is not learned from the geofeed rows and it
// reads no MMDB.
const (
	// MLabSitesLayer is the layer file in DataDir/geo/layers.
	MLabSitesLayer = mlab.SiteLayerFile
)

// downloadMLabSites puts the 35-speedtest-site layer into the layers directory as it is
// (fetchOptionalLayer does the ETag and age rules, in downloads).
func (p *Pipeline) downloadMLabSites(ctx context.Context, fetch r2Fetch, downloads, layers string, now time.Time, src *Source) error {
	saved := filepath.Join(downloads, MLabSitesLayer)
	if ok, err := p.fetchOptionalLayer(ctx, fetch, saved, mlab.SiteLayerKey, MLabSitesLayer, "import-mlab-sites", mlabMaxAge, now, src); err != nil || !ok {
		return err
	}
	if err := copyFile(saved, filepath.Join(layers, MLabSitesLayer)); err != nil {
		return err
	}
	p.log("download: %s uploaded %s", MLabSitesLayer, src.R2Uploaded[MLabSitesLayer])
	return nil
}
