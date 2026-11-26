package ping

import (
	"bufio"
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
)

func listSource(lists ...[]string) hostingSource {
	return func(_ context.Context, reset func(), visit func([]string)) error {
		reset()
		for _, l := range lists {
			visit(l)
		}
		return nil
	}
}

func TestExcludeFileParses(t *testing.T) {
	if _, err := parseExclude(excludeText); err != nil {
		t.Fatal(err)
	}
}

func TestParseExclude(t *testing.T) {
	spans, err := parseExclude("# c\n\n20.0.0.0/23 # x\n20.0.2.0/24\n30.1.2.3/32\n")
	if err != nil || len(spans) != 2 {
		t.Fatalf("%v %v", spans, err)
	}
	if spans[0].last-spans[0].first != 2 { // /23 and the next /24 merged: three /24s
		t.Errorf("merge: %+v", spans[0])
	}
	if !inSpans(spans, 20<<16|2) || inSpans(spans, 20<<16|3) || !inSpans(spans, 30<<16|1<<8|2) {
		t.Error("inSpans")
	}
	for _, bad := range []string{"nope\n", "2001:db8::/32\n", "20.0.0.0/33\n", "20.0.0.0 /24\n"} {
		if _, err := parseExclude(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBuildTargets(t *testing.T) {
	ex, _ := parseExclude("20.0.1.0/24\n")
	out, blocks, err := buildTargets(context.Background(), listSource(
		[]string{"20.0.0.0/22", "21.0.0.128/25", "bad", "22.0.0.0/7", "10.0.0.0/8", "2001:db8::/32", " 30.0.0.0/24 "},
		[]string{"30.0.0.0/24"}, // the same /24 again: once
		nil,
	), ex)
	if err != nil {
		t.Fatal(err)
	}
	// /22 gives 4 (one excluded), the /25 its /24, 30.0.0.0/24 once; 10/8 is not public, /7 is broader than /8.
	if out.HostingNetworks != 3 || out.Prefixes != 5 || out.Blocks != 6 || out.Excluded != 1 || out.Targets != 5 || len(blocks) != 5 {
		t.Errorf("%+v", out)
	}
	want := []uint32{20<<16 | 0, 20<<16 | 2, 20<<16 | 3, 21 << 16, 30 << 16}
	for i, b := range want {
		if blocks[i] != b {
			t.Errorf("block %d = %d, want %d", i, blocks[i], b)
		}
	}
	// A prefix longer than /24 gives its /24.
	_, blocks, _ = buildTargets(context.Background(), listSource([]string{"21.0.0.130/32"}), nil)
	if len(blocks) != 1 || blocks[0] != 21<<16 {
		t.Errorf("/32: %v", blocks)
	}
}

func TestHostingList(t *testing.T) {
	list, ok := hostingList(`{"metadata":{"category":"hosting"},"prefixes":{"ipv4":["1.0.0.0/24",5,"2.0.0.0/24"]}}`)
	if !ok || len(list) != 2 {
		t.Errorf("%v %v", list, ok)
	}
	for _, bad := range []string{`{"metadata":{"category":"isp"},"prefixes":{"ipv4":["1.0.0.0/24"]}}`, `{"metadata":{"category":"hosting"}}`, `x`, ``} {
		if _, ok := hostingList(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func targetsPipe(t *testing.T, src hostingSource) (*Pipeline, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	return &Pipeline{DataDir: t.TempDir(), GCPKey: "k", Progress: func(string) {}, store: store, hosting: src, compute: newFakeCompute(), minTargets: 3, maxTargets: 100}, store
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func TestTargetsTask(t *testing.T) {
	p, store := targetsPipe(t, listSource([]string{"20.0.0.0/22", "30.0.0.0/24"}))
	store.set(RunPrefix+"out/old.txt.gz", []byte("old"))
	store.set("internal/ping/hosting-rtt.csv.gz", []byte("keep"))
	out, err := p.Targets(context.Background())
	if err != nil || out.Targets != 5 || out.OldDeleted != 1 || out.Upload != "uploaded" || out.HostingNetworks != 1 && out.HostingNetworks != 2 {
		t.Fatalf("%+v %v", out, err)
	}
	if store.has(RunPrefix+"out/old.txt.gz") || !store.has("internal/ping/hosting-rtt.csv.gz") || !store.has(TargetsKey) {
		t.Error("objects of the run")
	}
	if store.events[0] != "delete "+RunPrefix {
		t.Errorf("the old objects must go first: %v", store.events)
	}
	if !store.putOpts[TargetsKey].Force {
		t.Error("the list is always sent")
	}
	// A VM of a crashed run is deleted before the objects, so it cannot upload into this run.
	comp := newFakeCompute()
	comp.vms[vmName("us-east1")] = vmRef{"us-east1-b", vmName("us-east1")}
	p.compute = comp
	if _, err := p.Targets(context.Background()); err != nil || len(comp.vms) != 0 {
		t.Errorf("leftover VM: %v, %d left", err, len(comp.vms))
	}
	lines := readLines(t, p.targetsPath())
	if len(lines) != 5 {
		t.Fatalf("%d lines", len(lines))
	}
	sorted := true
	for i, l := range lines {
		a, err := netip.ParseAddr(l)
		if err != nil || a.As4()[3] != 1 {
			t.Errorf("line %d %q", i, l)
		}
		if i > 0 && l < lines[i-1] {
			sorted = false
		}
	}
	// Shuffled with a fixed seed: a second run gives the same order.
	p2, _ := targetsPipe(t, listSource([]string{"20.0.0.0/22", "30.0.0.0/24"}))
	if _, err := p2.Targets(context.Background()); err != nil {
		t.Fatal(err)
	}
	again := readLines(t, p2.targetsPath())
	for i := range lines {
		if lines[i] != again[i] {
			t.Fatal("the order is not fixed")
		}
	}
	_ = sorted
	if string(store.objs[TargetsKey]) != strings.Join(lines, "\n")+"\n" {
		t.Error("the uploaded list differs from the file")
	}
}

func TestTargetsLimits(t *testing.T) {
	p, store := targetsPipe(t, listSource([]string{"20.0.0.0/24"}))
	if _, err := p.Targets(context.Background()); !errors.Is(err, ErrPermanent) {
		t.Errorf("too few: %v", err)
	}
	if store.has(TargetsKey) {
		t.Error("a refused list was uploaded")
	}
	p, _ = targetsPipe(t, listSource([]string{"20.0.0.0/20"}))
	p.maxTargets = 10
	if _, err := p.Targets(context.Background()); !errors.Is(err, ErrPermanent) {
		t.Errorf("too many: %v", err)
	}
}

func TestTargetsSkipsWithoutKey(t *testing.T) {
	p, store := targetsPipe(t, listSource([]string{"20.0.0.0/22"}))
	p.GCPKey = ""
	out, err := p.Targets(context.Background())
	if err != nil || out.Skipped != "no GCP key" || len(store.events) != 0 {
		t.Errorf("%+v %v", out, err)
	}
}

func TestTargetsSourceError(t *testing.T) {
	p, _ := targetsPipe(t, func(context.Context, func(), func([]string)) error { return errors.New("db down") })
	if _, err := p.Targets(context.Background()); err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("%v", err)
	}
}
