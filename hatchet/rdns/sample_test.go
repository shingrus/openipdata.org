package rdns

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

const gapsText = `10.0.0.0/24 4 DE
10.0.5.0/24 1 DE
junk line
10.0.9.0/24 0 FR
10.1.0.0/16 2 FR
10.0.9.0/24 2 fr
255.255.255.0/24 2 US
10.0.10.0/24 3 FRA
`

func TestParseGaps(t *testing.T) {
	g := parseGaps([]byte(gapsText))
	// Kept: the first, second and the lower-case fr run. Skipped: junk, a count of 0, a /16, a run past the
	// end of IPv4 and a 3-letter country.
	if len(g.runs) != 3 || g.total != 7 {
		t.Fatalf("runs = %+v, total %d", g.runs, g.total)
	}
	idx := func(p string) uint32 {
		b := netip.MustParsePrefix(p).Addr().As4()
		return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	}
	for _, want := range []struct {
		prefix  string
		country string
		ok      bool
	}{
		{"10.0.0.0/24", "DE", true}, {"10.0.3.0/24", "DE", true}, {"10.0.4.0/24", "", false}, {"10.0.5.0/24", "DE", true},
		{"10.0.9.0/24", "FR", true}, {"10.0.10.0/24", "FR", true}, {"10.0.11.0/24", "", false}, {"9.255.255.0/24", "", false},
		{"255.255.255.0/24", "", false},
	} {
		c, ok := g.contains(idx(want.prefix))
		if c != want.country || ok != want.ok {
			t.Errorf("contains(%s) = %q %v, want %q %v", want.prefix, c, ok, want.country, want.ok)
		}
	}
}

// The walk is a bijection on the 2^24 /24 indexes: every /24 is visited once per cycle.
func TestWalkIsABijection(t *testing.T) {
	seen := make([]bool, walkSize)
	for i := uint32(0); i < walkSize; i++ {
		v := walkAt(i)
		if v >= walkSize || seen[v] {
			t.Fatalf("position %d gives %d, taken or out of range", i, v)
		}
		seen[v] = true
	}
	// Neighbours in the walk are far apart in the address space.
	if d := int64(walkAt(1)) - int64(walkAt(0)); d < 1<<16 && d > -(1<<16) {
		t.Errorf("neighbours only %d apart", d)
	}
}

// walkGap takes the gap /24s in walk order, from the cursor, up to want, and says where to go on.
func TestWalkGap(t *testing.T) {
	g := parseGaps([]byte("20.0.0.0/24 1000 FR\n30.0.0.0/24 4 DE\n"))
	intn := func(int) int { return 7 }
	all, end := walkGap(g, newDoneSet(), 0, 1<<30, walkSize, intn)
	if len(all) != g.total || end != walkSize {
		t.Fatalf("whole walk: %d of %d, end %d", len(all), g.total, end)
	}
	seen := map[netip.Prefix]bool{}
	for k, s := range all {
		if seen[s.Prefix] {
			t.Fatalf("%s twice", s.Prefix)
		}
		seen[s.Prefix] = true
		if c, ok := g.contains(walkAt(uint32(s.Pos))); !ok || c != s.Country {
			t.Fatalf("%s not in the gap at its position", s.Prefix)
		}
		if k > 0 && s.Pos <= all[k-1].Pos {
			t.Fatalf("positions not increasing")
		}
		for _, h := range s.Hosts {
			if !s.Prefix.Contains(h) {
				t.Fatalf("host %s outside %s", h, s.Prefix)
			}
		}
	}
	// In pieces from the cursor: the same /24s in the same order, nothing twice, nothing missed.
	var got []sampled
	cursor := 0
	for cursor < walkSize {
		part, next := walkGap(g, newDoneSet(), cursor, 100, walkSize, intn)
		if len(part) > 100 || next <= cursor {
			t.Fatalf("part of %d, cursor %d -> %d", len(part), cursor, next)
		}
		if len(part) == 100 && next != part[99].Pos+1 {
			t.Fatalf("a full part ends right after its last /24: %d vs %d", next, part[99].Pos+1)
		}
		got = append(got, part...)
		cursor = next
	}
	if len(got) != len(all) {
		t.Fatalf("pieces found %d, whole walk %d", len(got), len(all))
	}
	for i := range got {
		if got[i].Prefix != all[i].Prefix {
			t.Fatalf("order differs at %d", i)
		}
	}
	// The step cap stops the walk early, without a /24 being skipped.
	part, next := walkGap(g, newDoneSet(), 0, 1<<30, 1000, intn)
	if next != 1000 || len(part) > 1000 {
		t.Errorf("cap: %d found, end %d", len(part), next)
	}
	if p, n := walkGap(g, newDoneSet(), walkSize, 10, 10, intn); len(p) != 0 || n != walkSize {
		t.Errorf("past the end: %v %d", p, n)
	}
}

// walkGap passes an index whose bit is done: the cursor moves past it, it is not picked.
func TestWalkGapSkipsDone(t *testing.T) {
	g := parseGaps([]byte("20.0.0.0/24 1000 FR\n"))
	intn := func(int) int { return 7 }
	all, _ := walkGap(g, newDoneSet(), 0, 1<<30, walkSize, intn)
	done := newDoneSet()
	skip := map[netip.Prefix]bool{}
	for k := 0; k < len(all); k += 3 {
		b := all[k].Prefix.Addr().As4()
		done.set(uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]))
		skip[all[k].Prefix] = true
	}
	got, end := walkGap(g, done, 0, 1<<30, walkSize, intn)
	if end != walkSize || len(got) != len(all)-len(skip) {
		t.Fatalf("got %d, want %d, end %d", len(got), len(all)-len(skip), end)
	}
	for _, s := range got {
		if skip[s.Prefix] {
			t.Fatalf("%s is done but was picked", s.Prefix)
		}
	}
	// The cursor still passes done indexes: one pick wanted, the end is after the skipped ones before it.
	first, end := walkGap(g, done, 0, 1, walkSize, intn)
	if len(first) != 1 || end != first[0].Pos+1 || first[0].Pos <= all[0].Pos {
		t.Fatalf("first %v end %d", first, end)
	}
}

func TestSampleState(t *testing.T) {
	path := t.TempDir() + "/sample-state.json"
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	if s := readSampleState(path, now); s.Cursor != 0 || !s.CycleStarted.Equal(now) || !s.LastRun.IsZero() {
		t.Errorf("missing file: %+v", s)
	}
	want := sampleState{Cursor: 1234, CycleStarted: now.Add(-24 * time.Hour), LastRun: now.Add(-time.Hour)}
	if err := writeSampleState(path, want); err != nil {
		t.Fatal(err)
	}
	if got := readSampleState(path, now); got.Cursor != 1234 || !got.CycleStarted.Equal(want.CycleStarted) || !got.LastRun.Equal(want.LastRun) {
		t.Errorf("round trip: %+v", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
	os.WriteFile(path, []byte("{broken"), 0o640)
	if s := readSampleState(path, now); s.Cursor != 0 || !s.CycleStarted.Equal(now) {
		t.Errorf("damaged file: %+v", s)
	}
}

func TestSampleHosts(t *testing.T) {
	p := netip.MustParsePrefix("5.6.7.0/24")
	// Every pair of choices: .1 first, then two distinct hosts of .2 to .254, never .0 or .255.
	seenHost := map[netip.Addr]bool{}
	for a := 0; a < 253; a++ {
		for c := 0; c < 252; c++ {
			seq := []int{a, c}
			i := 0
			h := sampleHosts(p, func(n int) int { v := seq[i]; i++; return min(v, n-1) })
			if h[0].String() != "5.6.7.1" || h[1] == h[2] || h[1] == h[0] || h[2] == h[0] {
				t.Fatalf("a=%d c=%d: hosts %v", a, c, h)
			}
			for _, x := range h[1:] {
				last := x.As4()[3]
				if last < 2 || last > 254 || x.As4()[0] != 5 {
					t.Fatalf("a=%d c=%d: host %s", a, c, x)
				}
				seenHost[x] = true
			}
		}
	}
	if len(seenHost) != 253 { // every host from .2 to .254 can be chosen
		t.Errorf("%d different hosts chosen", len(seenHost))
	}
	// A generator that always answers the same still terminates with distinct hosts.
	h := sampleHosts(p, func(int) int { return 0 })
	if h[1] == h[2] {
		t.Errorf("hosts %v", h)
	}
}

// The gap list is made again only when the ETag of the MMDB changes; with no usable MMDB nothing
// is made and the run reports it.
func TestEnsureGaps(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.put(todayKey, "e1", "mmdb one")
	p, _ := testPipeline(t, store)
	var dbs []string
	p.gaps = func(_ context.Context, db, out string) error {
		data, _ := os.ReadFile(db)
		dbs = append(dbs, string(data))
		return os.WriteFile(out, []byte("10.0.0.0/24 2 DE\n"), 0o640)
	}

	res, built := p.ensureGaps(ctx, p.statMMDBFor(ctx, whoSample))
	if res.Status != statusUsed || !built || len(dbs) != 1 || dbs[0] != "mmdb one" || res.ETag != "e1" {
		t.Fatalf("first: %+v built %v dbs %v", res, built, dbs)
	}
	if etag := savedMMDBETag(p.gapsPath()); etag != "e1" {
		t.Errorf("saved gaps ETag = %q", etag)
	}
	// Same ETag: no new list and no download.
	downloads := len(store.downloads)
	if res, built = p.ensureGaps(ctx, p.statMMDBFor(ctx, whoSample)); built || res.Status != statusUsed || len(dbs) != 1 || len(store.downloads) != downloads {
		t.Fatalf("same ETag: %+v built %v", res, built)
	}
	// A new MMDB: a new list.
	store.put(todayKey, "e2", "mmdb two")
	if res, built = p.ensureGaps(ctx, p.statMMDBFor(ctx, whoSample)); !built || len(dbs) != 2 || dbs[1] != "mmdb two" || savedMMDBETag(p.gapsPath()) != "e2" {
		t.Fatalf("new ETag: %+v built %v dbs %v", res, built, dbs)
	}
	// R2 down: the saved copy has the ETag of the list, so nothing is made.
	store.statErr = errors.New("r2 down")
	if res, built = p.ensureGaps(ctx, p.statMMDBFor(ctx, whoSample)); built || res.Status != statusSavedCopy || res.Reason != reasonStat || len(dbs) != 2 {
		t.Fatalf("outage: %+v built %v", res, built)
	}
	// No MMDB at all: skipped, with the old list left alone.
	empty, _ := testPipeline(t, newFakeStore())
	empty.gaps = p.gaps
	if res, built = empty.ensureGaps(ctx, empty.statMMDBFor(ctx, whoSample)); res.Status != statusSkipped || res.Reason != reasonNoObject || built {
		t.Fatalf("no MMDB: %+v built %v", res, built)
	}
}

// A failing mmdbbuilder gaps skips the run (no error) and leaves the saved ETag as it was; the
// log never holds an IP.
func TestEnsureGapsFailure(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.put(todayKey, "e1", "mmdb")
	p, logs := testPipeline(t, store)
	p.gaps = func(context.Context, string, string) error { return errors.New("boom at 203.0.113.9") }
	res, built := p.ensureGaps(ctx, p.statMMDBFor(ctx, whoSample))
	if res.Status != statusSkipped || res.Reason != reasonGaps || built || savedMMDBETag(p.gapsPath()) != "" {
		t.Fatalf("%+v built %v", res, built)
	}
	if text := strings.Join(*logs, "\n"); !strings.Contains(text, "WARNING") || strings.Contains(text, "203.0.113.9") {
		t.Errorf("logs: %s", text)
	}
}
