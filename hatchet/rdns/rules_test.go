package rdns

import (
	"strings"
	"testing"
)

var (
	contains = strings.Contains
	upper    = strings.ToUpper
)

func TestCompileRules(t *testing.T) {
	rows := []ruleRow{
		{"fra", `\.fra\d+\.`, `["isp.net", ".Example.COM."]`, "de", "Frankfurt"},
		{"lookahead", `(?!ams)ams`, `["isp.net"]`, "NL", "Amsterdam"}, // Go has no lookahead
		{"backref", `(\w)\1`, `["isp.net"]`, "NL", "Amsterdam"},       // nor backreferences
		{"nodomains", `par`, `[]`, "FR", "Paris"},
		{"emptydomains", `ber`, ``, "DE", "Berlin"},
		{"blankdomains", `par`, `[" ", "."]`, "FR", "Paris"},
		{"baddomains", `par`, `not json`, "FR", "Paris"},
		{"nocity", `par`, `["isp.net"]`, "FR", ""},
		{"nocountry", `par`, `["isp.net"]`, "France", "Paris"},
		{"nopattern", ` `, `["isp.net"]`, "FR", "Paris"},
		{"named", `(?P<city>lon)\d`, `["isp.net"]`, "GB", "London"},
	}
	rules, st := compileRules(rows)
	if st.Examined != 11 || st.Loaded != 4 || st.Skipped != 2 || st.Invalid != 5 || len(rules) != 4 {
		t.Fatalf("stats = %+v, %d rules", st, len(rules))
	}
	if rules[0].name != "fra" || rules[0].country != "DE" || rules[0].city != "Frankfurt" ||
		len(rules[0].domains) != 2 || rules[0].domains[1] != "example.com" || rules[1].name != "nodomains" || rules[2].name != "emptydomains" ||
		len(rules[1].domains) != 0 || rules[3].name != "named" {
		t.Errorf("rules = %+v", rules)
	}
}

func TestFirstMatch(t *testing.T) {
	rules, _ := compileRules([]ruleRow{
		{"a-generic", `\bfra\b`, `["isp.net"]`, "DE", "Frankfurt"},
		{"b-fra", `fra`, `["isp.net", "carrier.example"]`, "DE", "Frankfurt am Main"},
		{"c-ams", `ams\d`, `["isp.net"]`, "NL", "Amsterdam"},
		{"d-case", `^CORE-LON`, `["Other.org"]`, "GB", "London"},
	})
	for _, c := range []struct {
		ptr  string
		rule string // "" for no match
	}{
		{"ae1.fra.isp.net", "a-generic"},   // the first rule that matches wins
		{"AE1.Fra.ISP.NET.", "a-generic"},  // lower case, trailing dot
		{"ae1.fra02.isp.net", "b-fra"},     // not the first rule's \bfra\b
		{"fra02.carrier.example", "b-fra"}, // second domain
		{"ae2.ams1.isp.net", "c-ams"},
		{"ams1.isp.net", "c-ams"},     // the host is the domain's subdomain only if it has the dot: here it is isp.net's child
		{"ae2.ams1.evil-isp.net", ""}, // a domain suffix must start at a label
		{"ae2.ams1.isp.net.evil.org", ""},
		{"fra02.isp.net.example.org", ""},
		{"core-lon1.other.org", "d-case"},
		{"isp.net", ""}, // the domain itself is in scope but nothing matches
		{"", ""},
		{"ae9.par1.isp.net", ""},
	} {
		got := firstMatch(rules, c.ptr)
		switch {
		case c.rule == "" && got != nil:
			t.Errorf("%q matched %s", c.ptr, got.name)
		case c.rule != "" && (got == nil || got.name != c.rule):
			t.Errorf("%q: got %v, want %s", c.ptr, got, c.rule)
		}
	}
	// A rule's domain scope includes the domain itself.
	rs, _ := compileRules([]ruleRow{{"self", `^isp\.net$`, `["isp.net"]`, "DE", "Berlin"}})
	if firstMatch(rs, "isp.net") == nil {
		t.Error("the domain itself is out of scope")
	}
}

func TestRulesQueryOrder(t *testing.T) {
	if want := "ORDER BY lower(name), name"; !contains(rulesQuery, want) || !contains(rulesQuery, "FROM public.generated_rules") {
		t.Errorf("rulesQuery = %s", rulesQuery)
	}
	for _, bad := range []string{"INSERT", "UPDATE", "ALTER", "DELETE", "CREATE"} {
		if contains(upper(rulesQuery), bad) {
			t.Errorf("rulesQuery contains %s", bad)
		}
	}
}

// A rule without domains has no domain scope: it takes every name.
func TestRuleWithoutDomainsMatchesAnyHost(t *testing.T) {
	rules, st := compileRules([]ruleRow{
		{"scoped", `fra`, `["isp.net"]`, "DE", "Frankfurt"},
		{"open", `\bams\d*\b`, `[]`, "NL", "Amsterdam"},
		{"open-null", `\blon\d*\b`, `null`, "GB", "London"},
	})
	if st.Loaded != 3 || st.Invalid != 0 {
		t.Fatalf("stats = %+v", st)
	}
	for ptr, want := range map[string]string{
		"ae1.ams2.anything.example": "open", "ams1.isp.net": "open", "core.lon1.carrier.org": "open-null",
		"fra1.other.net":   "", // scoped, and not in its domain
		"ae1.fra1.isp.net": "scoped",
	} {
		got := firstMatch(rules, ptr)
		switch {
		case want == "" && got != nil:
			t.Errorf("%q matched %s", ptr, got.name)
		case want != "" && (got == nil || got.name != want):
			t.Errorf("%q: got %v, want %s", ptr, got, want)
		}
	}
}
