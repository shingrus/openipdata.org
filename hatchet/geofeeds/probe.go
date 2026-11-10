package geofeeds

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/net/idna"

	"openipdata.org/hatchet/schema"
)

// Files probe-domains writes to <data>/geofeeds/.
const (
	// DomainsFile holds one JSON line per probed domain, appended as probes
	// finish and compacted after each run. Like RDAPFile it is
	// sync-geofeeds' own state: losing it costs a few days of probes.
	DomainsFile = "domains.jsonl"
	// DomainPrefixesFile (url,prefix) lists the announced prefixes of the ASNs
	// of each found domain. export-rows keeps only rows inside them for a feed
	// that only a domain probe found, and discover lists exactly the URLs in it:
	// both see the same snapshot. Replaced at the end of each run.
	DomainPrefixesFile = "domain-prefixes.csv"
)

// sourceDomain labels the feeds found by probing an ASN's website domain.
const sourceDomain = "domain"

// Probing: recheck intervals, politeness, limits.
const (
	domainFoundRecheck  = 7 * 24 * time.Hour
	domainNoneRecheck   = 30 * 24 * time.Hour
	domainFailedRecheck = 24 * time.Hour
	domainWorkers       = 8
	domainTimeout       = 15 * time.Second
	defaultDomainBudget = 5000
	domainMaxBody       = 1 << 20 // bytes read per probe
	domainSampleLines   = 50      // data lines checked for a valid row
	domainProgressEvery = 5 * time.Minute
	// domainRunLimit ends the probing well inside the task's execution
	// timeout: what is left is still due next run.
	domainRunLimit = 2 * time.Hour
	// domainTrustFor is how long a found feed counts after its last good probe.
	// It covers a few failed rechecks, and drops a site that stopped serving it.
	domainTrustFor = 45 * 24 * time.Hour
	// minAnnounced4 and minAnnounced6 are the shortest ASN prefixes that vouch
	// for rows: the rows themselves cannot be broader (see parseRow).
	minAnnounced4 = 8
	minAnnounced6 = 16
)

// Probe results.
const (
	domainFound  = "found"
	domainNone   = "none"
	domainFailed = "failed" // no answer worth trusting: tried again after a day
)

// domainRecord is one probed domain.
type domainRecord struct {
	Domain  string  `json:"domain"`
	ASNs    []int64 `json:"asns"`
	Checked string  `json:"checked"`
	Next    string  `json:"next"`         // when it is due again
	OK      string  `json:"ok,omitempty"` // the last probe that found the feed
	Status  string  `json:"status"`       // found, none, or failed
	URL     string  `json:"url,omitempty"`
}

// listed reports whether the record's feed still counts: found, and its last
// good probe is recent enough. A failed recheck keeps the last answer.
func (r domainRecord) listed(now time.Time) bool {
	return r.Status == domainFound && r.URL != "" && r.OK != "" && now.Sub(parseTime(r.OK)) <= domainTrustFor
}

// asnDomain is one row of public.asn_domain.
type asnDomain struct {
	ASN    int64
	Domain string
}

// DomainsOutput is the probe-domains task's result.
type DomainsOutput struct {
	Domains         int     `json:"domains"`           // distinct domains in public.asn_domain
	Invalid         int     `json:"invalid,omitempty"` // domains that are not host names; not probed
	Due             int     `json:"due"`
	Probed          int     `json:"probed"`
	Found           int     `json:"found"`
	None            int     `json:"none"`
	Failed          int     `json:"failed"`
	Requests        int     `json:"requests"`
	Known           int     `json:"known"`      // domains with a stored result
	WithFeeds       int     `json:"with_feeds"` // domains whose feed counts now
	Stopped         string  `json:"stopped,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// candidateURLs lists where a domain's geofeed is tried, in order.
func candidateURLs(domain string) []string {
	return []string{
		"https://" + domain + "/geofeed.csv",
		"https://geofeed." + domain + "/geofeed.csv",
		"https://www." + domain + "/geofeed.csv",
		"https://" + domain + "/.well-known/geofeed.csv",
	}
}

// cleanDomain returns the domain as an ASCII host name, or "" if it is not one.
func cleanDomain(s string) string {
	s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
	ascii, err := idna.Lookup.ToASCII(s)
	if err != nil || !strings.Contains(ascii, ".") {
		return ""
	}
	for _, r := range ascii {
		if r != '.' && r != '-' && (r < '0' || r > '9') && (r < 'a' || r > 'z') {
			return ""
		}
	}
	if _, err := netip.ParseAddr(ascii); err == nil {
		return ""
	}
	return ascii
}

// dueDomains lists the domains to probe: never-probed first (by name), then
// the longest overdue.
func dueDomains(input map[string][]int64, known map[string]domainRecord, now time.Time) []string {
	var fresh, overdue []string
	for d := range input {
		r, ok := known[d]
		switch {
		case !ok:
			fresh = append(fresh, d)
		case !parseTime(r.Next).After(now):
			overdue = append(overdue, d)
		}
	}
	sort.Strings(fresh)
	sort.Slice(overdue, func(i, j int) bool {
		a, b := parseTime(known[overdue[i]].Next), parseTime(known[overdue[j]].Next)
		if !a.Equal(b) {
			return a.Before(b)
		}
		return overdue[i] < overdue[j]
	})
	return append(fresh, overdue...)
}

// probeResult is what trying all of a domain's candidate URLs found.
type probeResult struct {
	status   string
	url      string
	requests int
}

// Outcomes of one request.
const (
	probeMiss  = iota // the host answered, and it is not a feed (or there is no such host)
	probeHit          // a feed
	probeRetry        // no answer worth trusting: a timeout, a server error, a rate limit
)

// probeDomain tries the candidate URLs in order and stops at the first feed.
// Without one, the result is failed if any request got no answer worth
// trusting, else none. After a request that never got an HTTP response, the
// other candidates on the same host are skipped.
func probeDomain(ctx context.Context, client *http.Client, candidates []string) probeResult {
	res := probeResult{status: domainNone}
	down := map[string]int{} // hosts that gave no HTTP response, with the outcome
	for _, u := range candidates {
		if ctx.Err() != nil {
			return probeResult{status: domainFailed, requests: res.requests}
		}
		host := urlHost(u)
		if outcome, ok := down[host]; ok {
			if outcome == probeRetry {
				res.status = domainFailed
			}
			continue
		}
		res.requests++
		outcome, noResponse := probeURL(ctx, client, u)
		if noResponse {
			down[host] = outcome
		}
		switch outcome {
		case probeHit:
			res.status, res.url = domainFound, u
			return res
		case probeRetry:
			res.status = domainFailed
		}
	}
	return res
}

// probeURL requests one URL. A hit is HTTP 200 with a body that is not HTML
// and has a valid geofeed row among its first lines. noResponse reports that
// the request failed before any HTTP response.
func probeURL(ctx context.Context, client *http.Client, rawURL string) (outcome int, noResponse bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return probeMiss, true
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/csv, text/plain;q=0.9, */*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return probeErrorOutcome(err), true
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return probeRetry, false
	default:
		return probeMiss, false
	}
	body := bufio.NewReaderSize(io.LimitReader(resp.Body, domainMaxBody), 64*1024)
	if looksLikeHTML(body) || !hasFeedRow(body) {
		return probeMiss, false
	}
	return probeHit, false
}

// probeErrorOutcome tells a definite "no" from an error that says nothing
// about the site (the VM's network may be down): only a name that does not
// exist, a refused connection, a refused address or a TLS failure are misses.
func probeErrorOutcome(err error) int {
	var dnsErr *net.DNSError
	switch status := classifyError(err); {
	case status == feedBlocked, status == feedTLSError:
		return probeMiss
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return probeMiss
	case errors.Is(err, syscall.ECONNREFUSED):
		return probeMiss
	}
	return probeRetry
}

// hasFeedRow reports whether one of the first data lines is a valid geofeed row.
func hasFeedRow(r *bufio.Reader) bool {
	for data := 0; data < domainSampleLines; {
		line, tooLong, err := readLine(r)
		if err != nil {
			return false
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" || line[0] == '#' {
			continue
		}
		data++
		if tooLong {
			continue
		}
		fields, ok := splitLine(line)
		if !ok {
			continue
		}
		if _, reason := parseRow(fields); reason == "" || reason == dropBadRegion || reason == dropBadCity {
			return true
		}
	}
	return false
}

// ProbeDomains reads the website domain of each ASN from Postgres (read only,
// no DDL), probes the ones that are due, up to budget (or the default, if not
// positive), and appends the results to DomainsFile. At the end it replaces
// DomainPrefixesFile and compacts DomainsFile. Domains no longer in
// public.asn_domain are forgotten then.
func (p *Pipeline) ProbeDomains(ctx context.Context, budget int) (DomainsOutput, error) {
	started := time.Now()
	out := DomainsOutput{}
	if err := p.CheckPGSQL(); err != nil {
		return out, err
	}
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return out, err
	}
	rows, err := p.loadDomainRows(ctx)
	if err != nil {
		return out, err
	}
	input := map[string][]int64{} // domain -> its ASNs
	for _, row := range rows {
		d := cleanDomain(row.Domain)
		if d == "" {
			out.Invalid++
			continue
		}
		if !slices.Contains(input[d], row.ASN) {
			input[d] = append(input[d], row.ASN)
		}
	}
	for _, asns := range input {
		slices.Sort(asns)
	}
	out.Domains = len(input)
	if len(input) == 0 {
		// An empty table would wipe every stored result.
		return out, errors.New("public.asn_domain has no usable domains")
	}

	path := filepath.Join(p.dir(), DomainsFile)
	known, err := loadDomains(path)
	if err != nil {
		return out, err
	}
	now := p.clock().UTC()
	due := dueDomains(input, known, now)
	out.Due = len(due)
	if budget <= 0 {
		budget = defaultDomainBudget
	}
	if len(due) > budget {
		due = due[:budget]
	}
	p.log("domains: %d domains, %d due, probing %d with %d workers", out.Domains, out.Due, len(due), domainWorkers)

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return out, err
	}
	defer file.Close()
	w := bufio.NewWriter(file)

	candidates := p.candidates
	if candidates == nil {
		candidates = candidateURLs
	}
	client := newHTTPClient(domainTimeout, p.allowPrivate)
	deadline := started.Add(domainRunLimit)
	parent := ctx
	ctx, stop := context.WithCancel(parent)
	defer stop()
	var (
		mu           sync.Mutex // known, out, w, localErr, lastProgress
		localErr     error
		lastProgress = time.Now()
		jobs         = make(chan string)
		wg           sync.WaitGroup
	)
	// Each finished domain is written and flushed at once, so a retried task
	// resumes where it stopped.
	record := func(d string, res probeResult) {
		mu.Lock()
		defer mu.Unlock()
		prev, hadPrev := known[d]
		r := settle(domainRecord{Domain: d, ASNs: input[d], Status: res.status, URL: res.url}, prev, hadPrev, p.clock().UTC())
		known[d] = r
		out.Probed++
		out.Requests += res.requests
		switch res.status {
		case domainFound:
			out.Found++
		case domainNone:
			out.None++
		default:
			out.Failed++
		}
		line, _ := json.Marshal(r)
		_, err := w.Write(append(line, '\n'))
		if err == nil {
			err = w.Flush()
		}
		if err != nil && localErr == nil {
			localErr = err
			stop()
		}
		if time.Since(lastProgress) >= domainProgressEvery {
			lastProgress = time.Now()
			p.log("domains: %d/%d probed, %d with feeds, %d none, %d failed", out.Probed, len(due), out.Found, out.None, out.Failed)
		}
	}
	for range domainWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				res := probeDomain(ctx, client, candidates(d))
				if ctx.Err() != nil {
					continue // cut off: not recorded, still due
				}
				record(d, res)
			}
		}()
	}
feed:
	for _, d := range due {
		if time.Now().After(deadline) {
			out.Stopped = "out of time: the rest is due next run"
			break
		}
		select {
		case jobs <- d:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if localErr != nil {
		return out, localErr
	}
	if err := parent.Err(); err != nil {
		return out, err
	}

	// Only the domains in public.asn_domain stay, with their current ASNs.
	final := map[string]domainRecord{}
	for d, asns := range input {
		if r, ok := known[d]; ok {
			r.ASNs = asns
			final[d] = r
		}
	}
	if err := p.writeDomainPrefixes(ctx, final, now); err != nil {
		return out, err
	}
	if err := w.Flush(); err != nil {
		return out, err
	}
	if err := compactJSONL(path, final); err != nil {
		return out, err
	}
	out.Known = len(final)
	for _, r := range final {
		if r.listed(now) {
			out.WithFeeds++
		}
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("domains: %d probed (%d requests): %d with feeds, %d none, %d failed; %d known, %d with feeds, in %s%s",
		out.Probed, out.Requests, out.Found, out.None, out.Failed, out.Known, out.WithFeeds,
		time.Since(started).Round(time.Second), errSuffix(out.Stopped))
	return out, nil
}

// settle fills in the times of a fresh probe result and, for a failure, keeps
// the last good answer: it counts until it is too old to trust.
func settle(r, prev domainRecord, hadPrev bool, checked time.Time) domainRecord {
	r.Checked = checked.Format(time.RFC3339)
	switch r.Status {
	case domainFound:
		r.OK, r.Next = r.Checked, checked.Add(domainFoundRecheck).Format(time.RFC3339)
	case domainNone:
		r.Next = checked.Add(domainNoneRecheck).Format(time.RFC3339)
	default:
		r.Next = checked.Add(domainFailedRecheck).Format(time.RFC3339)
		if hadPrev && prev.listed(checked) {
			r.Status, r.URL, r.OK = prev.Status, prev.URL, prev.OK
		}
	}
	return r
}

// loadDomains reads DomainsFile, keeping the last line per domain. A line cut
// short by a crash is skipped.
func loadDomains(path string) (map[string]domainRecord, error) {
	known := map[string]domainRecord{}
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
		var r domainRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Domain != "" {
			known[r.Domain] = r
		}
	}
	return known, sc.Err()
}

// loadDomainRows reads the domains of public.asn_domain.
func (p *Pipeline) loadDomainRows(ctx context.Context) ([]asnDomain, error) {
	if p.domainRows != nil {
		return p.domainRows(ctx)
	}
	conn, err := p.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, domainsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []asnDomain
	for rows.Next() {
		var r asnDomain
		if err := rows.Scan(&r.ASN, &r.Domain); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadASNPrefixes reads the announced prefixes of the given ASNs from
// public.asn. Opened separately, after the probing, to read only the ASNs of
// the domains that turned out to have a feed.
func (p *Pipeline) loadASNPrefixes(ctx context.Context, asns []int64) (map[int64][]netip.Prefix, error) {
	if p.asnPrefixes != nil {
		return p.asnPrefixes(ctx, asns)
	}
	conn, err := p.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, prefixesQuery, asns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]netip.Prefix{}
	for rows.Next() {
		var asn int64
		var raw string
		if err := rows.Scan(&asn, &raw); err != nil {
			return nil, err
		}
		out[asn] = announcedPrefixes(raw)
	}
	return out, rows.Err()
}

// connect opens a read-only use of Postgres: no schema.Ensure, whose ALTER
// TABLE statements take locks on tables other workflows write.
func (p *Pipeline) connect(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, p.PGSQL)
	if err != nil {
		return nil, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	return conn, nil
}

// domainsQuery lists the ASNs that have a website domain. Read only.
const domainsQuery = `SELECT asn, domain FROM public.asn_domain WHERE btrim(coalesce(domain, '')) <> ''`

// prefixesQuery reads public.asn.json for the given ASNs. Like build-geo-data,
// it leaves out an ASN that build-asn-data has not seen for 7 days; a NULL
// seen_at counts only until the first run has stamped some row. Read only.
const prefixesQuery = `SELECT asn, json FROM public.asn
	WHERE asn = ANY($1)
	  AND (seen_at > now() - interval '7 days'
	       OR (seen_at IS NULL AND NOT EXISTS (SELECT 1 FROM public.asn WHERE seen_at IS NOT NULL)))`

// announcedPrefixes reads the announced prefixes from a public.asn json value.
// Unparsable ones, and ones broader than any feed row can be, are left out.
func announcedPrefixes(raw string) []netip.Prefix {
	var data struct {
		Prefixes struct {
			IPv4 []string `json:"ipv4"`
			IPv6 []string `json:"ipv6"`
		} `json:"prefixes"`
	}
	if json.Unmarshal([]byte(raw), &data) != nil {
		return nil
	}
	var out []netip.Prefix
	for _, text := range append(data.Prefixes.IPv4, data.Prefixes.IPv6...) {
		p, err := netip.ParsePrefix(strings.TrimSpace(text))
		if err != nil || p.Addr().Is4In6() {
			continue
		}
		if p.Addr().Is4() && p.Bits() < minAnnounced4 || p.Addr().Is6() && p.Bits() < minAnnounced6 {
			continue
		}
		out = append(out, p.Masked())
	}
	return out
}

// writeDomainPrefixes replaces DomainPrefixesFile with the announced prefixes
// of the ASNs of every domain whose feed counts, one row per feed URL and prefix.
func (p *Pipeline) writeDomainPrefixes(ctx context.Context, recs map[string]domainRecord, now time.Time) error {
	var asns []int64
	for _, r := range recs {
		if r.listed(now) {
			asns = append(asns, r.ASNs...)
		}
	}
	slices.Sort(asns)
	asns = slices.Compact(asns)
	var prefixes map[int64][]netip.Prefix
	if len(asns) > 0 {
		var err error
		if prefixes, err = p.loadASNPrefixes(ctx, asns); err != nil {
			return err
		}
	}
	var feeds []domainRecord
	for _, r := range recs {
		if r.listed(now) {
			feeds = append(feeds, r)
		}
	}
	sort.Slice(feeds, func(i, j int) bool { return feeds[i].URL < feeds[j].URL })
	var text strings.Builder
	w := csv.NewWriter(&text)
	w.Write([]string{"url", "prefix"})
	for _, r := range feeds {
		set := map[netip.Prefix]bool{}
		for _, asn := range r.ASNs {
			for _, prefix := range prefixes[asn] {
				set[prefix] = true
			}
		}
		list := make([]netip.Prefix, 0, len(set))
		for prefix := range set {
			list = append(list, prefix)
		}
		slices.SortFunc(list, func(a, b netip.Prefix) int {
			if c := a.Addr().Compare(b.Addr()); c != 0 {
				return c
			}
			return a.Bits() - b.Bits()
		})
		for _, prefix := range list {
			w.Write([]string{r.URL, prefix.String()})
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(p.dir(), DomainPrefixesFile), []byte(text.String()))
}

// prefixSet is a set of prefixes; contains reports whether a prefix lies
// inside one of them.
type prefixSet struct {
	prefixes     map[netip.Prefix]bool
	bits4, bits6 []int
}

func (s *prefixSet) add(p netip.Prefix) {
	if s.prefixes == nil {
		s.prefixes = map[netip.Prefix]bool{}
	}
	s.prefixes[p] = true
	bits := &s.bits6
	if p.Addr().Is4() {
		bits = &s.bits4
	}
	if !slices.Contains(*bits, p.Bits()) {
		*bits = append(*bits, p.Bits())
	}
}

// contains reports whether p equals or lies inside a prefix of the set. A nil
// set contains nothing.
func (s *prefixSet) contains(p netip.Prefix) bool {
	if s == nil {
		return false
	}
	lengths := s.bits6
	if p.Addr().Is4() {
		lengths = s.bits4
	}
	for _, bits := range lengths {
		if bits <= p.Bits() && s.prefixes[netip.PrefixFrom(p.Addr(), bits).Masked()] {
			return true
		}
	}
	return false
}

// loadDomainPrefixes reads DomainPrefixesFile: the announced prefixes of each
// domain-found feed URL. A missing file means none.
func loadDomainPrefixes(path string) (map[string]*prefixSet, error) {
	sets := map[string]*prefixSet{}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return sets, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	r := csv.NewReader(bufio.NewReader(file))
	r.FieldsPerRecord = 2
	r.ReuseRecord = true
	for line := 0; ; line++ {
		rec, err := r.Read()
		if err == io.EOF {
			return sets, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		prefix, err := netip.ParsePrefix(rec[1])
		if line == 0 || err != nil {
			continue
		}
		if sets[rec[0]] == nil {
			sets[rec[0]] = &prefixSet{}
		}
		sets[rec[0]].add(prefix.Masked())
	}
}
