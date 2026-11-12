package rdns

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
)

// rulesQuery reads the rules of public.generated_rules (written outside this
// repo, read only here). The first rule that matches a name wins, so
// the order is part of the data.
const rulesQuery = `SELECT coalesce(name, ''), coalesce(pattern, ''), coalesce(domains_json::text, ''),
		upper(btrim(coalesce(country, ''))), btrim(coalesce(city, ''))
	FROM public.generated_rules
	ORDER BY lower(name), name`

// ruleRow is a row of rulesQuery.
type ruleRow struct {
	Name, Pattern, DomainsJSON, Country, City string
}

// rule says that a host name inside one of its domains that matches its pattern
// is in the city. A rule with no domains has no domain scope: it applies to every host name.
type rule struct {
	name    string
	re      *regexp.Regexp
	domains []string // lower case, no dots at the ends; empty: no scope
	country string
	city    string
}

// ruleStats counts what happened to the rows of generated_rules.
type ruleStats struct {
	Loaded   int  // rules in use
	Skipped  int  // patterns Go's regexp does not compile (Python's lookarounds, backreferences, ...)
	Invalid  int  // no pattern, city or 2-letter country, or a domains_json that is not a list of domains
	NoTable  bool // public.generated_rules does not exist (yet)
	Examined int  // rows read
}

// compileRules turns the rows into rules, in the order of the rows. The Python
// patterns are compiled with Go's regexp, case-insensitive (names are lower
// case); those it rejects are skipped and counted. A rule without domains
// (an empty or missing domains_json) is valid and has no domain scope, as
// the rules were written for; a list whose entries are all blank is invalid.
func compileRules(rows []ruleRow) ([]rule, ruleStats) {
	st := ruleStats{Examined: len(rows)}
	var rules []rule
	for _, r := range rows {
		var domains []string
		if strings.TrimSpace(r.DomainsJSON) != "" && json.Unmarshal([]byte(r.DomainsJSON), &domains) != nil {
			st.Invalid++
			continue
		}
		var cleaned []string
		for _, d := range domains {
			if d = strings.Trim(strings.ToLower(strings.TrimSpace(d)), "."); d != "" {
				cleaned = append(cleaned, d)
			}
		}
		country, city := strings.ToUpper(strings.TrimSpace(r.Country)), strings.TrimSpace(r.City)
		if (len(domains) > 0 && len(cleaned) == 0) || city == "" || !ipdata.IsCountryCode(country) || strings.TrimSpace(r.Pattern) == "" {
			st.Invalid++
			continue
		}
		re, err := regexp.Compile("(?i)" + r.Pattern)
		if err != nil {
			st.Skipped++
			continue
		}
		rules = append(rules, rule{name: r.Name, re: re, domains: cleaned, country: country, city: city})
	}
	st.Loaded = len(rules)
	return rules, st
}

// inDomains reports whether host is one of the domains or below one; a rule
// with no domains takes every host.
func (r *rule) inDomains(host string) bool {
	if len(r.domains) == 0 {
		return true
	}
	for _, d := range r.domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// firstMatch returns the first rule that matches the PTR name, or nil. The name
// is lower-cased and loses its trailing dot first.
func firstMatch(rules []rule, ptr string) *rule {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(ptr)), ".")
	if host == "" {
		return nil
	}
	for i := range rules {
		if rules[i].inDomains(host) && rules[i].re.MatchString(host) {
			return &rules[i]
		}
	}
	return nil
}

// loadRules reads the rules. A missing table is no rules, not an error.
func loadRules(ctx context.Context, conn *pgx.Conn) ([]rule, ruleStats, error) {
	var table *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.generated_rules')::text`).Scan(&table); err != nil {
		return nil, ruleStats{}, err
	}
	if table == nil {
		return nil, ruleStats{NoTable: true}, nil
	}
	rows, err := conn.Query(ctx, rulesQuery)
	if err != nil {
		return nil, ruleStats{}, err
	}
	defer rows.Close()
	var all []ruleRow
	for rows.Next() {
		var r ruleRow
		if err := rows.Scan(&r.Name, &r.Pattern, &r.DomainsJSON, &r.Country, &r.City); err != nil {
			return nil, ruleStats{}, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, ruleStats{}, err
	}
	rules, st := compileRules(all)
	return rules, st, nil
}
