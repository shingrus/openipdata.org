package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func startTail(t *testing.T, path string) (*Tailer, chan string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tl := &Tailer{Path: path, Poll: 5 * time.Millisecond, Grace: 300 * time.Millisecond}
	out := make(chan string, 100)
	done := make(chan struct{})
	go func() { tl.Run(ctx, out); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(50 * time.Millisecond) // let it open the file and seek to the end
	return tl, out
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func want(t *testing.T, out chan string, lines ...string) {
	t.Helper()
	for _, w := range lines {
		select {
		case got := <-out:
			if got != w {
				t.Fatalf("got %q, want %q", got, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for %q", w)
		}
	}
}

func none(t *testing.T, out chan string) {
	t.Helper()
	select {
	case got := <-out:
		t.Fatalf("unexpected line %q", got)
	case <-time.After(60 * time.Millisecond):
	}
}

func TestTailAppendAndPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendTo(t, path, "old line\n")
	_, out := startTail(t, path)
	none(t, out) // no replay
	appendTo(t, path, "one\ntwo\npart")
	want(t, out, "one", "two")
	none(t, out)
	appendTo(t, path, "ial\n")
	want(t, out, "partial")
}

func TestTailLongLineSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendTo(t, path, "")
	_, out := startTail(t, path)
	long := make([]byte, maxLine+10)
	for i := range long {
		long[i] = 'a'
	}
	appendTo(t, path, string(long)+"\nnext\n")
	want(t, out, "next")
}

func TestTailRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	appendTo(t, path, "")
	tl, out := startTail(t, path)
	appendTo(t, path, "a\n")
	want(t, out, "a")
	// Written to the old file, then renamed and a new file created, before the next poll.
	appendTo(t, path, "b\n")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, "c\n")
	want(t, out, "b", "c")
	appendTo(t, path, "d\n")
	want(t, out, "d")
	if tl.Rotations.Load() != 1 {
		t.Errorf("rotations = %d", tl.Rotations.Load())
	}
}

func TestTailTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendTo(t, path, "")
	tl, out := startTail(t, path)
	appendTo(t, path, "first line here\n")
	want(t, out, "first line here")
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	appendTo(t, path, "x\n")
	want(t, out, "x")
	if tl.Rotations.Load() != 1 {
		t.Errorf("rotations = %d", tl.Rotations.Load())
	}
}

func TestTailMissingThenCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	_, out := startTail(t, path)
	none(t, out)
	appendTo(t, path, "hello\n")
	want(t, out, "hello") // a file created later is read from its start
}

func TestTailRotationGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendTo(t, path, "")
	_, out := startTail(t, path)
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the path is missing for a while
	appendTo(t, path, "new\n")
	want(t, out, "new")
}

func TestTailGraceBothFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendTo(t, path, "")
	_, out := startTail(t, path)
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, "new1\n")
	want(t, out, "new1")
	// nginx has not reopened yet: it still writes to the renamed file.
	appendTo(t, path+".1", "oldA\n")
	appendTo(t, path, "new2\n")
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case l := <-out:
			got[l] = true
		case <-time.After(2 * time.Second):
			t.Fatal("timeout")
		}
	}
	if !got["oldA"] || !got["new2"] {
		t.Fatalf("got %v", got)
	}
	time.Sleep(500 * time.Millisecond) // grace over: the old file is closed
	appendTo(t, path+".1", "late\n")
	none(t, out)
}
