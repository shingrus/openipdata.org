package main

import (
	"errors"
	"strings"
	"testing"

	"openipdata.org/hatchet/rdns"
)

// A run that picked its full limit and stored traces starts the next one; any other does not.
func TestChainNext(t *testing.T) {
	full := rdns.PickOutput{Picked: 1000, Limit: 1000}
	stored := rdns.TraceOutput{Stored: 3000}
	for name, c := range map[string]struct {
		pick    rdns.PickOutput
		trace   rdns.TraceOutput
		trigger error
		started bool
		chained bool
	}{
		"full run":                {full, stored, nil, true, true},
		"short run":               {rdns.PickOutput{Picked: 10, Limit: 1000}, stored, nil, false, false},
		"nothing stored":          {full, rdns.TraceOutput{}, nil, false, false},
		"trigger fails":           {full, stored, errors.New("hatchet is down"), true, false},
		"empty output (no limit)": {rdns.PickOutput{}, rdns.TraceOutput{}, nil, false, false},
	} {
		var logs []string
		started := false
		got := chainNext(c.pick, c.trace, func() error { started = true; return c.trigger }, func(m string) { logs = append(logs, m) })
		if got != c.chained || started != c.started {
			t.Errorf("%s: chained %v started %v, want %v %v", name, got, started, c.chained, c.started)
		}
		if c.chained && !strings.Contains(strings.Join(logs, "\n"), "queued") {
			t.Errorf("%s: logs %q", name, logs)
		}
	}
}
