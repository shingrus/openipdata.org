package geofeeds

import (
	"bufio"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

//go:embed countries.txt
var countryList string

var countries = func() map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(countryList, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		for _, code := range strings.Fields(line) {
			out[code] = true
		}
	}
	return out
}()

var regionPattern = regexp.MustCompile(`^[A-Z]{2}-[A-Z0-9]{1,3}$`)

// Row rejection reasons, counted per feed.
const (
	rejectBadLine    = "bad_line"    // not CSV, or fewer than two fields
	rejectBadPrefix  = "bad_prefix"  // not an IP prefix or address
	rejectBadCountry = "bad_country" // not an ISO 3166-1 alpha-2 code
	rejectReserved   = "reserved"    // not public: private, loopback, shared (100.64/10), documentation…
	rejectTooBroad   = "too_broad"   // shorter than /8 (IPv4) or /16 (IPv6)
	dropBadRegion    = "bad_region"  // row kept without its region
	dropBadCity      = "bad_city"    // row kept without its city
)

// feedRow is one checked geofeed line: prefix,country,region,city. The postal
// code field is dropped; RFC 8805 deprecates it.
type feedRow struct {
	prefix                netip.Prefix
	country, region, city string
}

// parseRow checks one RFC 8805 record. It returns the row, or the reason it
// was rejected. A malformed region or city is dropped from the row, which is
// kept, and reported as dropBadRegion or dropBadCity.
func parseRow(fields []string) (feedRow, string) {
	if len(fields) < 2 {
		return feedRow{}, rejectBadLine
	}
	field := func(i int) string {
		if i < len(fields) {
			return strings.TrimSpace(fields[i])
		}
		return ""
	}
	prefix, err := parseRowPrefix(field(0))
	if err != nil {
		return feedRow{}, rejectBadPrefix
	}
	addr := prefix.Addr()
	if (addr.Is4() && prefix.Bits() < 8) || (addr.Is6() && prefix.Bits() < 16) {
		return feedRow{}, rejectTooBroad
	}
	if !publicAddr(addr) {
		return feedRow{}, rejectReserved
	}
	row := feedRow{prefix: prefix, country: strings.ToUpper(field(1)), city: field(3)}
	if !countries[row.country] {
		return feedRow{}, rejectBadCountry
	}
	dropped := ""
	if !plainCity(row.city) {
		row.city, dropped = "", dropBadCity
	}
	region := strings.ToUpper(field(2))
	if region == "" {
		return row, dropped
	}
	if !regionPattern.MatchString(region) || region[:2] != row.country {
		return row, dropBadRegion
	}
	row.region = region
	return row, dropped
}

const maxCityLen = 100 // bytes; the longest real city names are well under it

// plainCity reports whether a city is safe to pass on: the text goes into a
// public database as the feed wrote it, so it must be short, valid UTF-8, and
// free of control characters and markup.
func plainCity(city string) bool {
	if len(city) > maxCityLen || !utf8.ValidString(city) {
		return false
	}
	for _, r := range city {
		if unicode.IsControl(r) || r == '<' || r == '>' {
			return false
		}
	}
	return true
}

// parseRowPrefix accepts a prefix or a bare address (a single host).
func parseRowPrefix(s string) (netip.Prefix, error) {
	if !strings.Contains(s, "/") {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if p.Addr().Is4In6() {
		return netip.Prefix{}, errors.New("IPv4-mapped prefix")
	}
	return p.Masked(), nil
}

// maxFeedLine caps one feed line: a longer one is rejected without being held
// in memory, so a feed with no line breaks cannot grow the worker.
const maxFeedLine = 64 * 1024

// bodyError marks a failure to read the feed itself. Every other writeFeed
// error is a local file error.
type bodyError struct{ err error }

func (e *bodyError) Error() string { return e.err.Error() }
func (e *bodyError) Unwrap() error { return e.err }

// readLine returns the next line without its line break. A line longer than
// the reader's buffer is skipped to its end and reported as tooLong.
func readLine(r *bufio.Reader) (line string, tooLong bool, err error) {
	chunk, more, err := r.ReadLine()
	if err != nil {
		return "", false, err
	}
	line = string(chunk)
	for more {
		tooLong = true
		if _, more, err = r.ReadLine(); err != nil {
			if err == io.EOF {
				break
			}
			return "", true, err
		}
	}
	return line, tooLong, nil
}

// splitLine splits one feed line into fields; quoted fields are rare, so the
// CSV parser runs only for lines that have a quote.
func splitLine(line string) ([]string, bool) {
	if !strings.Contains(line, `"`) {
		return strings.Split(line, ","), true
	}
	r := csv.NewReader(strings.NewReader(line))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	fields, err := r.Read()
	return fields, err == nil
}

// feedHeader is the first line of a stored feed file, after "# ". It keeps the
// HTTP validators with the rows they describe, so one rename updates both.
type feedHeader struct {
	URL          string         `json:"url"`
	Fetched      string         `json:"fetched"` // RFC 3339
	ETag         string         `json:"etag,omitempty"`
	LastModified string         `json:"last_modified,omitempty"`
	Rows         int            `json:"rows"`
	Rejected     map[string]int `json:"rejected,omitempty"`
}

// writeFeed checks body as a geofeed and writes the valid rows to path, via a
// .tmp file. Rows, rejection counts, and validators are in the returned header.
// A failure to read body is returned as a *bodyError.
func writeFeed(body io.Reader, path string, header feedHeader) (feedHeader, error) {
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return header, err
	}
	defer os.Remove(tmp)

	// The header goes first but needs the counts, so rows go to a second file.
	data, err := os.CreateTemp(filepath.Dir(path), ".rows-*")
	if err != nil {
		file.Close()
		return header, err
	}
	defer os.Remove(data.Name())
	defer data.Close()

	w := csv.NewWriter(data)
	header.Rows, header.Rejected = 0, map[string]int{}
	lines := bufio.NewReaderSize(body, maxFeedLine)
	for {
		line, tooLong, err := readLine(lines)
		if err == io.EOF {
			break
		}
		if err != nil {
			file.Close()
			return header, &bodyError{err}
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" || line[0] == '#' {
			continue
		}
		fields, ok := splitLine(line)
		if tooLong || !ok {
			header.Rejected[rejectBadLine]++
			continue
		}
		row, reason := parseRow(fields)
		if reason != "" {
			header.Rejected[reason]++
			if reason != dropBadRegion && reason != dropBadCity {
				continue
			}
		}
		header.Rows++
		w.Write([]string{row.prefix.String(), row.country, row.region, row.city})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		file.Close()
		return header, err
	}
	if len(header.Rejected) == 0 {
		header.Rejected = nil
	}

	line, _ := json.Marshal(header)
	out := bufio.NewWriter(file)
	fmt.Fprintf(out, "# %s\n", line)
	if _, err := data.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return header, err
	}
	if _, err := io.Copy(out, data); err != nil {
		file.Close()
		return header, err
	}
	if err := out.Flush(); err != nil {
		file.Close()
		return header, err
	}
	if err := file.Close(); err != nil {
		return header, err
	}
	return header, os.Rename(tmp, path)
}

// readFeedHeader returns the header of a stored feed file.
func readFeedHeader(path string) (feedHeader, error) {
	var h feedHeader
	file, err := os.Open(path)
	if err != nil {
		return h, err
	}
	defer file.Close()
	line, err := bufio.NewReader(file).ReadString('\n')
	if err != nil {
		return h, err
	}
	if !strings.HasPrefix(line, "# ") {
		return h, fmt.Errorf("%s: no header", path)
	}
	return h, json.Unmarshal([]byte(line[2:]), &h)
}
