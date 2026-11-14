package rdnsrules

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
	"unicode"
)

const admin1URL = "https://download.geonames.org/export/dump/admin1CodesASCII.txt"

// geoPlace is one row of GeoNames cities1000 (places of at least 1,000 people).
type geoPlace struct {
	ID       int64
	Name     string // GeoNames' ASCII name: the city name the rules store
	Country  string
	Admin1   string // first-level subdivision code
	Pop      int64
	Lat, Lon float64
}

// geoIndex finds the one GeoNames place of a (country, region, city) answer.
type geoIndex struct {
	places    []geoPlace
	byName    map[string][]int32  // country + "\x00" + folded name (name, ASCII name or alternate name)
	admin1    map[string][]string // "US.NY" -> folded names of the subdivision
	countries map[string]bool
	byID      map[int64]int32
}

// foldName lower-cases, drops accents and turns every other run of characters into one space.
func foldName(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("æ", "ae", "œ", "oe", "ø", "o", "ł", "l", "ß", "ss", "đ", "d", "ð", "d", "þ", "th", "ı", "i", "ħ", "h").Replace(s)
	var b strings.Builder
	space := true
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func geoKey(country, folded string) string { return country + "\x00" + folded }

// parseGeoIndex reads cities1000.zip and admin1CodesASCII.txt (admin1 may be nil: a region
// then never matches and the namesake rule decides).
func parseGeoIndex(citiesZip, admin1 []byte) (*geoIndex, error) {
	z, err := zip.NewReader(bytes.NewReader(citiesZip), int64(len(citiesZip)))
	if err != nil {
		return nil, fmt.Errorf("invalid GeoNames dictionary")
	}
	var f *zip.File
	for _, x := range z.File {
		if strings.HasSuffix(x.Name, "cities1000.txt") {
			f = x
			break
		}
	}
	if f == nil {
		return nil, fmt.Errorf("invalid GeoNames dictionary")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("invalid GeoNames dictionary")
	}
	defer rc.Close()
	buf, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("invalid GeoNames dictionary")
	}
	g := &geoIndex{byName: map[string][]int32{}, admin1: map[string][]string{}, countries: map[string]bool{}, byID: map[int64]int32{}}
	for _, line := range strings.Split(string(buf), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if len(cols) < 19 {
			return nil, fmt.Errorf("invalid GeoNames dictionary")
		}
		country := strings.ToUpper(cols[8])
		if len(country) != 2 || cols[2] == "" {
			continue
		}
		id, _ := strconv.ParseInt(cols[0], 10, 64)
		pop, _ := strconv.ParseInt(strings.TrimSpace(cols[14]), 10, 64)
		lat, _ := strconv.ParseFloat(cols[4], 64)
		lon, _ := strconv.ParseFloat(cols[5], 64)
		idx := int32(len(g.places))
		g.places = append(g.places, geoPlace{ID: id, Name: cols[2], Country: country, Admin1: cols[10], Pop: pop, Lat: lat, Lon: lon})
		g.countries[country] = true
		g.byID[id] = idx
		seen := map[string]bool{}
		names := append([]string{cols[1], cols[2]}, strings.Split(cols[3], ",")...)
		for _, n := range names {
			k := foldName(n)
			if len(k) < 2 || seen[k] {
				continue
			}
			seen[k] = true
			g.byName[geoKey(country, k)] = append(g.byName[geoKey(country, k)], idx)
		}
	}
	if len(g.places) == 0 {
		return nil, fmt.Errorf("empty GeoNames dictionary")
	}
	for _, line := range strings.Split(string(admin1), "\n") {
		cols := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if len(cols) < 2 || cols[0] == "" {
			continue
		}
		for _, n := range cols[1:min(len(cols), 3)] {
			if k := foldName(n); k != "" {
				g.admin1[cols[0]] = append(g.admin1[cols[0]], k)
			}
		}
	}
	return g, nil
}

// Resolution failures.
const (
	geoUnknownCountry = "unknown_country"
	geoUnknownCity    = "unknown_city"
	geoAmbiguousCity  = "ambiguous_city"
)

// resolve returns the one GeoNames place of an answer: the places of the country with that
// name (or alternate name); when the region names the first-level subdivision of some of them
// only those count. Several places left: the most populous wins only with dominance times the
// population of the next (the learner's namesake rule), else the answer is ambiguous.
func (g *geoIndex) resolve(country, region, city string) (*geoPlace, string) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if !g.countries[country] {
		return nil, geoUnknownCountry
	}
	ids := g.byName[geoKey(country, foldName(city))]
	if len(ids) == 0 {
		return nil, geoUnknownCity
	}
	cands := make([]*geoPlace, 0, len(ids))
	for _, i := range ids {
		cands = append(cands, &g.places[i])
	}
	if rk := foldName(region); rk != "" {
		var inRegion []*geoPlace
		for _, p := range cands {
			if g.inRegion(p, rk) {
				inRegion = append(inRegion, p)
			}
		}
		if len(inRegion) > 0 {
			cands = inRegion
		}
	}
	if len(cands) == 1 {
		return cands[0], ""
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Pop != cands[j].Pop {
			return cands[i].Pop > cands[j].Pop
		}
		return cands[i].ID < cands[j].ID
	})
	if cands[0].Pop > 0 && cands[0].Pop >= dominance*cands[1].Pop {
		return cands[0], ""
	}
	return nil, geoAmbiguousCity
}

// inRegion reports whether the folded region is the name or the code of p's subdivision.
func (g *geoIndex) inRegion(p *geoPlace, region string) bool {
	if strings.EqualFold(region, p.Admin1) {
		return true
	}
	for _, n := range g.admin1[p.Country+"."+p.Admin1] {
		if n == region {
			return true
		}
	}
	return false
}

// haversineKm is the great-circle distance in kilometres.
func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const earthKm = 6371.0088
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthKm * math.Asin(math.Min(1, math.Sqrt(a)))
}

// sameArea reports the same country and at most maxKm kilometres apart.
func sameArea(a, b *geoPlace, maxKm float64) bool {
	return a != nil && b != nil && a.Country == b.Country && haversineKm(a.Lat, a.Lon, b.Lat, b.Lon) <= maxKm
}

// loadGeoIndex reads GeoNames cities1000 and the admin1 names from the learner's monthly
// dictionary cache (the same directory and the same 30-day freshness).
func loadGeoIndex(ctx context.Context, dir string, client *http.Client, now time.Time) (*geoIndex, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create dictionary cache")
	}
	cities, citiesFresh, err := fetchSource(ctx, dir, "cities1000.zip", geoNamesURL, client, now)
	if err != nil {
		return nil, err
	}
	admin, adminFresh, err := fetchSource(ctx, dir, "admin1CodesASCII.txt", admin1URL, client, now)
	if err != nil {
		return nil, err
	}
	g, err := parseGeoIndex(cities, admin)
	if err != nil {
		_ = os.Remove(filepath.Join(dir, "cities1000.zip"))
		_ = os.Remove(filepath.Join(dir, "cities1000.zip.etag"))
		return nil, err
	}
	for name, fresh := range map[string]bool{"cities1000.zip": citiesFresh, "admin1CodesASCII.txt": adminFresh} {
		if !fresh {
			continue
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p+".checked.tmp", []byte(now.UTC().Format(time.RFC3339Nano)), 0o640); err != nil {
			return nil, fmt.Errorf("dictionary cache unavailable")
		}
		if err := os.Rename(p+".checked.tmp", p+".checked"); err != nil {
			return nil, fmt.Errorf("dictionary cache unavailable")
		}
	}
	return g, nil
}
