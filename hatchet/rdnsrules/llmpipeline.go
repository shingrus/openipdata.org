package rdnsrules

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"openipdata.org/hatchet/schema"
)

// Limits of one propose-codes run.
const (
	maxRequestsPerRun = 400   // a guard against our own bugs
	costPerRequestUSD = 0.005 // estimate used against the key's remaining credit
	packSize          = 30    // small domains per packed request
	reaskAfter        = 30 * 24 * time.Hour
	llmParallel       = 4
)

// Kinds of a question (rdns_llm_answers.kind) and verdicts.
const (
	kindPropose      = "propose"
	kindProposeSmall = "propose_small"
	kindSecond       = "second_opinion"

	verdictAccepted = "accepted"
	verdictRejected = "rejected"
	verdictNoCity   = "no_city"
	verdictBlocked  = "blocked"
	verdictError    = "error"
	verdictStopped  = "stopped" // the credit stop reached this request: asked again next run, no error

	statusComplete   = "complete"
	statusIncomplete = "incomplete"
)

// Stop reasons.
const (
	stopCredit = "credit_limit"
	stopCap    = "request_cap"
	stopDicts  = "dictionaries"
)

// ProposeOptions restricts or forces the questions of a run.
type ProposeOptions struct {
	Domains []string // only these registered domains (empty: all)
	Force   bool     // ask again whatever the record says
}

// ProposeOutput is counts only.
type ProposeOutput struct {
	Skipped         string         `json:"skipped,omitempty"`
	Eligible        int            `json:"eligible"`
	Asked           int            `json:"asked"`
	Requests        int            `json:"requests"`
	Accepted        int            `json:"accepted"`
	SmallAccepted   int            `json:"small_accepted"`
	Rejected        map[string]int `json:"rejected,omitempty"`
	NoCity          int            `json:"no_city"`
	Blocked         int            `json:"blocked"`
	Errors          int            `json:"errors"`
	StoppedDomains  int            `json:"stopped_domains"` // domains of a request the credit stop reached (incomplete, not errors)
	Incomplete      int            `json:"incomplete"`
	ComparedRules   int            `json:"compared_rules"`
	Disagreements   int            `json:"disagreements"`
	Overrides       int            `json:"overrides"`
	Disputed        int            `json:"disputed"`
	CostUSD         float64        `json:"cost_usd"`
	KeyRemainingUSD *float64       `json:"key_remaining_usd,omitempty"`
	Stopped         string         `json:"stopped,omitempty"`
	DurationSeconds float64        `json:"duration_seconds"`
}

// llmRuleRow is a stored row of public.rdns_city_rules as the LLM task needs it.
type llmRuleRow struct {
	Rank       int
	Version    string
	Status     string
	Origin     string
	Pattern    string
	Dictionary string
	Codes      map[string]Code
	Places     map[string][2]string
	Rejected   []string
}

// lastAnswer is what the record says about a domain: its newest complete answer and whether
// the newest question of all (propose or propose_small) is an incomplete one.
type lastAnswer struct {
	Hash             string    // pattern set of the newest complete answer
	AskedAt          time.Time // when it was asked
	RuleVersions     []string  // learned versions it was compared with (after its overrides)
	HasComplete      bool
	NewestIncomplete bool
}

// answerRecord is a row of public.rdns_llm_answers.
type answerRecord struct {
	AskedAt                            time.Time
	Kind, Domain, RequestID, Model     string
	Hosts                              int
	Patterns                           []string
	Hash                               string
	RuleVersions                       []string
	Answer                             any
	Status, Verdict, Reason            string
	Disagreements, Overrides, Disputed int
	TokensIn, TokensOut                int
	Cost                               float64
}

// llmRuleWrite is an accepted answer as the domain's rank-4 row.
type llmRuleWrite struct {
	Regex        string
	Codes        map[string]Code
	Places       map[string]Place
	Names        int
	Decoded      int
	Distinct     int
	CountryAgree *float64
}

// rowUpdate changes the codes and places of one learned row (an override).
type rowUpdate struct {
	Rank         int
	OldVersion   string
	NewVersion   string
	Codes        map[string]Code
	Places       map[string]Place
	CountryAgree *float64
}

// proposeStore is the database side of propose-codes (a fake in tests).
type proposeStore interface {
	LastAnswers(ctx context.Context) (map[string]lastAnswer, error)
	// LearnedVersions returns the versions of each domain's current learned rows (ranks 1-3, not rejected).
	LearnedVersions(ctx context.Context) (map[string][]string, error)
	Rules(ctx context.Context, domain string) ([]llmRuleRow, error)
	SaveAnswer(ctx context.Context, rec answerRecord) error
	// StoreLLMRule writes the rank-4 row. It returns a rejection reason (a withdrawn version) or "".
	StoreLLMRule(ctx context.Context, domain string, w llmRuleWrite) (string, error)
	// UpdateLearnedRow returns false when the row is no longer at OldVersion.
	UpdateLearnedRow(ctx context.Context, domain string, u rowUpdate) (bool, error)
}

// domainInfo is what a run knows about a domain.
type domainInfo struct {
	Domain   string
	Names    []Name
	Hosts    int
	Patterns []string
	Hash     string
	Country  string // majority announced country, "" if unknown
	Small    bool
}

// buildDomainInfo prepares a domain's names (all real names stay on the VM).
func buildDomainInfo(domain string, names []Name) *domainInfo {
	var clean []Name
	ips := map[string]bool{}
	var ptrs []string
	for _, n := range names {
		if n.PTR == "" || !cleanLabels(n.PTR, domain) {
			continue
		}
		clean = append(clean, n)
		if !ips[n.IP] {
			ips[n.IP] = true
			ptrs = append(ptrs, n.PTR)
		}
	}
	pats := topPatterns(ptrs, domain, maxPatternsPerDomain)
	return &domainInfo{Domain: domain, Names: clean, Hosts: len(ips), Patterns: pats, Hash: patternsHash(pats),
		Country: majorityCountry(clean), Small: len(ips) <= llmSmallHosts}
}

// needsAsking: a domain is asked when it never had a complete answer, when its newest question
// is incomplete (an error, a stop, a failed second opinion), when one of its current learned
// versions was not checked by its newest complete answer (no wait), or when its patterns
// changed and that answer is 30 days old; force asks regardless.
func needsAsking(info *domainInfo, last *lastAnswer, learned []string, now time.Time, force bool) bool {
	if force || last == nil || !last.HasComplete || last.NewestIncomplete {
		return true
	}
	for _, v := range learned {
		if !slicesContains(last.RuleVersions, v) {
			return true
		}
	}
	return info.Hash != last.Hash && now.Sub(last.AskedAt) >= reaskAfter
}

// llmRequest is one question to the model: a domain with 5+ hosts, or a pack of small ones.
type llmRequest struct {
	Kind    string
	ID      string
	Domains []*domainInfo
}

func (r llmRequest) biggest() int {
	n := 0
	for _, d := range r.Domains {
		n = max(n, d.Hosts)
	}
	return n
}

// planRequests packs the domains: 5+ hosts one per request; small ones about packSize per
// request, all of one majority announced country (an unknown country: alone). Biggest first.
func planRequests(infos []*domainInfo, newID func() string) []llmRequest {
	var out []llmRequest
	byCountry := map[string][]*domainInfo{}
	for _, d := range infos {
		switch {
		case !d.Small:
			out = append(out, llmRequest{Kind: kindPropose, ID: newID(), Domains: []*domainInfo{d}})
		case d.Country == "":
			out = append(out, llmRequest{Kind: kindProposeSmall, ID: newID(), Domains: []*domainInfo{d}})
		default:
			byCountry[d.Country] = append(byCountry[d.Country], d)
		}
	}
	for _, c := range sortedKeys(byCountry) {
		list := byCountry[c]
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Hosts != list[j].Hosts {
				return list[i].Hosts > list[j].Hosts
			}
			return list[i].Domain < list[j].Domain
		})
		for len(list) > 0 {
			n := min(packSize, len(list))
			out = append(out, llmRequest{Kind: kindProposeSmall, ID: newID(), Domains: list[:n]})
			list = list[n:]
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].biggest() != out[j].biggest() {
			return out[i].biggest() > out[j].biggest()
		}
		return out[i].Domains[0].Domain < out[j].Domains[0].Domain
	})
	return out
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// proposeRun is one run of the task.
type proposeRun struct {
	store    proposeStore
	api      *openRouter
	model    string
	second   string
	loadGeo  func(context.Context) (*geoIndex, error)
	now      func() time.Time
	progress func(string)
	parallel int
	newID    func() string

	mu        sync.Mutex // guards everything below and every store call
	out       ProposeOutput
	requests  int
	maxReq    int
	remaining *float64
	spent     float64
	stopped   string
	done      int // first-model requests finished, for the progress line
	planned   int // first-model requests planned this run
	fatal     error
	cancel    context.CancelFunc

	geoOnce sync.Once
	geo     *geoIndex
	geoErr  error
}

func (r *proposeRun) logf(format string, args ...any) {
	if r.progress != nil {
		r.progress(fmt.Sprintf(format, args...))
	}
}

func (r *proposeRun) geoDict(ctx context.Context) (*geoIndex, error) {
	r.geoOnce.Do(func() { r.geo, r.geoErr = r.loadGeo(ctx) })
	return r.geo, r.geoErr
}

// take reserves one request. It stops the run (no new requests) at the request cap or when
// the estimate no longer fits the key's remaining credit.
func (r *proposeRun) take() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped != "" || r.fatal != nil {
		return false
	}
	if r.requests >= r.maxReq {
		r.stopped = stopCap
		if r.remaining != nil && r.maxReq < maxRequestsPerRun {
			r.stopped = stopCredit
		}
		return false
	}
	if r.remaining != nil && r.spent+costPerRequestUSD > *r.remaining {
		r.stopped = stopCredit
		return false
	}
	r.requests++
	return true
}

func (r *proposeRun) addCost(c float64) {
	r.mu.Lock()
	r.spent += c
	r.mu.Unlock()
}

// apiFailure applies the run-level rules of a failed request: a credit stop ends the run
// quietly, an invalid key fails it.
func (r *proposeRun) apiFailure(e *apiError) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch e.Kind {
	case apiStopCredit:
		if r.stopped == "" {
			r.stopped = stopCredit
			r.logf("propose-codes: OpenRouter credit limit reached (%s); stopping, nothing stored is lost", e.Reason)
		}
	case apiAuth:
		if r.fatal == nil {
			r.fatal = ErrInvalidKey
			if r.cancel != nil {
				r.cancel()
			}
		}
	}
}

func (r *proposeRun) setFatal(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fatal == nil {
		r.fatal = err
		if r.cancel != nil {
			r.cancel()
		}
	}
}

// save records an answer and counts an incomplete one.
func (r *proposeRun) save(ctx context.Context, rec answerRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.AskedAt.IsZero() {
		rec.AskedAt = r.now()
	}
	if err := r.store.SaveAnswer(ctx, rec); err != nil {
		if r.fatal == nil {
			r.fatal = err
			if r.cancel != nil {
				r.cancel()
			}
		}
		return
	}
	if rec.Status == statusIncomplete {
		r.out.Incomplete++
	}
	switch rec.Verdict {
	case verdictNoCity:
		r.out.NoCity++
	case verdictBlocked:
		r.out.Blocked++
	case verdictError:
		r.out.Errors++
	case verdictStopped:
		r.out.StoppedDomains++
	case verdictRejected:
		if rec.Kind != kindSecond {
			if r.out.Rejected == nil {
				r.out.Rejected = map[string]int{}
			}
			r.out.Rejected[rec.Reason]++
		}
	}
}

// storeCall runs a store call under the lock; a database error fails the run.
func storeCall[T any](r *proposeRun, f func() (T, error)) (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := f()
	if err != nil {
		if r.fatal == nil {
			r.fatal = err
			if r.cancel != nil {
				r.cancel()
			}
		}
		var zero T
		return zero, false
	}
	return v, true
}

// execute asks the eligible domains. infos are all domains with current names.
func (r *proposeRun) execute(ctx context.Context, infos map[string]*domainInfo, opts ProposeOptions) (ProposeOutput, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.cancel = cancel
	if r.now == nil {
		r.now = time.Now
	}
	if r.newID == nil {
		r.newID = newRequestID
	}
	r.maxReq = maxRequestsPerRun
	last, err := r.store.LastAnswers(ctx)
	if err != nil {
		return r.out, err
	}
	learnedNow, err := r.store.LearnedVersions(ctx)
	if err != nil {
		return r.out, err
	}
	only := map[string]bool{}
	for _, d := range opts.Domains {
		if d = normalize(d); d != "" {
			only[d] = true
		}
	}
	var eligible []*domainInfo
	for _, d := range sortedKeys(infos) {
		if len(only) > 0 && !only[d] {
			continue
		}
		info := infos[d]
		if info.Hosts == 0 || len(info.Patterns) == 0 {
			continue
		}
		var l *lastAnswer
		if a, ok := last[d]; ok {
			l = &a
		}
		if needsAsking(info, l, learnedNow[d], r.now(), opts.Force) {
			eligible = append(eligible, info)
		}
	}
	r.out.Eligible = len(eligible)
	if len(eligible) == 0 {
		r.logf("propose-codes: no domain to ask")
		return r.out, nil
	}
	rem, aerr := r.api.keyRemaining(ctx)
	switch {
	case aerr != nil && aerr.Kind == apiAuth:
		return r.out, ErrInvalidKey
	case aerr != nil:
		r.logf("propose-codes: could not read the key's credit (%s); going on without an estimate", aerr.Reason)
	default:
		r.remaining = rem
		r.out.KeyRemainingUSD = rem
		if rem != nil {
			r.maxReq = min(r.maxReq, int(*rem/costPerRequestUSD))
			if r.maxReq <= 0 {
				r.out.Stopped = stopCredit
				r.logf("propose-codes: the key has $%.2f left, not enough for a request", *rem)
				return r.out, nil
			}
		}
	}
	requests := planRequests(eligible, r.newID)
	r.planned = min(len(requests), r.maxReq)
	r.logf("propose-codes: %d domains eligible in %d requests (up to %d requests this run)", len(eligible), len(requests), r.maxReq)
	work := make(chan llmRequest)
	var wg sync.WaitGroup
	for i := 0; i < max(1, r.parallel); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for req := range work {
				r.handle(ctx, req, false)
			}
		}()
	}
	for _, req := range requests {
		r.mu.Lock()
		done := r.stopped != "" || r.fatal != nil
		r.mu.Unlock()
		if done {
			break
		}
		work <- req
	}
	close(work)
	wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fatal != nil {
		return r.out, r.fatal
	}
	r.out.Requests = r.requests
	r.out.CostUSD = r.spent
	r.out.Stopped = r.stopped
	return r.out, nil
}

// handle sends one request and finishes each of its domains. retry marks a pack that is
// asked again one domain at a time (its domains are already counted as asked).
func (r *proposeRun) handle(ctx context.Context, req llmRequest, retry bool) {
	if !r.take() {
		return
	}
	var prompt string
	if req.Kind == kindPropose {
		d := req.Domains[0]
		prompt = singlePrompt(d.Domain, d.Patterns, d.Hosts)
	} else {
		doms := make([]string, len(req.Domains))
		pats := map[string][]string{}
		for i, d := range req.Domains {
			doms[i], pats[d.Domain] = d.Domain, d.Patterns
		}
		prompt = packPrompt(doms, pats)
	}
	if !retry {
		r.mu.Lock()
		r.out.Asked += len(req.Domains)
		r.mu.Unlock()
	}
	res, aerr := r.api.chat(ctx, r.model, prompt)
	r.addCost(res.Cost)
	if aerr != nil {
		r.apiFailure(aerr)
		if aerr.Kind == apiBlocked && len(req.Domains) > 1 {
			// One domain's names may have tripped the block: ask each alone.
			for _, d := range req.Domains {
				r.handle(ctx, llmRequest{Kind: kindProposeSmall, ID: r.newID(), Domains: []*domainInfo{d}}, true)
			}
			return
		}
		verdict, status := verdictError, statusIncomplete
		reason := aerr.Reason
		switch aerr.Kind {
		case apiBlocked:
			verdict, status = verdictBlocked, statusComplete
		case apiStopCredit:
			verdict, reason = verdictStopped, "stopped: "+reason
		}
		for _, d := range req.Domains {
			r.save(ctx, answerRecord{Kind: req.Kind, Domain: d.Domain, RequestID: req.ID, Model: r.model, Hosts: d.Hosts,
				Patterns: d.Patterns, Hash: d.Hash, Status: status, Verdict: verdict, Reason: reason})
		}
		return
	}
	share := float64(len(req.Domains))
	usage := func(rec *answerRecord) {
		rec.TokensIn, rec.TokensOut = int(float64(res.TokensIn)/share), int(float64(res.TokensOut)/share)
		rec.Cost = res.Cost / share
	}
	answers := map[string]llmAnswer{}
	parsed := false
	if req.Kind == kindPropose {
		if a, ok := parseSingle(res.Content); ok {
			answers[req.Domains[0].Domain], parsed = a, true
		}
	} else {
		answers = parsePack(res.Content)
		parsed = len(answers) > 0
	}
	for _, d := range req.Domains {
		rec := answerRecord{Kind: req.Kind, Domain: d.Domain, RequestID: req.ID, Model: r.model, Hosts: d.Hosts, Patterns: d.Patterns, Hash: d.Hash}
		usage(&rec)
		a, ok := answers[d.Domain]
		if !ok {
			rec.Status, rec.Verdict = statusIncomplete, verdictError
			rec.Reason = "answer is not JSON"
			if parsed {
				rec.Reason = "domain missing from the answer"
			}
			if res.FinishReason == "length" {
				rec.Reason = "answer cut off"
			}
			r.save(ctx, rec)
			continue
		}
		r.finishDomain(ctx, d, a, &rec)
	}
	label := req.Domains[0].Domain
	if len(req.Domains) > 1 {
		label += fmt.Sprintf(" +%d", len(req.Domains)-1)
	}
	r.mu.Lock()
	if !retry {
		r.done++
	}
	done, planned := r.done, r.planned
	r.mu.Unlock()
	r.logf("propose-codes: request of %q done %d/%d (%s)", label, done, planned, req.Kind)
}

// finishDomain checks one answer and, when it is accepted, stores the rank-4 rule, compares it
// with the learned rules and asks the second model about the disagreements.
func (r *proposeRun) finishDomain(ctx context.Context, d *domainInfo, a llmAnswer, rec *answerRecord) {
	geo, err := r.geoDict(ctx)
	if err != nil {
		// No dictionary: the answer cannot be checked. Not the API's fault, not a database error.
		rec.Status, rec.Verdict, rec.Reason = statusIncomplete, verdictError, "dictionary unavailable"
		r.mu.Lock()
		if r.stopped == "" {
			r.stopped = stopDicts
		}
		r.mu.Unlock()
		r.save(ctx, *rec)
		return
	}
	rule, reason := checkAnswer(d.Domain, a, d.Names, d.Small, geo)
	rec.Answer = answerDoc{Raw: a, Dropped: nilIfEmpty(ruleDropped(rule))}
	rec.Status = statusComplete
	switch {
	case reason == "no_city":
		rec.Verdict = verdictNoCity
		r.save(ctx, *rec)
		return
	case reason != "":
		rec.Verdict, rec.Reason = verdictRejected, reason
		r.save(ctx, *rec)
		return
	}
	rejected, ok := storeCall(r, func() (string, error) {
		return r.store.StoreLLMRule(ctx, d.Domain, llmRuleWrite{Regex: rule.Regex, Codes: rule.Codes, Places: rule.Places,
			Names: rule.Names, Decoded: rule.Decoded, Distinct: rule.Distinct, CountryAgree: rule.CountryAgree})
	})
	if !ok {
		return
	}
	if rejected != "" {
		rec.Verdict, rec.Reason = verdictRejected, rejected
		r.save(ctx, *rec)
		return
	}
	rec.Verdict = verdictAccepted
	r.mu.Lock()
	r.out.Accepted++
	if d.Small {
		r.out.SmallAccepted++
	}
	r.mu.Unlock()
	rows, ok := storeCall(r, func() ([]llmRuleRow, error) { return r.store.Rules(ctx, d.Domain) })
	if !ok {
		return
	}
	learned := compileLearned(d.Domain, rows)
	// Every current learned version counts as checked, also a row whose regex cannot be compiled
	// (LearnedVersions reads them all).
	for _, row := range rows {
		if row.Rank >= 1 && row.Rank <= MaxRank && row.Status != "rejected" {
			rec.RuleVersions = append(rec.RuleVersions, row.Version)
		}
	}
	if len(learned) > 0 {
		r.mu.Lock()
		r.out.ComparedRules += len(learned)
		r.mu.Unlock()
		disputes := compareRules(d.Domain, d.Names, learned, rule, geo)
		rec.Disagreements = len(disputes)
		if len(disputes) > 0 {
			overrides, disputed, incomplete, newVersions := r.secondOpinion(ctx, d, learned, rows, disputes, geo, rec)
			rec.Overrides, rec.Disputed = overrides, disputed
			// The versions an override wrote are the ones this answer checked: no re-ask for them.
			for i, v := range rec.RuleVersions {
				if nv, ok := newVersions[v]; ok {
					rec.RuleVersions[i] = nv
				}
			}
			if incomplete != "" {
				rec.Status, rec.Reason = statusIncomplete, incomplete
			}
			r.mu.Lock()
			r.out.Disagreements += len(disputes)
			r.out.Overrides += overrides
			r.out.Disputed += disputed
			r.mu.Unlock()
		}
	}
	r.save(ctx, *rec)
}

// answerDoc is the stored form of a domain's answer.
type answerDoc struct {
	Raw     llmAnswer         `json:"answer"`
	Dropped map[string]string `json:"dropped_codes,omitempty"`
	Second  map[string]any    `json:"second_opinion,omitempty"`
}

func ruleDropped(r *checkedRule) map[string]string {
	if r == nil {
		return nil
	}
	return r.Dropped
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// secondOpinion asks the second model about the disputed tokens (one request) and applies the
// overrides it supports. It returns the overrides, the disputed tokens and, when the answer
// cannot be called complete, why.
func (r *proposeRun) secondOpinion(ctx context.Context, d *domainInfo, learned []learnedRule, rows []llmRuleRow, disputes []tokenDispute, geo *geoIndex, rec *answerRecord) (overrides, disputed int, incomplete string, newVersions map[string]string) {
	newVersions = map[string]string{}
	byRank := map[int]*learnedRule{}
	for i := range learned {
		byRank[learned[i].Row.Rank] = &learned[i]
	}
	var ask []tokenDispute
	for _, t := range disputes {
		if t.Terra == nil {
			disputed++ // the names split between several LLM cities: ours is kept
			continue
		}
		ask = append(ask, t)
	}
	if len(ask) == 0 {
		return 0, disputed, "", newVersions
	}
	items := make([]secondItem, len(ask))
	for i, t := range ask {
		items[i] = secondItem{Token: t.Token, Regex: byRank[t.Rank].Row.Pattern, Examples: t.Examples}
	}
	if !r.take() {
		return 0, disputed + len(ask), "second opinion not asked: " + r.stoppedReason(), newVersions
	}
	res, aerr := r.api.chat(ctx, r.second, secondPrompt(d.Domain, items))
	r.addCost(res.Cost)
	sec := answerRecord{Kind: kindSecond, Domain: d.Domain, RequestID: rec.RequestID, Model: r.second, Hosts: d.Hosts,
		RuleVersions: rec.RuleVersions, TokensIn: res.TokensIn, TokensOut: res.TokensOut, Cost: res.Cost}
	for _, it := range items {
		sec.Patterns = append(sec.Patterns, it.Examples...)
	}
	sec.Hash = patternsHash(sec.Patterns)
	if aerr != nil {
		r.apiFailure(aerr)
		sec.Status, sec.Verdict, sec.Reason = statusIncomplete, verdictError, aerr.Reason
		if aerr.Kind == apiStopCredit {
			sec.Verdict = verdictStopped
		}
		r.save(ctx, sec)
		return 0, disputed + len(ask), "second opinion failed: " + aerr.Reason, newVersions
	}
	answers := parseSecond(res.Content)
	sec.Answer = answers
	// Tokens whose second opinion names the same country within 100 km of the first model's city.
	agreed := map[int][]tokenDispute{}
	for _, t := range ask {
		c, ok := answers[t.Token]
		var p *geoPlace
		if ok {
			p, _ = geo.resolve(c.Country, c.Region, c.City)
		}
		if p != nil && sameArea(t.Terra, p, sameCityKm) {
			agreed[t.Rank] = append(agreed[t.Rank], t)
		} else {
			disputed++
		}
	}
	for _, rank := range sortedInts(agreed) {
		n, rejected, nv := r.applyOverrides(ctx, d, byRank[rank], rows, agreed[rank])
		overrides += n
		disputed += rejected
		if n > 0 {
			newVersions[byRank[rank].Row.Version] = nv
		}
	}
	sec.Status, sec.Verdict = statusComplete, verdictRejected
	sec.Reason = "no override"
	if overrides > 0 {
		sec.Verdict, sec.Reason = verdictAccepted, ""
	}
	sec.Overrides, sec.Disputed = overrides, disputed
	r.save(ctx, sec)
	return overrides, disputed, "", newVersions
}

func sortedInts[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (r *proposeRun) stoppedReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped != "" {
		return r.stopped
	}
	return "no request left"
}

// applyOverrides sets the agreed tokens of one learned rank to the first model's city, as
// origin llm codes scoped to that rank's extraction. A manual code is never overridden; the
// effective map is checked again (the domestic gate) and a withdrawn version cannot return.
// It returns how many tokens were overridden, how many were refused and the learned row's new version.
func (r *proposeRun) applyOverrides(ctx context.Context, d *domainInfo, lr *learnedRule, rows []llmRuleRow, items []tokenDispute) (applied, refused int, newVersion string) {
	if lr == nil || !lr.HasScp {
		return 0, len(items), ""
	}
	codes := map[string]Code{}
	for t, c := range lr.Row.Codes {
		codes[t] = c
	}
	places := map[string]Place{}
	for t, p := range lr.Places {
		places[t] = p
	}
	var done []tokenDispute
	for _, it := range items {
		if c, has := codes[it.Token]; has && c.Origin == "manual" {
			refused++
			continue
		}
		codes[it.Token] = Code{Country: it.Terra.Country, City: it.Terra.Name, Origin: "llm", Scope: lr.Scope, GeoNameID: it.Terra.ID}
		places[it.Token] = Place{it.Terra.Country, it.Terra.Name}
		done = append(done, it)
	}
	if len(done) == 0 {
		return 0, refused, ""
	}
	newVersion = version(lr.Row.Pattern, lr.Row.Dictionary, places)
	for _, row := range rows {
		if row.Rank <= MaxRank && slicesContains(row.Rejected, newVersion) {
			return 0, refused + len(done), ""
		}
	}
	agree := agreement(d.Domain, d.Names, lr.Re, places)
	if isDomestic(uniqueNames(d.Names, d.Domain)) && (agree == nil || *agree < domesticMinAgree) {
		return 0, refused + len(done), ""
	}
	ok, stored := storeCall(r, func() (bool, error) {
		return r.store.UpdateLearnedRow(ctx, d.Domain, rowUpdate{Rank: lr.Row.Rank, OldVersion: lr.Row.Version, NewVersion: newVersion, Codes: codes, Places: places, CountryAgree: agree})
	})
	if !stored || !ok {
		return 0, refused + len(done), ""
	}
	return len(done), refused, newVersion
}

// ProposeCodes is the propose-codes task: it asks the model about the router names of every
// eligible domain and stores the accepted answers as rank-4 rules (see docs/rdns-llm-codes-plan.md).
func (p *Pipeline) ProposeCodes(ctx context.Context, opts ProposeOptions) (ProposeOutput, error) {
	started := time.Now()
	if p.OpenRouterKey == "" {
		p.log("propose-codes: skipped, no OPENROUTER_API_KEY")
		return ProposeOutput{Skipped: "no key"}, nil
	}
	if err := p.CheckPGSQL(); err != nil {
		return ProposeOutput{}, err
	}
	conn, err := pgx.Connect(ctx, p.PGSQL)
	if err != nil {
		return ProposeOutput{}, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	defer conn.Close(ctx)
	if err = schema.EnsureRDNSCityRules(ctx, conn); err != nil {
		return ProposeOutput{}, safeDBError("ensure city rules schema", err)
	}
	if err = schema.EnsureRDNSLLMAnswers(ctx, conn); err != nil {
		return ProposeOutput{}, safeDBError("ensure llm answers schema", err)
	}
	byDomain, _, err := readDomainNames(ctx, conn)
	if err != nil {
		return ProposeOutput{}, err
	}
	if err = loadRouterCountries(ctx, conn, byDomain); err != nil {
		return ProposeOutput{}, safeDBError("read router countries", err)
	}
	infos := make(map[string]*domainInfo, len(byDomain))
	for d, names := range byDomain {
		infos[d] = buildDomainInfo(d, names)
	}
	base := p.OpenRouterURL
	if base == "" {
		base = openRouterBase
	}
	httpClient := p.llmHTTP
	if httpClient == nil {
		httpClient = newOpenRouterHTTP(requestTimeout)
	}
	model, second := p.OpenRouterModel, p.OpenRouterSecondModel
	if model == "" {
		model = DefaultModel
	}
	if second == "" {
		second = DefaultSecondModel
	}
	load := func(ctx context.Context) (*geoIndex, error) {
		if p.testGeo != nil {
			return p.testGeo, nil
		}
		return loadGeoIndex(ctx, filepath.Join(p.DataDir, "rdnsrules", "dictionaries"), p.client, p.clock().UTC())
	}
	run := &proposeRun{store: &pgProposeStore{conn: conn}, api: &openRouter{base: base, key: p.OpenRouterKey, http: httpClient, backoff: p.llmBackoff},
		model: model, second: second, loadGeo: load, now: p.clock, progress: p.Progress, parallel: llmParallel}
	out, err := run.execute(ctx, infos, opts)
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	if err != nil {
		return out, err
	}
	p.log("propose-codes: %d eligible, %d asked in %d requests, %d accepted (%d small), %d no city, %d rejected, %d blocked, %d errors, %d incomplete, %d disagreements, %d overrides, %d disputed, $%.4f, stopped=%q",
		out.Eligible, out.Asked, out.Requests, out.Accepted, out.SmallAccepted, out.NoCity, sumCounts(out.Rejected), out.Blocked, out.Errors, out.Incomplete, out.Disagreements, out.Overrides, out.Disputed, out.CostUSD, out.Stopped)
	return out, nil
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// Default models (OPENROUTER_MODEL, OPENROUTER_SECOND_MODEL).
const (
	DefaultModel       = "openai/gpt-6.1-terra"
	DefaultSecondModel = "openai/gpt-6-luna"
)
