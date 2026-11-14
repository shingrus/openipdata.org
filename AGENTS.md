# AGENTS.md

## Overview

- This repo holds the openipdata.org data workers, workflows, and builders. There is no website or web interface here.
- Workflows are being moved here from another repo step by step. This repo must stay self-contained: no imports, submodules, URLs, or file references to other repos. Reproduce anything needed here.

## Layout

- `infra/`: VM provisioning (`init-hatchet-runner-vm.sh`), systemd unit, env template. No Go code.
- `hatchet/`: Go module for `openipdata-worker`, the Hatchet worker. `main.go` registers workflows; each workflow lives in its own folder (`hatchet/asn/` for `build-asn-data`, `hatchet/pdb/` for `sync-peeringdb`, `hatchet/domains/` for `update-asn-domains`, `hatchet/geofeeds/` for `sync-geofeeds`, `hatchet/geo/` for `build-geo-data`, `hatchet/rdns/` for `resolve-ips`, `hatchet/rdnsrules/` for `learn-rdns-rules` with its tasks `learn` and `propose-codes`). `hatchet/schema/` creates and upgrades the shared Postgres tables; `hatchet/publish/` uploads artifacts to R2 and GitHub releases (a file that is already there byte-for-byte is not sent again) and downloads the R2 layer files; `hatchet/rirstats/` parses RIR delegated-extended stats and splits address ranges into prefixes; `hatchet/download/` downloads a file unless the local copy is current (the caller passes the HTTP client); `hatchet/outputs/` prunes old output directories and has the small helpers `asn` and `geo` share (last log lines, file size); `hatchet/ipdata/` has what several workflows need about IP space and `public.asn` (public-address check, country-code check, announced prefixes of `public.asn.json`, the `seen_at` filter). Workflows share this code, never each other's downloaded files.
- `mmdbbuilder/`: Go module for `mmdbbuilder`, a standalone CLI (`package main`) with one subcommand per database (`asn`, `geo`), plus `eval` (scores a geo MMDB) and `lookup` (reads one: a report, or with `-in`/`-out` the city of each IP of a file).
- `tools/llm-codes-test/`: scratch Python test (standard library only), not run by the worker: compares LLMs (through OpenRouter, key only in `OPENROUTER_TEST_API_KEY`) on proposing city codes for router-name domains. It sends router hop names only, never visitor names; its exports and answers are git-ignored.
- No `go.mod` or Go code at the repo root. Each module builds on its own.
- `hatchet` must not import `mmdbbuilder`; it runs the `mmdbbuilder` binary (and, for `resolve-ips`, `scamper`) found next to its own executable.

## Hatchet terms

- Use Hatchet's current terms: workflow, task, worker, run. Do not use "job" or "step".

## VM

- System user/group `openipdata`, no login shell.
- Binaries: `/opt/openipdata/bin/openipdata-worker`, `/opt/openipdata/bin/mmdbbuilder` and `/opt/openipdata/bin/scamper` (root, 0755, not setuid). `scamper` is built by the init script from the pinned upstream tarball with `--disable-scamper-privsep` (the apt package is setuid and cannot start under the unit); `resolve-ips` runs the one next to the worker executable, never one from the PATH. The unit grants only `CAP_NET_RAW` (`AmbientCapabilities`, `CapabilityBoundingSet`), `NoNewPrivileges` stays; the init script runs a smoke trace under those limits.
- Env file: `/etc/openipdata/worker.env` (root:openipdata, 0640); template in `infra/`.
- State: `/var/lib/openipdata` (openipdata, 0750); the only writable path for the service.
- Build cache: `/var/cache/openipdata-build` (root, 0700): pinned Go toolchain plus module/build caches; safe to delete.
- `infra/init-hatchet-runner-vm.sh` is idempotent and never overwrites an existing env file.

## Rules

- Keep it simple: read env vars directly in `main.go`; no config packages or frameworks.
- Use the standard library where possible (e.g. `encoding/json`).
- Postgres is the source of truth; the VM only keeps caches and the last few builds.
- Output files (MMDB, SQLite) are rebuilt from scratch each run and written via a `.tmp` file plus rename.
- One writer per table; every other workflow only reads it:
  `public.asn` ← `build-asn-data`, `public.asn_geo_pdb` ← `sync-peeringdb`, `public.asn_domain` ← `update-asn-domains`,
  `public.geo_ix_pdb` ← `sync-peeringdb`, `public.geo_eval` ← `build-geo-data`, `public.geofeed_urls` ← `sync-geofeeds`,
  `public.visitor_ips` ← external websites insert; `resolve-ips` is its only deleter (exception to one writer: it never inserts or updates),
  `public.network_traces`, `public.traced_networks`, `public.ip_ptr_names` and `public.rdns_geo_layer` ← `resolve-ips`,
  `public.rdns_city_rules` and `public.rdns_llm_answers` ← `learn-rdns-rules`.
- Schema changes go in `hatchet/schema` (idempotent `IF NOT EXISTS` statements), never by hand. A workflow runs DDL only
  for tables it writes: `ALTER TABLE` takes an ACCESS EXCLUSIVE lock even when the column exists.
- Do not delete rows from `public.asn`; ASNs missing from the source are kept for now.
  `build-asn-data` sets `public.asn.seen_at` for every ASN in the source each run; readers use it to ignore old rows
  (NULL = not yet seen since the column was added).
- `public.asn_geo_pdb` mirrors PeeringDB: `sync-peeringdb` replaces it in one transaction each run, together with
  `public.geo_ix_pdb` (global unicast internet exchange peering LAN prefixes with the exchange's country and city).
  An asn_geo_pdb shrink refuses the whole replace; a geo_ix_pdb shrink or empty result only keeps the old
  geo_ix_pdb rows (`StoreStats.IXKept`) and asn_geo_pdb is still replaced.
- `build-geo-data` only reads these tables and runs DDL only for `public.geo_eval` (`schema.EnsureGeoEval`), never `schema.Ensure`. A missing or empty `public.geo_ix_pdb`
  skips the `50-ix` layer for that run, and missing facility columns or no matching network skip `40-hosting`
  (`Source.SkippedLayers`); a `50-ix` of 1 to 199 usable rows fails, `40-hosting` has no minimum. Exchanges without a
  city are never written to `50-ix`.
  `asn_geo_pdb.fac_count`/`fac_city`/`fac_country` (facilities only, city set only if all are in one city) feed `40-hosting`.
- ip2geo layers have ONE format and ONE name in every stage: a gzipped CSV in the geofeed (RFC 8805) layout, header
  `prefix,country,region,city,source`, named `NN-name.csv.gz` (`10-rir`, `20-ipverse`, `30-pdb`, `35-rdns-wide`, `40-hosting`,
  `50-ix`, `60-whois`, `65-rdns`, `70-geofeeds`; steps of 5 or 10, the name is the order). `build-geo-data` `download` empties its layers dir, each
  source writes its file through the one shared writer (`.tmp` plus rename), and the R2 layers are put in as they are (see the R2 rule below: the last download of each stays in `geo/downloads/` with its ETag, and an unchanged object is copied from there).
  The build passes every `*.csv.gz` in the dir, sorted by name, to `mmdbbuilder geo` (one reader, columns found by header);
  there is no per-layer code and no layer list in the build. Special rules live only in `layerRules` (`geo/pipeline.go`),
  one line per layer name; a rule for an absent file is not passed. A new layer is a new file name, not new code.
  Optional layers (`35-rdns-wide`, `40-hosting`, `50-ix`, `60-whois`, `65-rdns`) that are absent are listed in
  `Source.SkippedLayers` for reporting (the two rDNS layers are written from `public.rdns_geo_layer` by scope, both
  with `-same-country`: they add cities, never change a country); an R2 layer (`60-whois`, `70-geofeeds`) that is missing or older than 3 days skips the `HC_PING_GEO` ping.
- `public.asn_domain` holds the final domain per ASN (`source` = `pdb` or `rdap`); PeeringDB wins over RDAP.
  Domain cleanup rules (registered domain, rejection list) live in `hatchet/domains/clean.go`.
- `resolve-ips` (cron every 30 minutes, tasks `pick-networks` → `trace` → `resolve-names` → `locate`, own `rdns` limit) reads and deletes rows of
  `public.visitor_ips` (DDL only `schema.EnsureVisitorIPs`: `CREATE TABLE IF NOT EXISTS`, so it never locks the table the websites write; deletes: `trace` removes all but the newest visitor IP
  (`last_seen_at`, then `ip`) of each /24 it just stored traces for, in the transaction that stores them, and `locate` removes every visitor IP of a `located` /24 after its commit; counts only in the outputs),
  `public.generated_rules` (read only; a missing table means no rules; a rule without domains applies to every name) and `public.asn` (only the IPv4 prefixes of `json`, extracted in SQL, kept in memory), and runs
  DDL only for its own tables (`schema.EnsureRDNS`), never `schema.Ensure`. That DDL runs once per workflow run, in the first task
  (`pick-networks`, which the `run-rdns` command also starts with: `connectEnsure`); `trace`, `resolve-names` and `locate` open their connections
  with no DDL (`connect`), because `ALTER TABLE ... IF NOT EXISTS` takes an ACCESS EXCLUSIVE lock even when the column exists, and `build-geo-data` reads `public.rdns_geo_layer`. IPv4 only, global unicast only. Visitor IPs and every
  traced IP stay in Postgres and in the batch file under `/var/lib/openipdata/rdns/`: never in Hatchet task inputs, outputs, log
  lines or returned errors (tasks report counts; errors that could quote an IP are replaced by `dbError` or `scrubIPs`).
  `pick-networks` writes the batch file that `trace` reads. `public.network_traces.candidates` (`inet[]`, added by `schema.EnsureRDNS`) holds the candidates of a trace (the target, then the last quarter of the
  public hops, closest first): `trace` stores them with the path, and `locate` reads only `prefix`, `target_kind`, `target` and `candidates`, never `hops` or `hop_ips`; `resolve-names` reads the same plus `hop_ips`, never `hops`
  (`public.network_traces.hop_ips`, `inet[]`, added by `schema.EnsureRDNS`: every public answering hop of the path, target left out, each IP once; `trace` stores it, rows with NULL are filled once from `hops` at the start of `resolve-names` only; the stat `reached_asn` is "a tail hop candidate, not the target itself, is in the target's ASN"). Rows written before the column existed (NULL) are filled once from `hops`
  in chunks at the start of `resolve-names` (and of a `locate` that rebuilds). `resolve-names` names the candidates and every public hop (`hop_ips`) of the newest trace per prefix and kind of the last 7 days, never replacing an `ok`/`not_found` name by an `error`; `locate` reads only the names of its candidates. `locate` replaces `public.rdns_geo_layer` in one transaction and
  refuses a shrink of more than 10% of the sticky rows (traced rows with method `rdns_rule`, which a legacy rule located) when the table had any (`ErrShrink`; `force_replace` overrides). Candidate traced rows and all wide rows are volatile: recomputed every rebuild and re-learned rules change them daily, so they are not guarded; `previous_volatile` and `stored_volatile` in the output report their counts before and after. `locate` is skipped (`skipped` in its output, table untouched) when a fingerprint of its inputs
  (count and newest timestamp of the traces of the 180-day window, of the usable `ip_ptr_names` and of the current ASNs, and hashes of `generated_rules` and `rdns_city_rules` content/version/status, all computed in SQL)
  equals the one saved after the last rebuild and that rebuild is less than 24 hours old; the fingerprint and the rebuild time are in `/var/lib/openipdata/rdns/locate-state.json`
  (`.tmp` plus rename, written only after a committed rebuild; losing it only costs a rebuild). `force_replace` always rebuilds.
  **MMDB hop fallback**: `locate` reads its own copy of our published ip2geo MMDB from the private R2 bucket (`Pipeline.R2`, the `R2` env setting), never `build-geo-data`'s files on the VM: it
  stats today's (UTC) dated key `<YYYY-MM-DD>/ip2geo-openipdata-<YYYYMMDD>.mmdb`, else yesterday's, keeps the object as `/var/lib/openipdata/rdns/ip2geo.mmdb` with its ETag in `ip2geo.mmdb.etag`, and downloads it
  (`publish.R2.DownloadIfChanged`) only when the ETag differs and only when `locate` rebuilds. The ETag of the MMDB that is read (or `none`) is part of the fingerprint, so a new MMDB triggers a rebuild.
  An R2 outage must not change the rows (a rebuild without the MMDB votes could hit `ErrShrink`): when R2 cannot be used (no R2 setting, a Stat error, no object for both days, or a failed download) the saved copy is
  read instead if `ip2geo.mmdb` has its ETag beside it, and that saved ETag is in the fingerprint (`mmdb: saved copy` and `mmdb_reason` in the output, a logged warning; after a failed download the fingerprint names the saved copy,
  so the next run tries R2 again). Only with no usable saved copy, or when `mmdbbuilder lookup` exits non-zero on it (the copy and its ETag are then deleted), is the fallback skipped (`mmdb: skipped`, fingerprint `none`
  so the next run tries again); `locate` still succeeds.
  A /24 that no trace of gets a rule match casts ONE wide vote (never a traced row) for the city of the first router hop (never the target itself; traces by kind order, hops closest first, same-ASN hops first,
  as `matchTrace` tries them) that has a city in the MMDB; no origin ASN, no vote. Rule votes and MMDB votes count the same. `locate` writes the hop IPs (only those of no-match /24s, plus the winning
  router of rule-matched /24s for the agreement counts `mmdb_compared`/`mmdb_agreed`) to `rdns/mmdb-hops.txt`, runs `mmdbbuilder lookup -db <mmdb> -in <ips> -out <tsv>` (`ip<TAB>country<TAB>city`, only IPs with a city;
  invalid lines are skipped and counted on stderr, never echoed) and removes both files; the core `locate()` stays pure and gets the hop-to-city map. `public.rdns_geo_layer.method` is `rdns_rule` for traced rows a legacy rule found, `candidate_rule` for traced rows a learned candidate found (and for a wide row only candidates and MMDB hops voted for, with `candidate_votes`), and
  `rdns_rule` for a wide row that at least one legacy rule voted for, `mmdb_hop` for a wide row of MMDB votes only; wide rows also hold `rule_votes` and `mmdb_votes` for their city (the columns are added by `schema.EnsureRDNS`).
  `build-geo-data` reads prefix, country and city by scope and ignores these columns.
  **Schedule** (`public.traced_networks`, one row per traced /24, `resolve-ips` the only writer, created by `schema.EnsureRDNS`; pure rules in `rdns/schedule.go`): `status` is `silent` (no hop answered in the /24's new traces),
  `unmatched` (hops answered, no city) or `located`; `attempts` counts consecutive traces with the same non-located status; `next_trace_at` NULL means "only when a visitor comes again". Silent: next trace after 7 days, at most 3 attempts, then NULL.
  Unmatched: after attempt 1, 2, 3 wait 7, 20, 60 days, then NULL. NULL rows are traced again only when a visitor of the /24 has `last_seen_at` newer than `last_traced_at` and that trace is at least 60 days old (attempts restart).
  Located: recheck 150 days after the last trace, whatever the visitors do; a recheck keeps the old city unless it finds one. `trace` writes silent/provisional `unmatched` (a located /24 stays located) in the transaction that stores the traces;
  `locate` makes the /24s of its traced rows `located` in the transaction that replaces the layer. `pick-networks` (limit 1000, `PickOutput.Limit`) picks, in this order: /24s with no row and a visitor seen in the last 30 days (most visitors first),
  then rows with `next_trace_at <= now()` (oldest first), then visitor-revived NULL rows (oldest trace first). Targets: newest visitor IP (if the /24 has one; it is optional in the batch file), `.1` and a random host. The first `pick-networks` after the table
  was added (table empty) backfills it from `network_traces` and `rdns_geo_layer` (located if the layer has a traced row, else unmatched/silent from the newest traces, attempts 1). `trace` also deletes `network_traces` rows older than 200 days and
  `traced_networks` rows that are not located, have no `next_trace_at` and were last traced more than 200 days ago, and reports `path_compared`/`path_changed` (the two newest traces per prefix and kind, candidates without the target; counts only).
  **Located /24s stay**: `locate` only uses the newest round of traces of a /24 (those within an hour of its newest), keeps every traced row of the current layer that a legacy rule found (never a `candidate_rule` row) whose /24 no trace located in this rebuild (same columns, same `located_at`) and
  counts it as a rule vote for its announced prefix (a carried /24 casts no other vote); a row located by a trace gets that trace's time as `located_at`. The shrink guard counts the carried rows (they are sticky). `resolve-names` also looks up again up to 500
  answers per run older than 30 days that belong to located /24s (legacy traced rows only, never `candidate_rule` rows: routers of traced rows, candidates of their newest traces), oldest first (`refreshed`), with the same no-overwrite-on-error rule. Every lookup sets `public.ip_ptr_names.tried_at` (an error that does not overwrite a stored `ok`/`not_found` row moves only `tried_at`); the refresh skips IPs tried in the last 7 days, so failing names cannot hold its slots. Freshness for the normal lookups stays on `resolved_at`. `public.ip_ptr_names.error_count` (added by `schema.EnsureRDNS`) counts failed lookups in a row (each error adds one, also when it only moves `tried_at` on a stored `ok`/`not_found` row; `ok`/`not_found` reset it to 0): an IP whose last lookup failed is not looked up again until `tried_at` plus 1 day (1 error), 2 days (2) or 7 days (3 or more); an old `error` row with NULL count is a first failure. The refresh uses the same wait, and a bounded retry pass looks up due failures that no trace of the last 7 days names any more but a 180-day trace candidate or a carried traced router still does. `resolve-names` reports `errors`, `errors_timeout`, `errors_servfail`, `errors_other` (kind of the final answer after the DoH fallback), `backed_off` and `retried`, counts only.
  When a Hatchet run picked its full limit and `trace` stored traces, its successful `locate` task starts a new `resolve-ips` run (not a child; `chained` in the output; the `rdns` limit queues it behind the current run); never from `run-rdns`.
- `learn-rdns-rules` (daily 05:00 UTC, task `learn`, own `rdns-rules` limit; CLI `run-rdns-rules`) reads the PTR names of every IP of the traces of the last 180 days (target, candidates and `hop_ips`, each IP once; `hop_ips` is skipped while the column does not exist), successful PTR names and current ASN prefixes. It learns up to 3 extractions per registered domain (`rdns_city_rules` keyed by `(domain, rank)`, rank 1 to 3): rank 1 on all names, rank 2 on the names rank 1 does not decode, rank 3 likewise, each from at least 3 distinct IPs with distinct names that decode on those names (the regex is still checked against all the domain's names); `locate` tries a domain's ranks in order. Stop list (`roleTokens` in `rdnsrules/learn.go`): router role tokens (`ccr`, `core`, `gw`, ...), common network/product words (`ip`, `pool`, `sub`, `isp`, `res`, `biz`, ...) and all US state and Canadian province codes are never decoded from any dictionary (scoped manual/LLM overrides still can). GeoNames only gives tokens of 4+ letters and places of at least 15,000 people (applied after the 10x namesake rule). An extraction needs at least 2 distinct decoded token values at every rank (no single-valued rules). Domestic gate: when at least 80% of a domain's routers with a known country are in one country, a rule needs `country_agree` >= 0.7 (global carriers stay ungated); names with a label outside `[a-z0-9-]` are skipped (`names_skipped`); the output separates `unsupported` (observed domains below the floor) from `lost_support` (stored rules with no current names, kept) and `retired` (see below). The announced-prefix read is shared in `hatchet/ipdata` (Postgres 15 compatible). OurAirports, UN/LOCODE and GeoNames have their own monthly cache in `rdnsrules/dictionaries/`, never another workflow's files. Only this workflow writes `public.rdns_city_rules` and runs `schema.EnsureRDNSCityRules`, which fails with the fixed `schema.ErrOldRuleLayout` (never drops or alters) when the table has the old per-domain layout without `rank`. Pattern, effective token-to-city mappings and version are published atomically; failed refreshes and loss of support (a domain with NO current names) keep old rules. For a domain WITH current names, a stored `origin = learned` row this run does not reproduce is retired: `status = rejected` and its version added to `rejected_versions` (so it cannot come back unchanged; a row whose content moved to another rank is deleted as a stale duplicate); manual/LLM rows are never retired. Candidate rows are volatile in `locate`, so its shrink guard is unaffected. Unchanged content preserves status/measurements (also when it moves to another rank), changed content becomes a candidate; a rank the learner no longer produces keeps its last row only while the domain has no current names. Scoped manual/LLM overrides of any rank of a domain retain provenance and apply to the rank that now has their extraction; entry and active promotion are future work.
  `run-rdns-rules -reject example.net` (comma-separated) or `reject_domains` workflow input withdraws the current versions of every rank of the domain without downloads/learning. Rejected content hashes are retained per row, and a version withdrawn at one rank cannot return at another. `resolve-ips` reads the table without DDL; missing means no candidates. Only `candidate` rows are consumed in v1, after all legacy matches and carried rows, before MMDB fallback. For a /24 with no legacy match and no carried row, a candidate is tried on the target names of its traces first (by kind), then on router hops (same ordering as MMDB; hops need a known target origin), a domain's ranks in order; a match casts at most one wide vote per /24 and, when it is a target or a hop of the target's ASN, also writes a traced row with method `candidate_rule` (`rule_name` `<domain>#<rank>`). Those rows are never sticky (never carried, recomputed each rebuild; `readTracedRows`, `markLocatedSQL`, the schedule backfills and the refresh/retry reads of located names ignore them) and never mark a network located; only `generated_rules` rows are sticky and located. All votes weigh equally. Wide rows count `candidate_votes`; method is `rdns_rule` if any legacy vote supports the winner, else `candidate_rule` if any candidate vote does, else `mmdb_hop`. Fingerprinting includes rank/effective mappings/version/status, so withdrawal removes the rows and recalculates votes on the next rebuild, subject to the existing shrink guard (only the sticky rows are guarded, so a withdrawal of candidate rules never needs `force_replace`; the swing shows in `previous_volatile`/`stored_volatile`). Output reports candidate votes, winning wide rows, and agreement against existing rules on the same winning router. Active learned rules must gain sticky-row provenance/revocation before being enabled.
- `learn-rdns-rules` has a second task, `propose-codes` (after `learn`, same `rdns-rules` limit; CLI `run-rdns-rules -codes [-domains a,b] [-force]`; workflow input `domains` and `force`; skipped with `reject_domains`; plan in
  `docs/rdns-llm-codes-plan.md`). It asks an LLM through OpenRouter (IPv4 only, 180 s timeout, JSON, temperature 0, reasoning low) which part of a domain's PTR names is a city code, for every registered domain with current names
  (5+ hosts one domain per request; 1-4 hosts packed, about 30 per request, one announced country per pack, unknown country alone). **Only patterns are sent, never a name or an IP**: per label, hex runs (6+ characters with a digit, or exactly 7 or 8)
  become `x`, then decimal runs `0` (`hatchet/rdnsrules/llmpatterns.go`; a required unit test checks that no octet survives in any encoding), distinct patterns, top 300 per domain; the real names stay on the VM and test the regex.
  Env (env file only, read in `main.go`): `OPENROUTER_API_KEY` (a secret; absent: `skipped: no key`), `OPENROUTER_MODEL` (default `openai/gpt-6.1-terra`), `OPENROUTER_SECOND_MODEL` (default `openai/gpt-6-luna`); the key never appears in logs, outputs, errors or the table
  (HTTP errors are replaced by fixed texts). A domain is asked when it has no `complete` answer, its newest question is incomplete, a current learned row (ranks 1-3, not withdrawn) has a version missing from the `rule_versions` of its newest complete answer (no wait; an override's new version is recorded), or its pattern set changed and the last complete answer is 30+ days old (`force` always); biggest first, at most 400 requests a run (`stopped: request_cap`),
  fewer when `GET /api/v1/key` shows less credit (about $0.005 a request). An answer is used only after the checks (RE2 regex, one group, anchored, letters-only captures on the real names, 2+ tokens and 20%+ decoded names, small domains: the announced country,
  ISO country, ONE GeoNames place per city by country+region+name or the 10x namesake rule, the learner's domestic gate). GeoNames `cities1000` and `admin1CodesASCII` come from the learner's own monthly cache (`rdnsrules/dictionaries/`).
  An accepted answer is the domain's **rank-4 row** of `public.rdns_city_rules` (`origin = llm`, `dictionary = llm`, `status = candidate`); the learner never reads, writes, retires or deletes rank 4 (`rank <= 3` in its queries), `-reject` withdraws it like every rank,
  and a withdrawn version cannot return. Learned rows are compared per name with rank 4; a disagreement (other country or 100+ km apart) goes to the second model, and when it names the same country within 100 km the learned token gets an `origin = llm` code scoped to its
  extraction (never over a `manual` code; the domestic gate is checked again; effective at once, and kept by the learner). `schema.EnsureRDNSCityRules` widens the rank check to 1-4 only when the old constraint exists.
  `public.rdns_llm_answers` (DDL `schema.EnsureRDNSLLMAnswers`, written only by `learn-rdns-rules`) records every question; only `status = complete` answers count, so an error, a stop or a failed second opinion is asked again next run (verdicts: `accepted`, `rejected`, `no_city`, `blocked`, `error`, and `stopped` for domains a credit stop reached, which are not counted as errors).
  API trouble never fails the task: 403 with `limit_source` key/workspace/guardrail budget and 402 end the run quietly (`stopped: credit_limit`), moderation/other 403 give the domain the verdict `blocked` (a blocked pack is asked again one domain at a time),
  429/5xx/timeouts retry 3 times then count as `error`; only a database error and an invalid key (401, fixed `ErrInvalidKey`) fail the task. The output has counts only.
- Workflows that write the ASN tables share the `asn-db` concurrency limit. `sync-geofeeds` writes only `public.geofeed_urls`
  (DDL only through `schema.EnsureGeofeedURLs`, never `schema.Ensure`; its `probe-domains` task only reads
  `public.asn_domain` and `public.asn`) and has its own `geofeeds` limit; `build-geo-data` only reads them and has its own `geo` limit.
- `public.geofeed_urls` is the one list of every geofeed URL ever found (the table pre-dates this repo and its definition
  must not change). `discover` upserts into it (new URL: insert; known URL: `last_seen_at = now()` and `sources` = sorted
  distinct union, never a removed source) and never deletes rows. `check-feeds` writes only `last_checked_at`,
  `last_success_at`, `last_fetch_status` (our status names) and `result_text` (`HTTP 200, 3412 rows`, `HTTP 404`, or a short
  error), in batches. Its backoff and the 30-day working window depend only on the timestamps (failing since
  `last_success_at`, or `first_seen_at` if it never worked), never on the status text, so rows written by an older tool
  just come due. The stored feed copies on the VM are only caches.
- Data between workflows goes through Postgres tables, except the geofeed rows (millions of rows) and the whois geoloc
  rows (city rows for RIPE, APNIC and AFRINIC `inetnum` objects with a `geoloc:`): `sync-geofeeds` uploads them as the
  layer files `70-geofeeds.csv.gz` and `60-whois.csv.gz`, each to the fixed key `internal/layers/<name>` in the private R2
  bucket, and `build-geo-data` reads them from there (no fallback to older keys).
  Both uploads put the file's sha256 in the object's user metadata (`X-Amz-Meta-Sha256`); when R2 already holds the same sha256 the bytes are not sent, and a server-side copy
  onto itself renews the object's `LastModified` (`refreshed` in the task output), because `build-geo-data` treats an object older than 3 days as stale. `force_upload` always sends the bytes.
  `build-geo-data` keeps the last download of each layer in its own `/var/lib/openipdata/geo/downloads/` with the object's ETag beside it (`<name>.etag`); `Stat` first, and an object
  with the saved ETag is not downloaded again (the saved copy is copied into `layers/`, which is still rebuilt each run); the age check always uses the object's `LastModified` from `Stat`.
  `sync-geofeeds` downloads GeoNames `cities1000.zip` itself (7-day age gate, own `dumps/` dir) and names the whois cities
  in `discover`; a problem with the whois rows (GeoNames, gate, upload) only skips them (the previous `60-whois` file is
  kept and is not uploaded again) and is reported in the task output, never blocks the geofeed rows.
  A workflow's own caches and state on the VM (e.g. `/var/lib/openipdata/geofeeds/`) are never read by another workflow.
- `sync-geofeeds` HTTP and FTP requests use IPv4 only and must never reach non-public addresses (feed URLs come from
  whois records anyone can write). Nothing it builds is published. Its RDAP results live in `rdap.jsonl` on the VM,
  not in Postgres. `probe-domains` looks for feeds on the websites in `public.asn_domain` (source `domain`); its results
  live in `domains.jsonl`, and the announced prefixes of the ASNs of found domains in `domain-prefixes.csv`, so
  `export-rows` reads no ASN table. A feed that only this source found adds no whois blocks and keeps only rows
  inside those prefixes (drop reason `outside_asn`).
- `build-geo-data` scores each MMDB it builds (`evaluate` task, results in `public.geo_eval`) and `publish` refuses a
  clear drop against the last published row (`geo.ErrGate`). All MMDB reading is done by `mmdbbuilder` (`eval`, `lookup`), never by `hatchet`.
  The holdout leaves out a frozen sample of feeds, read from `public.geofeed_urls` (`id % 10 = 0`, `first_seen_at` before the
  freeze date in `hatchet/geo/holdout.go`); a failed read is an evaluation error, never a different sample. Moving the date moves the score once.
  The other references are DB-IP, MaxMind GeoLite2 City and IP2Location LITE DB11 (`MAXMIND_LICENSE_KEY`, `IP2LOCATION_TOKEN`, both optional; skipped when unset), all scored like ours (coverage, Atlas, holdout looked up in the reference itself). The MaxMind and IP2Location URLs hold the key/token: any error from those requests is replaced by a fixed message (only an HTTP status stays), never passed on. Their archives are requested at most every 3 and 7 days (`<archive>.checked`); a 200 answer that is not a valid archive is a failed download (IP2Location then waits 1 day, `<archive>.failed`).
- Published files are never sent twice: `build-asn-data` and `build-geo-data` upload with the sha256 in R2 user metadata and skip the upload when the object at the same dated key has the same sha256
  (a new day's folder still gets its upload); for the rolling GitHub release they skip replacing an asset whose reported `digest` (`sha256:<hex>`) equals the file's (no digest: replace as before).
  Gates, the `public.geo_eval` published flag and the health-check pings behave as if the file was published (an identical file is the published file); the task outputs say `unchanged`.
- Secrets live only in the env file. Never put secrets or visitor IPs in Hatchet task inputs, outputs, log lines, or returned errors.
- Pass every error from `pgx.Connect` / `ParseConfig` through `schema.HideDSN`: pgx parse errors can echo the whole `PGSQL` value.
- The `R2` setting holds the secret key: errors about it use the fixed `publish.ErrInvalidR2`, never the value.
