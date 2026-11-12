package rdns

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// scamperFixture is what "scamper -O json" writes for three targets: a trace
// that reached its target, one that stopped at a gap, one where nothing answered,
// and the cycle lines around them. (Addresses are made up.)
const scamperFixture = `{"type":"cycle-start", "list_name":"default", "id":1, "hostname":"vm", "start_time":1762900000}
{"type":"trace", "version":"0.1", "userid":0, "method":"icmp-echo-paris", "src":"198.51.100.2", "dst":"5.6.7.9", "icmp_sum":16431, "stop_reason":"COMPLETED", "stop_data":0, "start":{"sec":1762900001,"usec":123456,"ftime":"2025-11-11 22:46:41"}, "hop_count":4, "attempts":2, "hoplimit":255, "firsthop":1, "wait":5, "wait_probe":0, "tos":0, "probe_size":84, "probe_count":5, "hops":[{"addr":"10.0.0.1", "probe_id":1, "probe_ttl":1, "probe_size":84, "rtt":0.412, "reply_ttl":64, "reply_tos":0, "reply_size":56, "icmp_type":11, "icmp_code":0, "icmp_q_ttl":1, "icmp_q_ipl":84, "icmp_q_tos":0},{"addr":"9.9.9.9", "probe_id":1, "probe_ttl":2, "probe_size":84, "rtt":3.2189, "reply_ttl":253, "reply_tos":0, "reply_size":56, "icmp_type":11, "icmp_code":0, "icmp_q_ttl":1, "icmp_q_ipl":84, "icmp_q_tos":0},{"addr":"5.6.0.1", "probe_id":1, "probe_ttl":4, "probe_size":84, "rtt":12.0004, "reply_ttl":61, "reply_tos":0, "reply_size":56, "icmp_type":11, "icmp_code":0, "icmp_q_ttl":1, "icmp_q_ipl":84, "icmp_q_tos":0},{"addr":"5.6.0.1", "probe_id":2, "probe_ttl":4, "probe_size":84, "rtt":12.9, "reply_ttl":61, "reply_tos":0, "reply_size":56, "icmp_type":11, "icmp_code":0, "icmp_q_ttl":1, "icmp_q_ipl":84, "icmp_q_tos":0},{"addr":"5.6.7.9", "probe_id":1, "probe_ttl":5, "probe_size":84, "rtt":13.5, "reply_ttl":58, "reply_tos":0, "reply_size":84, "icmp_type":0, "icmp_code":0}]}
{"type":"trace", "version":"0.1", "userid":0, "method":"icmp-echo-paris", "src":"198.51.100.2", "dst":"5.6.7.1", "stop_reason":"GAPLIMIT", "stop_data":0, "hop_count":2, "hops":[{"addr":"10.0.0.1", "probe_id":1, "probe_ttl":1, "rtt":0.5},{"addr":"9.9.9.9", "probe_id":1, "probe_ttl":2, "rtt":3.1}]}
{"type":"trace", "version":"0.1", "userid":0, "method":"icmp-echo-paris", "src":"198.51.100.2", "dst":"20.30.40.7", "stop_reason":"GAPLIMIT", "stop_data":0, "hop_count":0}
{"type":"trace", "version":"0.1", "userid":0, "method":"icmp-echo-paris", "src":"198.51.100.2", "dst":"99.99.99.99", "stop_reason":"COMPLETED", "hop_count":1, "hops":[{"addr":"10.0.0.1", "probe_ttl":1, "rtt":0.5}]}
{"type":"cycle-stop", "list_name":"default", "id":1, "hostname":"vm", "stop_time":1762900300}
`

func TestParseScamper(t *testing.T) {
	want := map[netip.Addr]bool{}
	for _, a := range addrs("5.6.7.9", "5.6.7.1", "20.30.40.7") {
		want[a] = true
	}
	paths, traces, err := parseScamper(strings.NewReader(scamperFixture), want)
	if err != nil {
		t.Fatal(err)
	}
	if traces != 4 {
		t.Errorf("traces = %d, want 4 (the unwanted one counts, the cycle lines do not)", traces)
	}
	if len(paths) != 3 {
		t.Fatalf("paths for %d targets (an unwanted one must be left out): %v", len(paths), paths)
	}

	// TTL 3 got no reply: a hop with a null IP; the second reply to TTL 4 is dropped.
	got, _ := json.Marshal(paths[netip.MustParseAddr("5.6.7.9")])
	wantJSON := `[{"ttl":1,"ip":"10.0.0.1","rtt_ms":0.412},{"ttl":2,"ip":"9.9.9.9","rtt_ms":3.219},{"ttl":3,"ip":null,"rtt_ms":null},` +
		`{"ttl":4,"ip":"5.6.0.1","rtt_ms":12},{"ttl":5,"ip":"5.6.7.9","rtt_ms":13.5}]`
	if string(got) != wantJSON {
		t.Errorf("path of 5.6.7.9:\n%s\nwant\n%s", got, wantJSON)
	}
	if p := paths[netip.MustParseAddr("5.6.7.1")]; len(p) != 2 || p[1].TTL != 2 {
		t.Errorf("path of 5.6.7.1 = %+v", p)
	}
	// A trace with no hops at all has an empty path, and is still there.
	if p, ok := paths[netip.MustParseAddr("20.30.40.7")]; !ok || len(p) != 0 {
		t.Errorf("path of 20.30.40.7 = %+v %v", p, ok)
	}
}

func TestParseScamperFirstResultWinsAndErrors(t *testing.T) {
	want := map[netip.Addr]bool{netip.MustParseAddr("5.6.7.9"): true}
	twice := `{"type":"trace","dst":"5.6.7.9","hops":[{"addr":"1.1.1.1","probe_ttl":1,"rtt":1}]}
{"type":"trace","dst":"5.6.7.9","hops":[{"addr":"2.2.2.2","probe_ttl":1,"rtt":1}]}`
	paths, _, err := parseScamper(strings.NewReader(twice), want)
	if err != nil || len(paths[netip.MustParseAddr("5.6.7.9")]) != 1 || *paths[netip.MustParseAddr("5.6.7.9")][0].IP != "1.1.1.1" {
		t.Errorf("paths = %v %v", paths, err)
	}
	// Output cut off by a kill is an error that does not quote it.
	_, _, err = parseScamper(strings.NewReader(`{"type":"trace","dst":"5.6.7.9","hops":[{"addr":"1.1.1.1`), want)
	if err == nil || strings.Contains(err.Error(), "5.6.7.9") || strings.Contains(err.Error(), "1.1.1.1") {
		t.Errorf("err = %v", err)
	}
	// An empty file is no results, not an error.
	if paths, traces, err := parseScamper(strings.NewReader(""), want); err != nil || len(paths) != 0 || traces != 0 {
		t.Errorf("empty: %v %d %v", paths, traces, err)
	}
	// Hops with an impossible TTL or an address that is not IPv4 are ignored.
	odd := `{"type":"trace","dst":"5.6.7.9","hops":[{"addr":"1.1.1.1","probe_ttl":0,"rtt":1},{"addr":"2001:db8::1","probe_ttl":1,"rtt":1},{"addr":"x","probe_ttl":2,"rtt":1}]}`
	if paths, _, err := parseScamper(strings.NewReader(odd), want); err != nil || len(paths[netip.MustParseAddr("5.6.7.9")]) != 0 {
		t.Errorf("odd hops: %v %v", paths, err)
	}
}

func TestTargetsFor(t *testing.T) {
	prefix := netip.MustParsePrefix("5.6.7.0/24")
	pick := func(vals ...int) func(int) int {
		i := 0
		return func(n int) int {
			v := vals[i%len(vals)]
			i++
			if v >= n {
				t.Fatalf("intn(%d) asked for %d", n, v)
			}
			return v
		}
	}
	kinds := func(ts []target) []string {
		var out []string
		for _, x := range ts {
			out = append(out, x.Kind+"="+x.Addr.String())
		}
		return out
	}
	join := func(ts []target) string { return strings.Join(kinds(ts), " ") }

	// No visitor (a recheck of a located /24): .1 and a random host only.
	got := targetsFor(batchTarget{Prefix: prefix}, pick(40))
	if join(got) != "gateway=5.6.7.1 random=5.6.7.42" {
		t.Errorf("no visitor: %s", join(got))
	}

	got = targetsFor(batchTarget{prefix, netip.MustParseAddr("5.6.7.9")}, pick(40))
	if join(got) != "visitor=5.6.7.9 gateway=5.6.7.1 random=5.6.7.42" {
		t.Errorf("targets = %s", join(got))
	}
	// The random host is in 2..254 and never the visitor: 7 is the visitor, so the next draw is used.
	got = targetsFor(batchTarget{prefix, netip.MustParseAddr("5.6.7.9")}, pick(7, 12))
	if join(got) != "visitor=5.6.7.9 gateway=5.6.7.1 random=5.6.7.14" {
		t.Errorf("a draw that hits the visitor must be redrawn: %s", join(got))
	}
	// The extremes of the range.
	if got = targetsFor(batchTarget{prefix, netip.MustParseAddr("5.6.7.9")}, pick(0)); got[2].Addr.String() != "5.6.7.2" {
		t.Errorf("lowest random = %s", got[2].Addr)
	}
	if got = targetsFor(batchTarget{prefix, netip.MustParseAddr("5.6.7.9")}, pick(252)); got[2].Addr.String() != "5.6.7.254" {
		t.Errorf("highest random = %s", got[2].Addr)
	}
	// The visitor is the gateway: no separate gateway target.
	got = targetsFor(batchTarget{prefix, netip.MustParseAddr("5.6.7.1")}, pick(40))
	if join(got) != "visitor=5.6.7.1 random=5.6.7.42" {
		t.Errorf("visitor .1: %s", join(got))
	}
	// The visitor is the host a draw lands on.
	got = targetsFor(batchTarget{prefix, netip.MustParseAddr("5.6.7.42")}, pick(40, 41))
	if join(got) != "visitor=5.6.7.42 gateway=5.6.7.1 random=5.6.7.43" {
		t.Errorf("redraw: %s", join(got))
	}
	for _, x := range got {
		if x.Prefix != prefix {
			t.Errorf("target %v outside the /24", x)
		}
	}
}

// traceBatch hands scamper a file of targets (never an address twice), parses
// its output, and gives every target a row, with an empty path when scamper had
// no result for it.
func TestTraceBatch(t *testing.T) {
	p := &Pipeline{DataDir: t.TempDir(), intn: func(int) int { return 0 }} // the random host is .2
	var gotPPS int
	var listed []string
	p.scamper = func(_ context.Context, targets, outFile string, pps int) error {
		gotPPS = pps
		data, err := os.ReadFile(targets)
		if err != nil {
			return err
		}
		listed = strings.Fields(string(data))
		return os.WriteFile(outFile, []byte(strings.ReplaceAll(scamperFixture, "20.30.40.7", "20.30.40.2")), 0o640)
	}
	batch := []batchTarget{
		{netip.MustParsePrefix("5.6.7.0/24"), netip.MustParseAddr("5.6.7.9")},
		{netip.MustParsePrefix("20.30.40.0/24"), netip.MustParseAddr("20.30.40.200")},
	}
	rows, out, err := p.traceBatch(context.Background(), batch, TraceOutput{})
	if err != nil {
		t.Fatal(err)
	}
	if gotPPS != scamperPPS || strings.Join(listed, " ") != "5.6.7.9 5.6.7.1 5.6.7.2 20.30.40.200 20.30.40.1 20.30.40.2" {
		t.Errorf("scamper ran at %d pps on %v", gotPPS, listed)
	}
	// The files of the run are gone.
	for _, name := range []string{targetsFile, scamperOut} {
		if _, err := os.Stat(filepath.Join(p.dir(), name)); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", name, err)
		}
	}
	if len(rows) != 6 || out.Networks != 2 || out.Targets != 6 {
		t.Fatalf("rows = %d, out = %+v", len(rows), out)
	}
	// 5.6.7.9 answered fully, 5.6.7.1 answered partway, the 20.30.40 targets: 20.30.40.2 had a trace with no hop, the others no result.
	if out.Answered != 2 || out.Silent != 4 || out.Reached != 1 {
		t.Errorf("out = %+v", out)
	}
	byTarget := map[string]traceRow{}
	for _, r := range rows {
		byTarget[r.Target] = r
	}
	// Answered is per trace: any hop that replied.
	for target, want := range map[string]bool{"5.6.7.9": true, "5.6.7.1": true, "5.6.7.2": false, "20.30.40.200": false, "20.30.40.1": false} {
		if got := byTarget[target].Answered; got != want {
			t.Errorf("%s answered = %v, want %v", target, got, want)
		}
	}
	if r := byTarget["5.6.7.9"]; r.Kind != kindVisitor || r.Prefix != "5.6.7.0/24" || !strings.Contains(string(r.Hops), `"ip":"5.6.7.9"`) {
		t.Errorf("visitor row = %+v", r)
	}
	// The candidates are stored with the path: the target, then the tail hops of the public ones.
	if r := byTarget["5.6.7.9"]; !reflect.DeepEqual(r.Candidates, addrs("5.6.7.9", "5.6.0.1")) {
		t.Errorf("visitor candidates = %v", r.Candidates)
	}
	if r := byTarget["20.30.40.200"]; !reflect.DeepEqual(r.Candidates, addrs("20.30.40.200")) {
		t.Errorf("silent candidates = %v", r.Candidates)
	}
	if r := byTarget["20.30.40.200"]; string(r.Hops) != "[]" || r.Kind != kindVisitor {
		t.Errorf("a target without a result must be stored with an empty path: %+v", r)
	}
	if r := byTarget["5.6.7.2"]; string(r.Hops) != "[]" || r.Kind != kindRandom {
		t.Errorf("random row = %+v", r)
	}

	// A scamper failure stores nothing.
	p.scamper = func(context.Context, string, string, int) error { return errors.New("boom") }
	if rows, _, err := p.traceBatch(context.Background(), batch, TraceOutput{}); err == nil || rows != nil {
		t.Errorf("failed run: %v %v", rows, err)
	}
	// Output with no trace object (scamper exits 0 when it cannot open its socket): an error, nothing stored.
	p.scamper = func(_ context.Context, _, outFile string, _ int) error {
		return os.WriteFile(outFile, []byte(`{"type":"cycle-start", "id":1}`+"\n"+`{"type":"cycle-stop", "id":1}`+"\n"), 0o640)
	}
	if rows, _, err := p.traceBatch(context.Background(), batch, TraceOutput{}); err == nil || rows != nil || !strings.Contains(err.Error(), "no trace") {
		t.Errorf("run without traces: %v %v", rows, err)
	}
	// An empty batch does not start scamper.
	p.scamper = func(context.Context, string, string, int) error { t.Error("scamper started"); return nil }
	if rows, out, err := p.traceBatch(context.Background(), nil, TraceOutput{}); err != nil || rows != nil || out.Targets != 0 {
		t.Errorf("empty batch: %v %+v %v", rows, out, err)
	}
}

func TestScamperCommand(t *testing.T) {
	// The real runner needs scamper; here only that a missing binary is an error that names no IP.
	err := runScamper(context.Background(), filepath.Join(t.TempDir(), "scamper"), "/nonexistent/targets", filepath.Join(t.TempDir(), "out"), scamperPPS)
	if err == nil || !strings.Contains(err.Error(), "scamper") {
		t.Errorf("err = %v", err)
	}
	// No binary path (the executable is unknown): an error, never a scamper from the PATH.
	if err := runScamper(context.Background(), "", "t", "o", scamperPPS); err == nil {
		t.Error("empty binary path accepted")
	}
}

// scamper exits 0 when it cannot open its raw socket; the runner must still fail,
// without quoting an address from stderr.
func TestScamperSocketFailure(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "scamper")
	script := "#!/bin/sh\n" +
		"echo 'scamper_icmp4_open_fd: could not open ICMP socket: Operation not permitted' >&2\n" +
		"echo 'trace to 5.6.7.9 failed: could not open icmp4 socket' >&2\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runScamper(context.Background(), fake, "/nonexistent/targets", filepath.Join(dir, "out"), scamperPPS)
	if err == nil || !strings.Contains(err.Error(), "socket") || strings.Contains(err.Error(), "5.6.7.9") {
		t.Errorf("err = %v", err)
	}
	// A clean run (nothing on stderr) is not an error.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runScamper(context.Background(), fake, "/nonexistent/targets", filepath.Join(dir, "out"), scamperPPS); err != nil {
		t.Errorf("clean run: %v", err)
	}
}

// storeTraces writes the candidates beside the path, and the backfill fills only rows that have none.
func TestTraceStatementsUseCandidates(t *testing.T) {
	for _, part := range []string{"candidates", "string_to_array(u.cands, ',')::inet[]"} {
		if !strings.Contains(backfillSQL, part) {
			t.Errorf("backfillSQL lacks %q", part)
		}
	}
	if !strings.Contains(backfillSQL, "t.candidates IS NULL") || !strings.Contains(backfillQuery, "WHERE candidates IS NULL") {
		t.Error("the backfill must touch only rows without candidates")
	}
}
