package rdnsrules

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The file and the limits it is read with. The file is untrusted input: whatever breaks a limit
// is skipped and counted, never echoed.
const (
	hoihoURL       = "https://publicdata.caida.org/datasets/topology/ark/ipv4/itdk/2024-08/midar-iff.geo-re.jsonl"
	hoihoCacheName = "hoiho-geo-re.jsonl"

	hoihoMaxFile       = 32 << 20 // bytes of the whole file
	hoihoMaxLine       = 8 << 20  // bytes of one line
	hoihoMaxRecords    = 5000
	hoihoMaxRegexes    = 32   // per record
	hoihoMaxRegexBytes = 1000 // per regular expression
	hoihoMaxHints      = 50000
	hoihoMaxDomain     = 253 // bytes

	// hoihoNameRadiusKm: a hint is the GeoNames place of its name and country that lies within
	// this distance of the hint's coordinates.
	hoihoNameRadiusKm = 50.0
	// hoihoNearbyRadiusKm: with no such place, the most populous GeoNames place of the country within
	// this distance.
	hoihoNearbyRadiusKm = 30.0
	// hoihoBigCityPop: a CLLI guess in a country other than the state or province of the code needs a
	// place of at least this many people.
	hoihoBigCityPop = 100_000

	// hoihoMinRows: a conversion with fewer rows is refused and the table stays as it is.
	hoihoMinRows = 500
	// hoihoMinRecords: a file with fewer records is not the file (it is dropped from the cache).
	hoihoMinRecords = 300
	// hoihoMaxRowKeys: a regex that would get more keys is skipped (one regex can emit hints times
	// splits times countries keys); hoihoMaxKeys: the whole conversion stops above this many keys.
	hoihoMaxRowKeys = 50_000
	hoihoMaxKeys    = 500_000
)

// Reasons counted in HoihoOutput.SkipReasons. A record reason skips the record, a regex reason the
// regular expression, a hint reason the hint (for that regular expression).
const (
	skipBadJSON        = "record_bad_json"
	skipLongLine       = "record_line_too_long"
	skipBadDomain      = "record_bad_domain"
	skipDuplicateDom   = "record_duplicate_domain"
	skipTooManyRecords = "record_over_limit"
	skipTooManyRegexes = "record_too_many_regexes"
	skipTooManyHints   = "record_too_many_hints"
	skipRegexLong      = "regex_too_long"
	skipRegexCompile   = "regex_not_compiled"
	skipPlanMismatch   = "regex_plan_mismatch"
	skipUnknownType    = "regex_unknown_type"
	skipNoLocation     = "regex_no_location_type"
	skipTwoLocations   = "regex_two_location_types"
	skipLocationTwice  = "regex_location_type_twice"
	skipTwoCC          = "regex_two_cc"
	skipTwoST          = "regex_two_st"
	skipNoKeys         = "regex_no_keys"
	skipTooManyKeys    = "regex_too_many_keys"
	skipHintBadCode    = "hint_bad_code"
	skipHintNoState    = "hint_no_state"
	skipHintBadCLLI    = "hint_bad_clli_code"
)

// hoihoRecord is one line of the file.
type hoihoRecord struct {
	Domain   string      `json:"domain"`
	Re       []string    `json:"re"`
	Plan     [][]string  `json:"plan"`
	Geohints []hoihoHint `json:"geohints"`
	Score    hoihoScore  `json:"score"`
}

type hoihoScore struct {
	Class string `json:"class"`
}

type hoihoHint struct {
	Code     string          `json:"code"`
	Type     string          `json:"type"`
	Lat      json.RawMessage `json:"lat"`
	Lng      json.RawMessage `json:"lng"`
	TP       float64         `json:"tp"`
	St       string          `json:"st"`
	Location *struct {
		CC    string `json:"cc"`
		Place string `json:"place"`
		St    string `json:"st"`
	} `json:"location"`
}

// HoihoRow is one row of public.rdns_hoiho_rules.
type HoihoRow struct {
	Domain  string
	Seq     int
	Pattern string
	Plan    string
	Places  map[string][2]string
	Class   string
	Version string
}

// ---- reading the file ----

// parseHoiho reads the lines of the file. A line that does not parse, is too long, has a bad or a
// repeated domain, or breaks a limit is skipped and counted.
func parseHoiho(data []byte, out *HoihoOutput) []hoihoRecord {
	var recs []hoihoRecord
	seen := map[string]bool{}
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		skip := func(reason string) {
			out.RecordsSkipped++
			out.skip(reason)
		}
		if len(line) > hoihoMaxLine {
			skip(skipLongLine)
			continue
		}
		if len(recs) >= hoihoMaxRecords {
			skip(skipTooManyRecords)
			continue
		}
		var rec hoihoRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			skip(skipBadJSON)
			continue
		}
		rec.Domain = strings.ToLower(strings.TrimSpace(rec.Domain))
		switch {
		case !plainHostName(rec.Domain):
			skip(skipBadDomain)
		case seen[rec.Domain]:
			skip(skipDuplicateDom)
		case len(rec.Re) > hoihoMaxRegexes:
			skip(skipTooManyRegexes)
		case len(rec.Geohints) > hoihoMaxHints:
			skip(skipTooManyHints)
		default:
			seen[rec.Domain] = true
			recs = append(recs, rec)
		}
	}
	return recs
}

// plainHostName: labels of [a-z0-9-] (1 to 63 bytes each), at least one dot, at most 253 bytes.
func plainHostName(s string) bool {
	if len(s) == 0 || len(s) > hoihoMaxDomain || !strings.Contains(s, ".") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func (o *HoihoOutput) skip(reason string) {
	if o.SkipReasons == nil {
		o.SkipReasons = map[string]int{}
	}
	o.SkipReasons[reason]++
}

// ---- the plan of a regular expression ----

// planInfo says what the capture groups of a regular expression are.
type planInfo struct {
	types []string // one per capture group, lower case
	loc   string   // the location type
	pair  bool     // clli,clli: the code is split over two adjacent captures
	hasCC bool
	hasST bool
}

var hoihoLocationTypes = map[string]bool{"iata": true, "place": true, "clli": true, "locode": true, "facility": true}

// analyzePlan checks the types of one plan; the reason is empty when it is usable.
func analyzePlan(plan []string) (planInfo, string) {
	pi := planInfo{types: make([]string, len(plan))}
	var locIdx []int
	ccs, sts := 0, 0
	for i, t := range plan {
		t = strings.ToLower(strings.TrimSpace(t))
		pi.types[i] = t
		switch {
		case t == "cc":
			ccs++
		case t == "st":
			sts++
		case hoihoLocationTypes[t]:
			locIdx = append(locIdx, i)
		default:
			return pi, skipUnknownType
		}
	}
	switch {
	case ccs > 1:
		return pi, skipTwoCC
	case sts > 1:
		return pi, skipTwoST
	case len(locIdx) == 0:
		return pi, skipNoLocation
	}
	pi.loc = pi.types[locIdx[0]]
	for _, i := range locIdx[1:] {
		if pi.types[i] != pi.loc {
			return pi, skipTwoLocations
		}
	}
	if len(locIdx) > 1 {
		if pi.loc != "clli" || len(locIdx) != 2 || locIdx[1] != locIdx[0]+1 {
			return pi, skipLocationTwice
		}
		pi.pair = true
	}
	pi.hasCC, pi.hasST = ccs == 1, sts == 1
	return pi, ""
}

// ---- GeoNames helpers ----

// hoihoGeo wraps the GeoNames index with the lookups the conversion needs.
type hoihoGeo struct {
	g        *geoIndex
	byLat    map[string][]int32 // country -> places sorted by latitude
	byRegion map[string][]int32 // country + "." + admin1 -> places, biggest first
	big      []bigCountry       // countries with places of at least hoihoBigCityPop, by code
	letters  []string           // per place: its name's a-z letters
}

type bigCountry struct {
	code   string
	places []int32 // at least hoihoBigCityPop people, biggest first
}

// placeBefore orders places: more people, then the smaller name, then the smaller id.
func (h *hoihoGeo) placeBefore(a, b int32) bool {
	pa, pb := &h.g.places[a], &h.g.places[b]
	if pa.Pop != pb.Pop {
		return pa.Pop > pb.Pop
	}
	if pa.Name != pb.Name {
		return pa.Name < pb.Name
	}
	return pa.ID < pb.ID
}

func newHoihoGeo(g *geoIndex) *hoihoGeo {
	h := &hoihoGeo{g: g, byLat: map[string][]int32{}, byRegion: map[string][]int32{}, letters: make([]string, len(g.places))}
	for i := range g.places {
		p := &g.places[i]
		idx := int32(i)
		h.byLat[p.Country] = append(h.byLat[p.Country], idx)
		key := p.Country + "." + p.Admin1
		h.byRegion[key] = append(h.byRegion[key], idx)
		h.letters[i] = asciiToken(p.Name)
	}
	for _, list := range h.byLat {
		sort.SliceStable(list, func(i, j int) bool { return g.places[list[i]].Lat < g.places[list[j]].Lat })
	}
	for _, list := range h.byRegion {
		sort.Slice(list, func(i, j int) bool { return h.placeBefore(list[i], list[j]) })
	}
	codes := make([]string, 0, len(g.countries))
	for c := range g.countries {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		var bigs []int32
		for _, idx := range h.byLat[c] {
			if g.places[idx].Pop >= hoihoBigCityPop {
				bigs = append(bigs, idx)
			}
		}
		if len(bigs) > 0 {
			sort.Slice(bigs, func(i, j int) bool { return h.placeBefore(bigs[i], bigs[j]) })
			h.big = append(h.big, bigCountry{c, bigs})
		}
	}
	return h
}

// best returns the first place of idxs (already filtered) that h.placeBefore ranks first.
func (h *hoihoGeo) best(idxs []int32) *geoPlace {
	if len(idxs) == 0 {
		return nil
	}
	b := idxs[0]
	for _, i := range idxs[1:] {
		if h.placeBefore(i, b) {
			b = i
		}
	}
	return &h.g.places[b]
}

// placeHint finds the GeoNames place of a hint that has a location: among the places named
// place in the country, those within hoihoNameRadiusKm of the coordinates, the most populous;
// else the most populous place of the country within hoihoNearbyRadiusKm. byName tells which.
func (h *hoihoGeo) placeHint(country, place string, lat, lon float64) (p *geoPlace, byName bool) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if !h.g.countries[country] || strings.TrimSpace(place) == "" {
		return nil, false
	}
	var near []int32
	for _, i := range h.g.byName[geoKey(country, foldName(place))] {
		q := &h.g.places[i]
		if haversineKm(lat, lon, q.Lat, q.Lon) <= hoihoNameRadiusKm {
			near = append(near, i)
		}
	}
	if p = h.best(near); p != nil {
		return p, true
	}
	list := h.byLat[country]
	window := hoihoNearbyRadiusKm/110.5 + 0.01 // degrees of latitude
	start := sort.Search(len(list), func(i int) bool { return h.g.places[list[i]].Lat >= lat-window })
	near = near[:0]
	for _, i := range list[start:] {
		q := &h.g.places[i]
		if q.Lat > lat+window {
			break
		}
		if haversineKm(lat, lon, q.Lat, q.Lon) <= hoihoNearbyRadiusKm {
			near = append(near, i)
		}
	}
	return h.best(near), false
}

// abbreviates reports whether abbr (4 letters) abbreviates the place: it starts with the first
// letter of the name and the other letters follow in order.
func abbreviates(letters, abbr string) bool {
	if letters == "" || abbr == "" || letters[0] != abbr[0] {
		return false
	}
	at := 1
	for i := 1; i < len(letters) && at < len(abbr); i++ {
		if letters[i] == abbr[at] {
			at++
		}
	}
	return at == len(abbr)
}

// firstAbbreviated returns the first place of the list (biggest first) that abbr abbreviates.
func (h *hoihoGeo) firstAbbreviated(list []int32, abbr string, minPop int64) *geoPlace {
	for _, i := range list {
		if h.g.places[i].Pop < minPop {
			break
		}
		if abbreviates(h.letters[i], abbr) {
			return &h.g.places[i]
		}
	}
	return nil
}

var usStates = map[string]bool{}

func init() {
	for _, s := range strings.Fields("AL AK AZ AR CA CO CT DE DC FL GA HI ID IL IN IA KS KY LA ME MD MA MI MN MS MO MT NE NV NH NJ NM NY NC ND OH OK OR PA RI SC SD TN TX UT VT VA WA WV WI WY") {
		usStates[s] = true
	}
}

// canadaAdmin1 maps the province codes of a CLLI code to the GeoNames admin1 code.
var canadaAdmin1 = map[string]string{
	"AB": "01", "BC": "02", "MB": "03", "NB": "04", "NF": "05", "NL": "05", "NS": "07", "NT": "13",
	"NU": "14", "ON": "08", "PE": "09", "PQ": "10", "QC": "10", "SK": "11", "YT": "12",
}

// nycCodes are the first four letters of the CLLI codes of New York City (with region NY).
var nycCodes = map[string]bool{"nycm": true, "nycq": true, "nyck": true, "nycx": true, "nycr": true}

// ---- hints ----

// hintState is what the conversion knows about one hint of a record.
type hintState struct {
	code    string
	typ     string
	tp      float64
	st      string    // lower-case state of a located hint ("" = none)
	located bool      // the hint has a location (a coordinate pair and a place)
	place   *geoPlace // the GeoNames place of a located hint
	clli    *clliGuess
	bad     bool // code not usable
	noState bool // hint_no_state already counted for this hint
}

// clliGuess is the guess for a CLLI hint with no location.
type clliGuess struct {
	region     string    // the last two letters, lower case
	country    string    // "US" or "CA" when region is a state or province, else ""
	place      *geoPlace // the place in that state or province
	others     []*geoPlace
	othersDone bool
}

func validCode(c string) bool {
	if len(c) == 0 || len(c) > 32 {
		return false
	}
	for i := 0; i < len(c); i++ {
		if !(c[i] >= 'a' && c[i] <= 'z' || c[i] >= '0' && c[i] <= '9') {
			return false
		}
	}
	return true
}

func parseCoord(raw json.RawMessage, limit float64) (float64, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < -limit || v > limit {
		return 0, false
	}
	return v, true
}

func isLetters(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return s != ""
}

// classify places every hint of a record once and counts them.
func (h *hoihoGeo) classify(rec *hoihoRecord, out *HoihoOutput) []hintState {
	states := make([]hintState, len(rec.Geohints))
	for i := range rec.Geohints {
		hint := &rec.Geohints[i]
		s := &states[i]
		out.Hints++
		s.code = strings.ToLower(strings.TrimSpace(hint.Code))
		s.typ = strings.ToLower(strings.TrimSpace(hint.Type))
		s.tp = hint.TP
		if !validCode(s.code) {
			s.bad = true
			out.skip(skipHintBadCode)
			continue
		}
		if hint.Location != nil && strings.TrimSpace(hint.Location.Place) != "" {
			s.located = true
			if s.typ == "facility" {
				s.st = strings.ToLower(strings.TrimSpace(hint.St))
			} else {
				s.st = strings.ToLower(strings.TrimSpace(hint.Location.St))
			}
			lat, okLat := parseCoord(hint.Lat, 90)
			lon, okLon := parseCoord(hint.Lng, 180)
			if okLat && okLon {
				var byName bool
				s.place, byName = h.placeHint(hint.Location.CC, hint.Location.Place, lat, lon)
				if s.place != nil {
					if byName {
						out.HintsPlacedByName++
					} else {
						out.HintsPlacedNearby++
					}
					continue
				}
			}
			out.HintsUnplaced++
			continue
		}
		if s.typ != "clli" {
			out.HintsUnplaced++
			continue
		}
		out.CLLIHints++
		if len(s.code) != 6 || !isLetters(s.code) {
			s.bad = true
			out.skip(skipHintBadCLLI)
			continue
		}
		s.clli = h.guessCLLI(s.code, out)
	}
	return states
}

// guessCLLI is the CLLI guess: four letters that abbreviate a place name and a two-letter region.
func (h *hoihoGeo) guessCLLI(code string, out *HoihoOutput) *clliGuess {
	g := &clliGuess{region: code[4:6]}
	upper := strings.ToUpper(g.region)
	admin1 := ""
	switch {
	case usStates[upper]:
		g.country, admin1 = "US", upper
	case canadaAdmin1[upper] != "":
		g.country, admin1 = "CA", canadaAdmin1[upper]
	default:
		out.CLLINoRegion++
		return g
	}
	if g.country == "US" && g.region == "ny" && nycCodes[code[:4]] {
		var nyc []int32
		for _, i := range h.g.byName[geoKey("US", "new york city")] {
			if h.g.places[i].Admin1 == "NY" {
				nyc = append(nyc, i)
			}
		}
		if g.place = h.best(nyc); g.place != nil {
			out.CLLIKnownNYC++
			out.CLLIGuessed++
			return g
		}
		out.CLLINoCandidate++
		return g
	}
	if g.place = h.firstAbbreviated(h.byRegion[g.country+"."+admin1], code[:4], 0); g.place == nil {
		out.CLLINoCandidate++
		return g
	}
	out.CLLIGuessed++
	return g
}

// otherCountries returns, once, the place of every country other than the code's own state or
// province whose name the four letters abbreviate, among places of at least hoihoBigCityPop people.
func (h *hoihoGeo) otherCountries(g *clliGuess, code string) []*geoPlace {
	if g.othersDone {
		return g.others
	}
	g.othersDone = true
	for _, bc := range h.big {
		if bc.code == g.country {
			continue
		}
		if p := h.firstAbbreviated(bc.places, code[:4], hoihoBigCityPop); p != nil {
			g.others = append(g.others, p)
		}
	}
	return g.others
}

// ---- conversion ----

type keyCand struct {
	tp      float64
	country string
	city    string
}

// better reports whether c replaces o under one key: the larger tp, then the smaller city name,
// then the smaller country.
func (c keyCand) better(o keyCand) bool {
	if c.tp != o.tp {
		return c.tp > o.tp
	}
	if c.city != o.city {
		return c.city < o.city
	}
	return c.country < o.country
}

// convertHoiho turns the records into rows. It is pure: the same records and GeoNames index always
// give the same rows, sorted by domain and seq.
func convertHoiho(recs []hoihoRecord, geo *geoIndex, out *HoihoOutput) (rows []HoihoRow, tooMany bool) {
	h := newHoihoGeo(geo)
	for ri := range recs {
		rec := &recs[ri]
		out.Records++
		states := h.classify(rec, out)
		class := strings.ToLower(strings.TrimSpace(rec.Score.Class))
		if class == "" {
			class = "unknown"
		}
		for i, pattern := range rec.Re {
			out.Regexes++
			skip := func(reason string) {
				out.RegexesSkipped++
				out.skip(reason)
			}
			if len(pattern) > hoihoMaxRegexBytes {
				skip(skipRegexLong)
				continue
			}
			re, err := regexp.Compile("(?i)" + pattern)
			if err != nil {
				skip(skipRegexCompile)
				continue
			}
			if i >= len(rec.Plan) || re.NumSubexp() != len(rec.Plan[i]) {
				skip(skipPlanMismatch)
				continue
			}
			pi, reason := analyzePlan(rec.Plan[i])
			if reason != "" {
				skip(reason)
				continue
			}
			cands, over := h.keysFor(pi, states, out)
			if over {
				skip(skipTooManyKeys)
				continue
			}
			if len(cands) == 0 {
				skip(skipNoKeys)
				continue
			}
			places := make(map[string][2]string, len(cands))
			vplaces := make(map[string]Place, len(cands))
			for k, c := range cands {
				places[k] = [2]string{c.country, c.city}
				vplaces[k] = Place{Country: c.country, City: c.city}
			}
			plan := strings.Join(pi.types, ",")
			rows = append(rows, HoihoRow{Domain: rec.Domain, Seq: i + 1, Pattern: pattern, Plan: plan, Places: places, Class: class,
				Version: version(pattern, "hoiho:"+plan+":"+class, vplaces)})
			out.Keys += len(places)
			if out.Keys > hoihoMaxKeys {
				return nil, true
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Domain != rows[j].Domain {
			return rows[i].Domain < rows[j].Domain
		}
		return rows[i].Seq < rows[j].Seq
	})
	out.Rows = len(rows)
	return rows, false
}

// keysFor builds the keys of one regular expression from the hints of its location type.
func (h *hoihoGeo) keysFor(pi planInfo, states []hintState, out *HoihoOutput) (map[string]keyCand, bool) {
	cands := map[string]keyCand{}
	over := false // more than hoihoMaxRowKeys keys: stop adding at once
	add := func(tp float64, p *geoPlace, ccs []string, st string, codes ...string) {
		for _, cc := range ccs {
			for _, parts := range splitCodes(codes, pi.pair) {
				if over {
					return
				}
				k := buildKey(pi, parts, cc, st)
				c := keyCand{tp: tp, country: p.Country, city: p.Name}
				if old, ok := cands[k]; !ok || c.better(old) {
					cands[k] = c
					over = len(cands) > hoihoMaxRowKeys
				}
			}
		}
	}
	for i := range states {
		if over {
			break
		}
		s := &states[i]
		if s.bad || s.typ != pi.loc {
			continue
		}
		switch {
		case s.located:
			if s.place == nil {
				continue
			}
			if pi.hasST && s.st == "" {
				if !s.noState {
					s.noState = true // counted once per hint and record, not once per regex
					out.skip(skipHintNoState)
				}
				continue
			}
			add(s.tp, s.place, countryParts(pi, s.place.Country), s.st, s.code)
		case s.clli != nil:
			g := s.clli
			st := ""
			if pi.hasST {
				st = g.region
			}
			if g.place != nil {
				add(s.tp, g.place, countryParts(pi, g.country), st, s.code)
			}
			if pi.hasCC {
				for _, p := range h.otherCountries(g, s.code) {
					add(s.tp, p, countryParts(pi, p.Country), st, s.code)
				}
			}
		}
	}
	return cands, over
}

// countryParts: the lower-case country values a key may carry (GB also as uk); a plan without cc
// has one empty value.
func countryParts(pi planInfo, country string) []string {
	if !pi.hasCC {
		return []string{""}
	}
	cc := strings.ToLower(country)
	if cc == "gb" {
		return []string{"gb", "uk"}
	}
	return []string{cc}
}

// splitCodes returns the capture values for a code: the code itself, or for clli,clli every way to
// split it in two non-empty parts.
func splitCodes(codes []string, pair bool) [][]string {
	if !pair {
		return [][]string{codes}
	}
	code := codes[0]
	var out [][]string
	for k := 1; k < len(code); k++ {
		out = append(out, []string{code[:k], code[k:]})
	}
	return out
}

// buildKey joins the capture values in capture order.
func buildKey(pi planInfo, codes []string, cc, st string) string {
	parts := make([]string, 0, len(pi.types))
	ci := 0
	for _, t := range pi.types {
		switch t {
		case "cc":
			parts = append(parts, cc)
		case "st":
			parts = append(parts, st)
		default:
			parts = append(parts, codes[ci])
			ci++
		}
	}
	return strings.Join(parts, ",")
}
