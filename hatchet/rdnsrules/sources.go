package rdnsrules

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"openipdata.org/hatchet/download"
)

const (
	airportsURL = "https://davidmegginson.github.io/ourairports-data/airports.csv"
	geoNamesURL = "https://download.geonames.org/export/dump/cities1000.zip"
	// UNECE's current official production release artifact; update on publication.
	locodeURL = "https://opensource.unicc.org/un/unece/uncefact/vocab-locode/-/jobs/artifacts/2025-1/download?job=package-release"
)

func fetchSource(ctx context.Context, dir, name, url string, client *http.Client, now time.Time) ([]byte, bool, error) {
	path := filepath.Join(dir, name)
	checkedPath := path + ".checked"
	if _, statErr := os.Stat(path); statErr == nil {
		if b, err := os.ReadFile(checkedPath); err == nil {
			if checked, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); parseErr == nil && now.Sub(checked) < 30*24*time.Hour {
				data, e := os.ReadFile(path)
				return data, false, e
			}
		}
	}
	// Stale cached bytes are revalidated with ETag/Last-Modified by download.File.
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	if _, err := download.File(ctx, client, url, path); err != nil {
		return nil, false, fmt.Errorf("dictionary source unavailable")
	}
	b, err := os.ReadFile(path)
	return b, true, err
}

func sourceNameToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
			continue
		}
		if r > 127 || (r >= '0' && r <= '9') {
			return ""
		}
		switch r {
		case ' ', '\t', '-', '.', '\'', ',', '’':
		default:
			return ""
		}
	}
	return b.String()
}

func addAmbiguous(m map[string]Place, ambiguous map[string]bool, token string, p Place) {
	token = sourceNameToken(token)
	if token == "" || p.Country == "" || p.City == "" || ambiguous[token] {
		return
	}
	if old, ok := m[token]; ok && old != p {
		delete(m, token)
		ambiguous[token] = true
		return
	}
	m[token] = p
}

func parseAirports(b []byte) (map[string]Place, error) {
	m, _, err := parseAirportsWithCount(b)
	return m, err
}

func parseAirportsWithCount(b []byte) (map[string]Place, int, error) {
	r := csv.NewReader(strings.NewReader(string(b)))
	r.FieldsPerRecord = -1
	head, err := r.Read()
	if err != nil {
		return nil, 0, fmt.Errorf("invalid airport dictionary")
	}
	idx := map[string]int{}
	for i, h := range head {
		idx[strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))] = i
	}
	get := func(row []string, k string) string {
		i, ok := idx[k]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	if _, ok := idx["iata_code"]; !ok {
		return nil, 0, fmt.Errorf("invalid airport dictionary")
	}
	for _, key := range []string{"type", "iso_country", "municipality"} {
		if _, ok := idx[key]; !ok {
			return nil, 0, fmt.Errorf("invalid airport dictionary")
		}
	}
	r.FieldsPerRecord = len(head)
	m, amb := map[string]Place{}, map[string]bool{}
	validRows := 0
	for {
		row, e := r.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, 0, fmt.Errorf("invalid airport dictionary")
		}
		validRows++
		typ := get(row, "type")
		if typ != "large_airport" && typ != "medium_airport" && typ != "small_airport" {
			continue
		}
		city := get(row, "municipality")
		code, country := strings.ToUpper(get(row, "iata_code")), strings.ToUpper(get(row, "iso_country"))
		if len(code) != 3 || len(country) != 2 || city == "" || !asciiLetters(code) || !asciiLetters(country) {
			continue
		}
		addAmbiguous(m, amb, code, Place{Country: country, City: city})
	}
	realCodes := len(m)
	// Small, explicit airline metro codes absent from individual airport rows.
	for code, p := range map[string]Place{"lon": {Country: "GB", City: "London"}, "nyc": {Country: "US", City: "New York"}, "par": {Country: "FR", City: "Paris"}, "tyo": {Country: "JP", City: "Tokyo"}, "sto": {Country: "SE", City: "Stockholm"}, "rom": {Country: "IT", City: "Rome"}, "mil": {Country: "IT", City: "Milan"}, "sao": {Country: "BR", City: "Sao Paulo"}} {
		m[code] = p
		delete(amb, code)
	}
	if validRows == 0 || realCodes == 0 {
		return nil, 0, fmt.Errorf("empty airport dictionary")
	}
	return m, realCodes, nil
}

func asciiLetters(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func parseGeoNames(b []byte) (map[string]Place, error) {
	m, _, err := parseGeoNamesWithAliases(b)
	return m, err
}

// dominance: a name shared by several places resolves to the most populous one
// only when it is at least this many times larger than the next.
const dominance = 10

type popPlace struct {
	place Place
	pop   int64
}

// popIndex collects the places of each key with their population (an identical
// place is kept once, with its largest population).
type popIndex[K comparable] map[K][]popPlace

func (x popIndex[K]) add(key K, p Place, pop int64) {
	list := x[key]
	for i := range list {
		if list[i].place == p {
			if pop > list[i].pop {
				list[i].pop = pop
			}
			return
		}
	}
	x[key] = append(list, popPlace{p, pop})
}

func (x popIndex[K]) resolve() map[K]Place {
	out := make(map[K]Place, len(x))
	for key, list := range x {
		if len(list) == 1 {
			out[key] = list[0].place
			continue
		}
		top, second := popPlace{pop: -1}, int64(-1)
		for _, c := range list {
			if c.pop > top.pop {
				second, top = top.pop, c
			} else if c.pop > second {
				second = c.pop
			}
		}
		if top.pop > 0 && top.pop >= dominance*second {
			out[key] = top.place
		}
	}
	return out
}

type cityAlias struct {
	country string
	token   string
}

func parseGeoNamesWithAliases(b []byte) (map[string]Place, map[cityAlias]Place, error) {
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, nil, fmt.Errorf("invalid GeoNames dictionary")
	}
	var f *zip.File
	for _, x := range z.File {
		if strings.HasSuffix(x.Name, "cities1000.txt") {
			f = x
			break
		}
	}
	if f == nil {
		return nil, nil, fmt.Errorf("invalid GeoNames dictionary")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid GeoNames dictionary")
	}
	defer rc.Close()
	global, byCountryPop := popIndex[string]{}, popIndex[cityAlias]{}
	buf, err := io.ReadAll(rc)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid GeoNames dictionary")
	}
	valid := 0
	for _, line := range strings.Split(string(buf), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if len(cols) < 19 {
			return nil, nil, fmt.Errorf("invalid GeoNames dictionary")
		}
		valid++
		country, city := strings.ToUpper(cols[8]), cols[2]
		if len(country) != 2 || city == "" {
			continue
		}
		place := Place{country, city}
		pop, _ := strconv.ParseInt(strings.TrimSpace(cols[14]), 10, 64)
		aliases := []string{cols[1], cols[2]}
		aliases = append(aliases, strings.Split(cols[3], ",")...)
		for _, alias := range aliases {
			token := sourceNameToken(alias)
			if token == "" {
				continue
			}
			global.add(token, place, pop)
			byCountryPop.add(cityAlias{country: country, token: token}, place, pop)
		}
	}
	m, byCountry := global.resolve(), byCountryPop.resolve()
	if valid == 0 || len(m) == 0 {
		return nil, nil, fmt.Errorf("empty GeoNames dictionary")
	}
	return m, byCountry, nil
}

func canonicalizePlaces(places map[string]Place, aliases map[cityAlias]Place) {
	for token, place := range places {
		if canonical, ok := aliases[cityAlias{country: place.Country, token: sourceNameToken(place.City)}]; ok {
			places[token] = canonical
		}
	}
}

func parseLOCODE(b []byte) (map[string]Place, error) {
	// UNECE's official release artifact is a zip with its CSV inside.
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("invalid UN/LOCODE dictionary")
	}
	m, amb := map[string]Place{}, map[string]bool{}
	parts := make([]*zip.File, 0, 3)
	for _, f := range z.File {
		base := strings.ToLower(filepath.Base(f.Name))
		if !strings.HasSuffix(base, ".csv") || !strings.Contains(base, "codelistpart") {
			continue
		}
		parts = append(parts, f)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("invalid UN/LOCODE dictionary")
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Name < parts[j].Name })
	validParts := 0
	for _, f := range parts {
		rc, e := f.Open()
		if e != nil {
			return nil, fmt.Errorf("invalid UN/LOCODE dictionary")
		}
		r := csv.NewReader(rc)
		r.FieldsPerRecord = -1
		head, e := r.Read()
		if e != nil {
			rc.Close()
			continue
		}
		idx := map[string]int{}
		for i, h := range head {
			idx[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
		}
		_, hasCountry := idx["country"]
		_, hasLocation := idx["location"]
		_, hasName := idx["name"]
		header := hasCountry && hasLocation && hasName && strings.EqualFold(strings.TrimSpace(head[idx["country"]]), "Country")
		if !header {
			idx = map[string]int{"country": 1, "location": 2, "name": 3, "name without diacritics": 4}
		}
		r.FieldsPerRecord = len(head)
		get := func(row []string, keys ...string) string {
			for _, k := range keys {
				if i, ok := idx[k]; ok && i < len(row) {
					return strings.TrimSpace(row[i])
				}
			}
			return ""
		}
		partRows := 0
		if !header {
			// The official parts have no header row: the first record is data.
			if err := addLOCODERow(head, get, m, amb); err != nil {
				rc.Close()
				return nil, err
			}
			partRows++
		}
		for {
			row, e := r.Read()
			if e == io.EOF {
				break
			}
			if e != nil {
				rc.Close()
				return nil, fmt.Errorf("invalid UN/LOCODE dictionary")
			}
			if err := addLOCODERow(row, get, m, amb); err != nil {
				rc.Close()
				return nil, err
			}
			partRows++
		}
		rc.Close()
		if partRows == 0 {
			return nil, fmt.Errorf("invalid UN/LOCODE dictionary")
		}
		validParts++
	}
	if validParts == len(parts) && len(m) > 0 {
		return m, nil
	}
	return nil, fmt.Errorf("invalid UN/LOCODE dictionary")
}

func addLOCODERow(row []string, get func([]string, ...string) string, m map[string]Place, amb map[string]bool) error {
	country := strings.ToUpper(get(row, "country", "country code"))
	loc := strings.ToLower(get(row, "locode", "location code", "location", "code"))
	city := get(row, "name without diacritics", "namewodiacritics", "name", "location")
	if loc == "" {
		return nil
	} // country marker row
	if len(country) != 2 || len(loc) != 3 || city == "" {
		return fmt.Errorf("invalid UN/LOCODE dictionary")
	}
	addAmbiguous(m, amb, country+loc, Place{country, city})
	return nil
}

func loadDictionaries(ctx context.Context, dir string, client *http.Client, now time.Time) (Dictionaries, error) {
	return loadDictionariesWithMinimum(ctx, dir, client, now, 1000, 10000, 1000)
}

func loadDictionariesWithMinimum(ctx context.Context, dir string, client *http.Client, now time.Time, minAirports, minGeoNames, minLOCODE int) (Dictionaries, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create dictionary cache")
	}
	air, airFresh, err := fetchSource(ctx, dir, "airports.csv", airportsURL, client, now)
	if err != nil {
		return nil, err
	}
	geo, geoFresh, err := fetchSource(ctx, dir, "cities1000.zip", geoNamesURL, client, now)
	if err != nil {
		return nil, err
	}
	loc, locFresh, err := fetchSource(ctx, dir, "unlocode.zip", locodeURL, client, now)
	if err != nil {
		return nil, err
	}
	i, airportCodes, err := parseAirportsWithCount(air)
	if err != nil {
		_ = os.Remove(filepath.Join(dir, "airports.csv"))
		_ = os.Remove(filepath.Join(dir, "airports.csv.etag"))
		return nil, err
	}
	g, aliases, err := parseGeoNamesWithAliases(geo)
	if err != nil {
		_ = os.Remove(filepath.Join(dir, "cities1000.zip"))
		_ = os.Remove(filepath.Join(dir, "cities1000.zip.etag"))
		return nil, err
	}
	l, err := parseLOCODE(loc)
	if err != nil {
		_ = os.Remove(filepath.Join(dir, "unlocode.zip"))
		_ = os.Remove(filepath.Join(dir, "unlocode.zip.etag"))
		return nil, err
	}
	canonicalizePlaces(i, aliases)
	canonicalizePlaces(l, aliases)
	if airportCodes < minAirports || len(g) < minGeoNames || len(l) < minLOCODE {
		return nil, fmt.Errorf("incomplete public dictionary")
	}
	for _, source := range []struct {
		name  string
		fresh bool
	}{{"airports.csv", airFresh}, {"cities1000.zip", geoFresh}, {"unlocode.zip", locFresh}} {
		if !source.fresh {
			continue
		}
		name := source.name
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p+".checked.tmp", []byte(now.UTC().Format(time.RFC3339Nano)), 0o640); err != nil {
			return nil, fmt.Errorf("dictionary cache unavailable")
		}
		if err := os.Rename(p+".checked.tmp", p+".checked"); err != nil {
			return nil, fmt.Errorf("dictionary cache unavailable")
		}
	}
	return Dictionaries{"iata": i, "geonames": g, "locode": l}, nil
}
