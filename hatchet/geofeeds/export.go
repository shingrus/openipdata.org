package geofeeds

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"openipdata.org/hatchet/publish"
	"openipdata.org/hatchet/rirstats"
)

const (
	// RowsFile, in OutDir, holds every row of every working feed that passes
	// the ownership check, as a layer file of build-geo-data (see layerHeader).
	RowsFile = "70-geofeeds.csv.gz"
	// RowsKey is where build-geo-data finds RowsFile: one fixed key in the
	// private R2 bucket, replaced by each upload.
	RowsKey = "internal/layers/" + RowsFile

	uploadFile     = "upload.json" // what the last upload sent
	maxRowsDrop    = 0.10          // refuse an upload with this much fewer rows than the last one
	minFirstUpload = 1_000_000     // with no previous upload, refuse fewer rows than this
	minFeeds       = 5000          // refuse an upload from fewer working feeds than this
)

// layerHeader is the header of the layer files, in the geofeed (RFC 8805)
// layout: source is the feed URL for geofeed rows and whois:<registry> for
// whois rows. build-geo-data reads every layer in this format.
var layerHeader = []string{"prefix", "country", "region", "city", "source"}

// dropOtherOwner counts rows dropped because the most specific whois block
// covering them links a feed on another site.
const dropOtherOwner = "other_owner"

// dropOutsideASN counts rows dropped from a feed that only a domain probe found
// (source "domain"): they lie outside the announced prefixes of the domain's ASNs.
const dropOutsideASN = "outside_asn"

// ErrGate means the rows file shrank too much to upload. Retrying cannot help.
var ErrGate = errors.New("upload gate")

// ExportOutput is the export-rows task's result.
type ExportOutput struct {
	Feeds           int               `json:"feeds"` // working feeds read
	Rows            int               `json:"rows"`  // rows written
	Dropped         map[string]int    `json:"dropped,omitempty"`
	MissingFeeds    int               `json:"missing_feeds,omitempty"`    // working feeds with no stored copy
	UnreadableFeeds int               `json:"unreadable_feeds,omitempty"` // stored copies that could not be read; skipped
	BroadRows       int               `json:"broad_rows,omitempty"`       // rows kept although they span a block of another owner
	Bytes           int64             `json:"bytes"`
	Uploaded        bool              `json:"uploaded"`
	PreviousRows    int               `json:"previous_rows,omitempty"`
	Whois           WhoisUploadOutput `json:"whois"` // never fails the task: see WhoisUploadOutput.Skipped
	DurationSeconds float64           `json:"duration_seconds"`
}

// owners indexes the whois blocks that point to feeds, by the feed's site:
// one operator often publishes several feed URLs. IRR route objects are left
// out: they show who announces a prefix, not who holds it.
type owners struct {
	sites  map[string]int32
	blocks map[netip.Prefix][]int32 // site ids
	bits4  []int                    // prefix lengths present, longest first
	bits6  []int
	sorted []netip.Prefix // every block, by first address, for finding blocks inside a row
}

func loadOwners(path string) (*owners, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	o := &owners{sites: map[string]int32{}, blocks: map[netip.Prefix][]int32{}}
	seen4, seen6 := map[int]bool{}, map[int]bool{}
	r := csv.NewReader(bufio.NewReader(file))
	r.ReuseRecord = true
	for line := 0; ; line++ {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		if line == 0 || len(rec) != 3 || rec[0] == "irr" {
			continue
		}
		prefix, err := netip.ParsePrefix(rec[1])
		if err != nil {
			continue
		}
		site := feedSite(rec[2])
		id, ok := o.sites[site]
		if !ok {
			id = int32(len(o.sites))
			o.sites[site] = id
		}
		if !slices.Contains(o.blocks[prefix], id) {
			o.blocks[prefix] = append(o.blocks[prefix], id)
		}
		if prefix.Addr().Is4() {
			seen4[prefix.Bits()] = true
		} else {
			seen6[prefix.Bits()] = true
		}
	}
	for bits := range seen4 {
		o.bits4 = append(o.bits4, bits)
	}
	for bits := range seen6 {
		o.bits6 = append(o.bits6, bits)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(o.bits4)))
	sort.Sort(sort.Reverse(sort.IntSlice(o.bits6)))
	o.sorted = make([]netip.Prefix, 0, len(o.blocks))
	for prefix := range o.blocks {
		o.sorted = append(o.sorted, prefix)
	}
	slices.SortFunc(o.sorted, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	return o, nil
}

// sharedHosts serve feeds for many unrelated operators, so the host alone
// does not name an owner. For true, the owner is the first path segment (a
// GitHub user, a bucket); for false, it is the file itself (feed hosting
// services). Hand-kept: add a host when many operators turn out to share it.
var sharedHosts = map[string]bool{
	"raw.githubusercontent.com":  true,
	"gist.githubusercontent.com": true,
	"github.com":                 true,
	"gitlab.com":                 true,
	"bitbucket.org":              true,
	"storage.googleapis.com":     true,
	"s3.amazonaws.com":           true,
	"docs.google.com":            false,
	"drive.google.com":           false,
	"geofeed.info":               false,
	"geofeed.cc":                 false,
	"api.ipbnb.com":              false,
	"api.ipv4center.com":         false,
	"geofeeds.packetvis.com":     false,
	"opengeofeed.org":            false,
}

// feedSite names the owner of a feed: the registered domain of its host
// (api.cloudflare.com -> cloudflare.com), the host itself when it has none
// (an IP address), or, on a shared host, the user or the file.
func feedSite(feedURL string) string {
	parsed, err := url.Parse(feedURL)
	if err != nil {
		return feedURL
	}
	host := strings.ToLower(parsed.Hostname())
	if _, err := netip.ParseAddr(host); err == nil {
		return host
	}
	byUser, shared := sharedHosts[host]
	site, err := publicsuffix.EffectiveTLDPlusOne(host)
	switch {
	case shared && !byUser:
		return host + parsed.EscapedPath()
	case shared || err != nil && strings.Contains(host, "."):
		// A shared host, or a host that is itself a public suffix (s3.us-east-1.amazonaws.com).
		first, _, _ := strings.Cut(strings.TrimPrefix(parsed.EscapedPath(), "/"), "/")
		return host + "/" + strings.ToLower(first)
	case err != nil:
		return host
	}
	return site
}

// covering returns the sites of the most specific block that contains p.
func (o *owners) covering(p netip.Prefix) []int32 {
	lengths := o.bits6
	if p.Addr().Is4() {
		lengths = o.bits4
	}
	for _, bits := range lengths {
		if bits > p.Bits() {
			continue
		}
		if sites, ok := o.blocks[netip.PrefixFrom(p.Addr(), bits).Masked()]; ok {
			return sites
		}
	}
	return nil
}

// classify applies the ownership rule to one row of a feed on site, with one
// lookup. backed: the most specific whois block covering the row links a feed
// on site, so the registry's record backs it. allowed: the row is kept, which
// is every backed row, and every row that no block covers. A row is dropped
// when the most specific block covering it links only feeds on other sites.
// Operators link their feed from a few whois records but describe all their
// space in it, so a row outside every linking block stays.
func (o *owners) classify(site string, p netip.Prefix) (backed, allowed bool) {
	sites := o.covering(p)
	if sites == nil {
		return false, true
	}
	id, ok := o.sites[site]
	backed = ok && slices.Contains(sites, id)
	return backed, backed
}

// allows reports whether the row is kept (see classify).
func (o *owners) allows(site string, p netip.Prefix) bool {
	_, allowed := o.classify(site, p)
	return allowed
}

// spansOther reports whether p contains a more specific block that links no
// feed on site. Such a row is kept; in the MMDB the more specific rows of
// that block's owner win wherever they exist.
func (o *owners) spansOther(site string, p netip.Prefix) bool {
	id, known := o.sites[site]
	first, last := p.Masked().Addr(), rirstats.LastAddr(p)
	i := sort.Search(len(o.sorted), func(i int) bool { return o.sorted[i].Addr().Compare(first) >= 0 })
	for ; i < len(o.sorted) && o.sorted[i].Addr().Compare(last) <= 0; i++ {
		inner := o.sorted[i]
		if inner.Bits() > p.Bits() && !(known && slices.Contains(o.blocks[inner], id)) {
			return true
		}
	}
	return false
}

// CheckUpload fails when the R2 setting is missing or invalid, so a run can
// stop before the downloads instead of after them.
func (p *Pipeline) CheckUpload() error {
	_, err := publish.NewR2(p.R2)
	return err
}

// ExportRows writes RowsFile from the stored copies of the working feeds (the
// feeds that public.geofeed_urls says worked in the last 30 days) and, if upload is
// set, puts it at RowsKey in R2. It also uploads the whois geoloc rows that
// discover wrote (WhoisRowsFile) to WhoisRowsKey. An upload with more than 10% fewer rows than
// the last one is refused with ErrGate, unless force is set (for a manual run
// that accepts a real drop).
func (p *Pipeline) ExportRows(ctx context.Context, upload, force bool) (ExportOutput, error) {
	started := time.Now()
	out := ExportOutput{Dropped: map[string]int{}}
	var r2 *publish.R2
	if upload {
		var err error
		if r2, err = publish.NewR2(p.R2); err != nil {
			return out, err
		}
	}
	idx, err := loadOwners(filepath.Join(p.dir(), BlocksFile))
	if err != nil {
		return out, fmt.Errorf("read blocks (run discover first): %w", err)
	}
	store, err := p.openStore()
	if err != nil {
		return out, err
	}
	urlRows, err := store.Load(ctx)
	if err != nil {
		return out, fmt.Errorf("read public.geofeed_urls: %w", err)
	}
	// The announced prefixes probe-domains saved; export-rows never queries Postgres.
	asnPrefixes, err := loadDomainPrefixes(filepath.Join(p.dir(), DomainPrefixesFile))
	if err != nil {
		return out, err
	}
	now := p.clock().UTC()
	var urls []string
	domainOnly := map[string]bool{} // feeds that only a domain probe found
	for i := range urlRows {
		r := &urlRows[i]
		if r.working(now) {
			urls = append(urls, r.URL)
			domainOnly[r.URL] = len(r.Sources) == 1 && r.Sources[0] == sourceDomain
		}
	}
	sort.Strings(urls)
	p.log("export: %d working feeds, %d whois blocks", len(urls), len(idx.blocks))

	outDir := filepath.Join(p.dir(), OutDir)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return out, err
	}
	path := filepath.Join(outDir, RowsFile)
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return out, err
	}
	defer os.Remove(tmp)
	defer file.Close()
	gz := gzip.NewWriter(file)
	w := csv.NewWriter(gz)
	w.Write(layerHeader)
	// The rows a whois block backs go last, because for a prefix that several
	// feeds list, the build keeps the last row. They wait in a second file
	// and are copied after the others.
	backedFile, err := os.CreateTemp(outDir, "backed-*.csv.tmp")
	if err != nil {
		return out, err
	}
	defer os.Remove(backedFile.Name())
	defer backedFile.Close()
	backedW := csv.NewWriter(backedFile)
	usable := make([]string, 0, len(urls))
	for _, u := range urls {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		path := feedPath(filepath.Join(p.dir(), FeedsDir), u)
		// The copy is read once to check it, so a damaged one adds no rows.
		err := readStoredFeed(path, func([]string, netip.Prefix) {})
		if errors.Is(err, os.ErrNotExist) {
			out.MissingFeeds++
			continue
		}
		if err != nil {
			// One bad copy must not stop the export; the next check rewrites it.
			p.log("export: skip %s: %v", filepath.Base(path), err)
			out.UnreadableFeeds++
			continue
		}
		usable = append(usable, u)
	}
	out.Feeds = len(usable)
	for _, u := range usable {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		// A feed that only a domain probe found keeps just the rows inside its
		// ASNs' announced prefixes; with none on record, it keeps none.
		var inside *prefixSet
		if domainOnly[u] {
			if inside = asnPrefixes[u]; inside == nil {
				inside = &prefixSet{}
			}
		}
		rows, err := p.exportFeed(w, backedW, idx, inside, u, feedPath(filepath.Join(p.dir(), FeedsDir), u), &out)
		if err != nil {
			return out, err // it was readable a moment ago
		}
		out.Rows += rows
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return out, err
	}
	backedW.Flush()
	if err := backedW.Error(); err != nil {
		return out, err
	}
	if _, err := backedFile.Seek(0, io.SeekStart); err != nil {
		return out, err
	}
	if _, err := io.Copy(gz, backedFile); err != nil {
		return out, err
	}
	if err := gz.Close(); err != nil {
		return out, err
	}
	if err := file.Close(); err != nil {
		return out, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return out, err
	}
	if info, err := os.Stat(path); err == nil {
		out.Bytes = info.Size()
	}
	if len(out.Dropped) == 0 {
		out.Dropped = nil
	}
	p.log("export: %d rows from %d feeds, dropped %v, %d broad rows kept, %d feeds missing, %d unreadable, %d bytes",
		out.Rows, out.Feeds, out.Dropped, out.BroadRows, out.MissingFeeds, out.UnreadableFeeds, out.Bytes)

	// The whois geoloc rows are separate: whatever goes wrong with them leaves the geofeed rows alone.
	var whoisUpload uploadFunc
	if upload {
		whoisUpload = r2.Upload
	}
	out.Whois = p.uploadWhois(ctx, whoisUpload, force)
	if err := ctx.Err(); err != nil {
		return out, err
	}

	if upload {
		if err := p.uploadRows(ctx, r2, path, force, &out); err != nil {
			return out, err
		}
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	return out, nil
}

// exportFeed copies the rows of one stored feed that pass the ownership
// check: rows a whois block backs go to backedW, the others to w. Drops and
// broad rows are counted from the rows that are not backed. If inside is not
// nil, rows outside it are dropped first (dropOutsideASN).
func (p *Pipeline) exportFeed(w, backedW *csv.Writer, idx *owners, inside *prefixSet, feed, path string, out *ExportOutput) (int, error) {
	site, rows, dropped, outside := feedSite(feed), 0, 0, 0
	err := readStoredFeed(path, func(rec []string, prefix netip.Prefix) {
		if inside != nil && !inside.contains(prefix) {
			outside++
			return
		}
		backed, allowed := idx.classify(site, prefix)
		row := []string{rec[0], rec[1], rec[2], rec[3], feed}
		switch {
		case backed:
			backedW.Write(row)
		case !allowed:
			dropped++
			return
		default:
			if idx.spansOther(site, prefix) {
				out.BroadRows++
			}
			w.Write(row)
		}
		rows++
	})
	if dropped > 0 {
		out.Dropped[dropOtherOwner] += dropped
	}
	if outside > 0 {
		out.Dropped[dropOutsideASN] += outside
	}
	return rows, err
}

// readStoredFeed calls fn for each row of a stored feed copy.
func readStoredFeed(path string, fn func(rec []string, prefix netip.Prefix)) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	r := csv.NewReader(bufio.NewReader(file))
	r.Comment = '#' // the header line
	r.FieldsPerRecord = 4
	r.ReuseRecord = true
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		prefix, err := netip.ParsePrefix(rec[0])
		if err != nil {
			return err
		}
		fn(rec, prefix)
	}
}

type uploaded struct {
	Date string `json:"date"`
	Rows int    `json:"rows"`
}

// uploadRows checks the gate (unless force), uploads the rows file, and
// records the upload.
func (p *Pipeline) uploadRows(ctx context.Context, r2 *publish.R2, path string, force bool, out *ExportOutput) error {
	statePath := filepath.Join(p.dir(), uploadFile)
	var last uploaded
	data, err := os.ReadFile(statePath)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &last); err != nil && !force {
			return fmt.Errorf("%w: %s is damaged (%v); run once with force_upload", ErrGate, uploadFile, err)
		}
	case !errors.Is(err, os.ErrNotExist) && !force:
		return fmt.Errorf("%w: %v", ErrGate, err)
	}
	out.PreviousRows = last.Rows
	switch {
	case force:
		p.log("export: gate skipped (force_upload)")
	case out.Feeds < minFeeds:
		return fmt.Errorf("%w: %d working feeds, fewer than %d", ErrGate, out.Feeds, minFeeds)
	case last.Rows > 0 && float64(out.Rows) < float64(last.Rows)*(1-maxRowsDrop):
		return fmt.Errorf("%w: %d rows, down from %d on %s", ErrGate, out.Rows, last.Rows, last.Date)
	case last.Rows == 0 && out.Rows < minFirstUpload:
		return fmt.Errorf("%w: %d rows, fewer than %d", ErrGate, out.Rows, minFirstUpload)
	}
	location, err := r2.Upload(ctx, RowsKey, path)
	if err != nil {
		return err
	}
	p.log("export: uploaded %s", location)
	out.Uploaded = true
	data, _ = json.Marshal(uploaded{Date: p.clock().UTC().Format(time.RFC3339), Rows: out.Rows})
	return writeFileAtomic(statePath, data)
}
