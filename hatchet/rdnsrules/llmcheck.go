package rdnsrules

import (
	"regexp"
	"sort"
	"strings"

	"openipdata.org/hatchet/ipdata"
)

// Minimum evidence of a rule on the domain's real names (domains with 5+ hosts).
const (
	llmMinTokens  = 2
	llmMinDecoded = 0.2
	llmSmallHosts = 4 // up to this many hosts a domain is "small"
)

// Reasons an answer is rejected (the keys of the output's rejected counts).
const (
	rejBadJSON         = "bad_json"
	rejNoRegex         = "no_regex"
	rejRegexCompile    = "regex_compile"
	rejRegexGroups     = "regex_groups"
	rejRegexAnchor     = "regex_anchor"
	rejBadCountry      = "bad_country"
	rejNoCodes         = "no_resolvable_codes"
	rejNonLetter       = "non_letter_capture"
	rejFewTokens       = "too_few_tokens"
	rejLowDecoded      = "low_decoded"
	rejSmallNoDecode   = "small_no_decode"
	rejSmallCountry    = "small_country_mismatch"
	rejDomesticGate    = "domestic_gate"
	rejRejectedVersion = "rejected_version"
	rejMissing         = "missing_domain"
)

var lettersOnly = regexp.MustCompile(`^[a-z]+$`)

// checkedRule is an accepted answer: the regex, its resolved codes, and its evidence.
type checkedRule struct {
	Regex        string
	Re           *regexp.Regexp // compiled as locate does: (?i) + the regex
	Codes        map[string]Code
	Places       map[string]Place
	Geo          map[string]*geoPlace // token -> the GeoNames place
	Names        int
	Decoded      int
	Distinct     int
	CountryAgree *float64
	Dropped      map[string]string // token -> why it was dropped (unknown or ambiguous city)
}

// anchored reports whether the regex starts with ^ (after an optional (?i)) and ends with
// \.<domain>$.
func anchored(regex, domain string) bool {
	r := strings.TrimPrefix(regex, "(?i)")
	if !strings.HasPrefix(r, "^") {
		return false
	}
	q := regexp.QuoteMeta(domain)
	return strings.HasSuffix(r, `\.`+q+`$`) || strings.HasSuffix(r, `\.`+strings.ReplaceAll(q, "-", `\-`)+`$`)
}

// uniqueNames keeps one name per distinct PTR (lower case) with labels [a-z0-9-] below domain.
func uniqueNames(names []Name, domain string) []Name {
	seen := map[string]bool{}
	var out []Name
	for _, n := range names {
		if n.PTR == "" || !cleanLabels(n.PTR, domain) {
			continue
		}
		k := ptrKey(n.PTR)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
	}
	return out
}

// applyRule runs a compiled rule on a name the way locate does and returns the captured token.
func captureOf(re *regexp.Regexp, domain, ptr string) (string, bool) {
	host := ptrKey(ptr)
	if host != domain && !strings.HasSuffix(host, "."+domain) {
		return "", false
	}
	m := re.FindStringSubmatch(host)
	if len(m) != 2 || strings.TrimSpace(m[1]) == "" {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(m[1])), true
}

// checkAnswer applies the checks of the plan to one domain's answer on its real names. The
// first result is the accepted rule, or nil; the second is "" (accepted), "no_city" (the
// model found no location) or a rejection reason. small is a domain of 1 to 4 hosts.
func checkAnswer(domain string, a llmAnswer, names []Name, small bool, geo *geoIndex) (*checkedRule, string) {
	if !a.Location {
		return nil, "no_city"
	}
	if a.Regex == "" {
		return nil, rejNoRegex
	}
	re, err := regexp.Compile("(?i)" + a.Regex)
	if err != nil {
		return nil, rejRegexCompile
	}
	if re.NumSubexp() != 1 {
		return nil, rejRegexGroups
	}
	if !anchored(a.Regex, domain) {
		return nil, rejRegexAnchor
	}
	rule := &checkedRule{Regex: a.Regex, Re: re, Codes: map[string]Code{}, Places: map[string]Place{}, Geo: map[string]*geoPlace{}, Dropped: map[string]string{}}
	seenToken := map[string]bool{}
	for _, rawToken := range sortedKeys(a.Codes) {
		c := a.Codes[rawToken]
		// Keys are trimmed and lower-cased; of two keys that fold to one token the first in sorted order stays.
		token := strings.ToLower(strings.TrimSpace(rawToken))
		if seenToken[token] {
			continue
		}
		seenToken[token] = true
		country := strings.ToUpper(strings.TrimSpace(c.Country))
		if !ipdata.IsCountryCode(country) || !geo.countries[country] {
			return nil, rejBadCountry
		}
		if !lettersOnly.MatchString(token) {
			rule.Dropped[token] = "bad_token"
			continue
		}
		p, why := geo.resolve(country, c.Region, c.City)
		if p == nil {
			rule.Dropped[token] = why
			continue
		}
		rule.Places[token] = Place{p.Country, p.Name}
		rule.Geo[token] = p
		rule.Codes[token] = Code{Country: p.Country, City: p.Name, Origin: "llm", Scope: Scope{Dictionary: "llm"}, GeoNameID: p.ID}
	}
	if len(rule.Places) == 0 {
		return nil, rejNoCodes
	}
	real := uniqueNames(names, domain)
	used := map[string]bool{}
	cities := map[string]bool{}
	known, agrees := 0, 0
	for _, n := range real {
		token, ok := captureOf(re, domain, n.PTR)
		if !ok {
			continue
		}
		if !lettersOnly.MatchString(token) {
			return nil, rejNonLetter
		}
		place, ok := rule.Places[token]
		if !ok {
			continue
		}
		rule.Decoded++
		used[token] = true
		cities[place.Country+"\x00"+place.City] = true
		if n.Country != "" {
			known++
			if strings.EqualFold(n.Country, place.Country) {
				agrees++
			} else if small {
				return nil, rejSmallCountry
			}
		}
	}
	rule.Names, rule.Distinct = len(real), len(cities)
	if known > 0 {
		x := float64(agrees) / float64(known)
		rule.CountryAgree = &x
	}
	if small {
		if rule.Decoded < 1 {
			return nil, rejSmallNoDecode
		}
	} else {
		if len(used) < llmMinTokens {
			return nil, rejFewTokens
		}
		if float64(rule.Decoded) < llmMinDecoded*float64(len(real)) {
			return nil, rejLowDecoded
		}
	}
	if isDomestic(real) && (rule.CountryAgree == nil || *rule.CountryAgree < domesticMinAgree) {
		return nil, rejDomesticGate
	}
	return rule, ""
}

// majorityCountry is the country most names are announced in ("" when none is known).
func majorityCountry(names []Name) string {
	count := map[string]int{}
	for _, n := range names {
		if n.Country != "" {
			count[strings.ToUpper(n.Country)]++
		}
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best := ""
	for _, k := range keys {
		if best == "" || count[k] > count[best] {
			best = k
		}
	}
	return best
}
