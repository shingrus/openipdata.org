package geo

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"openipdata.org/hatchet/download"
)

// cloudOperator is one operator of the 68-cloud-regions layer.
type cloudOperator struct {
	name   string // in log lines and the task output
	url    string // its region file
	file   string // the saved copy in the downloads directory
	source string // what the source column of its rows starts with
	feeds  bool   // its cities are learned from the geofeed layer file
	none   string // why it has no row although its file is good
	// rows makes the operator's rows from the bytes of its region file (and the geofeed layer file).
	rows func(data []byte, feeds string) ([]cloudRow, cloudStats, error)
}

// cloudOperators are the operators of the layer, in the order of their names in the output.
func (p *Pipeline) cloudOperators() []cloudOperator {
	aws := cloudOperator{name: "aws", url: p.cloudRegionsURL, file: cloudRegionsFile, source: cloudRegionsSource, feeds: true,
		none: "no region has a main geofeed city", rows: cloudRegionRows}
	if aws.url == "" {
		aws.url = cloudRegionsURL
	}
	oracle := cloudOperator{name: "oracle", url: p.oracleRegionsURL, file: oracleRegionsFile, source: oracleRegionsSource,
		none: "no region of the file is in the table",
		rows: func(data []byte, _ string) ([]cloudRow, cloudStats, error) { return oracleRegionRows(data) }}
	if oracle.url == "" {
		oracle.url = oracleRegionsURL
	}
	return []cloudOperator{aws, oracle}
}

// readCloudFile fetches one operator's region file into its saved copy (see downloadCloudRegions)
// and reads it. When the file cannot be had, why says so and data is nil; only a cancelled
// context or a local file error while reading the copy is an error.
func readCloudFile(ctx context.Context, client *http.Client, url, saved string, now time.Time) (data []byte, status, why string, err error) {
	status, err = fetchWithin(ctx, download.Capped(client, cloudRegionsMaxFile), url, saved, now, cloudRegionsMaxAge)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", "", ctx.Err()
		}
		// The address of a region file is public, so the error text may be logged as it is.
		return nil, "", fmt.Sprintf("the region file could not be fetched and there is no recent copy: %v", err), nil
	}
	if info, err := os.Stat(saved); err != nil || info.Size() > cloudRegionsMaxFile {
		return nil, "", "the saved region file is missing or too large", nil
	}
	data, err = os.ReadFile(saved)
	return data, status, "", err
}

// forgetCloudFile removes a bad saved copy so that the next run fetches the file again.
func forgetCloudFile(saved string) {
	os.Remove(saved + etagSuffix)
	os.Remove(saved + ".checked")
	os.Remove(saved)
}

// downloadCloudRegions writes the 68-cloud-regions layer from the region files of its operators:
// Amazon, whose cities are learned from the geofeed layer in the layers directory, and Oracle,
// whose cities come from a fixed table. Each operator is handled on its own: the trouble of one
// leaves only that one out (a log line, its name in src.CloudRegionsLeftOut) and never takes the
// rows of the other away. A file is fetched into downloads with the one shared downloader, capped
// at cloudRegionsMaxFile; when the fetch fails the saved copy is used while its last confirmation
// (the modification time of <file>.checked) is younger than cloudRegionsMaxAge. An unusable file
// is not kept. The layer is optional: with no row from any operator it is skipped
// (src.SkippedLayers, a warning). It never adds to src.StaleLayers, so the health check ping is
// still sent. Cancellation, or a local file error while reading a saved copy or writing the layer
// in the layers directory, fails; any problem while fetching or saving a region file (also a local
// one inside the downloader) only leaves that operator out. The layers directory is emptied at the
// start of Download, so no layer file of an earlier run is left when the layer is skipped.
func (p *Pipeline) downloadCloudRegions(ctx context.Context, client *http.Client, downloads, layers string, now time.Time, src *Source) error {
	feeds := filepath.Join(layers, FeedsLayer)
	haveFeeds := true
	if _, err := os.Stat(feeds); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		haveFeeds = false
	}
	var all [][]cloudRow
	var reasons []string
	for _, op := range p.cloudOperators() {
		rows, why, err := p.cloudOperatorRows(ctx, client, op, filepath.Join(downloads, op.file), feeds, haveFeeds, now, src)
		if err != nil {
			return err
		}
		if why != "" {
			p.log("download: cloud regions: %s left out: %s", op.name, why)
			src.CloudRegionsLeftOut = append(src.CloudRegionsLeftOut, op.name)
			reasons = append(reasons, op.name+": "+why)
			continue
		}
		all = append(all, rows)
	}
	if len(all) == 0 {
		p.skipLayer(src, CloudRegionsLayer, "no cloud region rows (%s)", strings.Join(reasons, "; "))
		return nil
	}
	rows, err := writeLayer(filepath.Join(layers, CloudRegionsLayer), func(w *csv.Writer) error {
		writeCloudRows(w, all...)
		return nil
	})
	if err != nil {
		return fmt.Errorf("cloud region layer: %w", err)
	}
	src.Rows[CloudRegionsLayer] = rows
	return nil
}

// cloudOperatorRows is one operator's part of downloadCloudRegions: its rows, or why it is left
// out, or an error that fails the task. It adds the operator's counts to src.
func (p *Pipeline) cloudOperatorRows(ctx context.Context, client *http.Client, op cloudOperator, saved, feeds string, haveFeeds bool, now time.Time, src *Source) (rows []cloudRow, why string, err error) {
	if op.feeds && !haveFeeds {
		return nil, "no " + FeedsLayer, nil
	}
	data, status, why, err := readCloudFile(ctx, client, op.url, saved, now)
	if err != nil || why != "" {
		return nil, why, err
	}
	rows, stats, err := op.rows(data, feeds)
	var inputErr *cloudInputError
	if errors.As(err, &inputErr) {
		// A bad copy must not be kept as current: fetch it again next run.
		forgetCloudFile(saved)
		return nil, "the region file is unusable: " + err.Error(), nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("cloud region rows (%s): %w", op.name, err)
	}
	src.CloudRegions += stats.Qualified
	src.CloudRegionsSeen += stats.Seen
	p.log("download: cloud regions: %s %s: %d of %d regions got a city, %d entries (%d bad), %d rows", op.name, status, stats.Qualified, stats.Seen, stats.Parse.Entries, stats.Parse.Bad, len(rows))
	if len(rows) == 0 {
		return nil, op.none, nil
	}
	return rows, "", nil
}

// cloudLayerSources reports which operators have a row in a cloud region layer file.
func cloudLayerSources(path string, operators []cloudOperator) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	have := map[string]bool{}
	for len(have) < len(operators) {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		for _, op := range operators {
			if len(rec) == 5 && strings.HasPrefix(rec[4], op.source) {
				have[op.name] = true
			}
		}
	}
	return have, nil
}

// holdoutCloudRegions recomputes the cloud region layer into out and sets replace[CloudRegionsLayer]
// to it. It never fetches. Only the operators that have a row in the layer file of this run take
// part (the saved copy of an operator Download left out may be an old one). Amazon's rows are
// recomputed from the kept feeds file and the region file Download saved; Oracle's come from its
// saved file as they are (they are not learned from geofeed rows). A saved file that is missing or
// unusable, or an operator with no row, only leaves that operator out, and the returned note
// names it. When no operator has rows the layer is left out of the holdout build
// (replace[...] = "") and the note says so.
func (p *Pipeline) holdoutCloudRegions(kept, out string, replace map[string]string) (note string, err error) {
	replace[CloudRegionsLayer] = ""
	operators := p.cloudOperators()
	inLayer, err := cloudLayerSources(filepath.Join(p.layersDir(), CloudRegionsLayer), operators)
	if err != nil {
		return "", fmt.Errorf("holdout cloud region layer: %w", err)
	}
	var all [][]cloudRow
	var without []string
	for _, op := range operators {
		if !inLayer[op.name] {
			continue
		}
		why := ""
		data, err := os.ReadFile(filepath.Join(p.dir(), "downloads", op.file))
		switch {
		case os.IsNotExist(err):
			why = "no saved region file"
		case err != nil:
			return "", fmt.Errorf("holdout region file (%s): %w", op.name, err)
		default:
			rows, _, err := op.rows(data, kept)
			var inputErr *cloudInputError
			switch {
			case errors.As(err, &inputErr):
				why = "saved region file unusable"
			case err != nil:
				return "", fmt.Errorf("holdout cloud region rows (%s): %w", op.name, err)
			case len(rows) == 0:
				why = op.none
			default:
				all = append(all, rows)
			}
		}
		if why != "" {
			without = append(without, op.name+": "+why)
		}
	}
	if len(all) == 0 {
		os.Remove(out)
		return "; " + CloudRegionsLayer + " left out: " + strings.Join(without, ", "), nil
	}
	if _, err := writeLayer(out, func(w *csv.Writer) error {
		writeCloudRows(w, all...)
		return nil
	}); err != nil {
		return "", fmt.Errorf("holdout cloud region layer: %w", err)
	}
	replace[CloudRegionsLayer] = out
	if len(without) > 0 {
		note = "; " + CloudRegionsLayer + " without " + strings.Join(without, ", ")
	}
	return note, nil
}
