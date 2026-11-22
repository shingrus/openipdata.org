package main

import (
	"errors"
	"strings"
	"testing"

	"openipdata.org/hatchet/geo"
	"openipdata.org/hatchet/rdns"
)

func TestStartASNRun(t *testing.T) {
	noLog := func(string) {}
	var out geo.PublishOutput
	called := 0
	startASNRun(&out, func() error { called++; return nil }, noLog)
	if called != 1 || out.ASNRun != "started" {
		t.Errorf("started: called=%d asn_run=%q", called, out.ASNRun)
	}
	out = geo.PublishOutput{}
	startASNRun(&out, func() error { return errors.New("down") }, noLog)
	if out.ASNRun != "failed" {
		t.Errorf("failed: asn_run=%q", out.ASNRun)
	}
	out = geo.PublishOutput{Skipped: true}
	startASNRun(&out, func() error { t.Error("called for a skipped output"); return nil }, noLog)
	if out.ASNRun != "" {
		t.Errorf("skipped: asn_run=%q", out.ASNRun)
	}
}

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
