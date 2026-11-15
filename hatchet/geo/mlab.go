package geo

import (
	"context"
	"path/filepath"
	"time"

	"openipdata.org/hatchet/mlab"
)

// The M-Lab hop layer (36-mlab). import-mlab turns the M-Lab traceroute export the owner copies
// into its inbox (by hand, about once a month) into this layer and uploads it to the fixed R2 key
// mlab.LayerKey: each client /24 gets the city of its last router hops (a router-name rule, else
// the hop IP's own city in the published MMDB when its source is a strong one). build-geo-data
// puts the file in as it is, with -same-country (it adds cities, never changes a country); like
// 25-bgp it sits below the later layers, so it only shows where nothing stronger gives a city.
//
// mlabMaxAge: the export is made by hand about monthly, so the layer is used until it is 45 days
// old (by the object's modified time; import-mlab renews it only when it runs). A missing or
// older object, or any fetch error, skips the layer with a warning and never skips the health
// check ping: the layer is optional and does not come from a daily job.
//
// The holdout build passes the layer as it is: it is not learned from the geofeed rows directly.
// Its MMDB-hop part does read the published MMDB (which holds geofeed cities), like the MMDB hop
// votes of 35-rdns-wide, so a held-out feed can reach the holdout score through it a little; the
// holdout is a trend, not an absolute score.
const (
	// MLabLayer is the layer file in DataDir/geo/layers.
	MLabLayer = mlab.LayerFile

	mlabMaxAge = 45 * 24 * time.Hour
)

// downloadMLab puts the 36-mlab layer into the layers directory as it is (fetchOptionalLayer
// does the ETag and age rules, in downloads).
func (p *Pipeline) downloadMLab(ctx context.Context, fetch r2Fetch, downloads, layers string, now time.Time, src *Source) error {
	saved := filepath.Join(downloads, MLabLayer)
	if ok, err := p.fetchOptionalLayer(ctx, fetch, saved, mlab.LayerKey, MLabLayer, "import-mlab", mlabMaxAge, now, src); err != nil || !ok {
		return err
	}
	if err := copyFile(saved, filepath.Join(layers, MLabLayer)); err != nil {
		return err
	}
	p.log("download: %s uploaded %s", MLabLayer, src.R2Uploaded[MLabLayer])
	return nil
}
