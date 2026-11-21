package main

// mmdbbuilder convert: the flat ip2asn MMDB becomes the GeoLite2-ASN layout (see
// convertASN), and the flat ip2geo MMDB (country_code, country_name,
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
	withOrg, skipped                   int // ASN conversion
	asn                                bool
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

// convertMMDB writes the compat-layout file for the flat file in: the City layout
// for an ip2geo input, the ASN layout for an ip2asn input (decided by the input's
// database_type, anything else is an error). geonamesPath may be empty and is an
// error for an ip2asn input. The output is written to out+".tmp" and renamed.
func convertMMDB(in, out, geonamesPath string) (stats convertStats, err error) {
	reader, err := maxminddb.Open(in)
	if err != nil {
		return stats, err
	}
	defer reader.Close()
	switch t := reader.Metadata.DatabaseType; t {
	case "ip2geo":
		return convertGeo(reader, out, geonamesPath)
	case "ip2asn":
		if geonamesPath != "" {
			return stats, errors.New("-geonames means nothing for an ip2asn input")
		}
		return convertASN(reader, out)
	default:
		return stats, fmt.Errorf("input database_type %q is not ip2geo or ip2asn", t)
	}
}

func convertGeo(reader *maxminddb.Reader, out, geonamesPath string) (convertStats, error) {
	countries, err := parseCountries(countriesTSV)
	if err != nil {
		return convertStats{}, err
	}
	var index geoNames
	if geonamesPath != "" {
		if index, err = loadGeoNames(geonamesPath); err != nil {
			return convertStats{}, fmt.Errorf("geonames %s: %w", geonamesPath, err)
		}
	}
	conv := &converter{countries: countries, index: index, cache: map[convertKey]mmdbtype.Map{}}
	err = convertFile(reader, out, compatDatabaseType, func(res maxminddb.Result) (mmdbtype.Map, error) {
		var r flatRecord
		if err := res.Decode(&r); err != nil {
			return nil, err
		}
		return conv.record(r), nil
	})
	return conv.stats, err
}

// convertFile is the walk both conversions share: every network of the flat file is
// given to rec, which returns the new record (nil: write nothing), and the result
// goes to out through out+".tmp". Metadata description and build epoch are carried over.
func convertFile(reader *maxminddb.Reader, out, dbType string, rec func(maxminddb.Result) (mmdbtype.Map, error)) (err error) {
	m := reader.Metadata
	if m.IPVersion != 6 {
		return errors.New("input is not an IPv6 MMDB")
	}
	writer, err := mmdbwriter.New(mmdbwriter.Options{
		BuildEpoch:              int64(m.BuildEpoch),
		DatabaseType:            dbType,
		Description:             m.Description,
		Languages:               []string{"en"},
		RecordSize:              28,
		IPVersion:               6,
		IncludeReservedNetworks: true,
		DisableIPv4Aliasing:     true,
	})
	if err != nil {
		return err
	}
	insert := func(p netip.Prefix, res maxminddb.Result) error {
		r, err := rec(res)
		if err != nil || r == nil {
			return err
		}
		return writer.Insert(ipNet(p), r)
	}
	// IPv4 comes back as ::a.b.c.d (prefix length counted in 128 bits) from the whole
	// tree; the IPv4 walk gives plain IPv4 prefixes, the same ones geo inserted.
	for res := range reader.NetworksWithin(allIPv4) {
		if err := res.Err(); err != nil {
			return err
		}
		p := res.Prefix()
		if a := p.Addr().Unmap(); !a.Is4() {
			continue
		} else if a != p.Addr() {
			p = netip.PrefixFrom(a, p.Bits()-96)
		}
		if err := insert(p, res); err != nil {
			return err
		}
	}
	for res := range reader.Networks() {
		if err := res.Err(); err != nil {
			return err
		}
		p := res.Prefix()
		if p.Addr().Is4() || (p.Bits() >= 96 && v4Compat.Contains(p.Addr())) || v4Mapped.Contains(p.Addr()) {
			continue // the IPv4 part, done above
		}
		if err := insert(p, res); err != nil {
			return err
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
		return err
	}
	if _, err = writer.WriteTo(f); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, out)
}

// asnCompatType: MaxMind's Python library accepts an ASN file only when its type
// contains "GeoLite2-ASN" (DB-IP's ASN file does the same).
const asnCompatType = "OpenIPData-ASN (compat=GeoLite2-ASN)"

// flatASN is a record of the flat ip2asn file.
type flatASN struct {
	ASN         string `maxminddb:"asn"`
	Name        string `maxminddb:"name"`
	Org         string `maxminddb:"org"`
	CountryCode string `maxminddb:"country_code"`
}

// convertASN writes the GeoLite2-ASN layout: autonomous_system_number and
// autonomous_system_organization (the flat org, else the flat name), plus two
// extra keys under their old flat names, each only when known: name (the
// network's short handle, e.g. GOOGLE) and country_code (where the network is
// registered). A record with no valid number is not written and counted in skipped.
func convertASN(reader *maxminddb.Reader, out string) (convertStats, error) {
	stats := convertStats{asn: true}
	err := convertFile(reader, out, asnCompatType, func(res maxminddb.Result) (mmdbtype.Map, error) {
		var r flatASN
		if err := res.Decode(&r); err != nil {
			return nil, err
		}
		n, err := strconv.ParseUint(r.ASN, 10, 32)
		if err != nil || n == 0 {
			stats.skipped++
			return nil, nil
		}
		rec := mmdbtype.Map{"autonomous_system_number": mmdbtype.Uint32(n)}
		org := r.Org
		if org == "" {
			org = r.Name
		}
		if org != "" {
			rec["autonomous_system_organization"] = mmdbtype.String(org)
			stats.withOrg++
		}
		setString(rec, "name", r.Name)
		setString(rec, "country_code", r.CountryCode)
		stats.networks++
		return rec, nil
	})
	return stats, err
}

var (
	v4Compat = netip.MustParsePrefix("::/96")
	v4Mapped = netip.MustParsePrefix("::ffff:0:0/96")
)

func runConvert(args []string) {
	flags := flag.NewFlagSet("convert", flag.ExitOnError)
	db := flags.String("db", "", "input flat ip2geo or ip2asn MMDB")
	out := flags.String("out", "", "output MMDB in the GeoLite2-City (ip2geo) or GeoLite2-ASN (ip2asn) layout")
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
	if s.asn {
		fmt.Printf("convert: %d networks, %d with an organization, %d skipped\n", s.networks, s.withOrg, s.skipped)
		return
	}
	fmt.Printf("convert: %d networks, %d with a city, %d placed, %d with a time zone\n", s.networks, s.withCity, s.placed, s.withTZ)
}
