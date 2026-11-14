# rDNS city codes from an LLM: plan

Implemented 2026-11-14 (task `propose-codes` of `learn-rdns-rules`), after an external review of the
draft. Builds on `docs/rdns-rules-plan.md` (the learner without an LLM) and on the model test in
`tools/llm-codes-test/` (GPT-6.1 Terra chosen). Where the code differs from the first draft, see
Deviations at the end.

## Goal

Cities for the router and target names the dictionary learner cannot decode: operator codes
(`lnd` = London on cw.net, `nycmny` = New York on Verizon), small operators, naming conventions a
learned rule misses, and learned codes whose dictionary guess is wrong (`fnt` = Flint instead of
Frankfurt). Greedy: every domain is asked, even one with a single host; checks decide what is used.

## Where it runs

A second task `propose-codes` in `learn-rdns-rules`, after `learn` (same daily schedule, same
`rdns-rules` limit, same single writer of `public.rdns_city_rules`).

- API trouble never fails the task: it is counted, and the task ends successfully (see Spending for the
  stop rules). Only database errors and an invalid key (HTTP 401, fixed error text) fail it. `learn`'s
  results stand either way.
- No `OPENROUTER_API_KEY`: the task is skipped (`skipped: no key` in the output). A run with
  `reject_domains` skips it too.
- Manual use: workflow input `{"domains": ["sfr.net"], "force": true}` (runs `learn` first), or the CLI
  `run-rdns-rules -codes [-domains a,b] [-force]`, which runs only this task; `force` asks again whatever
  the record says.

## Config (env file only)

| Setting | Default | Use |
| --- | --- | --- |
| `OPENROUTER_API_KEY` | none | the key (with its own credit limit on OpenRouter) |
| `OPENROUTER_MODEL` | `openai/gpt-6.1-terra` | the questions |
| `OPENROUTER_SECOND_MODEL` | `openai/gpt-6-luna` | second opinion before an override |

Requests: OpenRouter chat completions over IPv4 only (the VM's IPv6 egress stalls: a connection can open
and then hang, which Go's dual-stack fallback does not catch), JSON response format, temperature 0,
reasoning effort low, max 16,000 output tokens, 180 s timeout, up to 4 requests at a time. The key never
appears in logs, outputs or errors.

## What is sent: name patterns, never IPs

The names of a domain are its hosts' PTR names: router hops (`hop_ips`) and traced targets of the last
180 days, status `ok`, labels only `[a-z0-9-]`. The domain of a name is its registered domain (public
suffix list, as in the learner). Each name becomes a pattern before it leaves the VM, label by label,
in this order:

1. **Hex runs → `x`.** A run of `[0-9a-f]` that is 6+ characters long and contains a digit, or that is
   exactly 7 or 8 characters long (an IPv4 address in hex, also with no digit or with the leading zero
   dropped: `abcdefab` = 171.205.239.171, `abbccdd` = 10.187.204.221). A 7- or 8-letter word made of
   a-f only (`defaced`) is redacted too. `p57ba8141.dip0.t-ipconnect.de` → `px.dip0.t-ipconnect.de`.
2. **Decimal runs → `0`.** Every remaining run of digits: `pool-71-183-45-12.nycmny.fios.verizon.net`
   → `pool-0-0-0-0.nycmny.fios.verizon.net`.

The prompt says what the placeholders stand for: `0` is `[0-9]+`, `x` is `[0-9a-f]+`. Equal patterns
are sent once, at most 300 per domain (the most frequent first). The regex is always tested on the real
names locally; they never leave the VM.

**Privacy test** (unit test): for a set of IPs, names built in every common encoding (dotted, dashed,
reversed, zero-padded, hex, hex with letters only, hex with the leading zero dropped, mixed into labels
like `ip-71-183-45-12`, `syn-072-129-061-099`, `p57ba8141`) must give patterns that contain none of the
IP's octets in decimal or hex, and the model-side regex for the placeholders must match the original
names.

## Which domains are asked

There is one kind of question, for every domain:

| Domains | Asked |
| --- | --- |
| 5+ hosts | one domain per request |
| 1-4 hosts ("small") | packed, about 30 domains per request, all of the same country (the majority country of their hosts' announced prefixes; unknown country: alone) |

A domain is asked when:

- it never had a complete answer;
- its newest answer is incomplete (an error, a credit stop, a failed second opinion), whatever its
  patterns and age;
- one of its current learned rows (ranks 1-3, not `rejected`) has a version that is not in the
  `rule_versions` of its newest complete answer (no wait);
- or its pattern set changed and its last complete answer is at least 30 days old.

Same patterns, same answer: otherwise a domain is not asked again (`force` overrides). This holds for
every outcome, "no city" included, and for domains with a rank-4 rule, so new cities come in when the
names change. Biggest domains first; at most 400 requests per run.

## Prompt and answer

The prompt of the test (`tools/llm-codes-test/llm_codes_test.py`), plus the placeholders, plus: the
capture group takes letters only; give the region (first-level subdivision) of each city. Packed
requests list several domains and get one answer object per domain. Answer per domain:

```json
{"location": true, "regex": "...", "codes": {"lnd": {"country": "GB", "region": "England", "city": "London"}}, "comment": "..."}
```

Code keys are trimmed and lowercased; when two keys fold to the same token, the first in sorted order
is kept.

## Checks before an answer is used

1. The regex compiles in Go (RE2), has exactly one capture group, is anchored and ends with
   `\.<domain>$`.
2. Run on the domain's real names: every capture is letters only; at least 2 distinct decoded tokens
   occur and at least 20% of the names decode. **Small domains** instead: at least one name decodes,
   and each decoded name's country equals its announced prefix's country where that is known.
3. Every country is ISO 3166-1 alpha-2, and every city **resolves to one GeoNames place** (`cities1000`
   plus `admin1CodesASCII.txt`, both in the learner's monthly dictionary cache; names and alternate
   names): by country, region and name; without a region match, the most populous place only if it has
   10x the population of the next one (the learner's namesake rule); otherwise the code is dropped as
   ambiguous. The code is stored under GeoNames' ASCII name, with its GeoNames ID.
4. The domestic gate of learned rules: when 80%+ of the domain's routers with a known country are in
   one country, `country_agree` must be at least 0.7.
5. Stop-list tokens (`ny`, `ca`, ...) are allowed (they were blocked only for dictionary guesses).
6. A version in `rejected_versions` cannot come back.

## How an answer becomes a rule

**Rank 4.** An accepted answer is the domain's rank-4 row: `origin = llm`, `dictionary = llm`, `status =
candidate`, the model's regex as `pattern`, its resolved codes as `places` (same `[country, city]` format
as learned rows; the GeoNames IDs stay in `codes` and in the record). The learner owns ranks 1-3 and never
reads, replaces, retires or deletes rank 4. `locate` tries ranks in order, so on a domain with a learned
rule the rank-4 rule decodes what ranks 1-3 do not (new codes, other naming conventions). A newer
accepted answer replaces the row. `schema.EnsureRDNSCityRules` widens the rank check to 1-4 (guarded:
only when the old constraint is there).

**Comparison with learned rules, per name.** For a domain with learned ranks, every real name is run
through the learned rank that decodes it (rank order, as `locate` does) and through the new rank-4
rule. Where both give a city, they disagree when the countries differ or the cities are more than 100 km
apart (GeoNames coordinates). Within 100 km ours is kept (usually the finer name: Esenyurt vs
Istanbul). Each disagreement belongs to exactly one learned (rank, token).

**Second opinion and override.** The disagreeing tokens of a domain go to the second model in one small
request (the token, the learned regex and example patterns; the second model is shown neither city).
When it gives the **same country and a city within 100 km** of the first model's, that city becomes an
`origin = llm` code scoped to that rank's extraction (the existing override mechanism), the learned
row's effective map is checked again (rules 3 and 4 above) and it takes effect in the same run. A
`manual` code is never overridden. Otherwise ours is kept and the token counts as disputed; so does a
token whose names the first model splits between several cities.

In `locate` nothing changes: rank-4 rows are candidates like learned ones (volatile, a wide vote and a
`candidate_rule` traced row on a target or same-ASN hop match, `rule_name` `<domain>#4`). `-reject`
withdraws rank 4 like every rank.

## Record: `public.rdns_llm_answers`

One row per domain and question, written only by `learn-rdns-rules`:

`id`, `asked_at`, `kind` (`propose`, `propose_small`, `second_opinion`), `domain`, `request_id` (shared
by a packed request), `model`, `hosts`, `patterns_sent`, `patterns_hash`, `rule_versions` (every current
learned version of the domain, after any override this answer applied), `answer` jsonb (that domain's
part, with resolved GeoNames IDs), `status` (`complete` or `incomplete`), `verdict` (`accepted`,
`rejected`, `no_city`, `blocked`, `error`, `stopped`), `reason`, `disagreements`, `overrides`,
`disputed`, `tokens_in`, `tokens_out`, `cost_usd`.

A question counts as answered only with `status = complete`: the answer was checked, and for a domain
with disagreements the second opinion came back and was applied. An error, a credit stop (`stopped`), or
a failed second opinion leaves it `incomplete`, and it is asked again on the next run. "Checked" for a
learned rule = a complete answer whose `rule_versions` holds its current version.

## Spending and errors

The status alone is not enough on OpenRouter; the task branches on `error.metadata.limit_source` and
the other metadata (OpenRouter docs, errors and debugging):

| Response | Meaning | What the task does |
| --- | --- | --- |
| 403 with `limit_source` `openrouter_key_limit`, `openrouter_workspace_budget` or `openrouter_guardrail_budget` | the key's limit or a budget is used up | stop asking at once, keep everything stored, end successfully with `stopped: credit_limit` |
| 402 (no `limit_source`) | out of credit | the same stop |
| 402 with `limit_source` `openrouter_in_flight_budget` | too much in flight | retry only when `Retry-After` is set, else the same stop |
| 403 with `reasons` / `flagged_input` | moderation block | that domain's verdict is `blocked` (complete); a blocked pack is asked again one domain at a time |
| other 403 | permissions or guardrail | `blocked` for that domain; continue |
| 401 | invalid key | fail the task with a fixed error |
| 429, 5xx, timeouts | temporary | retry with backoff (3 tries), then `error` for that domain |

- Before the run: `GET /api/v1/key` (`limit_remaining`); the number of requests is capped by the
  remaining credit at about $0.005 per request (a request measured about $0.0024 in the first run).
- At most 400 requests per run, second opinions included (a guard against our own bugs);
  `stopped: request_cap` when reached.
- Domains of a request that hit a credit stop get the verdict `stopped` and are counted in
  `stopped_domains`, not in `errors`.

## Output and log

Output (counts only): `eligible`, `asked`, `requests`, `accepted`, `small_accepted`, `rejected` (by
reason), `no_city`, `blocked`, `errors`, `incomplete`, `stopped_domains`, `compared_rules`,
`disagreements`, `overrides`, `disputed`, `cost_usd`, `key_remaining_usd`, `stopped`, `skipped`.

Log: one line per finished first-model request, `propose-codes: request of "<domain>" done n/total
(kind)` (a pack shows its first domain and `+N`), plus a summary line at the end. No IPs or real names.

## First run (2026-11-14)

400 requests (the cap), 380 domains asked: 111 accepted, 184 no city, 70 rejected (29 domestic gate,
20 too few tokens, 15 low decoded, 3 anchor, 3 no resolvable codes), 15 errors; 46 learned rules
compared, 67 disagreements, 34 overrides, 33 disputed; $0.96. On cw.net the second model confirmed
London, Frankfurt, Dublin, Munich and others, replacing the dictionary's Lander, Flint and Baicheng.

## Caveats

- **Plausible but wrong cities** for opaque codes. The country checks, the GeoNames resolution and
  `-same-country` in the geo build limit the harm: rDNS never changes a country.
- **PTR names are written by anyone**, and wrong cities can reach other people's IPs: a router hop casts a
  wide vote for the target's announced prefix (up to a /16), and in a packed request one domain's names
  can influence the answers for the others. Bounds: labels are `[a-z0-9-]`; each answer is checked
  against its own domain's names; packs hold domains of one country only and small-domain answers must
  match the announced country; wide rows need a majority of votes; the geo build keeps the country.
  So a bad answer can move cities within a country, not across one.
- **Model changes**: each answer records the model; a new model does not re-ask by itself (`force`).
- **Proof**: the baseline is `geo_eval` id 21 (city_pct 30.51, holdout_city_pct 20.03); after the first
  build with LLM rules compare `holdout_city_pct`, the Atlas scores and `dbip_city_agree_pct`.

## Deviations from the first draft

- Cities are stored under the GeoNames ASCII name, like the learner's.
- A non-ISO or unknown country rejects the whole answer; an unknown or ambiguous city only drops that
  code.
- Disagreements are counted per learned (rank, token); a token is disputed only when more of its names
  disagree than agree. A learned city GeoNames cannot place counts as a disagreement unless the names
  are equal.
- The second model is asked what each token stands for (learned regex and example patterns), without
  either city; its record is kind `second_opinion`.
- `blocked` counts as complete (not asked again until the patterns change).
- Up to 4 requests run in parallel; the task's execution timeout is 8 hours.
- A workflow run with `domains` or `force` still runs `learn` first; only the CLI `-codes` runs
  `propose-codes` alone.

## Later

Promotion of LLM rows to sticky (`active`) needs ground truth (Atlas) and revocation; IPv6 names; batch
API if the volume grows 100x.
