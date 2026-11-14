// Command openipdata-site serves the openipdata.org website.
//
// Settings (environment):
//
//	LISTEN    address to listen on (default 127.0.0.1:8080)
//	GEO_MMDB  path of the ip2geo MMDB whose metadata holds the scores
//	          (default /var/lib/openipdata-site/data/ip2geo-openipdata.mmdb)
package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
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

// page is one rendered home page with its validators.
type page struct {
	body []byte
	etag string
	size string
}

type fileSig struct {
	mod  time.Time
	size int64
}

type server struct {
	tmpl  *template.Template
	mmdb  string
	csp   string
	home  atomic.Pointer[page]
	fonts map[string][]byte

	// Only the reload goroutine (or a test calling check) touches these.
	sig     fileSig
	lastErr string
}

func newServer(mmdb string) (*server, error) {
	tmpl, err := template.New("home").Parse(homeTemplate)
	if err != nil {
		return nil, err
	}
	s := &server{tmpl: tmpl, mmdb: mmdb, csp: csp(), fonts: map[string][]byte{}}
	for _, name := range []string{"geist-latin.woff2", "geist-mono-latin.woff2"} {
		b, err := fontFiles.ReadFile("static/fonts/" + name)
		if err != nil {
			return nil, err
		}
		s.fonts["/fonts/"+name] = b
	}
	// A missing or unreadable MMDB is not fatal: the page then shows dashes.
	sc, err := readScores(mmdb)
	if err != nil {
		// sig stays empty, so the first check tries the file again.
		log.Printf("scores: %v (the page shows dashes until the file is readable)", err)
	} else if info, serr := os.Stat(mmdb); serr == nil {
		s.sig = fileSig{info.ModTime(), info.Size()}
	}
	if err := s.publish(sc); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *server) publish(sc scores) error {
	b, err := render(s.tmpl, sc)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	s.home.Store(&page{body: b, etag: `"` + hex.EncodeToString(sum[:16]) + `"`, size: strconv.Itoa(len(b))})
	return nil
}

// check reloads the scores when the MMDB file changed. A failed reload keeps
// the page that is served and logs one line per distinct error.
func (s *server) check() {
	info, err := os.Stat(s.mmdb)
	if err != nil {
		s.fail(err)
		return
	}
	sig := fileSig{info.ModTime(), info.Size()}
	if sig == s.sig {
		return
	}
	sc, err := readScores(s.mmdb)
	if err != nil {
		s.fail(err)
		return
	}
	if err := s.publish(sc); err != nil {
		s.fail(err)
		return
	}
	s.sig, s.lastErr = sig, ""
	log.Printf("reloaded scores from %s", s.mmdb)
}

func (s *server) fail(err error) {
	if msg := err.Error(); msg != s.lastErr {
		s.lastErr = msg
		log.Printf("reload failed, keeping the current page: %v", err)
	}
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

func (s *server) serveHome(w http.ResponseWriter, r *http.Request) {
	pg := s.home.Load()
	h := w.Header()
	h.Set("ETag", pg.etag)
	// This page shows no visitor data yet, so a shared cache may keep it for
	// a minute. Once it shows anything about the visitor, that response must
	// be "private, no-store".
	h.Set("Cache-Control", "public, max-age=60")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", s.csp)
	if etagMatch(r.Header.Get("If-None-Match"), pg.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", pg.size)
	w.Write(pg.body)
}

// etagMatch reports whether an If-None-Match value names etag (weak
// comparison, as for GET).
func etagMatch(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == etag {
			return true
		}
	}
	return false
}

func plain(w http.ResponseWriter, code int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	w.Write([]byte(body))
}

func fatal(name string, err error) {
	fmt.Fprintf(os.Stderr, "openipdata-site: %s: %v\n", name, err)
	os.Exit(2)
}

func main() {
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

	s, err := newServer(mmdb)
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
	log.Printf("openipdata-site %s listening on %s, scores from %s", version, listen, mmdb)
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
