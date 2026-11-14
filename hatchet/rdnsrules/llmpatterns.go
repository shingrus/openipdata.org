package rdnsrules

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
)

// maxPatternsPerDomain is how many distinct name patterns of a domain are sent (most frequent first).
const maxPatternsPerDomain = 300

func isHexDigit(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }
func isDigit(c byte) bool    { return c >= '0' && c <= '9' }

// redactLabel turns a label into its pattern, in this order:
//  1. a run of [0-9a-f] that is 6+ characters long and has a digit, or exactly 7 or 8 characters long
//     (an IPv4 address in hex, also without a digit; 7 when the leading zero is dropped), becomes `x`;
//  2. every remaining run of digits becomes `0`.
//
// So no octet of an address survives in decimal or in hex, whatever the encoding.
func redactLabel(label string) string {
	var b strings.Builder
	for i := 0; i < len(label); {
		if !isHexDigit(label[i]) {
			b.WriteByte(label[i])
			i++
			continue
		}
		j, digit := i, false
		for j < len(label) && isHexDigit(label[j]) {
			digit = digit || isDigit(label[j])
			j++
		}
		if n := j - i; (n >= 6 && digit) || n == 7 || n == 8 {
			b.WriteByte('x')
		} else {
			b.WriteString(label[i:j])
		}
		i = j
	}
	step1 := b.String()
	b.Reset()
	for i := 0; i < len(step1); {
		if !isDigit(step1[i]) {
			b.WriteByte(step1[i])
			i++
			continue
		}
		for i < len(step1) && isDigit(step1[i]) {
			i++
		}
		b.WriteByte('0')
	}
	return b.String()
}

// patternOf redacts the labels left of the registered domain; the domain itself is public
// text and stays as it is. It returns "" for a name that is not below the domain.
func patternOf(ptr, domain string) string {
	labels := labelsBeforeDomain(ptr, domain)
	if len(labels) == 0 {
		return ""
	}
	for i, l := range labels {
		labels[i] = redactLabel(l)
	}
	return strings.Join(labels, ".") + "." + domain
}

// topPatterns returns the distinct patterns of the names (one count per name), the most
// frequent first (ties alphabetical), at most limit.
func topPatterns(ptrs []string, domain string, limit int) []string {
	count := map[string]int{}
	for _, p := range ptrs {
		if pat := patternOf(p, domain); pat != "" {
			count[pat]++
		}
	}
	out := make([]string, 0, len(count))
	for p := range count {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if count[out[i]] != count[out[j]] {
			return count[out[i]] > count[out[j]]
		}
		return out[i] < out[j]
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// patternsHash identifies a set of patterns, whatever their order.
func patternsHash(patterns []string) string {
	sorted := slices.Clone(patterns)
	slices.Sort(sorted)
	h := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(h[:])
}
