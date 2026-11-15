// Package mlab is the import-mlab workflow. M-Lab (Measurement Lab) publishes, under CC0, a
// traceroute from its speed-test server back to every client. The owner exports from BigQuery,
// by hand about once a month, the last 3 router hops before each client /24 (see the README) and
// copies the CSV into the inbox (DataDir/mlab/inbox). The workflow turns it into the ip2geo layer
// 36-mlab: each client /24 gets the city of its hops.
//
// Each Pipeline method is one task; the Hatchet workflow and the local run-mlab command call
// them in the same order:
//
//	Import       the CSV -> pairs.csv.gz (one row per client /24 and hop IP; replaces the last one)
//	ResolveNames PTR names of the distinct hop IPs -> public.mlab_hop_names (this workflow is the
//	             only writer; learn-rdns-rules reads the ok names)
//	Locate       pairs + names + the production rules (+ the city of the hop IP in our own
//	             published ip2geo MMDB) -> 36-mlab.csv.gz
//	Upload       the layer -> the fixed key LayerKey in the private R2 bucket (build-geo-data reads it)
//
// A client /24 takes its city in two rounds (the variant measured on RIPE Atlas, "R then M"). R: the
// hops whose router name the rules of resolve-ips decode to a city (the legacy public.generated_rules,
// then the candidate rules of public.rdns_city_rules) vote, each with the traces it has there; the
// city with the largest weight wins. When R gives no winner (no rule city, or a tie), M: the hops
// with a city in the hop IP's own record in our published MMDB vote the same way, but only records
// whose city_source is one of mmdbStrongSources count (never a guess or a weak layer) and not a hop
// inside the client /24 itself. Only a city in the client /24's country (its country in our MMDB)
// votes. A tie in both rounds gives none.
//
// Hop IPs are public router addresses, but the CSV is untrusted input: bad rows are counted,
// never echoed, and no IP is put in a log line, output or returned error.
package mlab

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"openipdata.org/hatchet/rdns"
)

const (
	// LayerFile is the ip2geo layer this workflow writes (in DataDir/mlab) and build-geo-data reads.
	LayerFile = "36-mlab.csv.gz"
	// LayerKey is the one fixed key in the private R2 bucket, replaced by each upload.
	LayerKey = "internal/layers/" + LayerFile

	pairsFile = "pairs.csv.gz"
	inboxDir  = "inbox"

	layerSource = "mlab" // the "source" column of the layer rows
)

// mmdbStrongSources are the city_sources of our own MMDB whose city of a hop IP may stand for the
// router's city: the sources that place a network from its own data. Everything else (80-asn-main,
// 25-bgp, 30-pdb, 35-rdns-wide, 36-mlab itself, 65-rdns, 67-rdns-names, no source) never counts.
var mmdbStrongSources = []string{"70-geofeeds", "60-whois", "40-hosting", "50-ix", "38-asn-geofeed"}

// ErrPermanent marks an error that no retry can fix (a bad input file, too few rows). The worker
// turns it into a non-retryable task error.
var ErrPermanent = errors.New("permanent")

func permanent(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPermanent, fmt.Sprintf(format, args...))
}

// Pipeline runs import-mlab.
type Pipeline struct {
	DataDir  string
	PGSQL    string
	Progress func(string)

	// MMDBBuilder is the mmdbbuilder binary next to the worker, and R2 the setting of the private
	// bucket (the published ip2geo MMDB is read from it, the layer is uploaded to it).
	MMDBBuilder string
	R2          string

	// Tests only.
	store        rdns.ObjectStore
	lookup       lookupFunc
	resolve      resolveFunc
	loadDecoder  func(ctx context.Context) (decodeFunc, error)
	readNames    func(ctx context.Context, hops []uint32) (map[uint32]string, error)
	upload       uploadFunc
	now          func() time.Time
	minRows      int
	minLayerRows int
	maxPairs     int
	budget       time.Duration
	batch        int
}

func (p *Pipeline) dir() string       { return filepath.Join(p.DataDir, "mlab") }
func (p *Pipeline) inbox() string     { return filepath.Join(p.dir(), inboxDir) }
func (p *Pipeline) pairsPath() string { return filepath.Join(p.dir(), pairsFile) }
func (p *Pipeline) LayerPath() string { return filepath.Join(p.dir(), LayerFile) }

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

// CheckPGSQL fails when PGSQL is not set, so a run stops before the first task works.
func (p *Pipeline) CheckPGSQL() error {
	if p.PGSQL == "" {
		return errors.New("PGSQL is not set")
	}
	return nil
}
