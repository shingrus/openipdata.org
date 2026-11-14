package main

import (
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestFmtPct(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
		ok   bool
	}{
		{97.2, "97.2", true}, {61, "61.0", true}, {100, "100.0", true}, {0, "0.0", true},
		{30.54, "30.5", true}, {math.NaN(), "—", false}, {-0.1, "—", false}, {100.1, "—", false},
		{math.Inf(1), "—", false}, {math.Copysign(0, -1), "0.0", true},
	} {
		got, ok := fmtPct(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("fmtPct(%v) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestFmtInt(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 12480: "12,480", 1234567: "1,234,567"} {
		if got := fmtInt(in); got != want {
			t.Errorf("fmtInt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParseStamp(t *testing.T) {
	epoch := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	full := `{"built_at":"2026-11-14T07:42:10Z","metrics":{"atlas_country_pct":97.2,"atlas_city_pct":30.5,"city_pct":61.0,"atlas_probes":12480,"x":"y"},"future":1}`
	s := parseStamp(full, epoch)
	if s.Country != 97.2 || s.City != 30.5 || s.Coverage != 61 || s.Probes != 12480 {
		t.Errorf("full stamp: %+v", s)
	}
	if want := time.Date(2026, 11, 14, 7, 42, 10, 0, time.UTC); !s.Built.Equal(want) {
		t.Errorf("built = %v", s.Built)
	}

	s = parseStamp(`{"built_at":"2026-11-14T07:42:10Z"}`, epoch)
	if !math.IsNaN(s.Country) || !math.IsNaN(s.City) || !math.IsNaN(s.Coverage) || s.Probes != -1 || s.Built.IsZero() {
		t.Errorf("missing metrics: %+v", s)
	}

	for _, raw := range []string{"", "not json", "[1,2]", `{"metrics":"x"}`, `{"metrics":{"city_pct":"61"}}`} {
		s = parseStamp(raw, time.Time{})
		if !math.IsNaN(s.Country) || !math.IsNaN(s.City) || !math.IsNaN(s.Coverage) || s.Probes != -1 || !s.Built.IsZero() {
			t.Errorf("garbage %q: %+v", raw, s)
		}
	}

	// no built_at, or an invalid one: the MMDB build epoch
	for _, raw := range []string{`{"metrics":{"city_pct":50}}`, `{"built_at":"yesterday"}`} {
		if s = parseStamp(raw, epoch); !s.Built.Equal(epoch) {
			t.Errorf("fallback %q: built = %v", raw, s.Built)
		}
	}

	// fractional or negative probes are not a device count
	for _, raw := range []string{`{"metrics":{"atlas_probes":1.5}}`, `{"metrics":{"atlas_probes":-3}}`} {
		if s = parseStamp(raw, time.Time{}); s.Probes != -1 {
			t.Errorf("%q: probes = %d", raw, s.Probes)
		}
	}
}

func TestMakeViewMissing(t *testing.T) {
	v := makeView(noScores())
	if v.Updated || v.Probes != "" {
		t.Errorf("view: %+v", v)
	}
	if z := makeView(scores{Country: 1, City: 1, Coverage: 1, Probes: 0}); z.Probes != "" {
		t.Errorf("probes 0 shown as %q", z.Probes)
	}
	for _, c := range v.Cards {
		if c.Num != "—" || c.Pct || c.Bar != "0%" {
			t.Errorf("card: %+v", c)
		}
	}
	if !strings.Contains(v.Cards[2].Label, "have a city") {
		t.Errorf("labels: %+v", v.Cards)
	}
}

func TestLightBlocksIdentical(t *testing.T) {
	a := regexp.MustCompile(`(?s)prefers-color-scheme: light\)\{\s*:root:not\(\[data-theme="dark"\]\)\{(.*?)\}`).FindStringSubmatch(styleCSS)
	b := regexp.MustCompile(`(?s):root\[data-theme="light"\]\{(.*?)\}`).FindStringSubmatch(styleCSS)
	if a == nil || b == nil {
		t.Fatal("light block not found")
	}
	if strings.Join(strings.Fields(a[1]), " ") != strings.Join(strings.Fields(b[1]), " ") {
		t.Errorf("light blocks differ:\n%s\n%s", a[1], b[1])
	}
}
