package rdns

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
)

// cityRule is a learned extraction rule. V1 consumes candidates only; active
// rules stay unused until sticky learned rows with provenance and revocation exist.
// A domain has up to 3 rules, tried in rank order (1 first).
type cityRule struct {
	domain  string
	rank    int
	re      *regexp.Regexp
	places  map[string]cityKey
	status  string
	version string
}

type cityRuleRow struct {
	Domain, Pattern, PlacesJSON, Status, Version string
	Rank                                         int
}

type cityRuleStats struct {
	Loaded, Invalid, Skipped int
	Missing                  bool
}

const cityRulesQuery = `SELECT domain, rank::int, pattern, places::text, status, version
	FROM public.rdns_city_rules ORDER BY lower(domain), domain, rank`

func compileCityRules(rows []cityRuleRow) ([]cityRule, cityRuleStats) {
	st := cityRuleStats{}
	out := make([]cityRule, 0, len(rows))
	for _, row := range rows {
		// Only candidates are eligible in v1. Rejected and active rows cannot
		// contribute a vote.
		if row.Status != "candidate" {
			continue
		}
		domain := strings.Trim(strings.ToLower(strings.TrimSpace(row.Domain)), ".")
		var raw map[string][]string
		if domain == "" || strings.TrimSpace(row.Pattern) == "" || json.Unmarshal([]byte(row.PlacesJSON), &raw) != nil {
			st.Invalid++
			continue
		}
		re, err := regexp.Compile("(?i)" + row.Pattern)
		if err != nil {
			st.Skipped++
			continue
		}
		if re.NumSubexp() != 1 {
			st.Invalid++
			continue
		}
		places := make(map[string]cityKey, len(raw))
		conflict := false
		for token, place := range raw {
			if len(place) != 2 {
				continue
			}
			country, city := strings.ToUpper(strings.TrimSpace(place[0])), strings.TrimSpace(place[1])
			if !ipdata.IsCountryCode(country) || city == "" {
				continue
			}
			key, value := strings.ToLower(strings.TrimSpace(token)), cityKey{Country: country, City: city}
			if existing, ok := places[key]; ok {
				if existing != value {
					conflict = true
					break
				}
				continue // equivalent normalized keys can be merged
			}
			places[key] = value
		}
		if conflict || len(places) == 0 {
			st.Invalid++
			continue
		}
		out = append(out, cityRule{domain: domain, rank: max(row.Rank, 1), re: re, places: places, status: row.Status, version: row.Version})
	}
	st.Loaded = len(out)
	return out, st
}

func (r *cityRule) match(ptr string) (cityKey, bool) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(ptr)), ".")
	if host == "" || (host != r.domain && !strings.HasSuffix(host, "."+r.domain)) {
		return cityKey{}, false
	}
	m := r.re.FindStringSubmatch(host)
	if len(m) != 2 || strings.TrimSpace(m[1]) == "" {
		return cityKey{}, false
	}
	c, ok := r.places[strings.ToLower(strings.TrimSpace(m[1]))]
	return c, ok
}

// cityRuleIndex holds the rules of each domain in rank order.
type cityRuleIndex map[string][]cityRule

func indexCityRules(rules []cityRule) cityRuleIndex {
	index := make(cityRuleIndex, len(rules))
	for _, rule := range rules {
		index[rule.domain] = append(index[rule.domain], rule)
	}
	for _, list := range index {
		sort.SliceStable(list, func(i, j int) bool { return list[i].rank < list[j].rank })
	}
	return index
}

// firstCityRuleMatch checks only registered-domain suffixes present in the
// index, so each hostname does work proportional to its label count rather than
// the number of learned domains. A domain's rules are tried in rank order; a regex
// match with no place does not stop the next rank.
func firstCityRuleMatch(rules cityRuleIndex, ptr string) (cityKey, bool) {
	c, _, ok := firstCityRuleMatchRule(rules, ptr)
	return c, ok
}

// firstCityRuleMatchRule is firstCityRuleMatch that also returns the rule that matched.
func firstCityRuleMatchRule(rules cityRuleIndex, ptr string) (cityKey, *cityRule, bool) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(ptr)), ".")
	for start := 0; start < len(host); {
		for i := range rules[host[start:]] {
			rule := &rules[host[start:]][i]
			if c, ok := rule.match(host); ok {
				return c, rule, true
			}
		}
		dot := strings.IndexByte(host[start:], '.')
		if dot < 0 {
			break
		}
		start += dot + 1
	}
	return cityKey{}, nil, false
}

// loadCityRules reads only the learner's published rows. A missing table is
// expected during rollout and means no candidate rules.
func loadCityRules(ctx context.Context, conn *pgx.Conn) ([]cityRule, cityRuleStats, error) {
	var table *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.rdns_city_rules')::text`).Scan(&table); err != nil {
		return nil, cityRuleStats{}, err
	}
	if table == nil {
		return nil, cityRuleStats{Missing: true}, nil
	}
	rows, err := conn.Query(ctx, cityRulesQuery)
	if err != nil {
		return nil, cityRuleStats{}, err
	}
	defer rows.Close()
	var all []cityRuleRow
	for rows.Next() {
		var row cityRuleRow
		if err := rows.Scan(&row.Domain, &row.Rank, &row.Pattern, &row.PlacesJSON, &row.Status, &row.Version); err != nil {
			return nil, cityRuleStats{}, err
		}
		all = append(all, row)
	}
	if err := rows.Err(); err != nil {
		return nil, cityRuleStats{}, err
	}
	rules, st := compileCityRules(all)
	return rules, st, nil
}
