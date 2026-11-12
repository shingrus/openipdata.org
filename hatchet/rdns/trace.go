package rdns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/outputs"
)

const (
	// scamperPPS is the packet rate of the scamper process: ~3,000 targets with
	// a 15-hop path each are traced in about five minutes.
	scamperPPS = 200
	// scamperTimeout bounds one scamper process; a batch that takes longer is an error.
	scamperTimeout = 40 * time.Minute
	// keepTraces is how many traces are kept per (prefix, target kind).
	keepTraces = 2

	targetsFile = "targets.txt"
	scamperOut  = "scamper.jsonl"
)

// scamperFunc runs scamper on a file of addresses (one per line) and writes the
// JSON results to outFile. Tests replace it.
type scamperFunc func(ctx context.Context, targets, outFile string, pps int) error

// socketFailure matches scamper's stderr when it cannot open its raw socket
// ("could not open ICMP socket: Operation not permitted"): it then prints the
// failure and still exits 0, with no trace in its output.
var socketFailure = regexp.MustCompile(`(?i)could not open[^\n]*socket`)

// runScamper is the one scamper process of a batch: Paris ICMP traceroute to
// every address of the file, JSON lines in outFile. bin is the scamper next to
// the worker, built without privilege separation (the setuid package needs
// mkdir, chroot and setuid, which the unit forbids). It needs CAP_NET_RAW (the
// systemd unit grants it); without it scamper exits 0, so a socket failure on
// stderr is an error too. scamper's stderr can name targets, so its text goes
// into the error only with the IPs hidden.
func runScamper(ctx context.Context, bin, targets, outFile string, pps int) error {
	if bin == "" {
		return errors.New("scamper: binary path unknown")
	}
	ctx, cancel := context.WithTimeout(ctx, scamperTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-O", "json", "-p", strconv.Itoa(pps),
		"-c", "trace -P icmp-paris", "-o", outFile, "-f", targets)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scamper: %w: %s", err, scrubIPs(outputs.LastLines(stderr.String(), 3)))
	}
	if socketFailure.MatchString(stderr.String()) {
		return fmt.Errorf("scamper could not open its socket (CAP_NET_RAW missing?): %s", scrubIPs(outputs.LastLines(stderr.String(), 3)))
	}
	return nil
}

// target is one host to trace.
type target struct {
	Prefix netip.Prefix
	Kind   string
	Addr   netip.Addr
}

// targetsFor picks the hosts of a /24: the newest visitor IP (when there is one: a
// recheck of a located /24 may have none), .1 and one random host (not .0 or .255, and
// not one of the other two). When the visitor IP is .1 there is no separate gateway target.
func targetsFor(b batchTarget, intn func(n int) int) []target {
	base := b.Prefix.Addr().As4()
	host := func(last int) netip.Addr {
		a := base
		a[3] = byte(last)
		return netip.AddrFrom4(a)
	}
	var out []target
	if b.Visitor.IsValid() {
		out = append(out, target{b.Prefix, kindVisitor, b.Visitor})
	}
	gateway := host(1)
	if gateway != b.Visitor {
		out = append(out, target{b.Prefix, kindGateway, gateway})
	}
	// 2..254 minus the visitor host leaves at least 252 choices (an invalid visitor equals no host).
	for {
		random := host(2 + intn(253))
		if random != b.Visitor {
			return append(out, target{b.Prefix, kindRandom, random})
		}
	}
}

// scamperTrace is the part of scamper's JSON trace object that is used.
type scamperTrace struct {
	Type string `json:"type"`
	Dst  string `json:"dst"`
	Hops []struct {
		Addr     string  `json:"addr"`
		ProbeTTL int     `json:"probe_ttl"`
		RTT      float64 `json:"rtt"`
	} `json:"hops"`
}

// parseScamper reads scamper's JSON output (one object per line; the "trace"
// objects matter, others such as the cycle markers are skipped) and returns the
// path of every wanted destination. A destination that is not in want is
// ignored; the first result for a destination wins. traces counts every "trace"
// object, wanted or not: none at all means scamper did not trace. The error
// never quotes the input.
func parseScamper(r io.Reader, want map[netip.Addr]bool) (map[netip.Addr][]Hop, int, error) {
	paths := map[netip.Addr][]Hop{}
	traces := 0
	dec := json.NewDecoder(r)
	for {
		var tr scamperTrace
		err := dec.Decode(&tr)
		if err == io.EOF {
			return paths, traces, nil
		}
		if err != nil {
			return nil, 0, errors.New("scamper output is not valid JSON")
		}
		if tr.Type != "trace" {
			continue
		}
		traces++
		dst, err := netip.ParseAddr(tr.Dst)
		if err != nil || !want[dst] {
			continue
		}
		if _, done := paths[dst]; done {
			continue
		}
		byTTL := map[int]Hop{}
		maxTTL := 0
		for _, h := range tr.Hops {
			addr, err := netip.ParseAddr(h.Addr)
			if err != nil || !addr.Is4() || h.ProbeTTL < 1 || h.ProbeTTL > 255 {
				continue
			}
			if _, dup := byTTL[h.ProbeTTL]; dup {
				continue // more than one reply to the same TTL: the first stays
			}
			ip := addr.String()
			rtt := math.Round(h.RTT*1000) / 1000
			byTTL[h.ProbeTTL] = Hop{TTL: h.ProbeTTL, IP: &ip, RTTms: &rtt}
			maxTTL = max(maxTTL, h.ProbeTTL)
		}
		// The full path: a TTL with no reply is a hop with a null IP.
		path := make([]Hop, 0, maxTTL)
		for ttl := 1; ttl <= maxTTL; ttl++ {
			if h, ok := byTTL[ttl]; ok {
				path = append(path, h)
			} else {
				path = append(path, Hop{TTL: ttl})
			}
		}
		paths[dst] = path
	}
}

// traceRow is one network_traces row to store.
type traceRow struct {
	Prefix string
	Kind   string
	Target string
	Hops   []byte // JSON
	// Candidates are the IPs of the path that get named and matched (see candidates):
	// stored beside the path, so later tasks never read it.
	Candidates []netip.Addr
	// Answered: at least one hop of the path replied (any address).
	Answered bool
}

// TraceOutput counts the traces of a run.
type TraceOutput struct {
	Networks int `json:"networks"`
	Targets  int `json:"targets"`
	Answered int `json:"answered"` // traces with at least one hop that replied
	Silent   int `json:"silent"`   // stored with an empty or all-silent path
	Reached  int `json:"reached"`  // the target itself replied
	Dropped  int `json:"dropped"`  // batch lines that were not a /24 with a public visitor IP
	Pruned   int `json:"pruned"`   // older traces removed (more than two per prefix and kind)

	Stored      int `json:"stored"`       // traces stored
	SilentNets  int `json:"silent_nets"`  // /24s with no answering hop in any new trace (see schedule.go)
	VisitorsCut int `json:"visitors_cut"` // visitor IPs deleted: all but the newest of each traced /24

	// PathCompared counts the (prefix, kind) pairs that have two stored traces now, and
	// PathChanged those whose candidates (the target left out) differ between them.
	PathCompared int `json:"path_compared"`
	PathChanged  int `json:"path_changed"`

	// Removed by the cleanup: traces older than 200 days, and the rows of /24s that are not
	// located, have no next_trace_at and were last traced more than 200 days ago.
	TracesExpired   int `json:"traces_expired"`
	NetworksExpired int `json:"networks_expired"`
}

// Trace traces the targets of every /24 of the batch (the newest visitor IP if any, .1 and a
// random host) with one scamper process, stores every trace (a target that got no result is
// stored with an empty path), keeps the two newest per (prefix, kind), and in the same
// transaction updates the schedule of the /24s (traced_networks), deletes all but the
// newest visitor IP of each, and counts how many paths changed since the previous trace.
// Then it removes what is older than 200 days (see cleanupSchedule).
func (p *Pipeline) Trace(ctx context.Context) (TraceOutput, error) {
	var out TraceOutput
	if err := p.CheckPGSQL(); err != nil {
		return out, err
	}
	batch, dropped, err := readBatch(filepath.Join(p.dir(), batchFile))
	if err != nil {
		return out, err
	}
	out.Dropped = dropped
	rows, out, err := p.traceBatch(ctx, batch, out)
	if err != nil {
		return out, err
	}
	conn, err := connect(ctx, p.PGSQL)
	if err != nil {
		return out, err
	}
	defer conn.Close(ctx)
	if len(rows) > 0 {
		res, err := storeTraces(ctx, conn, rows, p.clock())
		if err != nil {
			return out, err
		}
		out.Stored, out.Pruned, out.SilentNets, out.VisitorsCut = len(rows), res.Pruned, res.Silent, res.VisitorsDeleted
		out.PathCompared, out.PathChanged = res.PathCompared, res.PathChanged
	}
	// The batch is done: a retry of this task has nothing left to trace.
	os.Remove(filepath.Join(p.dir(), batchFile))
	if out.TracesExpired, out.NetworksExpired, err = cleanupSchedule(ctx, conn); err != nil {
		return out, err
	}
	p.log("trace: %d /24s (%d silent), %d targets, %d answered, %d silent, %d reached the target, %d old traces removed; paths: %d of %d changed; %d visitor IPs deleted; expired: %d traces, %d /24s",
		out.Networks, out.SilentNets, out.Targets, out.Answered, out.Silent, out.Reached, out.Pruned, out.PathChanged, out.PathCompared, out.VisitorsCut,
		out.TracesExpired, out.NetworksExpired)
	return out, nil
}

// traceBatch picks the targets, runs scamper and turns its output into rows.
func (p *Pipeline) traceBatch(ctx context.Context, batch []batchTarget, out TraceOutput) ([]traceRow, TraceOutput, error) {
	out.Networks = len(batch)
	if len(batch) == 0 {
		return nil, out, nil
	}
	intn := p.randInt
	var targets []target
	for _, b := range batch {
		targets = append(targets, targetsFor(b, intn)...)
	}
	out.Targets = len(targets)

	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return nil, out, err
	}
	targetsPath, resultPath := filepath.Join(p.dir(), targetsFile), filepath.Join(p.dir(), scamperOut)
	var list strings.Builder
	want := map[netip.Addr]bool{}
	for _, t := range targets {
		list.WriteString(t.Addr.String() + "\n")
		want[t.Addr] = true
	}
	if err := os.WriteFile(targetsPath, []byte(list.String()), 0o640); err != nil {
		return nil, out, err
	}
	defer os.Remove(targetsPath)
	defer os.Remove(resultPath)
	os.Remove(resultPath)

	run := p.scamper
	if run == nil {
		run = func(ctx context.Context, targets, outFile string, pps int) error {
			return runScamper(ctx, p.Scamper, targets, outFile, pps)
		}
	}
	if err := run(ctx, targetsPath, resultPath, scamperPPS); err != nil {
		return nil, out, err
	}
	f, err := os.Open(resultPath)
	if err != nil {
		return nil, out, fmt.Errorf("scamper wrote no output: %w", err)
	}
	defer f.Close()
	paths, traces, err := parseScamper(f, want)
	if err != nil {
		return nil, out, err
	}
	// A non-empty batch with no trace object at all: scamper did not trace (no
	// socket, no permission). Storing empty paths would mark the /24s as traced.
	if traces == 0 {
		return nil, out, errors.New("scamper output has no trace results")
	}

	rows := make([]traceRow, 0, len(targets))
	for _, t := range targets {
		hops := paths[t.Addr]
		if hops == nil {
			hops = []Hop{}
		}
		answered, reached := false, false
		for _, h := range hops {
			if a, ok := h.addr(); ok {
				answered = true
				reached = reached || a == t.Addr
			}
		}
		if answered {
			out.Answered++
		} else {
			out.Silent++
		}
		if reached {
			out.Reached++
		}
		data, err := json.Marshal(hops)
		if err != nil {
			return nil, out, err
		}
		rows = append(rows, traceRow{Prefix: t.Prefix.String(), Kind: t.Kind, Target: t.Addr.String(), Hops: data,
			Candidates: candidates(t.Addr, hops), Answered: answered})
	}
	return rows, out, nil
}

// joinAddrs is the comma-separated text of addrs, which string_to_array turns back
// into the array (an array of arrays cannot go through unnest).
func joinAddrs(addrs []netip.Addr) string {
	texts := make([]string, len(addrs))
	for i, a := range addrs {
		texts[i] = a.String()
	}
	return strings.Join(texts, ",")
}

// storeResult is what storeTraces did besides inserting the rows.
type storeResult struct {
	Pruned          int // older traces removed (more than two per prefix and kind)
	Silent          int // /24s that are silent after this batch
	VisitorsDeleted int
	PathCompared    int
	PathChanged     int
}

// comparePathsSQL compares, for each of the prefixes ($1), the two newest traces of each
// target kind: their candidates with the target left out (the target differs between
// traces of the random kind and is not part of the path). Rows with no candidates yet
// (written before the column existed) are left out.
const comparePathsSQL = `WITH r AS (
		SELECT prefix, target_kind, array_remove(candidates, target) AS c,
		       row_number() OVER (PARTITION BY prefix, target_kind ORDER BY traced_at DESC) AS n
		FROM public.network_traces
		WHERE prefix = ANY ($1::text[]::cidr[]) AND candidates IS NOT NULL)
	SELECT count(*), count(*) FILTER (WHERE a.c <> b.c)
	FROM r a JOIN r b ON a.prefix = b.prefix AND a.target_kind = b.target_kind AND a.n = 1 AND b.n = 2`

// storeTraces, in one transaction, inserts the rows, removes the traces of the same
// prefixes beyond the two newest per kind, updates the schedule of the prefixes
// (traced_networks), deletes all but the newest visitor IP of each prefix, and compares
// the two newest paths. A failed statement is reported by code only: its message can
// quote an IP.
func storeTraces(ctx context.Context, conn *pgx.Conn, rows []traceRow, at time.Time) (storeResult, error) {
	var res storeResult
	tx, err := conn.Begin(ctx)
	if err != nil {
		return res, dbError("store traces", err)
	}
	defer tx.Rollback(ctx)
	var prefixes, kinds, targets, hops, cands []string
	answered := map[string]bool{}
	var touched []string
	for _, r := range rows {
		prefixes = append(prefixes, r.Prefix)
		kinds = append(kinds, r.Kind)
		targets = append(targets, r.Target)
		hops = append(hops, string(r.Hops))
		cands = append(cands, joinAddrs(r.Candidates))
		if _, seen := answered[r.Prefix]; !seen {
			touched = append(touched, r.Prefix)
		}
		answered[r.Prefix] = answered[r.Prefix] || r.Answered
	}
	sort.Strings(touched)
	if _, err := tx.Exec(ctx, `INSERT INTO public.network_traces (prefix, target_kind, target, traced_at, hops, candidates)
		SELECT u.prefix::cidr, u.kind, u.target::inet, $6::timestamptz, u.hops::jsonb, string_to_array(u.cands, ',')::inet[]
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[]) AS u(prefix, kind, target, hops, cands)`,
		prefixes, kinds, targets, hops, cands, at); err != nil {
		return res, dbError("store traces", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM public.network_traces t USING (
			SELECT prefix, target_kind, traced_at,
			       row_number() OVER (PARTITION BY prefix, target_kind ORDER BY traced_at DESC) AS n
			FROM public.network_traces
			WHERE prefix = ANY ($1::text[]::cidr[])) old
		WHERE t.prefix = old.prefix AND t.target_kind = old.target_kind AND t.traced_at = old.traced_at AND old.n > $2`,
		touched, keepTraces)
	if err != nil {
		return res, dbError("prune traces", err)
	}
	res.Pruned = int(tag.RowsAffected())
	if err := tx.QueryRow(ctx, comparePathsSQL, touched).Scan(&res.PathCompared, &res.PathChanged); err != nil {
		return res, dbError("compare paths", err)
	}
	if res.Silent, err = updateSchedule(ctx, tx, answered, at); err != nil {
		return res, err
	}
	tag, err = tx.Exec(ctx, keepNewestVisitorSQL, touched)
	if err != nil {
		return res, dbError("delete visitor IPs", err)
	}
	res.VisitorsDeleted = int(tag.RowsAffected())
	if err := tx.Commit(ctx); err != nil {
		return res, dbError("store traces", err)
	}
	return res, nil
}
