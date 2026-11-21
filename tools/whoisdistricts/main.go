// Command whoisdistricts makes hatchet/geofeeds/whois-districts.txt: the names of city
// districts (GeoNames feature code PPLX) that lie within districtKm of a big city
// (districtCityPop people or more) of the same country and that CLASH with the name of
// a small place (fewer than smallPop people) of that country in cities1000.
//
// The whois text layer (37-whois-text) never names a district, so "descr: DALSTON"
// (Dalston in London) became Dalston in Cumbria, a village of 1,285 people. The worker
// drops such a match when the place it chose is small, is not itself inside a big city
// and its matched name is in this list. See the README. Standard library only; run by
// hand, never by the worker.
package main

import (
	"archive/zip"
	"bufio"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	districtCityPop = 100_000 // a district counts when a place this big is near
	districtKm      = 30.0
	smallPop        = 15_000 // the clashing place must be smaller than this
	earthKm         = 6371.0088
	maxLine         = 8 << 20
)

// skipFeature is the list readPlaces of the worker skips (hatchet/geofeeds/whoisgeo.go).
var skipFeature = map[string]bool{"PPLX": true, "PPLH": true, "PPLQ": true, "PPLW": true, "PPLCH": true}

type point struct{ lat, lon float64 }

func distanceKm(a, b point) float64 {
	const rad = math.Pi / 180
	dLat, dLon := (b.lat-a.lat)*rad, (b.lon-a.lon)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(a.lat*rad)*math.Cos(b.lat*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthKm * math.Asin(math.Min(1, math.Sqrt(h)))
}

// fold is the loose key of a name: lower case, a few accents removed, "st" as "saint",
// letters a-z only (no spaces). It is over-inclusive on purpose: the worker checks the
// names again with its own normalisation.
func fold(s string) string {
	var b strings.Builder
	var word []byte
	flush := func() {
		w := string(word)
		if w == "st" {
			w = "saint"
		}
		b.WriteString(w)
		word = word[:0]
	}
	for _, r := range strings.ToLower(s) {
		if f, ok := foldMap[r]; ok {
			word = append(word, f...)
		} else if r >= 'a' && r <= 'z' {
			word = append(word, byte(r))
		} else if unicode.Is(unicode.Mn, r) {
			// a combining mark: the letter before it stays
		} else {
			flush()
		}
	}
	flush()
	return b.String()
}

var foldMap = func() map[rune]string {
	out := map[rune]string{'ß': "ss", 'æ': "ae", 'œ': "oe", 'þ': "th", 'ĳ': "ij"}
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

// scanZip calls fn with the tab-separated fields of every line of the file inside the zip.
func scanZip(path, inner string, fn func(f []string)) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != inner {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return scanLines(rc, fn)
	}
	return fmt.Errorf("%s is not in %s", inner, path)
}

func scanLines(r io.Reader, fn func(f []string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		fn(strings.Split(sc.Text(), "\t"))
	}
	return sc.Err()
}

// parseCoords reads columns 4 and 5.
func parseCoords(f []string) (point, bool) {
	lat, e1 := strconv.ParseFloat(f[4], 64)
	lon, e2 := strconv.ParseFloat(f[5], 64)
	if e1 != nil || e2 != nil || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
		return point{}, false
	}
	return point{lat, lon}, true
}

// readCities returns the big places and the loose keys of the small places per country,
// from cities1000.txt (columns as in readPlaces of the worker).
func readCities(path string) (big map[string][]point, small map[string]map[string]bool, err error) {
	big, small = map[string][]point{}, map[string]map[string]bool{}
	err = scanZip(path, "cities1000.txt", func(f []string) {
		if len(f) < 15 || len(f[8]) != 2 || skipFeature[f[7]] || f[1] == "" {
			return
		}
		p, ok := parseCoords(f)
		if !ok {
			return
		}
		pop, _ := strconv.Atoi(f[14])
		cc := f[8]
		switch {
		case pop >= districtCityPop:
			big[cc] = append(big[cc], p)
		case pop < smallPop:
			if small[cc] == nil {
				small[cc] = map[string]bool{}
			}
			small[cc][fold(f[1])] = true
			small[cc][fold(f[2])] = true
		}
	})
	return big, small, err
}

// districts streams allCountries.txt and returns the sorted, distinct "CC\tname" entries.
func districts(allPath string, big map[string][]point, small map[string]map[string]bool) ([]string, error) {
	set := map[string]bool{}
	err := scanZip(allPath, "allCountries.txt", func(f []string) {
		if len(f) < 15 || f[7] != "PPLX" || len(f[8]) != 2 {
			return
		}
		cc := f[8]
		names := small[cc]
		if names == nil || big[cc] == nil {
			return
		}
		clash := false
		for _, n := range []string{f[1], f[2]} {
			if n != "" && names[fold(n)] {
				clash = true
			}
		}
		if !clash {
			return
		}
		p, ok := parseCoords(f)
		if !ok {
			return
		}
		near := false
		for _, b := range big[cc] {
			if distanceKm(p, b) <= districtKm {
				near = true
				break
			}
		}
		if !near {
			return
		}
		for _, n := range []string{f[1], f[2]} {
			if n != "" && names[fold(n)] {
				set[cc+"\t"+n] = true
			}
		}
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out, nil
}

func main() {
	all := flag.String("all", "", "GeoNames allCountries.zip")
	cities := flag.String("cities", "", "GeoNames cities1000.zip")
	out := flag.String("out", "", "output file")
	date := flag.String("date", "", "download date of the GeoNames files, YYYY-MM-DD (default: the modified date of -cities)")
	flag.Parse()
	if *all == "" || *cities == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *date == "" {
		info, err := os.Stat(*cities)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		*date = info.ModTime().UTC().Format(time.DateOnly)
	}
	big, small, err := readCities(*cities)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cities:", err)
		os.Exit(1)
	}
	entries, err := districts(*all, big, small)
	if err != nil {
		fmt.Fprintln(os.Stderr, "all:", err)
		os.Exit(1)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# City district names that clash with the name of a small place of the same country.
# Used by the whois text layer (hatchet/geofeeds/whoistext.go): a text match on a place
# of fewer than 15,000 people outside any big city is dropped when its name is on this
# list, because the text most likely means the district ("DALSTON" is Dalston in
# London, not the village in Cumbria).
# A line is: country code, a tab, a GeoNames name or asciiname of a district.
# Districts: GeoNames feature code PPLX within %.0f km of a place of %d or more people
# of the same country; clash: a place of fewer than %d people in cities1000 has the same
# name or asciiname (letters only, accents folded).
# Source: GeoNames allCountries and cities1000, downloaded %s, CC BY 4.0
# (https://www.geonames.org/). Made by tools/whoisdistricts:
#   go run . -all allCountries.zip -cities cities1000.zip -out ../../hatchet/geofeeds/whois-districts.txt
`, districtKm, districtCityPop, smallPop, *date)
	for _, e := range entries {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	tmp := *out + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%d entries, %d bytes\n", len(entries), b.Len())
}
