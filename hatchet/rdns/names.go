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
)

// PTR answers and how long they are trusted. An error is not trusted at all:
// the IP is tried again in the next run.
const (
	statusOK       = "ok"
	statusNotFound = "not_found"
	statusError    = "error"

	freshOK       = "30 days"
	freshNotFound = "7 days"
	// Names that no trace younger than 180 days can use any more are removed.
	pruneAfter = "200 days"

	// resolveWindow is how far back resolve-names looks for traces to name. Each
	// run names the candidates of the newest trace per (prefix, target kind) of that
	// window and skips the fresh answers, so a run that failed is caught up by the
	// next one for a week. The window is not the 180 days that locate reads: a /24 is
	// traced about once in 60 days, so 7 days is a seventh of the table and the work
	// stays small, and an outage longer than a week is a failure someone must fix.
	resolveWindow = "7 days"

	resolveWorkers = 32
	resolveChunk   = 500 // answers stored per statement
	systemTimeout  = 5 * time.Second
	dohTimeout     = 8 * time.Second

	dohURL = "https://cloudflare-dns.com/dns-query"
)

// ptrAnswer is the result of one PTR lookup.
type ptrAnswer struct {
	PTR    string // lower case, no trailing dot; empty unless Status is ok
	Status string
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

// lookup answers for ip; viaDoH is true when the DoH fallback was used.
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
		return ptrAnswer{Status: statusError}, false
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
		return ptrAnswer{Status: statusError}
	}
	req.Header.Set("Accept", "application/dns-json")
	req.Header.Set("User-Agent", download.UserAgent)
	resp, err := d.http.Do(req)
	if err != nil {
		return ptrAnswer{Status: statusError}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ptrAnswer{Status: statusError}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return ptrAnswer{Status: statusError}
	}
	return parseDoH(body)
}

// dohResponse is the part of the JSON answer that is used.
type dohResponse struct {
	Status int `json:"Status"`
	Answer []struct {
		Type int    `json:"type"`
		Data string `json:"data"`
	} `json:"Answer"`
}

const dnsTypePTR = 12

// parseDoH turns a DoH JSON answer into a PTR answer: NXDOMAIN (3), or a
// NOERROR with no PTR record, is not_found; any other status is an error.
func parseDoH(body []byte) ptrAnswer {
	var r dohResponse
	if json.Unmarshal(body, &r) != nil {
		return ptrAnswer{Status: statusError}
	}
	switch r.Status {
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
	}
	return ptrAnswer{Status: statusError}
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
	Traces   int `json:"traces"`    // newest traces of the last 7 days
	IPs      int `json:"ips"`       // distinct public targets and last hops in them
	Fresh    int `json:"fresh"`     // already answered recently
	Resolved int `json:"resolved"`  // looked up now
	OK       int `json:"ok"`        // got a name
	NotFound int `json:"not_found"` // no PTR record
	Errors   int `json:"errors"`    // failed; tried again next run
	DoH      int `json:"doh"`       // of the lookups, answered through the DoH fallback
	Pruned   int `json:"pruned"`    // old names removed
}

// ResolveNames looks up the PTR name of the target and of the last quarter of
// the hops of the newest trace per (prefix, target kind) of the last 7 days,
// unless ip_ptr_names has a fresh answer (ok 30 days, not found 7 days; an
// error is retried), and stores the answers as they come, 500 at a time.
func (p *Pipeline) ResolveNames(ctx context.Context) (NamesOutput, error) {
	var out NamesOutput
	conn, err := connect(ctx, p.PGSQL)
	if err != nil {
		return out, err
	}
	defer conn.Close(ctx)

	traces, err := readTraces(ctx, conn, resolveWindow)
	if err != nil {
		return out, err
	}
	out.Traces = len(traces)
	ips := candidateIPs(traces)
	out.IPs = len(ips)
	var todo []netip.Addr
	for start := 0; start < len(ips); start += 5000 {
		chunk := ips[start:min(start+5000, len(ips))]
		fresh, err := freshNames(ctx, conn, chunk)
		if err != nil {
			return out, err
		}
		for _, ip := range chunk {
			if fresh[ip] {
				out.Fresh++
			} else {
				todo = append(todo, ip)
			}
		}
	}

	resolver := p.resolver
	if resolver == nil {
		resolver = newPTRResolver()
	}
	for start := 0; start < len(todo); start += resolveChunk {
		chunk := todo[start:min(start+resolveChunk, len(todo))]
		answers, doh := resolveAll(ctx, resolver, chunk, resolveWorkers)
		if err := ctx.Err(); err != nil {
			return out, err // answers cut short by a stop are not real errors
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
			}
		}
		out.Resolved += len(chunk)
		if err := storeNames(ctx, conn, chunk, answers); err != nil {
			return out, err
		}
	}

	tag, err := conn.Exec(ctx, `DELETE FROM public.ip_ptr_names WHERE resolved_at < now() - $1::text::interval`, pruneAfter)
	if err != nil {
		return out, dbError("prune names", err)
	}
	out.Pruned = int(tag.RowsAffected())
	p.log("resolve-names: %d traces, %d IPs (%d fresh); looked up %d: %d names, %d not found, %d errors, %d through DoH; %d old removed",
		out.Traces, out.IPs, out.Fresh, out.Resolved, out.OK, out.NotFound, out.Errors, out.DoH, out.Pruned)
	return out, nil
}

// freshNames returns the IPs among ips that have an answer in ip_ptr_names that is
// still trusted (ok for 30 days, not found for 7; an error never is).
func freshNames(ctx context.Context, conn *pgx.Conn, ips []netip.Addr) (map[netip.Addr]bool, error) {
	list := make([]string, len(ips))
	for i, ip := range ips {
		list[i] = ip.String()
	}
	rows, err := conn.Query(ctx, `SELECT host(ip) FROM public.ip_ptr_names
		WHERE ip = ANY ($1::text[]::inet[])
		  AND ((status = 'ok' AND resolved_at > now() - $2::text::interval)
		    OR (status = 'not_found' AND resolved_at > now() - $3::text::interval))`,
		list, freshOK, freshNotFound)
	if err != nil {
		return nil, dbError("read names", err)
	}
	defer rows.Close()
	fresh := map[netip.Addr]bool{}
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, dbError("read names", err)
		}
		if ip, err := netip.ParseAddr(text); err == nil {
			fresh[ip] = true
		}
	}
	return fresh, rows.Err()
}

// storeNamesSQL upserts the answers. A failed lookup never replaces an answer
// that is there: the old ok or not_found name stays usable (and is tried again
// when it gets stale); an error only replaces another error. An error for an IP
// with no row is inserted, so the IP is known to have failed.
const storeNamesSQL = `INSERT INTO public.ip_ptr_names AS t (ip, ptr, status, resolved_at)
	SELECT u.ip::inet, u.ptr, u.status, now()
	FROM unnest($1::text[], $2::text[], $3::text[]) AS u(ip, ptr, status)
	ON CONFLICT (ip) DO UPDATE SET ptr = excluded.ptr, status = excluded.status, resolved_at = excluded.resolved_at
	WHERE excluded.status <> 'error' OR t.status = 'error'`

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
