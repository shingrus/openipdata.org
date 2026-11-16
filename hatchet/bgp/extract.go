package bgp

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"time"
)

// The communities file (OutFile, and at CommunitiesKey in R2) is a gzipped CSV:
//
//	prefix,communities
//	192.0.2.0/24,64500:100 64500:200 4200000000:1:2
//
// One row for every IPv4 prefix of /8 to /24 in the RIB dump (also one whose values
// were all dropped: its communities field is then empty, and build-geo-data needs
// every prefix to know which addresses a broader prefix still owns). The values are
// separated by a space: a standard community is a:b, a large one a:b:c; standard
// values come first, each group in numeric order. Rows are in the order of the dump
// (by address, then length).
//
// A value is dropped when it can never be a rule: carried by more than commonPercent
// of all prefixes (the networks that every route gets, not a place) or by fewer than
// minPrefixes prefixes (nothing to learn from).
const (
	// File names: the local one, and the one build-geo-data downloads.
	OutFile = "bgp-communities.csv.gz"
	// CommunitiesKey is the one fixed key in the private R2 bucket, replaced by each upload.
	CommunitiesKey = "internal/bgp/communities.csv.gz"

	commonPercent = 5
	minPrefixes   = 3
	// minFilePrefixes is the least number of prefixes a real table has; fewer is a bad dump.
	minFilePrefixes = 500_000
)

// ExtractOutput is the extract task's result: counts only.
type ExtractOutput struct {
	Bytes           int64   `json:"bytes"`
	Prefixes        int     `json:"prefixes"`
	Peers           int     `json:"peers"`
	ValuesSeen      int     `json:"values_seen"`
	ValuesKept      int     `json:"values_kept"`
	ValuesCommon    int     `json:"values_common"`
	ValuesRare      int     `json:"values_rare"`
	Pairs           int64   `json:"pairs"` // (prefix, value) pairs written
	OutOfRange      uint64  `json:"prefixes_out_of_range"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// ErrTooFewPrefixes means the dump has too few prefixes to be a real table.
var ErrTooFewPrefixes = errors.New("too few prefixes in the dump")

// scanFile streams the gzipped MRT file at path through the scanner.
func scanFile(ctx context.Context, s *Scanner, path string, fn func(netip.Prefix, []uint32) error) (Stats, error) {
	f, err := os.Open(path)
	if err != nil {
		return Stats{}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return Stats{}, fmt.Errorf("%w: %v", ErrDamaged, err)
	}
	defer gz.Close()
	n := 0
	st, err := s.Scan(gz, func(p netip.Prefix, ids []uint32) error {
		if n++; n%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		return fn(p, ids)
	})
	return st, asDamaged(err)
}

// asDamaged wraps the errors of a broken gzip stream (bad header or checksum, corrupt
// deflate data, a file that ends early) in ErrDamaged; other errors pass as they are.
func asDamaged(err error) error {
	var corrupt flate.CorruptInputError
	if err == nil || errors.Is(err, ErrDamaged) {
		return err
	}
	if errors.Is(err, gzip.ErrChecksum) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &corrupt) {
		return fmt.Errorf("%w: %v", ErrDamaged, err)
	}
	return err
}

// checkGzip reads the whole gzip file once (nothing is kept) and returns an error
// wrapping ErrDamaged when the stream is broken: the checksum at its end covers every
// byte, so a file that passes is the file the server sent.
func checkGzip(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDamaged, err) // no gzip header, or an empty file
	}
	defer gz.Close()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := gz.Read(buf); err == io.EOF {
			return nil
		} else if err != nil {
			return asDamaged(err)
		}
	}
}

// ExtractFile reads the gzipped MRT dump at in (two passes, streaming) and writes the
// communities file at out through a temporary file. Memory stays small: pass 1 only
// counts, in how many prefixes each value occurs; pass 2 writes the rows. minRows is
// the least number of prefixes the dump must have (ErrTooFewPrefixes otherwise; the
// output is not written).
func ExtractFile(ctx context.Context, in, out string, minRows int) (ExtractOutput, error) {
	started := time.Now()
	var res ExtractOutput
	s := NewScanner()

	// Pass 1: in how many prefixes does each value occur?
	var counts []uint32
	var total int
	st, err := scanFile(ctx, s, in, func(_ netip.Prefix, ids []uint32) error {
		total++
		for _, id := range ids {
			for int(id) >= len(counts) {
				counts = append(counts, 0)
			}
			counts[id]++
		}
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("read %s: %w", in, err)
	}
	if total < minRows {
		return res, fmt.Errorf("%w: %d, fewer than %d", ErrTooFewPrefixes, total, minRows)
	}
	res.Prefixes, res.Peers, res.OutOfRange = total, st.Peers, st.OutOfRange
	res.ValuesSeen = len(s.Values)

	// Which values stay, and in which order they are written (numeric, standard first).
	var kept []uint32
	for id := range s.Values {
		c := uint64(counts[id])
		switch {
		case c*100 > uint64(total)*commonPercent:
			res.ValuesCommon++
		case c < minPrefixes:
			res.ValuesRare++
		default:
			kept = append(kept, uint32(id))
		}
	}
	slices.SortFunc(kept, func(a, b uint32) int {
		switch {
		case s.Values[a].less(s.Values[b]):
			return -1
		case s.Values[b].less(s.Values[a]):
			return 1
		}
		return 0
	})
	res.ValuesKept = len(kept)
	rank := make([]int32, len(s.Values)) // id -> position in kept, -1 dropped
	for i := range rank {
		rank[i] = -1
	}
	names := make([][]byte, len(kept))
	for r, id := range kept {
		rank[id] = int32(r)
		names[r] = []byte(s.Values[id].String())
	}

	// Pass 2: write the rows.
	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return res, err
	}
	defer os.Remove(tmp)
	defer f.Close()
	gz, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return res, err
	}
	w := bufio.NewWriterSize(gz, 1<<20)
	w.WriteString("prefix,communities\n")
	var ranks []int32
	var line []byte
	rows := 0
	_, err = scanFile(ctx, s, in, func(p netip.Prefix, ids []uint32) error {
		ranks = ranks[:0]
		for _, id := range ids {
			if int(id) < len(rank) && rank[id] >= 0 {
				ranks = append(ranks, rank[id])
			}
		}
		slices.Sort(ranks)
		line = p.Addr().AppendTo(line[:0])
		line = append(line, '/')
		line = strconv.AppendInt(line, int64(p.Bits()), 10)
		line = append(line, ',')
		for i, r := range ranks {
			if i > 0 {
				line = append(line, ' ')
			}
			line = append(line, names[r]...)
		}
		line = append(line, '\n')
		rows++
		res.Pairs += int64(len(ranks))
		_, err := w.Write(line)
		return err
	})
	if err != nil {
		return res, fmt.Errorf("read %s: %w", in, err)
	}
	if rows != total {
		return res, fmt.Errorf("%s changed while it was read (%d prefixes, then %d)", in, total, rows)
	}
	if err := w.Flush(); err != nil {
		return res, err
	}
	if err := errors.Join(gz.Close(), f.Close()); err != nil {
		return res, err
	}
	if err := os.Rename(tmp, out); err != nil {
		return res, err
	}
	info, err := os.Stat(out)
	if err != nil {
		return res, err
	}
	res.Bytes = info.Size()
	res.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	return res, nil
}
