package rdnsrules

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgProposeStore is proposeStore on one Postgres connection (the run serializes the calls).
type pgProposeStore struct{ conn *pgx.Conn }

func (s *pgProposeStore) LastAnswers(ctx context.Context) (map[string]lastAnswer, error) {
	out := map[string]lastAnswer{}
	// The newest complete answer of each domain.
	rows, err := s.conn.Query(ctx, `SELECT DISTINCT ON (domain) domain, patterns_hash, asked_at, rule_versions
		FROM public.rdns_llm_answers WHERE status = 'complete' AND kind IN ('propose', 'propose_small')
		ORDER BY domain, asked_at DESC, id DESC`)
	if err != nil {
		return nil, safeDBError("read llm answers", err)
	}
	for rows.Next() {
		var d string
		a := lastAnswer{HasComplete: true}
		if err = rows.Scan(&d, &a.Hash, &a.AskedAt, &a.RuleVersions); err != nil {
			rows.Close()
			return nil, safeDBError("read llm answers", err)
		}
		out[d] = a
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, safeDBError("read llm answers", err)
	}
	// Domains whose newest question of all is incomplete are asked again.
	rows, err = s.conn.Query(ctx, `SELECT domain FROM (
			SELECT DISTINCT ON (domain) domain, status FROM public.rdns_llm_answers
			WHERE kind IN ('propose', 'propose_small') ORDER BY domain, asked_at DESC, id DESC) newest
		WHERE status <> 'complete'`)
	if err != nil {
		return nil, safeDBError("read llm answers", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err = rows.Scan(&d); err != nil {
			return nil, safeDBError("read llm answers", err)
		}
		a := out[d]
		a.NewestIncomplete = true
		out[d] = a
	}
	if err = rows.Err(); err != nil {
		return nil, safeDBError("read llm answers", err)
	}
	return out, nil
}

func (s *pgProposeStore) LearnedVersions(ctx context.Context) (map[string][]string, error) {
	rows, err := s.conn.Query(ctx, `SELECT domain, version FROM public.rdns_city_rules WHERE rank <= 3 AND status <> 'rejected'`)
	if err != nil {
		return nil, safeDBError("read city rule versions", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var d, v string
		if err = rows.Scan(&d, &v); err != nil {
			return nil, safeDBError("read city rule versions", err)
		}
		out[d] = append(out[d], v)
	}
	if err = rows.Err(); err != nil {
		return nil, safeDBError("read city rule versions", err)
	}
	return out, nil
}

func (s *pgProposeStore) Rules(ctx context.Context, domain string) ([]llmRuleRow, error) {
	rows, err := s.conn.Query(ctx, `SELECT rank,version,status,origin,pattern,dictionary,codes,places,rejected_versions
		FROM public.rdns_city_rules WHERE domain=$1 ORDER BY rank`, domain)
	if err != nil {
		return nil, safeDBError("read city rules", err)
	}
	defer rows.Close()
	var out []llmRuleRow
	for rows.Next() {
		r := llmRuleRow{Codes: map[string]Code{}, Places: map[string][2]string{}}
		var rank int16
		if err = rows.Scan(&rank, &r.Version, &r.Status, &r.Origin, &r.Pattern, &r.Dictionary, &r.Codes, &r.Places, &r.Rejected); err != nil {
			return nil, safeDBError("read city rules", err)
		}
		r.Rank = int(rank)
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, safeDBError("read city rules", err)
	}
	return out, nil
}

func (s *pgProposeStore) SaveAnswer(ctx context.Context, rec answerRecord) error {
	answer, _ := json.Marshal(rec.Answer)
	if rec.Answer == nil {
		answer = []byte("null")
	}
	patterns, versions := rec.Patterns, rec.RuleVersions
	if patterns == nil {
		patterns = []string{}
	}
	if versions == nil {
		versions = []string{}
	}
	_, err := s.conn.Exec(ctx, `INSERT INTO public.rdns_llm_answers
		(asked_at,kind,domain,request_id,model,hosts,patterns_sent,patterns_hash,rule_versions,answer,status,verdict,reason,disagreements,overrides,disputed,tokens_in,tokens_out,cost_usd)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		rec.AskedAt, rec.Kind, rec.Domain, rec.RequestID, rec.Model, rec.Hosts, patterns, rec.Hash, versions, string(answer),
		rec.Status, rec.Verdict, rec.Reason, rec.Disagreements, rec.Overrides, rec.Disputed, rec.TokensIn, rec.TokensOut, rec.Cost)
	if err != nil {
		return safeDBError("save llm answer", err)
	}
	return nil
}

// llmRank is the rank of the LLM rule of a domain; the learner owns ranks 1 to MaxRank.
const llmRank = MaxRank + 1

func placesArray(places map[string]Place) map[string][2]string {
	out := make(map[string][2]string, len(places))
	for t, p := range places {
		out[t] = [2]string{p.Country, p.City}
	}
	return out
}

func (s *pgProposeStore) StoreLLMRule(ctx context.Context, domain string, w llmRuleWrite) (string, error) {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return "", safeDBError("begin llm rule update", err)
	}
	defer tx.Rollback(ctx)
	// A version withdrawn at any rank of the domain cannot come back.
	var withdrawn []string
	rows, err := tx.Query(ctx, `SELECT unnest(rejected_versions) FROM public.rdns_city_rules WHERE domain=$1`, domain)
	if err != nil {
		return "", safeDBError("read withdrawn versions", err)
	}
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			rows.Close()
			return "", safeDBError("read withdrawn versions", err)
		}
		withdrawn = append(withdrawn, v)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return "", safeDBError("read withdrawn versions", err)
	}
	v := version(w.Regex, "llm", w.Places)
	if slicesContains(withdrawn, v) {
		return rejRejectedVersion, nil
	}
	var oldVersion, oldStatus string
	var oldRejected []string
	found := true
	err = tx.QueryRow(ctx, `SELECT version,status,rejected_versions FROM public.rdns_city_rules WHERE domain=$1 AND rank=$2 FOR UPDATE`, domain, llmRank).Scan(&oldVersion, &oldStatus, &oldRejected)
	if err == pgx.ErrNoRows {
		found, err = false, nil
	}
	if err != nil {
		return "", safeDBError("read llm rule", err)
	}
	if found && oldVersion == v {
		return "", nil // the same rule: keeps its status and measurements
	}
	rejected := oldRejected
	if rejected == nil {
		rejected = []string{}
	}
	if found && oldStatus == "rejected" {
		rejected = appendUnique(append([]string{}, rejected...), oldVersion)
	}
	places, _ := json.Marshal(placesArray(w.Places))
	codes, _ := json.Marshal(w.Codes)
	_, err = tx.Exec(ctx, `INSERT INTO public.rdns_city_rules(domain,rank,pattern,dictionary,codes,places,version,names,decoded,distinct_places,country_agree,status,origin,learned_at,rejected_versions)
		VALUES($1,$2,$3,'llm',$4::jsonb,$5::jsonb,$6,$7,$8,$9,$10,'candidate','llm',$11,$12)
		ON CONFLICT(domain,rank) DO UPDATE SET pattern=EXCLUDED.pattern,dictionary=EXCLUDED.dictionary,codes=EXCLUDED.codes,places=EXCLUDED.places,version=EXCLUDED.version,names=EXCLUDED.names,decoded=EXCLUDED.decoded,distinct_places=EXCLUDED.distinct_places,country_agree=EXCLUDED.country_agree,truth_hits=NULL,truth_misses=NULL,precision=NULL,measured_at=NULL,status='candidate',origin='llm',learned_at=EXCLUDED.learned_at,rejected_versions=EXCLUDED.rejected_versions`,
		domain, llmRank, w.Regex, string(codes), string(places), v, w.Names, w.Decoded, w.Distinct, w.CountryAgree, time.Now().UTC(), rejected)
	if err != nil {
		return "", safeDBError("write llm rule", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return "", safeDBError("commit llm rule", err)
	}
	return "", nil
}

func (s *pgProposeStore) UpdateLearnedRow(ctx context.Context, domain string, u rowUpdate) (bool, error) {
	places, _ := json.Marshal(placesArray(u.Places))
	codes, _ := json.Marshal(u.Codes)
	// Changed content becomes a candidate again and loses its measurements, as in the learner.
	tag, err := s.conn.Exec(ctx, `UPDATE public.rdns_city_rules SET codes=$4::jsonb,places=$5::jsonb,version=$6,country_agree=$7,
			status='candidate',truth_hits=NULL,truth_misses=NULL,precision=NULL,measured_at=NULL,learned_at=now()
		WHERE domain=$1 AND rank=$2 AND version=$3 AND rank <= 3 AND status <> 'rejected'`,
		domain, u.Rank, u.OldVersion, string(codes), string(places), u.NewVersion, u.CountryAgree)
	if err != nil {
		return false, safeDBError("update learned city rule", err)
	}
	return tag.RowsAffected() == 1, nil
}
