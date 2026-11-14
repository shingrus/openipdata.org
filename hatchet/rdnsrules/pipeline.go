package rdnsrules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"openipdata.org/hatchet/download"
	"openipdata.org/hatchet/ipdata"
	"openipdata.org/hatchet/schema"
)

// inputQuery reads the PTR names of every IP of the traces of the last 180 days that
// has a successful name: the target, the candidates and every public answering hop
// (network_traces.hop_ips), each IP once. Target names are included on purpose: customer
// pool names carry a city too.
const inputQuery = `SELECT DISTINCT host(n.ip), n.ptr, ''::text
	FROM public.network_traces t
	CROSS JOIN LATERAL unnest(COALESCE(t.candidates, '{}'::inet[]) || COALESCE(t.hop_ips, '{}'::inet[]) || t.target) AS c(ip)
	JOIN public.ip_ptr_names n ON n.ip = c.ip AND n.status = 'ok' AND n.ptr IS NOT NULL
	WHERE t.traced_at >= now() - interval '180 days'
	  AND n.ptr <> ''
	ORDER BY host(n.ip)`

// inputQueryNoHops is inputQuery for a database where resolve-ips has not added
// network_traces.hop_ips yet (the learner never runs DDL on a table it does not own).
const inputQueryNoHops = `SELECT DISTINCT host(n.ip), n.ptr, ''::text
	FROM public.network_traces t
	CROSS JOIN LATERAL unnest(COALESCE(t.candidates, '{}'::inet[]) || t.target) AS c(ip)
	JOIN public.ip_ptr_names n ON n.ip = c.ip AND n.status = 'ok' AND n.ptr IS NOT NULL
	WHERE t.traced_at >= now() - interval '180 days'
	  AND n.ptr <> ''
	ORDER BY host(n.ip)`

const hopIPsColumnQuery = `SELECT EXISTS (SELECT 1 FROM information_schema.columns
	WHERE table_schema = 'public' AND table_name = 'network_traces' AND column_name = 'hop_ips')`

type Pipeline struct {
	DataDir  string
	PGSQL    string
	Progress func(string)
	// propose-codes: OpenRouter key and models (env settings read in main.go). The key is
	// never logged, returned or stored.
	OpenRouterKey         string
	OpenRouterModel       string
	OpenRouterSecondModel string
	// Tests may replace endpoints, transport, and the clock.
	OpenRouterURL string
	llmHTTP       *http.Client
	llmBackoff    time.Duration
	testGeo       *geoIndex
	client        *http.Client
	now           func() time.Time
	dictionaries  Dictionaries
	load          func(context.Context) (Dictionaries, error)
}

type Output struct {
	Domains         int     `json:"domains"`
	Candidates      int     `json:"candidates"`  // rule rows learned (up to 3 per domain)
	ExtraRanks      int     `json:"extra_ranks"` // of them rank 2 or 3
	Updated         int     `json:"updated"`
	Unchanged       int     `json:"unchanged"`
	Unsupported     int     `json:"unsupported"`   // observed domains that did not reach the support floor
	LostSupport     int     `json:"lost_support"`  // stored rules whose domain has no current names (kept as they are)
	NamesSkipped    int     `json:"names_skipped"` // names with a label outside [a-z0-9-], never learned from
	Retired         int     `json:"retired"`       // learned rows of domains with current names that this run no longer reproduces (now rejected)
	Rejected        int     `json:"rejected"`
	DurationSeconds float64 `json:"duration_seconds"`
}

func (p *Pipeline) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}
func (p *Pipeline) log(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

func (p *Pipeline) CheckPGSQL() error {
	if p.PGSQL == "" {
		return errors.New("PGSQL is not set")
	}
	return nil
}

func safeDBError(op string, err error) error {
	if errors.Is(err, schema.ErrOldRuleLayout) {
		return err // a fixed message that names no data
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: Postgres error %s", op, pe.Code)
	}
	return fmt.Errorf("%s failed", op)
}

// Learn downloads and parses every dictionary before it touches the rules table.
// A failed refresh therefore leaves all published rule rows intact.
func (p *Pipeline) Learn(ctx context.Context) (Output, error) {
	started := time.Now()
	out := Output{}
	if err := p.CheckPGSQL(); err != nil {
		return out, err
	}
	var dict Dictionaries
	var err error
	if p.dictionaries != nil {
		dict = p.dictionaries
	} else if p.load != nil {
		dict, err = p.load(ctx)
	} else {
		dict, err = loadDictionaries(ctx, filepath.Join(p.DataDir, "rdnsrules", "dictionaries"), p.client, p.clock().UTC())
	}
	if err != nil {
		return out, fmt.Errorf("refresh dictionaries: %s", safeSourceError(err))
	}
	conn, err := pgx.Connect(ctx, p.PGSQL)
	if err != nil {
		return out, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	defer conn.Close(ctx)
	if err = schema.EnsureRDNSCityRules(ctx, conn); err != nil {
		return out, safeDBError("ensure city rules schema", err)
	}
	// Scoped manual/LLM overrides of all of a domain's rows: an extraction can move to
	// another rank between runs, so each learned row gets the union (first rank wins a token).
	codeRows, err := conn.Query(ctx, `SELECT domain,codes FROM public.rdns_city_rules WHERE rank <= 3 ORDER BY domain,rank`)
	if err != nil {
		return out, safeDBError("read scoped city overrides", err)
	}
	existingCodes := map[string]map[string]Code{}
	storedRows := map[string]int{} // rule rows of each domain
	for codeRows.Next() {
		var domain string
		var codes map[string]Code
		if err = codeRows.Scan(&domain, &codes); err != nil {
			codeRows.Close()
			return out, safeDBError("read scoped city overrides", err)
		}
		storedRows[domain]++
		merged := existingCodes[domain]
		if merged == nil {
			merged = map[string]Code{}
			existingCodes[domain] = merged
		}
		for token, code := range codes {
			if _, taken := merged[token]; !taken {
				merged[token] = code
			}
		}
	}
	if err = codeRows.Err(); err != nil {
		codeRows.Close()
		return out, safeDBError("read scoped city overrides", err)
	}
	codeRows.Close()
	byDomain, namesSkipped, err := readDomainNames(ctx, conn)
	if err != nil {
		return out, err
	}
	out.NamesSkipped = namesSkipped
	// Rules with no current router evidence remain untouched. Count them as
	// unsupported so a loss of support is visible in the count-only report.
	for domain, n := range storedRows {
		if _, observed := byDomain[domain]; !observed {
			out.LostSupport += n
		}
	}
	// Country is diagnostic only. Map each router IP through current announced
	// prefixes; failures are fatal before the write transaction begins.
	if err = loadRouterCountries(ctx, conn, byDomain); err != nil {
		return out, safeDBError("read router countries", err)
	}
	out.Domains = len(byDomain)
	candidates := make([]Candidate, 0, len(byDomain))
	domains := make([]string, 0, len(byDomain))
	for d := range byDomain {
		domains = append(domains, d)
	}
	slices.Sort(domains)
	for _, d := range domains {
		if ranked := learnDomainRanks(d, byDomain[d], dict, existingCodes[d]); len(ranked) > 0 {
			candidates = append(candidates, ranked...)
			out.ExtraRanks += len(ranked) - 1
		} else {
			out.Unsupported++
		}
	}
	out.Candidates = len(candidates)
	observed := make(map[string]bool, len(byDomain))
	for d := range byDomain {
		observed[d] = true
	}
	if err = storeRules(ctx, conn, candidates, observed, &out); err != nil {
		return out, err
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("learn-rdns-rules: %d domains, %d rules (%d of rank 2 or 3), %d updated, %d unchanged, %d unsupported, %d lost support, %d retired, %d names skipped", out.Domains, out.Candidates, out.ExtraRanks, out.Updated, out.Unchanged, out.Unsupported, out.LostSupport, out.Retired, out.NamesSkipped)
	return out, nil
}

// readDomainNames reads the PTR names of the traced IPs of the last 180 days, grouped by
// registered domain (public IPv4 only, labels [a-z0-9-] only; the second result counts the
// names skipped for another label). Countries are not set yet (loadRouterCountries).
func readDomainNames(ctx context.Context, conn *pgx.Conn) (map[string][]Name, int, error) {
	skipped := 0
	var hasHops bool
	if err := conn.QueryRow(ctx, hopIPsColumnQuery).Scan(&hasHops); err != nil {
		return nil, 0, safeDBError("read network_traces columns", err)
	}
	query := inputQueryNoHops
	if hasHops {
		query = inputQuery
	}
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, 0, safeDBError("read recent router names", err)
	}
	byDomain := map[string][]Name{}
	for rows.Next() {
		var ip, ptr, country string
		if err = rows.Scan(&ip, &ptr, &country); err != nil {
			rows.Close()
			return nil, 0, safeDBError("read recent router names", err)
		}
		a, addrErr := netip.ParseAddr(ip)
		if addrErr != nil || !ipdata.PublicV4(a) {
			continue
		}
		domain := registeredDomain(ptr)
		if domain == "" {
			continue
		}
		if !cleanLabels(ptr, domain) {
			skipped++
			continue
		}
		byDomain[domain] = append(byDomain[domain], Name{IP: ip, PTR: ptr, Country: country})
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, 0, safeDBError("read recent router names", err)
	}
	rows.Close()
	return byDomain, skipped, nil
}

// Reject withdraws the current rules for registered domains. It does not
// download dictionaries, and records every withdrawn version so it cannot be
// revived by a later identical learning result.
func (p *Pipeline) Reject(ctx context.Context, domains []string) (Output, error) {
	started := time.Now()
	out := Output{}
	var rejectedCount, unchangedCount int
	if err := p.CheckPGSQL(); err != nil {
		return out, err
	}
	clean := make([]string, 0, len(domains))
	seen := map[string]bool{}
	for _, d := range domains {
		d = normalize(d)
		if d == "" || registeredDomain(d) != d {
			return out, errors.New("invalid registered domain")
		}
		if !seen[d] {
			clean = append(clean, d)
			seen[d] = true
		}
	}
	slices.Sort(clean)
	conn, err := pgx.Connect(ctx, p.PGSQL)
	if err != nil {
		return out, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	defer conn.Close(ctx)
	if err = schema.EnsureRDNSCityRules(ctx, conn); err != nil {
		return out, safeDBError("ensure city rules schema", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return out, safeDBError("begin city rule rejection", err)
	}
	defer tx.Rollback(ctx)
	for _, domain := range clean {
		// Every rank of the domain is withdrawn, each version remembered in its own row.
		rows, qerr := tx.Query(ctx, `SELECT rank,version,status,rejected_versions FROM public.rdns_city_rules WHERE domain=$1 ORDER BY rank FOR UPDATE`, domain)
		if qerr != nil {
			return out, safeDBError("read city rule for rejection", qerr)
		}
		type row struct {
			rank            int16
			version, status string
			rejected        []string
		}
		var found []row
		for rows.Next() {
			var r row
			if err = rows.Scan(&r.rank, &r.version, &r.status, &r.rejected); err != nil {
				rows.Close()
				return out, safeDBError("read city rule for rejection", err)
			}
			found = append(found, r)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return out, safeDBError("read city rule for rejection", err)
		}
		changed := false
		for _, r := range found {
			if r.status == "rejected" {
				continue
			}
			rejected := appendUnique(r.rejected, r.version)
			if _, err = tx.Exec(ctx, `UPDATE public.rdns_city_rules SET status='rejected',rejected_versions=$3 WHERE domain=$1 AND rank=$2`, domain, r.rank, rejected); err != nil {
				return out, safeDBError("reject city rule", err)
			}
			changed = true
		}
		if changed {
			rejectedCount++
		} else {
			unchangedCount++
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return out, safeDBError("commit city rule rejection", err)
	}
	out.Domains = len(clean)
	out.Rejected = rejectedCount
	out.Unchanged = unchangedCount
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("reject-rdns-rules: %d domains, %d rejected, %d unchanged", out.Domains, out.Rejected, out.Unchanged)
	return out, nil
}

func loadRouterCountries(ctx context.Context, conn *pgx.Conn, groups map[string][]Name) error {
	byBits := [33]map[uint32]string{}
	add := func(asn int64, country *string, texts []string) {
		if country == nil {
			return
		}
		for _, raw := range texts {
			if p, e := netip.ParsePrefix(strings.TrimSpace(raw)); e == nil && p.Addr().Is4() {
				p = p.Masked()
				bits := p.Bits()
				if byBits[bits] == nil {
					byBits[bits] = map[uint32]string{}
				}
				b := p.Addr().As4()
				key := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
				if _, ok := byBits[bits][key]; !ok {
					byBits[bits][key] = strings.ToUpper(*country)
				}
			}
		}
	}
	reset := func() { byBits = [33]map[uint32]string{} }
	if err := ipdata.ReadAnnounced(ctx, conn, reset, add); err != nil {
		return err
	}
	for domain, ns := range groups {
		for i := range ns {
			a, e := netip.ParseAddr(ns[i].IP)
			if e != nil {
				continue
			}
			a = a.Unmap()
			if !a.Is4() {
				continue
			}
			b := a.As4()
			addr := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
			for bits := 32; bits >= 8; bits-- {
				mask := uint32(0xffffffff)
				if bits < 32 {
					mask <<= 32 - bits
				}
				if country, ok := byBits[bits][addr&mask]; ok {
					ns[i].Country = country
					break
				}
			}
		}
		groups[domain] = ns
	}
	return nil
}

func safeSourceError(err error) string {
	var se *download.StatusError
	if errors.As(err, &se) {
		return fmt.Sprintf("HTTP %d", se.Code)
	}
	return "source unavailable or invalid"
}

type storedRule struct {
	Rank                   int
	Version                string
	Status                 string
	Origin                 string
	Pattern                string
	Dictionary             string
	Codes                  map[string]Code
	Rejected               []string
	TruthHits, TruthMisses *int64
	Precision              *float64
	MeasuredAt             *time.Time
	LearnedAt              time.Time
}

const readDomainRulesSQL = `SELECT rank,version,status,origin,pattern,dictionary,codes,rejected_versions,truth_hits,truth_misses,precision,measured_at,learned_at
	FROM public.rdns_city_rules WHERE domain=$1 AND rank <= 3 ORDER BY rank FOR UPDATE`

func readDomainRules(ctx context.Context, tx pgx.Tx, domain string) ([]storedRule, error) {
	rows, err := tx.Query(ctx, readDomainRulesSQL, domain)
	if err != nil {
		return nil, safeDBError("read existing city rule", err)
	}
	defer rows.Close()
	var out []storedRule
	for rows.Next() {
		old := storedRule{Codes: map[string]Code{}}
		var rank int16
		if err = rows.Scan(&rank, &old.Version, &old.Status, &old.Origin, &old.Pattern, &old.Dictionary, &old.Codes, &old.Rejected, &old.TruthHits, &old.TruthMisses, &old.Precision, &old.MeasuredAt, &old.LearnedAt); err != nil {
			return nil, safeDBError("read existing city rule", err)
		}
		old.Rank = int(rank)
		out = append(out, old)
	}
	if err = rows.Err(); err != nil {
		return nil, safeDBError("read existing city rule", err)
	}
	return out, nil
}

func storeCandidates(ctx context.Context, conn *pgx.Conn, candidates []Candidate, out *Output) error {
	return storeRules(ctx, conn, candidates, nil, out)
}

// storeRules writes the learned rows, one per (domain, rank), in one transaction.
// observed is the set of domains that have current names. A stored row of a domain that is
// not observed stays as it is (absence of names is not proof that a rule is wrong). For an
// observed domain, a stored `origin = learned` row at a rank this run did not write is
// retired: marked rejected, its version added to rejected_versions so it cannot come back
// unchanged (a row whose content moved to another rank is a stale duplicate and is deleted).
// Manual and LLM rows are never retired.
func storeRules(ctx context.Context, conn *pgx.Conn, candidates []Candidate, observed map[string]bool, out *Output) error {
	var updatedCount, unchangedCount, retiredCount int
	tx, err := conn.Begin(ctx)
	if err != nil {
		return safeDBError("begin city rules update", err)
	}
	defer tx.Rollback(ctx)
	byDomain := map[string][]Candidate{}
	var domains []string
	var retireOnly []string
	for d := range observed {
		retireOnly = append(retireOnly, d)
	}
	slices.Sort(retireOnly)
	for _, c := range candidates {
		if c.Rank == 0 {
			c.Rank = 1
		}
		if _, ok := byDomain[c.Domain]; !ok {
			domains = append(domains, c.Domain)
		}
		byDomain[c.Domain] = append(byDomain[c.Domain], c)
	}
	for _, d := range retireOnly {
		if _, ok := byDomain[d]; !ok {
			domains = append(domains, d)
		}
	}
	for _, domain := range domains {
		olds, err := readDomainRules(ctx, tx, domain)
		if err != nil {
			return err
		}
		// Manual and LLM overrides of any rank are kept with their scope and provenance, but
		// only those scoped to the extraction chosen now shape the places and the version.
		codes := map[string]Code{}
		// A withdrawn version stays withdrawn whatever rank it comes back at.
		var rejectedAll []string
		for _, old := range olds {
			for token, code := range old.Codes {
				if _, taken := codes[token]; !taken && (code.Origin == "manual" || code.Origin == "llm") {
					codes[token] = code
				}
			}
			for _, v := range old.Rejected {
				rejectedAll = appendUnique(rejectedAll, v)
			}
		}
		written := map[int]bool{}       // ranks written (or deliberately skipped) by this run
		reproduced := map[string]bool{} // versions this run reproduces at any rank
		for _, c := range byDomain[domain] {
			var old storedRule
			var haveOld bool
			places := map[string]Place{}
			for token, place := range c.Places {
				places[token] = place
			}
			for token, code := range codes {
				if code.Scope == c.Scope {
					places[token] = Place{code.Country, code.City}
				}
			}
			c.Places = places
			c.Version = version(c.Pattern, c.Dictionary, c.Places)
			if slicesContains(rejectedAll, c.Version) {
				unchangedCount++
				continue
			}
			written[c.Rank] = true
			reproduced[c.Version] = true
			// The row that already holds this content (the extraction may have moved to another
			// rank) keeps its status and measurements; else the row of the same rank is replaced.
			for _, o := range olds {
				if o.Version == c.Version {
					old, haveOld = o, true
					break
				}
			}
			if !haveOld {
				for _, o := range olds {
					if o.Rank == c.Rank {
						old, haveOld = o, true
						break
					}
				}
			}
			// Rows of this rank keep their own list of withdrawn versions.
			var rankRejected []string
			for _, o := range olds {
				if o.Rank == c.Rank {
					rankRejected = o.Rejected
				}
			}
			if rankRejected == nil {
				rankRejected = []string{}
			}
			status, origin := "candidate", "learned"
			var hits, misses, precision, measured any
			learnedAt := time.Now().UTC()
			if haveOld && old.Version == c.Version {
				status, origin = old.Status, old.Origin
				hits, misses, precision, measured = old.TruthHits, old.TruthMisses, old.Precision, old.MeasuredAt
				learnedAt = old.LearnedAt
				unchangedCount++
			} else {
				if haveOld && old.Status == "rejected" {
					rankRejected = appendUnique(rankRejected, old.Version)
				}
				updatedCount++
			}
			placesArray := make(map[string][2]string, len(c.Places))
			for token, place := range c.Places {
				placesArray[token] = [2]string{place.Country, place.City}
			}
			placesJSON, _ := json.Marshal(placesArray)
			codesJSON, _ := json.Marshal(codes)
			_, err = tx.Exec(ctx, `INSERT INTO public.rdns_city_rules(domain,rank,pattern,dictionary,codes,places,version,names,decoded,distinct_places,country_agree,truth_hits,truth_misses,precision,status,origin,learned_at,measured_at,rejected_versions)
				VALUES($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
			ON CONFLICT(domain,rank) DO UPDATE SET pattern=EXCLUDED.pattern,dictionary=EXCLUDED.dictionary,codes=EXCLUDED.codes,places=EXCLUDED.places,version=EXCLUDED.version,names=EXCLUDED.names,decoded=EXCLUDED.decoded,distinct_places=EXCLUDED.distinct_places,country_agree=EXCLUDED.country_agree,truth_hits=EXCLUDED.truth_hits,truth_misses=EXCLUDED.truth_misses,precision=EXCLUDED.precision,status=EXCLUDED.status,origin=EXCLUDED.origin,learned_at=EXCLUDED.learned_at,measured_at=EXCLUDED.measured_at,rejected_versions=EXCLUDED.rejected_versions`,
				c.Domain, c.Rank, c.Pattern, c.Dictionary, string(codesJSON), string(placesJSON), c.Version, c.Names, c.Decoded, c.Distinct, c.CountryAgree, hits, misses, precision, status, origin, learnedAt, measured, rankRejected)
			if err != nil {
				return safeDBError("write city rule", err)
			}
		}
		if !observed[domain] {
			continue
		}
		for _, o := range olds {
			if o.Origin != "learned" || o.Status == "rejected" || written[o.Rank] {
				continue
			}
			if reproduced[o.Version] {
				// Same content now lives at another rank: drop the stale duplicate.
				if _, err = tx.Exec(ctx, `DELETE FROM public.rdns_city_rules WHERE domain=$1 AND rank=$2`, domain, o.Rank); err != nil {
					return safeDBError("delete moved city rule", err)
				}
				continue
			}
			if _, err = tx.Exec(ctx, `UPDATE public.rdns_city_rules SET status='rejected',rejected_versions=$3 WHERE domain=$1 AND rank=$2`, domain, o.Rank, appendUnique(append([]string{}, o.Rejected...), o.Version)); err != nil {
				return safeDBError("retire city rule", err)
			}
			retiredCount++
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return safeDBError("commit city rules update", err)
	}
	out.Updated += updatedCount
	out.Unchanged += unchangedCount
	out.Retired += retiredCount
	return nil
}

func slicesContains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}
func appendUnique(a []string, s string) []string {
	if s == "" || slicesContains(a, s) {
		return a
	}
	return append(a, s)
}
