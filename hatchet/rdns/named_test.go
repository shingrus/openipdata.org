package rdns

import (
	"net/netip"
	"strings"
	"testing"
)

func cnet(prefix, country string, pairs ...string) customerNet {
	n := customerNet{Prefix: netip.MustParsePrefix(prefix), Country: country}
	for i := 0; i < len(pairs); i += 2 {
		n.Names = append(n.Names, customerName{IP: netip.MustParseAddr(pairs[i]), PTR: pairs[i+1]})
	}
	return n
}

// Legacy rules: fra and muc in isp.net (DE). Candidate rule: the token after "cust-" in cable.net.
func namedRules(t *testing.T) ([]rule, cityRuleIndex) {
	t.Helper()
	_, rules := locateWorld(t)
	cand, st := compileCityRules([]cityRuleRow{{Domain: "cable.net", Rank: 1, Pattern: `^[a-z0-9-]+\.([a-z]+)\.cable\.net$`,
		PlacesJSON: `{"nyc":["US","New York"],"sea":["US","Seattle"],"ber":["DE","Berlin"],"muc":["DE","Munich"]}`, Status: "candidate", Version: "v1"}})
	if st.Loaded != 1 {
		t.Fatalf("candidate rules: %+v", st)
	}
	return rules, indexCityRules(cand)
}

func TestLocateNamedMajorityTieAndCountry(t *testing.T) {
	rules, cands := namedRules(t)
	nets := []customerNet{
		// Two of three names say New York: a row, the first matching host of the winner.
		cnet("30.0.1.0/24", "US", "30.0.1.1", "h1.nyc.cable.net", "30.0.1.50", "h2.sea.cable.net", "30.0.1.60", "h3.nyc.cable.net"),
		// One name each: a tie, no row.
		cnet("30.0.2.0/24", "US", "30.0.2.1", "h1.nyc.cable.net", "30.0.2.50", "h2.sea.cable.net"),
		// The city's country is not the row's: no row.
		cnet("30.0.3.0/24", "DE", "30.0.3.1", "h1.nyc.cable.net", "30.0.3.50", "h2.nyc.cable.net"),
		// A single decodable name is enough; the other names decode to nothing.
		cnet("30.0.4.0/24", "US", "30.0.4.1", "dyn-1-2-3-4.cable.net", "30.0.4.50", "h2.sea.cable.net"),
		// No name decodes, or no names at all.
		cnet("30.0.5.0/24", "US", "30.0.5.1", "dyn-1.cable.net"),
		cnet("30.0.6.0/24", "US"),
		// A legacy rule decodes it: method rdns_rule, rule name of the legacy rule.
		cnet("30.0.7.0/24", "DE", "30.0.7.9", "x.fra02.isp.net", "30.0.7.3", "y.fra03.isp.net"),
		// A legacy name and a candidate name give the winning city: the row takes the legacy one, though
		// the candidate's host comes first.
		cnet("30.0.8.0/24", "DE", "30.0.8.1", "x.muc.cable.net", "30.0.8.2", "y.muc01.isp.net", "30.0.8.3", "z.ber.cable.net"),
	}
	rows, st := locateNamed(nets, rules, cands)
	want := map[string]struct{ country, city, ip, rule, method string }{
		"30.0.1.0/24": {"US", "New York", "30.0.1.1", "cable.net#1", methodCandidate},
		"30.0.4.0/24": {"US", "Seattle", "30.0.4.50", "cable.net#1", methodCandidate},
		"30.0.7.0/24": {"DE", "Frankfurt", "30.0.7.3", "fra", methodRule},
		"30.0.8.0/24": {"DE", "Munich", "30.0.8.2", "muc", methodRule},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		w, ok := want[r.Prefix.String()]
		if !ok || r.Scope != scopeNamed || r.Country != w.country || r.City != w.city || r.RouterIP.String() != w.ip ||
			r.Rule != w.rule || r.Method != w.method || r.SameASN != nil || r.PTR == "" || !r.LocatedAt.IsZero() {
			t.Errorf("row %+v, want %+v", r, w)
		}
	}
	if st.Networks != 7 || st.Names != 15 || st.Rows != 4 || st.Ties != 1 || st.Foreign != 1 || st.Matched != 13 {
		t.Errorf("stats = %+v", st)
	}
	for i := 1; i < len(rows); i++ {
		if !rows[i-1].Prefix.Addr().Less(rows[i].Prefix.Addr()) {
			t.Errorf("rows not in address order: %v", rows)
		}
	}
}

// A name that a legacy rule matches is not decoded by a candidate (the order of the target names),
// and the country check is case-insensitive.
func TestLocateNamedLegacyFirst(t *testing.T) {
	_, rules := locateWorld(t)
	cand, _ := compileCityRules([]cityRuleRow{{Domain: "isp.net", Rank: 1, Pattern: `^[a-z]+\.([a-z0-9]+)\.isp\.net$`,
		PlacesJSON: `{"fra02":["DE","Offenbach"]}`, Status: "candidate", Version: "v"}})
	rows, _ := locateNamed([]customerNet{cnet("30.0.9.0/24", "de", "30.0.9.1", "x.fra02.isp.net")}, rules, indexCityRules(cand))
	if len(rows) != 1 || rows[0].City != "Frankfurt" || rows[0].Method != methodRule {
		t.Fatalf("rows = %+v", rows)
	}
}

// Named rows are volatile: never sticky, so the shrink guard and the carried rows never see them.
func TestNamedRowsAreNotSticky(t *testing.T) {
	r := geoRow{Scope: scopeNamed, Method: methodRule}
	if r.isSticky() {
		t.Error("a named row is sticky")
	}
	r.Method = methodCandidate
	if r.isSticky() {
		t.Error("a named candidate row is sticky")
	}
	// locate itself neither carries nor votes for them: a previous named row is not read as a traced one.
	o, rules := locateWorld(t)
	prev := []geoRow{{Prefix: netip.MustParsePrefix("5.6.7.0/24"), Scope: scopeNamed, Country: "DE", City: "Frankfurt", Method: methodRule}}
	rows, out := locate(nil, nil, rules, o, nil, prev)
	if len(rows) != 0 || out.Carried != 0 || out.Votes != 0 {
		t.Errorf("rows %+v out %+v", rows, out)
	}
}

// Every query that reads rdns_geo_layer by scope or method selects the traced scope explicitly,
// so the named scope changes none of them.
func TestScopeQueriesLeaveNamedAlone(t *testing.T) {
	for name, q := range map[string]string{
		"markLocatedSQL": markLocatedSQL, "tracedRowsQuery": tracedRowsQuery,
		"refreshQuery": refreshQuery, "retryFailedNamesQuery": retryFailedNamesQuery,
	} {
		if !strings.Contains(q, "scope = 'traced'") || strings.Contains(q, scopeNamed) {
			t.Errorf("%s does not select the traced scope only", name)
		}
	}
}
