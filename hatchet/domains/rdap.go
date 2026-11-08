package domains

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

// rdapOptions configure the RDAP lookups. ARIN answers for its own ASNs and
// redirects queries for ASNs held by the other registries.
type rdapOptions struct {
	baseURL        string
	userAgent      string
	timeout        time.Duration
	delay          time.Duration // between lookups
	rateLimitSleep time.Duration
	maxRateLimits  int // per lookup, before it counts as failed
}

func defaultRDAPOptions() rdapOptions {
	return rdapOptions{
		baseURL:        "https://rdap.arin.net/registry/autnum/",
		userAgent:      "openipdata-worker/1.0",
		timeout:        20 * time.Second,
		delay:          200 * time.Millisecond,
		rateLimitSleep: 15 * time.Second,
		maxRateLimits:  5,
	}
}

type lookupStatus int

const (
	statusFound lookupStatus = iota
	statusNotFound
	statusFailed // transient: try again next run
)

type lookupResult struct {
	status   lookupStatus
	domain   string
	rejected int // candidates skipped because they were on the rejection list
}

// lookupRDAP fetches the autnum record for asn and picks its domain.
func lookupRDAP(ctx context.Context, client *http.Client, opts rdapOptions, asn int64, log func(string)) lookupResult {
	endpoint := fmt.Sprintf("%s%d", opts.baseURL, asn)
	rateLimits := 0
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return lookupResult{status: statusFailed}
		}
		req.Header.Set("User-Agent", opts.userAgent)
		req.Header.Set("Accept", "application/rdap+json")
		resp, err := client.Do(req)
		if err != nil {
			return lookupResult{status: statusFailed}
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			resp.Body.Close()
			rateLimits++
			if rateLimits > opts.maxRateLimits {
				return lookupResult{status: statusFailed}
			}
			log(fmt.Sprintf("rdap: rate limited; sleeping %s", opts.rateLimitSleep))
			if err := sleep(ctx, opts.rateLimitSleep); err != nil {
				return lookupResult{status: statusFailed}
			}
			continue
		case resp.StatusCode == http.StatusNotFound:
			resp.Body.Close()
			return lookupResult{status: statusNotFound}
		case resp.StatusCode < 200 || resp.StatusCode > 299:
			resp.Body.Close()
			return lookupResult{status: statusFailed}
		}
		var record any
		err = json.NewDecoder(resp.Body).Decode(&record)
		resp.Body.Close()
		if err != nil {
			return lookupResult{status: statusFailed}
		}
		domain, rejected := domainFromRDAP(record)
		if domain == "" {
			return lookupResult{status: statusNotFound, rejected: rejected}
		}
		return lookupResult{status: statusFound, domain: domain, rejected: rejected}
	}
}

// rolePriority orders contacts from most to least likely to belong to the ASN holder.
var rolePriority = []string{"registrant", "administrative", "technical", "abuse"}

type rdapEntity struct {
	roles      []string
	candidates []string // registered domains from the vCard: url first, then emails
}

// domainFromRDAP returns the first acceptable domain among the record's contacts,
// taken in role order, and how many rejected candidates it skipped.
func domainFromRDAP(record any) (string, int) {
	entities := collectEntities(record)
	rejectedCount := 0
	seen := map[*rdapEntity]bool{}
	try := func(e *rdapEntity) string {
		if seen[e] {
			return ""
		}
		seen[e] = true
		for _, d := range e.candidates {
			if Rejected(d) {
				rejectedCount++
				continue
			}
			return d
		}
		return ""
	}
	for _, role := range rolePriority {
		for _, e := range entities {
			if hasRole(e, role) {
				if d := try(e); d != "" {
					return d, rejectedCount
				}
			}
		}
	}
	for _, e := range entities { // contacts with other or no roles, e.g. "noc"
		if d := try(e); d != "" {
			return d, rejectedCount
		}
	}
	return "", rejectedCount
}

// collectEntities walks the record's entities breadth-first, nested ones included.
func collectEntities(record any) []*rdapEntity {
	var out []*rdapEntity
	queue := []any{record}
	for len(queue) > 0 {
		current, ok := queue[0].(map[string]any)
		queue = queue[1:]
		if !ok {
			continue
		}
		if children, ok := current["entities"].([]any); ok {
			for _, child := range children {
				obj, ok := child.(map[string]any)
				if !ok {
					continue
				}
				out = append(out, &rdapEntity{roles: stringList(obj["roles"]), candidates: vcardCandidates(obj)})
				queue = append(queue, obj)
			}
		}
	}
	return out
}

// vcardCandidates returns registered domains from a vCard's url, then its emails.
func vcardCandidates(entity map[string]any) []string {
	vcard, ok := entity["vcardArray"].([]any)
	if !ok || len(vcard) < 2 {
		return nil
	}
	props, ok := vcard[1].([]any)
	if !ok {
		return nil
	}
	var urls, emails []string
	for _, p := range props {
		prop, ok := p.([]any)
		if !ok || len(prop) < 4 {
			continue
		}
		name, _ := prop[0].(string)
		for _, value := range stringList(prop[3]) {
			switch name {
			case "url":
				if d := domainFromURL(value); d != "" {
					urls = append(urls, d)
				}
			case "email":
				if d := domainFromEmail(value); d != "" {
					emails = append(emails, d)
				}
			}
		}
	}
	return append(urls, emails...)
}

func domainFromURL(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return Normalize(u.Hostname())
}

func domainFromEmail(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "mailto:") {
		value = value[len("mailto:"):]
	}
	address := value
	if parsed, err := mail.ParseAddress(value); err == nil && parsed.Address != "" {
		address = parsed.Address
	}
	_, domain, ok := strings.Cut(address, "@")
	if !ok {
		return ""
	}
	return Normalize(domain)
}

func hasRole(e *rdapEntity, role string) bool {
	for _, r := range e.roles {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}

// stringList accepts a JSON string or a list of strings.
func stringList(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
