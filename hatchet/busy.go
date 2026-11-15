package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

// busyMarker is the file <data dir>/worker-busy. It exists exactly while at least one task
// runs, so the updater (infra/openipdata-worker-update) can wait for an idle worker.
// It holds the number of running tasks and nothing else. A failure to write or remove it only
// logs a warning: the marker must never fail a task.
type busyMarker struct {
	path string
	mu   sync.Mutex
	n    int
}

func newBusyMarker(dataDir string) *busyMarker {
	return &busyMarker{path: filepath.Join(dataDir, "worker-busy")}
}

// reset removes a leftover of a killed process; call it at start and on shutdown.
func (b *busyMarker) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n = 0
	b.remove()
}

// enter counts one more running task and returns the function that counts it out
// (call it with defer, so a panic or an error also counts out).
func (b *busyMarker) enter() (leave func()) {
	b.mu.Lock()
	b.n++
	b.write()
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.n > 0 {
				b.n--
			}
			if b.n == 0 {
				b.remove()
			} else {
				b.write()
			}
		})
	}
}

// write must be called with the lock held. The count is replaced through a temp file, so a
// reader never sees a half-written number; the file exists from the first task on.
func (b *busyMarker) write() {
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(b.n)+"\n"), 0o644); err != nil {
		slog.Warn("cannot write the busy marker", "err", err)
		return
	}
	if err := os.Rename(tmp, b.path); err != nil {
		slog.Warn("cannot write the busy marker", "err", err)
		_ = os.Remove(tmp)
	}
}

// remove must be called with the lock held.
func (b *busyMarker) remove() {
	if err := os.Remove(b.path); err != nil && !os.IsNotExist(err) {
		slog.Warn("cannot remove the busy marker", "err", err)
	}
}

// middleware is the one Hatchet middleware that counts running tasks.
func (b *busyMarker) middleware(ctx hatchet.Context, next func(hatchet.Context) error) error {
	defer b.enter()()
	return next(ctx)
}
