package main

// The page budgets: the guard against a page that gets heavier or slower.
// Every kind of page has a limit on its size (raw and gzipped), on the memory
// one request allocates and on the time one request takes, all measured on the
// test fixture. A change that goes over a limit fails the tests, and the site
// workflow runs the tests before it publishes a build.
//
// A limit is about 10% above the size, and well above the memory and the time,
// measured when it was set. Raise one only for a change the owner agreed to,
// in the same commit, with the reason on its line. Run
//
//	go test -run TestPageBudgets -v
//
// to see the current numbers of every page. Under the race detector only the
// sizes are checked; the site workflow runs this test a second time without it.

import (
	"io"
	"log"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type pageBudget struct {
	target string
	raw    int           // response body, bytes
	gz     int           // the same gzipped (what a visitor downloads)
	alloc  int           // memory allocated by one request, bytes
	took   time.Duration // one request (the fastest of several, so a busy machine does not fail it)
}

// pageBudgets lists one page of every kind. The time limits are loose on
// purpose (a slow CI machine must pass): they catch a page that got several
// times slower, the memory limit catches the smaller regressions. The comment
// on each line is what was measured when the limit was set (2026-11-17).
var pageBudgets = []pageBudget{
	{"/", 13000, 4700, 110000, 2000 * time.Microsecond},                                    // 11426, 4199, 67537, 25µs
	{"/download", 21000, 6900, 40000, 2000 * time.Microsecond},                             // 18649, 6240, 25772, 3µs
	{"/as64500", 15500, 5300, 140000, 2000 * time.Microsecond},                             // 13980, 4751, 88552, 68µs
	{"/as64503", 79000, 12000, 1490000, 19000 * time.Microsecond},                          // 71472, 10830, 991941, 1824µs
	{"/as64600", 29500, 6400, 490000, 7000 * time.Microsecond},                             // 26433, 5738, 325387, 605µs
	{"/asn-example-net", 15000, 5100, 130000, 2000 * time.Microsecond},                     // 13412, 4575, 80363, 47µs
	{"/asn-huge-net", 112500, 12300, 2530000, 41000 * time.Microsecond},                    // 101928, 11143, 1682261, 4090µs
	{"/prefix/203.0.113.0/24", 18000, 6200, 150000, 2000 * time.Microsecond},               // 15944, 5634, 93909, 45µs
	{"/prefix/198.18.0.0/24", 15500, 5200, 130000, 2000 * time.Microsecond},                // 13769, 4652, 86304, 58µs
	{"/?203.0.113.5", 21500, 7400, 190000, 2000 * time.Microsecond},                        // 19358, 6640, 126370, 69µs
	{"/top-asn-by-ip-address", 27000, 6200, 50000, 2000 * time.Microsecond},                // 24474, 5596, 30922, 3µs
	{"/list-of-countries-by-ipv4-allocation", 14500, 4900, 30000, 2000 * time.Microsecond}, // 12884, 4377, 19946, 2µs
	{"/as64500/prefixes.txt", 100, 100, 30000, 2000 * time.Microsecond},                    // 45, 60, 16020, 7µs
	{"/asn-huge-net/prefixes.txt", 100, 70, 60000, 2000 * time.Microsecond},                // 13, 34, 35956, 24µs
	{"/sitemap.xml", 550, 250, 20000, 2000 * time.Microsecond},                             // 425, 191, 6695, 1µs
	{"/sitemap-networks-1.xml", 38000, 2000, 80000, 2000 * time.Microsecond},               // 34295, 1812, 47277, 3µs
	{"/llms.txt", 3000, 1130, 20000, 2000 * time.Microsecond},                              // 2604, 999, 8880, 1µs
	{"/robots.txt", 200, 150, 10000, 2000 * time.Microsecond},                              // 124, 107, 6319, 1µs
}

func TestPageBudgets(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	main, _ := asnServer(t)
	pfx := prefixServer(t) // the fixture with prefix pages; documentation addresses count as public there
	const runs = 40
	for _, c := range pageBudgets {
		s := main
		if strings.HasPrefix(c.target, "/prefix/") || strings.HasPrefix(c.target, "/?") {
			s = pfx
		}
		w := get(s, "GET", c.target) // also warms what is built once (cached pages, maps)
		if w.Code != 200 {
			t.Errorf("%s: status %d", c.target, w.Code)
			continue
		}
		get(s, "GET", c.target)
		body := w.Body.Bytes()
		raw, gz := len(body), gzipped(body)

		times := make([]time.Duration, runs)
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		for i := range times {
			start := time.Now()
			get(s, "GET", c.target)
			times[i] = time.Since(start)
		}
		runtime.ReadMemStats(&m1)
		alloc := int(m1.TotalAlloc-m0.TotalAlloc) / runs
		took := slices.Min(times)

		t.Logf("%-40s %6d raw %5d gz %8d alloc %8s", c.target, raw, gz, alloc, took.Round(time.Microsecond))
		if raw > c.raw {
			t.Errorf("%s: %d bytes, the limit is %d", c.target, raw, c.raw)
		}
		if gz > c.gz {
			t.Errorf("%s: %d bytes gzipped, the limit is %d", c.target, gz, c.gz)
		}
		if raceOn {
			continue // sizes only: the workflow runs this test once more without the race detector
		}
		if alloc > c.alloc {
			t.Errorf("%s: one request allocates %d bytes, the limit is %d", c.target, alloc, c.alloc)
		}
		if !testing.Short() && took > c.took {
			t.Errorf("%s: one request takes %s, the limit is %s", c.target, took, c.took)
		}
	}
}
