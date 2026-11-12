package rdns

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocateStateCanSkip(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	s := locateState{Fingerprint: "fp", RebuiltAt: now.Add(-time.Hour)}
	for name, c := range map[string]struct {
		state       locateState
		fingerprint string
		now         time.Time
		want        bool
	}{
		"same, recent":        {s, "fp", now, true},
		"changed":             {s, "other", now, false},
		"just under a day":    {locateState{"fp", now.Add(-locateMaxAge + time.Second)}, "fp", now, true},
		"a day old":           {locateState{"fp", now.Add(-locateMaxAge)}, "fp", now, false},
		"older":               {locateState{"fp", now.Add(-48 * time.Hour)}, "fp", now, false},
		"no state":            {locateState{}, "fp", now, false},
		"empty fingerprint":   {locateState{"", now}, "", now, false},
		"clock moved back":    {s, "fp", now.Add(-2 * time.Hour), false},
		"zero rebuild time":   {locateState{Fingerprint: "fp"}, "fp", now, false},
		"same, a minute ago":  {locateState{"fp", now.Add(-time.Minute)}, "fp", now, true},
		"changed and a day":   {locateState{"fp", now.Add(-25 * time.Hour)}, "x", now, false},
		"same and exactly 0s": {locateState{"fp", now}, "fp", now, true},
	} {
		if got := c.state.canSkip(c.fingerprint, c.now); got != c.want {
			t.Errorf("%s: canSkip = %v, want %v", name, got, c.want)
		}
	}
}

// The state file is written through a temporary file; a missing or damaged one is no state.
func TestLocateStateFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rdns") // not there yet
	path := filepath.Join(dir, locateStateFile)
	if s := readLocateState(path); s != (locateState{}) {
		t.Errorf("missing file: %+v", s)
	}
	want := locateState{Fingerprint: `["1:x","2:y"]`, RebuiltAt: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	if err := writeLocateState(path, want); err != nil {
		t.Fatal(err)
	}
	if got := readLocateState(path); got != want {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
	os.WriteFile(path, []byte("{not json"), 0o640)
	if s := readLocateState(path); s != (locateState{}) {
		t.Errorf("damaged file: %+v", s)
	}
}
