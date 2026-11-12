package rdns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
)

// Scopes of rdns_geo_layer rows.
const (
	scopeTraced = "traced" // a /24 that a trace located: layer 65-rdns
	scopeWide   = "wide"   // an announced prefix (at most /16) that traced /24s voted for: layer 35-rdns-wide
)

// maxShrink is the largest drop in row count a replace accepts (10%).
const maxShrink = 0.10

// ErrShrink is returned when the new rows are more than 10% fewer than the
// stored ones; run with force_replace after checking that the drop is real.
var ErrShrink = errors.New("rdns_geo_layer shrink")

// geoRow is one row of rdns_geo_layer.
type geoRow struct {
	Prefix   netip.Prefix
	Scope    string
	Country  string
	City     string
	RouterIP netip.Addr // traced rows only
	PTR      string     // the router's PTR name; traced rows only
	Rule     string     // the rule that matched it; traced rows only
	SameASN  *bool      // traced rows: nil when the target's ASN is unknown
}

// match is what one trace found: the first candidate, same-ASN ones first, whose
// PTR name a rule matches.
type match struct {
	Kind     string
	Rank     int // position among the candidates, closest to the target first: 0 is the target
	Router   netip.Addr
	PTR      string
	Rule     *rule
	SameASN  bool // the router is in the target's ASN
	ASNKnown bool // the target's origin ASN is known
}

// traceable reports whether the match may give the /24 its own (traced) row:
// the router is in the target's ASN, or the ASN is unknown so nothing is better.
func (m *match) traceable() bool { return m.SameASN || !m.ASNKnown }

func (m *match) sameCity(o *match) bool {
	return m.Rule.country == o.Rule.country && m.Rule.city == o.Rule.city
}

// KindStats says how well one target kind did.
type KindStats struct {
	Traces     int `json:"traces"`
	ReachedASN int `json:"reached_asn"` // the path has a router in the target's ASN
	Located    int `json:"located"`     // a rule matched a name on the path
	UniqueWins int `json:"unique_wins"` // located, and no other target of the /24 was
	Compared   int `json:"compared"`    // located, and another target of the /24 was too
	Agreed     int `json:"agreed"`      // of those, the city is the one the /24 got
}

// LocateOutput counts what locate read, found and stored.
type LocateOutput struct {
	Traces   int `json:"traces"`
	Networks int `json:"networks"` // /24s with a trace
	Names    int `json:"names"`    // PTR names read

	Rules        int  `json:"rules"`
	RulesSkipped int  `json:"rules_skipped"` // Go's regexp could not compile the Python pattern
	RulesInvalid int  `json:"rules_invalid"` // no domains, city or country
	RulesMissing bool `json:"rules_table_missing,omitempty"`
	ASNPrefixes  int  `json:"asn_prefixes"`

	Traced        int `json:"traced"`        // /24s located
	Wide          int `json:"wide"`          // announced prefixes located
	Votes         int `json:"votes"`         // /24s that voted for their announced prefix
	WideTies      int `json:"wide_ties"`     // announced prefixes skipped: two cities with the most votes
	Disagreements int `json:"disagreements"` // /24s whose targets named different cities (same-ASN ones)
	NoASN         int `json:"no_asn"`        // /24s whose target has no origin ASN

	Kinds map[string]*KindStats `json:"kinds"`

	Previous int `json:"previous"` // rows before the replace
	Stored   int `json:"stored"`
}

// matchTrace finds the first candidate of the trace, same-ASN ones first, whose
// PTR name a rule matches. reached is whether any router on the path (or the
// target) is in the target's ASN.
func matchTrace(t traceIn, ptrs map[netip.Addr]string, rules []rule, origin *originTable) (m *match, reached bool) {
	_, targetASN, known := origin.lookup(t.Target)
	if known {
		for _, h := range t.Hops {
			if a, ok := h.addr(); ok {
				if _, asn, ok := origin.lookup(a); ok && asn == targetASN {
					reached = true
					break
				}
			}
		}
	}
	cands := candidates(t.Target, t.Hops)
	order := make([]int, 0, len(cands))
	if known {
		// A router of the target's own network says more about the target than a
		// transit router named after a peering city.
		for _, same := range []bool{true, false} {
			for i, c := range cands {
				if _, asn, ok := origin.lookup(c); (ok && asn == targetASN) == same {
					order = append(order, i)
				}
			}
		}
	} else {
		for i := range cands {
			order = append(order, i)
		}
	}
	for _, i := range order {
		ptr := ptrs[cands[i]]
		if ptr == "" {
			continue
		}
		if r := firstMatch(rules, ptr); r != nil {
			_, asn, ok := origin.lookup(cands[i])
			return &match{Kind: t.Kind, Rank: i, Router: cands[i], PTR: ptr, Rule: r,
				SameASN: known && ok && asn == targetASN, ASNKnown: known}, reached
		}
	}
	return nil, reached
}

// best returns the match with the smallest rank, the earlier kind first on a tie.
func best(ms []*match) *match {
	var b *match
	for _, m := range ms {
		if b == nil || m.Rank < b.Rank || (m.Rank == b.Rank && kindIndex(m.Kind) < kindIndex(b.Kind)) {
			b = m
		}
	}
	return b
}

func kindIndex(kind string) int {
	for i, k := range kindOrder {
		if k == kind {
			return i
		}
	}
	return len(kindOrder)
}

// widePrefix is the prefix a /24's vote counts for: the longest announced
// prefix that contains the /24, and never wider than the /16 around it.
func widePrefix(origin *originTable, p24 netip.Prefix) (netip.Prefix, bool) {
	announced, _, ok := origin.containing(p24)
	if !ok {
		return netip.Prefix{}, false
	}
	if announced.Bits() < 16 {
		w, err := p24.Addr().Prefix(16)
		if err != nil {
			return netip.Prefix{}, false
		}
		return w, true
	}
	return announced, true
}

type cityKey struct{ Country, City string }

// locate turns the newest trace of every (/24, target kind), the PTR names and
// the rules into the rows of rdns_geo_layer.
//
// Per trace the candidates are the target and the last quarter of the hops, closest
// first; those in the target's ASN come first; the first whose name a rule
// matches is the trace's match. Per /24:
//   - a traced row when some match is in the target's ASN (or the ASN is unknown):
//     the one closest to the target wins, an earlier kind (visitor, gateway,
//     random) on a tie;
//   - one vote for the announced prefix of the /24 (at most /16): the traced
//     row's city, or else the closest match of a router in another ASN (a transit
//     router is named after the city it peers in). With no origin ASN there is
//     no announced prefix and no vote.
//
// Per announced prefix the city with the most votes gets a wide row; a tie gets none.
func locate(traces []traceIn, ptrs map[netip.Addr]string, rules []rule, origin *originTable) ([]geoRow, LocateOutput) {
	out := LocateOutput{Traces: len(traces), Names: len(ptrs), Rules: len(rules), Kinds: map[string]*KindStats{}}
	for _, k := range kindOrder {
		out.Kinds[k] = &KindStats{}
	}
	byNet := map[netip.Prefix][]traceIn{}
	for _, t := range traces {
		byNet[t.Prefix] = append(byNet[t.Prefix], t)
	}
	prefixes := make([]netip.Prefix, 0, len(byNet))
	for p := range byNet {
		prefixes = append(prefixes, p)
	}
	sort.Slice(prefixes, func(i, j int) bool { return prefixes[i].Addr().Less(prefixes[j].Addr()) })
	out.Networks = len(prefixes)

	var rows []geoRow
	votes := map[netip.Prefix]map[cityKey]int{}
	for _, p24 := range prefixes {
		var matches []*match
		noASN := false
		for _, t := range byNet[p24] {
			ks := out.Kinds[t.Kind]
			if ks == nil {
				ks = &KindStats{}
				out.Kinds[t.Kind] = ks
			}
			ks.Traces++
			m, reached := matchTrace(t, ptrs, rules, origin)
			if reached {
				ks.ReachedASN++
			}
			if _, _, ok := origin.lookup(t.Target); !ok {
				noASN = true
			}
			if m != nil {
				matches = append(matches, m)
			}
		}
		if noASN {
			out.NoASN++
		}

		var traceable, others []*match
		for _, m := range matches {
			if m.traceable() {
				traceable = append(traceable, m)
			} else {
				others = append(others, m)
			}
		}
		winner := best(traceable)
		for _, m := range traceable {
			if !m.sameCity(winner) {
				out.Disagreements++
				break
			}
		}
		final := winner
		if final == nil {
			final = best(others)
		}

		for _, m := range matches {
			ks := out.Kinds[m.Kind]
			ks.Located++
			if len(matches) == 1 {
				ks.UniqueWins++
			} else {
				ks.Compared++
				if m.sameCity(final) {
					ks.Agreed++
				}
			}
		}

		if winner != nil {
			row := geoRow{Prefix: p24, Scope: scopeTraced, Country: winner.Rule.country, City: winner.Rule.city,
				RouterIP: winner.Router, PTR: winner.PTR, Rule: winner.Rule.name}
			if winner.ASNKnown {
				same := winner.SameASN
				row.SameASN = &same
			}
			rows = append(rows, row)
			out.Traced++
		}
		if final != nil && final.ASNKnown {
			if wp, ok := widePrefix(origin, p24); ok {
				if votes[wp] == nil {
					votes[wp] = map[cityKey]int{}
				}
				votes[wp][cityKey{final.Rule.country, final.Rule.city}]++
				out.Votes++
			}
		}
	}

	wides := make([]netip.Prefix, 0, len(votes))
	for p := range votes {
		wides = append(wides, p)
	}
	sort.Slice(wides, func(i, j int) bool {
		if wides[i].Addr() != wides[j].Addr() {
			return wides[i].Addr().Less(wides[j].Addr())
		}
		return wides[i].Bits() < wides[j].Bits()
	})
	for _, wp := range wides {
		var top cityKey
		topVotes, tied := 0, false
		for key, n := range votes[wp] {
			switch {
			case n > topVotes:
				top, topVotes, tied = key, n, false
			case n == topVotes:
				tied = true
			}
		}
		if tied {
			out.WideTies++
			continue
		}
		rows = append(rows, geoRow{Prefix: wp, Scope: scopeWide, Country: top.Country, City: top.City})
		out.Wide++
	}
	return rows, out
}

// checkShrink refuses a replace that leaves more than 10% fewer rows than the
// table has, when the table has rows. (An empty table accepts anything.)
func checkShrink(previous, rows int) error {
	if previous > 0 && float64(rows) < float64(previous)*(1-maxShrink) {
		return fmt.Errorf("%w: refusing to replace %d rows by %d (more than %.0f%% fewer)", ErrShrink, previous, rows, maxShrink*100)
	}
	return nil
}

// locateWindow is how old a trace may be for locate to use it.
const locateWindow = "180 days"

// tracesQuery reads the newest trace of each (prefix, target kind) younger than
// $1 (an interval, as text). Read only.
const tracesQuery = `SELECT DISTINCT ON (prefix, target_kind) prefix::text, target_kind, host(target), hops::text
	FROM public.network_traces
	WHERE traced_at > now() - $1::text::interval
	ORDER BY prefix, target_kind, traced_at DESC`

// Locate rebuilds public.rdns_geo_layer from the traces younger than 180 days,
// the PTR names, the rules of public.generated_rules and the announced prefixes of
// public.asn, in one transaction. A shrink of more than 10% is refused
// (ErrShrink) unless force is set.
func (p *Pipeline) Locate(ctx context.Context, force bool) (LocateOutput, error) {
	var out LocateOutput
	conn, err := connect(ctx, p.PGSQL)
	if err != nil {
		return out, err
	}
	defer conn.Close(ctx)

	rules, rstats, err := loadRules(ctx, conn)
	if err != nil {
		return out, dbError("read rules", err)
	}
	origin, prefixes, err := loadOrigin(ctx, conn)
	if err != nil {
		return out, dbError("read announced prefixes", err)
	}
	traces, err := readTraces(ctx, conn, locateWindow)
	if err != nil {
		return out, err
	}
	// Only the names of the IPs the traces can use are read, not the whole table.
	ptrs, err := readNames(ctx, conn, candidateIPs(traces))
	if err != nil {
		return out, err
	}

	rows, out := locate(traces, ptrs, rules, origin)
	out.RulesSkipped, out.RulesInvalid, out.RulesMissing, out.ASNPrefixes = rstats.Skipped, rstats.Invalid, rstats.NoTable, prefixes
	if rstats.NoTable {
		p.log("locate: WARNING public.generated_rules does not exist; no rules, no rows")
	}
	out.Previous, err = replaceLayer(ctx, conn, rows, force)
	if err != nil {
		return out, err
	}
	out.Stored = len(rows)
	p.log("locate: %d traces in %d /24s, %d names, %d rules (%d skipped, %d invalid); %d traced rows, %d wide rows (%d votes, %d ties); %d rows before",
		out.Traces, out.Networks, out.Names, out.Rules, out.RulesSkipped, out.RulesInvalid, out.Traced, out.Wide, out.Votes, out.WideTies, out.Previous)
	for _, k := range kindOrder {
		ks := out.Kinds[k]
		p.log("locate: %s: %d traces, %d reached the ASN, %d located, %d unique, %d agreed of %d compared",
			k, ks.Traces, ks.ReachedASN, ks.Located, ks.UniqueWins, ks.Agreed, ks.Compared)
	}
	return out, nil
}

// readTraces reads the newest trace of each (prefix, target kind) younger than
// window (an interval such as "7 days").
func readTraces(ctx context.Context, conn *pgx.Conn, window string) ([]traceIn, error) {
	rows, err := conn.Query(ctx, tracesQuery, window)
	if err != nil {
		return nil, dbError("read traces", err)
	}
	defer rows.Close()
	var traces []traceIn
	for rows.Next() {
		var prefix, kind, target, hops string
		if err := rows.Scan(&prefix, &kind, &target, &hops); err != nil {
			return nil, dbError("read traces", err)
		}
		if t, ok := parseTrace(prefix, kind, target, hops); ok && t.Prefix.IsValid() {
			traces = append(traces, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, dbError("read traces", err)
	}
	return traces, nil
}

// candidateIPs lists the public IPs that locate and resolve-names look at in
// the traces (see candidates), each once, in address order.
func candidateIPs(traces []traceIn) []netip.Addr {
	seen := map[netip.Addr]bool{}
	for _, t := range traces {
		for _, ip := range candidates(t.Target, t.Hops) {
			if ipdata.PublicV4(ip) {
				seen[ip] = true
			}
		}
	}
	ips := make([]netip.Addr, 0, len(seen))
	for ip := range seen {
		ips = append(ips, ip)
	}
	sort.Slice(ips, func(i, j int) bool { return ips[i].Less(ips[j]) })
	return ips
}

// namesQuery reads the names of some IPs ($1, as text). Read only.
const namesQuery = `SELECT host(ip), ptr FROM public.ip_ptr_names
	WHERE ip = ANY ($1::text[]::inet[]) AND status = 'ok' AND ptr IS NOT NULL`

// readNames reads the PTR names of ips, 5,000 at a time.
func readNames(ctx context.Context, conn *pgx.Conn, ips []netip.Addr) (map[netip.Addr]string, error) {
	names := map[netip.Addr]string{}
	for start := 0; start < len(ips); start += 5000 {
		chunk := ips[start:min(start+5000, len(ips))]
		list := make([]string, len(chunk))
		for i, ip := range chunk {
			list[i] = ip.String()
		}
		rows, err := conn.Query(ctx, namesQuery, list)
		if err != nil {
			return nil, dbError("read names", err)
		}
		for rows.Next() {
			var ip, ptr string
			if err := rows.Scan(&ip, &ptr); err != nil {
				rows.Close()
				return nil, dbError("read names", err)
			}
			if a, err := netip.ParseAddr(ip); err == nil {
				names[a.Unmap()] = ptr
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, dbError("read names", err)
		}
	}
	return names, nil
}

// replaceLayer swaps the contents of public.rdns_geo_layer for rows in one
// transaction, so readers see either the old or the new rows. It returns the number of rows
// the table had.
func replaceLayer(ctx context.Context, conn *pgx.Conn, rows []geoRow, force bool) (int, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, dbError("replace rdns_geo_layer", err)
	}
	defer tx.Rollback(ctx)
	// Block other writers until commit; readers keep seeing the old rows.
	if _, err := tx.Exec(ctx, `LOCK TABLE public.rdns_geo_layer IN EXCLUSIVE MODE`); err != nil {
		return 0, dbError("replace rdns_geo_layer", err)
	}
	var previous int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.rdns_geo_layer`).Scan(&previous); err != nil {
		return 0, dbError("replace rdns_geo_layer", err)
	}
	if !force {
		if err := checkShrink(previous, len(rows)); err != nil {
			return previous, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.rdns_geo_layer`); err != nil {
		return previous, dbError("replace rdns_geo_layer", err)
	}
	const chunk = 10000
	for start := 0; start < len(rows); start += chunk {
		part := rows[start:min(start+chunk, len(rows))]
		prefixes := make([]string, len(part))
		scopes := make([]string, len(part))
		countries := make([]string, len(part))
		cities := make([]string, len(part))
		routerIPs := make([]*string, len(part))
		ptrs := make([]*string, len(part))
		ruleNames := make([]*string, len(part))
		same := make([]*bool, len(part))
		for i, r := range part {
			prefixes[i], scopes[i], countries[i], cities[i] = r.Prefix.String(), r.Scope, r.Country, r.City
			if r.RouterIP.IsValid() {
				s := r.RouterIP.String()
				routerIPs[i] = &s
				ptrs[i] = &part[i].PTR
				ruleNames[i] = &part[i].Rule
			}
			same[i] = r.SameASN
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.rdns_geo_layer
				(prefix, scope, country, city, router_ip, router_ptr, rule_name, same_asn, located_at)
			SELECT u.prefix::cidr, u.scope, u.country, u.city, u.router_ip::inet, u.router_ptr, u.rule_name, u.same_asn, now()
			FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::boolean[])
				AS u(prefix, scope, country, city, router_ip, router_ptr, rule_name, same_asn)`,
			prefixes, scopes, countries, cities, routerIPs, ptrs, ruleNames, same); err != nil {
			return previous, dbError("replace rdns_geo_layer", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return previous, dbError("replace rdns_geo_layer", err)
	}
	return previous, nil
}
