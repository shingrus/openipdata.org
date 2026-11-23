package main

// The JSON API of one address: /json (the visitor's own address, by the same
// rules as /ip) and /json/<ip> (a lookup). The fields are the names of the MMDB
// records of the download page, each left out when unknown. Nothing here logs
// an address.

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"unicode"
)

type ipJSON struct {
	IP            string   `json:"ip"`
	ContinentCode string   `json:"continent_code,omitempty"`
	ContinentName string   `json:"continent_name,omitempty"`
	CountryCode   string   `json:"country_code,omitempty"`
	CountryName   string   `json:"country_name,omitempty"`
	InEU          *bool    `json:"in_eu,omitempty"` // only for a new-layout record with a country
	CityName      string   `json:"city_name,omitempty"`
	CitySource    string   `json:"city_source,omitempty"` // only with a city
	CityGuess     *bool    `json:"city_guess,omitempty"`  // only with a city and a source
	Latitude      *float64 `json:"latitude,omitempty"`    // only for a placed city
	Longitude     *float64 `json:"longitude,omitempty"`
	TimeZone      string   `json:"timezone,omitempty"`
	ASN           uint32   `json:"asn,omitempty"`
	ASName        string   `json:"as_name,omitempty"`
	Org           string   `json:"org,omitempty"`
	Prefix        string   `json:"prefix,omitempty"`
}

// ipJSONFields are the JSON field names of ipJSON in struct order (for llms.txt),
// read from the tags once at start so the list cannot drift from the struct.
var ipJSONFields = jsonNames(ipJSON{})

func jsonNames(v any) []string {
	t := reflect.TypeOf(v)
	var out []string
	for i := 0; i < t.NumField(); i++ {
		out = append(out, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
	}
	return out
}

// ipJSONFor is the answer for a public address, from the same lookups the
// panel uses.
func (s *server) ipJSONFor(a netip.Addr) ipJSON {
	g, n, info := s.addrData(a)
	out := ipJSON{IP: a.String(), ContinentCode: g.Continent.Code, ContinentName: g.Continent.Names.EN, TimeZone: g.Location.TimeZone, CountryCode: g.CountryCode, CountryName: g.CountryName, CityName: g.CityName,
		ASN: info.ASN, ASName: firstNonEmpty(info.Handle, strings.TrimSpace(n.Name)), Org: strings.TrimSpace(n.Org), Prefix: info.Prefix}
	if g.newLayout && g.CountryCode != "" {
		eu := g.Country.EU
		out.InEU = &eu
	}
	if g.CityName != "" {
		if g.CitySource != "" {
			out.CitySource = g.CitySource
			guess := guessSources[g.CitySource]
			out.CityGuess = &guess
		}
		if lat, lon, ok := g.coords(); ok {
			out.Latitude, out.Longitude = &lat, &lon
		}
	}
	return out
}

// guessSources are the city_sources of a guessed city. Copied from `guessSources`
// in mmdbbuilder/asnmain.go (the site must not import it); TestGuessSourcesMatchBuilder
// fails when the two lists differ.
var guessSources = map[string]bool{
	"80-asn-main": true, "82-asn-likely": true, "84-asn-near": true, "85-holder-city": true,
	"86-country-main": true, "88-asn-far": true, "89-asn-anywhere": true, "89-country-near": true,
}

// writeIPJSON writes one JSON object and a newline.
func writeIPJSON(w http.ResponseWriter, r *http.Request, code int, cache string, v any) {
	h := w.Header()
	h.Set("Cache-Control", cache)
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("X-Content-Type-Options", "nosniff")
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)              // "&" stays "&" for curl readers; the type is JSON, never HTML
	if err := e.Encode(v); err != nil { // Encode adds the newline
		code = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString("{\"error\":\"error\"}\n")
	}
	writeBody(w, r, code, "application/json; charset=utf-8", buf.Bytes())
}

type errJSON struct {
	IP    string `json:"ip,omitempty"`
	Input string `json:"input,omitempty"`
	Error string `json:"error"`
}

const (
	jsonListMax = 100 // most addresses in the URL
	jsonBodyMax = 64 << 10
	jsonPostMax = bulkMax // most addresses in a POST body
)

// listEntry is the element of a list answer for one item.
func (s *server) listEntry(item string) any {
	a, ok := parseAddr(item)
	if !ok {
		in := item
		if len(in) > 64 {
			in = strings.ToValidUTF8(in[:64], "")
		}
		return errJSON{Input: in, Error: "not an IP address"}
	}
	if !s.public(a) {
		return errJSON{IP: a.String(), Error: "not a public address"}
	}
	return s.ipJSONFor(a)
}

func (s *server) writeList(w http.ResponseWriter, r *http.Request, cache string, items []string) {
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = s.listEntry(it)
	}
	writeIPJSON(w, r, http.StatusOK, cache, out)
}

// postJSON answers POST /json: a JSON array of strings, or text split on white
// space and commas.
func (s *server) postJSON(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, jsonBodyMax))
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeIPJSON(w, r, http.StatusRequestEntityTooLarge, "no-store", errJSON{Error: "body too large"})
		} else {
			writeIPJSON(w, r, http.StatusBadRequest, "no-store", errJSON{Error: "bad request"})
		}
		return
	}
	text := strings.TrimSpace(string(body))
	var items []string
	if strings.HasPrefix(text, "[") {
		if json.Unmarshal([]byte(text), &items) != nil {
			writeIPJSON(w, r, http.StatusBadRequest, "no-store", errJSON{Error: "bad JSON"})
			return
		}
	} else {
		items = strings.FieldsFunc(text, func(c rune) bool { return c == ',' || unicode.IsSpace(c) })
	}
	switch {
	case len(items) == 0:
		writeIPJSON(w, r, http.StatusBadRequest, "no-store", errJSON{Error: "no addresses"})
	case len(items) > jsonPostMax:
		writeIPJSON(w, r, http.StatusBadRequest, "no-store", errJSON{Error: "at most 1000 addresses"})
	default:
		s.writeList(w, r, "no-store", items)
	}
}

// cliPrefixes and cliExact are the User-Agents of command-line clients and
// common HTTP libraries: curl, wget, httpie; Go net/http; Python requests,
// urllib, httpx, aiohttp; Node (node-fetch, undici, axios, got, Deno, Bun).
// Node's own fetch sends exactly "node" and undici exactly "undici", so those
// two match whole (a monitor such as NodePing is not a client). A browser never
// starts with any of them, so fetch() of the page is unaffected.
var (
	cliPrefixes = []string{"curl/", "wget/", "httpie/", "go-http-client/", "python", "node-fetch/", "undici/", "axios/", "got ", "deno/", "bun/"}
	cliExact    = []string{"node", "undici"}
)

// isCLI says whether the request comes from a command-line client or an HTTP
// library by its User-Agent (case-insensitive, no allocation); nothing else is guessed.
func isCLI(r *http.Request) bool {
	ua := r.UserAgent()
	for _, p := range cliPrefixes {
		if len(ua) >= len(p) && strings.EqualFold(ua[:len(p)], p) {
			return true
		}
	}
	for _, e := range cliExact {
		if strings.EqualFold(ua, e) {
			return true
		}
	}
	return false
}

// serveOwnJSON answers with the JSON of the visitor's own address (the same
// clientAddr rules as /ip); used by /json and, for command-line clients, by
// "/" and by /ip on the v4 and v6 names.
func (s *server) serveOwnJSON(w http.ResponseWriter, r *http.Request) {
	a, ok := clientAddr(r, s.realIP)
	switch {
	case !ok:
		writeIPJSON(w, r, http.StatusServiceUnavailable, "no-store", errJSON{Error: "unknown address"})
	case !s.public(a):
		writeIPJSON(w, r, http.StatusBadRequest, "no-store", errJSON{IP: a.String(), Error: "not a public address"})
	default:
		writeIPJSON(w, r, http.StatusOK, "no-store", s.ipJSONFor(a))
	}
}

// serveIPJSON answers /json and /json/<ip> (GET and HEAD).
func (s *server) serveIPJSON(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/json"), "/")
	switch r.Method {
	case http.MethodOptions:
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
		if r.URL.Path != "/json" { // also /json/: nginx limits the body and rate of /json only
			w.Header().Set("Allow", "GET, HEAD")
			plain(w, http.StatusMethodNotAllowed, "method not allowed\n")
			return
		}
		s.postJSON(w, r)
		return
	}
	if rest == "" {
		s.serveOwnJSON(w, r)
		return
	}
	if strings.Contains(rest, "/") { // anything after the address
		writeIPJSON(w, r, http.StatusNotFound, asnCache, errJSON{Error: "not found"})
		return
	}
	if strings.Contains(rest, ",") {
		s.serveList(w, r, rest)
		return
	}
	a, ok := parseAddr(rest)
	if !ok {
		writeIPJSON(w, r, http.StatusBadRequest, "no-store", errJSON{Error: "not an IP address"})
		return
	}
	if canon := "/json/" + a.String(); r.URL.Path != canon || r.URL.RawPath != "" {
		h := w.Header()
		h.Set("Cache-Control", asnCache)
		h.Set("Location", canon)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}
	if !s.public(a) {
		writeIPJSON(w, r, http.StatusBadRequest, asnCache, errJSON{IP: a.String(), Error: "not a public address"})
		return
	}
	writeIPJSON(w, r, http.StatusOK, asnCache, s.ipJSONFor(a))
}

// serveList answers /json/<a>,<b>,...: an array, one entry per item.
func (s *server) serveList(w http.ResponseWriter, r *http.Request, rest string) {
	items := strings.Split(rest, ",")
	if len(items) > jsonListMax {
		writeIPJSON(w, r, http.StatusBadRequest, "no-store", errJSON{Error: "at most 100 addresses"})
		return
	}
	canon := make([]string, len(items))
	all := true
	for i, it := range items {
		if it == "" {
			writeIPJSON(w, r, http.StatusBadRequest, "no-store", errJSON{Error: "not an IP address"})
			return
		}
		a, ok := parseAddr(it)
		if !ok {
			all = false
			continue
		}
		canon[i] = a.String()
	}
	if all {
		if c := "/json/" + strings.Join(canon, ","); r.URL.Path != c || r.URL.RawPath != "" {
			h := w.Header()
			h.Set("Cache-Control", asnCache)
			h.Set("Location", c)
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
	}
	s.writeList(w, r, asnCache, items)
}

// apiPanel is the "API" panel of the home and lookup pages: the curl command
// and the real answer of that URL.
type apiPanel struct {
	Href string        // "/json" or "/json/<ip>"
	Cmd  string        // curl <origin><href>, with the scheme
	Body template.HTML // the pretty-printed answer, keys in <i>; every piece escaped here
}

var htmlText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// apiPanelFor builds the panel for the public address a from the same answer
// the API gives: one indented encoding, split into lines.
func (s *server) apiPanelFor(href string, a netip.Addr) *apiPanel {
	if !a.IsValid() {
		return nil
	}
	body, ok := apiBody(s.ipJSONFor(a))
	if !ok {
		return nil
	}
	return &apiPanel{Href: href, Cmd: "curl " + s.origin + href, Body: body}
}

// apiBody is the answer as the page shows it: indented, keys in <i>, the rest
// escaped (values come from data files).
func apiBody(v ipJSON) (template.HTML, bool) {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if e.Encode(v) != nil {
		return "", false
	}
	var b strings.Builder
	for i, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		trim := strings.TrimLeft(l, " ")
		pre := l[:len(l)-len(trim)]
		if strings.HasPrefix(trim, `"`) {
			if j := strings.Index(trim, `": `); j > 0 { // the keys are fixed names with no escapes
				b.WriteString(pre + "<i>" + htmlText.Replace(trim[:j+1]) + "</i>" + htmlText.Replace(trim[j+1:]))
				continue
			}
		}
		b.WriteString(htmlText.Replace(l))
	}
	return template.HTML(b.String()), true
}
