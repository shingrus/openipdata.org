package rdnsrules

import (
	"regexp"
	"sort"
	"strings"
)

// sameCityKm: a learned city and the LLM's city that are this close are the same place for
// the plan (Esenyurt and Istanbul); the finer LLM name is kept.
const sameCityKm = 100

// learnedRule is a stored learned row (ranks 1 to 3) compiled the way locate does.
type learnedRule struct {
	Row    llmRuleRow
	Re     *regexp.Regexp
	Places map[string]Place
	Scope  Scope
	HasScp bool
}

// compileLearned compiles the learned rows of a domain that are not withdrawn.
func compileLearned(domain string, rows []llmRuleRow) []learnedRule {
	var out []learnedRule
	for _, row := range rows {
		if row.Rank < 1 || row.Rank > MaxRank || row.Status == "rejected" {
			continue
		}
		re, err := regexp.Compile("(?i)" + row.Pattern)
		if err != nil || re.NumSubexp() != 1 {
			continue
		}
		lr := learnedRule{Row: row, Re: re, Places: map[string]Place{}}
		for t, p := range row.Places {
			lr.Places[strings.ToLower(t)] = Place{p[0], p[1]}
		}
		lr.Scope, lr.HasScp = scopeOfRule(domain, row.Pattern, row.Dictionary)
		out = append(out, lr)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Row.Rank < out[j].Row.Rank })
	return out
}

// scopeOfRule finds the extraction (label, token, dictionary) a learned pattern was written
// for, by writing the pattern of each candidate extraction again (the learner's patterns are
// deterministic). A manually written pattern has none.
func scopeOfRule(domain, pattern, dictionary string) (Scope, bool) {
	for label := 1; label <= 12; label++ {
		for token := 1; token <= 12; token++ {
			if extractionPattern(domain, extraction{label: label, token: token, dictionary: dictionary}) == pattern {
				return Scope{LabelFromRight: label, TokenFromLeft: token, Dictionary: dictionary}, true
			}
		}
	}
	return Scope{}, false
}

// firstLearned is what locate does with the learned rows of a domain: ranks in order, a
// regex match whose token has no place does not stop the next rank.
func firstLearned(rules []learnedRule, domain, ptr string) (rank int, token string, place Place, ok bool) {
	for _, r := range rules {
		t, matched := captureOf(r.Re, domain, ptr)
		if !matched {
			continue
		}
		if p, has := r.Places[t]; has {
			return r.Row.Rank, t, p, true
		}
	}
	return 0, "", Place{}, false
}

// tokenDispute is one learned (rank, token) whose city the LLM rule contradicts on the real names.
type tokenDispute struct {
	Rank     int
	Token    string
	Learned  Place
	Terra    *geoPlace // nil: the names split between several LLM cities (no clear one)
	Names    int       // names that disagree
	Examples []string  // patterns of such names
}

// compareRules runs every real name through the learned rule that decodes it and through the
// LLM rule. They disagree when the countries differ or the cities are more than 100 km apart
// (a learned city GeoNames cannot place counts as a disagreement unless the names are equal).
// A (rank, token) is a dispute when more of its names disagree than agree.
func compareRules(domain string, names []Name, learned []learnedRule, rule *checkedRule, geo *geoIndex) []tokenDispute {
	type key struct {
		rank  int
		token string
	}
	type acc struct {
		learned  Place
		agree    int
		terra    map[int64]int
		places   map[int64]*geoPlace
		examples []string
	}
	groups := map[key]*acc{}
	for _, n := range uniqueNames(names, domain) {
		rank, token, lp, ok := firstLearned(learned, domain, n.PTR)
		if !ok {
			continue
		}
		t, matched := captureOf(rule.Re, domain, n.PTR)
		if !matched {
			continue
		}
		tp := rule.Geo[t]
		if tp == nil {
			continue
		}
		k := key{rank, token}
		a := groups[k]
		if a == nil {
			a = &acc{learned: lp, terra: map[int64]int{}, places: map[int64]*geoPlace{}}
			groups[k] = a
		}
		if samePlace(geo, lp, tp) {
			a.agree++
			continue
		}
		a.terra[tp.ID]++
		a.places[tp.ID] = tp
		if len(a.examples) < 4 {
			if pat := patternOf(n.PTR, domain); pat != "" {
				a.examples = append(a.examples, pat)
			}
		}
	}
	var out []tokenDispute
	for k, a := range groups {
		disagree := 0
		var top, second int
		var topID int64
		for id, c := range a.terra {
			disagree += c
			if c > top || (c == top && id < topID) {
				second, top, topID = top, c, id
			} else if c > second {
				second = c
			}
		}
		if disagree == 0 || disagree <= a.agree {
			continue
		}
		d := tokenDispute{Rank: k.rank, Token: k.token, Learned: a.learned, Names: disagree, Examples: a.examples}
		if top > second {
			d.Terra = a.places[topID]
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank < out[j].Rank
		}
		return out[i].Token < out[j].Token
	})
	return out
}

// samePlace: the learned (country, city) and the LLM's GeoNames place are one place.
func samePlace(geo *geoIndex, learned Place, terra *geoPlace) bool {
	if !strings.EqualFold(learned.Country, terra.Country) {
		return false
	}
	if foldName(learned.City) == foldName(terra.Name) {
		return true
	}
	lp, _ := geo.resolve(learned.Country, "", learned.City)
	return lp != nil && sameArea(lp, terra, sameCityKm)
}

// agreement is country_agree of a learned rule with the given places on the real names.
func agreement(domain string, names []Name, re *regexp.Regexp, places map[string]Place) *float64 {
	known, agrees := 0, 0
	for _, n := range uniqueNames(names, domain) {
		t, ok := captureOf(re, domain, n.PTR)
		if !ok {
			continue
		}
		p, ok := places[t]
		if !ok || n.Country == "" {
			continue
		}
		known++
		if strings.EqualFold(n.Country, p.Country) {
			agrees++
		}
	}
	if known == 0 {
		return nil
	}
	x := float64(agrees) / float64(known)
	return &x
}
