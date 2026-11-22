package main

import (
	"bytes"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// City name cleanup for "mmdbbuilder geo -geonames". About 2-3% of the city addresses of the
// layers carry a name the exact index does not place ("São Paulo/SP", "ST PETERSBURG",
// "Wetzikon (ZH)", a Latin-1 "D\xfcsseldorf", the mojibake "DÃ¼sseldorf"). For a row whose city the
// index does not place exactly, cityCleaner finds the place by a folded name and gives the row the
// place's GeoNames main name, which then places exactly. A row that places is never touched, and a
// name nothing finds keeps its city. Measured with the real builder on real layer files (RIPE
// Atlas): about +40 net right probes, no coverage change.

// foldEntry is a place under a folded name: the same tie rule as the exact index (a primary
// name beats an alternate, then more people, the first on a tie), the GeoNames main name and id,
// how many distinct places (by id) carry the folded name and the people of the biggest other one.
type foldEntry struct {
	primary bool
	pop     int64
	name    string
	id      uint32
	places  int32
	second  int64
}

// namesake is the namesake rule used elsewhere: a place wins a name it shares only with
// places at most a tenth of its size.
const namesake = 10

// unique reports whether the entry's name is safe to use for a name that was cut: one place
// has it, or the chosen place is at least namesake times bigger than the second one.
func (e foldEntry) unique() bool {
	return e.places <= 1 || e.pop >= namesake*e.second
}

// foldedNames maps country code + "|" + folded name (foldName) to its place.
type foldedNames map[string]foldEntry

// foldLetters turns Latin letters with marks into plain letters (NFKD without the tables:
// marks are dropped, a few letters have their own spelling out). Lower case only.
var foldLetters = func() map[rune]string {
	out := map[rune]string{'ß': "ss", 'æ': "ae"}
	for plain, marked := range map[string]string{
		"a": "àáâãäåāăąǎǻạảấầẩẫậắằẳẵặ", "c": "çćĉċč", "d": "ďđð", "e": "èéêëēĕėęěẹẻẽếềểễệ", "g": "ĝğġģ", "h": "ĥħ",
		"i": "ìíîïĩīĭįıǐỉị", "j": "ĵ", "k": "ķ", "l": "ĺļľŀł", "n": "ñńņňŉŋ", "o": "òóôõöøōŏőǒǿọỏốồổỗộớờởỡợ",
		"r": "ŕŗř", "s": "śŝşšș", "t": "ţťŧț", "u": "ùúûüũūŭůűųǔǖǘǚǜụủứừửữự", "w": "ŵ", "y": "ýÿŷỳỵỷỹ", "z": "źżž",
	} {
		for _, r := range marked {
			out[r] = plain
		}
	}
	return out
}()

// foldName is the folded form of a name: lower case, marks dropped, ß ø æ ł đ ı spelled out,
// every run of characters outside a-z and 0-9 (an invalid byte too) one space, trimmed.
func foldName(s string) string {
	var b strings.Builder
	space := false
	put := func(t string) {
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteString(t)
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			put(string(r))
		case unicode.Is(unicode.Mn, r):
			// A combining mark: dropped, the letter before it stays.
		default:
			if f, ok := foldLetters[r]; ok {
				put(f)
			} else {
				space = true
			}
		}
	}
	return b.String()
}

// cp1252High is Windows-1252 for the bytes 0x80 to 0x9F; 0 marks an undefined byte.
var cp1252High = [32]rune{
	0x20AC, 0, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, 0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0, 0x017D, 0,
	0, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0, 0x017E, 0x0178,
}

// repairEncoding reads a city that is not valid UTF-8 as Windows-1252 (a byte that
// Windows-1252 leaves undefined: no repair), and a valid one that is UTF-8 read as
// Latin-1 ("DÃ¼sseldorf") back as UTF-8. Anything else is returned as it is.
func repairEncoding(s string) string {
	if !utf8.ValidString(s) {
		out := make([]rune, 0, len(s))
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c >= 0x80 && c < 0xA0 {
				r := cp1252High[c-0x80]
				if r == 0 {
					return s
				}
				out = append(out, r)
			} else {
				out = append(out, rune(c))
			}
		}
		return string(out)
	}
	raw := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			return s
		}
		raw = append(raw, byte(r))
	}
	if utf8.Valid(raw) && !bytes.Equal(raw, []byte(s)) {
		return string(raw)
	}
	return s
}

var (
	parenGroup = regexp.MustCompile(`\s*\(.*?\)\s*`)
	partSplit  = regexp.MustCompile(`\s*[,/;]\s*|\s+[-–]\s+`)
)

// cityCleaner finds the GeoNames place of a city name the exact index does not place.
type cityCleaner struct {
	index  geoNames
	folded foldedNames
	memo   map[cleanKey]cleanResult // each distinct (country, city) is cleaned once
}

type cleanKey struct{ code, city string }

type cleanResult struct {
	city    string
	renamed bool
}

func (c *cityCleaner) entry(code, folded string) (foldEntry, bool) {
	if folded == "" {
		return foldEntry{}, false
	}
	e, ok := c.folded[normCountry(code)+"|"+folded]
	return e, ok
}

// lookup finds the main name of the place under a folded name. The name is used only when it
// places (exactly) at that same place, and, for a name that was cut ("UNION (EAU CLAIRE)" to
// "union"), only when the folded name is not shared with a place of comparable size.
func (c *cityCleaner) lookup(code, folded string, cut bool) (string, bool) {
	e, ok := c.entry(code, folded)
	if !ok || (cut && !e.unique()) {
		return "", false
	}
	if p, ok := c.index.place(code, e.name); !ok || p.id != e.id {
		return "", false
	}
	return e.name, true
}

// clean returns the city a row should carry (and whether it replaced the city): the city as it is
// when the index places it, else the repaired name or the main name of the place a folded form of
// it finds, else the city as it is.
func (c *cityCleaner) clean(code, city string) (string, bool) {
	key := cleanKey{code, city}
	if r, ok := c.memo[key]; ok {
		return r.city, r.renamed
	}
	r := c.cleanOne(code, city)
	if c.memo == nil {
		c.memo = map[cleanKey]cleanResult{}
	}
	c.memo[key] = r
	return r.city, r.renamed
}

func (c *cityCleaner) cleanOne(code, city string) cleanResult {
	city = strings.TrimSpace(city)
	if city == "" {
		return cleanResult{city, false}
	}
	if _, ok := c.index.place(code, city); ok {
		return cleanResult{city, false}
	}
	if name, ok := c.find(code, city); ok && name != city {
		return cleanResult{name, true}
	}
	return cleanResult{city, false}
}

func (c *cityCleaner) find(code, city string) (string, bool) {
	s := repairEncoding(city)
	if _, ok := c.index.place(code, s); ok {
		return s, true
	}
	cut := parenGroup.MatchString(s) // a bracket group is dropped
	s = strings.TrimSpace(parenGroup.ReplaceAllString(s, " "))
	var parts []string
	for _, p := range partSplit.Split(s, -1) {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	cut = cut || len(parts) > 1 // only the first part is taken
	for _, p := range parts[1:] {
		if _, ok := c.entry(code, foldName(p)); ok {
			return "", false // a list of places: left as it is
		}
	}
	f := foldName(parts[0])
	g := f
	for _, sp := range [...][2]string{{"st ", "saint "}, {"ste ", "sainte "}, {"ft ", "fort "}, {"mt ", "mount "}} {
		if strings.HasPrefix(g, sp[0]) {
			g = sp[1] + g[len(sp[0]):]
		}
	}
	for _, t := range [...]string{f, g, dropTail(g)} {
		if name, ok := c.lookup(code, t, cut); ok {
			return name, true
		}
	}
	return "", false
}

// dropTail drops one trailing word "dc", "d c", "city", "district", "municipality" or a number.
func dropTail(s string) string {
	for _, w := range [...]string{" dc", " d c", " city", " district", " municipality"} {
		if strings.HasSuffix(s, w) {
			return strings.TrimSpace(s[:len(s)-len(w)])
		}
	}
	if i := strings.LastIndexByte(s, ' '); i >= 0 && i+1 < len(s) && strings.Trim(s[i+1:], "0123456789") == "" {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
