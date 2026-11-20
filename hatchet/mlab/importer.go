package mlab

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"openipdata.org/hatchet/ipdata"
)

const (
	// maxRows caps the data lines read (good or bad): a file of garbage or a decompression bomb stops here.
	maxRows = 200_000_000
	// maxPairs caps the pairs held in memory (12 bytes each: 480 MB; growing the slice peaks near 1.1 GB).
	maxPairs = 40_000_000
	// minValidRows is the least a plausible export has (about 8.5M a week); less is a wrong file.
	minValidRows = 100_000
	// maxTraces caps one row's trace count: a count above it is a broken or hostile value.
	maxTraces = 1_000_000

	maxLine = 4096 // a longer line is a bad row
)

// ImportOutput counts an import (counts only: no IP, no row).
type ImportOutput struct {
	File            string  `json:"file"`
	Bytes           int64   `json:"bytes"`
	Rows            int     `json:"rows"`  // data lines read
	Valid           int     `json:"valid"` // rows kept
	BadColumns      int     `json:"bad_columns"`
	BadTooLong      int     `json:"bad_too_long"`
	BadClient       int     `json:"bad_client"`
	BadHop          int     `json:"bad_hop"`
	BadFromEnd      int     `json:"bad_from_end"` // not 0, 1 or 2
	BadTraces       int     `json:"bad_traces"`   // not a number of at least 1
	NotPublicClient int     `json:"not_public_client"`
	NotPublicHop    int     `json:"not_public_hop"`
	Pairs           int     `json:"pairs"`   // distinct (client /24, hop) pairs, written to the pairs file
	Clients         int     `json:"clients"` // distinct client /24s
	Hops            int     `json:"hops"`    // distinct hop IPs
	FirstDay        string  `json:"first_day,omitempty"`
	LastDay         string  `json:"last_day,omitempty"`
	Days            int     `json:"days"` // span from the first to the last first_day, both counted
	PairsBytes      int64   `json:"pairs_bytes"`
	Seconds         float64 `json:"duration_seconds"`
}

// Bad is the number of rows that were not kept.
func (o ImportOutput) Bad() int {
	return o.BadColumns + o.BadTooLong + o.BadClient + o.BadHop + o.BadFromEnd + o.BadTraces + o.NotPublicClient + o.NotPublicHop
}

// inboxFile resolves the workflow's file input to a path in the inbox. name is a base name; empty
// means the newest *.csv or *.csv.gz there. A name with a path separator or a dot-dot is refused,
// and so is anything but a regular file (no symbolic link).
func (p *Pipeline) inboxFile(name string) (string, error) {
	path, found, err := findInput(p.inbox(), name)
	if err == nil && !found {
		return "", permanent("the inbox %s has no .csv or .csv.gz file", p.inbox())
	}
	return path, err
}

// findInput is inboxFile for any directory. With an empty name and no CSV in dir (or no dir) it
// returns found false and no error; the caller decides what that means.
func findInput(dir, name string) (path string, found bool, err error) {
	if name != "" {
		if name != filepath.Base(name) || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
			return "", false, permanent("file must be a base name inside the inbox, not a path")
		}
		if !isCSVName(name) {
			return "", false, permanent("file must end in .csv or .csv.gz")
		}
		path := filepath.Join(dir, name)
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			return "", false, permanent("no such file in the inbox: %s", name)
		}
		return path, true, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return "", false, err
	}
	type cand struct {
		name string
		mod  time.Time
	}
	var cands []cand
	for _, e := range entries {
		if !isCSVName(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		cands = append(cands, cand{e.Name(), fi.ModTime()})
	}
	if len(cands) == 0 {
		return "", false, nil
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].mod.Equal(cands[j].mod) {
			return cands[i].mod.After(cands[j].mod)
		}
		return cands[i].name > cands[j].name
	})
	return filepath.Join(dir, cands[0].name), true, nil
}

func isCSVName(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".csv") || strings.HasSuffix(n, ".csv.gz")
}

// Import is the import task: it reads the CSV named by name in the inbox (see inboxFile) and
// replaces the pairs file.
func (p *Pipeline) Import(ctx context.Context, name string) (ImportOutput, error) {
	path, err := p.inboxFile(name)
	if err != nil {
		return ImportOutput{}, err
	}
	return p.ImportFile(ctx, path)
}

// ImportFile imports the CSV at path (any local path: the run-mlab command uses it).
func (p *Pipeline) ImportFile(ctx context.Context, path string) (ImportOutput, error) {
	started := time.Now()
	var out ImportOutput
	out.File = filepath.Base(path)
	f, err := os.Open(path)
	if err != nil {
		return out, permanent("cannot open the input file")
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil {
		out.Bytes = fi.Size()
	}
	min := p.minRows
	if min == 0 {
		min = minValidRows
	}
	limit := p.maxPairs
	if limit == 0 {
		limit = maxPairs
	}
	pairs, err := readCSV(ctx, f, &out, min, limit)
	if err != nil {
		return out, err
	}
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return out, err
	}
	out.Pairs = len(pairs)
	out.Clients = countClients(pairs)
	out.Hops = len(distinctHops(pairs))
	if out.PairsBytes, err = writePairs(p.pairsPath(), pairs); err != nil {
		return out, fmt.Errorf("write %s: %w", pairsFile, err)
	}
	out.Seconds = time.Since(started).Round(time.Second).Seconds()
	p.log("import: %d rows (%d bad, %d kept), %d pairs, %d client /24s, %d hops, %d days; %s is %d bytes",
		out.Rows, out.Bad(), out.Valid, out.Pairs, out.Clients, out.Hops, out.Days, pairsFile, out.PairsBytes)
	return out, nil
}

func countClients(pairs []pair) int {
	n := 0
	for i, p := range pairs {
		if i == 0 || p.client() != pairs[i-1].client() {
			n++
		}
	}
	return n
}

// readCSV streams the CSV (plain or gzip, found by its first bytes), counts into out and returns the
// merged pairs. The header names the columns; the rows are untrusted.
func readCSV(ctx context.Context, src io.Reader, out *ImportOutput, minValid, pairLimit int) ([]pair, error) {
	r, col, closeFn, err := openCSV(src)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	cClient, ok1 := col["client24"]
	cHop, ok2 := col["hop_ip"]
	cFrom, ok3 := col["from_end"]
	cTraces, ok4 := col["traces"]
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil, permanent("the header must have the columns client24, hop_ip, from_end and traces")
	}
	cDay, haveDay := col["first_day"]
	need := max(cClient, cHop, cFrom, cTraces) + 1
	if haveDay {
		need = max(need, cDay+1)
	}

	var pairs []pair
	var firstDay, lastDay time.Time
	fields := make([][]byte, 0, 16)
	for {
		line, err := readLine(r)
		if err == io.EOF {
			break
		}
		if errors.Is(err, errLineTooLong) {
			out.Rows++
			out.BadTooLong++
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the input file: %w", err)
		}
		if len(line) == 0 {
			continue
		}
		if out.Rows++; out.Rows > maxRows {
			return nil, permanent("more than %d rows", maxRows)
		}
		if out.Rows&0xFFFF == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if len(line) > maxLine {
			out.BadTooLong++
			continue
		}
		fields = fields[:0]
		for rest := line; ; {
			i := bytes.IndexByte(rest, ',')
			if i < 0 {
				fields = append(fields, trimField(rest))
				break
			}
			fields = append(fields, trimField(rest[:i]))
			rest = rest[i+1:]
			if len(fields) == cap(fields) {
				break
			}
		}
		if len(fields) < need {
			out.BadColumns++
			continue
		}
		client, ok := parseClient(fields[cClient])
		if !ok {
			out.BadClient++
			continue
		}
		hop, err := netip.ParseAddr(string(fields[cHop]))
		if err != nil || !hop.Is4() {
			out.BadHop++
			continue
		}
		from, ok := parseUint(fields[cFrom])
		if !ok || from > 2 {
			out.BadFromEnd++
			continue
		}
		traces, ok := parseUint(fields[cTraces])
		if !ok || traces < 1 {
			out.BadTraces++
			continue
		}
		if !ipdata.PublicV4(addrOf(client<<8 | 1)) {
			out.NotPublicClient++
			continue
		}
		if !ipdata.PublicV4(hop) {
			out.NotPublicHop++
			continue
		}
		traces = min(traces, maxTraces)
		if haveDay && len(fields[cDay]) >= 10 {
			if d, err := time.Parse("2006-01-02", string(fields[cDay][:10])); err == nil {
				if firstDay.IsZero() || d.Before(firstDay) {
					firstDay = d
				}
				if d.After(lastDay) {
					lastDay = d
				}
			}
		}
		out.Valid++
		if len(pairs) >= pairLimit {
			pairs = compactPairs(pairs)
			if len(pairs) > pairLimit/4*3 {
				return nil, permanent("more than %d distinct (client /24, hop) pairs", pairLimit/4*3)
			}
		}
		pairs = append(pairs, makePair(client, uint8(from), u32Of(hop), uint32(traces)))
	}
	if out.Valid < minValid {
		return nil, permanent("only %d valid rows of %d, fewer than %d: is this the right file?", out.Valid, out.Rows, minValid)
	}
	pairs = compactPairs(pairs)
	if !firstDay.IsZero() {
		out.FirstDay, out.LastDay = firstDay.Format("2006-01-02"), lastDay.Format("2006-01-02")
		out.Days = int(lastDay.Sub(firstDay).Hours()/24) + 1
	}
	return pairs, nil
}

// openCSV wraps src (plain or gzip, found by its first bytes), reads the header line and returns
// the reader at the first data line with the lower-cased column names mapped to their positions.
// closeFn must be called when the reader is done.
func openCSV(src io.Reader) (r *bufio.Reader, col map[string]int, closeFn func(), err error) {
	br := bufio.NewReaderSize(src, 1<<20)
	var rd io.Reader = br
	closeFn = func() {}
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, nil, nil, permanent("the input file is not a valid gzip file")
		}
		closeFn = func() { gz.Close() }
		rd = gz
	}
	r = bufio.NewReaderSize(rd, 64<<10)
	header, err := readLine(r)
	if err != nil {
		closeFn()
		return nil, nil, nil, permanent("the input file has no header line")
	}
	header = bytes.TrimPrefix(header, []byte("\xef\xbb\xbf"))
	col = map[string]int{}
	for i, name := range bytes.Split(header, []byte{','}) {
		col[strings.ToLower(strings.Trim(string(name), " \t\""))] = i
	}
	return r, col, closeFn, nil
}

// trimField drops spaces and quotes around a field.
func trimField(b []byte) []byte { return bytes.Trim(b, " \t\"") }
