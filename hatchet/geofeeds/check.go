package geofeeds

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	feedWorkers        = 16
	perHostLimit       = 2
	feedRequestTimeout = 3 * time.Minute
	maxFeedBytes       = 256 << 20

	freshFor      = time.Hour           // a feed checked this recently is skipped, so a retried task resumes
	workingWindow = 7 * 24 * time.Hour  // the lists and the export keep feeds that worked this recently
	keepUnseenFor = 30 * 24 * time.Hour // feeds no source points to any more are checked this long
	saveEvery     = 200                 // feeds between state saves
	hostFailLimit = 5                   // unreachable fetches in a row before a host's other feeds wait for the next run
)

// Feed statuses.
const (
	feedOK         = "ok"
	feedHTML       = "html"    // a web page, not a feed
	feedEmpty      = "empty"   // no data lines
	feedInvalid    = "invalid" // data lines, none valid
	feedTooLarge   = "too_large"
	feedHTTPError  = "http_error"
	feedTimedOut   = "timeout"
	feedTLSError   = "tls_error"
	feedDNSError   = "dns_error"
	feedBlocked    = "blocked" // resolves to a non-public address
	feedConnection = "connection_error"
	feedHostDown   = "host_down" // not fetched: other feeds on its host could not be reached this run
)

// Files the check task writes to <data>/geofeeds/.
const (
	StateFile = "feeds.json" // per-URL state, saved during the run
	FeedsDir  = "feeds"      // one checked copy of each working feed
	OutDir    = "out"        // the URL lists
	ListTXT   = "geofeeds-openipdata.txt"
	ListCSV   = "geofeeds-openipdata.csv"
)

// feedState is what the check task remembers about one URL.
type feedState struct {
	Sources      string `json:"sources"`
	Blocks       int    `json:"blocks"`
	LastSeen     string `json:"last_seen"` // RFC 3339: last run whose discovery listed the URL
	Checked      string `json:"checked,omitempty"`
	Status       string `json:"status,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	Rows         int    `json:"rows"`              // valid rows the last time it worked
	LastOK       string `json:"last_ok,omitempty"` // RFC 3339
	FailingSince string `json:"failing_since,omitempty"`
}

// CheckOutput is the check task's result.
type CheckOutput struct {
	Feeds           int            `json:"feeds"`
	Requests        int            `json:"requests"`
	NotModified     int            `json:"not_modified"`
	Skipped         int            `json:"skipped"` // checked within the last hour, backing off, or on a host that stopped answering
	Statuses        map[string]int `json:"statuses"`
	Rejected        map[string]int `json:"rejected,omitempty"` // rows, by reason, in feeds fetched this run
	Working         int            `json:"working"`            // URLs in the working list
	Rows            int            `json:"rows"`               // valid rows in working feeds
	Dropped         int            `json:"dropped"`            // URLs no source listed for 30 days
	DurationSeconds float64        `json:"duration_seconds"`
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// due reports whether a feed should be fetched now. Failing feeds are retried
// every run for 3 days, then weekly, and monthly after 60 days.
func (s *feedState) due(now time.Time) bool {
	if s.Checked != "" && now.Sub(parseTime(s.Checked)) < freshFor {
		return false
	}
	if s.FailingSince == "" {
		return true
	}
	failing, sinceCheck := now.Sub(parseTime(s.FailingSince)), now.Sub(parseTime(s.Checked))
	switch {
	case failing < 3*24*time.Hour:
		return true
	case failing < 60*24*time.Hour:
		return sinceCheck >= 7*24*time.Hour-freshFor
	default:
		return sinceCheck >= 30*24*time.Hour-freshFor
	}
}

func feedPath(dir, rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".csv")
}

// fetchResult is one feed check.
type fetchResult struct {
	status      string
	httpStatus  int
	rows        int
	notModified bool
	rejected    map[string]int
	err         error // a local file error: it says nothing about the feed
}

// fetchFeed fetches one feed, revalidating the stored copy if there is one,
// and stores the valid rows. Network errors become statuses; a transfer that
// fails keeps the stored copy.
func fetchFeed(ctx context.Context, client *http.Client, rawURL, path string, now time.Time) fetchResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fetchResult{status: feedConnection}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/csv, text/plain;q=0.9, */*;q=0.5")
	stored, storedErr := readFeedHeader(path)
	if storedErr == nil {
		if stored.ETag != "" {
			req.Header.Set("If-None-Match", stored.ETag)
		}
		if stored.LastModified != "" {
			req.Header.Set("If-Modified-Since", stored.LastModified)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fetchResult{status: classifyError(err)}
	}
	defer resp.Body.Close()
	res := fetchResult{httpStatus: resp.StatusCode}
	switch {
	case resp.StatusCode == http.StatusNotModified && storedErr == nil:
		res.status, res.rows, res.notModified = feedOK, stored.Rows, true
		return res
	case resp.StatusCode != http.StatusOK:
		res.status = feedHTTPError
		return res
	}

	body := &countingReader{r: io.LimitReader(resp.Body, maxFeedBytes+1)}
	buffered := bufio.NewReaderSize(body, 64*1024)
	if looksLikeHTML(buffered) {
		res.status = feedHTML
		os.Remove(path)
		return res
	}
	header, err := writeFeed(buffered, path, feedHeader{
		URL:          rawURL,
		Fetched:      now.UTC().Format(time.RFC3339),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	})
	res.rejected = header.Rejected
	var readErr *bodyError
	switch {
	case errors.As(err, &readErr):
		// The transfer broke: the stored copy is untouched and still the last good one.
		res.status = classifyError(err)
		return res
	case err != nil:
		res.err = err
		return res
	case body.n > maxFeedBytes:
		res.status = feedTooLarge
	case header.Rows > 0:
		res.status, res.rows = feedOK, header.Rows
		return res
	case len(header.Rejected) == 0:
		res.status = feedEmpty
	default:
		res.status = feedInvalid
	}
	os.Remove(path) // the server answered with something that is not a feed
	return res
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// looksLikeHTML checks the first non-blank byte: servers often label CSV as
// text/html, so the Content-Type header is not trusted.
func looksLikeHTML(r *bufio.Reader) bool {
	head, _ := r.Peek(512)
	text := strings.TrimLeft(strings.TrimPrefix(string(head), "\ufeff"), " \t\r\n")
	return strings.HasPrefix(text, "<")
}

func classifyError(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var alertErr tls.AlertError
	var recordErr tls.RecordHeaderError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	var netErr net.Error
	switch {
	case errors.Is(err, errBlockedAddress):
		return feedBlocked
	case errors.As(err, &dnsErr):
		return feedDNSError
	case errors.As(err, &certErr), errors.As(err, &alertErr), errors.As(err, &recordErr),
		errors.As(err, &unknownAuth), errors.As(err, &hostErr), errors.As(err, &invalidCert):
		return feedTLSError
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return feedTimedOut
	}
	return feedConnection
}

// urlHost is the key for the per-host limits.
func urlHost(rawURL string) string {
	if parsed, err := url.Parse(rawURL); err == nil {
		return parsed.Hostname()
	}
	return rawURL
}

// hostGate limits one host: at most perHostLimit fetches at a time, and none
// once hostFailLimit fetches in a row could not reach it.
type hostGate struct {
	slots chan struct{}
	fails int
}

// unreachable reports whether a status means the host itself did not answer.
func unreachable(status string) bool {
	switch status {
	case feedTimedOut, feedDNSError, feedConnection, feedBlocked:
		return true
	}
	return false
}

// interleaveByHost orders URLs so consecutive ones are on different hosts,
// which keeps the workers busy while each host gets at most perHostLimit.
func interleaveByHost(urls []string) []string {
	byHost := map[string][]string{}
	var hosts []string
	for _, u := range urls {
		host := urlHost(u)
		if _, ok := byHost[host]; !ok {
			hosts = append(hosts, host)
		}
		byHost[host] = append(byHost[host], u)
	}
	sort.Strings(hosts)
	out := make([]string, 0, len(urls))
	for len(out) < len(urls) {
		for _, h := range hosts {
			if list := byHost[h]; len(list) > 0 {
				out = append(out, list[0])
				byHost[h] = list[1:]
			}
		}
	}
	return out
}

// CheckFeeds fetches every discovered URL that is due, stores the working
// feeds, and writes the URL lists.
func (p *Pipeline) CheckFeeds(ctx context.Context) (CheckOutput, error) {
	started := time.Now()
	now := p.clock().UTC()
	out := CheckOutput{Statuses: map[string]int{}, Rejected: map[string]int{}}
	feedsDir := filepath.Join(p.dir(), FeedsDir)
	if err := os.MkdirAll(feedsDir, 0o750); err != nil {
		return out, err
	}
	statePath := filepath.Join(p.dir(), StateFile)
	state, err := loadState(statePath)
	if err != nil {
		return out, err
	}
	discovered, err := readURLs(filepath.Join(p.dir(), URLsFile))
	if err != nil {
		return out, fmt.Errorf("read discovered URLs (run discover first): %w", err)
	}
	for u, d := range discovered {
		s := state[u]
		if s == nil {
			s = &feedState{}
			state[u] = s
		}
		s.Sources, s.Blocks, s.LastSeen = d.Sources, d.Blocks, now.Format(time.RFC3339)
	}
	for u, s := range state {
		if now.Sub(parseTime(s.LastSeen)) > keepUnseenFor {
			delete(state, u)
			os.Remove(feedPath(feedsDir, u))
			out.Dropped++
		}
	}

	var due []string
	for u, s := range state {
		if s.due(now) {
			due = append(due, u)
		} else {
			out.Skipped++
		}
	}
	due = interleaveByHost(due)
	p.log("check: %d feeds, %d due", len(state), len(due))

	client := newHTTPClient(feedRequestTimeout, p.allowPrivate)
	// A local file error stops the run: it would fail every other feed too.
	parent := ctx
	ctx, stop := context.WithCancel(parent)
	defer stop()
	var (
		mu       sync.Mutex // state, out, gates, done, localErr
		saveMu   sync.Mutex // one state save at a time, in order
		done     int
		localErr error
		gates    = map[string]*hostGate{}
		jobs     = make(chan string)
		wg       sync.WaitGroup
	)
	gateFor := func(rawURL string) *hostGate {
		host := urlHost(rawURL)
		mu.Lock()
		defer mu.Unlock()
		if gates[host] == nil {
			gates[host] = &hostGate{slots: make(chan struct{}, perHostLimit)}
		}
		return gates[host]
	}
	save := func() error {
		saveMu.Lock()
		defer saveMu.Unlock()
		mu.Lock()
		data, err := json.Marshal(state)
		mu.Unlock()
		if err != nil {
			return err
		}
		return writeFileAtomic(statePath, data)
	}
	// check fetches one feed and records the result. A feed that was not
	// fetched, or whose fetch was cut off, keeps its state for the next run.
	check := func(u string) {
		gate := gateFor(u)
		select {
		case gate.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		mu.Lock()
		skip := gate.fails >= hostFailLimit
		stale := false
		if skip {
			// Recorded like a failure, so backoff and cleanup apply to it too.
			s := state[u]
			s.Status, s.HTTPStatus = feedHostDown, 0
			if s.FailingSince == "" {
				s.FailingSince = time.Now().UTC().Format(time.RFC3339)
			}
			stale = !s.working(now)
			out.Skipped++
		}
		mu.Unlock()
		if skip {
			<-gate.slots
			if stale {
				os.Remove(feedPath(feedsDir, u))
			}
			return
		}
		path := feedPath(feedsDir, u)
		res := fetchFeed(ctx, client, u, path, now)
		<-gate.slots

		mu.Lock()
		if res.err != nil {
			if localErr == nil {
				localErr = res.err
				stop()
			}
			mu.Unlock()
			return
		}
		if ctx.Err() != nil {
			mu.Unlock()
			return
		}
		if unreachable(res.status) {
			gate.fails++
		} else {
			gate.fails = 0
		}
		s := state[u]
		s.Checked, s.Status, s.HTTPStatus = time.Now().UTC().Format(time.RFC3339), res.status, res.httpStatus
		if res.status == feedOK {
			s.Rows, s.LastOK, s.FailingSince = res.rows, s.Checked, ""
		} else if s.FailingSince == "" {
			s.FailingSince = s.Checked
		}
		stale = res.status != feedOK && !s.working(now)
		out.Requests++
		if res.notModified {
			out.NotModified++
		}
		for reason, n := range res.rejected {
			out.Rejected[reason] += n
		}
		done++
		n := done
		mu.Unlock()

		if stale {
			os.Remove(path) // a copy is kept only while the feed counts as working
		}
		if n%saveEvery == 0 {
			if err := save(); err != nil {
				p.log("check: save state: %v", err)
			}
			p.log("check: %d/%d checked", n, len(due))
		}
	}
	for range feedWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				check(u)
			}
		}()
	}
	for _, u := range due {
		if ctx.Err() != nil {
			break
		}
		jobs <- u
	}
	close(jobs)
	wg.Wait()
	if localErr != nil {
		save()
		return out, fmt.Errorf("store feed: %w", localErr)
	}
	if err := parent.Err(); err != nil {
		save()
		return out, err
	}
	if err := save(); err != nil {
		return out, err
	}

	out.Feeds = len(state)
	for _, s := range state {
		out.Statuses[s.Status]++
	}
	if len(out.Rejected) == 0 {
		out.Rejected = nil
	}
	out.Working, out.Rows, err = writeLists(filepath.Join(p.dir(), OutDir), state, now)
	if err != nil {
		return out, err
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("check: %d requests (%d not modified), %d skipped; %d working feeds with %d rows; statuses %v",
		out.Requests, out.NotModified, out.Skipped, out.Working, out.Rows, out.Statuses)
	return out, nil
}

// working reports whether a feed belongs in the working list and the export.
func (s *feedState) working(now time.Time) bool {
	return s.LastOK != "" && now.Sub(parseTime(s.LastOK)) <= workingWindow
}

// writeLists writes the URL lists: the working URLs as plain text, and
// every URL with its details as CSV.
func writeLists(dir string, state map[string]*feedState, now time.Time) (working, rows int, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, 0, err
	}
	urls := make([]string, 0, len(state))
	for u := range state {
		urls = append(urls, u)
	}
	sort.Strings(urls)

	var txt strings.Builder
	fmt.Fprintf(&txt, "# Geofeed (RFC 8805) URLs that worked in the 7 days before %s, from openipdata.org.\n", now.Format("2006-01-02"))
	txt.WriteString("# Sources: RIR whois and IRR dumps, a hand-kept list, and The CAIDA UCSD Geofeed WHOIS dataset\n")
	txt.WriteString("# (https://catalog.caida.org/dataset/geofeed_whois).\n")
	var csvText strings.Builder
	w := csv.NewWriter(&csvText)
	w.Write([]string{"url", "sources", "blocks", "status", "http_status", "rows", "last_ok"})
	for _, u := range urls {
		s := state[u]
		if s.working(now) {
			working++
			rows += s.Rows
			txt.WriteString(u + "\n")
		}
		httpStatus, lastOK := "", ""
		if s.HTTPStatus != 0 {
			httpStatus = strconv.Itoa(s.HTTPStatus)
		}
		if s.LastOK != "" {
			lastOK = parseTime(s.LastOK).Format("2006-01-02")
		}
		w.Write([]string{u, s.Sources, strconv.Itoa(s.Blocks), s.Status, httpStatus, strconv.Itoa(s.Rows), lastOK})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return 0, 0, err
	}
	for name, text := range map[string]string{ListTXT: txt.String(), ListCSV: csvText.String()} {
		if err := writeFileAtomic(filepath.Join(dir, name), []byte(text)); err != nil {
			return 0, 0, err
		}
	}
	return working, rows, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadState(path string) (map[string]*feedState, error) {
	state := map[string]*feedState{}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return state, nil
}

// discoveredURL is one row of URLsFile.
type discoveredURL struct {
	Sources string
	Blocks  int
}

func readURLs(path string) (map[string]discoveredURL, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		return nil, err
	}
	out := map[string]discoveredURL{}
	for i, rec := range records {
		if i == 0 || len(rec) != 3 {
			continue
		}
		blocks, _ := strconv.Atoi(rec[2])
		out[rec[0]] = discoveredURL{Sources: rec[1], Blocks: blocks}
	}
	return out, nil
}
