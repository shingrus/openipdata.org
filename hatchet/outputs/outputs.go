// Package outputs holds the small helpers the workflows share for their
// output directories and the mmdbbuilder output.
package outputs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
