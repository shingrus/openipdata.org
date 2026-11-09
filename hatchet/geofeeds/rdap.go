package geofeeds

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RDAPFile holds every RDAP lookup result, one JSON line each, appended as
// lookups finish and compacted to the latest line per block after each run.
// It is sync-geofeeds' own state: losing it costs a few days of lookups.
const RDAPFile = "rdap.jsonl"

// rdapRegistry is one RDAP service for space the whois dumps do not cover:
// ARIN has no public dump, and LACNIC's dump leaves out the blocks Brazil's
// and Mexico's registries manage.
type rdapRegistry struct {
	Name      string          // source label in blocks.csv and urls.csv
	StatsURL  string          // RIR delegated-extended stats file listing the blocks
	Countries map[string]bool // only blocks of these countries; nil for all
	BaseURL   string          // lookups go to <BaseURL>/ip/<prefix>
	Interval  time.Duration   // between lookups
	Budget    int             // lookups per run
	Backoff   time.Duration   // first wait after a 429 without Retry-After; doubles each time
}

const lacnicStats = "https://ftp.lacnic.net/pub/stats/lacnic/delegated-lacnic-extended-latest"

var defaultRDAP = []rdapRegistry{
	{"arin", "https://ftp.arin.net/pub/stats/arin/delegated-arin-extended-latest", nil,
		"https://rdap.arin.net/registry", 250 * time.Millisecond, 20000, 15 * time.Second},
	{"registro.br", lacnicStats, map[string]bool{"BR": true},
		"https://rdap.registro.br", time.Second, 3000, 15 * time.Second},
	{"lacnic-mx", lacnicStats, map[string]bool{"MX": true},
		"https://rdap.lacnic.net/rdap", time.Second, 2000, 15 * time.Second},
}

// Recheck intervals and rate-limit handling.
const (
	rdapFoundRecheck  = 7 * 24 * time.Hour
	rdapNoneRecheck   = 30 * 24 * time.Hour
	rdapFailedRecheck = 24 * time.Hour
	rdapMaxBackoff    = 5 * time.Minute
	rdapMaxLimited    = 5  // 429s in a row before a registry stops for this run
	rdapMaxFailed     = 10 // failed lookups in a row before a registry stops for this run

	// rdapRunLimit ends the lookups well inside the task's execution timeout:
	// what is left is still due next run.
	rdapRunLimit = 2 * time.Hour
	// rdapTrustFor is how long a result counts after its last good lookup. It
	// covers a few failed rechecks, and drops a block that left the registry.
	rdapTrustFor = 45 * 24 * time.Hour
	// rdapForgetAfter removes a result nobody has looked up for this long: its
	// block is no longer in the stats file.
	rdapForgetAfter = 90 * 24 * time.Hour
)

// rdapRecord is one lookup result.
type rdapRecord struct {
	Registry string   `json:"registry"`
	Query    string   `json:"query"` // the block looked up
	Checked  string   `json:"checked"`
	Next     string   `json:"next"`         // when it is due again
	OK       string   `json:"ok,omitempty"` // the last lookup that got an answer
	Status   string   `json:"status"`       // found, none, or failed (never answered)
	Start    string   `json:"start,omitempty"`
	End      string   `json:"end,omitempty"` // the network the registry answered with
	Feeds    []string `json:"feeds,omitempty"`
}

func (r rdapRecord) key() string { return r.Registry + "|" + r.Query }

// trusted reports whether the record's feeds still count: its last good
// lookup is recent enough.
func (r rdapRecord) trusted(now time.Time) bool {
	ok := r.OK
	if ok == "" && r.Status != "failed" {
		ok = r.Checked // written before OK existed
	}
	return ok != "" && now.Sub(parseTime(ok)) <= rdapTrustFor
}

// RDAPStats describes one registry in a run.
type RDAPStats struct {
	Status      string `json:"status"` // ok, stopped (rate limited, failing, or out of time), or failed
	Blocks      int    `json:"blocks"` // blocks in the stats file
	Due         int    `json:"due"`
	Lookups     int    `json:"lookups"`
	Found       int    `json:"found"`
	RateLimited int    `json:"rate_limited"`
	Failed      int    `json:"failed"`
	Error       string `json:"error,omitempty"`
}

// RDAPOutput is the rdap-lookups task's result.
type RDAPOutput struct {
	Registries      map[string]RDAPStats `json:"registries"`
	Known           int                  `json:"known"` // blocks with a stored result
	WithFeeds       int                  `json:"with_feeds"`
	DurationSeconds float64              `json:"duration_seconds"`
}

// LookupRDAP looks up the blocks that are due, up to each registry's budget
// (or budget, if positive), and appends the results to RDAPFile. Registries
// run side by side; a registry that fails is reported, not fatal.
func (p *Pipeline) LookupRDAP(ctx context.Context, budget int) (RDAPOutput, error) {
	started := time.Now()
	out := RDAPOutput{Registries: map[string]RDAPStats{}}
	dumpDir := filepath.Join(p.dir(), "dumps")
	if err := os.MkdirAll(dumpDir, 0o750); err != nil {
		return out, err
	}
	path := filepath.Join(p.dir(), RDAPFile)
	known, err := loadRDAP(path)
	if err != nil {
		return out, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return out, err
	}
	w := bufio.NewWriter(file)
	var mu sync.Mutex // w, known, out
	record := func(r rdapRecord) error {
		line, _ := json.Marshal(r)
		mu.Lock()
		defer mu.Unlock()
		known[r.key()] = r
		w.Write(append(line, '\n'))
		if w.Buffered() > 32*1024 {
			return w.Flush()
		}
		return nil
	}

	registries := p.rdap
	if registries == nil {
		registries = defaultRDAP
	}
	client := newHTTPClient(time.Minute, p.allowPrivate)
	now := p.clock().UTC()

	// Registries can share a stats file (LACNIC's): each one is fetched once.
	statsClient := newHTTPClient(10*time.Minute, p.allowPrivate)
	statsErr := map[string]error{}
	for _, reg := range registries {
		if _, done := statsErr[reg.StatsURL]; !done {
			_, statsErr[reg.StatsURL] = fetchFile(ctx, statsClient, reg.StatsURL, statsPath(dumpDir, reg.StatsURL))
		}
	}

	// A local file error stops every registry: it would fail the others too.
	parent := ctx
	ctx, stop := context.WithCancel(parent)
	defer stop()
	run := rdapRun{
		client: client, now: now, deadline: started.Add(rdapRunLimit),
		known: known, mu: &mu, record: record,
	}
	var wg sync.WaitGroup
	var localErr error
	for _, reg := range registries {
		limit := reg.Budget
		if budget > 0 {
			limit = budget
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats, err := p.lookupRegistry(ctx, run, reg, limit, statsPath(dumpDir, reg.StatsURL), statsErr[reg.StatsURL])
			mu.Lock()
			defer mu.Unlock()
			if err != nil && localErr == nil {
				localErr = err
				stop()
			}
			out.Registries[reg.Name] = stats
			p.log("rdap: %s: %s, %d blocks, %d due, %d lookups, %d with feeds, %d failed, %d rate limited%s",
				reg.Name, stats.Status, stats.Blocks, stats.Due, stats.Lookups, stats.Found, stats.Failed,
				stats.RateLimited, errSuffix(stats.Error))
		}()
	}
	wg.Wait()
	flushErr := w.Flush()
	if err := file.Close(); flushErr == nil {
		flushErr = err
	}
	if localErr != nil {
		return out, localErr
	}
	if flushErr != nil {
		return out, flushErr
	}
	if err := parent.Err(); err != nil {
		return out, err
	}
	for key, r := range known {
		if now.Sub(parseTime(r.Checked)) > rdapForgetAfter {
			delete(known, key)
		}
	}
	if err := compactRDAP(path, known); err != nil {
		return out, err
	}
	out.Known = len(known)
	for _, r := range known {
		if len(r.Feeds) > 0 {
			out.WithFeeds++
		}
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("rdap: %d blocks known, %d with feeds, in %s", out.Known, out.WithFeeds, time.Since(started).Round(time.Second))
	return out, nil
}

// statsPath is the cache file of a stats URL, named after the URL's last part.
func statsPath(dumpDir, statsURL string) string {
	name := statsURL
	if parsed, err := url.Parse(statsURL); err == nil {
		name = parsed.Host + "-" + path.Base(parsed.Path)
	}
	return filepath.Join(dumpDir, "stats-"+strings.Map(func(r rune) rune {
		if r == '.' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '-'
	}, name))
}

// rdapRun is what the registries of one run share.
type rdapRun struct {
	client   *http.Client
	now      time.Time
	deadline time.Time             // no lookup starts after it
	known    map[string]rdapRecord // guarded by mu
	mu       *sync.Mutex
	record   func(rdapRecord) error
}

// lookupRegistry runs one registry's due lookups. statsErr is the result of
// refreshing its stats file. Only a local file error is returned; everything
// else ends up in the stats.
func (p *Pipeline) lookupRegistry(ctx context.Context, run rdapRun, reg rdapRegistry, limit int,
	statsFile string, statsErr error) (RDAPStats, error) {
	stats := RDAPStats{Status: "ok"}
	client, now, known, mu, record := run.client, run.now, run.known, run.mu, run.record
	if statsErr != nil {
		if _, statErr := os.Stat(statsFile); statErr != nil {
			stats.Status, stats.Error = statusFailed, statsErr.Error()
			return stats, nil
		}
		stats.Error = "stats file: " + statsErr.Error() + " (using the cached copy)"
	}
	blocks, err := readStatsBlocks(statsFile, reg.Countries)
	if err != nil {
		os.Remove(statsFile)
		stats.Status, stats.Error = statusFailed, err.Error()
		return stats, nil
	}
	stats.Blocks = len(blocks)

	// Never-checked blocks first, then the longest overdue.
	type candidate struct {
		prefix netip.Prefix
		next   time.Time
	}
	var due []candidate
	mu.Lock()
	for _, b := range blocks {
		r, ok := known[reg.Name+"|"+b.String()]
		next := parseTime(r.Next)
		if !ok || !next.After(now) {
			due = append(due, candidate{b, next})
		}
	}
	mu.Unlock()
	sort.SliceStable(due, func(i, j int) bool { return due[i].next.Before(due[j].next) })
	stats.Due = len(due)
	if len(due) > limit {
		due = due[:limit]
	}

	ticker := time.NewTicker(reg.Interval)
	defer ticker.Stop()
	limited, failed := 0, 0
	for _, c := range due {
		if time.Now().After(run.deadline) {
			stats.Status, stats.Error = "stopped", "out of time: the rest is due next run"
			return stats, nil
		}
		var r rdapRecord
		for {
			select {
			case <-ctx.Done():
				return stats, nil
			case <-ticker.C:
			}
			var status int
			var retryAfter time.Duration
			r, status, retryAfter = lookupBlock(ctx, client, reg, c.prefix)
			if status != http.StatusTooManyRequests {
				break
			}
			// Rate limited: wait, then ask about the same block again.
			stats.RateLimited++
			limited++
			if limited >= rdapMaxLimited {
				stats.Status = "stopped"
				return stats, nil
			}
			wait := retryAfter
			if wait <= 0 {
				wait = reg.Backoff << (limited - 1)
			}
			if err := sleep(ctx, min(wait, rdapMaxBackoff)); err != nil {
				return stats, nil
			}
		}
		limited = 0
		if ctx.Err() != nil {
			return stats, nil
		}
		stats.Lookups++
		checked := time.Now().UTC()
		r.Checked = checked.Format(time.RFC3339)
		answered := r.Status != "failed"
		switch r.Status {
		case "found":
			stats.Found++
			r.OK, r.Next = r.Checked, checked.Add(rdapFoundRecheck).Format(time.RFC3339)
		case "none":
			r.OK, r.Next = r.Checked, checked.Add(rdapNoneRecheck).Format(time.RFC3339)
		default:
			stats.Failed++
			r.Next = checked.Add(rdapFailedRecheck).Format(time.RFC3339)
			// The last answer stands until a lookup gets a new one, or until
			// it is too old to trust.
			mu.Lock()
			if last, ok := known[r.key()]; ok && last.trusted(checked) {
				if last.OK == "" {
					last.OK = last.Checked
				}
				r.Status, r.Start, r.End, r.Feeds, r.OK = last.Status, last.Start, last.End, last.Feeds, last.OK
			}
			mu.Unlock()
		}
		if err := record(r); err != nil {
			return stats, err
		}
		if answered {
			failed = 0
		} else if failed++; failed >= rdapMaxFailed {
			stats.Status, stats.Error = "stopped", fmt.Sprintf("%d lookups in a row failed", failed)
			return stats, nil
		}
	}
	return stats, nil
}

// lookupBlock asks the registry about one block. It returns the HTTP status
// and, for a 429, the server's Retry-After (0 if it gave none).
func lookupBlock(ctx context.Context, client *http.Client, reg rdapRegistry, prefix netip.Prefix) (rdapRecord, int, time.Duration) {
	r := rdapRecord{Registry: reg.Name, Query: prefix.String(), Status: "failed"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.BaseURL+"/ip/"+prefix.String(), nil)
	if err != nil {
		return r, 0, 0
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/rdap+json")
	resp, err := client.Do(req)
	if err != nil {
		return r, 0, 0
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		var wait time.Duration
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			wait = time.Duration(s) * time.Second
		}
		return r, resp.StatusCode, wait
	case resp.StatusCode == http.StatusNotFound:
		r.Status = "none"
		return r, resp.StatusCode, 0
	case resp.StatusCode != http.StatusOK:
		return r, resp.StatusCode, 0
	}
	var network struct {
		StartAddress string `json:"startAddress"`
		EndAddress   string `json:"endAddress"`
		Remarks      []struct {
			Description []string `json:"description"`
		} `json:"remarks"`
		Links []struct {
			Rel  string `json:"rel"`
			Type string `json:"type"`
			Href string `json:"href"`
		} `json:"links"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&network); err != nil {
		return r, resp.StatusCode, 0
	}
	// The same rule as for whois objects: a geofeed link (RFC 9632 for RDAP),
	// or a remark that mentions "geofeed".
	var attrs []attr
	for _, remark := range network.Remarks {
		for _, line := range remark.Description {
			attrs = append(attrs, attr{"remarks", line})
		}
	}
	for _, link := range network.Links {
		if link.Rel == "geo" || strings.Contains(link.Type, "geofeed") {
			attrs = append(attrs, attr{"geofeed", link.Href})
		}
	}
	r.Start, r.End, r.Feeds = network.StartAddress, network.EndAddress, feedURLs(attrs)
	r.Status = "none"
	if len(r.Feeds) > 0 {
		r.Status = "found"
	}
	return r, resp.StatusCode, 0
}

// readStatsBlocks lists the allocated and assigned blocks of an RIR stats
// file (registry|cc|type|start|value|date|status), as prefixes.
func readStatsBlocks(path string, countries map[string]bool) ([]netip.Prefix, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var out []netip.Prefix
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "|")
		if len(f) < 7 || strings.HasPrefix(f[0], "#") || (f[6] != "allocated" && f[6] != "assigned") {
			continue
		}
		if countries != nil && !countries[f[1]] {
			continue
		}
		start, err := netip.ParseAddr(f[3])
		if err != nil {
			continue
		}
		value, err := strconv.ParseUint(f[4], 10, 64)
		if err != nil || value == 0 {
			continue
		}
		switch {
		case f[2] == "ipv4" && start.Is4():
			first := uint64(binary.BigEndian.Uint32(start.AsSlice()))
			last := first + value - 1
			if last > 0xffffffff {
				continue
			}
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(last))
			out = append(out, rangeToPrefixes(start, netip.AddrFrom4(b))...)
		case f[2] == "ipv6" && start.Is6() && value <= 128:
			out = append(out, netip.PrefixFrom(start, int(value)).Masked())
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("stats file lists no blocks")
	}
	return out, nil
}

// loadRDAP reads RDAPFile, keeping the last line per block. A line cut short
// by a crash is skipped.
func loadRDAP(path string) (map[string]rdapRecord, error) {
	known := map[string]rdapRecord{}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return known, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		var r rdapRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Registry != "" && r.Query != "" {
			known[r.key()] = r
		}
	}
	return known, sc.Err()
}

// compactRDAP rewrites RDAPFile with one line per block, via a .tmp file.
func compactRDAP(path string, known map[string]rdapRecord) error {
	keys := make([]string, 0, len(known))
	for k := range known {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	w := bufio.NewWriter(file)
	for _, k := range keys {
		line, _ := json.Marshal(known[k])
		w.Write(append(line, '\n'))
	}
	if err := w.Flush(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// networkPrefixes returns the blocks a record's feeds are linked from: the
// network the registry answered with when it lies inside the queried block,
// else the queried block. A broader answer (a parent network) must not hand
// its feed more space than was asked about.
func networkPrefixes(r rdapRecord) []netip.Prefix {
	query, err := netip.ParsePrefix(r.Query)
	if err != nil {
		return nil
	}
	start, err1 := netip.ParseAddr(r.Start)
	end, err2 := netip.ParseAddr(r.End)
	if err1 == nil && err2 == nil && start.Is4() == end.Is4() && !end.Less(start) &&
		query.Contains(start) && query.Contains(end) {
		return rangeToPrefixes(start, end)
	}
	return []netip.Prefix{query}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
