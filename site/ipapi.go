package main

// The JSON API of one address: /json (the visitor's own address, by the same
// rules as /ip) and /json/<ip> (a lookup). The fields are the names of the MMDB
// records of the download page, each left out when unknown. Nothing here logs
// an address.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
)

type ipJSON struct {
	IP          string   `json:"ip"`
	CountryCode string   `json:"country_code,omitempty"`
	CountryName string   `json:"country_name,omitempty"`
	CityName    string   `json:"city_name,omitempty"`
	Latitude    *float64 `json:"latitude,omitempty"` // only for a placed city
	Longitude   *float64 `json:"longitude,omitempty"`
	ASN         uint32   `json:"asn,omitempty"`
	ASName      string   `json:"as_name,omitempty"`
	Org         string   `json:"org,omitempty"`
	Prefix      string   `json:"prefix,omitempty"`
}

// ipJSONFor is the answer for a public address, from the same lookups the
// panel uses.
func (s *server) ipJSONFor(a netip.Addr) ipJSON {
	g, n, info := s.addrData(a)
	out := ipJSON{IP: a.String(), CountryCode: g.CountryCode, CountryName: g.CountryName, CityName: g.CityName,
		ASN: info.ASN, ASName: firstNonEmpty(info.Handle, strings.TrimSpace(n.Name)), Org: strings.TrimSpace(n.Org), Prefix: info.Prefix}
	if lat, lon, ok := g.coords(); ok && g.CityName != "" {
		out.Latitude, out.Longitude = &lat, &lon
	}
	return out
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
	Error string `json:"error"`
}

// serveIPJSON answers /json and /json/<ip> (GET and HEAD).
func (s *server) serveIPJSON(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/json"), "/")
	if rest == "" { // the visitor's own address
		a, ok := clientAddr(r, s.realIP)
		switch {
		case !ok:
			writeIPJSON(w, r, http.StatusServiceUnavailable, "no-store", errJSON{Error: "unknown address"})
		case !s.public(a):
			writeIPJSON(w, r, http.StatusBadRequest, "no-store", errJSON{IP: a.String(), Error: "not a public address"})
		default:
			writeIPJSON(w, r, http.StatusOK, "no-store", s.ipJSONFor(a))
		}
		return
	}
	if strings.Contains(rest, "/") { // anything after the address
		writeIPJSON(w, r, http.StatusNotFound, asnCache, errJSON{Error: "not found"})
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
