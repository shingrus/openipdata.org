package geofeeds

import (
	"cmp"
	_ "embed"
	"encoding/binary"
	"encoding/csv"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"openipdata.org/hatchet/outputs"
	"openipdata.org/hatchet/rirstats"
)

// The whois text layer: many RIPE inetnum objects name a city in their free text
// (descr: lines, netname:). While discover reads the dump it matches the words of
// those lines against the GeoNames places of the object's own country and writes
// WhoisTextFile, a layer file in the same format as RowsFile. export-rows uploads
// it to WhoisTextKey; build-geo-data applies it as 37-whois-text, below the strong
// layers. Measured on RIPE Atlas it is a clear gain; records broader than a /16
// are right only 32% of the time, so they are left out.
const (
	WhoisTextFile = "37-whois-text.csv.gz"
	WhoisTextKey  = "internal/layers/" + WhoisTextFile

	whoisTextUploadFile     = "whois-text-upload.json" // what the last whois text upload sent
	minWhoisTextFirstUpload = 200_000                  // with no previous upload, refuse fewer rows than this
	maxTextWords            = 3                        // longest word group (n-gram) that can name a place
	// GeoNames lists old names and foreign spellings for every village ("Melita" for Mdina,
	// "Trac" for Trazo), and those are mostly the name of an ISP or a product. So the alternate
	// names count only for places of at least this many people, where a text does use them
	// (Muenchen, Wien, Praha); a smaller place is matched by its name and asciiname only.
	altNameMinPop = 15_000
)

// whoisTextDumps are the whois dumps whose inetnum objects are read for text (by
// dump name). Only RIPE is measured; APNIC and AFRINIC are not yet, so they are
// not switched on. Adding a name here is all it takes to read another registry.
var whoisTextDumps = map[string]bool{"ripe-inetnum": true}

// whois-ignore.txt lists words that are also the name of some town but in a
// network description almost never mean that town (see the file).
//
//go:embed whois-ignore.txt
var whoisIgnoreList string

// ignoreEntries returns the entries of whois-ignore.txt as written.
func ignoreEntries(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// whoisIgnore holds the entries after the same normalisation as the text.
var whoisIgnore = func() map[string]bool {
	out := map[string]bool{}
	for _, e := range ignoreEntries(whoisIgnoreList) {
		if words, _ := normWords(e); len(words) > 0 {
			out[strings.Join(words, " ")] = true
		}
	}
	return out
}()

// foldMap turns Latin letters with marks into plain letters.
var foldMap = func() map[rune]string {
	out := map[rune]string{'ß': "ss", 'æ': "ae", 'œ': "oe", 'þ': "th", 'ə': "e", 'ĳ': "ij"}
	for plain, marked := range map[string]string{
		"a": "àáâãäåāăąǎǻ", "c": "çćĉċč", "d": "ďđð", "e": "èéêëēĕėęě", "g": "ĝğġģ", "h": "ĥħ",
		"i": "ìíîïĩīĭįıǐ", "j": "ĵ", "k": "ķ", "l": "ĺļľŀł", "n": "ñńņňŉŋ", "o": "òóôõöøōŏőǒǿ",
		"r": "ŕŗř", "s": "śŝşšș", "t": "ţťŧț", "u": "ùúûüũūŭůűųǔ", "w": "ŵ", "y": "ýÿŷ", "z": "źżž",
	} {
		for _, r := range marked {
			out[r] = plain
		}
	}
	return out
}()

// normWords cuts a text into lower case words of plain Latin letters: accents are
// folded, every character that is not a Latin letter separates words, "st" is
// "saint" and "ste" "sainte". other is true when the text has a letter of another
// script (Cyrillic, Greek, Arabic...): such a line is not about a place we name.
// A Latin letter the table does not know (a Vietnamese vowel) spoils only its own word:
// that word is dropped and the rest of the line is still read.
func normWords(s string) (words []string, other bool) {
	words, other, _ = normText(s)
	return words, other
}

// normText is normWords that also says whether a word was dropped. A place name that
// lost a word must not be indexed under the words that are left ("Bac Ninh" as "ninh").
func normText(s string) (words []string, other, dropped bool) {
	var cur []byte
	bad := false // the current word holds a Latin letter we cannot fold
	flush := func() {
		if bad {
			bad, dropped = false, true
			cur = cur[:0]
			return
		}
		if len(cur) == 0 {
			return
		}
		switch w := string(cur); w {
		case "st":
			words = append(words, "saint")
		case "ste":
			words = append(words, "sainte")
		default:
			words = append(words, w)
		}
		cur = cur[:0]
	}
	for _, r := range s {
		r = unicode.ToLower(r)
		switch {
		case r >= 'a' && r <= 'z':
			cur = append(cur, byte(r))
		case r < utf8.RuneSelf:
			flush()
		default:
			if f, ok := foldMap[r]; ok {
				cur = append(cur, f...)
			} else if unicode.Is(unicode.Mn, r) {
				// A combining mark: dropped, the letter before it stays.
			} else if unicode.Is(unicode.Latin, r) {
				bad = true
			} else if unicode.IsLetter(r) {
				other = true
				flush()
			} else {
				flush()
			}
		}
	}
	flush()
	return words, other, dropped
}

// latin1 reads a value that is not valid UTF-8 as Latin-1 (the RIPE dump has
// such lines); a valid one is returned as it is.
func latin1(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	runes := make([]rune, len(s))
	for i := 0; i < len(s); i++ {
		runes[i] = rune(s[i])
	}
	return string(runes)
}

// textPlace is a GeoNames place that a text can name.
type textPlace struct {
	name string // the main name: the city written to the layer
	pop  int
}

// nameIndex finds the place a word group names: the key is the country, "|" and
// the normalised name; the value is the most populous place with that key.
type nameIndex struct {
	places []textPlace
	ids    map[string]int32
}

func newNameIndex() *nameIndex { return &nameIndex{ids: map[string]int32{}} }

// bigger says whether place a ranks before place b: more people, then the
// smaller main name, then the earlier one in the file.
func (x *nameIndex) bigger(a, b int32) bool {
	pa, pb := x.places[a], x.places[b]
	if pa.pop != pb.pop {
		return pa.pop > pb.pop
	}
	if pa.name != pb.name {
		return pa.name < pb.name
	}
	return a < b
}

// add indexes a place of the country under its main name, asciiname and, for a
// place of at least altNameMinPop people, every alternate name (comma separated).
// A name counts when it is made of Latin letters and has at most maxTextWords words.
func (x *nameIndex) add(country, name, ascii, alternates string, pop int) {
	id := int32(len(x.places))
	x.places = append(x.places, textPlace{name, pop})
	one := func(n string) {
		words, other, dropped := normText(n)
		if other || dropped || len(words) == 0 || len(words) > maxTextWords {
			return
		}
		key := country + "|" + strings.Join(words, " ")
		if old, ok := x.ids[key]; !ok || x.bigger(id, old) {
			x.ids[key] = id
		}
	}
	one(name)
	one(ascii)
	if alternates != "" && pop >= altNameMinPop {
		for _, n := range strings.Split(alternates, ",") {
			one(n)
		}
	}
}

// refused says whether a word group can never be a city, before any lookup: a
// lone "saint", a single word of 3 letters or fewer, a group of words that all
// have 2 letters or fewer.
func refused(group []string) bool {
	if len(group) == 1 {
		w := group[0]
		return len(w) <= 3 || w == "saint" || w == "sainte"
	}
	for _, w := range group {
		if len(w) > 2 {
			return false
		}
	}
	return true
}

// textCity returns the place that the lines (word lists, see normWords) of an
// object of the country name: of all word groups of 1 to 3 words within a line
// that are the name of a place of the country, the longest wins, then the more
// populous place, then the smaller name. Groups on the ignore list are not names.
func (x *nameIndex) textCity(country string, lines [][]string) (best int32, ok bool) {
	bestWords := 0
	var key []byte
	for _, words := range lines {
		for n := 1; n <= maxTextWords && n <= len(words); n++ {
			for i := 0; i+n <= len(words); i++ {
				group := words[i : i+n]
				if refused(group) {
					continue
				}
				key = append(append(key[:0], country...), '|')
				for k, w := range group {
					if k > 0 {
						key = append(key, ' ')
					}
					key = append(key, w...)
				}
				id, found := x.ids[string(key)]
				if !found || whoisIgnore[string(key[len(country)+1:])] {
					continue
				}
				if n > bestWords || n == bestWords && x.bigger(id, best) {
					best, bestWords, ok = id, n, true
				}
			}
		}
	}
	return best, ok
}

// textBlock is an IPv4 inetnum whose text names a city. place is an index into
// nameIndex.places and registry one into the registries the blocks came from.
type textBlock struct {
	from, to uint32
	country  [2]byte
	place    int32
	registry uint8
}

// textReader collects the text blocks of one dump (or of all of them).
type textReader struct {
	objects int            // inetnum objects read
	dropped map[string]int // objects, by reason
	blocks  []textBlock
}

// whoisCountry returns the first country: value of an object, upper case, cut at the
// first space, tab or "#" (a comment may follow the code: "NL # moved"); "" if none.
// The geoloc and the text layer both read the country this way.
func whoisCountry(obj []attr) string {
	for _, a := range obj {
		if a.key == "country" {
			v := strings.ToUpper(strings.TrimSpace(a.value))
			if i := strings.IndexAny(v, " \t#"); i >= 0 {
				v = v[:i]
			}
			return v
		}
	}
	return ""
}

// read looks at one RPSL object. All of it is untrusted: a bad object only counts.
func (t *textReader) read(obj []attr, x *nameIndex, registry uint8) {
	if len(obj) == 0 || obj[0].key != "inetnum" {
		return
	}
	t.objects++
	from, to, err := parseRange(obj[0].value)
	if err != nil {
		return
	}
	first, last := from.As4(), to.As4()
	b := textBlock{from: binary.BigEndian.Uint32(first[:]), to: binary.BigEndian.Uint32(last[:]), registry: registry}
	if uint64(b.to)-uint64(b.from)+1 > maxGeolocBlock {
		t.drop(dropTooLarge)
		return
	}
	country := whoisCountry(obj)
	if !countries[country] {
		t.drop(dropBadCountry)
		return
	}
	var lines [][]string
	netname := false
	for _, a := range obj[1:] {
		if a.key != "descr" && (a.key != "netname" || netname) {
			continue
		}
		netname = netname || a.key == "netname"
		if words, other := normWords(latin1(a.value)); !other && len(words) > 0 {
			lines = append(lines, words)
		}
	}
	id, ok := x.textCity(country, lines)
	if !ok {
		return
	}
	b.place = id
	copy(b.country[:], country)
	t.blocks = append(t.blocks, b)
}

// merge adds what another reader found.
func (t *textReader) merge(o *textReader) {
	t.objects += o.objects
	t.blocks = append(t.blocks, o.blocks...)
	for reason, n := range o.dropped {
		if t.dropped == nil {
			t.dropped = map[string]int{}
		}
		t.dropped[reason] += n
	}
}

func (t *textReader) drop(reason string) {
	if t.dropped == nil {
		t.dropped = map[string]int{}
	}
	t.dropped[reason]++
}

// WhoisTextOutput is the whois text part of the discover result.
type WhoisTextOutput struct {
	Objects  int            `json:"objects"`           // inetnum objects of the text dumps
	Blocks   int            `json:"blocks"`            // objects whose text names a city
	Dropped  map[string]int `json:"dropped,omitempty"` // objects, by reason
	Rows     int            `json:"rows"`              // prefixes written
	Bytes    int64          `json:"bytes,omitempty"`
	GeoNames string         `json:"geonames,omitempty"` // downloaded, cached, fresh, stale
	// Skipped says why no layer file was written this run (the previous one
	// stays). A problem here never fails discover or touches the geofeed rows.
	Skipped string `json:"skipped,omitempty"`
}

// buildWhoisText writes WhoisTextFile from the text blocks that readDump
// collected. It never fails: a problem ends in the result's Skipped.
func (p *Pipeline) buildWhoisText(t *textReader, registries []string, gn *geoNames) WhoisTextOutput {
	out := WhoisTextOutput{Objects: t.objects, Blocks: len(t.blocks), Dropped: t.dropped, GeoNames: gn.status}
	skip := func(format string, args ...any) WhoisTextOutput {
		out.Skipped = fmt.Sprintf(format, args...)
		p.log("discover: WARNING whois text layer not built: %s", out.Skipped)
		return out
	}
	switch {
	case gn.err != nil:
		return skip("%v", gn.err)
	case len(t.blocks) == 0:
		return skip("no inetnum text names a city")
	}
	outDir := filepath.Join(p.dir(), OutDir)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return skip("%v", err)
	}
	path := filepath.Join(outDir, WhoisTextFile)
	var err error
	if out.Rows, err = writeWhoisTextRows(path, t.blocks, gn.names, registries); err != nil {
		return skip("%v", err)
	}
	out.Bytes = outputs.FileSize(path)
	p.log("discover: whois text: %d objects, %d with a city, dropped %v, %d rows, %d bytes, GeoNames %s",
		out.Objects, out.Blocks, out.Dropped, out.Rows, out.Bytes, out.GeoNames)
	return out
}

// writeWhoisTextRows writes one row per prefix of each block, sorted by start
// address and, for one start, the broader block first. A narrower block inside a
// broader one is written too: the builder inserts a layer's rows broadest first,
// so the narrower one overrides its parent, and a record without a city inherits
// its parent's. With no rows it fails and leaves the file there.
func writeWhoisTextRows(path string, blocks []textBlock, x *nameIndex, registries []string) (int, error) {
	sorted := slices.Clone(blocks)
	slices.SortFunc(sorted, func(a, b textBlock) int {
		return cmp.Or(
			cmp.Compare(a.from, b.from),
			cmp.Compare(b.to, a.to),
			cmp.Compare(string(a.country[:]), string(b.country[:])),
			cmp.Compare(a.registry, b.registry),
			cmp.Compare(x.places[a.place].name, x.places[b.place].name),
		)
	})
	return writeLayerFile(path, func(w *csv.Writer) int {
		rows := 0
		for _, b := range sorted {
			from, to := netip.AddrFrom4(u32Bytes(b.from)), netip.AddrFrom4(u32Bytes(b.to))
			for _, prefix := range rirstats.RangeToPrefixes(from, to) {
				w.Write([]string{prefix.String(), string(b.country[:]), "", x.places[b.place].name, "whois-text:" + registries[b.registry]})
				rows++
			}
		}
		return rows
	})
}

func u32Bytes(v uint32) [4]byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b
}
