package main

// The ASN data: the SQLite file the worker publishes (asn-openipdata.sqlite3).
// What the list pages need is read once per load into a compact slice, one
// record per network, and never holds the JSON column: the prefix counts and
// the country name come out of SQLite's own JSON functions while the table is
// read, and the announced prefixes go into one compact sorted index (pfxindex.go). A network page reads its one row (and its prefixes) from the file per
// request. The file is checked every 30 seconds and loaded again when it
// changes.

import (
	"context"
	"database/sql"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the SQLite driver (pure Go)
)

const (
	maxSlugLen = 255 // the data has slugs up to 200 characters
	maxASN     = 4294967295
	topCount   = 100 // rows of the ranking pages that are not "all"
	loadLimit  = 2 * time.Minute
)

// network is the compact record of one AS kept in memory.
type network struct {
	asn      uint32
	n4, n6   uint32  // number of IPv4 and IPv6 prefixes
	v4       int64   // IPv4 addresses
	v6       float64 // IPv6 addresses
	handle   string
	org      string
	slug     string // valid (see validSlug) or empty
	cc       string // two upper-case letters or empty
	hasPfxes bool   // the JSON was readable and had the prefix lists
}

type orgRow struct {
	slug, name string
	v4         int64
	n          int
}

type countryRow struct {
	cc, name string
	v4       int64
	n        int
}

// cachedPage is a page without visitor data, rendered once per data load.
type cachedPage struct {
	body []byte
	etag string
}

// asnData is built once per load and never changed afterwards, so a request
// may keep using it after a newer load has replaced it.
type asnData struct {
	nets      []network // by AS number
	bySlug    []int32   // indexes into nets of the networks that have a slug, by (slug, number)
	country   map[string]string
	topV4     []int32
	topV6     []int32
	topOrgs   []orgRow
	orgs      []orgRow         // every organization (valid slug), most IPv4 addresses first, ties by slug; the name search walks it
	byHandle  map[string]int32 // normalised handle (see slugify) -> index in orgs of the organization of the first network (by number) with that handle
	countries []countryRow
	pages     map[string]*cachedPage
	cslug     map[string]string // country code -> slug of its page /<slug>-asn-list (see country.go)
	cbySlug   map[string]string // slug -> country code
	cpMu      sync.Mutex
	cpg       map[string]*cachedPage // country pages, rendered on the first request of each
	sm        sitemapSet             // sitemap files, built on demand (see seo.go)
	idx       pfxIndex               // every announced prefix (see pfxindex.go)
	loadedIn  time.Duration
}

func validSlug(s string) bool {
	if s == "" || len(s) > maxSlugLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validCC(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// find returns the network with this number.
func (d *asnData) find(asn uint32) (*network, bool) {
	i, ok := slices.BinarySearchFunc(d.nets, asn, func(n network, a uint32) int {
		switch {
		case n.asn < a:
			return -1
		case n.asn > a:
			return 1
		}
		return 0
	})
	if !ok {
		return nil, false
	}
	return &d.nets[i], true
}

// org returns the indexes of the networks of one organization, by number.
func (d *asnData) org(slug string) []int32 {
	lo := sort.Search(len(d.bySlug), func(i int) bool { return d.nets[d.bySlug[i]].slug >= slug })
	hi := lo
	for hi < len(d.bySlug) && d.nets[d.bySlug[hi]].slug == slug {
		hi++
	}
	return d.bySlug[lo:hi]
}

// countryName is the name the data has for a code, or the code.
func (d *asnData) countryName(cc string) string {
	if n := d.country[cc]; n != "" {
		return n
	}
	return cc
}

// The JSON functions are guarded by json_valid: one malformed row must not
// stop the load, it only has no prefix counts.
const loadSQL = `SELECT asn, handle, organization, organization_slug, country, ipv4_amount, ipv6_amount,
 CASE WHEN json_valid(json) THEN json_array_length(json, '$.prefixes.ipv4') END,
 CASE WHEN json_valid(json) THEN json_array_length(json, '$.prefixes.ipv6') END,
 CASE WHEN json_valid(json) THEN json_extract(json, '$.metadata.country') END
 FROM asn ORDER BY asn`

// loadASN reads the table into memory.
func loadASN(ctx context.Context, db *sql.DB) (*asnData, error) {
	started := time.Now()
	rows, err := db.QueryContext(ctx, loadSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d := &asnData{country: map[string]string{}}
	for rows.Next() {
		var (
			asn                        int64
			handle, org, slug, cc, cnm sql.NullString
			v4                         sql.NullInt64
			v6                         sql.NullFloat64
			n4, n6                     sql.NullInt64
		)
		if err := rows.Scan(&asn, &handle, &org, &slug, &cc, &v4, &v6, &n4, &n6, &cnm); err != nil {
			return nil, err
		}
		if asn < 1 || asn > maxASN {
			continue
		}
		n := network{
			asn:    uint32(asn),
			handle: strings.TrimSpace(handle.String),
			org:    strings.TrimSpace(org.String),
			v4:     max(v4.Int64, 0),
			v6:     max(v6.Float64, 0),
		}
		if validSlug(slug.String) {
			n.slug = slug.String
		}
		if validCC(cc.String) {
			n.cc = cc.String
			if name := strings.TrimSpace(cnm.String); name != "" && d.country[n.cc] == "" {
				d.country[n.cc] = name
			}
		}
		if n4.Valid && n6.Valid && n4.Int64 >= 0 && n6.Int64 >= 0 {
			n.n4, n.n6, n.hasPfxes = uint32(n4.Int64), uint32(n6.Int64), true
		}
		d.nets = append(d.nets, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	d.nets = slices.Clip(d.nets)
	rows.Close() // the prefix read below needs no open result set
	if d.idx, err = loadPrefixes(ctx, db); err != nil {
		return nil, err
	}

	for i := range d.nets {
		if d.nets[i].slug != "" {
			d.bySlug = append(d.bySlug, int32(i))
		}
	}
	slices.SortFunc(d.bySlug, func(a, b int32) int {
		if c := strings.Compare(d.nets[a].slug, d.nets[b].slug); c != 0 {
			return c
		}
		return int(d.nets[a].asn) - int(d.nets[b].asn)
	})
	d.bySlug = slices.Clip(d.bySlug)

	// The rankings.
	idx := make([]int32, len(d.nets))
	for i := range idx {
		idx[i] = int32(i)
	}
	d.topV4 = top(d, idx, func(n *network) float64 { return float64(n.v4) })
	d.topV6 = top(d, idx, func(n *network) float64 { return n.v6 })

	var orgs []orgRow
	for lo := 0; lo < len(d.bySlug); {
		slug := d.nets[d.bySlug[lo]].slug
		o := orgRow{slug: slug}
		hi := lo
		for ; hi < len(d.bySlug) && d.nets[d.bySlug[hi]].slug == slug; hi++ {
			n := &d.nets[d.bySlug[hi]]
			o.v4 += n.v4
			if o.name == "" {
				o.name = n.org
			}
		}
		o.n = hi - lo
		orgs = append(orgs, o)
		lo = hi
	}
	slices.SortFunc(orgs, func(a, b orgRow) int {
		if a.v4 != b.v4 {
			if a.v4 > b.v4 {
				return -1
			}
			return 1
		}
		return strings.Compare(a.slug, b.slug)
	})
	d.orgs = slices.Clip(orgs)
	nTop := 0 // the ranking only has organizations with addresses; they come first
	for nTop < len(orgs) && orgs[nTop].v4 > 0 {
		nTop++
	}
	d.topOrgs = orgs[:min(topCount, nTop)]

	orgIdx := make(map[string]int32, len(orgs))
	for i := range orgs {
		orgIdx[orgs[i].slug] = int32(i)
	}
	d.byHandle = map[string]int32{}
	for i := range d.nets { // by number, so the first network of a handle wins
		n := &d.nets[i]
		if n.slug == "" || n.handle == "" {
			continue
		}
		if h := slugify(n.handle); h != "" {
			if _, dup := d.byHandle[h]; !dup {
				d.byHandle[h] = orgIdx[n.slug]
			}
		}
	}

	byCC := map[string]*countryRow{}
	for i := range d.nets {
		n := &d.nets[i]
		if n.cc == "" {
			continue
		}
		c := byCC[n.cc]
		if c == nil {
			c = &countryRow{cc: n.cc, name: d.countryName(n.cc)}
			byCC[n.cc] = c
		}
		c.v4 += n.v4
		c.n++
	}
	for _, c := range byCC {
		d.countries = append(d.countries, *c)
	}
	slices.SortFunc(d.countries, func(a, b countryRow) int {
		if a.v4 != b.v4 {
			if a.v4 > b.v4 {
				return -1
			}
			return 1
		}
		return strings.Compare(a.cc, b.cc)
	})
	d.indexCountries()
	d.loadedIn = time.Since(started)
	return d, nil
}

// top returns the indexes of the topCount networks with the largest positive
// amount, ties by number. idx is not changed.
func top(d *asnData, idx []int32, amount func(*network) float64) []int32 {
	c := slices.Clone(idx)
	slices.SortFunc(c, func(a, b int32) int {
		x, y := amount(&d.nets[a]), amount(&d.nets[b])
		switch {
		case x > y:
			return -1
		case x < y:
			return 1
		}
		return int(d.nets[a].asn) - int(d.nets[b].asn) // numbers are below 2^32, so the difference fits an int
	})
	n := 0
	for n < len(c) && n < topCount && amount(&d.nets[c[n]]) > 0 {
		n++
	}
	return slices.Clip(c[:n])
}

// asnLoaded is one load: the open file and what was read from it.
type asnLoaded struct {
	db   *sql.DB
	data *asnData
	st   fileStatus
}

// asnFile is the SQLite file kept open for the network page and loaded into
// memory for the list pages. A changed file is opened and read again outside
// the lock, then swapped in under the write lock, so a request that is using
// the old handle (it holds the read lock) is never cut off.
type asnFile struct {
	path   string
	onLoad func(*asnData) // renders the pages that hold no visitor data; runs before the swap

	mu  sync.RWMutex
	cur *asnLoaded // nil until a file loaded

	// Only reload (the watch goroutine, or a test) touches these.
	sig     fileSig
	lastErr string
}

// dsn is the file as a read-only, immutable SQLite URI (no locks, no journal:
// the worker replaces the file by rename, never writes into it).
func (f *asnFile) dsn() (string, error) {
	abs, err := filepath.Abs(f.path)
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String() + "?mode=ro&immutable=1", nil
}

// reload loads the file when it changed since the last good load and reports
// whether a new load is now in use. A missing or unreadable file keeps what is
// in use (or nothing) and logs once per distinct error.
func (f *asnFile) reload() bool {
	if f.path == "" {
		return false
	}
	info, err := os.Stat(f.path)
	if err != nil {
		f.fail(err)
		return false
	}
	if info.IsDir() {
		f.fail(&os.PathError{Op: "open", Path: f.path, Err: os.ErrInvalid})
		return false
	}
	sig := fileSig{info.ModTime(), info.Size()}
	if sig == f.sig {
		return false
	}
	dsn, err := f.dsn()
	if err != nil {
		f.fail(err)
		return false
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		f.fail(err)
		return false
	}
	db.SetMaxOpenConns(4) // each connection has its own page cache
	db.SetConnMaxIdleTime(0)
	ctx, cancel := context.WithTimeout(context.Background(), loadLimit)
	defer cancel()
	data, err := loadASN(ctx, db)
	if err != nil {
		db.Close()
		f.fail(err)
		return false
	}
	if f.onLoad != nil {
		f.onLoad(data)
	}
	f.mu.Lock()
	old := f.cur
	f.cur = &asnLoaded{db: db, data: data, st: fileStatus{Loaded: true, Built: info.ModTime().UTC(), LoadedAt: time.Now(), Size: info.Size()}}
	if old != nil {
		old.db.Close() // no request is inside: they hold the read lock
	}
	f.mu.Unlock()
	f.sig, f.lastErr = sig, ""
	log.Printf("loaded %s: %d networks, %d prefixes in %s", f.path, len(data.nets), data.idx.size(), data.loadedIn.Round(time.Millisecond))
	return true
}

func (f *asnFile) fail(err error) {
	if msg := err.Error(); msg != f.lastErr {
		f.lastErr = msg
		log.Printf("asn data %s: %v (keeping what is loaded, if anything)", f.path, err)
	}
}

// data is what is loaded in memory, or nil. It is never changed, so it may be
// used after the call.
func (f *asnFile) data() *asnData {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.cur == nil {
		return nil
	}
	return f.cur.data
}

// status describes the loaded file and its data (nil when nothing is loaded).
func (f *asnFile) status() (fileStatus, *asnData) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.cur == nil {
		return fileStatus{}, nil
	}
	return f.cur.st, f.cur.data
}

// read runs fn with the open file and the data of the same load. The file is
// valid only inside fn; fn must not write to the client. It reports false
// when nothing is loaded.
func (f *asnFile) read(fn func(db *sql.DB, d *asnData)) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.cur == nil {
		return false
	}
	fn(f.cur.db, f.cur.data)
	return true
}
