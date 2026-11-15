package rdns

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The decoder tries the legacy rules first, then the candidate rules, as locate does for a router name.
func TestDecoderDecode(t *testing.T) {
	rules, cands := namedRules(t)
	d := &Decoder{rules: rules, cityRules: cands}
	for _, c := range []struct {
		name, country, city string
		ok                  bool
	}{
		{"x.fra02.isp.net", "DE", "Frankfurt", true}, // legacy rule
		{"h1.nyc.cable.net", "US", "New York", true}, // candidate rule
		{"x.muc01.isp.net", "DE", "Munich", true},    // legacy
		{"dyn-1.cable.net", "", "", false},           // no token
		{"", "", "", false},
	} {
		country, city, ok := d.Decode(c.name)
		if ok != c.ok || country != c.country || city != c.city {
			t.Errorf("Decode(%q) = %q %q %v", c.name, country, city, ok)
		}
	}
	var none *Decoder
	if _, _, ok := none.Decode("x.fra02.isp.net"); ok {
		t.Error("a nil decoder decoded")
	}
	if legacy, domains := d.Counts(); legacy == 0 || domains != 1 {
		t.Errorf("counts %d %d", legacy, domains)
	}
	if _, err := LoadDecoder(context.Background(), ""); err == nil {
		t.Error("no PGSQL accepted")
	}
}

// MMDBCopy keeps the copy in its own directory, never in the rdns one, with the ETag rule of locate.
func TestMMDBCopyUsesItsOwnDir(t *testing.T) {
	store := newFakeStore()
	store.put(yesterdayKey, "e1", "mmdb bytes")
	dir := filepath.Join(t.TempDir(), "mlab")
	c := &MMDBCopy{Dir: dir, R2: "unused", Store: store, Now: func() time.Time { return testNow }}
	st := c.Prepare(context.Background(), "test")
	if !st.Usable() || st.Status != "used" || st.Path != filepath.Join(dir, "ip2geo.mmdb") || !st.Downloaded || st.ETag != "e1" {
		t.Fatalf("state %+v", st)
	}
	if data, _ := os.ReadFile(st.Path); string(data) != "mmdb bytes" {
		t.Errorf("copy = %q", data)
	}
	// The same ETag: not downloaded again.
	if st = c.Prepare(context.Background(), "test"); st.Downloaded || !st.Usable() || len(store.downloads) != 1 {
		t.Errorf("second %+v downloads %v", st, store.downloads)
	}
	// R2 down: the saved copy is used, and a reason says why.
	store.statErr = errors.New("boom")
	if st = c.Prepare(context.Background(), "test"); st.Status != "saved copy" || !st.Usable() || st.Reason == "" {
		t.Errorf("outage %+v", st)
	}
	// Dropped: nothing to use.
	c.Drop()
	if st = c.Prepare(context.Background(), "test"); st.Usable() || st.Status != "skipped" || st.Reason == "" {
		t.Errorf("dropped %+v", st)
	}
	// No R2 setting at all.
	if st = (&MMDBCopy{Dir: t.TempDir()}).Prepare(context.Background(), "test"); st.Usable() || st.Reason != reasonNoR2 {
		t.Errorf("no R2 %+v", st)
	}
}
