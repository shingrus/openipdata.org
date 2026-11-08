package pdb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/idna"
)

// cityThreshold is the minimum share of presence points the top city needs.
const cityThreshold = 0.3

// GeoRow is one asn_geo_pdb row.
type GeoRow struct {
	ASN       int64
	Country   *string
	City      *string
	Dominance float64
	Domain    *string
}

type orgRow struct {
	ID      int64  `json:"id"`
	City    string `json:"city"`
	Country string `json:"country"`
	Website string `json:"website"`
}

type netRow struct {
	ID      int64  `json:"id"`
	ASN     int64  `json:"asn"`
	OrgID   int64  `json:"org_id"`
	Website string `json:"website"`
}

type placeRow struct {
	ID      int64  `json:"id"`
	City    string `json:"city"`
	Country string `json:"country"`
}

type linkRow struct {
	NetID int64 `json:"net_id"`
	FacID int64 `json:"fac_id"`
	IXID  int64 `json:"ix_id"`
}

type place struct{ country, city string }

// Compute derives one row per ASN from the downloaded datasets in dir:
//   - every facility (netfac) and exchange (netixlan) link is one presence point;
//   - country is the most common country among located points, else the org's HQ country;
//   - city is the most common city within that country, if it holds at least
//     cityThreshold of all located points (its share is the dominance);
//   - domain is the host of the network's website, else the org's website.
//
// Ties go to the value seen first, in file order.
func Compute(dir string) ([]GeoRow, error) {
	orgs := map[int64]orgRow{}
	if err := readJSONL(datasetPath(dir, "org"), func(o orgRow) { orgs[o.ID] = o }); err != nil {
		return nil, err
	}
	places := map[string]map[int64]place{"fac": {}, "ix": {}}
	for name, byID := range places {
		if err := readJSONL(datasetPath(dir, name), func(p placeRow) {
			byID[p.ID] = place{country: strings.TrimSpace(p.Country), city: normCity(p.City)}
		}); err != nil {
			return nil, err
		}
	}
	var nets []netRow
	if err := readJSONL(datasetPath(dir, "net"), func(n netRow) {
		if n.ID > 0 && n.ASN > 0 {
			nets = append(nets, n)
		}
	}); err != nil {
		return nil, err
	}
	netFacs := map[int64][]int64{}
	if err := readJSONL(datasetPath(dir, "netfac"), func(l linkRow) {
		netFacs[l.NetID] = append(netFacs[l.NetID], l.FacID)
	}); err != nil {
		return nil, err
	}
	netIXs := map[int64][]int64{}
	if err := readJSONL(datasetPath(dir, "netixlan"), func(l linkRow) {
		netIXs[l.NetID] = append(netIXs[l.NetID], l.IXID)
	}); err != nil {
		return nil, err
	}

	rows := make([]GeoRow, 0, len(nets))
	seen := map[int64]bool{}
	for _, n := range nets {
		if seen[n.ASN] {
			continue
		}
		seen[n.ASN] = true
		org := orgs[n.OrgID]

		var points []place
		for _, id := range netFacs[n.ID] {
			points = append(points, places["fac"][id])
		}
		for _, id := range netIXs[n.ID] {
			points = append(points, places["ix"][id])
		}
		country, city, dominance := bestPlace(points, strings.TrimSpace(org.Country))

		domain := domainFromWebsite(n.Website)
		if domain == "" {
			domain = domainFromWebsite(org.Website)
		}
		rows = append(rows, GeoRow{
			ASN:       n.ASN,
			Country:   optional(country),
			City:      optional(city),
			Dominance: dominance,
			Domain:    optional(domain),
		})
	}
	return rows, nil
}

// bestPlace returns the country, the city within it, and the city's share of
// all located points (points with a known country).
func bestPlace(points []place, hqCountry string) (string, string, float64) {
	var countries []string
	for _, p := range points {
		if p.country != "" {
			countries = append(countries, p.country)
		}
	}
	if len(countries) == 0 {
		return hqCountry, "", 0
	}
	country, _ := mostCommon(countries)
	var cities []string
	for _, p := range points {
		if p.country == country && p.city != "" {
			cities = append(cities, p.city)
		}
	}
	if len(cities) == 0 {
		return country, "", 0
	}
	city, count := mostCommon(cities)
	share := float64(count) / float64(len(countries))
	if share < cityThreshold {
		return country, "", share
	}
	return country, city, share
}

// mostCommon returns the most frequent value; ties go to the value seen first.
func mostCommon[T comparable](values []T) (T, int) {
	counts := map[T]int{}
	var best T
	bestCount := 0
	for _, v := range values {
		counts[v]++
	}
	for _, v := range values {
		if counts[v] > bestCount {
			best, bestCount = v, counts[v]
		}
	}
	return best, bestCount
}

func normCity(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// domainFromWebsite returns the lowercased, IDNA-encoded host of a website URL,
// or "" when the host is not a valid domain name.
func domainFromWebsite(website string) string {
	raw := strings.TrimSpace(website)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.Trim(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return ""
	}
	encoded, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return ""
	}
	return encoded
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// readJSONL decodes each line of path into a T and passes it to fn.
func readJSONL[T any](path string, fn func(T)) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if len(strings.TrimSpace(scanner.Text())) == 0 {
			continue
		}
		var row T
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
		fn(row)
	}
	return scanner.Err()
}
