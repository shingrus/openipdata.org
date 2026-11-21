package main

// mmdbbuilder convert: the flat ip2geo MMDB (country_code, country_name,
// city_name, city_source, latitude, longitude) becomes a file in the MaxMind
// GeoLite2-City record layout (continent, country, city, location), English
// names only. A key is written only when it is known, never as an empty string
// or a zero placeholder. The input's metadata description (it holds the score
// stamp) and build epoch are carried over, so the same input and GeoNames file
// give the same bytes.

import (
	"bufio"
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang/v2"
)

const compatDatabaseType = "OpenIPData-City"

// countriesTSV is made once from GeoNames countryInfo.txt (CC BY 4.0): code, GeoNames id, continent.
//
//go:embed countries.tsv
var countriesTSV []byte

type countryInfo struct {
	id        uint32
	continent string
}

// continents are the GeoNames ids and the English names of the continent codes.
var continents = map[string]struct {
	id   uint32
	name string
}{
	"AF": {6255146, "Africa"}, "AS": {6255147, "Asia"}, "EU": {6255148, "Europe"},
	"NA": {6255149, "North America"}, "SA": {6255150, "South America"},
	"OC": {6255151, "Oceania"}, "AN": {6255152, "Antarctica"},
}

// euMembers are the 27 member states of the European Union.
var euMembers = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields("AT BE BG HR CY CZ DK EE FI FR DE GR HU IE IT LV LT LU MT NL PL PT RO SK SI ES SE") {
		m[c] = true
	}
	return m
}()

// parseCountries reads the table: lines of code<TAB>geoname_id<TAB>continent,
// "#" lines and empty lines ignored. A malformed line is an error.
func parseCountries(data []byte) (map[string]countryInfo, error) {
	out := map[string]countryInfo{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 || normCountry(f[0]) != f[0] {
			return nil, fmt.Errorf("countries.tsv line %d: malformed", n)
		}
		id, err := strconv.ParseUint(f[1], 10, 32)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("countries.tsv line %d: bad id", n)
		}
		if _, ok := continents[f[2]]; !ok {
			return nil, fmt.Errorf("countries.tsv line %d: bad continent", n)
		}
		if _, dup := out[f[0]]; dup {
			return nil, fmt.Errorf("countries.tsv line %d: repeated code", n)
		}
		out[f[0]] = countryInfo{uint32(id), f[2]}
	}
	return out, sc.Err()
}

// flatRecord is a record of the flat ip2geo file.
type flatRecord struct {
	CountryCode string   `maxminddb:"country_code"`
	CountryName string   `maxminddb:"country_name"`
	CityName    string   `maxminddb:"city_name"`
	CitySource  string   `maxminddb:"city_source"`
	Latitude    *float64 `maxminddb:"latitude"`
	Longitude   *float64 `maxminddb:"longitude"`
}

// convertStats are the counts printed at the end (networks, never addresses).
type convertStats struct {
	networks, withCity, placed, withTZ int
}

type convertKey struct {
	code, name, city, source string
	lat, lon                 float64
	hasLoc                   bool
}

// converter makes the new records; the same flat record gives the same map.
type converter struct {
	countries map[string]countryInfo
	index     geoNames // nil: no ids and no time zones
	cache     map[convertKey]mmdbtype.Map
	stats     convertStats
}

func enNames(en string) mmdbtype.Map {
	return mmdbtype.Map{"en": mmdbtype.String(en)}
}

// record converts one flat record; it returns nil for a record with nothing to write.
func (c *converter) record(r flatRecord) mmdbtype.Map {
	key := convertKey{r.CountryCode, r.CountryName, r.CityName, r.CitySource, 0, 0, false}
	if r.Latitude != nil && r.Longitude != nil {
		key.lat, key.lon, key.hasLoc = *r.Latitude, *r.Longitude, true
	}
	var rec mmdbtype.Map
	if cached, ok := c.cache[key]; ok {
		rec = cached
	} else {
		rec = c.build(key)
		c.cache[key] = rec
	}
	if rec == nil {
		return nil
	}
	c.stats.networks++
	if key.city != "" {
		c.stats.withCity++
		if city, ok := rec["city"].(mmdbtype.Map); ok {
			if _, placed := city["geoname_id"]; placed {
				c.stats.placed++
			}
		}
		if loc, ok := rec["location"].(mmdbtype.Map); ok {
			if _, tz := loc["time_zone"]; tz {
				c.stats.withTZ++
			}
		}
	}
	return rec
}

func (c *converter) build(k convertKey) mmdbtype.Map {
	rec := mmdbtype.Map{}
	code := normCountry(k.code)
	if code != "" {
		country := mmdbtype.Map{"iso_code": mmdbtype.String(code)}
		if info, ok := c.countries[code]; ok {
			country["geoname_id"] = mmdbtype.Uint32(info.id)
			cont := continents[info.continent]
			rec["continent"] = mmdbtype.Map{
				"code":       mmdbtype.String(info.continent),
				"geoname_id": mmdbtype.Uint32(cont.id),
				"names":      enNames(cont.name),
			}
		}
		if k.name != "" {
			country["names"] = enNames(k.name)
		}
		if euMembers[code] {
			country["is_in_european_union"] = mmdbtype.Bool(true)
		}
		rec["country"] = country
	}
	var tz string
	if k.city != "" {
		city := mmdbtype.Map{"names": enNames(k.city)}
		// The place's id and time zone are used only when its coordinates, rounded
		// as the builder rounds them, are the record's.
		if k.hasLoc && c.index != nil {
			if p, ok := c.index.place(code, k.city); ok && round4(p.lat) == k.lat && round4(p.lon) == k.lon {
				city["geoname_id"] = mmdbtype.Uint32(p.id)
				tz = p.tz
			}
		}
		rec["city"] = city
	}
	if k.hasLoc {
		loc := mmdbtype.Map{"latitude": mmdbtype.Float64(k.lat), "longitude": mmdbtype.Float64(k.lon)}
		if tz != "" {
			loc["time_zone"] = mmdbtype.String(tz)
		}
		rec["location"] = loc
	}
	if k.source != "" {
		rec["city_source"] = mmdbtype.String(k.source)
	}
	if len(rec) == 0 {
		return nil
	}
	return rec
}

// convertMMDB writes the GeoLite2-layout file for the flat file in. geonamesPath
// may be empty. The output is written to out+".tmp" and renamed.
func convertMMDB(in, out, geonamesPath string) (stats convertStats, err error) {
	countries, err := parseCountries(countriesTSV)
	if err != nil {
		return stats, err
	}
	var index geoNames
	if geonamesPath != "" {
		if index, err = loadGeoNames(geonamesPath); err != nil {
			return stats, fmt.Errorf("geonames %s: %w", geonamesPath, err)
		}
	}
	reader, err := maxminddb.Open(in)
	if err != nil {
		return stats, err
	}
	defer reader.Close()
	m := reader.Metadata
	if m.IPVersion != 6 {
		return stats, errors.New("input is not an IPv6 MMDB")
	}
	writer, err := mmdbwriter.New(mmdbwriter.Options{
		BuildEpoch:              int64(m.BuildEpoch),
		DatabaseType:            compatDatabaseType,
		Description:             m.Description,
		Languages:               []string{"en"},
		RecordSize:              28,
		IPVersion:               6,
		IncludeReservedNetworks: true,
		DisableIPv4Aliasing:     true,
	})
	if err != nil {
		return stats, err
	}
	conv := &converter{countries: countries, index: index, cache: map[convertKey]mmdbtype.Map{}}
	insert := func(p netip.Prefix, res maxminddb.Result) error {
		var r flatRecord
		if err := res.Decode(&r); err != nil {
			return err
		}
		rec := conv.record(r)
		if rec == nil {
			return nil
		}
		return writer.Insert(ipNet(p), rec)
	}
	// IPv4 comes back as ::a.b.c.d (prefix length counted in 128 bits) from the whole
	// tree; the IPv4 walk gives plain IPv4 prefixes, the same ones geo inserted.
	for res := range reader.NetworksWithin(allIPv4) {
		if err := res.Err(); err != nil {
			return stats, err
		}
		p := res.Prefix()
		if a := p.Addr().Unmap(); !a.Is4() {
			continue
		} else if a != p.Addr() {
			p = netip.PrefixFrom(a, p.Bits()-96)
		}
		if err := insert(p, res); err != nil {
			return stats, err
		}
	}
	for res := range reader.Networks() {
		if err := res.Err(); err != nil {
			return stats, err
		}
		p := res.Prefix()
		if p.Addr().Is4() || (p.Bits() >= 96 && v4Compat.Contains(p.Addr())) || v4Mapped.Contains(p.Addr()) {
			continue // the IPv4 part, done above
		}
		if err := insert(p, res); err != nil {
			return stats, err
		}
	}
	tmp := out + ".tmp"
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	f, err := os.Create(tmp)
	if err != nil {
		return stats, err
	}
	if _, err = writer.WriteTo(f); err != nil {
		f.Close()
		return stats, err
	}
	if err = f.Chmod(0o644); err != nil {
		f.Close()
		return stats, err
	}
	if err = f.Close(); err != nil {
		return stats, err
	}
	if err = os.Rename(tmp, out); err != nil {
		return stats, err
	}
	return conv.stats, nil
}

var (
	v4Compat = netip.MustParsePrefix("::/96")
	v4Mapped = netip.MustParsePrefix("::ffff:0:0/96")
)

func runConvert(args []string) {
	flags := flag.NewFlagSet("convert", flag.ExitOnError)
	db := flags.String("db", "", "input flat ip2geo MMDB")
	out := flags.String("out", "", "output MMDB in the GeoLite2-City layout")
	geonames := flags.String("geonames", "", "GeoNames cities1000.txt (adds geoname_id and time_zone of placed cities)")
	flags.Parse(args)
	if *db == "" || *out == "" || flags.NArg() > 0 {
		usage()
	}
	s, err := convertMMDB(*db, *out, *geonames)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("convert: %d networks, %d with a city, %d placed, %d with a time zone\n", s.networks, s.withCity, s.placed, s.withTZ)
}
