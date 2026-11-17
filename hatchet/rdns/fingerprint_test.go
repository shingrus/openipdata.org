package rdns

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocateStateCanSkip(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	st := func(fp, rules string, age time.Duration) locateState {
		return locateState{Fingerprint: fp, Rules: rules, RebuiltAt: now.Add(-age)}
	}
	for name, c := range map[string]struct {
		state       locateState
		fingerprint string
		rules       string
		now         time.Time
		want        bool
		reason      string
	}{
		"same, recent":             {st("fp", "r", time.Hour), "fp", "r", now, true, skipUnchanged},
		"same, just under a day":   {st("fp", "r", locateMaxAge-time.Second), "fp", "r", now, true, skipUnchanged},
		"same, a day old":          {st("fp", "r", locateMaxAge), "fp", "r", now, false, ""},
		"same, older":              {st("fp", "r", 48*time.Hour), "fp", "r", now, false, ""},
		"same, a minute ago":       {st("fp", "r", time.Minute), "fp", "r", now, true, skipUnchanged},
		"same and exactly 0s":      {st("fp", "r", 0), "fp", "r", now, true, skipUnchanged},
		"same, old state no rules": {st("fp", "", time.Hour), "fp", "r", now, true, skipUnchanged},
		"changed, same rules, 1h":  {st("fp", "r", time.Hour), "other", "r", now, true, skipRecent},
		"changed, just under 6h":   {st("fp", "r", locateMinInterval-time.Second), "other", "r", now, true, skipRecent},
		"changed, exactly 6h":      {st("fp", "r", locateMinInterval), "other", "r", now, false, ""},
		"changed, over 6h":         {st("fp", "r", 7*time.Hour), "other", "r", now, false, ""},
		"changed rules, 1h":        {st("fp", "r", time.Hour), "other", "r2", now, false, ""},
		"same fp, changed rules":   {st("fp", "r", 24*time.Hour+time.Minute), "fp", "r2", now, false, ""},
		"old state without rules":  {st("fp", "", time.Hour), "other", "r", now, false, ""},
		"no rules both":            {st("fp", "", time.Hour), "other", "", now, false, ""},
		"no state":                 {locateState{}, "fp", "r", now, false, ""},
		"empty fingerprint":        {locateState{Fingerprint: "", Rules: "r", RebuiltAt: now}, "", "r", now, false, ""},
		"clock moved back":         {st("fp", "r", time.Hour), "fp", "r", now.Add(-2 * time.Hour), false, ""},
		"future, other inputs":     {st("fp", "r", -time.Minute), "other", "r", now, false, ""},
		"zero rebuild time":        {locateState{Fingerprint: "fp", Rules: "r"}, "other", "r", now, false, ""},
	} {
		got, reason := c.state.canSkip(c.fingerprint, c.rules, c.now)
		if got != c.want || reason != c.reason {
			t.Errorf("%s: canSkip = %v %q, want %v %q", name, got, reason, c.want, c.reason)
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

// A rebuild that did not read the MMDB R2 offered saves no rules part, so the 6-hour skip does
// not apply and the next run rebuilds and tries R2 again.
func TestRebuiltState(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	good := rebuiltState("fp", "rules", now, true)
	if good.Rules != "rules" || good.Fingerprint != "fp" || !good.RebuiltAt.Equal(now) {
		t.Errorf("state %+v", good)
	}
	if ok, reason := good.canSkip("other", "rules", now.Add(time.Hour)); !ok || reason != skipRecent {
		t.Errorf("after a rebuild with the R2 MMDB: %v %q", ok, reason)
	}
	degraded := rebuiltState("fp", "rules", now, false)
	if degraded.Rules != "" {
		t.Errorf("state %+v", degraded)
	}
	if ok, _ := degraded.canSkip("other", "rules", now.Add(time.Hour)); ok {
		t.Error("the next run skipped after a rebuild without the R2 MMDB")
	}
	if ok, reason := degraded.canSkip("fp", "rules", now.Add(time.Hour)); !ok || reason != skipUnchanged {
		t.Errorf("the same fingerprint still skips as unchanged: %v %q", ok, reason)
	}
}
