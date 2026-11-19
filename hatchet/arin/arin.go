// Package arin is the sync-arin-whois workflow. ARIN has no public whois dump. The workflow asks
// ARIN's RDAP service about one address of each IPv4 /24 where our own published ip2geo database
// has a country but only a weak city or none (the gap), keeps the network record it gets back
// (range, registrant city, state, country) in a file on the VM (a copy goes to private R2 once a day), and writes the ip2geo layer
// 37-whois-arin from the small records.
//
// Each Pipeline method is one task; the Hatchet workflow and the local run-arin command call
// them in the same order:
//
//	Lookup  the gap /24s inside ARIN's address space, one RDAP lookup each (a walk over all /24s
//	        from a saved cursor, 2 lookups a second) -> records.csv.gz and its journal
//	Export  the stored records with a city that GeoNames places in the record's own state
//	        -> 37-whois-arin.csv.gz, and the wider records the same way -> holder-cities.csv.gz
//	Upload  the layer -> the fixed key LayerKey in the private R2 bucket (build-geo-data reads it),
//	        the holder blocks file -> HolderCitiesKey (build-geo-data reads it too), and once a day
//	        the records file -> RecordsKey (this workflow's own backup)
//
// Measured on RIPE Atlas: replacing weak answers with the registrant city of ARIN records of at
// most a /20, keeping only cities that GeoNames places in the record's own state, gives +197 right
// and -37 lost probes (61% of the 531 rows right). A record wider than a /16 holds the head office
// (9% right) and is not in the layer; records of at most a /16 whose holder class (derived from the
// registrant's name, which is never kept) is edu or gov also join it, and wide records up to a /8 are
// kept with their class in holders.csv. Export also writes the holder blocks file (HolderCitiesFile):
// the registrant city of every record wider than a /20, which build-geo-data gives to the builder
// for the guess 85-holder-city (only address space that no network announces gets it).
//
// The answers are untrusted input: only the range, country, state and city are kept (never a
// handle or a name), bad answers are counted, and no IP, name or secret is put in a log line,
// output or returned error.
package arin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"time"

	"openipdata.org/hatchet/rdns"
)

const (
	// LayerFile is the ip2geo layer this workflow writes (in DataDir/arin) and build-geo-data reads.
	LayerFile = "37-whois-arin.csv.gz"
	// LayerKey is the one fixed key in the private R2 bucket, replaced by each upload.
	LayerKey = "internal/layers/" + LayerFile

	layerSource = "whois-city:arin" // the "source" column of the layer rows

	// HolderCitiesFile is the holder blocks file (in DataDir/arin): the city of the registrant for
	// wide blocks, in the layer format, read by build-geo-data for the guess 85-holder-city. It is
	// not a layer file.
	HolderCitiesFile = "holder-cities.csv.gz"
	// HolderCitiesKey is its fixed key in the private R2 bucket.
	HolderCitiesKey = "internal/arin/" + HolderCitiesFile

	holderCitySource = "holder-city:arin" // the "source" column of its rows

	// HoldersKey is the fixed key of the daily copy of holders.csv in the private R2 bucket (this
	// workflow's own backup, like RecordsKey).
	HoldersKey = "internal/arin/holders.csv.gz"

	rdapBase      = "https://rdap.arin.net/registry"
	statsURL      = "https://ftp.arin.net/pub/stats/arin/delegated-arin-extended-latest"
	geoNamesURL   = "https://download.geonames.org/export/dump/cities1000.zip"
	statsFile     = "delegated-arin-extended-latest"
	geoNamesFile  = "cities1000.zip"
	gapsFile      = "gaps.txt" // the output of mmdbbuilder gaps; its MMDB's ETag is in gapsFile + etagSuffix
	etagSuffix    = ".etag"
	checkedSuffix = ".checked" // the time of the last request for a downloaded file is the stamp's mtime

	// Rates: two lookups a second over all workers together.
	lookupInterval = 500 * time.Millisecond
	lookupWorkers  = 4
	lookupBatch    = 200 // /24s looked up and stored together
	// lookupBudget is how long the lookups may take; the task's own timeout is 30 minutes and the
	// cron is every 30 minutes. A run that reaches it stores the batches it finished and stops.
	lookupBudget = 24 * time.Minute
	// maxBatchSteps ends a batch after this many walk positions, so the cursor always moves.
	maxBatchSteps = 200000
	// batchSlack: a batch is started only when the budget still has room for its lookups plus
	// this many more intervals (answers take time too).
	batchSlack = 20

	// maxStoredBlock: a wider record is not stored (it holds the head office). maxLayerBlock: only
	// records of at most this many addresses go into the layer.
	maxStoredBlock = 65536
	maxLayerBlock  = 4096
	// maxHolderBlock: a wider record is dropped even from the holders file (a /8).
	maxHolderBlock = 16777216

	cycleLength = 180 * 24 * time.Hour // a /24 is asked again about every 180 days
	pruneAfter  = 300 * 24 * time.Hour // a record not asked again for this long is deleted
	exportEvery = 6 * time.Hour

	statsGate    = 24 * time.Hour
	geoNamesGate = 7 * 24 * time.Hour

	// Fixed reasons of a skipped task (never an error's text).
	skipNoStats    = "no ARIN stats"
	skipCycleDone  = "cycle done"
	skipRecent     = "recent"
	skipNoGeoNames = "no GeoNames"
	skipNoRows     = "no rows"
	skipNoLayer    = "no layer file"
	skipNoBase     = "no records file"
	reasonGaps     = "mmdbbuilder gaps failed"
	reasonNoGaps   = "no gap /24s"
	reasonNoBuild  = "mmdbbuilder path unknown"
)

// Pipeline runs sync-arin-whois.
type Pipeline struct {
	DataDir  string
	Progress func(string)

	// MMDBBuilder is the mmdbbuilder binary next to the worker, and R2 the setting of the private
	// bucket (the published ip2geo MMDB is read from it, the layer is uploaded to it).
	MMDBBuilder string
	R2          string

	// Tests only.
	db         lookupStore
	objects    recordsR2    // R2 for the records file, instead of one made from R2
	client     *http.Client // lookups (must not follow redirects)
	dlClient   *http.Client // downloads
	base       string       // instead of rdapBase
	statsURL   string
	geoURL     string
	mmdbStore  rdns.ObjectStore
	gaps       gapsFunc
	upload     uploadFunc
	now        func() time.Time
	interval   time.Duration
	budget     time.Duration
	batch      int
	workers    int
	maxSteps   int
	size       int                   // the walk size, instead of rdns.WalkSize
	at         func(uint32) uint32   // the walk, instead of rdns.WalkAt
	public     func(netip.Addr) bool // instead of ipdata.PublicV4
	retryPause time.Duration         // between tries of a failing lookup (default 2 s)
	backoff    time.Duration         // first wait after a 429 without Retry-After (default 15 s)
	retryUnit  time.Duration         // the unit of Retry-After (default 1 s)
	maxWait    time.Duration         // the longest pause after a 429 (default 5 minutes)
}

func (p *Pipeline) dir() string       { return filepath.Join(p.DataDir, "arin") }
func (p *Pipeline) LayerPath() string { return filepath.Join(p.dir(), LayerFile) }

// HolderCitiesPath is the holder blocks file.
func (p *Pipeline) HolderCitiesPath() string { return filepath.Join(p.dir(), HolderCitiesFile) }

func (p *Pipeline) log(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

func (p *Pipeline) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// newClient makes the HTTP client: IPv4 only (on the VM, IPv6 egress stalls for many hosts). A
// lookup client never follows a redirect: a redirect means the address is another registry's.
func newClient(timeout time.Duration, follow bool) *http.Client {
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", addr)
	}
	transport.Proxy = nil
	c := &http.Client{Transport: transport, Timeout: timeout}
	if !follow {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return c
}

func (p *Pipeline) lookupClient() *http.Client {
	if p.client != nil {
		return p.client
	}
	return newClient(30*time.Second, false)
}

func (p *Pipeline) downloadClient() *http.Client {
	if p.dlClient != nil {
		return p.dlClient
	}
	return newClient(10*time.Minute, true)
}
