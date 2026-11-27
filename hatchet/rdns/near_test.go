package rdns

import (
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func idxOf(p string) uint32 {
	b := netip.MustParsePrefix(p).Addr().As4()
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

// walkPos is the inverse of walkAt on every index.
func TestWalkPosIsTheInverse(t *testing.T) {
	for i := uint32(0); i < walkSize; i++ {
		if walkPos(walkAt(i)) != i {
			t.Fatalf("walkPos(walkAt(%d)) = %d", i, walkPos(walkAt(i)))
		}
	}
	if (walkMul*walkMulInv)&(walkSize-1) != 1 {
		t.Fatal("walkMulInv is not the inverse of walkMul")
	}
}

func TestDoneSetFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample-done.bits")
	start := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	state := sampleState{Cursor: 5, CycleStarted: start}

	// No file: the positions below the cursor are marked, nothing else.
	d := readDone(path, state)
	for i := uint32(0); i < 10; i++ {
		if d.has(walkAt(i)) != (i < 5) {
			t.Fatalf("initial marking wrong at position %d", i)
		}
	}
	count := 0
	for _, b := range d.bits {
		for ; b != 0; b &= b - 1 {
			count++
		}
	}
	if count != 5 {
		t.Fatalf("%d bits set, want 5", count)
	}

	// Round trip.
	d.set(0x0A0B0C)
	if err := d.write(path, start); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Size() != doneHeader+doneBytes || fi.Mode().Perm() != 0o640 {
		t.Errorf("file size %d mode %v", fi.Size(), fi.Mode())
	}
	state.Cursor = 0 // the file wins, not the cursor
	got := readDone(path, state)
	if !got.has(0x0A0B0C) || !got.has(walkAt(2)) || got.has(0x0A0B0D) {
		t.Error("round trip lost or invented bits")
	}

	// Another cycle: ignored, marked from the cursor (here 0: empty).
	other := sampleState{CycleStarted: start.Add(time.Hour)}
	if readDone(path, other).has(0x0A0B0C) {
		t.Error("a file of another cycle was used")
	}
	// Wrong length: ignored.
	data, _ := os.ReadFile(path)
	os.WriteFile(path, data[:len(data)-1], 0o640)
	if readDone(path, state).has(0x0A0B0C) {
		t.Error("a short file was used")
	}
	// The header is the big-endian cycle start.
	os.WriteFile(path, data, 0o640)
	if binary.BigEndian.Uint64(data) != uint64(start.UnixNano()) {
		t.Error("header is not the cycle start")
	}
}

func TestNearPick(t *testing.T) {
	g := parseGaps([]byte("20.1.0.0/24 6 FR\n20.1.9.0/24 2 DE\n20.2.0.0/24 3 FR\n10.1.0.0/24 4 DE\n"))
	// Named: 20.1.3.0 (inside a gap run: it is a refresh, never counted twice), 20.1.200.0 (not in the
	// gap) and 20.1.250.0 (done). 10.1.x is not public, so its block gives nothing.
	named := map[uint32]string{
		idxOf("20.1.3.0/24"):   "FR",
		idxOf("20.1.200.0/24"): "FR",
		idxOf("20.1.250.0/24"): "FR",
		idxOf("10.1.1.0/24"):   "DE",
	}
	done := newDoneSet()
	done.set(idxOf("20.1.250.0/24"))
	done.set(idxOf("20.1.1.0/24")) // a gap /24, done
	intn := func(int) int { return 4 }

	all, cands, blocks := nearPick(g, named, done, 1000, intn)
	if blocks != 2 {
		t.Errorf("blocks = %d", blocks)
	}
	// Refresh: 20.1.3 and 20.1.200 (10.1.1 not public). Gap in block 20.1: 20.1.0, 2, 4, 5, 9, 10 (not 20.1.1
	// done, not 20.1.3 named). 20.2.x is another block with no named row: not looked at.
	if cands != 8 || len(all) != 8 {
		t.Fatalf("candidates %d, picked %d", cands, len(all))
	}
	for k, s := range all {
		idx := idxOf(s.Prefix.String())
		if !s.Near {
			t.Errorf("%s not marked Near", s.Prefix)
		}
		if (k < 2) != (named[idx] != "") {
			t.Errorf("%s in the wrong group", s.Prefix)
		}
		if k != 0 && k != 2 && walkPos(idx) <= walkPos(idxOf(all[k-1].Prefix.String())) {
			t.Errorf("%s not in walk order", s.Prefix)
		}
		for _, h := range s.Hosts {
			if !s.Prefix.Contains(h) {
				t.Errorf("host %s outside %s", h, s.Prefix)
			}
		}
		switch idx {
		case idxOf("20.1.3.0/24"), idxOf("20.1.200.0/24"):
			if s.Country != "FR" {
				t.Errorf("%s country %q", s.Prefix, s.Country)
			}
		case idxOf("20.1.9.0/24"), idxOf("20.1.10.0/24"):
			if s.Country != "DE" {
				t.Errorf("%s country %q", s.Prefix, s.Country)
			}
		}
		if idx == idxOf("20.1.250.0/24") || idx == idxOf("20.1.1.0/24") || idx == idxOf("10.1.1.0/24") || idx>>8 == idxOf("20.2.0.0/24")>>8 {
			t.Errorf("%s must not be picked", s.Prefix)
		}
	}
	// The cut keeps the first of the order: refreshes before gap /24s.
	cut, cands2, _ := nearPick(g, named, done, 3, intn)
	if len(cut) != 3 || cands2 != 8 {
		t.Fatalf("cut: %d picked, %d candidates", len(cut), cands2)
	}
	for k := range cut {
		if cut[k].Prefix != all[k].Prefix {
			t.Errorf("cut differs at %d", k)
		}
	}
	if none, c, b := nearPick(g, map[uint32]string{}, done, 10, intn); len(none) != 0 || c != 0 || b != 0 {
		t.Errorf("no named rows: %d %d %d", len(none), c, b)
	}
}

// Every /16 block at once must stay fast and not allocate per index.
func TestNearPickManyBlocks(t *testing.T) {
	g := parseGaps([]byte("20.0.0.0/24 65536 FR\n"))
	named := map[uint32]string{}
	for b := uint32(20 << 8); b < 21<<8; b++ {
		named[b<<8] = "FR"
	}
	picked, cands, blocks := nearPick(g, named, newDoneSet(), nearNetworks, func(int) int { return 0 })
	if len(picked) != nearNetworks || blocks != 256 || cands != 256+(1<<16-256) {
		t.Errorf("picked %d, candidates %d, blocks %d", len(picked), cands, blocks)
	}
}
