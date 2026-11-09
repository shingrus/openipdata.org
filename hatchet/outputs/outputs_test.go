package outputs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrune(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"20261101", "20261102", "20261103", "20261104"} {
		os.Mkdir(filepath.Join(root, name), 0o750)
	}
	os.WriteFile(filepath.Join(root, "file"), nil, 0o640)
	Prune(root, 2)
	entries, _ := os.ReadDir(root)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != 3 || got[0] != "20261103" || got[1] != "20261104" || got[2] != "file" {
		t.Errorf("left %v", got)
	}
	Prune(filepath.Join(root, "missing"), 2) // no error, no panic
}

func TestLastLines(t *testing.T) {
	if got := LastLines("a\nb\nc\n", 2); got != "b\nc" {
		t.Errorf("got %q", got)
	}
	if got := LastLines("a", 5); got != "a" {
		t.Errorf("got %q", got)
	}
}

func TestFileSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("abc"), 0o640)
	if FileSize(path) != 3 || FileSize(path+"x") != 0 {
		t.Error("wrong size")
	}
}
