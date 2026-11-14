// Command openipdata-site serves the openipdata.org website.
//
// Settings (environment):
//
//	LISTEN    address to listen on (default 127.0.0.1:8080)
//	GEO_MMDB  path of the ip2geo MMDB: city and country of an address, and in its
//	          metadata the scores (default
//	          /var/lib/openipdata-site/data/ip2geo-openipdata.mmdb)
//	ASN_MMDB  path of the ip2asn MMDB: network, AS number and announced prefix
//	          (default /var/lib/openipdata-site/data/ip2asn-openipdata.mmdb)
//	REAL_IP_HEADER  header that holds the visitor address (default X-Real-IP;
//	          set it to empty to use the address of the TCP connection).
//	          Trusted only when the connection comes from loopback, i.e. from
//	          our nginx.
//
// Both MMDBs are opened again when their file changes; a missing file only
// means unknown values on the page. Addresses are never logged.
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
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed templates/home.html
var homeTemplate string

//go:embed static/fonts/*.woff2
var fontFiles embed.FS

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

	geo, asn *mmdbFile // kept open for lookups
	realIP   string    // header that holds the visitor address, or ""
	public   func(netip.Addr) bool
}

func newServer(geoPath, asnPath, realIP string) (*server, error) {
	tmpl, err := template.New("home").Parse(homeTemplate)
	if err != nil {
		return nil, err
	}
	s := &server{
		tmpl: tmpl, csp: csp(), fonts: map[string][]byte{},
		geo: &mmdbFile{path: geoPath}, asn: &mmdbFile{path: asnPath},
		realIP: realIP, public: isPublic,
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
	return s, nil
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
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		plain(w, http.StatusMethodNotAllowed, "method not allowed\n")
		return
	}
	switch p := r.URL.Path; {
	case p == "/":
		s.serveHome(w, r)
	case p == "/healthz":
		w.Header().Set("Cache-Control", "no-store")
		plain(w, http.StatusOK, "ok "+version+"\n")
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
	default:
		plain(w, http.StatusNotFound, "not found\n")
	}
}

// serveHome answers "/": the visitor's own connection (no query), a lookup
// (the raw query is one address), the redirect of the lookup form (q=<address>)
// or "Not an IP address". Only a lookup page holds no visitor data, so only it
// may be cached.
func (s *server) serveHome(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", s.csp)

	q := parseQuery(r.URL.RawQuery)
	code, input, cache := http.StatusOK, "", "public, max-age=300"
	title := "openipdata.org"
	var p panel
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
	case "bad":
		code, input, cache = http.StatusBadRequest, q.input, "no-store"
		p = panel{Title: "Lookup", Msg: "Not an IP address"}
	case "lookup":
		input = q.addr.String()
		title = input + " · openipdata.org"
		p = s.panelFor("Lookup", q.addr)
	default: // the visitor: private data
		cache = "private, no-store"
		if a, ok := clientAddr(r, s.realIP); ok {
			input = a.String()
			p = s.panelFor("Your connection", a)
		} else {
			p = panel{Title: "Your connection", Msg: "Address unknown"}
		}
	}
	v := makeView(*s.scores.Load())
	v.Panel, v.Input, v.Title = &p, input, title
	body, err := render(s.tmpl, v)
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
		mmdb = "/var/lib/openipdata-site/data/ip2geo-openipdata.mmdb"
	}
	if info, err := os.Stat(mmdb); err == nil && info.IsDir() {
		fatal("GEO_MMDB", errors.New("is a directory"))
	}
	asnDB := os.Getenv("ASN_MMDB")
	if asnDB == "" {
		asnDB = "/var/lib/openipdata-site/data/ip2asn-openipdata.mmdb"
	}
	if info, err := os.Stat(asnDB); err == nil && info.IsDir() {
		fatal("ASN_MMDB", errors.New("is a directory"))
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

	s, err := newServer(mmdb, asnDB, realIP)
	if err != nil {
		fatal("start", err)
	}
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
	log.Printf("openipdata-site %s listening on %s, geo %s, asn %s, real ip header %q", version, listen, mmdb, asnDB, realIP)
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
