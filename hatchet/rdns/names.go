package rdns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/download"
	"openipdata.org/hatchet/ipdata"
)

// PTR answers and how long they are trusted. An error is never trusted, but the IP is
// left alone for a while after it (see errorBackoff).
const (
	statusOK       = "ok"
	statusNotFound = "not_found"
	statusError    = "error"

	freshOK       = "30 days"
	freshNotFound = "7 days"

	// Why a lookup failed (ptrAnswer.ErrKind); only counted.
	errKindTimeout  = "timeout"
	errKindServfail = "servfail"
	errKindOther    = "other"
	// Names that no trace younger than 180 days can use any more are removed.
	pruneAfter = "200 days"

	// resolveWindow is how far back resolve-names looks for traces to name. Each
	// run names the candidates of the newest trace per (prefix, target kind) of that
	// window and skips the fresh answers, so a run that failed is caught up by the
	// next one for a week. The window is not the 180 days that locate reads: a /24 is
	// traced about once in 60 days, so 7 days is a seventh of the table and the work
	// stays small, and an outage longer than a week is a failure someone must fix.
	resolveWindow = "7 days"

	// refreshMax is how many stale answers of located /24s one run looks up again
	// (see refreshQuery); a name older than freshOK is stale.
	refreshMax = 500
	// refreshRetry is how long the refresh leaves an IP alone after a lookup of any outcome.
	refreshRetry = "7 days"

	resolveWorkers = 32
	resolveChunk   = 500 // answers stored per statement
	systemTimeout  = 5 * time.Second
	dohTimeout     = 8 * time.Second

	dohURL = "https://cloudflare-dns.com/dns-query"
)

// ptrAnswer is the result of one PTR lookup.
type ptrAnswer struct {
	PTR     string // lower case, no trailing dot; empty unless Status is ok
	Status  string
	ErrKind string // only for Status error: errKindTimeout, errKindServfail or errKindOther
}

// errorBackoff is how long an IP whose last lookup failed is left alone, by the number
// of failures in a row: 1 -> 1 day, 2 -> 2 days, 3 or more -> 7 days. The SQL gets the
// three values from here (see freshNamesSQL).
func errorBackoff(errorCount int) time.Duration {
	switch {
	case errorCount >= 3:
		return 7 * 24 * time.Hour
	case errorCount == 2:
		return 2 * 24 * time.Hour
	case errorCount == 1:
		return 24 * time.Hour
	}
	return 0
}

// systemErrKind sorts an error of the system resolver, the final answer when no DoH
// fallback is tried: a timeout, "server misbehaving" (SERVFAIL and its kin) or other.
func systemErrKind(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsTimeout {
			return errKindTimeout
		}
		if strings.Contains(dnsErr.Err, "server misbehaving") {
			return errKindServfail
		}
	}
	return errKindOther
}

// httpErrKind sorts an error of the DoH request: a timeout or anything else.
func httpErrKind(err error) string {
	var nerr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &nerr) && nerr.Timeout()) {
		return errKindTimeout
	}
	return errKindOther
}

// ptrResolver looks up PTR names: the system resolver first, and Cloudflare's
// DNS-over-HTTPS JSON API when the system resolver fails (anything but "no such
// name"). Errors are never passed on: they name the IP.
type ptrResolver struct {
	system func(ctx context.Context, ip string) ([]string, error)
	doh    func(ctx context.Context, ip netip.Addr) ptrAnswer
}

func newPTRResolver() *ptrResolver {
	d := &dohClient{base: dohURL, http: newDoHHTTPClient()}
	return &ptrResolver{system: net.DefaultResolver.LookupAddr, doh: d.lookup}
}

// lookup answers for ip; viaDoH is true when the DoH fallback was used. Error kinds
// describe the final attempt: DoH's numeric RCODE distinguishes SERVFAIL from other
// DNS failures that the system resolver may all describe as "server misbehaving".
func (r *ptrResolver) lookup(ctx context.Context, ip netip.Addr) (ans ptrAnswer, viaDoH bool) {
	sctx, cancel := context.WithTimeout(ctx, systemTimeout)
	names, err := r.system(sctx, ip.String())
	cancel()
	var dnsErr *net.DNSError
	switch {
	case err == nil:
		for _, n := range names {
			if n = cleanName(n); n != "" {
				return ptrAnswer{PTR: n, Status: statusOK}, false
			}
		}
		return ptrAnswer{Status: statusNotFound}, false
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return ptrAnswer{Status: statusNotFound}, false
	}
	if ctx.Err() != nil {
		return ptrAnswer{Status: statusError, ErrKind: systemErrKind(err)}, false
	}
	return r.doh(ctx, ip), true
}

// cleanName lower-cases a PTR name and drops the trailing dot; a name that
// cannot be a host name gives "".
func cleanName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimSuffix(name, ".")
	if len(name) > 253 || strings.ContainsAny(name, " \t\r\n,") {
		return ""
	}
	return name
}

// reverseName is the in-addr.arpa name of an IPv4 address.
func reverseName(ip netip.Addr) string {
	b := ip.As4()
	return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", b[3], b[2], b[1], b[0])
}

// dohClient asks Cloudflare's DNS-over-HTTPS JSON API.
type dohClient struct {
	base string
	http *http.Client
}

// newDoHHTTPClient connects over IPv4 only: the VM's IPv6 egress stalls.
func newDoHHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", addr)
	}
	transport.Proxy = nil
	return &http.Client{Transport: transport, Timeout: dohTimeout}
}

// lookup returns the PTR answer for ip; any failure is statusError. The error
// is dropped on purpose: net/http's errors quote the URL, which holds the IP.
func (d *dohClient) lookup(ctx context.Context, ip netip.Addr) ptrAnswer {
	ctx, cancel := context.WithTimeout(ctx, dohTimeout)
	defer cancel()
	q := url.Values{"name": {reverseName(ip)}, "type": {"PTR"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"?"+q.Encode(), nil)
	if err != nil {
		return ptrAnswer{Status: statusError, ErrKind: errKindOther}
	}
	req.Header.Set("Accept", "application/dns-json")
	req.Header.Set("User-Agent", download.UserAgent)
	resp, err := d.http.Do(req)
	if err != nil {
		return ptrAnswer{Status: statusError, ErrKind: httpErrKind(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ptrAnswer{Status: statusError, ErrKind: errKindOther}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return ptrAnswer{Status: statusError, ErrKind: httpErrKind(err)}
	}
	return parseDoH(body)
}

// dohResponse is the part of the JSON answer that is used.
type dohResponse struct {
	Status *int `json:"Status"`
	Answer []struct {
		Type int    `json:"type"`
		Data string `json:"data"`
	} `json:"Answer"`
}

const dnsTypePTR = 12

// parseDoH turns a DoH JSON answer into a PTR answer: NXDOMAIN (3), or a
// NOERROR with no PTR record, is not_found; SERVFAIL (2) and any other status are errors.
func parseDoH(body []byte) ptrAnswer {
	var r dohResponse
	if json.Unmarshal(body, &r) != nil || r.Status == nil {
		return ptrAnswer{Status: statusError, ErrKind: errKindOther}
	}
	switch *r.Status {
	case 0:
		for _, a := range r.Answer {
			if a.Type == dnsTypePTR {
				if n := cleanName(a.Data); n != "" {
					return ptrAnswer{PTR: n, Status: statusOK}
				}
			}
		}
		return ptrAnswer{Status: statusNotFound}
	case 3:
		return ptrAnswer{Status: statusNotFound}
	case 2:
		return ptrAnswer{Status: statusError, ErrKind: errKindServfail}
	}
	return ptrAnswer{Status: statusError, ErrKind: errKindOther}
}

// resolveAll looks up every IP with at most workers lookups at a time. The
// answers are in the order of ips; doh counts the lookups that needed the fallback.
func resolveAll(ctx context.Context, r *ptrResolver, ips []netip.Addr, workers int) (answers []ptrAnswer, doh int) {
	answers = make([]ptrAnswer, len(ips))
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < min(workers, len(ips)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				ans, viaDoH := r.lookup(ctx, ips[i])
				answers[i] = ans
				if viaDoH {
					mu.Lock()
					doh++
					mu.Unlock()
				}
			}
		}()
	}
	for i := range ips {
		next <- i
	}
	close(next)
	wg.Wait()
	return answers, doh
}

// NamesOutput counts the lookups of a run.
type NamesOutput struct {
	Traces         int `json:"traces"`     // newest traces of the last 7 days
	IPs            int `json:"ips"`        // distinct public targets and answering hops in them
	Fresh          int `json:"fresh"`      // already answered recently
	BackedOff      int `json:"backed_off"` // skipped after a recent failed lookup
	Retried        int `json:"retried"`    // due failures outside the normal trace window, still referenced
	Resolved       int `json:"resolved"`   // looked up now (the refreshed ones included)
	OK             int `json:"ok"`         // got a name
	NotFound       int `json:"not_found"`  // no PTR record
	Errors         int `json:"errors"`     // failed (the three kinds below add up to it); tried again after errorBackoff
	ErrorsTimeout  int `json:"errors_timeout"`
	ErrorsServfail int `json:"errors_servfail"`
	ErrorsOther    int `json:"errors_other"`
	DoH            int `json:"doh"`    // of the lookups, answered through the DoH fallback
	Pruned         int `json:"pruned"` // old names removed

	Refreshed int `json:"refreshed"` // answers of located /24s' routers older than 30 days, looked up again (counted in resolved too)

	Backfilled     int `json:"backfilled,omitempty"`      // traces that got their candidates (see backfillCandidates)
	BackfilledHops int `json:"backfilled_hops,omitempty"` // traces that got their hop_ips (see backfillHopIPs)
}

// ResolveNames looks up the PTR name of the target and of every public answering hop of
// the newest trace per (prefix, target kind) of the last 7 days,
// unless ip_ptr_names has a fresh answer (ok 30 days, not found 7 days) or its last lookup
// failed less than errorBackoff ago, and stores the answers as they come, 500 at a time. Then it looks up
// again up to 500 answers older than 30 days that belong to located /24s (see refreshQuery),
// with the same resolver and the same rule: a failed lookup never replaces an answer.
func (p *Pipeline) ResolveNames(ctx context.Context) (NamesOutput, error) {
	var out NamesOutput
	conn, err := connect(ctx, p.PGSQL)
	if err != nil {
		return out, err
	}
	defer conn.Close(ctx)

	// Traces stored before network_traces.candidates existed get theirs first.
	if out.Backfilled, err = backfillCandidates(ctx, conn); err != nil {
		return out, err
	}
	// Traces stored before network_traces.hop_ips existed get theirs too (once).
	if out.BackfilledHops, err = backfillHopIPs(ctx, conn); err != nil {
		return out, err
	}
	traces, err := readTracesWithHops(ctx, conn, resolveWindow)
	if err != nil {
		return out, err
	}
	out.Traces = len(traces)
	ips := nameIPs(traces)
	out.IPs = len(ips)
	var todo []netip.Addr
	for start := 0; start < len(ips); start += 5000 {
		chunk := ips[start:min(start+5000, len(ips))]
		fresh, err := freshNames(ctx, conn, chunk)
		if err != nil {
			return out, err
		}
		for _, ip := range chunk {
			if backedOff, skip := fresh[ip]; skip {
				if backedOff {
					out.BackedOff++
				} else {
					out.Fresh++
				}
			} else {
				todo = append(todo, ip)
			}
		}
	}

	resolver := p.resolver
	if resolver == nil {
		resolver = newPTRResolver()
	}
	if err := resolveStore(ctx, conn, resolver, todo, &out); err != nil {
		return out, err
	}

	// The names behind the located /24s get old too: look up the stalest again, unless
	// this run just did.
	stale, err := staleLocatedIPs(ctx, conn, refreshMax)
	if err != nil {
		return out, err
	}
	asked := make(map[netip.Addr]bool, len(todo))
	for _, ip := range todo {
		asked[ip] = true
	}
	var refresh []netip.Addr
	for _, ip := range stale {
		if !asked[ip] {
			refresh = append(refresh, ip)
		}
	}
	out.Refreshed = len(refresh)
	if err := resolveStore(ctx, conn, resolver, refresh, &out); err != nil {
		return out, err
	}

	// A failure's 7-day delay can outlast resolveWindow. Retry due failures by IP
	// while a candidate is still in the 180-day locate window or its /24 remains
	// carried in the layer. This query runs only when a due error exists.
	retryExcluded := append(append([]netip.Addr(nil), todo...), refresh...)
	due, err := retryFailedIPs(ctx, conn, refreshMax, retryExcluded)
	if err != nil {
		return out, err
	}
	out.Retried = len(due)
	if err := resolveStore(ctx, conn, resolver, due, &out); err != nil {
		return out, err
	}

	tag, err := conn.Exec(ctx, `DELETE FROM public.ip_ptr_names WHERE resolved_at < now() - $1::text::interval`, pruneAfter)
	if err != nil {
		return out, dbError("prune names", err)
	}
	out.Pruned = int(tag.RowsAffected())
	p.log("resolve-names: %d traces, %d IPs (%d fresh, %d backed off); looked up %d (%d refreshed, %d retried): %d names, %d not found, %d errors (%d timeout, %d servfail, %d other), %d through DoH; %d old removed",
		out.Traces, out.IPs, out.Fresh, out.BackedOff, out.Resolved, out.Refreshed, out.Retried, out.OK, out.NotFound, out.Errors, out.ErrorsTimeout, out.ErrorsServfail, out.ErrorsOther, out.DoH, out.Pruned)
	return out, nil
}

// resolveStore looks up ips, resolveChunk at a time, stores the answers as they come
// (see storeNamesSQL: an error never replaces a name) and adds to the counts of out.
func resolveStore(ctx context.Context, conn *pgx.Conn, resolver *ptrResolver, ips []netip.Addr, out *NamesOutput) error {
	for start := 0; start < len(ips); start += resolveChunk {
		chunk := ips[start:min(start+resolveChunk, len(ips))]
		answers, doh := resolveAll(ctx, resolver, chunk, resolveWorkers)
		if err := ctx.Err(); err != nil {
			return err // answers cut short by a stop are not real errors
		}
		out.DoH += doh
		for _, a := range answers {
			switch a.Status {
			case statusOK:
				out.OK++
			case statusNotFound:
				out.NotFound++
			default:
				out.Errors++
				switch a.ErrKind {
				case errKindTimeout:
					out.ErrorsTimeout++
				case errKindServfail:
					out.ErrorsServfail++
				default:
					out.ErrorsOther++
				}
			}
		}
		out.Resolved += len(chunk)
		if err := storeNames(ctx, conn, chunk, answers); err != nil {
			return err
		}
	}
	return nil
}

// refreshQuery lists the IPs whose names matter for the located /24s and whose answer is
// older than 30 days, the oldest answer first: the routers of the traced rows of
// rdns_geo_layer, and the candidates of the newest traces (within the window locate reads)
// of those /24s. Failed attempts wait 1, 2, then 7 days; other attempts wait 7 days.
// An answer that keeps failing never gets a younger resolved_at, so tried_at stops it
// holding a slot in every run. $1 is the cap; $2..$4 are error backoff seconds. Read only.
const refreshQuery = `SELECT host(c.ip)
	FROM (
		SELECT g.router_ip AS ip FROM public.rdns_geo_layer g WHERE g.scope = 'traced' AND g.router_ip IS NOT NULL AND g.method IS DISTINCT FROM 'candidate_rule'
		UNION
		SELECT unnest(t.candidates) FROM (
			SELECT DISTINCT ON (prefix, target_kind) prefix, candidates FROM public.network_traces
			WHERE traced_at > now() - interval '` + locateWindow + `'
			ORDER BY prefix, target_kind, traced_at DESC) t
		WHERE t.prefix IN (SELECT prefix FROM public.rdns_geo_layer WHERE scope = 'traced' AND method IS DISTINCT FROM 'candidate_rule')
	) c
	JOIN public.ip_ptr_names n ON n.ip = c.ip
	WHERE n.resolved_at < now() - interval '` + freshOK + `'
	  AND (n.tried_at IS NULL OR n.tried_at <= now() - CASE
	    WHEN COALESCE(n.error_count, 0) > 0 THEN make_interval(secs => CASE
	      WHEN n.error_count >= 3 THEN $4::int WHEN n.error_count = 2 THEN $3::int ELSE $2::int END)
	    ELSE interval '` + refreshRetry + `' END)
	ORDER BY n.resolved_at, c.ip
	LIMIT $1`

// staleLocatedIPs reads refreshQuery: at most limit public IPs.
func staleLocatedIPs(ctx context.Context, conn *pgx.Conn, limit int) ([]netip.Addr, error) {
	rows, err := conn.Query(ctx, refreshQuery, limit, int(errorBackoff(1).Seconds()), int(errorBackoff(2).Seconds()), int(errorBackoff(3).Seconds()))
	if err != nil {
		return nil, dbError("read stale names", err)
	}
	defer rows.Close()
	var ips []netip.Addr
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, dbError("read stale names", err)
		}
		if ip, err := netip.ParseAddr(text); err == nil && ipdata.PublicV4(ip.Unmap()) {
			ips = append(ips, ip.Unmap())
		}
	}
	if err := rows.Err(); err != nil {
		return nil, dbError("read stale names", err)
	}
	return ips, nil
}

// hasDueNameErrors avoids reading the 180-day candidate set when no PTR failure
// is ready to retry. There is one resolve-names worker, so the following read
// sees the same due set unless a task is interrupted.
const hasDueNameErrorsSQL = `SELECT EXISTS (
	SELECT 1 FROM public.ip_ptr_names n
	WHERE (n.error_count > 0 OR (n.status = 'error' AND n.error_count IS NULL))
	  AND COALESCE(n.tried_at, n.resolved_at) <= now() - make_interval(secs => $1::int)
	  AND COALESCE(n.tried_at, n.resolved_at) <= now() - make_interval(secs => CASE
	    WHEN n.error_count >= 3 THEN $3::int WHEN n.error_count = 2 THEN $2::int ELSE $1::int END)
)`

// retryFailedNamesQuery finds due failures still referenced by recent trace
// candidates (never hops) or by a carried traced router row. Filtering due
// names first keeps the candidate intersection small; $1 is the cap, $2..$4
// are backoff seconds, and $5 excludes names already looked up this run.
const retryFailedNamesQuery = `WITH due AS MATERIALIZED (
SELECT n.ip, COALESCE(n.tried_at, n.resolved_at) AS last_tried_at FROM public.ip_ptr_names n
	WHERE (n.error_count > 0 OR (n.status = 'error' AND n.error_count IS NULL))
	  AND COALESCE(n.tried_at, n.resolved_at) <= now() - make_interval(secs => $2::int)
	  AND COALESCE(n.tried_at, n.resolved_at) <= now() - make_interval(secs => CASE
	    WHEN n.error_count >= 3 THEN $4::int WHEN n.error_count = 2 THEN $3::int ELSE $2::int END)
), candidates AS (
	SELECT unnest(t.candidates) AS ip FROM (
		SELECT DISTINCT ON (prefix, target_kind) prefix, target_kind, candidates
		FROM public.network_traces
		WHERE traced_at > now() - interval '` + locateWindow + `'
		ORDER BY prefix, target_kind, traced_at DESC
	) t
	UNION
	SELECT router_ip FROM public.rdns_geo_layer
	WHERE scope = 'traced' AND router_ip IS NOT NULL AND method IS DISTINCT FROM 'candidate_rule'
)
SELECT host(d.ip) FROM due d
JOIN candidates c ON c.ip = d.ip
WHERE NOT (d.ip = ANY($5::text[]::inet[]))
ORDER BY d.last_tried_at, d.ip
LIMIT $1`

// retryFailedIPs returns at most limit due errors that still matter to a trace
// or carried traced row. IPs already looked up in this run are excluded.
func retryFailedIPs(ctx context.Context, conn *pgx.Conn, limit int, asked []netip.Addr) ([]netip.Addr, error) {
	var due bool
	secs := func(n int) int { return int(errorBackoff(n).Seconds()) }
	if err := conn.QueryRow(ctx, hasDueNameErrorsSQL, secs(1), secs(2), secs(3)).Scan(&due); err != nil {
		return nil, dbError("check due name retries", err)
	}
	if !due || limit <= 0 {
		return nil, nil
	}
	skip := make([]string, len(asked))
	for i, ip := range asked {
		skip[i] = ip.String()
	}
	rows, err := conn.Query(ctx, retryFailedNamesQuery, limit, secs(1), secs(2), secs(3), skip)
	if err != nil {
		return nil, dbError("read due name retries", err)
	}
	defer rows.Close()
	var ips []netip.Addr
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, dbError("read due name retries", err)
		}
		if ip, err := netip.ParseAddr(text); err == nil && ipdata.PublicV4(ip.Unmap()) {
			ips = append(ips, ip.Unmap())
		}
	}
	if err := rows.Err(); err != nil {
		return nil, dbError("read due name retries", err)
	}
	return ips, nil
}

// freshNamesSQL selects the IPs of $1 that need no lookup now: an ok or not_found answer
// that is still trusted (resolved_at), or a failed lookup less than its backoff ago. A legacy
// status=error row with NULL error_count is treated as its first failure; tried_at falls back
// to resolved_at. $4, $5 and $6 are the
// backoff in seconds after 1, 2 and 3 or more failures (errorBackoff).
const freshNamesSQL = `SELECT host(ip),
	  COALESCE((error_count > 0 OR (status = 'error' AND error_count IS NULL))
	    AND COALESCE(tried_at, resolved_at) > now() - make_interval(secs => CASE
	    WHEN error_count >= 3 THEN $6::int WHEN error_count = 2 THEN $5::int ELSE $4::int END), false)
	FROM public.ip_ptr_names
	WHERE ip = ANY ($1::text[]::inet[])
	  AND ((status = 'ok' AND resolved_at > now() - $2::text::interval)
	    OR (status = 'not_found' AND resolved_at > now() - $3::text::interval)
	    OR ((error_count > 0 OR (status = 'error' AND error_count IS NULL))
	        AND COALESCE(tried_at, resolved_at) > now() - make_interval(secs => CASE
	          WHEN error_count >= 3 THEN $6::int WHEN error_count = 2 THEN $5::int ELSE $4::int END)))`

// freshNames returns the IPs among ips that have an answer in ip_ptr_names that is
// still trusted (ok for 30 days, not found for 7) or whose last lookup failed within its backoff.
// Map membership means skip; the value is true only for an error backoff.
func freshNames(ctx context.Context, conn *pgx.Conn, ips []netip.Addr) (map[netip.Addr]bool, error) {
	list := make([]string, len(ips))
	for i, ip := range ips {
		list[i] = ip.String()
	}
	secs := func(n int) int { return int(errorBackoff(n).Seconds()) }
	rows, err := conn.Query(ctx, freshNamesSQL, list, freshOK, freshNotFound, secs(1), secs(2), secs(3))
	if err != nil {
		return nil, dbError("read names", err)
	}
	defer rows.Close()
	fresh := map[netip.Addr]bool{}
	for rows.Next() {
		var text string
		var backedOff bool
		if err := rows.Scan(&text, &backedOff); err != nil {
			return nil, dbError("read names", err)
		}
		if ip, err := netip.ParseAddr(text); err == nil {
			fresh[ip] = backedOff
		}
	}
	if err := rows.Err(); err != nil {
		return nil, dbError("read names", err)
	}
	return fresh, nil
}

// storeNamesSQL upserts the answers. A failed lookup never replaces an answer
// that is there: the old ok or not_found name stays usable (and is tried again
// when it gets stale); an error only replaces another error. Errors over a stored
// ok/not_found answer move tried_at and error_count only. An error for an IP with no
// row is inserted, so the IP is known to have failed.
// Every lookup sets tried_at, which the refresh of old names uses to leave out
// IPs it has just tried; freshness of ok and not_found stays based on resolved_at.
// error_count counts the failures in a row, also those that only move tried_at:
// an error adds one, an ok or not_found answer sets it to 0 (backoff: freshNamesSQL).
const storeNamesSQL = `INSERT INTO public.ip_ptr_names AS t (ip, ptr, status, resolved_at, tried_at, error_count)
	SELECT u.ip::inet, u.ptr, u.status, now(), now(), CASE WHEN u.status = 'error' THEN 1 ELSE 0 END
	FROM unnest($1::text[], $2::text[], $3::text[]) AS u(ip, ptr, status)
	ON CONFLICT (ip) DO UPDATE SET
		ptr = CASE WHEN excluded.status <> 'error' OR t.status = 'error' THEN excluded.ptr ELSE t.ptr END,
		status = CASE WHEN excluded.status <> 'error' OR t.status = 'error' THEN excluded.status ELSE t.status END,
		resolved_at = CASE WHEN excluded.status <> 'error' OR t.status = 'error' THEN excluded.resolved_at ELSE t.resolved_at END,
		tried_at = excluded.tried_at,
		error_count = CASE WHEN excluded.status = 'error' THEN COALESCE(t.error_count, CASE WHEN t.status = 'error' THEN 1 ELSE 0 END) + 1 ELSE 0 END`

// storeNames upserts one answer per IP (see storeNamesSQL).
func storeNames(ctx context.Context, conn *pgx.Conn, ips []netip.Addr, answers []ptrAnswer) error {
	list := make([]string, len(ips))
	ptrs := make([]*string, len(ips))
	statuses := make([]string, len(ips))
	for i, ip := range ips {
		list[i] = ip.String()
		if answers[i].PTR != "" {
			ptrs[i] = &answers[i].PTR
		}
		statuses[i] = answers[i].Status
	}
	if _, err := conn.Exec(ctx, storeNamesSQL, list, ptrs, statuses); err != nil {
		return dbError("store names", err)
	}
	return nil
}
