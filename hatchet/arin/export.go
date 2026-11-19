package arin

import (
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"openipdata.org/hatchet/rirstats"
)

// ExportOutput counts an export run (counts only).
type ExportOutput struct {
	Skipped       string `json:"skipped,omitempty"` // "recent", "no GeoNames" or "no rows" (the old layer file stays)
	GeoNames      string `json:"geonames,omitempty"`
	Records       int    `json:"records"`        // records with a city, of at most a /20
	Rows          int    `json:"rows"`           // prefixes written
	Unplaced      int    `json:"unplaced"`       // GeoNames has no place of that name
	OtherState    int    `json:"other_state"`    // the place is in another state than the record's
	Ambiguous     int    `json:"ambiguous"`      // the place's main name would be placed elsewhere
	Pruned        int    `json:"pruned"`         // records older than 300 days deleted
	StoredRecords int    `json:"stored_records"` // all records of the file after the compaction

	HolderRecords     int  `json:"holder_records"`               // rows of holders.csv after the compaction
	HolderCandidates  int  `json:"holder_candidates"`            // of them, edu or gov, at most a /16, with a city
	HolderRows        int  `json:"holder_rows"`                  // layer rows that came from them
	HoldersPruned     int  `json:"holders_pruned"`               // holder rows older than 300 days deleted
	HoldersUnreadable bool `json:"holders_unreadable,omitempty"` // holders.csv could not be read or written: the layer is made without it

	HolderCityRecords int `json:"holder_city_records"` // candidates of the holder blocks file (main records wider than a /20, holder rows wider than a /16)
	HolderCityDropped int `json:"holder_city_dropped"` // of them unplaced, in another state or ambiguous
	HolderCityRows    int `json:"holder_city_rows"`    // prefixes written to the holder blocks file
	// HolderCityFile is what happened to the holder blocks file: written, removed (no row), kept
	// (holders.csv could not be used, the file is left as it is) or failed. A problem never fails the task.
	HolderCityFile string `json:"holder_city_file,omitempty"`

	Bytes           int64   `json:"bytes"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// place is a GeoNames place of the US or Canada as the layer needs it.
type place struct {
	id      int64
	name    string // the main name
	admin1  string
	pop     int64
	primary bool // the key is the place's own name or ascii name, not an alternate
}

// placeIndex is the placing rule of "mmdbbuilder geo -geonames" (loadGeoNames and place in
// mmdbbuilder/eval.go), kept for the US and Canada, the only countries of ARIN records: the key is the
// country and the lower-cased, trimmed name; every place is indexed under its name and ascii name
// (primary) and each alternate name that has a Latin letter; a primary name beats an alternate,
// otherwise the more populous place wins, the first on a tie. hatchet must not import mmdbbuilder.
type placeIndex map[string]place

func placeKey(country, name string) string {
	return strings.ToUpper(strings.TrimSpace(country)) + "|" + strings.ToLower(strings.TrimSpace(name))
}

func (x placeIndex) place(country, name string) (place, bool) {
	p, ok := x[placeKey(country, name)]
	return p, ok
}

// loadPlaces reads cities1000.zip (cities1000.txt inside, tab separated).
func loadPlaces(path string) (placeIndex, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var f io.ReadCloser
	for _, zf := range zr.File {
		if zf.Name == "cities1000.txt" {
			if f, err = zf.Open(); err != nil {
				return nil, err
			}
			break
		}
	}
	if f == nil {
		return nil, errors.New("no cities1000.txt")
	}
	defer f.Close()
	return readPlaces(f)
}

func readPlaces(r io.Reader) (placeIndex, error) {
	index := placeIndex{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 15 {
			continue
		}
		_, latErr := strconv.ParseFloat(fields[4], 64)
		_, lonErr := strconv.ParseFloat(fields[5], 64)
		country := strings.ToUpper(strings.TrimSpace(fields[8]))
		if len(country) != 2 || latErr != nil || lonErr != nil {
			continue
		}
		if country != "US" && country != "CA" {
			continue
		}
		id, _ := strconv.ParseInt(fields[0], 10, 64)
		pop, _ := strconv.ParseInt(fields[14], 10, 64)
		pl := place{id: id, name: fields[1], admin1: fields[10], pop: pop}
		set := func(name string, primary bool) {
			key := placeKey(country, name)
			old, ok := index[key]
			if !ok || (primary && !old.primary) || (primary == old.primary && pop > old.pop) {
				pl.primary = primary
				index[key] = pl
			}
		}
		for _, name := range fields[1:3] {
			if strings.TrimSpace(name) != "" {
				set(name, true)
			}
		}
		for _, name := range strings.Split(fields[3], ",") {
			if hasLatin(strings.ToLower(strings.TrimSpace(name))) {
				set(name, false)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(index) == 0 {
		return nil, errors.New("no places")
	}
	return index, nil
}

func hasLatin(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= 'a' && r <= 'z' })
}

// caAdmin1 maps the GeoNames admin1 codes of the Canadian provinces to their abbreviations.
var caAdmin1 = map[string]string{
	"01": "AB", "02": "BC", "03": "MB", "04": "NB", "05": "NL", "07": "NS",
	"08": "ON", "09": "PE", "10": "QC", "11": "SK", "12": "YT", "13": "NT", "14": "NU",
}

// stateOf is the state or province abbreviation of a GeoNames place; empty when unknown.
func stateOf(country, admin1 string) string {
	if country == "CA" {
		return caAdmin1[admin1]
	}
	return admin1 // the US: GeoNames uses the state abbreviation
}

// layerRow is one row of the layer. size is the number of addresses of the record it came from.
type layerRow struct {
	start   uint32
	bits    int
	size    uint64
	prefix  string
	state   string
	city    string
	country string
	holder  bool  // the row came from holders.csv
	at      int64 // looked_up of the record the row came from
}

// buildRows turns the records into layer rows (see the rules in the package comment of
// the README): the city must be a place GeoNames has in the record's own state, written under its
// main name, and only when that name places the same place again.
//
// holders are the compacted rows of holders.csv: those of class edu or gov, with a usable country,
// state and city and at most maxStoredBlock addresses (a /16) join the main records (whose own limit
// is maxLayerBlock) under exactly the same rules.
func buildRows(recs, holders []record, idx placeIndex, out *ExportOutput) []layerRow {
	var rows []layerRow
	var c placeCounts
	for _, r := range recs {
		if r.size() > maxLayerBlock || r.Country == "" || r.State == "" || r.City == "" {
			continue
		}
		out.Records++
		rows = append(rows, placeRecord(r, false, idx, &c)...)
	}
	for _, r := range holders {
		if (r.Class != classEdu && r.Class != classGov) || r.size() > maxStoredBlock || r.Country == "" || r.State == "" || r.City == "" {
			continue
		}
		out.HolderCandidates++
		rows = append(rows, placeRecord(r, true, idx, &c)...)
	}
	out.Unplaced += c.unplaced
	out.OtherState += c.otherState
	out.Ambiguous += c.ambiguous
	kept := sortRows(rows)
	for _, r := range kept {
		if r.holder {
			out.HolderRows++
		}
	}
	return kept
}

// placeCounts counts the records placeRecord dropped.
type placeCounts struct{ unplaced, otherState, ambiguous int }

// placeRecord is the placing rule of the layer: the city must be a place GeoNames has in the
// record's own state, written under its main name, and only when that name places the same place
// again. It returns the CIDR prefixes of the record as rows, or none (counted in c).
func placeRecord(r record, holder bool, idx placeIndex, c *placeCounts) []layerRow {
	pl, ok := idx.place(r.Country, r.City)
	if !ok {
		c.unplaced++
		return nil
	}
	if stateOf(r.Country, pl.admin1) != r.State {
		c.otherState++
		return nil
	}
	if again, ok := idx.place(r.Country, pl.name); !ok || again.id != pl.id {
		c.ambiguous++
		return nil
	}
	var rows []layerRow
	for _, p := range rirstats.RangeToPrefixes(addrOf(r.Start), addrOf(r.End)) {
		rows = append(rows, layerRow{start: u32(p.Addr()), bits: p.Bits(), size: r.size(), prefix: p.String(), country: r.Country, state: r.State, city: pl.name, holder: holder, at: r.LookedUp})
	}
	return rows
}

// sortRows orders rows by start address, the broader block first, and drops repeated prefixes.
// The same prefix can come from two records that overlap: the one from the smaller record goes
// first (it is the more specific answer); for the same range the later looked_up goes first (a
// holder row and a main record), and a full tie breaks on the text, so the file is the same every
// time. The first row of a prefix stays.
func sortRows(rows []layerRow) []layerRow {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case a.start != b.start:
			return a.start < b.start
		case a.bits != b.bits:
			return a.bits < b.bits
		case a.size != b.size:
			return a.size < b.size
		case a.at != b.at:
			return a.at > b.at // the same range: the later answer wins
		case a.city != b.city:
			return a.city < b.city
		case a.state != b.state:
			return a.state < b.state
		}
		return !a.holder && b.holder
	})
	kept := rows[:0]
	for _, r := range rows {
		if n := len(kept); n > 0 && kept[n-1].prefix == r.prefix {
			continue // the same prefix from two records: the first one stays
		}
		kept = append(kept, r)
	}
	return kept
}

// buildHolderCityRows makes the rows of the holder blocks file: main records wider than a /20 and
// holders.csv rows wider than a /16, whatever their class, with a usable country, state and city,
// through the placing rule of the layer. It returns the rows, the number of candidates and how
// many were dropped (unplaced, in another state or ambiguous).
func buildHolderCityRows(recs, holders []record, idx placeIndex) (rows []layerRow, candidates, dropped int) {
	var c placeCounts
	for _, r := range recs {
		if r.size() <= maxLayerBlock || r.Country == "" || r.State == "" || r.City == "" {
			continue
		}
		candidates++
		rows = append(rows, placeRecord(r, false, idx, &c)...)
	}
	for _, r := range holders {
		if r.size() <= maxStoredBlock || r.Country == "" || r.State == "" || r.City == "" {
			continue
		}
		candidates++
		rows = append(rows, placeRecord(r, true, idx, &c)...)
	}
	return sortRows(rows), candidates, c.unplaced + c.otherState + c.ambiguous
}

// writeLayer writes the layer file (gzipped CSV in the geofeed layout) through a temporary file.
func writeLayer(path string, rows []layerRow) (int64, error) {
	return writeRows(path, rows, layerSource)
}

// writeRows is writeLayer with the source text given.
func writeRows(path string, rows []layerRow, source string) (int64, error) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	gz := gzip.NewWriter(f)
	w := csv.NewWriter(gz)
	w.Write([]string{"prefix", "country", "region", "city", "source"})
	for _, r := range rows {
		w.Write([]string{r.prefix, r.country, r.state, r.city, source})
	}
	w.Flush()
	if err := errors.Join(w.Error(), gz.Close(), f.Close()); err != nil {
		return 0, err
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		return 0, err
	}
	return fi.Size(), os.Rename(tmp, path)
}

// Export writes the layer from the stored records (see the package comment). It does nothing when
// the layer file is younger than six hours (unless force); with no GeoNames copy or no row the
// old layer file stays. The records file is compacted first and records older than 300 days are deleted.
func (p *Pipeline) Export(ctx context.Context, force bool) (ExportOutput, error) {
	started := time.Now()
	var out ExportOutput
	if !force {
		if fi, err := os.Stat(p.LayerPath()); err == nil {
			if age := p.clock().Sub(fi.ModTime()); age >= 0 && age < exportEvery {
				out.Skipped = skipRecent
				p.log("export: skipped (the layer file is %s old)", age.Round(time.Minute))
				return out, nil
			}
		}
	}
	unlock, ok, err := p.lock()
	if err != nil {
		return out, err
	}
	if !ok {
		out.Skipped = skipBusy
		p.log("export: skipped (another task or run-arin is working in the directory)")
		return out, nil
	}
	defer unlock()
	// The records: the journal is merged into the base and old records go, whatever GeoNames does.
	recs, pruned, err := p.newStore().Compact(ctx, p.clock().UTC())
	if errors.Is(err, errNoBase) {
		// Only lookup makes a base (after asking R2 for the copy): nothing to export yet.
		out.Skipped = skipNoBase
		p.log("export: skipped (%s)", skipNoBase)
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Pruned, out.StoredRecords = pruned, len(recs)
	// The holders: a file that cannot be read or written only leaves them out.
	holders, hpruned, herr := compactHolders(p.dir(), p.clock().UTC())
	if herr != nil {
		out.HoldersUnreadable = true
		p.log("export: WARNING the holders file cannot be used; the layer is made without it")
		holders, hpruned = nil, 0
	}
	out.HolderRecords, out.HoldersPruned = len(holders), hpruned
	geoPath := filepath.Join(p.dir(), geoNamesFile)
	url := p.geoURL
	if url == "" {
		url = geoNamesURL
	}
	if out.GeoNames, err = p.fetchGated(ctx, url, geoPath, geoNamesGate); err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		out.Skipped, out.GeoNames = skipNoGeoNames, "failed"
		p.log("export: skipped (%s)", skipNoGeoNames)
		return out, nil
	}
	idx, err := loadPlaces(geoPath)
	if err != nil {
		os.Remove(geoPath) // a damaged copy must not stay
		os.Remove(geoPath + checkedSuffix)
		os.Remove(geoPath + ".etag")
		out.Skipped, out.GeoNames = skipNoGeoNames, "unreadable"
		p.log("export: skipped (%s)", skipNoGeoNames)
		return out, nil
	}

	rows := buildRows(recs, holders, idx, &out)
	out.Rows = len(rows)
	if len(rows) == 0 {
		// No record is left to make a row from: the layer file goes too, so upload stops renewing
		// the R2 object and build-geo-data drops the layer once the object is 14 days old.
		if err := os.Remove(p.LayerPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return out, err
		}
		p.exportHolderCities(recs, holders, idx, &out)
		out.Skipped = skipNoRows
		p.log("export: skipped (%s); %d records", skipNoRows, out.Records)
		return out, nil
	}
	if out.Bytes, err = writeLayer(p.LayerPath(), rows); err != nil {
		return out, fmt.Errorf("write %s: %w", LayerFile, err)
	}
	p.exportHolderCities(recs, holders, idx, &out)
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("export: %d records and %d holder candidates, %d unplaced, %d in another state, %d ambiguous; %d rows, %d from holders (%d bytes); %d old removed",
		out.Records, out.HolderCandidates, out.Unplaced, out.OtherState, out.Ambiguous, out.Rows, out.HolderRows, out.Bytes, out.Pruned)
	return out, nil
}

// Results of the holder blocks file (ExportOutput.HolderCityFile).
const (
	holderCitiesWritten = "written"
	holderCitiesRemoved = "removed" // no row: the file is gone
	holderCitiesKept    = "kept"    // holders.csv could not be used: the file is left as it is
)

// exportHolderCities writes the holder blocks file. Export calls it right after the layer is
// written or removed, so it is made also when the layer has no row and never keeps the layer
// back. A problem with it never fails the task: the result is only in the output and one
// WARNING. When holders.csv could not be used the file is left as it is: made from the main
// records alone it would lose every block wider than a /16 until the next good run.
func (p *Pipeline) exportHolderCities(recs, holders []record, idx placeIndex, out *ExportOutput) {
	if out.HoldersUnreadable {
		out.HolderCityFile = holderCitiesKept
		p.log("export: WARNING the holder blocks file is left as it is (the holders file cannot be used)")
		return
	}
	rows, candidates, dropped := buildHolderCityRows(recs, holders, idx)
	out.HolderCityRecords, out.HolderCityDropped, out.HolderCityRows = candidates, dropped, len(rows)
	out.HolderCityFile = holderCitiesWritten
	var err error
	if len(rows) == 0 {
		out.HolderCityFile = holderCitiesRemoved
		if err = os.Remove(p.HolderCitiesPath()); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		_, err = writeRows(p.HolderCitiesPath(), rows, holderCitySource)
	}
	if err != nil {
		out.HolderCityFile = holdersFailed
		p.log("export: WARNING the holder blocks file could not be written")
		return
	}
	p.log("export: holder blocks: %d candidates, %d dropped, %d rows", candidates, dropped, len(rows))
}
