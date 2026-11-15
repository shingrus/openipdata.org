package main

// The Headers button of the visitor's own page: the headers of the request that
// fetched the page, as pretty JSON in a data block for panel.js to copy. They
// are the visitor's own, shown only back to the visitor on "/" (private,
// no-store). Never put on a lookup or report page, never saved, never logged.

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
)

const maxHeadersJSON = 8 << 10 // the biggest JSON of the Headers button

// hiddenHeaders are left out: proxy headers (ours and Cloudflare's) and the
// private ones. Prefixes: x-forwarded-* and cf-*.
var hiddenHeaders = map[string]bool{
	"cookie": true, "authorization": true, "proxy-authorization": true,
	"x-real-ip": true, "forwarded": true, "true-client-ip": true,
	"cdn-loop": true, "x-share": true, "connection": true,
}

// requestHeaders is the JSON of the headers of r without the hidden ones and
// the header REAL_IP_HEADER names: an object with lower-case names (sorted by
// encoding/json), each value one string (repeated headers joined with ", "),
// at most maxHeadersJSON bytes (the entry with the longest value goes first
// until it fits). The text is safe inside a script data block (encoding/json
// escapes < > & and the line separators). Empty when nothing is left.
func (s *server) requestHeaders(r *http.Request) template.JS {
	m := map[string]string{}
	for name, vals := range r.Header {
		n := strings.ToLower(name)
		if hiddenHeaders[n] || strings.HasPrefix(n, "x-forwarded-") || strings.HasPrefix(n, "cf-") ||
			(s.realIP != "" && n == strings.ToLower(s.realIP)) {
			continue
		}
		m[n] = strings.Join(vals, ", ")
	}
	for len(m) > 0 {
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return ""
		}
		if len(b) <= maxHeadersJSON {
			return template.JS(b)
		}
		longest := ""
		for n, v := range m {
			if longest == "" || len(v) > len(m[longest]) || len(v) == len(m[longest]) && n < longest {
				longest = n
			}
		}
		delete(m, longest)
	}
	return ""
}
