// Package outputs holds the small helpers the workflows share for their
// output directories, the mmdbbuilder output and the progress lines of long tasks.
package outputs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProgressEvery is how often a long loop logs a progress line.
const ProgressEvery = time.Minute

// Pace spaces the progress lines of a long loop: Due is true at most once per Every, the first
// time Every after its first call, so a loop that ends sooner logs nothing extra.
type Pace struct {
	Every time.Duration
	next  time.Time
}

// Due reports whether a progress line is due now.
func (p *Pace) Due() bool {
	now := time.Now()
	if p.next.IsZero() {
		p.next = now.Add(p.Every)
		return false
	}
	if now.Before(p.next) {
		return false
	}
	p.next = now.Add(p.Every)
	return true
}

// Prune keeps the newest keep dated directories under root (names sort by
// date) and removes the rest.
func Prune(root string, keep int) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	for len(dirs) > keep {
		os.RemoveAll(filepath.Join(root, dirs[0]))
		dirs = dirs[1:]
	}
}

// LastLines returns the last n lines of text.
func LastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// FileSize returns the size of a file, or 0 if it cannot be read.
func FileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
