// Package ping is the workflow import-hosting-ping. The owner makes the file hosting-rtt.csv.gz by
// hand about monthly in Google Cloud Shell: ping times from about 27 Google Cloud regions to the
// .1 address of every /24 of hosting networks. This workflow checks the file (it is untrusted
// input, read as a stream) and uploads it as it is to the private R2 bucket, where build-geo-data
// reads it and gives it to "mmdbbuilder geo -ping-country" (a speed-of-light check of the country).
//
// The file is a gzipped CSV. The header is "prefix" and then 3 to 64 site columns named
// "<CC>/<City>" (two upper-case ASCII letters, a slash, a city name of 1 to 64 bytes, valid UTF-8,
// no control character, no < > or comma), all distinct. A data row is an IPv4 /24 in masked CIDR
// form (a.b.c.0/24) and, per site, an empty cell (no answer) or a round-trip time in ms (a decimal
// above 0 and at most 2000). Rows are strictly ascending by address and have at least 3 answers.
package ping

import (
	"bufio"
	"bytes"
	"compress/gzip"
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
	"unicode"
	"unicode/utf8"

	"openipdata.org/hatchet/ipdata"
	"openipdata.org/hatchet/outputs"
	"openipdata.org/hatchet/publish"
)

const (
	// RTTFile is the name of the file (in the inbox and in the geo directory of build-geo-data) and
	// RTTKey its one fixed key in the private R2 bucket, replaced by each new export.
	RTTFile = "hosting-rtt.csv.gz"
	RTTKey  = "internal/ping/" + RTTFile

	inboxDir   = "inbox"
	fileSuffix = ".csv.gz"

	maxFileBytes = 1 << 30 // the compressed file
	maxPingRows  = 20_000_000
	minPingRows  = 100_000 // fewer valid rows: not the right file
	maxLine      = 4096    // a longer data line is a bad row
	maxHeader    = 64 << 10

	minSites    = 3
	maxSites    = 64
	maxSiteName = 64   // bytes of a city
	minAnswers  = 3    // answers a row needs
	maxRTTMs    = 2000 // ms
)

// ErrPermanent marks an error that no retry can fix (a bad input file, too many bad rows). The
// worker turns it into a non-retryable task error.
var ErrPermanent = errors.New("permanent")

func permanent(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPermanent, fmt.Sprintf(format, args...))
}

// Pipeline runs import-hosting-ping.
type Pipeline struct {
	DataDir  string
	R2       string // the setting of the private bucket
	Progress func(string)

	// Tests only.
	upload  uploadFunc
	minRows int
}

// uploadFunc is publish.R2.Put. Tests replace it.
type uploadFunc func(ctx context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error)

func (p *Pipeline) dir() string   { return filepath.Join(p.DataDir, "ping") }
func (p *Pipeline) inbox() string { return filepath.Join(p.dir(), inboxDir) }

func (p *Pipeline) log(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

// CheckUpload fails when the R2 setting is missing or invalid, so a run stops before the work.
func (p *Pipeline) CheckUpload() error {
	_, err := publish.NewR2(p.R2)
	return err
}

// Options are the inputs of Upload.
type Options struct {
	// Path is a local file (the run-ping command); it wins over the name given to Upload.
	Path        string
	SkipUpload  bool
	ForceUpload bool
}

// Output counts a run (counts only: no prefix, no city, no time).
type Output struct {
	File       string  `json:"file,omitempty"`
	Bytes      int64   `json:"bytes"`
	Rows       int     `json:"rows"`  // data lines read
	Valid      int     `json:"valid"` // rows that passed every check
	Sites      int     `json:"sites"`
	BadColumns int     `json:"bad_columns"` // wrong field count, or a line over 4 KB
	BadPrefix  int     `json:"bad_prefix"`
	NotPublic  int     `json:"not_public"`
	NotSorted  int     `json:"not_sorted"`
	BadRTT     int     `json:"bad_rtt"`
	FewSites   int     `json:"few_sites"`
	Upload     string  `json:"upload,omitempty"` // uploaded, unchanged (R2 has the same file; its date stays) or skipped
	Skipped    string  `json:"skipped,omitempty"`
	Seconds    float64 `json:"duration_seconds"`
}

// Bad is the number of rows that were not kept.
func (o Output) Bad() int {
	return o.BadColumns + o.BadPrefix + o.NotPublic + o.NotSorted + o.BadRTT + o.FewSites
}

// Upload is the upload task. name is a base name in the inbox (empty: the newest .csv.gz there);
// opts.Path wins. With no input file at all it succeeds with Skipped "no file" and does nothing.
func (p *Pipeline) Upload(ctx context.Context, name string, opts Options) (Output, error) {
	started := time.Now()
	var out Output
	path := opts.Path
	if path == "" {
		var found bool
		var err error
		if path, found, err = findInput(p.inbox(), name); err != nil {
			return out, err
		}
		if !found {
			out.Skipped = "no file"
			p.log("upload: skipped (no file in the inbox)")
			return out, nil
		}
	}
	out.File = filepath.Base(path)
	f, err := os.Open(path)
	if err != nil {
		return out, permanent("cannot open the input file")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return out, permanent("the input file is not a regular file")
	}
	out.Bytes = fi.Size()
	if out.Bytes > maxFileBytes {
		return out, permanent("the input file is larger than %d bytes", int64(maxFileBytes))
	}
	minValid := p.minRows
	if minValid == 0 {
		minValid = minPingRows
	}
	if err := p.check(ctx, f, &out); err != nil {
		return out, err
	}
	// Valid enough rows, and at most 1% of the rows bad: else this is not the right file.
	if out.Valid < minValid {
		return out, permanent("only %d valid rows of %d, fewer than %d: is this the right file?", out.Valid, out.Rows, minValid)
	}
	if out.Bad()*100 > out.Rows {
		return out, permanent("%d of %d rows are bad, more than 1%%: is this the right file?", out.Bad(), out.Rows)
	}
	if opts.SkipUpload {
		out.Upload = "skipped"
	} else {
		// No refresh: the same export gives the same file, and an object that already holds it keeps
		// its date, so the age limit of build-geo-data counts from the last new export.
		put := p.upload
		if put == nil {
			r2, err := publish.NewR2(p.R2)
			if err != nil {
				return out, err
			}
			put = r2.Put
		}
		_, status, err := put(ctx, RTTKey, path, publish.PutOptions{Force: opts.ForceUpload, Refresh: false})
		if err != nil {
			return out, err
		}
		out.Upload = string(status)
	}
	out.Seconds = time.Since(started).Round(time.Second).Seconds()
	p.log("upload: %d rows (%d bad, %d valid), %d sites, upload %s", out.Rows, out.Bad(), out.Valid, out.Sites, out.Upload)
	return out, nil
}

// isPingName says whether a file name can be an export.
func isPingName(name string) bool {
	return strings.HasSuffix(name, fileSuffix) && !strings.HasPrefix(name, ".")
}

// findInput resolves the file input to a path in dir. name is a base name; empty means the newest
// *.csv.gz there (ties: the larger name). A name with a path separator or a dot-dot is refused, and
// so is anything but a regular file (no symbolic link). With an empty name and no file (or no dir)
// it returns found false and no error; a named file that is not there is ErrPermanent.
func findInput(dir, name string) (path string, found bool, err error) {
	if name != "" {
		if name != filepath.Base(name) || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
			return "", false, permanent("file must be a base name inside the inbox, not a path")
		}
		if !isPingName(name) {
			return "", false, permanent("file must end in %s", fileSuffix)
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
	var best string
	var bestMod time.Time
	for _, e := range entries {
		if !isPingName(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if best == "" || fi.ModTime().After(bestMod) || fi.ModTime().Equal(bestMod) && e.Name() > best {
			best, bestMod = e.Name(), fi.ModTime()
		}
	}
	if best == "" {
		return "", false, nil
	}
	return filepath.Join(dir, best), true, nil
}

var errLineTooLong = errors.New("line too long")

// readLine returns the next line without its newline (and carriage return); the slice is valid
// until the next call. A line that does not fit the buffer is skipped whole and gives
// errLineTooLong. io.EOF comes only when there is no more data.
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		for err == bufio.ErrBufferFull {
			_, err = r.ReadSlice('\n')
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		return nil, errLineTooLong
	}
	if err == io.EOF && len(line) > 0 {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	return bytes.TrimSuffix(line, []byte{'\r'}), nil
}

// check streams the file, counts into out and returns an error only for a bad header, a damaged
// file, too many lines or a cancelled context. The rows are untrusted and never echoed.
func (p *Pipeline) check(ctx context.Context, src io.Reader, out *Output) error {
	gz, err := gzip.NewReader(bufio.NewReaderSize(src, 1<<20))
	if err != nil {
		return permanent("the input file is not a valid gzip file")
	}
	defer gz.Close()
	r := bufio.NewReaderSize(gz, maxHeader)
	header, err := readLine(r)
	if err != nil {
		return permanent("the input file has no header line")
	}
	sites, ok := parseHeader(header)
	if !ok {
		return permanent("the header must be prefix and then 3 to 64 distinct site columns named CC/City")
	}
	out.Sites = sites
	width := sites + 1

	pace := outputs.Pace{Every: outputs.ProgressEvery}
	var prev uint32
	havePrev := false
	for lines := 0; ; {
		line, err := readLine(r)
		if err == io.EOF {
			break
		}
		// Every line counts for the safety checks, empty ones too.
		if lines++; lines > maxPingRows+1000 {
			return permanent("more than %d lines", maxPingRows)
		}
		if lines&0xFFFF == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			if pace.Due() {
				p.log("check: %d rows read, %d valid, %d bad", out.Rows, out.Valid, out.Bad())
			}
		}
		tooLong := errors.Is(err, errLineTooLong)
		if err != nil && !tooLong {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return permanent("the input file is damaged or cut short")
		}
		if !tooLong && len(line) == 0 {
			continue
		}
		if out.Rows++; out.Rows > maxPingRows {
			return permanent("more than %d rows", maxPingRows)
		}
		if tooLong || len(line) > maxLine {
			out.BadColumns++
			continue
		}
		switch checkRow(line, width, &prev, &havePrev) {
		case rowOK:
			out.Valid++
		case rowColumns:
			out.BadColumns++
		case rowPrefix:
			out.BadPrefix++
		case rowNotPublic:
			out.NotPublic++
		case rowNotSorted:
			out.NotSorted++
		case rowRTT:
			out.BadRTT++
		case rowFewSites:
			out.FewSites++
		}
	}
	return nil
}

// parseHeader checks the header line and returns the number of site columns.
func parseHeader(line []byte) (int, bool) {
	fields := bytes.Split(line, []byte{','})
	if len(fields) < 1+minSites || len(fields) > 1+maxSites || string(fields[0]) != "prefix" {
		return 0, false
	}
	seen := map[string]bool{}
	for _, f := range fields[1:] {
		if !validSite(f) || seen[string(f)] {
			return 0, false
		}
		seen[string(f)] = true
	}
	return len(fields) - 1, true
}

// validSite checks "<CC>/<City>": two upper-case ASCII letters, a slash and a city of 1 to 64 bytes
// that is valid UTF-8 with no control character, angle bracket or comma.
func validSite(f []byte) bool {
	if len(f) < 4 || f[2] != '/' || f[0] < 'A' || f[0] > 'Z' || f[1] < 'A' || f[1] > 'Z' {
		return false
	}
	city := f[3:]
	return len(city) <= maxSiteName && utf8.Valid(city) &&
		!bytes.ContainsFunc(city, func(r rune) bool { return unicode.IsControl(r) || r == '<' || r == '>' || r == ',' })
}

type rowStatus int

const (
	rowOK rowStatus = iota
	rowColumns
	rowPrefix
	rowNotPublic
	rowNotSorted
	rowRTT
	rowFewSites
)

// checkRow checks one data line against the rules in the order columns, prefix, public address,
// order, times, answers. prev is the address of the last row that was in order.
func checkRow(line []byte, width int, prev *uint32, havePrev *bool) rowStatus {
	// width-1 commas, counted before any split, so a long row costs no allocation.
	if bytes.Count(line, []byte{','}) != width-1 {
		return rowColumns
	}
	prefix, rest, _ := bytes.Cut(line, []byte{','})
	pfx, err := netip.ParsePrefix(string(prefix))
	if err != nil || !pfx.Addr().Is4() || pfx.Bits() != 24 || pfx.Masked() != pfx {
		return rowPrefix
	}
	if !ipdata.PublicV4(pfx.Addr()) {
		return rowNotPublic
	}
	a := pfx.Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	if *havePrev && v <= *prev {
		return rowNotSorted
	}
	*prev, *havePrev = v, true
	answers := 0
	for n := 0; n < width-1; n++ { // exactly width-1 cells: the comma count was checked
		var cell []byte
		cell, rest, _ = bytes.Cut(rest, []byte{','})
		if len(cell) > 0 {
			if !validRTT(cell) {
				return rowRTT
			}
			answers++
		}
	}
	if answers < minAnswers {
		return rowFewSites
	}
	return rowOK
}

// validRTT is a plain decimal (digits and at most one dot) above 0 and at most maxRTTMs.
func validRTT(cell []byte) bool {
	dots := 0
	for _, c := range cell {
		switch {
		case c >= '0' && c <= '9':
		case c == '.':
			dots++
		default:
			return false
		}
	}
	if dots > 1 || len(cell) > 32 {
		return false
	}
	v, err := strconv.ParseFloat(string(cell), 64)
	return err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 && v <= maxRTTMs
}
