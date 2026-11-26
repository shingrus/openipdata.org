package ping

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"openipdata.org/hatchet/publish"
)

// mergeStats counts a merge (counts only).
type mergeStats struct {
	Addresses int            // lines with a valid address, answered or not
	Ignored   int            // lines that were not valid, duplicates and out-of-order lines
	Rows      int            // rows written
	Short     int            // /24s left out because fewer than minAnswers regions answered
	Answers   map[string]int // answers per region
}

// regionStream reads the lines of one region result in file order: "a.b.c.d : 12.3" or
// "a.b.c.d : -", sorted by address.
type regionStream struct {
	name  string
	idx   int
	f     *os.File
	gz    *gzip.Reader
	r     *bufio.Reader
	last  int64
	stats *mergeStats

	addr uint32 // the current answered line
	rtt  float64
}

func openStream(path, name string, idx int, stats *mergeStats) (*regionStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open a region result")
	}
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		f.Close()
		return nil, errors.New("a region result is not a valid gzip file")
	}
	return &regionStream{name: name, idx: idx, f: f, gz: gz, r: bufio.NewReaderSize(gz, 8192), last: -1, stats: stats}, nil
}

func (s *regionStream) close() { s.gz.Close(); s.f.Close() }

// next moves to the next answered line; false at the end of the file.
func (s *regionStream) next() (bool, error) {
	for {
		line, err := readLine(s.r)
		if err == io.EOF {
			return false, nil
		}
		if errors.Is(err, errLineTooLong) {
			s.stats.Ignored++
			continue
		}
		if err != nil {
			return false, errors.New("a region result is damaged or cut short")
		}
		text := string(line)
		if strings.Contains(text, "duplicate") {
			s.stats.Ignored++
			continue
		}
		parts := strings.Fields(text)
		if len(parts) != 3 || parts[1] != ":" {
			s.stats.Ignored++
			continue
		}
		a, err := netip.ParseAddr(parts[0])
		if err != nil || !a.Is4() {
			s.stats.Ignored++
			continue
		}
		b := a.As4()
		v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		if int64(v) < s.last {
			s.stats.Ignored++
			continue
		}
		s.last = int64(v)
		s.stats.Addresses++
		if parts[2] == "-" {
			continue
		}
		rtt, err := strconv.ParseFloat(parts[2], 64)
		if err != nil || math.IsNaN(rtt) || math.IsInf(rtt, 0) || rtt < 0 {
			s.stats.Ignored++
			continue
		}
		s.stats.Answers[s.name]++
		s.addr, s.rtt = v, rtt
		return true, nil
	}
}

type streamHeap []*regionStream

func (h streamHeap) Len() int           { return len(h) }
func (h streamHeap) Less(i, j int) bool { return h[i].addr < h[j].addr }
func (h streamHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *streamHeap) Push(x any)        { *h = append(*h, x.(*regionStream)) }
func (h *streamHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func fmtRTT(v float64) string { return strconv.FormatFloat(math.Max(v, 0.1), 'f', 1, 64) }

// mergeRegions merges the sorted region files (paths[i] is the file of regions[i], "" when the
// region has none: its column stays empty) into the CSV on w: the header "prefix" and one column per
// region, then one row per /24 (a.b.c.0/24) with the lowest time of each region (one decimal, at
// least 0.1) or an empty cell, only for /24s with at least minAnswers answers, ascending. The
// files are streamed, never held in memory.
func mergeRegions(ctx context.Context, paths []string, w io.Writer) (mergeStats, error) {
	stats := mergeStats{Answers: map[string]int{}}
	var h streamHeap
	var open []*regionStream
	defer func() {
		for _, s := range open {
			s.close()
		}
	}()
	for i, rg := range regions {
		stats.Answers[rg.Name] = 0
		if i >= len(paths) || paths[i] == "" {
			continue
		}
		s, err := openStream(paths[i], rg.Name, i, &stats)
		if err != nil {
			return stats, err
		}
		open = append(open, s)
		ok, err := s.next()
		if err != nil {
			return stats, err
		}
		if ok {
			h = append(h, s)
		}
	}
	heap.Init(&h)

	bw := bufio.NewWriterSize(w, 1<<20)
	var header strings.Builder
	header.WriteString("prefix")
	for _, rg := range regions {
		header.WriteString("," + rg.Site)
	}
	header.WriteByte('\n')
	if _, err := bw.WriteString(header.String()); err != nil {
		return stats, err
	}
	cells := make([]float64, len(regions))
	have := make([]bool, len(regions))
	for n := 0; h.Len() > 0; n++ {
		if n&0xFFFF == 0 {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
		}
		block := h[0].addr >> 8
		for i := range have {
			have[i] = false
		}
		for h.Len() > 0 && h[0].addr>>8 == block {
			s := h[0]
			if !have[s.idx] || s.rtt < cells[s.idx] {
				cells[s.idx], have[s.idx] = s.rtt, true
			}
			ok, err := s.next()
			if err != nil {
				return stats, err
			}
			if ok {
				heap.Fix(&h, 0)
			} else {
				heap.Pop(&h)
			}
		}
		answers := 0
		for _, b := range have {
			if b {
				answers++
			}
		}
		if answers < minAnswers {
			stats.Short++
			continue
		}
		var row strings.Builder
		a := block << 8
		fmt.Fprintf(&row, "%d.%d.%d.0/24", a>>24, a>>16&255, a>>8&255)
		for i := range regions {
			row.WriteByte(',')
			if have[i] {
				row.WriteString(fmtRTT(cells[i]))
			}
		}
		row.WriteByte('\n')
		if _, err := bw.WriteString(row.String()); err != nil {
			return stats, err
		}
		stats.Rows++
	}
	return stats, bw.Flush()
}

// PublishOptions are the inputs of Publish.
type PublishOptions struct {
	SkipUpload  bool
	ForceUpload bool
}

// PublishOutput counts the publish task (counts only).
type PublishOutput struct {
	Skipped          string         `json:"skipped,omitempty"`
	Regions          int            `json:"regions"` // result files merged
	Addresses        int            `json:"addresses"`
	Ignored          int            `json:"ignored_lines"`
	Rows             int            `json:"rows"`
	ShortRows        int            `json:"short_rows"` // /24s with fewer than 3 answers, left out
	AnswersPerRegion map[string]int `json:"answers_per_region,omitempty"`
	Sites            int            `json:"sites"`
	Valid            int            `json:"valid"`
	Bad              int            `json:"bad"`
	Bytes            int64          `json:"bytes"`
	Upload           string         `json:"upload,omitempty"` // uploaded, unchanged or skipped
	RunObjectsGone   int            `json:"run_objects_deleted"`
	Seconds          float64        `json:"duration_seconds"`
}

// Publish is the publish task: it downloads the region results from R2, merges them into
// hosting-rtt.csv.gz, checks the file, uploads it to RTTKey (no Refresh: the same run gives the
// same file, and an object that already holds it keeps its date) and empties the objects of the
// run. Then it pings the health check.
func (p *Pipeline) Publish(ctx context.Context, opts PublishOptions) (PublishOutput, error) {
	started := time.Now()
	var out PublishOutput
	if !p.Enabled() {
		out.Skipped = "no GCP key"
		p.log("publish: skipped (no GCP key)")
		return out, nil
	}
	store, err := p.objects()
	if err != nil {
		return out, err
	}
	dir := p.outDir()
	if err := os.RemoveAll(dir); err != nil {
		return out, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return out, err
	}
	defer os.RemoveAll(dir)

	paths := make([]string, len(regions))
	for i, rg := range regions {
		path := filepath.Join(dir, rg.Name+".txt.gz")
		if _, err := store.Download(ctx, ResultKey(rg.Name), path); err != nil {
			if errors.Is(err, publish.ErrNotFound) {
				continue
			}
			return out, fmt.Errorf("download the result of a region: %w", err)
		}
		paths[i] = path
		out.Regions++
	}
	minRegions := p.minRegions
	if minRegions == 0 {
		minRegions = minRegionsOK
	}
	if out.Regions < minRegions {
		return out, fmt.Errorf("only %d of %d regions have a result", out.Regions, len(regions))
	}

	merged := p.mergedPath()
	tmp := merged + ".tmp"
	if err := os.MkdirAll(filepath.Dir(merged), 0o750); err != nil {
		return out, err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return out, err
	}
	gw := gzip.NewWriter(f)
	stats, err := mergeRegions(ctx, paths, gw)
	if err == nil {
		err = gw.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return out, fmt.Errorf("merge the results: %w", err)
	}
	if err := os.Rename(tmp, merged); err != nil {
		return out, err
	}
	out.Addresses, out.Ignored, out.Rows, out.ShortRows, out.AnswersPerRegion = stats.Addresses, stats.Ignored, stats.Rows, stats.Short, stats.Answers
	p.log("publish: %d regions merged, %d addresses, %d rows, %d with fewer than %d answers", out.Regions, out.Addresses, out.Rows, out.ShortRows, minAnswers)

	check, err := p.checkFile(ctx, merged, p.minValid)
	out.Sites, out.Valid, out.Bad, out.Bytes = check.Sites, check.Valid, check.Bad(), check.Bytes
	if err != nil {
		return out, err
	}
	if opts.SkipUpload {
		out.Upload = "skipped"
	} else {
		_, status, err := store.Put(ctx, RTTKey, merged, publish.PutOptions{Force: opts.ForceUpload, Refresh: false})
		if err != nil {
			return out, err
		}
		out.Upload = string(status)
		if n, err := store.DeletePrefix(ctx, RunPrefix); err != nil {
			p.log("publish: the objects of the run could not be deleted (the next run deletes them)")
		} else {
			out.RunObjectsGone = n
		}
		if err := publish.Ping(ctx, p.HCPing); err != nil {
			p.log("publish: %v", err) // the upload succeeded; a missed ping only delays the alert
		}
	}
	out.Seconds = time.Since(started).Round(time.Second).Seconds()
	p.log("publish: %d rows (%d valid, %d bad), %d sites, upload %s", check.Rows, out.Valid, out.Bad, out.Sites, out.Upload)
	return out, nil
}
