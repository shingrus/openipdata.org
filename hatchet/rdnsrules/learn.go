// Package rdnsrules learns one reproducible router-name extraction rule per
// registered domain and stores its effective city dictionary in Postgres.
package rdnsrules

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/net/publicsuffix"
	"golang.org/x/text/unicode/norm"
)

const MinSupport = 3

// roleTokens are router role and interface words that often sit at a fixed spot
// of every name and are also in a place dictionary (`ccr` is an airport). They are
// never decoded from a dictionary; a scoped manual/LLM override can still map one.
var roleTokens = map[string]bool{}

func init() {
	for _, t := range strings.Fields(`ccr core edge gw bb agg bng bras pe cr ar br dr sr mx lag rtr swi sw
		router cs cor cbr ear er ir lns olt ont dsl cmts ge gi te xe fa eth ae be po vlan lo
		border bdr csr dist ags cpe nas nat fw lb vpn mpls ospf bgp`) {
		roleTokens[t] = true
	}
}

// cleanLabels reports whether every label left of the registered domain consists
// only of [a-z0-9-], the alphabet the generated regex accepts; other names are not
// learned from, so the learner and the regex always agree.
func cleanLabels(ptr, domain string) bool {
	ls := labelsBeforeDomain(ptr, domain)
	if len(ls) == 0 {
		return false
	}
	for _, l := range ls {
		if l == "" {
			return false
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

type Place struct {
	Country string `json:"country"`
	City    string `json:"city"`
}

type Scope struct {
	LabelFromRight int    `json:"label_from_right"`
	TokenFromLeft  int    `json:"token_from_left"`
	Dictionary     string `json:"dictionary"`
}

type Code struct {
	Country string `json:"country"`
	City    string `json:"city"`
	Origin  string `json:"origin"`
	Scope   Scope  `json:"scope"`
}

type Name struct {
	IP      string
	PTR     string
	Country string
}

type Candidate struct {
	Domain       string
	Pattern      string
	Dictionary   string
	Scope        Scope
	Names        int
	Decoded      int
	Distinct     int
	CountryAgree *float64
	Places       map[string]Place
	Version      string
	Rank         int // 1..MaxRank: the order locate tries a domain's rules in
}

// Dictionaries are exact normalized-token lookups. A nil/ambiguous result is
// deliberately unresolved.
type Dictionaries map[string]map[string]Place

type extraction struct {
	label, token int
	dictionary   string
}

var runsRE = regexp.MustCompile(`[a-z]+|[0-9]+`)

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
	return strings.Trim(s, ".")
}

func registeredDomain(host string) string {
	host = normalize(host)
	if net.ParseIP(host) != nil {
		return ""
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return d
}

func labelsBeforeDomain(host, domain string) []string {
	host = normalize(host)
	if host == domain || !strings.HasSuffix(host, "."+domain) {
		return nil
	}
	left := strings.TrimSuffix(host, "."+domain)
	if left == "" {
		return nil
	}
	return strings.Split(left, ".")
}

func letterTokens(label string) []string {
	var out []string
	for _, s := range runsRE.FindAllString(strings.ToLower(label), -1) {
		if s[0] >= 'a' && s[0] <= 'z' {
			out = append(out, s)
		}
	}
	return out
}

func extract(host, domain string, e extraction) (string, bool) {
	labels := labelsBeforeDomain(host, domain)
	idx := len(labels) - e.label
	if idx < 0 || idx >= len(labels) {
		return "", false
	}
	tokens := letterTokens(labels[idx])
	if e.token < 1 || e.token > len(tokens) {
		return "", false
	}
	return tokens[e.token-1], true
}

func extractionPattern(domain string, e extraction) string {
	// A sublabel consists only of letters, digits and hyphens. The capture is
	// an entire letter run at exactly the same boundary used by extract.
	var selected strings.Builder
	selected.WriteString(`[0-9-]*`)
	for i := 1; i < e.token; i++ {
		selected.WriteString(`[a-z]+[0-9-]+`)
	}
	selected.WriteString(`([a-z]+)`)
	selected.WriteString(`[a-z0-9-]*`)
	var b strings.Builder
	b.WriteString(`(?i)^(?:[a-z0-9-]+\.)*`)
	b.WriteString(selected.String())
	b.WriteString(`(?:\.[a-z0-9-]+){`)
	b.WriteString(fmt.Sprint(e.label - 1))
	b.WriteString(`}`)
	b.WriteString(`\.`)
	b.WriteString(regexp.QuoteMeta(domain))
	b.WriteByte('$')
	return b.String()
}

func resolveToken(token string, dict Dictionaries, dictionary string) (Place, bool) {
	p, ok := dict[dictionary][token]
	return p, ok && p.Country != "" && p.City != ""
}

// MaxRank is how many extractions the learner keeps per domain: rank 1 is the best on
// all names, rank 2 the best on the names rank 1 does not decode, rank 3 likewise.
const MaxRank = 3

func learnDomain(domain string, names []Name, dict Dictionaries) (Candidate, bool) {
	return learnDomainCodes(domain, names, dict, nil)
}

// learnDomainCodes returns the rank 1 candidate of a domain.
func learnDomainCodes(domain string, names []Name, dict Dictionaries, overrides map[string]Code) (Candidate, bool) {
	ranked := learnDomainRanks(domain, names, dict, overrides)
	if len(ranked) == 0 {
		return Candidate{}, false
	}
	return ranked[0], true
}

// learnDomainRanks learns up to MaxRank extractions of a domain. Each one meets the
// support floor on the names that no better-ranked extraction of the domain decodes
// (Candidate.Names counts those names); the regex of every rank is still checked against
// all the domain's names, because locate applies it to any name of the domain.
func learnDomainRanks(domain string, names []Name, dict Dictionaries, overrides map[string]Code) []Candidate {
	unique := map[string]Name{}
	for _, n := range names {
		if n.PTR == "" || n.IP == "" || !cleanLabels(n.PTR, domain) {
			continue
		}
		u := n.IP
		if _, ok := unique[u]; !ok {
			unique[u] = n
		}
	}
	ordered := make([]Name, 0, len(unique))
	for _, n := range unique {
		ordered = append(ordered, n)
	}
	slices.SortFunc(ordered, func(a, b Name) int { return strings.Compare(a.IP, b.IP) })
	maxLabels, maxTokens := 0, 0
	for _, n := range ordered {
		ls := labelsBeforeDomain(n.PTR, domain)
		if len(ls) > maxLabels {
			maxLabels = len(ls)
		}
		for _, l := range ls {
			if x := len(letterTokens(l)); x > maxTokens {
				maxTokens = x
			}
		}
	}
	var out []Candidate
	taken := map[string]bool{} // names (lower case) some better rank already decodes
	for rank := 1; rank <= MaxRank; rank++ {
		active := ordered
		if len(taken) > 0 {
			active = make([]Name, 0, len(ordered))
			for _, n := range ordered {
				if !taken[ptrKey(n.PTR)] {
					active = append(active, n)
				}
			}
		}
		if len(active) < MinSupport {
			break
		}
		best, decodedPTRs, found := bestExtraction(domain, ordered, active, maxLabels, maxTokens, dict, overrides)
		if !found {
			break
		}
		best.Rank = rank
		best.Version = version(best.Pattern, best.Dictionary, best.Places)
		out = append(out, best)
		for k := range decodedPTRs {
			taken[k] = true
		}
	}
	return out
}

func ptrKey(ptr string) string { return strings.ToLower(strings.TrimSuffix(ptr, ".")) }

// bestExtraction tries every (label, token, dictionary) combination on the active names
// and returns the best supported one with the set of names it decodes. all is every name
// of the domain, which the generated regex must agree with.
func bestExtraction(domain string, all, active []Name, maxLabels, maxTokens int, dict Dictionaries, overrides map[string]Code) (Candidate, map[string]bool, bool) {
	var best Candidate
	var bestDecoded map[string]bool
	found := false
	for label := 1; label <= maxLabels; label++ {
		for token := 1; token <= maxTokens; token++ {
			for _, dn := range []string{"iata", "locode", "geonames"} {
				e := extraction{label: label, token: token, dictionary: dn}
				places := map[string]Place{}
				decoded, knownCountries, agrees, distinct := 0, 0, 0, map[string]bool{}
				decodedPTRs := map[string]bool{}
				for _, n := range active {
					code, ok := extract(n.PTR, domain, e)
					if !ok {
						continue
					}
					place, ok := Place{}, false
					if !roleTokens[code] {
						place, ok = resolveToken(code, dict, dn)
					}
					if override, exists := overrides[code]; exists && override.Scope == (Scope{LabelFromRight: label, TokenFromLeft: token, Dictionary: dn}) && (override.Origin == "manual" || override.Origin == "llm") {
						place = Place{override.Country, override.City}
						ok = place.Country != "" && place.City != ""
					}
					if !ok {
						continue
					}
					decoded++
					decodedPTRs[ptrKey(n.PTR)] = true
					places[code] = place
					distinct[place.Country+"\x00"+place.City] = true
					if n.Country != "" {
						knownCountries++
						if strings.EqualFold(n.Country, place.Country) {
							agrees++
						}
					}
				}
				if decoded < MinSupport || len(decodedPTRs) < MinSupport {
					continue
				}
				var agree *float64
				if knownCountries > 0 {
					x := float64(agrees) / float64(knownCountries)
					agree = &x
				}
				c := Candidate{Domain: domain, Pattern: extractionPattern(domain, e), Dictionary: dn, Scope: Scope{LabelFromRight: label, TokenFromLeft: token, Dictionary: dn}, Names: len(active), Decoded: decoded, Distinct: len(distinct), CountryAgree: agree, Places: places}
				if !patternAgrees(c.Pattern, all, domain, e) {
					continue
				}
				if !found || better(c, best) {
					best, bestDecoded, found = c, decodedPTRs, true
				}
			}
		}
	}
	return best, bestDecoded, found
}

func better(a, b Candidate) bool {
	// A token that is the same in every decoded name is usually a router role,
	// not a place: combinations with two or more token values come first.
	if am, bm := len(a.Places) >= 2, len(b.Places) >= 2; am != bm {
		return am
	}
	if a.Decoded != b.Decoded {
		return a.Decoded > b.Decoded
	}
	if a.Distinct != b.Distinct {
		return a.Distinct > b.Distinct
	}
	if a.Scope.LabelFromRight != b.Scope.LabelFromRight {
		return a.Scope.LabelFromRight < b.Scope.LabelFromRight
	}
	if a.Scope.TokenFromLeft != b.Scope.TokenFromLeft {
		return a.Scope.TokenFromLeft < b.Scope.TokenFromLeft
	}
	return a.Dictionary < b.Dictionary
}

func patternAgrees(pattern string, names []Name, domain string, e extraction) bool {
	r, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	for _, n := range names {
		want, ok := extract(n.PTR, domain, e)
		m := r.FindStringSubmatch(strings.ToLower(strings.TrimSuffix(n.PTR, ".")))
		if ok != (len(m) == 2) {
			return false
		}
		if ok && m[1] != want {
			return false
		}
	}
	return true
}

func version(pattern, dictionary string, places map[string]Place) string {
	b, _ := json.Marshal(struct {
		Pattern, Dictionary string
		Places              map[string]Place
	}{pattern, dictionary, places})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func asciiToken(s string) string {
	var b strings.Builder
	s = strings.ToLower(s)
	s = strings.NewReplacer("æ", "ae", "œ", "oe", "ø", "o", "ł", "l", "ß", "ss", "đ", "d", "ð", "d", "þ", "th", "ı", "i", "ħ", "h").Replace(s)
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if unicode.IsLetter(r) && r <= unicode.MaxASCII {
			b.WriteRune(r)
		}
	}
	return b.String()
}
