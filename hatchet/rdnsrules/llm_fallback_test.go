package rdnsrules

import (
	"context"
	"strings"
	"sync"
	"testing"
)

const noCityAnswer = `{"location": false, "regex": "", "codes": {}, "comment": "none"}`

// modelLog counts the chat requests per model.
type modelLog struct {
	mu sync.Mutex
	n  map[string]int
}

func (m *modelLog) add(model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.n == nil {
		m.n = map[string]int{}
	}
	m.n[model]++
}

func (m *modelLog) get(model string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n[model]
}

func fallbackRun(t *testing.T, store *fakeStore, api *fakeAPI, fb string) (*proposeRun, func()) {
	run, closeSrv := newTestRun(t, store, api)
	run.fallback = fb
	return run, closeSrv
}

func TestFallbackAcceptsAfterNoCity(t *testing.T) {
	store := newFakeStore()
	var ml modelLog
	api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
		ml.add(model)
		if model == "m/haiku" {
			return 200, completion(terraAnswer(terraRegex), 0.001)
		}
		return 200, completion(noCityAnswer, 0.01)
	}}
	run, closeSrv := fallbackRun(t, store, api, "m/haiku")
	defer closeSrv()
	out, err := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Asked != 1 || out.Requests != 2 || out.Accepted != 1 || out.FallbackAsked != 1 || out.FallbackReqs != 1 || out.FallbackOK != 1 || out.NoCity != 1 {
		t.Fatalf("%+v", out)
	}
	if _, ok := store.llmRule["carrier.net"]; !ok {
		t.Fatal("rank-4 rule not stored")
	}
	if len(store.answers) != 2 || store.answers[0].Model != "m/terra" || store.answers[1].Model != "m/haiku" ||
		store.answers[1].Verdict != verdictAccepted || store.answers[1].Kind != kindPropose {
		t.Fatalf("%+v", store.answers)
	}
}

func TestFallbackNotAskedAfterAccept(t *testing.T) {
	store := newFakeStore()
	var ml modelLog
	api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
		ml.add(model)
		return 200, completion(terraAnswer(terraRegex), 0.01)
	}}
	run, closeSrv := fallbackRun(t, store, api, "m/haiku")
	defer closeSrv()
	out, _ := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
	if out.Requests != 1 || out.FallbackReqs != 0 || ml.get("m/haiku") != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestFallbackAfterAPIError(t *testing.T) {
	store := newFakeStore()
	var ml modelLog
	api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
		ml.add(model)
		if model == "m/haiku" {
			return 200, completion(terraAnswer(terraRegex), 0.001)
		}
		return 500, `{"error":{"message":"boom"}}`
	}}
	run, closeSrv := fallbackRun(t, store, api, "m/haiku")
	defer closeSrv()
	out, err := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ml.get("m/haiku") != 1 || out.FallbackOK != 1 || out.Errors != 1 || out.Accepted != 1 {
		t.Fatalf("%+v", out)
	}
}

func packInfos() map[string]*domainInfo {
	return map[string]*domainInfo{
		"a.de": domainWith("a.de", "DE", "gw.muc"),
		"b.de": domainWith("b.de", "DE", "gw.par"), // rejected: small domain, other country
		"c.de": domainWith("c.de", "DE", "gw.zzz"), // no city
	}
}

const packMain = `{"domains": {
	"a.de": {"location": true, "regex": "^[a-z0-9-]+\\.([a-z]+)\\.a\\.de$", "codes": {"muc": {"country": "DE", "region": "", "city": "Munich"}}},
	"b.de": {"location": true, "regex": "^[a-z0-9-]+\\.([a-z]+)\\.b\\.de$", "codes": {"par": {"country": "FR", "region": "", "city": "Paris"}}},
	"c.de": {"location": false}}}`

func TestFallbackPackAsksOnlyTheDomainsThatNeedIt(t *testing.T) {
	store := newFakeStore()
	var ml modelLog
	var fbPrompt string
	api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
		ml.add(model)
		if model == "m/haiku" {
			fbPrompt = prompt
			return 200, completion(`{"domains": {"b.de": {"location": false}, "c.de": {"location": false}}}`, 0.001)
		}
		return 200, completion(packMain, 0.006)
	}}
	run, closeSrv := fallbackRun(t, store, api, "m/haiku")
	defer closeSrv()
	out, err := run.execute(context.Background(), packInfos(), ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ml.get("m/haiku") != 1 || out.FallbackAsked != 2 || out.Asked != 3 || out.Requests != 2 || out.Accepted != 1 {
		t.Fatalf("%+v", out)
	}
	if strings.Count(fbPrompt, "=== domain ") != 2 || strings.Contains(fbPrompt, "a.de") || store.answer("b.de", kindProposeSmall) == nil {
		t.Fatalf("prompt:\n%s", fbPrompt)
	}
	last := store.answers[len(store.answers)-1]
	if last.Model != "m/haiku" || last.Kind != kindProposeSmall {
		t.Fatalf("%+v", last)
	}
}

func TestFallbackBlockedPackRetriesPerDomainThenFallsBackPerDomain(t *testing.T) {
	store := newFakeStore()
	var mu sync.Mutex
	var fbDomains []int
	var ml modelLog
	blocked := `{"error":{"code":403,"metadata":{"reasons":["x"],"flagged_input":"y"}}}`
	api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
		ml.add(model)
		if model == "m/haiku" {
			mu.Lock()
			fbDomains = append(fbDomains, strings.Count(prompt, "=== domain "))
			mu.Unlock()
			return 200, completion(`{"domains": {}}`, 0.001)
		}
		return 403, blocked
	}}
	run, closeSrv := fallbackRun(t, store, api, "m/haiku")
	defer closeSrv()
	infos := map[string]*domainInfo{"a.de": domainWith("a.de", "DE", "gw.muc"), "b.de": domainWith("b.de", "DE", "gw.par")}
	out, err := run.execute(context.Background(), infos, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 1 pack + 2 single retries on the main model, then 2 single fallbacks (never a pack of two).
	if ml.get("m/terra") != 3 || ml.get("m/haiku") != 2 || out.FallbackReqs != 2 || out.FallbackAsked != 2 || out.Asked != 2 {
		t.Fatalf("%+v terra=%d haiku=%d", out, ml.get("m/terra"), ml.get("m/haiku"))
	}
	for _, n := range fbDomains {
		if n != 1 {
			t.Fatalf("fallback pack of %d", n)
		}
	}
}

func TestFallbackNotAfterCreditStopOrAuthError(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"credit": {403, `{"error":{"code":403,"message":"limit","metadata":{"limit_source":"openrouter_key_limit"}}}`},
		"auth":   {401, `{"error":{"message":"bad key"}}`},
	} {
		store := newFakeStore()
		var ml modelLog
		api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
			ml.add(model)
			return tc.status, tc.body
		}}
		run, closeSrv := fallbackRun(t, store, api, "m/haiku")
		_, _ = run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
		closeSrv()
		if ml.get("m/haiku") != 0 || ml.get("m/terra") == 0 {
			t.Errorf("%s: haiku %d terra %d", name, ml.get("m/haiku"), ml.get("m/terra"))
		}
	}
}

func TestFallbackDisabled(t *testing.T) {
	for name, fb := range map[string]string{"off": "off", "same as main": "m/terra"} {
		if got := fallbackModel(fb, "m/terra"); got != "" {
			t.Errorf("%s: fallbackModel = %q", name, got)
		}
	}
	if fallbackModel("", "m/terra") != DefaultFallbackModel || fallbackModel("m/x", "m/terra") != "m/x" {
		t.Fatal("default or explicit fallback wrong")
	}
	store := newFakeStore()
	var ml modelLog
	api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
		ml.add(model)
		return 200, completion(noCityAnswer, 0.01)
	}}
	run, closeSrv := fallbackRun(t, store, api, "")
	defer closeSrv()
	out, _ := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
	if out.Requests != 1 || out.FallbackReqs != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestFallbackNotSentAtTheRequestCap(t *testing.T) {
	store := newFakeStore()
	var ml modelLog
	api := &fakeAPI{remaining: "0.006", chat: func(model, prompt string, n int) (int, string) {
		ml.add(model)
		return 200, completion(noCityAnswer, 0.01)
	}}
	run, closeSrv := fallbackRun(t, store, api, "m/haiku")
	defer closeSrv()
	out, err := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ml.get("m/haiku") != 0 || out.Requests != 1 || len(store.answers) != 1 || out.NoCity != 1 {
		t.Fatalf("%+v answers=%d", out, len(store.answers))
	}
}

func TestFallbackNoCityDoesNotAskAgain(t *testing.T) {
	store := newFakeStore()
	var ml modelLog
	api := &fakeAPI{chat: func(model, prompt string, n int) (int, string) {
		ml.add(model)
		return 200, completion(noCityAnswer, 0.01)
	}}
	run, closeSrv := fallbackRun(t, store, api, "m/haiku")
	defer closeSrv()
	out, _ := run.execute(context.Background(), map[string]*domainInfo{"carrier.net": carrierDomain()}, ProposeOptions{})
	if out.Requests != 2 || ml.get("m/haiku") != 1 || out.FallbackOK != 0 || out.NoCity != 2 {
		t.Fatalf("%+v", out)
	}
}
