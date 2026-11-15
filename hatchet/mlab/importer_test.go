package mlab

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const csvHeader = "client24,hop_ip,from_end,first_day,traces\n"

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func newPipeline(t *testing.T) *Pipeline {
	t.Helper()
	return &Pipeline{DataDir: t.TempDir(), minRows: 1}
}

func writeInbox(t *testing.T, p *Pipeline, name string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(p.inbox(), 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.inbox(), name)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every bad row is counted by its reason and never echoed; good rows are merged.
func TestImportCountsBadRows(t *testing.T) {
	p := newPipeline(t)
	rows := []string{
		"8.8.8,1.1.1.1,0,2026-11-07,10",                          // good
		"8.8.8,1.1.1.1,1,2026-11-09,5",                           // the same pair: merged, min from_end 0, traces 15
		"8.8.8,1.0.0.1,2,2026-11-08,3",                           // good
		"8.8.9,1.1.1.1,0,2026-11-13,1",                           // good
		"8.8.8,1.1.1.1,3,2026-11-07,1",                           // from_end 3
		"8.8.8,1.1.1.1,x,2026-11-07,1",                           // from_end not a number
		"8.8.8,1.1.1.1,0,2026-11-07,0",                           // traces 0
		"8.8.8,1.1.1.1,0,2026-11-07,-4",                          // traces negative
		"8.8.8,1.1.1.1,0,2026-11-07,abc",                         // traces not a number
		"8.8,1.1.1.1,0,2026-11-07,1",                             // client too short
		"8.8.8.8,1.1.1.1,0,2026-11-07,1",                         // client with four octets
		"8.8.300,1.1.1.1,0,2026-11-07,1",                         // octet out of range
		"::1,1.1.1.1,0,2026-11-07,1",                             // not an IPv4 client
		"8.8.8,not-an-ip,0,2026-11-07,1",                         // hop invalid
		"8.8.8,2606:4700::1111,0,2026-11-07,1",                   // hop IPv6
		"8.8.8,::ffff:1.1.1.1,0,2026-11-07,1",                    // hop IPv4-mapped
		"8.8.8,*,0,2026-11-07,1",                                 // hop '*'
		"8.8.8,10.0.0.1,0,2026-11-07,1",                          // private hop
		"10.1.1,1.1.1.1,0,2026-11-07,1",                          // private client
		"192.0.2,1.1.1.1,0,2026-11-07,1",                         // documentation client
		"8.8.8,1.1.1.1",                                          // too few columns
		"",                                                       // blank: not counted
		"8.8.8," + strings.Repeat("9", 5000) + ",0,2026-11-07,1", // too long
	}
	path := writeInbox(t, p, "x.csv", []byte(csvHeader+strings.Join(rows, "\r\n")+"\r\n"))
	out, err := p.ImportFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	want := ImportOutput{File: "x.csv", Rows: 22, Valid: 4, BadColumns: 1, BadTooLong: 1, BadClient: 4, BadHop: 4, BadFromEnd: 2, BadTraces: 3,
		NotPublicClient: 2, NotPublicHop: 1, Pairs: 3, Clients: 2, Hops: 2, FirstDay: "2026-11-07", LastDay: "2026-11-13", Days: 7}
	out.Bytes, out.PairsBytes, out.Seconds = 0, 0, 0
	if out != want {
		t.Errorf("output\n%+v\nwant\n%+v", out, want)
	}
	pairs, err := readPairs(p.pairsPath())
	if err != nil {
		t.Fatal(err)
	}
	wantPairs := []pair{
		makePair(0x080808, 2, 0x01000001, 3), // 1.0.0.1, from_end 2
		makePair(0x080808, 0, 0x01010101, 15),
		makePair(0x080809, 0, 0x01010101, 1),
	}
	if len(pairs) != 3 || pairs[0] != wantPairs[0] || pairs[1] != wantPairs[1] || pairs[2] != wantPairs[2] {
		t.Errorf("pairs = %+v, want %+v", pairs, wantPairs)
	}
	// No bad row is echoed anywhere in the output or the log.
	if strings.Contains(fmt.Sprintf("%+v", out), "10.0.0.1") {
		t.Error("a row was echoed")
	}
}

func TestImportGzipColumnsByHeaderAndTraceCap(t *testing.T) {
	p := newPipeline(t)
	// Columns in another order, an extra column, a BOM, quotes, no first_day.
	data := "\xef\xbb\xbftraces,extra,hop_ip,client24,from_end\n" +
		"2000000,z,\"1.1.1.1\",\"8.8.8\",0\n" + // capped to 1e6
		"7,z,1.0.0.1,8.8.8,1\n"
	path := writeInbox(t, p, "y.csv.gz", gz(t, data))
	out, err := p.ImportFile(context.Background(), path)
	if err != nil || out.Valid != 2 || out.Pairs != 2 || out.Days != 0 || out.FirstDay != "" {
		t.Fatalf("%+v %v", out, err)
	}
	pairs, _ := readPairs(p.pairsPath())
	if pairs[1].traces != 1_000_000 || pairs[0].traces != 7 {
		t.Errorf("pairs = %+v", pairs)
	}
	// Saturating sum of duplicates.
	merged := mergeSorted([]pair{makePair(1, 0, 5, 4_000_000_000), makePair(1, 2, 5, 4_000_000_000)})
	if len(merged) != 1 || merged[0].traces != 4294967295 || merged[0].fromEnd() != 0 {
		t.Errorf("merged = %+v", merged)
	}
}

func TestImportRefusals(t *testing.T) {
	p := newPipeline(t)
	for name, data := range map[string]string{
		"nohead.csv":  "",
		"badhead.csv": "a,b,c\n1,2,3\n",
		"nocol.csv":   "client24,hop_ip,from_end\n8.8.8,1.1.1.1,0\n",
		"fewrows.csv": csvHeader + "10.1.1,1.1.1.1,0,2026-11-07,1\n",
	} {
		path := writeInbox(t, p, name, []byte(data))
		if _, err := p.ImportFile(context.Background(), path); !errors.Is(err, ErrPermanent) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Not a gzip file with the gzip magic bytes.
	path := writeInbox(t, p, "broken.csv.gz", []byte{0x1f, 0x8b, 1, 2, 3})
	if _, err := p.ImportFile(context.Background(), path); !errors.Is(err, ErrPermanent) {
		t.Errorf("broken gzip: %v", err)
	}
	if _, err := p.ImportFile(context.Background(), filepath.Join(p.dir(), "missing.csv")); !errors.Is(err, ErrPermanent) {
		t.Errorf("missing: %v", err)
	}
	// The default minimum is a real export's size; a refused import leaves the old pairs file.
	q := &Pipeline{DataDir: p.DataDir}
	if err := os.MkdirAll(q.dir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := writePairs(q.pairsPath(), []pair{makePair(1, 0, 2, 3)}); err != nil {
		t.Fatal(err)
	}
	ok := writeInbox(t, q, "ok.csv", []byte(csvHeader+"8.8.8,1.1.1.1,0,2026-11-07,1\n"))
	if _, err := q.ImportFile(context.Background(), ok); err == nil || !errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "100000") {
		t.Errorf("default minimum: %v", err)
	}
	if pairs, err := readPairs(q.pairsPath()); err != nil || len(pairs) != 1 || pairs[0] != makePair(1, 0, 2, 3) {
		t.Errorf("the old pairs file changed: %+v %v", pairs, err)
	}
}

// Duplicates make up most of an input that is over the pair limit: they are merged on the way,
// and a limit that distinct pairs reach is a permanent error.
func TestImportPairLimit(t *testing.T) {
	p := newPipeline(t)
	p.maxPairs = 8
	var dups, distinct strings.Builder
	dups.WriteString(csvHeader)
	distinct.WriteString(csvHeader)
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&dups, "8.8.%d,1.1.1.1,0,2026-11-07,1\n", i%2) // two distinct pairs, 200 rows
		fmt.Fprintf(&distinct, "8.8.%d,1.1.1.1,0,2026-11-07,1\n", i%200)
	}
	out, err := p.ImportFile(context.Background(), writeInbox(t, p, "d.csv", []byte(dups.String())))
	if err != nil || out.Pairs != 2 || out.Valid != 200 {
		t.Fatalf("duplicates: %+v %v", out, err)
	}
	if _, err := p.ImportFile(context.Background(), writeInbox(t, p, "e.csv", []byte(distinct.String()))); !errors.Is(err, ErrPermanent) {
		t.Errorf("distinct pairs over the limit: %v", err)
	}
}

func TestImportCancelled(t *testing.T) {
	p := newPipeline(t)
	var b strings.Builder
	b.WriteString(csvHeader)
	for i := 0; i < 70000; i++ {
		b.WriteString("8.8.8,1.1.1.1,0,2026-11-07,1\n")
	}
	path := writeInbox(t, p, "c.csv", []byte(b.String()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.ImportFile(ctx, path); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestInboxFileChoice(t *testing.T) {
	p := newPipeline(t)
	if _, err := p.inboxFile(""); !errors.Is(err, ErrPermanent) {
		t.Errorf("no inbox: %v", err)
	}
	old := writeInbox(t, p, "a.csv", nil)
	newer := writeInbox(t, p, "b.csv.gz", nil)
	writeInbox(t, p, "notes.txt", nil)
	writeInbox(t, p, "c.csv.tmp", nil)
	os.Mkdir(filepath.Join(p.inbox(), "dir.csv"), 0o750)
	now := time.Now()
	os.Chtimes(old, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	os.Chtimes(newer, now.Add(-time.Hour), now.Add(-time.Hour))
	if got, err := p.inboxFile(""); err != nil || got != newer {
		t.Errorf("newest = %q %v", got, err)
	}
	if got, err := p.inboxFile("a.csv"); err != nil || got != old {
		t.Errorf("named = %q %v", got, err)
	}
	// Same time: the name decides, so the choice is stable.
	os.Chtimes(old, now, now)
	os.Chtimes(newer, now, now)
	if got, _ := p.inboxFile(""); got != newer {
		t.Errorf("tie = %q", got)
	}
}

// The file input is a base name: no path, no dot-dot, no other type, no link, no directory.
func TestInboxFileBaseNameOnly(t *testing.T) {
	p := newPipeline(t)
	writeInbox(t, p, "ok.csv", nil)
	outside := filepath.Join(p.DataDir, "secret.csv")
	os.WriteFile(outside, []byte("x"), 0o640)
	os.Symlink(outside, filepath.Join(p.inbox(), "link.csv"))
	os.Mkdir(filepath.Join(p.inbox(), "dir.csv"), 0o750)
	for _, name := range []string{"../secret.csv", "sub/ok.csv", "/etc/passwd.csv", "..", ".", `..\secret.csv`, "ok.csv/", "ok.txt", "missing.csv", "link.csv", "dir.csv", "a\x00.csv"} {
		if got, err := p.inboxFile(name); !errors.Is(err, ErrPermanent) {
			t.Errorf("%q accepted: %q %v", name, got, err)
		}
	}
	if _, err := p.inboxFile("ok.csv"); err != nil {
		t.Error(err)
	}
	// Import with the name goes through the same check.
	if _, err := p.Import(context.Background(), "../secret.csv"); !errors.Is(err, ErrPermanent) {
		t.Errorf("Import: %v", err)
	}
}

func TestPairsFileRoundTrip(t *testing.T) {
	p := newPipeline(t)
	os.MkdirAll(p.dir(), 0o750)
	in := []pair{makePair(0x080808, 0, 0x01010101, 15), makePair(0x080808, 2, 0x01010102, 1), makePair(0xC00000, 1, 0xFFFFFFFE, 4294967295)}
	if _, err := writePairs(p.pairsPath(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.pairsPath() + ".tmp"); !os.IsNotExist(err) {
		t.Error("temporary file left")
	}
	got, err := readPairs(p.pairsPath())
	if err != nil || len(got) != 3 || got[0] != in[0] || got[1] != in[1] || got[2] != in[2] {
		t.Errorf("round trip: %+v %v", got, err)
	}
	// A file out of order (written by hand) is sorted and merged.
	if _, err := writePairs(p.pairsPath(), []pair{in[2], in[0], in[0], in[1]}); err != nil {
		t.Fatal(err)
	}
	if got, err = readPairs(p.pairsPath()); err != nil || len(got) != 3 || got[0].traces != 30 {
		t.Errorf("unordered: %+v %v", got, err)
	}
	// A damaged line or header is an error; a missing file is the permanent "no pairs".
	for name, content := range map[string]string{"head": "wrong\n", "line": pairsHeader + "\n8.8.8,1.1.1.1,0\n", "from": pairsHeader + "\n8.8.8,1.1.1.1,5,1\n"} {
		if err := os.WriteFile(p.pairsPath(), gz(t, content), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := readPairs(p.pairsPath()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	os.Remove(p.pairsPath())
	if _, err := readPairs(p.pairsPath()); !errors.Is(err, ErrPermanent) {
		t.Errorf("missing: %v", err)
	}
	if h := distinctHops(in); len(h) != 3 || h[0] != 0x01010101 {
		t.Errorf("hops = %v", h)
	}
}

// A later import replaces the pairs file: each export is the whole current window.
func TestImportReplacesPairs(t *testing.T) {
	p := newPipeline(t)
	a := writeInbox(t, p, "a.csv", []byte(csvHeader+"8.8.8,1.1.1.1,0,2026-11-07,1\n"))
	b := writeInbox(t, p, "b.csv", []byte(csvHeader+"8.8.9,1.0.0.1,0,2026-11-08,2\n"))
	if _, err := p.ImportFile(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ImportFile(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	pairs, _ := readPairs(p.pairsPath())
	if len(pairs) != 1 || pairs[0] != makePair(0x080809, 0, 0x01000001, 2) {
		t.Errorf("pairs = %+v", pairs)
	}
}
