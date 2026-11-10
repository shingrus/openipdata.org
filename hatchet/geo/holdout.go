package geo

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The holdout test: a fixed tenth of the geofeeds (chosen by a hash of the
// feed URL, so the same feeds every run) is left out of a second build. The
// feeds are the best data we have, so how well that build, which only has the
// lower layers and the other feeds, guesses the held-out rows measures how
// good those layers are where nobody told us the answer.
const (
	holdoutBuckets = 10
	holdoutBucket  = 0 // feeds whose hash falls in this bucket are held out
)

// heldOut reports whether the feed is in the holdout bucket.
func heldOut(feedURL string) bool {
	sum := sha256.Sum256([]byte(feedURL))
	return binary.BigEndian.Uint64(sum[:8])%holdoutBuckets == holdoutBucket
}

// holdoutStats counts what splitFeeds did.
type holdoutStats struct {
	Feeds, HeldFeeds int
	KeptRows         int
	HeldRows         int // rows of the held-out feeds
	TruthRows        int // held-out IPv4 rows no kept row overlaps
}

// block is a range of IPv4 addresses.
type block struct{ lo, hi uint32 }

// parseV4 reads an IPv4 prefix or single address; ok is false for anything else.
func parseV4(text string) (netip.Prefix, bool) {
	text = strings.TrimSpace(text)
	p, err := netip.ParsePrefix(text)
	if err != nil {
		addr, aerr := netip.ParseAddr(text)
		if aerr != nil {
			return netip.Prefix{}, false
		}
		p = netip.PrefixFrom(addr, addr.BitLen())
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, false
	}
	return p.Masked(), true
}

func prefixBlock(p netip.Prefix) block {
	a := p.Addr().As4()
	lo := binary.BigEndian.Uint32(a[:])
	return block{lo, uint32(uint64(lo) + 1<<(32-p.Bits()) - 1)}
}

// splitFeeds reads the geofeed rows layer (prefix,country,region,city,feed_url)
// and writes
//
//   - keptPath: the same file without the rows of the held-out feeds
//   - truthPath: prefix,country,city of the held-out IPv4 rows that no kept
//     row overlaps. Where a kept row covers or lies inside a held-out prefix,
//     the holdout build still knows its answer, and the score would measure
//     the overlap of two feeds, not our lower layers.
//
// Both are written through temporary files.
func splitFeeds(feedsPath, keptPath, truthPath string) (holdoutStats, error) {
	var stats holdoutStats
	in, err := os.Open(feedsPath)
	if err != nil {
		return stats, err
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return stats, err
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return stats, fmt.Errorf("read header: %w", err)
	}
	col := func(name string) (int, error) {
		if i := slices.Index(header, name); i >= 0 {
			return i, nil
		}
		return 0, fmt.Errorf("header %q lacks %s", strings.Join(header, ","), name)
	}
	prefixCol, err1 := col("prefix")
	countryCol, err2 := col("country")
	cityCol, err3 := col("city")
	urlCol, err4 := col("feed_url")
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return stats, err
	}

	keptTmp := keptPath + ".tmp"
	kept, err := os.Create(keptTmp)
	if err != nil {
		return stats, err
	}
	defer os.Remove(keptTmp)
	defer kept.Close()
	keptGz := gzip.NewWriter(kept)
	keptCSV := csv.NewWriter(keptGz)
	keptCSV.Write(header)

	type heldRow struct {
		block
		prefix, country, city string
	}
	var keptBlocks []block
	var held []heldRow
	feeds := map[string]bool{} // feed URL -> held out
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			var parseErr *csv.ParseError
			if errors.As(err, &parseErr) {
				continue // the build skips such a row as well
			}
			return stats, err
		}
		field := func(i int) string {
			if i < len(rec) {
				return rec[i]
			}
			return ""
		}
		feedURL := field(urlCol)
		isHeld, seen := feeds[feedURL]
		if !seen {
			isHeld = heldOut(feedURL)
			feeds[feedURL] = isHeld
		}
		p, isV4 := parseV4(field(prefixCol))
		if !isHeld {
			keptCSV.Write(rec)
			stats.KeptRows++
			if isV4 {
				keptBlocks = append(keptBlocks, prefixBlock(p))
			}
			continue
		}
		stats.HeldRows++
		if country := strings.ToUpper(strings.TrimSpace(field(countryCol))); isV4 && len(country) == 2 {
			held = append(held, heldRow{prefixBlock(p), p.String(), country, strings.TrimSpace(field(cityCol))})
		}
	}
	stats.Feeds = len(feeds)
	for _, isHeld := range feeds {
		if isHeld {
			stats.HeldFeeds++
		}
	}
	keptCSV.Flush()
	if err := errors.Join(keptCSV.Error(), keptGz.Close(), kept.Close()); err != nil {
		return stats, err
	}

	// The kept rows as sorted, merged blocks, to test overlaps by binary search.
	slices.SortFunc(keptBlocks, func(a, b block) int { return cmpU32(a.lo, b.lo) })
	merged := keptBlocks[:0]
	for _, b := range keptBlocks {
		if n := len(merged); n > 0 && uint64(b.lo) <= uint64(merged[n-1].hi)+1 {
			merged[n-1].hi = max(merged[n-1].hi, b.hi)
			continue
		}
		merged = append(merged, b)
	}
	overlaps := func(b block) bool {
		// The first merged block that ends at or after b starts; it overlaps if it starts by b's end.
		i, _ := slices.BinarySearchFunc(merged, b.lo, func(m block, lo uint32) int { return cmpU32(m.hi, lo) })
		return i < len(merged) && merged[i].lo <= b.hi
	}

	truthTmp := truthPath + ".tmp"
	truth, err := os.Create(truthTmp)
	if err != nil {
		return stats, err
	}
	defer os.Remove(truthTmp)
	defer truth.Close()
	truthCSV := csv.NewWriter(truth)
	truthCSV.Write([]string{"prefix", "country", "city"})
	for _, row := range held {
		if overlaps(row.block) {
			continue
		}
		truthCSV.Write([]string{row.prefix, row.country, row.city})
		stats.TruthRows++
	}
	truthCSV.Flush()
	if err := errors.Join(truthCSV.Error(), truth.Close()); err != nil {
		return stats, err
	}
	return stats, errors.Join(os.Rename(keptTmp, keptPath), os.Rename(truthTmp, truthPath))
}

func cmpU32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// prepareHoldout splits the geofeed rows and builds holdout.mmdb from the
// kept feeds with the same arguments as the real build. It returns the paths
// of the MMDB and the truth CSV, or empty paths and a status when there is
// nothing to score.
func (p *Pipeline) prepareHoldout(ctx context.Context, dir string) (mmdbPath, truthPath, status string, err error) {
	// The layer's file name matters to mmdbbuilder's -within-first flag.
	keptDir := filepath.Join(dir, "holdout")
	if err := os.MkdirAll(keptDir, 0o750); err != nil {
		return "", "", "", err
	}
	kept := filepath.Join(keptDir, FeedsLayer)
	truthPath = filepath.Join(dir, "holdout-truth.csv")
	stats, err := splitFeeds(filepath.Join(p.layersDir(), FeedsLayer), kept, truthPath)
	if err != nil {
		return "", "", "", fmt.Errorf("holdout split: %w", err)
	}
	summary := fmt.Sprintf("%d of %d feeds held out, %d of %d rows, %d scored", stats.HeldFeeds, stats.Feeds, stats.HeldRows, stats.HeldRows+stats.KeptRows, stats.TruthRows)
	if stats.KeptRows == 0 || stats.TruthRows == 0 {
		return "", "", "skipped: " + summary, nil
	}
	mmdbPath = filepath.Join(dir, "holdout.mmdb")
	if _, err := p.runBuilder(ctx, p.geoArgs(mmdbPath, kept)); err != nil {
		return "", "", "", fmt.Errorf("holdout build: %w", err)
	}
	return mmdbPath, truthPath, summary, nil
}
