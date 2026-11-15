package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestBusyMarkerCountsTasks(t *testing.T) {
	dir := t.TempDir()
	b := newBusyMarker(dir)
	p := filepath.Join(dir, "worker-busy")
	if exists(p) {
		t.Fatal("marker before any task")
	}
	l1 := b.enter()
	if !exists(p) {
		t.Fatal("no marker with one task")
	}
	l2 := b.enter()
	l1()
	if !exists(p) {
		t.Fatal("marker gone with one task still running")
	}
	l2()
	if exists(p) {
		t.Fatal("marker left with no task")
	}
	l2() // a second leave must not go below zero
	l3 := b.enter()
	if !exists(p) {
		t.Fatal("no marker after a double leave")
	}
	l3()
	if exists(p) {
		t.Fatal("marker left")
	}
}

func TestBusyMarkerConcurrent(t *testing.T) {
	dir := t.TempDir()
	b := newBusyMarker(dir)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer b.enter()()
		}()
	}
	wg.Wait()
	if exists(filepath.Join(dir, "worker-busy")) {
		t.Fatal("marker left after all tasks")
	}
	if b.n != 0 {
		t.Fatalf("count %d", b.n)
	}
}

func TestBusyMarkerMiddlewareErrorAndPanic(t *testing.T) {
	dir := t.TempDir()
	b := newBusyMarker(dir)
	p := filepath.Join(dir, "worker-busy")
	want := errors.New("boom")
	err := b.middleware(nil, func(_ hatchet.Context) error {
		if !exists(p) {
			t.Error("no marker while the task runs")
		}
		return want
	})
	if err != want {
		t.Fatalf("error not passed on: %v", err)
	}
	if exists(p) {
		t.Fatal("marker left after an error")
	}
	func() {
		defer func() { _ = recover() }()
		_ = b.middleware(nil, func(_ hatchet.Context) error { panic("x") })
	}()
	if exists(p) {
		t.Fatal("marker left after a panic")
	}
}

func TestBusyMarkerResetRemovesLeftover(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "worker-busy")
	if err := os.WriteFile(p, []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := newBusyMarker(dir)
	b.reset()
	if exists(p) {
		t.Fatal("leftover not removed")
	}
	b.reset() // no file: no problem
}

func TestBusyMarkerWriteFailureDoesNotPanic(t *testing.T) {
	b := newBusyMarker(filepath.Join(t.TempDir(), "missing", "dir"))
	leave := b.enter()
	leave()
	b.reset()
}
