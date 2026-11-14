package rdnsrules

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// llmCode is the model's reading of one token.
type llmCode struct {
	Country string `json:"country"`
	Region  string `json:"region"`
	City    string `json:"city"`
}

// llmAnswer is one domain's answer.
type llmAnswer struct {
	Location bool               `json:"location"`
	Regex    string             `json:"regex"`
	Codes    map[string]llmCode `json:"codes"`
	Comment  string             `json:"comment"`
}

const placeholderText = `Digits were removed from the names before you got them. In a name pattern the placeholder 0 stands for a
run of decimal digits (regex [0-9]+) and the placeholder x for a run of hexadecimal characters (regex [0-9a-f]+). These
runs are addresses, counters and interface numbers, never a city code. Your regex is tested on the REAL names, where each
0 is a number and each x is a hex string, so write it to match both: for example the pattern "pool-0-0-0-0.nyc.example.net"
needs "pool-[0-9]+-[0-9]+-[0-9]+-[0-9]+" in the regex. Never put a placeholder in a code.`

const answerRules = `Rules:
- Give a code only when you are confident of the city; leave out tokens you cannot decode.
- Router roles and interface names (core, edge, gw, ae1, xe-0-0-0, ...) are not places.
- The capture group takes letters only ([a-z]+).
- The regex must be anchored: start with ^ and end with \.%s$ .
- For each city give its region (the first-level subdivision: state, province, England, ...), so the city can be found in the GeoNames database.
- If the names hold no city at all, answer {"location": false, "regex": "", "codes": {}, "comment": "..."}.
- Decode every distinct city token you see in the names below, not only a few.`

const answerShape = `{"location": true,
  "regex": "a Go RE2 regex, case-insensitive, matching a whole name, with exactly ONE capture group: the city token",
  "codes": {"<token as captured, lowercase>": {"country": "ISO 3166-1 alpha-2", "region": "first-level subdivision", "city": "English city name"}},
  "comment": "one short sentence"}`

func escapedDomain(domain string) string { return strings.ReplaceAll(domain, ".", `\.`) }

// singlePrompt asks about one domain with 5 or more hosts.
func singlePrompt(domain string, patterns []string, hosts int) string {
	return fmt.Sprintf(`You get reverse DNS (PTR) name patterns of internet routers of one operator, domain %s.
Find the part of the name that encodes the router's CITY (an airport code like "fra", a UN/LOCODE like
"defra", a CLLI like "nycmny", an abbreviation or a full city name). Answer ONLY with JSON:

%s

%s

`+answerRules+`

Name patterns (%d distinct, of %d hosts):
%s`, domain, answerShape, placeholderText, escapedDomain(domain), len(patterns), hosts, strings.Join(patterns, "\n"))
}

// packPrompt asks about several small domains (of one country) at once.
func packPrompt(domains []string, patterns map[string][]string) string {
	var b strings.Builder
	for _, d := range domains {
		fmt.Fprintf(&b, "=== domain %s (%d patterns)\n%s\n", d, len(patterns[d]), strings.Join(patterns[d], "\n"))
	}
	return fmt.Sprintf(`You get reverse DNS (PTR) name patterns of internet routers of several small operators, each with
its own domain, listed below. For each domain find the part of the name that encodes the router's CITY (an airport
code like "fra", a UN/LOCODE like "defra", a CLLI like "nycmny", an abbreviation or a full city name). Treat every
domain on its own; names of one domain say nothing about another. Answer ONLY with JSON, one object per domain:

{"domains": {"<domain>": %s}}

%s

`+strings.ReplaceAll(answerRules, `\.%s$`, `\.<domain>$`)+`

%s`, answerShape, placeholderText, b.String())
}

// secondPrompt asks the second model what the disputed tokens stand for, without telling it
// either city that is in dispute.
func secondPrompt(domain string, items []secondItem) string {
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "=== token %q, regex %s\n%s\n", it.Token, it.Regex, strings.Join(it.Examples, "\n"))
	}
	return fmt.Sprintf(`Reverse DNS (PTR) names of the internet routers of one operator, domain %s, hold a city code. For each
token below, tell which city it stands for in the names of this operator. The regex shows where the token sits in the
name; example name patterns follow it (%s). Answer ONLY with JSON:

{"tokens": {"<token>": {"country": "ISO 3166-1 alpha-2", "region": "first-level subdivision", "city": "English city name"}}}

Use null for a token you cannot decode with confidence. A wrong city is worse than null.

%s`, domain, strings.ReplaceAll(placeholderText, "\n", " "), b.String())
}

type secondItem struct {
	Token    string
	Regex    string
	Examples []string
}

// jsonObject returns the text from the first { to the last } (models wrap JSON in prose or fences).
func jsonObject(text string) string {
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j < i {
		return ""
	}
	return text[i : j+1]
}

// parseAnswer reads one domain's answer. Entries of codes that are not objects are dropped.
func parseAnswer(raw json.RawMessage) (llmAnswer, bool) {
	var shell struct {
		Location bool                       `json:"location"`
		Regex    string                     `json:"regex"`
		Codes    map[string]json.RawMessage `json:"codes"`
		Comment  string                     `json:"comment"`
	}
	if json.Unmarshal(raw, &shell) != nil {
		return llmAnswer{}, false
	}
	a := llmAnswer{Location: shell.Location, Regex: strings.TrimSpace(shell.Regex), Comment: shell.Comment, Codes: map[string]llmCode{}}
	for token, r := range shell.Codes {
		var c llmCode
		if json.Unmarshal(r, &c) == nil {
			a.Codes[token] = c // keys are normalized by checkAnswer
		}
	}
	return a, true
}

// parseSingle reads the answer of a one-domain request.
func parseSingle(text string) (llmAnswer, bool) {
	obj := jsonObject(text)
	if obj == "" {
		return llmAnswer{}, false
	}
	return parseAnswer(json.RawMessage(obj))
}

// parsePack reads the answers of a packed request: {"domains": {"a.net": {...}}}. A domain
// that is missing or malformed is missing from the result.
func parsePack(text string) map[string]llmAnswer {
	obj := jsonObject(text)
	out := map[string]llmAnswer{}
	if obj == "" {
		return out
	}
	var shell struct {
		Domains map[string]json.RawMessage `json:"domains"`
	}
	if json.Unmarshal([]byte(obj), &shell) != nil {
		return out
	}
	for d, raw := range shell.Domains {
		if a, ok := parseAnswer(raw); ok {
			out[strings.ToLower(strings.TrimSpace(d))] = a
		}
	}
	return out
}

// parseSecond reads the second opinion: token -> city (a null token is missing).
func parseSecond(text string) map[string]llmCode {
	obj := jsonObject(text)
	out := map[string]llmCode{}
	if obj == "" {
		return out
	}
	var shell struct {
		Tokens map[string]json.RawMessage `json:"tokens"`
	}
	if json.Unmarshal([]byte(obj), &shell) != nil {
		return out
	}
	// Two keys that fold to the same token: the first in sorted order wins.
	for _, t := range sortedKeys(shell.Tokens) {
		var c llmCode
		k := strings.ToLower(strings.TrimSpace(t))
		if _, dup := out[k]; dup || k == "" {
			continue
		}
		if json.Unmarshal(shell.Tokens[t], &c) == nil && c.City != "" {
			out[k] = c
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
