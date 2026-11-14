package rdns

import (
	"fmt"
	"testing"
)

func TestCompileCityRulesStatusPatternAndMappings(t *testing.T) {
	rules, st := compileCityRules([]cityRuleRow{
		{Domain: "Transit.NET.", Pattern: `^be2\.([a-z]+)\d*\.transit\.net$`, PlacesJSON: `{"xyz":["nl"," Amsterdam "]}`, Status: "candidate", Version: "v1"},
		{Domain: "active.net", Pattern: `(.+)`, PlacesJSON: `{"x":["US","X"]}`, Status: "active"},
		{Domain: "bad.net", Pattern: `(.*)(.*)`, PlacesJSON: `{"x":["US","X"]}`, Status: "candidate"},
		{Domain: "empty.net", Pattern: `(.+)`, PlacesJSON: `{"x":["DEU","X"]}`, Status: "candidate"},
	})
	if st.Loaded != 1 || st.Invalid != 2 || st.Skipped != 0 {
		t.Fatalf("stats = %+v", st)
	}
	for _, tc := range []struct {
		ptr string
		ok  bool
	}{
		{"be2.xyz1.transit.net", true},
		{"be2.unknown1.transit.net", false},
		{"be2.xyz1.other.net", false},
		{"prefix.be2.xyz1.transit.net", false},
	} {
		_, got := rules[0].match(tc.ptr)
		if got != tc.ok {
			t.Errorf("match(%q) = %v, want %v", tc.ptr, got, tc.ok)
		}
	}
}

func TestCompileCityRulesRejectsConflictingNormalizedTokens(t *testing.T) {
	rules, st := compileCityRules([]cityRuleRow{
		{Domain: "conflict.net", Pattern: `^([a-z]+)\.conflict\.net$`, PlacesJSON: `{"xyz":["NL","Amsterdam"],"XYZ":["BE","Brussels"]}`, Status: "candidate"},
		{Domain: "same.net", Pattern: `^([a-z]+)\.same\.net$`, PlacesJSON: `{"xyz":["NL","Amsterdam"],"XYZ":["NL","Amsterdam"]}`, Status: "candidate"},
	})
	if st.Invalid != 1 || st.Loaded != 1 || len(rules) != 1 {
		t.Fatalf("conflicting token stats = %+v, rules=%+v", st, rules)
	}
	if got, ok := rules[0].places["xyz"]; !ok || got != (cityKey{Country: "NL", City: "Amsterdam"}) {
		t.Errorf("merged token = %+v, ok=%v", got, ok)
	}
}

// A domain's rules are tried in rank order whatever order the rows come in, and a regex
// match with no place does not stop the next rank.
func TestCityRuleRanksTriedInOrder(t *testing.T) {
	rules, st := compileCityRules([]cityRuleRow{
		{Domain: "isp.net", Rank: 3, Pattern: `^c-([a-z]+)\d*\.isp\.net$`, PlacesJSON: `{"abc":["US","Chicago"]}`, Status: "candidate"},
		{Domain: "isp.net", Rank: 2, Pattern: `^b-([a-z]+)\d*\.isp\.net$`, PlacesJSON: `{"xyz":["NL","Amsterdam"]}`, Status: "candidate"},
		{Domain: "isp.net", Rank: 1, Pattern: `^([a-z]+)\d*\.isp\.net$`, PlacesJSON: `{"fra":["DE","Frankfurt"],"b":["GB","London"]}`, Status: "candidate"},
		{Domain: "isp.net", Rank: 1, Pattern: `^never\.isp\.net$`, PlacesJSON: `{}`, Status: "candidate"},
	})
	if st.Loaded != 3 || st.Invalid != 1 {
		t.Fatalf("stats = %+v", st)
	}
	index := indexCityRules(rules)
	if got := index["isp.net"]; len(got) != 3 || got[0].rank != 1 || got[1].rank != 2 || got[2].rank != 3 {
		t.Fatalf("index order = %+v", got)
	}
	for _, tc := range []struct {
		ptr  string
		want cityKey
		rank int
		ok   bool
	}{
		{"fra01.isp.net", cityKey{"DE", "Frankfurt"}, 1, true},
		{"b-xyz2.isp.net", cityKey{"NL", "Amsterdam"}, 2, true}, // rank 1 does not match this shape
		{"c-abc.isp.net", cityKey{"US", "Chicago"}, 3, true},
		{"c-unknown.isp.net", cityKey{}, 0, false}, // regex matches, token has no place
		{"fra01.other.net", cityKey{}, 0, false},
	} {
		got, rule, ok := firstCityRuleMatchRule(index, tc.ptr)
		if ok != tc.ok || got != tc.want || (ok && rule.rank != tc.rank) {
			t.Errorf("%s = %+v rank %v ok=%v, want %+v rank %d ok=%v", tc.ptr, got, rule, ok, tc.want, tc.rank, tc.ok)
		}
	}
	// A rank 1 regex that matches but has no place for the token falls through to rank 2.
	fall, _ := compileCityRules([]cityRuleRow{
		{Domain: "isp.net", Rank: 1, Pattern: `^[a-z]-([a-z]+)\d*\.isp\.net$`, PlacesJSON: `{"fra":["DE","Frankfurt"]}`, Status: "candidate"},
		{Domain: "isp.net", Rank: 2, Pattern: `^[a-z]-([a-z]+)\d*\.isp\.net$`, PlacesJSON: `{"xyz":["NL","Amsterdam"]}`, Status: "candidate"},
	})
	if got, ok := firstCityRuleMatch(indexCityRules(fall), "b-xyz1.isp.net"); !ok || got.City != "Amsterdam" {
		t.Errorf("fall through to rank 2 = %+v ok=%v", got, ok)
	}
}

// Rank 4 is the LLM rule of learn-rdns-rules: tried after the learned ranks, same rules.
func TestCityRuleRankFourIsTriedLast(t *testing.T) {
	rules, st := compileCityRules([]cityRuleRow{
		{Domain: "isp.net", Rank: 4, Pattern: `^[a-z0-9-]+\.([a-z]+)\.isp\.net$`, PlacesJSON: `{"lnd":["GB","London"],"fra":["DE","Frankfurt am Main"]}`, Status: "candidate", Version: "v4"},
		{Domain: "isp.net", Rank: 1, Pattern: `^([a-z]+)\d*\.isp\.net$`, PlacesJSON: `{"fra":["DE","Frankfurt"]}`, Status: "candidate"},
		{Domain: "other.net", Rank: 4, Pattern: `^([a-z]+)\.other\.net$`, PlacesJSON: `{"ams":["NL","Amsterdam"]}`, Status: "rejected"},
	})
	if st.Loaded != 2 {
		t.Fatalf("stats %+v", st)
	}
	index := indexCityRules(rules)
	for _, tc := range []struct {
		ptr  string
		city string
		rank int
	}{
		{"fra1.isp.net", "Frankfurt", 1}, // the learned rank decodes it first
		{"r1.lnd.isp.net", "London", 4},  // only the LLM rule does
		{"r1.fra.isp.net", "Frankfurt am Main", 4},
	} {
		got, rule, ok := firstCityRuleMatchRule(index, tc.ptr)
		if !ok || got.City != tc.city || rule.rank != tc.rank {
			t.Errorf("%s = %+v %v ok=%v", tc.ptr, got, rule, ok)
		}
		if ok && candidateRuleName(rule) != "isp.net#"+fmt.Sprint(tc.rank) {
			t.Errorf("rule name %s", candidateRuleName(rule))
		}
	}
	if _, _, ok := firstCityRuleMatchRule(index, "r1.ams.other.net"); ok {
		t.Error("a withdrawn rank 4 rule voted")
	}
}
