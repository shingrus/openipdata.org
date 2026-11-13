# rDNS rules without an LLM

Greedy candidate v1 implemented, 2026-11-13. The "Greedier v1.1" section below (decided 2026-11-13) overrides the earlier sections where they differ. Validation and active-rule promotion remain future work.

## Greedier v1.1 (overrides the sections below where they differ)

1. **Candidates also locate the /24 itself.** In `locate`, after `generated_rules` found nothing for a /24, a candidate match gives the /24 a traced row in `65-rdns` (method `candidate_rule`) as well as its wide vote. These rows are **not sticky**: they are recomputed on every rebuild from the current rules and traces, are never carried over, and never set `traced_networks.status = located` (the /24 keeps its silent/unmatched schedule, so it is still retraced). Withdrawing or changing a rule therefore removes or changes its rows on the next rebuild; no provenance-based revocation is needed. Only `generated_rules` matches remain sticky `located` rows.
2. **Several naming conventions per domain.** The learner keeps up to 3 extractions per domain, each meeting the support floor on names that no better-ranked extraction of that domain already decodes. `rdns_city_rules` gets one row per (domain, rank) instead of one per domain; `locate` tries a domain's rules in rank order. Rejection works per domain (all its rules) and per version.
3. **Target names count.** Learning also uses the PTR names of trace targets (customer-pool names such as `pool-….nycmny.fios.verizon.net`), and in `locate` a candidate is tried on the target's own name first, then on router hops in the existing order.
4. **All public hops are named.** `resolve-names` looks up every public answering hop of the newest traces (not only the last quarter), so the learner sees more names per domain. `locate` matching still uses the candidates (target + last quarter of public hops) as before.
5. **Daily learning.** `learn-rdns-rules` runs every day at 05:00 UTC instead of Sundays.

Kept as they are: `-same-country` on both rDNS layers, the role-token stop list, the 10x GeoNames population rule, the 3-router floor.

## Goal

Learn one rule per operator domain that turns router names into cities, from the names we already have and public place-code lists. No LLM, no paid API in v1. An LLM can later propose operator-specific mappings.

**V1 is greedy: use learned candidates immediately to add wide votes, before Atlas validation exists.** Prefer additional city coverage, record uncertainty, and correct rules as evidence arrives. Candidates fill gaps after existing rule matches and before the MMDB hop fallback; they never create traced rows. The later layers still take precedence over `35-rdns-wide`.

The old `generated_rules` hold one regex per city (`comcast_newark`, `comcast_boston`, …). A learned rule is different: **one regex per domain that captures a location token, plus a dictionary that turns the token into a city.** V1 learns the best single naming convention per domain; it need not decode every name of that operator.

## Inputs

| Input | Where | Notes |
| --- | --- | --- |
| Router names | `public.ip_ptr_names` (`status = ok`), grouped by registered domain | join to router-hop candidates of recent traces; exclude IPs seen only as targets; count each IP once |
| Router country | current announced IPv4 prefixes in `public.asn` | diagnostic only; never a hard gate for global carriers |
| IATA codes | OurAirports `airports.csv` (public domain), plus IATA metro codes (`lon`, `nyc`, `par`, …) | 3 letters |
| UN/LOCODE | UNECE code list | 2-letter country + 3 letters, e.g. `defra` |
| City names | GeoNames `cities1000` (own copy) | ASCII names and common alternate names; a name shared by several places resolves to the most populous one only if it is at least 10 times larger than the next (London → GB, Frankfurt → Frankfurt am Main), otherwise it stays unresolved |
| Validation evidence (later) | `atlas_router_locations` (routers within 1 ms of a RIPE Atlas probe) | proximity evidence, not proof of an exact city; record coordinates, observation time and uncertainty |

CLLI codes (`nycmny`, `lsanca`) have no free official list; they are left out of the first version and handled as learned `codes` (see below).

## How a rule is learned (per domain)

1. **Normalize the name**, strip the registered domain, and split the rest into labels. Within each label, split on hyphens and into letter and digit runs. Record label positions from the right and letter-token positions from the left:

   ```
   be2433.ccr41.fra03.atlas.cogentco.com
   labels from the right:  L4=be2433  L3=ccr41  L2=fra03  L1=atlas
   letter tokens:          be         ccr       fra       atlas
   ```

2. **Try every (label position, letter-token position, dictionary) combination** on all names of the domain. Label positions are counted from the right, because names differ in length on the left. For each name the selected token either decodes unambiguously to a place or stays unresolved. Learning and application use the same extraction boundaries; do not match arbitrary substrings inside tokens.

3. **Score each combination:**

   - `decode_rate`: share of the domain's names whose token decodes
   - `distinct_places`: how many different places it decodes to; one city is allowed, since an operator may serve only one city
   - `country_agree`: share of decoded names whose country matches the router's country where that is known; diagnostic only, with NULL when none are known

4. **Keep the best supported combination as a candidate.** The initial support floor is 3 distinct router IPs with distinct names that decode. Combinations whose decoded names use at least 2 distinct token values rank first; a single-valued combination wins only when no multi-valued one meets the floor, so one-city operators still get a rule. Within each group, rank by decoded-name count, then distinct places, then a stable ordering of the extraction parameters and dictionary name. Router role and interface tokens (`ccr`, `core`, `edge`, `gw`, `bb`, `agg`, `bng`, `bras`, `pe`, `cr`, `xe`, …) are never decoded from a dictionary (a scoped manual/LLM override can still map them): without this, a token present in every name that happens to be an airport code (Cogent's `ccr` = Concord, US) beats the real location token. Names with a label outside `[a-z0-9-]` left of the registered domain are skipped (`names_skipped`), so one odd name cannot block a domain. The floor and ranking can be tuned from the first run's results.

   - No minimum domain size of 20 names.
   - No 50% decode-rate gate: partial coverage is useful.
   - No minimum of 3 places: one-city rules are useful.
   - No 90% country-agreement gate: ASN/RIR country is often the operator's home country.

5. **Write the rule** as a regex with one capture group for the token, built from the chosen positions. Store the dictionary name for provenance and the resolved mappings for the observed tokens with the rule. Require the generated regex to extract exactly the same tokens as the learner on the input names.

## Rule format

New table `public.rdns_city_rules` (written only by the new workflow `learn-rdns-rules`), one row per domain:

| Column | Example | Meaning |
| --- | --- | --- |
| `domain` | `cogentco.com` | registered domain, the key |
| `pattern` | `^(?:[^.]+\.)*([a-z]+)\d*\.[^.]+\.cogentco\.com$` | built from the chosen label position (here L2); the group captures `fra` |
| `dictionary` | `iata` | `iata`, `locode` or `geonames`; learning source, not a runtime download |
| `codes` | `{"msk": {"country": "RU", "city": "Moscow", "origin": "manual", "scope": {"label_from_right": 2, "token_from_left": 1, "dictionary": "iata"}}}` | scoped per-domain overrides, including tokens a dictionary knows with a different meaning; entry UI/API is future work |
| `places` | `{"fra": ["DE", "Frankfurt am Main"]}` | effective token-to-place mappings, with canonical city names, used by `locate`; unresolved tokens are absent |
| `version` | content hash | identifies the pattern and effective mappings used for a result |
| `names`, `decoded`, `distinct_places`, `country_agree` | `412, 371, 38, 0.97` | learning numbers |
| `truth_hits`, `truth_misses`, `precision` | `24, 1, 0.96` | measured against independent validation evidence, once we have it; NULL before measurement |
| `status` | `candidate` / `active` / `rejected` | candidate: wide votes only; active and rejected: unused in v1; active matching requires future provenance and withdrawal support |
| `origin` | `learned` / `llm` / `manual` | who proposed the extraction rule; `codes` entries have their own origin |
| `learned_at`, `measured_at` | | |
| `rejected_versions` | content hashes | remembers withdrawn versions so relearning cannot silently revive them |

The learner resolves tokens in this order: domain-specific `codes`, then the chosen dictionary, else unresolved. It writes the resulting `places` map with the pattern and version in one transaction. `locate` captures the token and reads `places`; it never reads the learner's files or independently refreshes a dictionary. A regex match without a place is not a successful match and does not block later rules or the MMDB fallback. Newly observed tokens are added by the next learning run.

## Examples

These name shapes are illustrative; the real patterns come from our own `ip_ptr_names`.

| Name | Token | Dictionary | City |
| --- | --- | --- | --- |
| `be2433.ccr41.fra03.atlas.cogentco.com` | `fra` (L2) | IATA | Frankfurt, DE |
| `be3042.ccr22.lon13.atlas.cogentco.com` | `lon` (L2) | IATA metro | London, GB |
| `ae-7.r24.amstnl07.nl.bb.gin.ntt.net` | `amstnl` (L4) | none (CLLI-style) | needs `codes`: `amstnl` → Amsterdam, NL |
| `pool-71-105-12-9.nycmny.fios.verizon.net` | `nycmny` (L2) | none (CLLI) | needs `codes`: `nycmny` → New York, US |
| `frankfurt-de1.core.example-isp.net` | `frankfurt` (L2, first letter token) | GeoNames | Frankfurt am Main, DE |
| `msk-m9-p1.ip.example-isp.ru` | `msk` (L2, first letter token) | none | needs `codes`: `msk` → Moscow, RU |

The first two rows show one extraction rule covering many cities: the Cogent rule locates `fra`, `lon`, `ams`, `jfk`, … through dictionary-derived mappings, with no hand-written regex per city.

## Learning `codes` (operator-specific tokens)

A token at a rule's position may need a new mapping or an override of a known dictionary mapping. For example, a valid airport code can mean a different city in an operator's names. Proposed `codes` entries come from:

- **Atlas evidence:** the same token on at least 3 distinct router IPs consistently near probes in one area, with no contradictory observations. This proposes a mapping; it does not prove exact-city accuracy.
- **An LLM proposal** (later, the paid or free model we choose), initially used under the same candidate policy.
- **Manual (later input API):** supplied to `learn-rdns-rules` with `origin = manual`; the workflow remains the table's only writer. V1 preserves and applies existing scoped overrides, but does not expose an entry command.

Adding or changing an unvalidated mapping makes the changed rule version a candidate. Preserve override provenance across runs; do not silently attach an old override to a different extraction position.

## Measuring and status

- **Before validation exists:** a rule stays `candidate` and is used immediately for wide votes. Store `names`, `decoded` (their ratio is decode rate), and country agreement. Report candidate votes, wide rows with winning candidate votes, and agreement when both a candidate and the existing rule decode the winning legacy router. DB-IP/Atlas validation is later. Agreement is a diagnostic, not measured accuracy; missing evaluation data does not block candidate use.
- **Later promotion:** `active` allows the rule to produce traced rows as well. Before enabling automatic promotion, define a geographic tolerance, require independent evidence across several tokens and places (or across several routers for a one-city rule), and report uncertainty with precision. Split by target router IP, not only probe ID, so the same target cannot be used for both learning and evaluation. Ten hits alone are not a sufficient promotion policy.
- **Rejection:** `run-rdns-rules -reject example.net,example.org` or workflow input `{"reject_domains":["example.net"]}` withdraws the current rule versions without refreshing sources or learning. A changed future version may become a candidate; a withdrawn content hash stays rejected. A failed download or an unavailable evaluation is not a rejection. Sparse or unmeasured evidence leaves a candidate usable.
- Measure hostname decoding and the resulting /24 and wide-prefix assignments separately: a correct router city does not establish the city of every host behind it. Report how many results change, rather than relying only on a global build score.

## Schedule and updates

- `learn-rdns-rules` runs Sundays at 05:00 UTC and re-learns every domain from scratch: new traces bring new domains and names. Atlas evidence can be added later.
- Rows are updated in place by `domain`. Rejected rules are kept, not deleted. A successful learning run that no longer finds enough support leaves the last usable rule in place and reports the loss of support (`lost_support`: stored rules whose domain has no current names; `unsupported`: observed domains below the floor); absence of fresh names is not proof that the old rule is wrong. Preserve manual/LLM overrides with their extraction scope and provenance.
- Dictionaries are downloaded into the workflow's own directory and refreshed monthly (conditional downloads). Changed patterns and effective mappings are published atomically as a new candidate version. Unchanged content preserves its status and measurements; changed content cannot inherit old precision. Failed refreshes leave the published rules untouched.
- `locate` fingerprints the rule content/version and status, including the effective mappings. A change rebuilds the layer from stored traces; candidate votes are recalculated each rebuild, never carried forward as traced rows.
- A withdrawal that removes many wide rows can trip the existing shrink guard. Report the proposed removals; apply an intentional large removal through the existing `force_replace` path after inspecting it. Rejection does not silently bypass the guard.
- **Withdrawal must undo learned results.** Once active learned rules can write traced rows, store their source rule/version on those rows. Re-evaluate carried learned rows against the current rule and mapping: preserve still-supported locations, but replace or remove results whose evidence was withdrawn. Update `traced_networks` when a /24 loses its located row. The current carry-forward behavior alone cannot revoke a bad rule.
- DDL belongs in `hatchet/schema`: `learn-rdns-rules` ensures only its rule table; `resolve-ips` owns changes to its layer and scheduling tables.

## Use in `locate`

For each /24:

1. Try existing `generated_rules` using the current traced/wide matching behavior. Complete this pass across the /24's traces before considering any candidate. A valid carried traced row keeps its normal vote. Active learned rules are deliberately unused in v1 until their traced results can be revoked safely.
2. Only if that produces no rule vote, try candidate rules on router hops in the same order as the MMDB fallback (same-ASN hops first, closest first; never the target itself). The first successful decode casts **one wide vote**, provided the target has a known announced prefix. It never writes a traced row or marks the /24 located. Candidate and other votes have equal weight in v1.
3. If no candidate supplies a city, use the existing MMDB hop fallback. A /24 casts at most one vote across all methods.

Count candidate votes for the winning city separately (`candidate_votes`). Set a wide row's method to `rdns_rule` if that city has any existing rule votes, else `candidate_rule` if it has candidate votes, else `mmdb_hop`. The `35-rdns-wide` layer retains `-same-country` and its existing precedence. Once learned rules cover the same domains with better measured precision, `generated_rules` can be retired (it is written outside this repo).

## V1 scope and later work

- **V1:** dictionary-based candidate learning, immediate wide votes, reproducible mappings in Postgres, content/status fingerprinting, rejection, and coverage/agreement counts.
- **Tune from actual results:** the 3-name support floor, ranking of extraction candidates, ambiguous place names, and recurring false matches. Country agreement remains diagnostic.
- **Later:** Atlas validation and automatic active-rule promotion, multiple naming conventions per domain, and LLM proposals. Active-rule promotion includes the traced-row withdrawal behavior above; none of this is required to start using candidates as wide votes.
