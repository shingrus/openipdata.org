package rdns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
)

// Scopes of rdns_geo_layer rows.
const (
	scopeTraced = "traced" // a /24 that a trace located: layer 65-rdns
	scopeWide   = "wide"   // an announced prefix (at most /16) that traced /24s voted for: layer 35-rdns-wide
)

// How a row of rdns_geo_layer was found (its method column).
const (
	methodRule = "rdns_rule" // a rule matched the PTR name of a router
	methodMMDB = "mmdb_hop"  // only the city of a router hop in our own ip2geo MMDB (wide rows only)
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

	// LocatedAt is when the row was found: for a traced row the time of the trace that located
	// the /24 (so a rebuild from the same trace keeps it, a newer trace moves it, and a row
	// carried over from the previous layer keeps its own); zero for a row found without a
	// trace time and for wide rows (the table stamps now()).
	LocatedAt time.Time

	Method string // methodRule for a traced row; for a wide row methodRule if a rule voted for the city, else methodMMDB
	// RuleVotes and MMDBVotes count the votes for the city of a wide row by method.
	RuleVotes, MMDBVotes int
}

// match is what one trace found: the first candidate, same-ASN ones first, whose
// PTR name a rule matches.
type match struct {
	Kind     string
	Rank     int // position among the candidates, closest to the target first: 0 is the target
	Router   netip.Addr
	PTR      string
	Rule     *rule
	SameASN  bool      // the router is in the target's ASN
	ASNKnown bool      // the target's origin ASN is known
	At       time.Time // when the trace was made
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
	ReachedASN int `json:"reached_asn"` // a tail hop candidate (not the target itself) is in the target's ASN
	Located    int `json:"located"`     // a rule matched a name on the path
	UniqueWins int `json:"unique_wins"` // located, and no other target of the /24 was
	Compared   int `json:"compared"`    // located, and another target of the /24 was too
	Agreed     int `json:"agreed"`      // of those, the city is the one the /24 got
}

// LocateOutput counts what locate read, found and stored.
type LocateOutput struct {
	// Skipped: nothing locate reads changed since the last rebuild (less than a day
	// ago), so the table was left as it is and every count is zero.
	Skipped    bool `json:"skipped,omitempty"`
	Backfilled int  `json:"backfilled,omitempty"` // traces that got their candidates (see backfillCandidates)

	Traces   int `json:"traces"`
	Networks int `json:"networks"` // /24s with a trace
	Names    int `json:"names"`    // PTR names read

	Rules        int  `json:"rules"`
	RulesSkipped int  `json:"rules_skipped"` // Go's regexp could not compile the Python pattern
	RulesInvalid int  `json:"rules_invalid"` // no domains, city or country
	RulesMissing bool `json:"rules_table_missing,omitempty"`
	ASNPrefixes  int  `json:"asn_prefixes"`

	Traced        int `json:"traced"`        // /24s located now by a trace
	Carried       int `json:"carried"`       // traced rows of the previous layer kept: their /24 was not located again (see locate)
	Wide          int `json:"wide"`          // announced prefixes located
	Votes         int `json:"votes"`         // /24s that voted for their announced prefix
	WideTies      int `json:"wide_ties"`     // announced prefixes skipped: two cities with the most votes
	Disagreements int `json:"disagreements"` // /24s whose targets named different cities (same-ASN ones)
	NoASN         int `json:"no_asn"`        // /24s whose target has no origin ASN

	// MMDB says whether the fallback to the router hops' cities in our own published
	// ip2geo MMDB was used ("used") or not ("skipped", with MMDBReason); empty when
	// locate did not run. The counts are zero when it was skipped.
	MMDB           string `json:"mmdb,omitempty"`
	MMDBReason     string `json:"mmdb_reason,omitempty"`
	MMDBKey        string `json:"mmdb_key,omitempty"`        // the R2 object used (a date and a file name)
	MMDBDownloaded bool   `json:"mmdb_downloaded,omitempty"` // the saved copy was out of date and downloaded again
	MMDBLookups    int    `json:"mmdb_lookups"`              // router IPs looked up in the MMDB
	MMDBVotes      int    `json:"mmdb_votes"`                // /24s with no rule match whose vote came from an MMDB hop city
	MMDBCompared   int    `json:"mmdb_compared"`             // rule-matched /24s whose winning router has an MMDB city
	MMDBAgreed     int    `json:"mmdb_agreed"`               // of those, the MMDB city is the rule's (country and city)

	Kinds map[string]*KindStats `json:"kinds"`

	Previous int `json:"previous"` // rows before the replace
	Stored   int `json:"stored"`

	// VisitorsCut counts the visitor IPs deleted after the commit: those of the /24s that are located.
	VisitorsCut int `json:"visitors_cut"`

	// Chained says whether this run started the next resolve-ips run (set by the task, see main.go).
	Chained bool `json:"chained"`
}

// candidateOrder gives the order in which the candidates of t are tried: the
// closest to the target first, those in the target's ASN before the others
// (A router of the target's own network says more about the target than a
// transit router named after a peering city). Indexes into t.Candidates; 0 is
// the target itself. known is whether the target has an origin ASN. reached is
// whether a tail hop among the candidates (not the target, which is trivially in
// its own ASN) is in the target's ASN.
func candidateOrder(t traceIn, origin *originTable) (order []int, known, reached bool) {
	_, targetASN, known := origin.lookup(t.Target)
	cands := t.Candidates
	if known {
		for _, c := range cands[min(1, len(cands)):] {
			if _, asn, ok := origin.lookup(c); ok && asn == targetASN {
				reached = true
				break
			}
		}
	}
	order = make([]int, 0, len(cands))
	if known {
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
	return order, known, reached
}

// matchTrace finds the first candidate of the trace, same-ASN ones first, whose
// PTR name a rule matches. reached is whether a tail hop among the candidates (not
// the target, which is trivially in its own ASN) is in the target's ASN.
func matchTrace(t traceIn, ptrs map[netip.Addr]string, rules []rule, origin *originTable) (m *match, reached bool) {
	order, known, reached := candidateOrder(t, origin)
	_, targetASN, _ := origin.lookup(t.Target)
	cands := t.Candidates
	for _, i := range order {
		ptr := ptrs[cands[i]]
		if ptr == "" {
			continue
		}
		if r := firstMatch(rules, ptr); r != nil {
			_, asn, ok := origin.lookup(cands[i])
			return &match{Kind: t.Kind, Rank: i, Router: cands[i], PTR: ptr, Rule: r,
				SameASN: known && ok && asn == targetASN, ASNKnown: known, At: t.TracedAt}, reached
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

// tally counts the votes for one city of an announced prefix by method.
type tally struct{ rule, mmdb int }

func (t *tally) total() int { return t.rule + t.mmdb }

// groupByNetwork groups the traces by /24; the /24s come in address order.
func groupByNetwork(traces []traceIn) (map[netip.Prefix][]traceIn, []netip.Prefix) {
	byNet := map[netip.Prefix][]traceIn{}
	for _, t := range traces {
		byNet[t.Prefix] = append(byNet[t.Prefix], t)
	}
	prefixes := make([]netip.Prefix, 0, len(byNet))
	for p := range byNet {
		prefixes = append(prefixes, p)
	}
	sort.Slice(prefixes, func(i, j int) bool { return prefixes[i].Addr().Less(prefixes[j].Addr()) })
	return byNet, prefixes
}

// pickFinal chooses among the matches of one /24: winner is the traced match
// (in the target's ASN, or the ASN unknown) closest to the target, an earlier
// kind on a tie; final is the winner, or else the closest match of a router in
// another ASN. Both are nil without matches; winner is nil when only other-ASN
// routers matched.
func pickFinal(matches []*match) (winner, final *match) {
	var traceable, others []*match
	for _, m := range matches {
		if m.traceable() {
			traceable = append(traceable, m)
		} else {
			others = append(others, m)
		}
	}
	winner = best(traceable)
	final = winner
	if final == nil {
		final = best(others)
	}
	return winner, final
}

// hopsByKind returns the traces of one /24 with the earlier kind (visitor,
// gateway, random) first.
func hopsByKind(ts []traceIn) []traceIn {
	sorted := slices.Clone(ts)
	slices.SortStableFunc(sorted, func(a, b traceIn) int { return kindIndex(a.Kind) - kindIndex(b.Kind) })
	return sorted
}

// mmdbVoteCandidates lists the router hops of the /24's traces in the order the
// MMDB fallback tries them: the traces by kind, in each the hop candidates
// (never the target itself) closest first, those in the target's ASN before the
// others (the order matchTrace tries them). A trace whose target has no origin ASN
// gives none: such a /24 has no announced prefix to vote for.
func mmdbVoteCandidates(ts []traceIn, origin *originTable) []netip.Addr {
	var ips []netip.Addr
	for _, t := range hopsByKind(ts) {
		order, known, _ := candidateOrder(t, origin)
		if !known {
			continue
		}
		for _, i := range order {
			if i > 0 {
				ips = append(ips, t.Candidates[i])
			}
		}
	}
	return ips
}

// mmdbVote is the vote of a /24 that no rule matched: the city of the first hop
// of mmdbVoteCandidates that has one in hops.
func mmdbVote(ts []traceIn, hops map[netip.Addr]cityKey, origin *originTable) (cityKey, bool) {
	for _, ip := range mmdbVoteCandidates(ts, origin) {
		if c, ok := hops[ip]; ok {
			return c, true
		}
	}
	return cityKey{}, false
}

// hopIPs lists the router IPs locate wants an MMDB city for, each once, in
// address order, never the target itself: the hops of every /24 that no rule
// matches (see mmdbVoteCandidates), and the winning router of every /24 that a
// rule matches, for the agreement count. It repeats the matching of locate.
func hopIPs(traces []traceIn, ptrs map[netip.Addr]string, rules []rule, origin *originTable) []netip.Addr {
	byNet, prefixes := groupByNetwork(traces)
	seen := map[netip.Addr]bool{}
	for _, p24 := range prefixes {
		var matches []*match
		for _, t := range byNet[p24] {
			if m, _ := matchTrace(t, ptrs, rules, origin); m != nil {
				matches = append(matches, m)
			}
		}
		if len(matches) == 0 {
			for _, ip := range mmdbVoteCandidates(byNet[p24], origin) {
				seen[ip] = true
			}
		} else if _, final := pickFinal(matches); final.Rank > 0 {
			seen[final.Router] = true
		}
	}
	ips := make([]netip.Addr, 0, len(seen))
	for ip := range seen {
		ips = append(ips, ip)
	}
	sort.Slice(ips, func(i, j int) bool { return ips[i].Less(ips[j]) })
	return ips
}

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
// A /24 with no rule match from any trace votes the city of the first router hop
// that has one in hops (see mmdbVote); that is its only vote and never a traced row.
// A rule-matched /24 whose winning router (not the target itself) has a city in hops
// is only compared with it (MMDBCompared, MMDBAgreed). hops may be nil.
//
// A located /24 stays located: every traced row of previous (the current layer) whose /24
// no trace located now is kept as it is, original LocatedAt included, and casts its wide
// vote like a freshly located one, as a rule vote for its city (instead of whatever its
// newest traces would have voted). A /24 that a newer trace locates, in the same city or in
// another, is replaced by the new row.
//
// Per announced prefix the city with the most votes (rule and MMDB votes count
// the same) gets a wide row; a tie gets none. The row's method is methodRule when
// a rule voted for the city, else methodMMDB.
func locate(traces []traceIn, ptrs map[netip.Addr]string, rules []rule, origin *originTable, hops map[netip.Addr]cityKey, previous []geoRow) ([]geoRow, LocateOutput) {
	out := newLocateOutput()
	prevTraced := map[netip.Prefix]geoRow{}
	for _, r := range previous {
		if r.Scope == scopeTraced {
			prevTraced[r.Prefix] = r
		}
	}
	out.Traces, out.Names, out.Rules = len(traces), len(ptrs), len(rules)
	byNet, prefixes := groupByNetwork(traces)
	out.Networks = len(prefixes)

	var rows []geoRow
	votes := map[netip.Prefix]map[cityKey]*tally{}
	// vote counts one vote of the /24 p24 for its announced prefix.
	vote := func(p24 netip.Prefix, key cityKey, fromMMDB bool) {
		wp, ok := widePrefix(origin, p24)
		if !ok {
			return
		}
		if votes[wp] == nil {
			votes[wp] = map[cityKey]*tally{}
		}
		if votes[wp][key] == nil {
			votes[wp][key] = &tally{}
		}
		if fromMMDB {
			votes[wp][key].mmdb++
			out.MMDBVotes++
		} else {
			votes[wp][key].rule++
		}
		out.Votes++
	}
	located := map[netip.Prefix]bool{}
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

		winner, final := pickFinal(matches)
		for _, m := range matches {
			if m.traceable() && !m.sameCity(winner) {
				out.Disagreements++
				break
			}
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
				RouterIP: winner.Router, PTR: winner.PTR, Rule: winner.Rule.name, Method: methodRule, LocatedAt: winner.At}
			if winner.ASNKnown {
				same := winner.SameASN
				row.SameASN = &same
			}
			rows = append(rows, row)
			located[p24] = true
			out.Traced++
		}
		if _, kept := prevTraced[p24]; kept && winner == nil {
			continue // carried below, with its own vote
		}
		var voteKey cityKey
		var fromMMDB, voting bool
		switch {
		case final != nil:
			voteKey, voting = cityKey{final.Rule.country, final.Rule.city}, final.ASNKnown
			if final.Rank > 0 { // a router hop, not the target itself
				if c, ok := hops[final.Router]; ok {
					out.MMDBCompared++
					if c == voteKey {
						out.MMDBAgreed++
					}
				}
			}
		case len(hops) > 0:
			voteKey, voting = mmdbVote(byNet[p24], hops, origin)
			fromMMDB = true
		}
		if voting {
			vote(p24, voteKey, fromMMDB)
		}
	}
	// The traced rows of the previous layer that no trace located now.
	kept := make([]netip.Prefix, 0, len(prevTraced))
	for p24 := range prevTraced {
		if !located[p24] {
			kept = append(kept, p24)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Addr().Less(kept[j].Addr()) })
	for _, p24 := range kept {
		r := prevTraced[p24]
		rows = append(rows, r)
		out.Carried++
		vote(p24, cityKey{r.Country, r.City}, false)
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
		for key, t := range votes[wp] {
			switch n := t.total(); {
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
		won := votes[wp][top]
		method := methodRule
		if won.rule == 0 {
			method = methodMMDB
		}
		rows = append(rows, geoRow{Prefix: wp, Scope: scopeWide, Country: top.Country, City: top.City,
			Method: method, RuleVotes: won.rule, MMDBVotes: won.mmdb})
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
// $1 (an interval, as text): its candidates, never its path (hops). A row not
// backfilled yet reads as no candidates, and parseTrace gives it its target. Only the
// newest round of traces of a /24 counts (those within an hour of its newest trace): a
// recheck of a located /24 has no visitor trace, and an older one must not stand in for it.
// Read only.
const tracesQuery = `SELECT prefix::text, target_kind, host(target),
		ARRAY(SELECT host(c) FROM unnest(candidates) WITH ORDINALITY AS u(c, n) ORDER BY n), traced_at
	FROM (
		SELECT prefix, target_kind, target, candidates, traced_at,
		       row_number() OVER (PARTITION BY prefix, target_kind ORDER BY traced_at DESC) AS n,
		       max(traced_at) OVER (PARTITION BY prefix) AS newest
		FROM public.network_traces
		WHERE traced_at > now() - $1::text::interval) t
	WHERE n = 1 AND traced_at > newest - interval '1 hour'
	ORDER BY prefix, target_kind`

// newLocateOutput is an output with every kind present, all counts zero.
func newLocateOutput() LocateOutput {
	out := LocateOutput{Kinds: map[string]*KindStats{}}
	for _, k := range kindOrder {
		out.Kinds[k] = &KindStats{}
	}
	return out
}

func (p *Pipeline) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Locate rebuilds public.rdns_geo_layer from the traces younger than 180 days
// (their candidates, never their paths), the PTR names, the rules of
// public.generated_rules and the announced prefixes of public.asn, in one
// transaction. A shrink of more than 10% is refused (ErrShrink) unless force is set.
//
// A run whose inputs look as they did at the last rebuild (see inputsSummary and
// fingerprintOf), less than a day ago, leaves the table alone and says so
// (LocateOutput.Skipped); force always rebuilds. The fingerprint is taken before the
// inputs are read and saved only after the commit, so a change during the run only
// causes one more rebuild.
//
// The inputs include the ETag of our own published ip2geo MMDB in R2 (today's, else
// yesterday's; see statMMDB). Only a run that rebuilds downloads it. When R2 cannot be
// used (no R2 setting, no object, a Stat or download error) the saved copy and its saved
// ETag are used instead (LocateOutput.MMDB "saved copy" and the reason), so an outage
// neither changes the fingerprint nor the rows. Only without a usable saved copy, or when
// mmdbbuilder cannot read it, is the router hop fallback skipped with a warning
// (LocateOutput.MMDB "skipped") and the rebuild goes on without it; the fingerprint then
// says "none", so the next run tries again.
func (p *Pipeline) Locate(ctx context.Context, force bool) (LocateOutput, error) {
	out := newLocateOutput()
	conn, err := connect(ctx, p.PGSQL)
	if err != nil {
		return out, err
	}
	defer conn.Close(ctx)

	statePath := filepath.Join(p.dir(), locateStateFile)
	ref := p.statMMDB(ctx)
	summary, err := inputsSummary(ctx, conn)
	fingerprint := fingerprintOf(summary, ref.etag())
	if err != nil {
		// Without it the table is simply rebuilt, as before.
		p.log("locate: %v; rebuilding", err)
		fingerprint = ""
	}
	if !force && fingerprint != "" && readLocateState(statePath).canSkip(fingerprint, p.clock()) {
		out.Skipped = true
		p.log("locate: skipped, no trace, name, rule or ASN changed since the last rebuild")
		return out, nil
	}

	if out.Backfilled, err = backfillCandidates(ctx, conn); err != nil {
		return out, err
	}
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

	res, mmdbLookups := skipped(ref.Reason), 0
	if ref.usable() {
		ips := hopIPs(traces, ptrs, rules, origin)
		mmdbLookups = len(ips)
		res = p.mmdbHops(ctx, ref, ips)
	}

	previous, err := readTracedRows(ctx, conn)
	if err != nil {
		return out, err
	}
	backfilled := out.Backfilled
	rows, out := locate(traces, ptrs, rules, origin, res.Hops, previous)
	out.Backfilled = backfilled
	out.MMDB, out.MMDBReason, out.MMDBDownloaded = res.Status, res.Reason, res.Downloaded
	if res.Status == statusUsed {
		out.MMDBKey = ref.Key
	}
	if res.Status != statusSkipped {
		out.MMDBLookups = mmdbLookups
	}
	// The fingerprint names the MMDB that was read, so a run that had to use another
	// (or none) is followed by one that tries R2 again.
	if fingerprint != "" && res.ETag != ref.etag() {
		fingerprint = fingerprintOf(summary, res.ETag)
	}
	out.RulesSkipped, out.RulesInvalid, out.RulesMissing, out.ASNPrefixes = rstats.Skipped, rstats.Invalid, rstats.NoTable, prefixes
	if rstats.NoTable {
		p.log("locate: WARNING public.generated_rules does not exist; no rules, no rows")
	}
	out.Previous, err = replaceLayer(ctx, conn, rows, force)
	if err != nil {
		return out, err
	}
	out.Stored = len(rows)
	// The located /24s need no visitor IP any more: replaceLayer made them located in
	// traced_networks, so every visitor IP of theirs can go.
	tag, err := conn.Exec(ctx, deleteLocatedVisitorsSQL)
	if err != nil {
		return out, dbError("delete visitor IPs", err)
	}
	out.VisitorsCut = int(tag.RowsAffected())
	if fingerprint != "" {
		// Only the next skip is lost when this fails: the layer itself is stored.
		if err := writeLocateState(statePath, locateState{Fingerprint: fingerprint, RebuiltAt: p.clock().UTC()}); err != nil {
			p.log("locate: could not save %s: %v", locateStateFile, err)
		}
	}
	if out.MMDB != statusSkipped {
		how := "saved copy"
		if out.MMDBDownloaded {
			how = "downloaded"
		}
		key := out.MMDBKey
		if key == "" {
			key = "no R2 object"
		}
		p.log("locate: MMDB %s (%s): %d router IPs looked up, %d /24s voted by an MMDB hop city; of %d rule-matched /24s whose router has an MMDB city, %d agree",
			key, how, out.MMDBLookups, out.MMDBVotes, out.MMDBCompared, out.MMDBAgreed)
	}
	p.log("locate: %d traces in %d /24s, %d names, %d rules (%d skipped, %d invalid); %d traced rows (%d carried over), %d wide rows (%d votes, %d ties); %d rows before; %d visitor IPs deleted",
		out.Traces, out.Networks, out.Names, out.Rules, out.RulesSkipped, out.RulesInvalid, out.Traced+out.Carried, out.Carried, out.Wide, out.Votes, out.WideTies, out.Previous, out.VisitorsCut)
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
		var prefix, kind, target string
		var cands []string
		var at time.Time
		if err := rows.Scan(&prefix, &kind, &target, &cands, &at); err != nil {
			return nil, dbError("read traces", err)
		}
		if t, ok := parseTrace(prefix, kind, target, cands); ok && t.Prefix.IsValid() {
			t.TracedAt = at
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
		for _, ip := range t.Candidates {
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
		methods := make([]string, len(part))
		ruleVotes := make([]*int32, len(part))
		mmdbVotes := make([]*int32, len(part))
		locatedAt := make([]*time.Time, len(part))
		for i, r := range part {
			prefixes[i], scopes[i], countries[i], cities[i] = r.Prefix.String(), r.Scope, r.Country, r.City
			if r.RouterIP.IsValid() {
				s := r.RouterIP.String()
				routerIPs[i] = &s
				ptrs[i] = &part[i].PTR
				ruleNames[i] = &part[i].Rule
			}
			same[i] = r.SameASN
			methods[i] = r.Method
			if !r.LocatedAt.IsZero() {
				locatedAt[i] = &part[i].LocatedAt
			}
			if r.Scope == scopeWide {
				rv, mv := int32(r.RuleVotes), int32(r.MMDBVotes)
				ruleVotes[i], mmdbVotes[i] = &rv, &mv
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.rdns_geo_layer
				(prefix, scope, country, city, router_ip, router_ptr, rule_name, same_asn, method, rule_votes, mmdb_votes, located_at)
			SELECT u.prefix::cidr, u.scope, u.country, u.city, u.router_ip::inet, u.router_ptr, u.rule_name, u.same_asn,
				u.method, u.rule_votes, u.mmdb_votes, coalesce(u.located_at, now())
			FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::boolean[],
				$9::text[], $10::integer[], $11::integer[], $12::timestamptz[])
				AS u(prefix, scope, country, city, router_ip, router_ptr, rule_name, same_asn, method, rule_votes, mmdb_votes, located_at)`,
			prefixes, scopes, countries, cities, routerIPs, ptrs, ruleNames, same, methods, ruleVotes, mmdbVotes, locatedAt); err != nil {
			return previous, dbError("replace rdns_geo_layer", err)
		}
	}
	// Every traced row is a located /24 now: its schedule says so (rechecked 150 days after
	// its last trace, no more attempts of the silent or unmatched kind).
	if _, err := tx.Exec(ctx, markLocatedSQL); err != nil {
		return previous, dbError("mark located networks", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return previous, dbError("replace rdns_geo_layer", err)
	}
	return previous, nil
}

// markLocatedSQL makes every /24 of the traced rows of rdns_geo_layer located in
// traced_networks, in the transaction that replaced them. An existing row keeps its
// last_traced_at; next_trace_at is 150 days after it. A row that is already located and
// whose located_at did not change is left alone.
const markLocatedSQL = `INSERT INTO public.traced_networks AS t (prefix, status, attempts, last_traced_at, next_trace_at, located_at)
	SELECT g.prefix, 'located', 0, g.located_at, g.located_at + interval '150 days', g.located_at
	FROM public.rdns_geo_layer g WHERE g.scope = 'traced'
	ON CONFLICT (prefix) DO UPDATE SET status = 'located', attempts = 0, located_at = excluded.located_at,
		next_trace_at = t.last_traced_at + interval '150 days'
	WHERE t.status <> 'located' OR t.located_at IS DISTINCT FROM excluded.located_at`

// tracedRowsQuery reads the traced rows of the current layer. Read only.
const tracedRowsQuery = `SELECT prefix::text, country, city, host(router_ip), router_ptr, rule_name, same_asn, method, located_at
	FROM public.rdns_geo_layer WHERE scope = 'traced' ORDER BY prefix`

// readTracedRows reads the traced rows of the current rdns_geo_layer, with all their columns,
// for locate to keep the ones that no trace locates again. A row older than the method
// column has no method: it was found by a rule.
func readTracedRows(ctx context.Context, conn *pgx.Conn) ([]geoRow, error) {
	rows, err := conn.Query(ctx, tracedRowsQuery)
	if err != nil {
		return nil, dbError("read rdns_geo_layer", err)
	}
	defer rows.Close()
	var out []geoRow
	for rows.Next() {
		r := geoRow{Scope: scopeTraced}
		var prefix string
		var router, ptr, rule, method *string
		if err := rows.Scan(&prefix, &r.Country, &r.City, &router, &ptr, &rule, &r.SameASN, &method, &r.LocatedAt); err != nil {
			return nil, dbError("read rdns_geo_layer", err)
		}
		var perr error
		if r.Prefix, perr = netip.ParsePrefix(prefix); perr != nil {
			continue
		}
		if router != nil {
			if a, err := netip.ParseAddr(*router); err == nil {
				r.RouterIP = a.Unmap()
			}
		}
		if ptr != nil {
			r.PTR = *ptr
		}
		if rule != nil {
			r.Rule = *rule
		}
		r.Method = methodRule
		if method != nil && *method != "" {
			r.Method = *method
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError("read rdns_geo_layer", err)
	}
	return out, nil
}
