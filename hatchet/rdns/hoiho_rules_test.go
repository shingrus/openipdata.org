package rdns

import "testing"

func hoihoRow(domain string, seq int, pattern, places, status string) cityRuleRow {
	return cityRuleRow{Domain: domain, Rank: seq, Pattern: pattern, PlacesJSON: places, Status: status, Version: "h", Hoiho: true}
}

func TestHoihoRuleMatchesSeveralCaptures(t *testing.T) {
	rules, st := compileCityRules([]cityRuleRow{
		hoihoRow("example.net", 1, `^.+\.([a-z]{3})\d+\.([a-z]{2})\.example\.net$`, `{"ams,nl":["NL","Amsterdam"],"ams,us":["US","Amsterdam"]}`, "candidate"),
	})
	if st.Loaded != 1 || st.Hoiho != 1 || st.Invalid != 0 {
		t.Fatalf("stats %+v", st)
	}
	index := indexCityRules(rules)
	for _, tc := range []struct {
		ptr, city string
		ok        bool
	}{
		{"ae1.ams01.nl.example.net", "Amsterdam", true},
		{"AE1.AMS01.US.example.net.", "Amsterdam", true},
		{"ae1.ams01.de.example.net", "", false}, // no such key
		{"ae1.ams01.nl.other.net", "", false},
	} {
		got, rule, ok := firstCityRuleMatchRule(index, tc.ptr)
		if ok != tc.ok || got.City != tc.city {
			t.Errorf("%s = %+v ok=%v, want %q ok=%v", tc.ptr, got, ok, tc.city, tc.ok)
		}
		if ok && candidateRuleName(rule) != "example.net#h1" {
			t.Errorf("rule name %s", candidateRuleName(rule))
		}
	}
	if c, _ := firstCityRuleMatch(index, "ae1.ams01.us.example.net"); c.Country != "US" {
		t.Errorf("country %+v", c)
	}
}

func TestHoihoEmptyCaptureIsNoMatch(t *testing.T) {
	rules, _ := compileCityRules([]cityRuleRow{
		hoihoRow("example.net", 1, `^([a-z]+)(\d*)\.example\.net$`, `{"ams":["NL","Amsterdam"]}`, "candidate"),
		hoihoRow("example.org", 1, `^([a-z]+)(?:-([a-z]+))?\.example\.org$`, `{"ams":["NL","Amsterdam"]}`, "candidate"),
	})
	index := indexCityRules(rules)
	if _, ok := firstCityRuleMatch(index, "ams.example.net"); ok {
		t.Error("an empty second capture matched")
	}
	if _, ok := firstCityRuleMatch(index, "ams.example.org"); ok {
		t.Error("an unset capture matched")
	}
}

func TestHoihoRejectedRowsAreIgnored(t *testing.T) {
	rules, st := compileCityRules([]cityRuleRow{
		hoihoRow("example.net", 1, `^([a-z]+)\.example\.net$`, `{"ams":["NL","Amsterdam"]}`, "rejected"),
	})
	if len(rules) != 0 || st.Hoiho != 0 {
		t.Fatalf("a rejected row was loaded: %+v", st)
	}
}

func TestHoihoRowNeedsACapture(t *testing.T) {
	_, st := compileCityRules([]cityRuleRow{
		hoihoRow("example.net", 1, `^[a-z]+\.example\.net$`, `{"ams":["NL","Amsterdam"]}`, "candidate"),
	})
	if st.Loaded != 0 || st.Invalid != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// A learned row keeps exactly one capture group.
func TestLearnedRowWithTwoCapturesStaysInvalid(t *testing.T) {
	_, st := compileCityRules([]cityRuleRow{
		{Domain: "example.net", Rank: 1, Pattern: `^([a-z]+)\.([a-z]+)\.example\.net$`, PlacesJSON: `{"a,b":["NL","Amsterdam"]}`, Status: "candidate"},
	})
	if st.Loaded != 0 || st.Invalid != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// Hoiho rules are tried before the learned ones for the same name, whether the Hoiho domain is a
// shorter or a longer suffix than the learned rule's domain.
func TestHoihoBeforeLearned(t *testing.T) {
	learned := func(domain string) cityRuleRow {
		return cityRuleRow{Domain: domain, Rank: 1, Pattern: `^[a-z0-9]+\.([a-z]+)\d*\.[a-z]+\.example\.net$`, PlacesJSON: `{"ams":["DE","Learned"]}`, Status: "candidate"}
	}
	hoiho := func(domain string) cityRuleRow {
		return hoihoRow(domain, 1, `^[a-z0-9]+\.([a-z]+)\d*\.[a-z]+\.example\.net$`, `{"ams":["NL","Hoiho"]}`, "candidate")
	}
	const name = "ae1.ams01.nl.example.net"
	for _, tc := range []struct{ name, learnedDomain, hoihoDomain string }{
		{"same domain", "example.net", "example.net"},
		{"hoiho shorter suffix", "nl.example.net", "example.net"},
		{"hoiho longer suffix", "example.net", "nl.example.net"},
	} {
		rules, _ := compileCityRules([]cityRuleRow{learned(tc.learnedDomain), hoiho(tc.hoihoDomain)})
		got, rule, ok := firstCityRuleMatchRule(indexCityRules(rules), name)
		if !ok || got.City != "Hoiho" || !rule.hoiho {
			t.Errorf("%s: got %+v ok=%v", tc.name, got, ok)
		}
	}
	// When no Hoiho rule gives a city the learned rule decides.
	rules, _ := compileCityRules([]cityRuleRow{learned("example.net"), hoihoRow("example.net", 1, `^zzz([a-z]+)\.example\.net$`, `{"ams":["NL","Hoiho"]}`, "candidate")})
	if got, rule, ok := firstCityRuleMatchRule(indexCityRules(rules), name); !ok || got.City != "Learned" || rule.hoiho {
		t.Errorf("learned fallback: %+v ok=%v", got, ok)
	}
}

// A Hoiho regex that matches without a place does not stop the next one (seq order).
func TestHoihoFallsThroughInSeqOrder(t *testing.T) {
	rules, _ := compileCityRules([]cityRuleRow{
		hoihoRow("example.net", 2, `^[a-z]+-([a-z]+)\.example\.net$`, `{"ams":["NL","Second"]}`, "candidate"),
		hoihoRow("example.net", 1, `^[a-z]+-([a-z]+)\.example\.net$`, `{"fra":["DE","First"]}`, "candidate"),
	})
	got, rule, ok := firstCityRuleMatchRule(indexCityRules(rules), "rr-ams.example.net")
	if !ok || got.City != "Second" || candidateRuleName(rule) != "example.net#h2" {
		t.Fatalf("got %+v %v ok=%v", got, rule, ok)
	}
	if got, _, ok := firstCityRuleMatchRule(indexCityRules(rules), "rr-fra.example.net"); !ok || got.City != "First" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}
