// Command openipdata-site serves the openipdata.org website.
//
// Settings (environment):
//
//	LISTEN    address to listen on (default 127.0.0.1:8080)
//	GEO_MMDB  path of the ip2geo MMDB: city and country of an address, and in its
//	          metadata the scores (default
//	          /var/lib/openipdata-site/data/openipdata-org-ip2geo.mmdb)
//	ASN_MMDB  path of the ip2asn MMDB: network, AS number and announced prefix
//	          (default /var/lib/openipdata-site/data/openipdata-org-ip2asn.mmdb)
//	ASN_SQLITE  path of the SQLite file with the ASN data, published by the
//	          worker as asn-openipdata.sqlite3 (default
//	          /var/lib/openipdata-site/data/asn-openipdata.sqlite3)
//	REAL_IP_HEADER  header that holds the visitor address (default X-Real-IP;
//	          set it to empty to use the address of the TCP connection).
//	          Trusted only when the connection comes from loopback, i.e. from
//	          our nginx.
//	V4_URL, V6_URL  URLs that answer GET with the visitor's own address, on
//	          a name that has only an A (V4_URL) or only an AAAA (V6_URL) record;
//	          the page asks the one of the family the visitor did not use
//	          (default https://v4.openipdata.org/ and https://v6.openipdata.org/).
//	          Each is an absolute https URL (http only for 127.0.0.1 and
//	          localhost) or empty: with the URL of that family empty the page
//	          has no second row and no script.
//
//	BEACON_ZONE  zone of the one-time names the DNS row makes the browser look
//	          up (<token>.<zone>, e.g. r.openipdata.org); empty (the default) = no DNS row,
//	          no script, /resolver answers 404.
//	REDIS_ADDR  loopback address of the Redis where our DNS server records the
//	          resolver of each name, and where shared reports are kept (default
//	          127.0.0.1:6379).
//	SHARE     on (default) or off: POST /share (the Share button) saves a report
//	          for 30 days and /report/<id> serves it; a page view never writes.
//	          Needs Redis; with it off /share and /report/... answer 404.
//	SITE_ORIGIN  origin of the share links, the canonical URLs, robots.txt and the
//	          sitemaps (default https://openipdata.org; an absolute https URL
//	          without path).
//
// Both MMDBs are opened again when their file changes; a missing file only
// means unknown values on the page. The ASN SQLite file is loaded again when
// it changes; without it the ASN pages answer 503 and the rest works.
// Addresses are never logged.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static/fonts/*.woff2
var fontFiles embed.FS

// The home-screen icon (180x180, the three connected nodes of the favicon in
// the accent colour on the dark background), asked for as /apple-touch-icon.png and, by iOS, as the
// -precomposed name.
//
//go:embed static/apple-touch-icon.png
var touchIcon []byte

// version is the commit the binary was built from; the release build sets it
// with -ldflags "-X main.version=<sha>".
var version = "dev"

const reloadEvery = 30 * time.Second

type fileSig struct {
	mod  time.Time
	size int64
}

type server struct {
	tmpl   *template.Template
	csp    string
	fonts  map[string][]byte
	scores atomic.Pointer[scores]

	geo, asn    *mmdbFile // kept open for lookups
	asnf        *asnFile  // the ASN data (SQLite), loaded into memory; nothing loaded until useASN
	realIP      string    // header that holds the visitor address, or ""
	familyHosts []string  // host names of V4_URL and V6_URL: /ip answers command-line clients there with JSON
	v4, v6      string    // URLs that answer with the visitor's IPv4 / IPv6 address, or ""
	cspFill     string    // csp of a page that has the fill script
	cspRes      string    // csp of a page that has the DNS row script
	cspBoth     string    // csp of a page that has both scripts
	origins     []string  // origins of V4_URL and V6_URL
	zone        string    // names of the DNS row are <token>.<zone>; empty = no DNS row
	redis       *redisClient
	share       bool                      // SHARE: save a snapshot of every visit and serve /report/<id> (needs Redis)
	origin      string                    // SITE_ORIGIN: the origin of the share links
	seo         atomic.Pointer[seoStatic] // robots.txt, llms.txt and the fixed sitemaps for s.origin
	dl          dlCache                   // the rendered /download page
	bulk        bulkCache                 // the rendered GET /bulk-ip-lookup page
	smMain      smMainCache               // the rendered /sitemap.xml
	cmp         cmpCache                  // the rendered /geolite2-alternative page
	blog        blogCache                 // the rendered /blog pages
	shareMu     [64]sync.Mutex            // updates of one report run one after another (see updateReport)
	cspPnl      [8]string                 // csp of a page that has panel.js, by fill (1), res (2) and the Share button (4)
	public      func(netip.Addr) bool
	world       *world           // country outlines for the map, decoded once at start
	placeLimit  int              // most MMDB records read for the places of a prefix page
	started     time.Time        // when this process started (the status page)
	now         func() time.Time // the clock of the status page; time.Now
}

// useURLs sets V4_URL and V6_URL (already checked) and the policy of the pages
// that have the fill script. Without any URL no page has it.
func (s *server) useURLs(v4, v6 string) {
	s.v4, s.v6 = v4, v6
	var origins []string
	s.familyHosts = nil
	for _, u := range []string{v4, v6} {
		if u != "" {
			x, _ := url.Parse(u)
			s.familyHosts = append(s.familyHosts, strings.ToLower(x.Hostname()))
			if o := x.Scheme + "://" + x.Host; !slices.Contains(origins, o) {
				origins = append(origins, o)
			}
		}
	}
	s.origins = origins
	s.buildCSP()
}

// buildCSP sets the policies of the pages that have the fill script, the DNS
// row script or both. Without any of them a page keeps s.csp.
func (s *server) buildCSP() {
	s.cspFill, s.cspRes, s.cspBoth = s.csp, s.csp, s.csp
	if len(s.origins) > 0 {
		s.cspFill = csp(s.origins...)
		s.cspBoth = s.cspFill
	}
	if s.zone != "" {
		s.cspRes = cspWith(nil, s.zone)
		s.cspBoth = cspWith(s.origins, s.zone)
	}
	// The same four with panel.js, and the four again for the page with the Share button
	// (its POST goes to the same origin).
	for i := range s.cspPnl {
		var o []string
		if i&1 != 0 {
			o = s.origins
		}
		z := ""
		if i&2 != 0 {
			z = s.zone
		}
		s.cspPnl[i] = cspFor(o, z, true, i&4 != 0)
	}
}

// pageCSP is the policy of a page with the fill script, the DNS row script
// and/or the panel script (copy and share buttons).
func (s *server) pageCSP(fill, res, pnl, share bool) string {
	if pnl {
		i := 0
		if share {
			i |= 4
		}
		if fill {
			i |= 1
		}
		if res {
			i |= 2
		}
		return s.cspPnl[i]
	}
	switch {
	case fill && res:
		return s.cspBoth
	case fill:
		return s.cspFill
	case res:
		return s.cspRes
	}
	return s.csp
}

// checkURL accepts an empty value or an absolute https URL (http only for
// 127.0.0.1 and localhost, for local tests) without user info or fragment.
func checkURL(v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(v, " \t\r\n\"'<>") {
		return errors.New("not an absolute https URL")
	}
	local := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return errors.New("must be an https URL")
	}
	return nil
}

func newServer(geoPath, asnPath, realIP string) (*server, error) {
	tmpl, err := template.ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	w, err := parseWorld(worldBin)
	if err != nil {
		return nil, fmt.Errorf("map data: %w", err)
	}
	s := &server{
		world: w, placeLimit: placeRecords,
		tmpl: tmpl, csp: csp(), fonts: map[string][]byte{},
		geo: &mmdbFile{path: geoPath}, asn: &mmdbFile{path: asnPath}, asnf: &asnFile{},
		realIP: realIP, public: isPublic, share: true, origin: defaultOrigin,
		started: time.Now(), now: time.Now,
	}
	for _, name := range []string{"geist-latin.woff2", "geist-mono-latin.woff2"} {
		b, err := fontFiles.ReadFile("static/fonts/" + name)
		if err != nil {
			return nil, err
		}
		s.fonts["/fonts/"+name] = b
	}
	// A missing or unreadable MMDB is not fatal: the page then shows dashes
	// and unknowns until check finds a readable file.
	s.geo.reload()
	s.asn.reload()
	sc := s.geo.stamp()
	s.scores.Store(&sc)
	s.buildCSP()
	return s, nil
}

// useASN sets the SQLite file of the ASN pages and loads it. A missing or
// unreadable file is not fatal: the ASN pages then answer 503 until check
// finds a readable file.
func (s *server) useASN(path string) {
	s.asnf.path = path
	s.asnf.onLoad = s.renderASNPages
	s.asnf.reload()
}

// check opens the two MMDBs again when their files changed and takes the
// scores from the new ip2geo file. A failed reload keeps what is in use (the
// readers and the scores) and logs one line per distinct error.
func (s *server) check() {
	if s.geo.reload() {
		sc := s.geo.stamp()
		s.scores.Store(&sc)
	}
	s.asn.reload()
	s.asnf.reload()
}

func (s *server) watch(ctx context.Context) {
	t := time.NewTicker(reloadEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.check()
		}
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, post := r.URL.Path, r.Method == http.MethodPost
	isJSON := p == "/json" || strings.HasPrefix(p, "/json/")
	switch {
	case p == "/share": // takes POST only
		s.serveShare(w, r)
		return
	case post && p == "/":
		s.servePostHome(w, r)
		return
	case post && p == bulkPath:
		s.serveBulk(w, r)
		return
	case isJSON && (post || r.Method == http.MethodOptions):
		s.serveIPJSON(w, r)
		return
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
	default:
		allow := "GET, HEAD"
		if p == "/" || p == bulkPath || p == "/json" {
			allow = "GET, HEAD, POST"
		}
		w.Header().Set("Allow", allow)
		plain(w, http.StatusMethodNotAllowed, "method not allowed\n")
		return
	}
	switch {
	case p == "/":
		if r.URL.RawQuery == "" { // the answer depends on the client, a lookup page never does
			w.Header().Add("Vary", "User-Agent")
			if isCLI(r) {
				s.serveOwnJSON(w, r)
				return
			}
		}
		s.serveHome(w, r)
	case p == "/ip":
		s.serveIP(w, r)
	case p == "/json" || strings.HasPrefix(p, "/json/"):
		s.serveIPJSON(w, r)
	case p == "/status":
		s.serveStatus(w, r)
	case p == "/ss":
		w.Header().Set("Cache-Control", asnCache)
		w.Header().Set("Location", "/status")
		w.WriteHeader(http.StatusMovedPermanently)
	case p == "/resolver":
		s.serveResolver(w, r)
	case strings.HasPrefix(p, "/report/"):
		s.serveReport(w, r)
	case p == downloadPath:
		s.serveDownload(w, r)
	case p == bulkPath:
		s.serveBulk(w, r)
	case p == comparePath:
		s.serveCompare(w, r)
	case p == blogPath || strings.HasPrefix(p, blogPath+"/"):
		s.serveBlog(w, r)
	case p == "/maxmind-alternative":
		w.Header().Set("Cache-Control", asnCache)
		to := comparePath
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", to)
		w.WriteHeader(http.StatusMovedPermanently)
	case p == "/llms.txt":
		s.serveLLMs(w, r)
	case p == "/robots.txt":
		s.serveRobots(w, r)
	case strings.HasPrefix(p, "/sitemap") && s.serveSitemap(w, r):
	case s.serveASN(w, r):
	case p == "/healthz":
		w.Header().Set("Cache-Control", "no-store")
		plain(w, http.StatusOK, "ok "+version+"\n")
	case p == "/apple-touch-icon.png" || p == "/apple-touch-icon-precomposed.png":
		h := w.Header()
		h.Set("Content-Type", "image/png")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Length", strconv.Itoa(len(touchIcon)))
		w.Write(touchIcon)
	case p == "/favicon.ico":
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(p, "/fonts/"):
		b, ok := s.fonts[p]
		if !ok {
			plain(w, http.StatusNotFound, "not found\n")
			return
		}
		h := w.Header()
		h.Set("Content-Type", "font/woff2")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	case s.serveMoved(w, r):
	default:
		plain(w, http.StatusNotFound, "not found\n")
	}
}

// serveHome answers "/": the visitor's own connection (no query), a lookup
// (the raw query is one address), the redirect of the lookup form (q=<address>)
// an AS number, a prefix (CIDR), or a name search, or the "bad" answer. Only a lookup page holds no visitor data, so only it
// may be cached.
func (s *server) serveHome(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", s.csp)

	q := parseQuery(r.URL.RawQuery)
	code, input, cache := http.StatusOK, "", "public, max-age=300"
	title, desc, canon := "openipdata.org", "", ""
	var p panel
	fill, res := false, false
	var visitor netip.Addr
	switch q.kind {
	case "redirect":
		h.Set("Cache-Control", cache)
		loc := "/"
		if q.addr.IsValid() {
			loc = "/?" + q.addr.String()
		}
		h.Set("Location", loc)
		w.WriteHeader(http.StatusFound)
		return
	case "prefix":
		h.Set("Cache-Control", cache)
		h.Set("Location", prefixHref(q.pfx))
		w.WriteHeader(http.StatusFound)
		return
	case "asn":
		if q.asn == "" { // 0 or out of range: the not-found page of the /as route
			s.serveNetwork(w, r, 0)
			return
		}
		h.Set("Cache-Control", cache)
		h.Set("Location", pagePath+q.asn)
		w.WriteHeader(http.StatusFound)
		return
	case "search":
		s.serveSearch(w, r, q)
		return
	case "bad":
		code, input, cache = http.StatusBadRequest, q.input, "no-store"
		p = panel{Title: "Lookup", Msg: badMsg}
	case "lookup":
		input = q.addr.String()
		title = input + " · openipdata.org"
		p = s.panelFor("Lookup", q.addr)
	default: // the visitor: private data
		cache = "private, no-store"
		title, desc, canon = homeTitle, homeDesc, s.canon("/")
		if a, ok := clientAddr(r, s.realIP); ok {
			visitor = a
			input = a.String()
			p = s.panelFor("Your connection", a)
			if i := slices.IndexFunc(p.Rows, func(r row) bool { return r.Own }); i >= 0 {
				p.Rows[i].Look = "/?" + a.String() // the visitor's address opens its lookup page
			}
			fill = s.addFill(&p, a)
			res = s.addDNS(&p)
			if p.Msg == "" {
				p.Hdr = s.requestHeaders(r) // the visitor's own headers, only back to the visitor
				p.Br = browserRowFor(r.UserAgent(), r.Header.Get("Sec-CH-UA"))
			}
			p.Share = s.shareOn() && p.Msg == "" // a page view stores nothing; the button makes the report
		} else {
			p = panel{Title: "Your connection", Msg: "Address unknown"}
		}
	}
	v := makeView(*s.scores.Load())
	v.Panel, v.Input, v.Title, v.Fill, v.Res = &p, input, title, fill, res
	v.Pnl = p.Copy
	if p.Msg == "" && (q.kind == "lookup" || q.kind == "visitor") {
		if na, ok := netAddr(q, r, s.realIP); ok {
			v.Net = s.netPanelFor(r.Context(), na)
		}
	}
	if p.Msg == "" {
		switch q.kind {
		case "lookup":
			v.API = s.apiPanelFor("/json/"+q.addr.String(), q.addr)
		case "visitor":
			v.API = s.apiPanelFor("/json", visitor)
		}
	}
	v.Home = q.kind == "visitor"
	if v.Home {
		v.Latest = blogLatest()
	}
	p.H2 = v.Home // the heading in the header is the page's h1
	share := p.Share
	v.Share = share
	v.Desc, v.Canon = desc, canon
	v.NoIndex = q.kind == "bad" || q.kind == "lookup"
	if fill || res || v.Pnl {
		h.Set("Content-Security-Policy", s.pageCSP(fill, res, v.Pnl, share))
	}
	v.CSS = v.homeCSS()
	body, err := render(s.tmpl, "home.html", v)
	if err != nil {
		plain(w, http.StatusInternalServerError, "error\n")
		return
	}
	h.Set("Cache-Control", cache)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// serveIP answers "/ip": the visitor's own address and a newline, nothing
// else, for the page to read from the names that have one address family only.
// Any site may read it (it is only the requester's own address). Never logged.
// A command-line client on the v4 or v6 name gets the JSON instead; the
// page's script (a browser) keeps the plain address.
func (s *server) serveIP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Add("Vary", "User-Agent")
	if isCLI(r) && s.onFamilyHost(r) {
		s.serveOwnJSON(w, r)
		return
	}
	h.Set("Cache-Control", "no-store")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	code, body := http.StatusOK, "unknown\n"
	if a, ok := clientAddr(r, s.realIP); ok {
		body = a.String() + "\n"
		s.noteOther(r, a) // r=<report id>: the report gets this address when it is the other family's
	} else {
		code = http.StatusServiceUnavailable
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		w.Write([]byte(body))
	}
}

// onFamilyHost says whether the request was made to the V4_URL or V6_URL name.
func (s *server) onFamilyHost(r *http.Request) bool {
	h := strings.ToLower(r.Host)
	if x, _, err := net.SplitHostPort(h); err == nil {
		h = x
	} else {
		h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]") // "[::1]" without a port
	}
	return slices.Contains(s.familyHosts, h)
}

func plain(w http.ResponseWriter, code int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	w.Write([]byte(body))
}

// validHeaderName allows letters, digits and "-" only.
func validHeaderName(n string) bool {
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func fatal(name string, err error) {
	fmt.Fprintf(os.Stderr, "openipdata-site: %s: %v\n", name, err)
	os.Exit(2)
}

func main() {
	showVersion := flag.Bool("version", false, "print the version (the commit) and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	listen := os.Getenv("LISTEN")
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	if _, port, err := net.SplitHostPort(listen); err != nil {
		fatal("LISTEN", err)
	} else if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		fatal("LISTEN", errors.New("invalid port"))
	}
	mmdb := os.Getenv("GEO_MMDB")
	if mmdb == "" {
		mmdb = "/var/lib/openipdata-site/data/openipdata-org-ip2geo.mmdb"
	}
	if info, err := os.Stat(mmdb); err == nil && info.IsDir() {
		fatal("GEO_MMDB", errors.New("is a directory"))
	}
	asnDB := os.Getenv("ASN_MMDB")
	if asnDB == "" {
		asnDB = "/var/lib/openipdata-site/data/openipdata-org-ip2asn.mmdb"
	}
	if info, err := os.Stat(asnDB); err == nil && info.IsDir() {
		fatal("ASN_MMDB", errors.New("is a directory"))
	}
	asnSQLite := os.Getenv("ASN_SQLITE")
	if asnSQLite == "" {
		asnSQLite = "/var/lib/openipdata-site/data/asn-openipdata.sqlite3"
	}
	if info, err := os.Stat(asnSQLite); err == nil && info.IsDir() {
		fatal("ASN_SQLITE", errors.New("is a directory"))
	}
	// Unset means X-Real-IP, so a new binary under an older unit still sees
	// the visitor behind nginx; set but empty means no header is trusted.
	realIP, isSet := os.LookupEnv("REAL_IP_HEADER")
	if !isSet {
		realIP = "X-Real-IP"
	}
	if realIP != "" && !validHeaderName(realIP) {
		fatal("REAL_IP_HEADER", errors.New("not a header name"))
	}
	if v := os.Getenv("TEST_IP"); v != "" {
		a, ok := parseAddr(v)
		if !ok {
			fatal("TEST_IP", errors.New("not an IP address"))
		}
		testAddr = a
		log.Printf("TEST_IP is set: a loopback request without the %s header is shown this address", realIP)
	}

	urls := map[string]string{"V4_URL": "https://v4.openipdata.org/", "V6_URL": "https://v6.openipdata.org/"}
	for _, name := range []string{"V4_URL", "V6_URL"} {
		if v, isSet := os.LookupEnv(name); isSet {
			urls[name] = v
		}
		if err := checkURL(urls[name]); err != nil {
			fatal(name, err)
		}
	}

	zone := os.Getenv("BEACON_ZONE") // off unless set: the row needs the DNS server and its certificate
	if err := checkZone(zone); zone != "" && err != nil {
		fatal("BEACON_ZONE", err)
	}
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	if err := checkRedisAddr(redisAddr); err != nil {
		fatal("REDIS_ADDR", err)
	}

	shareOn := true
	switch os.Getenv("SHARE") {
	case "", "on":
	case "off":
		shareOn = false
	default:
		fatal("SHARE", errors.New("must be on or off"))
	}
	origin := defaultOrigin
	if v := os.Getenv("SITE_ORIGIN"); v != "" {
		o, err := checkOrigin(v)
		if err != nil {
			fatal("SITE_ORIGIN", err)
		}
		origin = o
	}

	s, err := newServer(mmdb, asnDB, realIP)
	if err != nil {
		fatal("start", err)
	}
	s.useShare(shareOn, origin)
	s.useURLs(urls["V4_URL"], urls["V6_URL"])
	s.useBeacon(zone, redisAddr)
	s.useASN(asnSQLite)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.watch(ctx)

	srv := &http.Server{
		Addr:              listen,
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fatal("LISTEN", err)
	}
	log.Printf("openipdata-site %s listening on %s, geo %s, asn %s, asn data %s, real ip header %q", version, listen, mmdb, asnDB, asnSQLite, realIP)
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		stop() // a second signal now kills the process at once
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		close(done)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		fatal("serve", err)
	}
	<-done
}
