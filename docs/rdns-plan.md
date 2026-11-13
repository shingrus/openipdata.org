# rDNS city layer: plan

Agreed 2026-11-12. Goal: `visitor_city_pct` of 90%+: visitor /24s with a city from a measured layer.

## Flow

```
websites ──insert──▶ visitor_ips ──▶ resolve-ips: pick-networks → trace → resolve-names → locate ──▶ rdns_geo_layer
                                                                                                         │
                                       build-geo-data download ◀───────────────────────────────────────┘
                                       writes 65-rdns.csv.gz (traced /24s) and 35-rdns-wide.csv.gz (announced prefix)
```

## Tables

| Table | Writer | One row | Columns |
| --- | --- | --- | --- |
| `visitor_ips` | external websites | one visitor IP | `ip inet` PK, `source`, `first_seen_at`, `last_seen_at` |
| `network_traces` | `resolve-ips` | one traceroute of one host in a /24 | `prefix` (/24), `target_kind` (`visitor`, `gateway` = `.1`, `random`), `target`, `traced_at`, `hops` jsonb `[{ttl, ip, rtt_ms}]` (full path, `null` ip for no answer); the 2 newest per (`prefix`, `target_kind`) are kept |
| `ip_ptr_names` | `resolve-ips` | the PTR name of one IP (hop or target) | `ip` PK, `ptr` (null = none), `status` (`ok`, `not_found`, `error`), `resolved_at` |
| `rdns_geo_layer` | `resolve-ips` | one located prefix | `(scope, prefix)` PK (an announced `/24` can be both a traced and a wide row), `scope` (`traced`, `wide`), `country`, `city`, `router_ip`, `router_ptr`, `rule_name`, `same_asn` (NULL when the target's ASN is unknown), `located_at` |
| `generated_rules` | written outside this repo (read only here) | one regex rule | `name`, `pattern`, `domains_json`, `country`, `city`, … |

- Visitor IPs stay in Postgres only: never in Hatchet task inputs, outputs, logs or errors (AGENTS.md). Tasks report counts.
- `visitor_ips` DDL: `schema.EnsureVisitorIPs`, `CREATE TABLE IF NOT EXISTS` only (no `ALTER`, no extra index), so `resolve-ips` can run it without locking the websites' inserts.
- Retention: the writing website deletes rows not seen for 90 days (it is the writer).

## How the queue is filled

A website, on every request of an active user:

```sql
INSERT INTO public.visitor_ips (ip, source) VALUES ($1, '<site name>')
ON CONFLICT (ip) DO UPDATE SET last_seen_at = now();
```

Once a day:

```sql
DELETE FROM public.visitor_ips WHERE last_seen_at < now() - interval '90 days';
```

Its database role needs only `SELECT, INSERT, UPDATE, DELETE` on `public.visitor_ips`. There is no queue table: the queue is a query over `visitor_ips` and `network_traces`.

## `resolve-ips` workflow

Cron every 30 minutes, own concurrency limit `rdns`. IPv4 only (the VM's IPv6 egress stalls); IPv6 visitor IPs are stored but not traced yet.

1. **`pick-networks`**: IPv4 /24s with a visitor seen in the last 30 days and no trace in the last 60 days, most visitors first, up to 1,000 per run. No "already known" check: tracing is cheap, and the geofeed layer still wins over `65-rdns`. Global unicast only.
2. **`trace`**: three targets per /24, the newest visitor IP, `.1`, one random host; one `scamper` process (`-O json`, Paris ICMP traceroute, fixed packet rate). Writes `network_traces`, prunes to the 2 newest.
3. **`resolve-names`**: PTR for each target and the last 25% of hops of each new trace, unless `ip_ptr_names` has a fresh answer (ok: 30 days, not found: 7 days). System resolver first, Cloudflare DoH (IPv4) when it fails.
4. **`locate`**: rebuilds `rdns_geo_layer` from traces younger than 180 days, in one transaction; refuses a shrink of more than 10%.
   - Candidates per trace: the target itself, then the last 25% of hops, closest first. A candidate whose origin ASN (from `public.asn` prefixes, offline) equals the target's comes before one that does not.
   - First candidate whose PTR matches a rule (regex, scoped to the rule's domains when it has any, first match wins) gives the city.
   - Same-ASN match → a `traced` row for the /24 and a vote for its announced prefix. Other-ASN match → a vote only.
   - `wide` rows: per announced prefix (never wider than /16), the city with most votes; ties skip.
   - Targets of one /24 that disagree: the same-ASN match closest to the target wins.
   - Output (counts only): /24s traced, located (traced / wide), per target kind: reached the target's ASN, located, unique wins, agreement with the other targets; rules that did not compile in Go.

## `build-geo-data` changes

- `download` writes `65-rdns.csv.gz` (`scope = traced`) and `35-rdns-wide.csv.gz` (`scope = wide`) from `rdns_geo_layer`, like `30-pdb`. Both optional: a missing or empty table lists them in `Source.SkippedLayers`.
- `layerRules`: both `-same-country`; rDNS adds cities, never changes a country.
- `35-rdns-wide` is a trial: keep it only if `holdout_city_pct` and the Atlas scores do not drop.

## VM

- Build `scamper` from the pinned upstream tarball with `--disable-scamper-privsep` into `/opt/openipdata/bin/scamper` (the apt package is setuid and cannot start under the unit); the worker runs the one next to itself. The init script smoke-tests a trace under the unit's limits.
- Unit: `AmbientCapabilities=CAP_NET_RAW`, `CapabilityBoundingSet=CAP_NET_RAW` (`NoNewPrivileges` stays; setuid tools would not work, hence no privsep).
- Run the traces from a VM with a PTR name and a page that explains them; abuse mail will come.

## Phases

1. **Now:** `visitor_ips` DDL, `resolve-ips`, the two layers in `build-geo-data`, VM changes, AGENTS.md table writers.
2. **Next:** `visitor_city_pct` in `evaluate` (needs `mmdbbuilder eval` to read the visitor /24 list).
3. **Then:** Atlas ground truth (`atlas_router_locations`) and `learn-rdns-rules` (Hoiho-style per-domain rules, LLM proposals from `hostname_reviews`, Atlas ping checks) writing `rdns_city_rules`, which replaces `generated_rules`.
4. **Later:** the trace-target trial verdict (drop targets with few unique wins); IPv6.
