package main

// GET /resolver?t=<token>: the network of the DNS resolver that looked up the
// page's one-time name. Our DNS server writes the resolver address to Redis;
// the answer holds that address (the resolver's, not the visitor's), its
// network name and a page link. Neither the token nor the address is logged.

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
)

var (
	tokenRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	zoneRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
)

// checkZone accepts a plain lower-case host name (letters, digits, hyphens, dots).
func checkZone(v string) error {
	if len(v) > 200 || !zoneRE.MatchString(v) {
		return errNotZone
	}
	return nil
}

var errNotZone = errString("not a lower-case host name")

type errString string

func (e errString) Error() string { return string(e) }

// useBeacon turns the DNS row on for names under zone (empty = off) and sets
// the Redis address, and rebuilds the policies of the pages that have scripts.
func (s *server) useBeacon(zone, redisAddr string) {
	s.zone, s.redis = zone, &redisClient{addr: redisAddr}
	s.buildCSP()
}

func (s *server) serveResolver(w http.ResponseWriter, r *http.Request) {
	if s.zone == "" {
		plain(w, http.StatusNotFound, "not found\n")
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Access-Control-Allow-Origin", "*") // the token is a secret
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	code, body := http.StatusOK, []byte("{}")
	if t := r.URL.Query()["t"]; len(t) != 1 || !tokenRE.MatchString(t[0]) {
		code = http.StatusBadRequest
	} else if val, found, err := s.redis.hget("beacon:"+t[0], "resolver_ip"); err != nil {
		code = http.StatusServiceUnavailable
	} else if !found {
		code = http.StatusNotFound
	} else if a, ok := parseAddr(val); ok && s.public(a) {
		body = s.resolverNetwork(a)
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// resolverNetwork is the JSON of resolver address a: the address, the network
// name and the page to link it to; unknown fields are left out.
func (s *server) resolverNetwork(a netip.Addr) []byte {
	var n asnRecord
	s.asn.lookup(a, &n)
	_, num := networkOf(n)
	name := firstNonEmpty(n.Name, n.Org) // the short name (handle), not the organisation
	org := s.orgLink(n)
	if org == "" && num != "" {
		org = s.asnLink(num)
	}
	b, _ := json.Marshal(struct {
		IP   string `json:"ip"`
		Name string `json:"name,omitempty"`
		Org  string `json:"org,omitempty"`
	}{a.String(), name, org})
	return b
}
