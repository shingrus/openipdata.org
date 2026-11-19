package geo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"openipdata.org/hatchet/rirstats"
)

// Oracle Cloud is the second operator of the cloud region layer (68-cloud-regions). Its file lists
// address ranges per region (public_ip_ranges.json) but Oracle publishes no geofeed, so the city
// of a region cannot be learned: it comes from the fixed table below. A region that is not in the
// table gets no row. The ranges are made disjoint like Amazon's (cloudSegments: an address belongs
// to the most specific prefix) and neighbouring ranges are not merged.
const (
	// oracleRegionsURL is Oracle's region file.
	oracleRegionsURL = "https://docs.oracle.com/en-us/iaas/tools/public_ip_ranges.json"
	// oracleRegionsFile is the saved copy in the downloads directory.
	oracleRegionsFile = "oracle-ip-ranges.json"

	oracleRegionsMinPrefixes = 300 // fewer valid entries and it is not the file
	// oracleRegionsMinBits: an entry broader than a /12 is not used. The city of a range comes
	// from the table whatever the range is, so one wrong broad entry would move a large block;
	// the broadest entry of the real file is a /16.
	oracleRegionsMinBits = 12
	oracleRegionsSource  = "cloud-regions:oracle:"
)

// oracleRegionPlace is the country and the city (the GeoNames main name, so the builder can place
// it) of an Oracle region.
type oracleRegionPlace struct{ country, city string }

// oracleRegions maps a region identifier to its place. The place is Oracle's own location of the
// region; it is not read from the identifier: uk-cardiff-1 is in Newport.
var oracleRegions = map[string]oracleRegionPlace{
	"af-casablanca-1":   {"MA", "Casablanca"},
	"af-johannesburg-1": {"ZA", "Johannesburg"},
	"ap-batam-1":        {"ID", "Batam"},
	"ap-chuncheon-1":    {"KR", "Chuncheon"},
	"ap-hyderabad-1":    {"IN", "Hyderabad"},
	"ap-kulai-2":        {"MY", "Kulai"},
	"ap-melbourne-1":    {"AU", "Melbourne"},
	"ap-mumbai-1":       {"IN", "Mumbai"},
	"ap-osaka-1":        {"JP", "Osaka"},
	"ap-seoul-1":        {"KR", "Seoul"},
	"ap-singapore-1":    {"SG", "Singapore"},
	"ap-singapore-2":    {"SG", "Singapore"},
	"ap-sydney-1":       {"AU", "Sydney"},
	"ap-tokyo-1":        {"JP", "Tokyo"},
	"ca-montreal-1":     {"CA", "Montréal"},
	"ca-toronto-1":      {"CA", "Toronto"},
	"eu-amsterdam-1":    {"NL", "Amsterdam"},
	"eu-frankfurt-1":    {"DE", "Frankfurt am Main"},
	"eu-jovanovac-1":    {"RS", "Jovanovac"},
	"eu-madrid-1":       {"ES", "Madrid"},
	"eu-madrid-3":       {"ES", "Madrid"},
	"eu-marseille-1":    {"FR", "Marseille"},
	"eu-milan-1":        {"IT", "Milan"},
	"eu-paris-1":        {"FR", "Paris"},
	"eu-stockholm-1":    {"SE", "Stockholm"},
	"eu-turin-1":        {"IT", "Turin"},
	"eu-zurich-1":       {"CH", "Zürich"},
	"il-jerusalem-1":    {"IL", "Jerusalem"},
	"me-abudhabi-1":     {"AE", "Abu Dhabi"},
	"me-dubai-1":        {"AE", "Dubai"},
	"me-jeddah-1":       {"SA", "Jeddah"},
	"me-riyadh-1":       {"SA", "Riyadh"},
	"mx-monterrey-1":    {"MX", "Monterrey"},
	"mx-queretaro-1":    {"MX", "Santiago de Querétaro"},
	"sa-bogota-1":       {"CO", "Bogotá"},
	"sa-santiago-1":     {"CL", "Santiago"},
	"sa-saopaulo-1":     {"BR", "São Paulo"},
	"sa-valparaiso-1":   {"CL", "Valparaíso"},
	"sa-vinhedo-1":      {"BR", "Vinhedo"},
	"uk-cardiff-1":      {"GB", "Newport"},
	"uk-london-1":       {"GB", "London"},
	"us-ashburn-1":      {"US", "Ashburn"},
	"us-chicago-1":      {"US", "Chicago"},
	"us-phoenix-1":      {"US", "Phoenix"},
	"us-sanjose-1":      {"US", "San Jose"},
}

// parseOracleRegions reads Oracle's region file with a streaming decoder, like parseCloudRegions:
// the entries of every region's "cidrs" are decoded one at a time and counted over the whole file,
// so a body with too many entries is rejected as soon as the limit is passed. A wrong JSON type
// anywhere, too many entries or too few valid ones is a *cloudInputError. An IPv6 range is ignored
// (not counted as bad); any other entry that is not a public IPv4 prefix between /8 and /32 with no
// host bits, or whose region label is not valid, is only counted as bad.
func parseOracleRegions(data []byte) ([]cloudRange, cloudParse, error) {
	var st cloudParse
	notFile := &cloudInputError{errors.New("not a region file")}
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, st, notFile
	}
	var out []cloudRange
	for dec.More() {
		t, err := dec.Token()
		key, ok := t.(string)
		if err != nil || !ok {
			return nil, st, notFile
		}
		if key != "regions" {
			if err := skipJSONValue(dec); err != nil {
				return nil, st, notFile
			}
			continue
		}
		if t, err := dec.Token(); err != nil || t != json.Delim('[') {
			return nil, st, notFile
		}
		for dec.More() {
			if t, err := dec.Token(); err != nil || t != json.Delim('{') {
				return nil, st, notFile
			}
			// The region key may come after cidrs: keep the prefixes until the object ends.
			var label string
			var cidrs []string
			for dec.More() {
				t, err := dec.Token()
				k, ok := t.(string)
				if err != nil || !ok {
					return nil, st, notFile
				}
				switch k {
				case "region":
					var v any
					if err := dec.Decode(&v); err != nil {
						return nil, st, notFile
					}
					s, ok := v.(string)
					if !ok {
						return nil, st, notFile
					}
					label = strings.ToLower(strings.TrimSpace(s))
				case "cidrs":
					if t, err := dec.Token(); err != nil || t != json.Delim('[') {
						return nil, st, notFile
					}
					for dec.More() {
						var raw json.RawMessage
						if err := dec.Decode(&raw); err != nil || len(raw) == 0 || raw[0] != '{' {
							return nil, st, notFile
						}
						var e struct {
							CIDR string `json:"cidr"`
						}
						if err := json.Unmarshal(raw, &e); err != nil {
							return nil, st, notFile
						}
						if st.Entries++; st.Entries > cloudRegionsMaxEntries {
							return nil, st, &cloudInputError{fmt.Errorf("more than %d entries", cloudRegionsMaxEntries)}
						}
						cidrs = append(cidrs, e.CIDR)
					}
					if t, err := dec.Token(); err != nil || t != json.Delim(']') {
						return nil, st, notFile
					}
				default:
					if err := skipJSONValue(dec); err != nil {
						return nil, st, notFile
					}
				}
			}
			if t, err := dec.Token(); err != nil || t != json.Delim('}') {
				return nil, st, notFile
			}
			for _, c := range cidrs {
				p, err := netip.ParsePrefix(strings.TrimSpace(c))
				if err == nil && p.Addr().Is6() {
					continue
				}
				r, ok := cloudEntry(p, label, oracleRegionsMinBits)
				if !ok {
					st.Bad++
					continue
				}
				out = append(out, r)
			}
		}
		if t, err := dec.Token(); err != nil || t != json.Delim(']') {
			return nil, st, notFile
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, st, notFile
	}
	st.Valid = len(out)
	if st.Valid < oracleRegionsMinPrefixes {
		return nil, st, &cloudInputError{fmt.Errorf("%d valid entries, fewer than %d", st.Valid, oracleRegionsMinPrefixes)}
	}
	return out, st, nil
}

// oracleRegionRows is Oracle's part of the 68-cloud-regions layer: every range of a region that is
// in the table, split into prefixes, in address order, with the table's country and city. Pure: no
// database, no network. A problem with the region file is a *cloudInputError.
func oracleRegionRows(data []byte) ([]cloudRow, cloudStats, error) {
	var st cloudStats
	ranges, parse, err := parseOracleRegions(data)
	st.Parse = parse
	if err != nil {
		return nil, st, err
	}
	segs, regions := cloudSegments(ranges)
	st.Seen = len(regions)
	var rows []cloudRow
	got := make([]bool, len(regions)) // regions with a row
	for _, s := range segs {
		place, ok := oracleRegions[regions[s.region]]
		if !ok {
			continue
		}
		if !got[s.region] {
			got[s.region] = true
			st.Qualified++
		}
		source := oracleRegionsSource + regions[s.region]
		for _, p := range rirstats.RangeToPrefixes(addrFrom32(s.start), addrFrom32(s.end)) {
			rows = append(rows, cloudRow{p, place.country, place.city, source})
		}
	}
	st.Rows = len(rows)
	return rows, st, nil
}
