package main

// The full GeoNames list as a fallback for city names the main list does not place
// (mmdbbuilder geo -geonames-all). cities1000.txt holds the places of 1,000 people or more; a name
// it lacks (JP/Shinagawa-ku, SE/Spanga, CH/Rapperswil-Jona) gets no coordinates and counts as a
// wrong answer. GeoNames' allCountries file also has the smaller places, the city sections and the
// municipalities, but it is far too large to index, so only the names that are still unplaced
// after everything else are looked up in it:
//
//  1. wantedFromLayers reads the layer files (and the holders file) with the same reader and the
//     same per-row steps as the build (country check, city cleanup, cleanCity, the main index) and
//     collects the (country, city) keys the main index does not place.
//  2. addFullPlaces streams the full file once and keeps only the candidates of those keys.
//  3. The winners go into the index under keys that no country code can equal ("US+|name", like
//     the regional keys "US:ME|name"), so every loop that takes the country of a key with
//     plainKeyCountry, the cleaner (geoNames.place) and the site names of the RTT and ping checks
//     see exactly the main index. Only the record-level lookups, placeFull and through it
//     placeOf and placeIn, consult them, and only when the main index has no place of the name.
//
// The fallback never renames a city, never changes a city the main index places and gives no
// city_region (the row's region rule is for main-index names).

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

const (
	// fullAmbiguousKm: two candidates of the same rank further apart than this make a name ambiguous.
	fullAmbiguousKm = 50
	// fullMaxLine is the longest line read; a longer one is skipped (the main reader fails at 4 MB).
	fullMaxLine = 4 << 20
	// fullMaxBytes caps what is read from the file, a zip bomb included (allCountries.txt is about 1.7 GB).
	fullMaxBytes = 8 << 30
	// fullEntry is the name of the file inside GeoNames' zip.
	fullEntry = "allCountries.txt"
)

// fullKeyOf turns the key of a name ("US|portland") into its key in the fallback part of the
// index ("US+|portland").
func fullKeyOf(key string) string { return key[:2] + "+" + key[2:] }

// placeFull is place, and, for a name the main index lacks, the place the full list gave it.
func (g geoNames) placeFull(country, city string) (gnPlace, bool) {
	if p, ok := g.place(country, city); ok {
		return p, true
	}
	p, ok := g[normCountry(country)+"+|"+normCity(city)]
	return p, ok
}

// fullWanted maps the key (country|normalised city) of a city the main index does not place to
// the number of layer rows that carry it (0 for a name only the holders file has).
type fullWanted map[string]int

// wantedFromLayers collects the unplaced names of the layers and the holders file ("" for none).
// cleaner is the build's (nil: no cleanup). A row the build would skip for its layer's rule
// (broader than the first layer, no part of the first layer's country) still counts here, which
// only means a name too many in the file pass.
func wantedFromLayers(layers []string, holders string, index geoNames, cleaner *cityCleaner) (fullWanted, error) {
	wanted := fullWanted{}
	for _, layer := range layers {
		err := readLayerRegions(layer, func(_ int, prefix, code, _, city string) {
			code = strings.ToUpper(strings.TrimSpace(code))
			if !validCountries[code] {
				return
			}
			if _, err := parseGeoPrefix(prefix); err != nil {
				return
			}
			if cleaner != nil {
				city, _ = cleaner.clean(code, city)
			}
			if city = cleanCity(city); city == "" {
				return
			}
			if _, ok := index.place(code, city); !ok {
				wanted[geoNamesKey(code, city)]++
			}
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", layer, err)
		}
	}
	if holders != "" {
		err := readLayerRegions(holders, func(_ int, _, code, _, city string) {
			code = strings.ToUpper(strings.TrimSpace(code))
			if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
				return
			}
			if city = cleanCity(city); city == "" {
				return
			}
			if _, ok := index.place(code, city); !ok {
				wanted[geoNamesKey(code, city)] += 0
			}
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", holders, err)
		}
	}
	return wanted, nil
}

// fullStats are the counts of one lookup in the full list. names = placed + ambiguous + unplaced.
type fullStats struct {
	rows      int // layer rows whose name got a place
	placed    int // distinct names placed
	ambiguous int // names with two same-rank candidates over fullAmbiguousKm apart
	unplaced  int // names with no candidate at all
}

// fullCand is a candidate place of a wanted name.
type fullCand struct {
	lat, lon float64
	pop      int64
	id       uint32
	area     bool // class A (an administrative division) instead of P (a populated place)
	primary  bool // the key is the place's own name or ascii name
	tz       string
}

// applyFullList is what `geo -geonames-all` does before the build: it collects the unplaced names
// of the layers and the holders file (cleaned as the build cleans them: folded is the folded
// index of the main list, nil for no cleanup) and adds the places the full list gives them.
func applyFullList(index geoNames, folded foldedNames, layers []string, holders, path string) (fullStats, error) {
	if index == nil {
		return fullStats{}, errors.New("-geonames-all needs -geonames")
	}
	var cleaner *cityCleaner
	if folded != nil {
		cleaner = &cityCleaner{index: index, folded: folded}
	}
	wanted, err := wantedFromLayers(layers, holders, index, cleaner)
	if err != nil {
		return fullStats{}, err
	}
	st, err := addFullPlaces(index, wanted, path)
	if err != nil {
		err = fmt.Errorf("%s: %w", path, err)
	}
	return st, err
}

// addFullPlaces reads the full list at path (the GeoNames zip or the plain allCountries.txt),
// finds the place of every wanted name and adds the places to index (see the header). The file
// is untrusted: a line with too few fields or numbers that do not parse is skipped.
func addFullPlaces(index geoNames, wanted fullWanted, path string) (fullStats, error) {
	var st fullStats
	rc, err := openFullList(path)
	if err != nil {
		return st, err
	}
	defer rc.Close()
	if len(wanted) == 0 {
		return st, nil
	}
	cands, err := scanFullList(rc, wanted)
	if err != nil {
		return st, err
	}
	places, ambiguous := resolveFull(cands)
	for key, p := range places {
		index[fullKeyOf(key)] = p
		st.rows += wanted[key]
	}
	st.placed, st.ambiguous = len(places), ambiguous
	st.unplaced = len(wanted) - st.placed - st.ambiguous
	return st, nil
}

type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m multiCloser) Close() error {
	var err error
	for _, c := range m.closers {
		err = errors.Join(err, c.Close())
	}
	return err
}

// openFullList opens the full list: a zip (found by its first bytes) is read as a stream, never
// unpacked to disk; anything else is read as the plain text file.
func openFullList(file string) (io.ReadCloser, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	magic := make([]byte, 4)
	n, _ := io.ReadFull(f, magic)
	if n == 4 && bytes.Equal(magic, []byte("PK\x03\x04")) {
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		zr, err := zip.NewReader(f, info.Size())
		if err != nil {
			f.Close()
			return nil, errors.New("not a readable zip archive")
		}
		var entry *zip.File
		var regular []*zip.File
		for _, zf := range zr.File {
			if !zf.Mode().IsRegular() {
				continue
			}
			regular = append(regular, zf)
			if path.Base(zf.Name) == fullEntry {
				entry = zf
				break
			}
		}
		if entry == nil && len(regular) == 1 {
			entry = regular[0] // the only file, whatever its name
		}
		if entry == nil {
			f.Close()
			return nil, errors.New("archive has no " + fullEntry)
		}
		rc, err := entry.Open()
		if err != nil {
			f.Close()
			return nil, errors.New("bad zip entry")
		}
		return multiCloser{io.LimitReader(rc, fullMaxBytes), []io.Closer{rc, f}}, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return multiCloser{io.LimitReader(f, fullMaxBytes), []io.Closer{f}}, nil
}

// fullLine is one line of the full list split at its tabs (fields 0 to 17 are used).
type fullLine struct {
	line         []byte
	starts, ends [18]int
	n            int
}

func (l *fullLine) split(line []byte) {
	l.line, l.n = line, 0
	pos := 0
	for l.n < len(l.starts) {
		i := bytes.IndexByte(line[pos:], '\t')
		l.starts[l.n] = pos
		if i < 0 {
			l.ends[l.n] = len(line)
			l.n++
			return
		}
		l.ends[l.n] = pos + i
		l.n++
		pos += i + 1
	}
}

func (l *fullLine) field(i int) []byte { return l.line[l.starts[i]:l.ends[i]] }

// fullScan holds the state of one pass: the wanted names, what the current line matched, and a
// buffer for the key.
type fullScan struct {
	wanted    fullWanted
	countries [26 * 26]bool
	cands     map[string][]fullCand
	buf       []byte
	matches   []fullMatch
}

type fullMatch struct {
	key     string
	primary bool
}

// lookup notes the name of the current line when it is wanted (and builds the key without
// allocating when it is not).
func (s *fullScan) lookup(cc []byte, name []byte, alternate, primary bool) {
	s.buf = append(s.buf[:0], cc[0], cc[1], '|')
	if !appendNorm(&s.buf, name, alternate) {
		return
	}
	if _, ok := s.wanted[string(s.buf)]; !ok {
		return
	}
	key := string(s.buf)
	for i := range s.matches {
		if s.matches[i].key == key {
			s.matches[i].primary = s.matches[i].primary || primary
			return
		}
	}
	s.matches = append(s.matches, fullMatch{key, primary})
}

// scanFullList reads the lines of the full list and returns the candidates of the wanted keys.
// It looks at the feature class and code and the country before it touches a name, and
// allocates only for a name that is wanted.
func scanFullList(r io.Reader, wanted fullWanted) (map[string][]fullCand, error) {
	s := &fullScan{wanted: wanted, cands: map[string][]fullCand{}}
	for key := range wanted {
		s.countries[int(key[0]-'A')*26+int(key[1]-'A')] = true
	}
	br := bufio.NewReaderSize(r, fullMaxLine)
	var l fullLine
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) { // too long: skip the rest of the line
				_, err = br.ReadSlice('\n')
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err // a damaged archive ends here, not quietly
			}
			continue
		}
		if len(line) > 0 {
			l.split(bytes.TrimRight(line, "\r\n"))
			if l.n >= 15 && wantedLine(l.field(6), l.field(7), l.field(8), &s.countries) {
				cc := l.field(8)
				s.matches = s.matches[:0]
				s.lookup(cc, l.field(1), false, true)
				s.lookup(cc, l.field(2), false, true)
				for alt := l.field(3); len(alt) > 0; {
					name := alt
					if i := bytes.IndexByte(alt, ','); i >= 0 {
						name, alt = alt[:i], alt[i+1:]
					} else {
						alt = nil
					}
					s.lookup(cc, name, true, false)
				}
				if len(s.matches) > 0 {
					if c, ok := parseFullCand(&l); ok {
						for _, m := range s.matches {
							c.primary = m.primary
							s.cands[m.key] = append(s.cands[m.key], c)
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	return s.cands, nil
}

// wantedLine says whether a line is a candidate at all: a populated place (class P), or an
// administrative division of level 2 to 5 (class A, code ADM2 to ADM5; not ADM1), in a country
// that has a wanted name.
func wantedLine(class, code, country []byte, countries *[26 * 26]bool) bool {
	if len(class) != 1 || len(country) != 2 {
		return false
	}
	switch class[0] {
	case 'P':
	case 'A':
		if len(code) != 4 || string(code[:3]) != "ADM" || code[3] < '2' || code[3] > '5' {
			return false
		}
	default:
		return false
	}
	a, b := country[0], country[1]
	return a >= 'A' && a <= 'Z' && b >= 'A' && b <= 'Z' && countries[int(a-'A')*26+int(b-'A')]
}

// appendNorm appends the normalised form of a name (trimmed, lower case, as normCity) to buf.
// It returns false for an empty name, and, for an alternate name, for one without a letter a-z
// (as the main index does).
func appendNorm(buf *[]byte, name []byte, alternate bool) bool {
	name = bytes.TrimFunc(name, func(r rune) bool { return r == ' ' || (r >= '\t' && r <= '\r') })
	if len(name) == 0 {
		return false
	}
	ascii, latin := true, false
	for _, c := range name {
		if c >= 0x80 {
			ascii = false
			break
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			latin = true
		}
	}
	if !ascii {
		s := normCity(string(name))
		if s == "" || (alternate && !hasLatin(s)) {
			return false
		}
		*buf = append(*buf, s...)
		return true
	}
	if alternate && !latin {
		return false
	}
	for _, c := range name {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		*buf = append(*buf, c)
	}
	return true
}

// parseFullCand reads the numbers of a line whose name matched.
func parseFullCand(l *fullLine) (fullCand, bool) {
	lat, latErr := strconv.ParseFloat(string(l.field(4)), 64)
	lon, lonErr := strconv.ParseFloat(string(l.field(5)), 64)
	id, idErr := strconv.ParseUint(string(l.field(0)), 10, 32)
	if idErr != nil || latErr != nil || lonErr != nil || !validCoords(lat, lon) {
		return fullCand{}, false
	}
	pop, _ := strconv.ParseInt(string(l.field(14)), 10, 64)
	c := fullCand{lat: lat, lon: lon, pop: pop, id: uint32(id), area: l.field(6)[0] == 'A'}
	if l.n > 17 {
		c.tz = strings.TrimSpace(string(l.field(17)))
	}
	return c, true
}

// resolveFull picks the place of every key and counts the ambiguous names. The order of the
// candidates: a populated place before an administrative division, a primary name before an
// alternate, more people, the smaller id. A name is ambiguous, and left out, when another
// candidate of the winner's class and kind of name lies over fullAmbiguousKm from the winner.
// The result does not depend on the order of the file's lines.
func resolveFull(cands map[string][]fullCand) (places map[string]gnPlace, ambiguous int) {
	places = map[string]gnPlace{}
	for key, list := range cands {
		slices.SortFunc(list, func(a, b fullCand) int {
			switch {
			case a.area != b.area:
				if !a.area {
					return -1
				}
				return 1
			case a.primary != b.primary:
				if a.primary {
					return -1
				}
				return 1
			case a.pop != b.pop:
				if a.pop > b.pop {
					return -1
				}
				return 1
			}
			return cmpU32(a.id, b.id)
		})
		w := list[0]
		far := false
		for _, c := range list[1:] {
			if c.area == w.area && c.primary == w.primary && haversineKm(w.lat, w.lon, c.lat, c.lon) > fullAmbiguousKm {
				far = true
				break
			}
		}
		if far {
			ambiguous++
			continue
		}
		places[key] = gnPlace{w.lat, w.lon, w.pop, w.primary, w.id, w.tz}
	}
	return places, ambiguous
}

// addFlatFullPlaces is addFullPlaces for convert: the wanted names are those of the flat file's
// records that have a city and coordinates and whose name the main index lacks (the build placed
// them from the full list).
func addFlatFullPlaces(reader *maxminddb.Reader, index geoNames, path string) error {
	wanted := fullWanted{}
	for res := range reader.Networks() {
		if err := res.Err(); err != nil {
			return err
		}
		var r flatRecord
		if err := res.Decode(&r); err != nil {
			return err
		}
		code := normCountry(r.CountryCode)
		if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' || normCity(r.CityName) == "" || r.Latitude == nil || r.Longitude == nil {
			continue
		}
		if _, ok := index.place(code, r.CityName); !ok {
			wanted[geoNamesKey(code, r.CityName)]++
		}
	}
	_, err := addFullPlaces(index, wanted, path)
	return err
}
