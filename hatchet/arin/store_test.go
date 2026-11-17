package arin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

// fakeR2 is the R2 of the records file: objects by key, with errors to order.
type fakeR2 struct {
	objects     map[string][]byte
	statErr     error
	downloadErr error
	stats       int
	downloads   int
}

func (f *fakeR2) Stat(_ context.Context, key string) (publish.ObjectInfo, error) {
	f.stats++
	if f.statErr != nil {
		return publish.ObjectInfo{}, f.statErr
	}
	if _, ok := f.objects[key]; !ok {
		return publish.ObjectInfo{}, fmt.Errorf("stat: %w", publish.ErrNotFound)
	}
	return publish.ObjectInfo{ETag: "x"}, nil
}

func (f *fakeR2) Download(_ context.Context, key, path string) (time.Time, error) {
	f.downloads++
	if f.downloadErr != nil {
		return time.Time{}, f.downloadErr
	}
	return time.Time{}, os.WriteFile(path, f.objects[key], 0o640)
}

// newFileStore makes a store over a temporary directory.
func newFileStore(t *testing.T, r2 *fakeR2) (*fileStore, *[]string) {
	t.Helper()
	logs := &[]string{}
	s := &fileStore{
		dir: filepath.Join(t.TempDir(), "arin"),
		objects: func() (recordsR2, error) {
			if r2 == nil {
				return nil, publish.ErrInvalidR2
			}
			return r2, nil
		},
		log: func(f string, a ...any) { *logs = append(*logs, fmt.Sprintf(f, a...)) },
	}
	return s, logs
}

// start makes the first base, as the first lookup does (R2 must say it has no copy).
func start(t *testing.T, s *fileStore) {
	t.Helper()
	if _, _, _, err := s.Progress(context.Background(), testNow); err != nil {
		t.Fatalf("first start: %v", err)
	}
}

func recAt(start, end, state, city string, at time.Time) record {
	r := rec(start, end, "US", state, city)
	r.LookedUp = at.Unix()
	return r
}

func readBaseText(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(gz)
	return string(data)
}

func TestSaveAndProgress(t *testing.T) {
	s, _ := newFileStore(t, &fakeR2{})
	ctx := context.Background()
	cursor, started, restored, err := s.Progress(ctx, testNow)
	if err != nil || cursor != 0 || !started.Equal(testNow) || restored {
		t.Fatalf("first start: %d %v %v %v", cursor, started, restored, err)
	}
	at := testNow.Add(-time.Hour)
	if err := s.Save(ctx, []record{rec("10.0.0.0", "10.0.0.255", "US", "OR", "Portland")}, 200, testNow.Add(-24*time.Hour), at); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, nil, 300, testNow.Add(-24*time.Hour), at); err != nil { // no records: only the position
		t.Fatal(err)
	}
	if err := s.Save(ctx, []record{rec("10.0.1.0", "10.0.1.255", "US", "OR", "Portland")}, 400, testNow.Add(-24*time.Hour), at); err != nil {
		t.Fatal(err)
	}
	if cursor, started, _, err = s.Progress(ctx, testNow); err != nil || cursor != 400 || !started.Equal(testNow.Add(-24*time.Hour)) {
		t.Errorf("progress: %d %v %v", cursor, started, err)
	}
	// The journal grew by one batch each time it had records.
	data, _ := os.ReadFile(s.path(journalFile))
	want := fmt.Sprintf("10.0.0.0,10.0.0.255,US,OR,Portland,%d\n10.0.1.0,10.0.1.255,US,OR,Portland,%d\n", at.Unix(), at.Unix())
	if string(data) != want {
		t.Errorf("journal %q", data)
	}
	// The files are 0640 and no .tmp is left.
	if fi, _ := os.Stat(s.path(journalFile)); fi.Mode().Perm() != 0o640 {
		t.Errorf("journal mode %v", fi.Mode())
	}
	if _, err := os.Stat(s.path(progressFile) + ".tmp"); err == nil {
		t.Error("a .tmp file is left")
	}
}

func TestCompact(t *testing.T) {
	s, _ := newFileStore(t, &fakeR2{})
	ctx := context.Background()
	old := testNow.Add(-pruneAfter - time.Hour)
	day1, day2 := testNow.Add(-48*time.Hour), testNow.Add(-24*time.Hour)
	s.Save(ctx, []record{
		rec("10.0.2.0", "10.0.2.255", "US", "OR", "Portland"),
		rec("10.0.1.0", "10.0.1.255", "US", "OR", "Portland"),
		rec("10.0.9.0", "10.0.9.255", "US", "TX", "Foo Town"),
	}, 100, day1, day1)
	s.Save(ctx, []record{rec("10.0.1.0", "10.0.1.255", "US", "MO", "Springfield")}, 200, day1, day2) // the newer answer of a range
	// A torn last line after a crash is skipped.
	f, _ := os.OpenFile(s.path(journalFile), os.O_APPEND|os.O_WRONLY, 0o640)
	f.WriteString("10.0.3.0,10.0.3.255,US,\"OR,Port")
	f.Close()
	// An old record in the base of an earlier compaction.
	writeBase(s.path(recordsFile), position{50, day1}, []record{recAt("10.0.5.0", "10.0.5.255", "OR", "Old Town", old), recAt("10.0.0.0", "10.0.0.255", "WA", "Seattle", day1)})

	recs, pruned, err := s.Compact(ctx, testNow)
	if err != nil || pruned != 1 || len(recs) != 4 {
		t.Fatalf("compact: %d records, %d pruned, %v", len(recs), pruned, err)
	}
	want := fmt.Sprintf("#position 200 %s\n", day1.UTC().Format(time.RFC3339)) +
		"net_start,net_end,country,state,city,looked_up\n" +
		fmt.Sprintf("10.0.0.0,10.0.0.255,US,WA,Seattle,%d\n", day1.Unix()) +
		fmt.Sprintf("10.0.1.0,10.0.1.255,US,MO,Springfield,%d\n", day2.Unix()) +
		fmt.Sprintf("10.0.2.0,10.0.2.255,US,OR,Portland,%d\n", day1.Unix()) +
		fmt.Sprintf("10.0.9.0,10.0.9.255,US,TX,Foo Town,%d\n", day1.Unix())
	got := readBaseText(t, s.path(recordsFile))
	if got != want {
		t.Errorf("base\n%s\nwant\n%s", got, want)
	}
	if _, err := os.Stat(s.path(journalFile)); err == nil {
		t.Error("the journal was not removed")
	}
	first, _ := os.ReadFile(s.path(recordsFile))
	// A second compaction changes nothing.
	recs, pruned, err = s.Compact(ctx, testNow)
	second, _ := os.ReadFile(s.path(recordsFile))
	if err != nil || pruned != 0 || len(recs) != 4 || !bytes.Equal(first, second) {
		t.Errorf("second compact: %d %d %v same=%v", len(recs), pruned, err, bytes.Equal(first, second))
	}
	// The same journal applied again (a crash before its removal) gives the same base.
	s.Save(ctx, []record{rec("10.0.2.0", "10.0.2.255", "US", "OR", "Portland")}, 200, day1, day1)
	if _, _, err = s.Compact(ctx, testNow); err != nil {
		t.Fatal(err)
	}
	if third, _ := os.ReadFile(s.path(recordsFile)); !bytes.Equal(first, third) {
		t.Error("an old row applied again changed the base")
	}
}

func TestCompactWithoutRecords(t *testing.T) {
	s, _ := newFileStore(t, &fakeR2{})
	// Compact never makes a base: only a lookup does, after asking R2.
	if _, _, err := s.Compact(context.Background(), testNow); !errors.Is(err, errNoBase) {
		t.Fatalf("no base: %v", err)
	}
	if _, err := os.Stat(s.path(recordsFile)); err == nil {
		t.Fatal("Compact made a base")
	}
	start(t, s)
	recs, pruned, err := s.Compact(context.Background(), testNow)
	if err != nil || len(recs) != 0 || pruned != 0 {
		t.Fatalf("%v %d %v", recs, pruned, err)
	}
	if got := readBaseText(t, s.path(recordsFile)); got != fmt.Sprintf("#position 0 %s\nnet_start,net_end,country,state,city,looked_up\n", testNow.UTC().Format(time.RFC3339)) {
		t.Errorf("empty base %q", got)
	}
}

func TestCompactDamagedBase(t *testing.T) {
	r2 := &fakeR2{}
	s, _ := newFileStore(t, r2)
	ctx := context.Background()
	start(t, s)
	s.Save(ctx, []record{rec("10.0.1.0", "10.0.1.255", "US", "OR", "Portland")}, 10, testNow, testNow)
	if _, _, err := s.Compact(ctx, testNow); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(s.path(recordsFile))
	r2.objects = map[string][]byte{RecordsKey: good}
	s.Save(ctx, []record{rec("10.0.2.0", "10.0.2.255", "US", "OR", "Portland")}, 20, testNow, testNow)

	for name, bad := range map[string][]byte{
		"truncated": good[:len(good)-6],
		"not gzip":  []byte("hello"),
		"bad row":   gz(t, "#position 1 "+testNow.Format(time.RFC3339)+"\nnet_start,net_end,country,state,city,looked_up\nnot,a,row\n"),
		"no header": gz(t, "#position 1 "+testNow.Format(time.RFC3339)+"\n10.0.0.0,10.0.0.255,US,OR,X,1\n"),
	} {
		os.WriteFile(s.path(recordsFile), bad, 0o640)
		os.Remove(s.path(damagedFile))
		_, _, err := s.Compact(ctx, testNow)
		if !errors.Is(err, errDamaged) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(s.path(recordsFile)); err == nil {
			t.Errorf("%s: the damaged base stays", name)
		}
		if _, err := os.Stat(s.path(damagedFile)); err != nil {
			t.Errorf("%s: the damaged base was not moved aside", name)
		}
		if _, err := os.Stat(s.path(journalFile)); err != nil {
			t.Errorf("%s: the journal is gone", name)
		}
		// Compact again before a restore (a retry of the task): it must not write a base from the journal alone.
		if _, _, err := s.Compact(ctx, testNow); !errors.Is(err, errNoBase) {
			t.Errorf("%s: second compact %v", name, err)
		}
		if _, err := os.Stat(s.path(recordsFile)); err == nil {
			t.Errorf("%s: a base was written from the journal alone", name)
		}
		// The next lookup restores it from R2, then compaction works and keeps the journal's rows.
		if _, _, restored, err := s.Progress(ctx, testNow); err != nil || !restored {
			t.Fatalf("%s: restore %v %v", name, restored, err)
		}
		if _, err := os.Stat(s.path(damagedFile)); err == nil {
			t.Errorf("%s: the damaged file stays after the restore", name)
		}
		recs, _, err := s.Compact(ctx, testNow)
		if err != nil || len(recs) != 2 {
			t.Errorf("%s: after the restore %d records, %v", name, len(recs), err)
		}
		s.Save(ctx, []record{rec("10.0.2.0", "10.0.2.255", "US", "OR", "Portland")}, 20, testNow, testNow)
	}
}

func gz(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write([]byte(text))
	w.Close()
	return buf.Bytes()
}

func TestProgressWithBase(t *testing.T) {
	s, _ := newFileStore(t, nil) // R2 is not needed while the base is there
	ctx := context.Background()
	started := testNow.Add(-72 * time.Hour)
	os.MkdirAll(s.dir, 0o750)
	if err := writeBase(s.path(recordsFile), position{50, started}, nil); err != nil {
		t.Fatal(err)
	}
	// No progress.json: the position of the base's first line.
	if c, st, restored, err := s.Progress(ctx, testNow); err != nil || c != 50 || !st.Equal(started) || restored {
		t.Errorf("no progress.json: %d %v %v %v", c, st, restored, err)
	}
	// progress.json wins.
	s.writeProgress(position{80, started.Add(time.Hour)})
	if c, st, _, err := s.Progress(ctx, testNow); err != nil || c != 80 || !st.Equal(started.Add(time.Hour)) {
		t.Errorf("progress.json: %d %v %v", c, st, err)
	}
	// An unreadable progress.json falls back on the base.
	os.WriteFile(s.path(progressFile), []byte("{"), 0o640)
	if c, _, _, err := s.Progress(ctx, testNow); err != nil || c != 50 {
		t.Errorf("bad progress.json: %d %v", c, err)
	}
}

func TestProgressRestore(t *testing.T) {
	started := testNow.Add(-72 * time.Hour)
	var src bytes.Buffer
	{
		w := gzip.NewWriter(&src)
		fmt.Fprintf(w, "#position 120 %s\nnet_start,net_end,country,state,city,looked_up\n10.0.0.0,10.0.0.255,US,OR,Portland,5\n", started.Format(time.RFC3339))
		w.Close()
	}
	r2 := &fakeR2{objects: map[string][]byte{RecordsKey: src.Bytes()}}
	s, logs := newFileStore(t, r2)
	ctx := context.Background()
	os.MkdirAll(s.dir, 0o750)
	os.WriteFile(s.path(journalFile), []byte("10.0.1.0,10.0.1.255,US,OR,Portland,6\n"), 0o640)
	s.writeProgress(position{999, testNow}) // a stale one: the object's first line is used
	c, st, restored, err := s.Progress(ctx, testNow)
	if err != nil || !restored || c != 120 || !st.Equal(started) {
		t.Fatalf("restore: %d %v %v %v", c, st, restored, err)
	}
	if data, _ := os.ReadFile(s.path(recordsFile)); !bytes.Equal(data, src.Bytes()) {
		t.Error("the base is not the object")
	}
	if p, ok := s.readProgress(); !ok || p.Cursor != 120 || !p.CycleStarted.Equal(started) {
		t.Errorf("progress.json %+v %v", p, ok)
	}
	if data, _ := os.ReadFile(s.path(journalFile)); !strings.Contains(string(data), "10.0.1.0") {
		t.Error("the journal was not kept")
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "restored") || !strings.Contains((*logs)[0], "120") || ipText.MatchString((*logs)[0]) {
		t.Errorf("logs %q", *logs)
	}
	// The base is there now: no second request.
	r2.stats = 0
	if _, _, restored, _ = s.Progress(ctx, testNow); restored || r2.stats != 0 {
		t.Error("restored twice")
	}

	// An object that is not a records file is not used.
	r2.objects[RecordsKey] = []byte("junk")
	os.Remove(s.path(recordsFile))
	if _, _, _, err := s.Progress(ctx, testNow); !errors.Is(err, errNoRecords) {
		t.Errorf("junk object: %v", err)
	}
	if _, err := os.Stat(s.path(recordsFile)); err == nil {
		t.Error("a junk base was written")
	}

	// An object with a good first line that is cut short is not used either.
	whole := src.Bytes()
	r2.objects[RecordsKey] = whole[:len(whole)-6]
	if _, _, _, err := s.Progress(ctx, testNow); !errors.Is(err, errNoRecords) {
		t.Errorf("cut object: %v", err)
	}
	if _, err := os.Stat(s.path(recordsFile)); err == nil {
		t.Error("a cut base was written")
	}
}

// The restore writes the position before the base: when the position cannot be written there is no
// base either, and the next run restores again (never a restored base under a newer position).
func TestRestoreWritesThePositionFirst(t *testing.T) {
	started := testNow.Add(-72 * time.Hour)
	src := gz(t, fmt.Sprintf("#position 120 %s\nnet_start,net_end,country,state,city,looked_up\n10.0.0.0,10.0.0.255,US,OR,Portland,%d\n", started.Format(time.RFC3339), testNow.Unix()))
	s, _ := newFileStore(t, &fakeR2{objects: map[string][]byte{RecordsKey: src}})
	ctx := context.Background()
	os.MkdirAll(s.path(progressFile), 0o750) // a directory in the way: progress.json cannot be written
	if _, _, _, err := s.Progress(ctx, testNow); err == nil {
		t.Fatal("the restore did not fail")
	}
	if _, err := os.Stat(s.path(recordsFile)); err == nil {
		t.Fatal("the base is in place although its position was not saved")
	}
	os.Remove(s.path(progressFile))
	c, st, restored, err := s.Progress(ctx, testNow)
	if err != nil || !restored || c != 120 || !st.Equal(started) {
		t.Fatalf("second try: %d %v %v %v", c, st, restored, err)
	}
	// The copy's record count is known now: a file that shrank is not copied over it.
	if b, ok := (&Pipeline{DataDir: filepath.Dir(s.dir)}).readBackup(); !ok || b.Records != 1 {
		t.Errorf("backup.json after the restore: %+v %v", b, ok)
	}
	if fi, err := os.Stat(s.path(recordsFile)); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("mode of the restored base: %v %v", fi, err)
	}
}

// A base that is there but cannot be opened is not a lost base: an error, and the file stays.
func TestUnopenableBaseIsLeftAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	s, _ := newFileStore(t, &fakeR2{})
	ctx := context.Background()
	start(t, s)
	s.Save(ctx, []record{rec("10.0.0.0", "10.0.0.255", "US", "OR", "Portland")}, 500, testNow, testNow)
	if _, _, err := s.Compact(ctx, testNow); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.path(recordsFile))
	os.Chmod(s.path(recordsFile), 0)
	defer os.Chmod(s.path(recordsFile), 0o640)
	if _, _, _, err := s.Progress(ctx, testNow); err == nil || errors.Is(err, errNoRecords) {
		t.Errorf("progress with an unopenable base: %v", err)
	}
	if _, _, err := s.Compact(ctx, testNow); err == nil || errors.Is(err, errDamaged) || errors.Is(err, errNoBase) {
		t.Errorf("compact with an unopenable base: %v", err)
	}
	os.Chmod(s.path(recordsFile), 0o640)
	if after, _ := os.ReadFile(s.path(recordsFile)); !bytes.Equal(before, after) {
		t.Error("the base was changed")
	}
	if _, err := os.Stat(s.path(damagedFile)); err == nil {
		t.Error("the base was moved aside as damaged")
	}
	if c, _, _, err := s.Progress(ctx, testNow); err != nil || c != 500 {
		t.Errorf("after the mode is back: %d %v", c, err)
	}
}

// A base that the disk cannot read (here: a directory in its place) is an error for the lookup and
// for the compaction, and nothing is moved, fetched or written.
func TestUnreadableBaseIsLeftAlone(t *testing.T) {
	r2 := &fakeR2{objects: map[string][]byte{RecordsKey: []byte("x")}}
	s, logs := newFileStore(t, r2)
	ctx := context.Background()
	os.MkdirAll(s.path(recordsFile), 0o750)
	if _, _, _, err := s.Progress(ctx, testNow); err == nil || errors.Is(err, errNoRecords) {
		t.Errorf("progress: %v", err)
	}
	if _, _, err := s.Compact(ctx, testNow); err == nil || errors.Is(err, errDamaged) || errors.Is(err, errNoBase) {
		t.Errorf("compact: %v", err)
	}
	if fi, err := os.Stat(s.path(recordsFile)); err != nil || !fi.IsDir() {
		t.Error("the unreadable base was moved or replaced")
	}
	if _, err := os.Stat(s.path(damagedFile)); err == nil {
		t.Error("it was moved aside as damaged")
	}
	if r2.stats != 0 || len(*logs) != 0 {
		t.Errorf("R2 asked %d times, logs %q", r2.stats, *logs)
	}
}

// A base whose content is not a records file is moved aside (never written over), then R2 decides.
func TestBadFirstLineIsMovedAside(t *testing.T) {
	good := gz(t, fmt.Sprintf("#position 120 %s\nnet_start,net_end,country,state,city,looked_up\n", testNow.Format(time.RFC3339)))
	r2 := &fakeR2{objects: map[string][]byte{RecordsKey: good}}
	s, logs := newFileStore(t, r2)
	ctx := context.Background()
	os.MkdirAll(s.dir, 0o750)
	os.WriteFile(s.path(recordsFile), gz(t, "something else\n"), 0o640)
	c, _, restored, err := s.Progress(ctx, testNow)
	if err != nil || !restored || c != 120 {
		t.Fatalf("restore after a bad first line: %d %v %v", c, restored, err)
	}
	if len(*logs) != 2 || !strings.Contains((*logs)[0], "WARNING") || !strings.Contains((*logs)[0], "moved aside") {
		t.Errorf("logs %q", *logs)
	}
	// With no copy in R2 the walk starts again and the file that was moved aside stays.
	s2, _ := newFileStore(t, &fakeR2{})
	os.MkdirAll(s2.dir, 0o750)
	bad := gz(t, "something else\n")
	os.WriteFile(s2.path(recordsFile), bad, 0o640)
	if c, _, _, err := s2.Progress(ctx, testNow); err != nil || c != 0 {
		t.Fatalf("no copy: %d %v", c, err)
	}
	if data, _ := os.ReadFile(s2.path(damagedFile)); !bytes.Equal(data, bad) {
		t.Error("the file moved aside is gone")
	}
}

// A restore that fails on the local disk fails the task; it is not "R2 cannot be read".
func TestRestoreFileErrorIsAnError(t *testing.T) {
	r2 := &fakeR2{objects: map[string][]byte{RecordsKey: []byte("x")}}
	r2.downloadErr = fmt.Errorf("download: %w", &os.PathError{Op: "write", Path: "x", Err: errors.New("no space left on device")})
	s, _ := newFileStore(t, r2)
	if _, _, _, err := s.Progress(context.Background(), testNow); err == nil || errors.Is(err, errNoRecords) {
		t.Errorf("a disk error during the restore: %v", err)
	}
}

// Without progress.json the base keeps the position it had: a base with records never goes back to 0.
func TestCompactKeepsThePositionOfTheBase(t *testing.T) {
	s, _ := newFileStore(t, &fakeR2{})
	ctx := context.Background()
	start(t, s)
	started := testNow.Add(-48 * time.Hour)
	if err := s.Save(ctx, []record{rec("10.0.0.0", "10.0.0.255", "US", "OR", "Portland")}, 500, started, testNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Compact(ctx, testNow); err != nil {
		t.Fatal(err)
	}
	os.Remove(s.path(progressFile))
	if _, _, err := s.Compact(ctx, testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if bp, ok := basePosition(s.path(recordsFile)); !ok || bp.Cursor != 500 || !bp.CycleStarted.Equal(started) {
		t.Errorf("position line %+v %v, want 500", bp, ok)
	}
	if c, st, _, err := s.Progress(ctx, testNow); err != nil || c != 500 || !st.Equal(started) {
		t.Errorf("progress %d %v %v", c, st, err)
	}
}

// A damaged base with no copy in R2: its records are gone, so the walk starts again in the same cycle.
func TestDamagedBaseWithoutACopy(t *testing.T) {
	r2 := &fakeR2{objects: map[string][]byte{}}
	s, logs := newFileStore(t, r2)
	ctx := context.Background()
	start(t, s)
	started := testNow.Add(-48 * time.Hour)
	if err := s.Save(ctx, []record{rec("10.0.0.0", "10.0.0.255", "US", "OR", "Portland")}, 500, started, testNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Compact(ctx, testNow); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.path(recordsFile), []byte("not gzip"), 0o640)
	if _, _, err := s.Compact(ctx, testNow); !errors.Is(err, errDamaged) {
		t.Fatalf("damaged base: %v", err)
	}
	c, st, restored, err := s.Progress(ctx, testNow)
	if err != nil || restored || c != 0 || !st.Equal(started) {
		t.Fatalf("progress %d %v %v %v, want 0 in the same cycle", c, st, restored, err)
	}
	if p, ok := s.readProgress(); !ok || p.Cursor != 0 {
		t.Errorf("progress.json %+v %v", p, ok)
	}
	if data, _ := os.ReadFile(s.path(damagedFile)); string(data) != "not gzip" {
		t.Errorf("the damaged file, the last copy of that data, is gone or changed: %q", data)
	}
	if got := readBaseText(t, s.path(recordsFile)); !strings.HasPrefix(got, "#position 0 ") || strings.Count(got, "\n") != 2 {
		t.Errorf("the new base %q", got)
	}
	if len(*logs) == 0 || !strings.Contains((*logs)[len(*logs)-1], "starts again") {
		t.Errorf("logs %q", *logs)
	}
	// The next compaction writes a base again.
	if _, _, err := s.Compact(ctx, testNow); err != nil {
		t.Errorf("compact after the restart: %v", err)
	}
}

// Half a row left by a crash must not swallow the first row of the next batch.
func TestSaveAfterTornRow(t *testing.T) {
	s, _ := newFileStore(t, &fakeR2{})
	ctx := context.Background()
	start(t, s)
	// The torn row ends inside a quoted city: a newline alone would not end it.
	os.WriteFile(s.path(journalFile), []byte(fmt.Sprintf("10.0.0.0,10.0.0.255,US,OR,Portland,%d\n10.0.1.0,10.0.1.255,US,DC,\"Washing", testNow.Unix())), 0o640)
	batch := []record{rec("10.0.2.0", "10.0.2.255", "US", "OR", "Portland"), rec("10.0.3.0", "10.0.3.255", "US", "OR", "Portland")}
	if err := s.Save(ctx, batch, 7, testNow, testNow); err != nil {
		t.Fatal(err)
	}
	recs, _, err := s.Compact(ctx, testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, addrOf(r.Start).String())
	}
	if want := "10.0.0.0 10.0.2.0 10.0.3.0"; strings.Join(got, " ") != want {
		t.Errorf("records %v, want %s", got, want)
	}
}

func TestProgressNoBase(t *testing.T) {
	ctx := context.Background()
	// No object and nothing saved: the very first start makes an empty base at position 0.
	s, logs := newFileStore(t, &fakeR2{})
	if c, st, restored, err := s.Progress(ctx, testNow); err != nil || c != 0 || !st.Equal(testNow) || restored {
		t.Errorf("first start: %d %v %v %v", c, st, restored, err)
	}
	if len(*logs) != 0 {
		t.Errorf("a first start logged %q", *logs)
	}
	// With the base there, the position is progress.json's.
	s.writeProgress(position{33, testNow.Add(-time.Hour)})
	if c, st, _, err := s.Progress(ctx, testNow); err != nil || c != 33 || !st.Equal(testNow.Add(-time.Hour)) {
		t.Errorf("progress.json: %d %v %v", c, st, err)
	}
	// The base deleted, a position saved, no copy in R2: the records are gone, the walk starts
	// again in the same cycle, with a WARNING.
	os.Remove(s.path(recordsFile))
	if c, st, _, err := s.Progress(ctx, testNow); err != nil || c != 0 || !st.Equal(testNow.Add(-time.Hour)) {
		t.Errorf("base deleted: %d %v %v", c, st, err)
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "WARNING") {
		t.Errorf("logs %q", *logs)
	}
	// Everything wiped but backup.json: a copy was sent once and R2 no longer has it. Also a WARNING.
	s2, logs2 := newFileStore(t, &fakeR2{})
	os.MkdirAll(s2.dir, 0o750)
	os.WriteFile(s2.path(backupFile), []byte(`{"uploaded_at":"2026-11-16T00:00:00Z","records":10}`), 0o640)
	if c, _, _, err := s2.Progress(ctx, testNow); err != nil || c != 0 {
		t.Errorf("copy lost: %d %v", c, err)
	}
	if len(*logs2) != 1 || !strings.Contains((*logs2)[0], "WARNING") {
		t.Errorf("logs %q", *logs2)
	}
	if _, err := os.Stat(s2.path(backupFile)); err == nil {
		t.Error("backup.json still describes a copy that is gone")
	}
	// R2 cannot be used: the sentinel, and nothing is written.
	for name, r2 := range map[string]*fakeR2{
		"stat error":     {statErr: errors.New("boom")},
		"download error": {objects: map[string][]byte{RecordsKey: []byte("x")}, downloadErr: errors.New("boom")},
		"no setting":     nil,
	} {
		s, _ := newFileStore(t, r2)
		if _, _, _, err := s.Progress(ctx, testNow); !errors.Is(err, errNoRecords) {
			t.Errorf("%s: %v", name, err)
		}
		if names := listDir(t, s.dir); len(names) != 0 {
			t.Errorf("%s: files written: %v", name, names)
		}
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// Lookup with the real file store: R2 down while the records file is gone means no lookup, no file, no request.
func TestLookupNeedsRecordsOrR2(t *testing.T) {
	srv := newRDAP(t, statusHandler)
	e := newEnv(t, srv, 8)
	e.p.db = nil
	e.p.objects = &fakeR2{statErr: errors.New("boom")}
	out, err := e.p.Lookup(context.Background())
	if err != nil || out.Skipped != skipNoRecords || len(srv.paths) != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	if names := listDir(t, e.p.dir()); strings.Join(names, " ") != lockFile {
		t.Errorf("files written: %v", names)
	}
	// The export and the upload of the same run must not make a base either, or an empty file
	// would be copied over the good one in R2.
	e.p.upload = func(context.Context, string, string, publish.PutOptions) (string, publish.PutStatus, error) {
		t.Error("something was uploaded")
		return "", "", nil
	}
	ex, err := e.p.Export(context.Background(), false)
	if err != nil || ex.Skipped != skipNoBase {
		t.Errorf("export: %+v %v", ex, err)
	}
	up, err := e.p.Upload(context.Background(), false)
	if err != nil || up.Backup != backupNoFile {
		t.Errorf("upload: %+v %v", up, err)
	}
	if names := listDir(t, e.p.dir()); strings.Join(names, " ") != lockFile {
		t.Errorf("files written by export or upload: %v", names)
	}
	e.noAddress(t, out)
}

// A task that finds the directory in use (run-arin beside the worker) does nothing.
func TestTasksSkipWhenBusy(t *testing.T) {
	srv := newRDAP(t, statusHandler)
	e := newEnv(t, srv, 8)
	unlock, ok, err := e.p.lock()
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	ctx := context.Background()
	if out, err := e.p.Lookup(ctx); err != nil || out.Skipped != skipBusy || len(srv.paths) != 0 {
		t.Errorf("lookup: %+v %v", out, err)
	}
	if out, err := e.p.Export(ctx, true); err != nil || out.Skipped != skipBusy {
		t.Errorf("export: %+v %v", out, err)
	}
	if out, err := e.p.Upload(ctx, true); err != nil || out.Skipped != skipBusy {
		t.Errorf("upload: %+v %v", out, err)
	}
	unlock()
	if out, err := e.p.Lookup(ctx); err != nil || out.Skipped == skipBusy {
		t.Errorf("lookup after the lock is free: %+v %v", out, err)
	}
}

// A whole round: lookups, export, upload, then the data is wiped and the next lookup restores
// the records and goes on from the saved position without asking the stored /24s again.
func TestFullRound(t *testing.T) {
	srv := newRDAP(t, statusHandler)
	e := newEnv(t, srv, 8)
	e.p.db = nil
	r2 := &fakeR2{objects: map[string][]byte{}}
	e.p.objects = r2
	puts := map[string][]byte{}
	e.p.upload = func(_ context.Context, key, path string, _ publish.PutOptions) (string, publish.PutStatus, error) {
		data, _ := os.ReadFile(path)
		puts[key] = data
		r2.objects[key] = data
		return "", publish.PutUploaded, nil
	}
	geo := newGeo(t, zipOf(t, append([]string{city(20, "Springfield", "Springfield", "", "US", "IL", 100000)}, testCities[2:]...)), 200)
	e.p.geoURL = geo.URL
	e.p.dlClient = geo.Client()

	// One run walks all 8 targets, in batches of four.
	e.p.batch = 4
	ctx := context.Background()
	out, err := e.p.Lookup(ctx)
	if err != nil || out.Batches == 0 || out.Restored {
		t.Fatalf("lookup: %+v %v", out, err)
	}
	if _, err := e.p.Export(ctx, true); err != nil {
		t.Fatal(err)
	}
	up, err := e.p.Upload(ctx, false)
	if err != nil || up.Backup != "uploaded" || puts[RecordsKey] == nil {
		t.Fatalf("upload: %+v %v", up, err)
	}
	asked := len(srv.paths)
	progress, _ := os.ReadFile(filepath.Join(e.p.dir(), progressFile))
	var saved position
	json.Unmarshal(progress, &saved)

	// Wipe everything on the VM.
	os.RemoveAll(e.p.dir())
	// The walk is done (cursor at the end); make it go on instead: the object's position says so.
	e.p.size = 512 // a longer walk: positions 256 to 511 are new, nothing in the gap there
	out, err = e.p.Lookup(ctx)
	if err != nil || !out.Restored {
		t.Fatalf("after the wipe: %+v %v", out, err)
	}
	if len(srv.paths) != asked {
		t.Errorf("the stored /24s were asked again: %d requests more", len(srv.paths)-asked)
	}
	if saved.Cursor != 256 || out.Steps != 256 || out.Targets != 0 {
		t.Errorf("saved position %d; after the restore %d positions, %d targets", saved.Cursor, out.Steps, out.Targets)
	}
	if p, ok := (&fileStore{dir: e.p.dir()}).readProgress(); !ok || p.Cursor != 512 {
		t.Errorf("position after the restore: %+v", p)
	}
	e.noAddress(t, out)
}

func TestUploadBackup(t *testing.T) {
	var keys []string
	var opts []publish.PutOptions
	status := publish.PutUploaded
	p := &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return testNow }}
	p.upload = func(_ context.Context, key, _ string, o publish.PutOptions) (string, publish.PutStatus, error) {
		keys, opts = append(keys, key), append(opts, o)
		return "", status, nil
	}
	ctx := context.Background()
	var logs []string
	p.Progress = func(s string) { logs = append(logs, s) }

	// No records file.
	if out, err := p.Upload(ctx, false); err != nil || out.Backup != "no records file" || len(keys) != 0 {
		t.Errorf("no file: %+v %v", out, err)
	}
	// A base of 10 records: copied.
	s := &fileStore{dir: p.dir()}
	mk := func(n int) {
		var recs []record
		for i := 0; i < n; i++ {
			a := addrOf(0x0A000000 + uint32(i)<<8)
			recs = append(recs, record{Start: u32(a), End: u32(a) | 255, Country: "US", State: "OR", City: "Portland", LookedUp: testNow.Unix()})
		}
		os.MkdirAll(p.dir(), 0o750)
		if err := writeBase(s.path(recordsFile), position{1, testNow}, recs); err != nil {
			t.Fatal(err)
		}
	}
	mk(10)
	out, err := p.Upload(ctx, false)
	if err != nil || out.Backup != "uploaded" || out.BackupRecords != 10 || keys[0] != RecordsKey || opts[0].Force {
		t.Fatalf("first copy: %+v %v %v", out, err, keys)
	}
	if b, ok := p.readBackup(); !ok || b.Records != 10 || !b.UploadedAt.Equal(testNow) {
		t.Errorf("backup.json %+v %v", b, ok)
	}
	// Within 24 hours: nothing.
	n := len(keys)
	p.now = func() time.Time { return testNow.Add(23 * time.Hour) }
	if out, err = p.Upload(ctx, false); err != nil || out.Backup != "recent" || len(keys) != n {
		t.Errorf("recent: %+v %v", out, err)
	}
	// After 24 hours: sent; R2 already holds the bytes: unchanged.
	p.now = func() time.Time { return testNow.Add(24 * time.Hour) }
	status = publish.PutUnchanged
	if out, err = p.Upload(ctx, false); err != nil || out.Backup != "unchanged" || len(keys) != n+1 {
		t.Errorf("unchanged: %+v %v", out, err)
	}
	// Less than half the records of the last copy: refused, with a warning that has both counts.
	mk(4)
	p.now = func() time.Time { return testNow.Add(49 * time.Hour) }
	n = len(keys)
	logs = nil
	if out, err = p.Upload(ctx, false); err != nil || out.Backup != "refused" || len(keys) != n {
		t.Errorf("refused: %+v %v", out, err)
	}
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "WARNING") || !strings.Contains(logs[len(logs)-1], "4 records") || !strings.Contains(logs[len(logs)-1], "10") {
		t.Errorf("logs %q", logs)
	}
	// Half is fine (5 of 10 is not less than half).
	mk(5)
	if out, err = p.Upload(ctx, false); err != nil || out.Backup != "unchanged" {
		t.Errorf("half: %+v %v", out, err)
	}
	// force sends it although the last copy is young.
	mk(6)
	status = publish.PutUploaded
	if out, err = p.Upload(ctx, true); err != nil || out.Backup != "uploaded" || out.BackupRecords != 6 || !opts[len(opts)-1].Force {
		t.Errorf("force: %+v %v", out, err)
	}
	// force never sends a file that shrank to less than half.
	mk(1)
	n = len(keys)
	if out, err = p.Upload(ctx, true); err != nil || out.Backup != "refused" || len(keys) != n {
		t.Errorf("force with a shrunken file: %+v %v", out, err)
	}
	// A failed copy fails the task and does not write backup.json.
	mk(7)
	before, _ := p.readBackup()
	p.now = func() time.Time { return testNow.Add(100 * time.Hour) }
	p.upload = func(context.Context, string, string, publish.PutOptions) (string, publish.PutStatus, error) {
		return "", "", errors.New("boom")
	}
	if _, err = p.Upload(ctx, false); err == nil {
		t.Error("a failed copy did not fail the task")
	}
	if b, _ := p.readBackup(); b != before {
		t.Errorf("backup.json written after a failed copy: %+v", b)
	}
	// A base that cannot be read is never copied.
	os.WriteFile(s.path(recordsFile), []byte("junk"), 0o640)
	p.upload = func(context.Context, string, string, publish.PutOptions) (string, publish.PutStatus, error) {
		t.Error("a damaged base was copied")
		return "", publish.PutUploaded, nil
	}
	if out, err = p.Upload(ctx, true); err != nil || out.Backup != "refused" {
		t.Errorf("damaged: %+v %v", out, err)
	}
}
