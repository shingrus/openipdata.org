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

// targetsFor picks the hosts of a /24: the newest visitor IP, .1 and one random
// host (not .0 or .255, and not one of the other two). When the visitor IP is
// .1 there is no separate gateway target.
func targetsFor(b batchTarget, intn func(n int) int) []target {
	base := b.Prefix.Addr().As4()
	host := func(last int) netip.Addr {
		a := base
		a[3] = byte(last)
		return netip.AddrFrom4(a)
	}
	out := []target{{b.Prefix, kindVisitor, b.Visitor}}
	gateway := host(1)
	if gateway != b.Visitor {
		out = append(out, target{b.Prefix, kindGateway, gateway})
	}
	// 2..254 minus the visitor host leaves at least 252 choices.
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
}

// Trace traces the three targets of every /24 of the batch with one scamper
// process, stores every trace (a target that got no result is stored with an
// empty path, so its /24 is not picked again at once), and keeps the two newest per
// (prefix, kind).
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
	if len(rows) > 0 {
		conn, err := connect(ctx, p.PGSQL)
		if err != nil {
			return out, err
		}
		defer conn.Close(ctx)
		if out.Pruned, err = storeTraces(ctx, conn, rows, time.Now()); err != nil {
			return out, err
		}
	}
	// The batch is done: a retry of this task has nothing left to trace.
	os.Remove(filepath.Join(p.dir(), batchFile))
	p.log("trace: %d /24s, %d targets, %d answered, %d silent, %d reached the target, %d old traces removed",
		out.Networks, out.Targets, out.Answered, out.Silent, out.Reached, out.Pruned)
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
		rows = append(rows, traceRow{Prefix: t.Prefix.String(), Kind: t.Kind, Target: t.Addr.String(), Hops: data})
	}
	return rows, out, nil
}

// storeTraces inserts the rows in one transaction and removes the traces of the
// same prefixes beyond the two newest per kind. It returns the number removed.
// A failed statement is reported by code only: its message can quote an IP.
func storeTraces(ctx context.Context, conn *pgx.Conn, rows []traceRow, at time.Time) (int, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, dbError("store traces", err)
	}
	defer tx.Rollback(ctx)
	var prefixes, kinds, targets, hops []string
	seen := map[string]bool{}
	var touched []string
	for _, r := range rows {
		prefixes = append(prefixes, r.Prefix)
		kinds = append(kinds, r.Kind)
		targets = append(targets, r.Target)
		hops = append(hops, string(r.Hops))
		if !seen[r.Prefix] {
			seen[r.Prefix] = true
			touched = append(touched, r.Prefix)
		}
	}
	sort.Strings(touched)
	if _, err := tx.Exec(ctx, `INSERT INTO public.network_traces (prefix, target_kind, target, traced_at, hops)
		SELECT u.prefix::cidr, u.kind, u.target::inet, $5::timestamptz, u.hops::jsonb
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[]) AS u(prefix, kind, target, hops)`,
		prefixes, kinds, targets, hops, at); err != nil {
		return 0, dbError("store traces", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM public.network_traces t USING (
			SELECT prefix, target_kind, traced_at,
			       row_number() OVER (PARTITION BY prefix, target_kind ORDER BY traced_at DESC) AS n
			FROM public.network_traces
			WHERE prefix = ANY ($1::text[]::cidr[])) old
		WHERE t.prefix = old.prefix AND t.target_kind = old.target_kind AND t.traced_at = old.traced_at AND old.n > $2`,
		touched, keepTraces)
	if err != nil {
		return 0, dbError("prune traces", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, dbError("store traces", err)
	}
	return int(tag.RowsAffected()), nil
}
