# openipdata pipeline and data sources

As of 2026-11-10.

Five Hatchet workflows on one VM turn public registry, routing and operator data into two published MMDBs, ip2asn and ip2geo. Postgres holds the shared tables (one writer each); the private R2 bucket carries the large layer files between workflows.

## At a glance

```mermaid
flowchart LR
  pdbapi[PeeringDB API] --> pdb[sync-peeringdb<br/>03:00]
  pdb --> t_pdb[(asn_geo_pdb<br/>geo_ix_pdb)]
  rdapasn[PeeringDB websites<br/>RDAP for ASNs] --> dom[update-asn-domains<br/>04:00, 16:00]
  dom --> t_dom[(asn_domain)]
  asnip[ipverse/asn-ip] --> asn[build-asn-data<br/>06:00, 18:00]
  t_pdb --> asn
  t_dom --> asn
  asn --> t_asn[(asn)]
  asn --> pub_asn[/ip2asn MMDB<br/>asn.sqlite3/]
  whois[whois + IRR dumps, RDAP, CAIDA<br/>feeds.txt, ASN websites, geofeeds<br/>GeoNames] --> gf[sync-geofeeds<br/>00:00]
  t_dom --> gf
  gf --> t_gf[(geofeed_urls)]
  gf --> r2[(R2: 60-whois<br/>70-geofeeds)]
  rir[RIR stats<br/>ipverse/geo-ip-blocks] --> geo[build-geo-data<br/>07:00, 19:00]
  t_pdb --> geo
  t_asn --> geo
  r2 --> geo
  geo --> t_eval[(geo_eval)]
  geo --> pub_geo[/ip2geo MMDB/]
```

The collectors write Postgres tables or private R2 files; only the two builders publish, and `build-geo-data` scores each build before it ships.

## Schedule (UTC)

The morning chain runs PeeringDB → domains → ASN build, so the 06:00 ASN build sees fresh asn-ip, PeeringDB and domain data. Tasks retry twice.

| Time | Workflow | Concurrency limit |
| --- | --- | --- |
| 00:00 | `sync-geofeeds` | `geofeeds` |
| 03:00 | `sync-peeringdb` | `asn-db` |
| 04:00, 16:00 | `update-asn-domains` | `asn-db` |
| 06:00, 18:00 | `build-asn-data` | `asn-db` |
| 07:00, 19:00 | `build-geo-data` (decided: every 4 h, not yet switched) | `geo` |

## Workflows

Each workflow lives in its own folder of `hatchet/` and runs as task chains on the VM; `mmdbbuilder` is a separate binary the worker calls.

| Workflow | Tasks (in order) | Writes | Publishes |
| --- | --- | --- | --- |
| `sync-peeringdb` | `download` → `compute-and-store` | `public.asn_geo_pdb`, `public.geo_ix_pdb` (one transaction; refuses a >10% shrink) | — |
| `update-asn-domains` | `pdb-domains` → `rdap-domains` (≤3,000 ASNs a run) | `public.asn_domain` | — |
| `build-asn-data` | `sync-source` → `ingest` → `build-mmdb` + `export-sqlite` → `publish` | `public.asn` (incl. `seen_at`) | `ip2asn-openipdata.mmdb`, `asn.sqlite3` |
| `sync-geofeeds` | `rdap-lookups` and `probe-domains` beside `discover` → `check-feeds` → `export-rows` | `public.geofeed_urls`; layers `60-whois` and `70-geofeeds` to private R2 | nothing |
| `build-geo-data` | `download` → `build-mmdb` → `evaluate` → `publish` | `public.geo_eval` | `ip2geo-openipdata.mmdb` |

- **`sync-geofeeds`**: finds every geofeed URL (whois, IRR, RDAP, CAIDA, built-in list, ASN websites), fetches and checks each feed, drops rows that fail the ownership check, exports ~6.4M rows (36 MB gz). `discover` also builds the whois geoloc layer.
- **`build-geo-data`**: builds the MMDB from stacked layers (see ip2geo layers), scores it, and publishes only if the scores did not clearly drop.

## Data sources

Every external input, grouped by what it feeds. Third-party geo databases are used only as evaluation references, never as data.

| Source | Read by | Used for | Refresh |
| --- | --- | --- | --- |
| ipverse/asn-ip (git) | `build-asn-data` | ASN → announced prefixes | daily (~02:30 upstream) |
| PeeringDB API: org, net, fac, ix, netfac, netixlan, ixlan, ixpfx (`PDB_KEY`) | `sync-peeringdb` | ASN country/city, facility city, exchange LANs, websites | daily |
| RDAP for ASNs | `update-asn-domains` | ASN website domain when PeeringDB has none | 180 d found / 30 d not found |
| Whois dumps: RIPE + APNIC inetnum/inet6num, AFRINIC, LACNIC | `sync-geofeeds` | geofeed links, ownership check; RIPE/APNIC/AFRINIC `geoloc:` | daily, conditional |
| IRR dumps: RIPE, APNIC, ARIN, LACNIC, RADB + 10 mirrors (FTP) | `sync-geofeeds` | more geofeed links | daily, conditional |
| RDAP: ARIN, registro.br, LACNIC (Mexico) | `sync-geofeeds` | geofeed links in space with no public dump | budget per run, 7/30/1-day recheck |
| CAIDA geofeed whois lists | `sync-geofeeds` | ARIN and NIR feeds | last 7 days |
| `feeds.txt` (built-in list: Apple Private Relay, Google Cloud, Charter, …) | `sync-geofeeds` | feeds no whois record points to | with the code |
| ASN websites (from `public.asn_domain`) | `sync-geofeeds` `probe-domains` | feeds at the usual paths | budget per run |
| Operator geofeeds (RFC 8805) | `sync-geofeeds` `check-feeds` | city rows, ~6.4M | backoff 3 d / weekly / monthly |
| GeoNames `cities1000.zip` | `sync-geofeeds` | city name for whois `geoloc:` points | 7-day age gate |
| RIR delegated-extended stats (5 RIRs) | `sync-geofeeds`, `build-geo-data` | RDAP block list; country layer | conditional |
| ipverse/geo-ip-blocks (CC0) | `build-geo-data` | registration country (IPv4) | conditional |
| **Evaluation only:** DB-IP Lite city, IPinfo Lite (`IPINFO_TOKEN`), RIPE Atlas probe archive `meta-latest` | `build-geo-data` `evaluate` | agreement and accuracy scores | monthly / daily |

## Postgres tables, R2 and outputs

Each table has exactly one writer; data between workflows goes through these tables, except the large layer files, which go through the private R2 bucket. VM files are caches only.

| Store | Written by | Read by |
| --- | --- | --- |
| `public.asn` | `build-asn-data` | `build-geo-data` (`seen_at`), `sync-geofeeds` `probe-domains` |
| `public.asn_geo_pdb` | `sync-peeringdb` | `build-asn-data` (`asn.sqlite3`), `build-geo-data` |
| `public.geo_ix_pdb` | `sync-peeringdb` | `build-geo-data` |
| `public.asn_domain` | `update-asn-domains` | `build-asn-data` (`asn.sqlite3`), `sync-geofeeds` `probe-domains` |
| `public.geofeed_urls` | `sync-geofeeds` | `build-geo-data` (holdout sample) |
| `public.geo_eval` | `build-geo-data` | `build-geo-data` (publish gate) |
| R2 private `internal/layers/60-whois.csv.gz` | `sync-geofeeds` | `build-geo-data` |
| R2 private `internal/layers/70-geofeeds.csv.gz` | `sync-geofeeds` | `build-geo-data` |
| `generated_rules`, `hostname_reviews` (from nossl.sh) | nobody yet | to be adopted as they are by `resolve-ips` |

**Published:** `ip2asn-openipdata.mmdb` and `asn.sqlite3` (R2 date folder + rolling GitHub release), `ip2geo-openipdata.mmdb` (R2 + GitHub release, `releases/latest/download/`).

## ip2geo layers

Every layer is a gzipped CSV in the geofeed layout (`prefix,country,region,city,source`). Layers are applied in file-name order and each one overwrites the earlier ones under its prefixes, so operator geofeeds (last) always win.

| # | File | Source | Gives | Guard |
| --- | --- | --- | --- | --- |
| 10 | `10-rir.csv.gz` | RIR delegated stats | country | — (must be first: the guards compare against it) |
| 20 | `20-ipverse.csv.gz` | ipverse/geo-ip-blocks | country | — |
| 30 | `30-pdb.csv.gz` | `public.asn_geo_pdb`: ASN's dominant city (≥30% of its presence) | city | same-country (only refines) |
| 40 | `40-hosting.csv.gz` | single-city PeeringDB `Content` networks, non-Global scope | city | first-country (only back to the RIR country) |
| 50 | `50-ix.csv.gz` | `public.geo_ix_pdb`: exchange peering LANs | city | — |
| 60 | `60-whois.csv.gz` | whois `geoloc:`, most populous place within 25 km | city | within-first |
| 70 | `70-geofeeds.csv.gz` | operator geofeeds | city | within-first |

- Optional layers (40, 50, 60) are skipped with a warning when missing; a missing or stale R2 layer (60, 70) also skips the health-check ping.
- Planned: `65-rdns` (from `resolve-ips`) between whois and geofeeds.

## Evaluation and publish gate

`evaluate` scores every build into `public.geo_eval`; `publish` refuses when `atlas_country_pct` or `holdout_country_pct` fell more than 2.0 points against the last published row (`force_publish` overrides).

| Reference | Measures | 2026-11-10 |
| --- | --- | --- |
| Coverage (RIR layer space) | `country_pct`, `city_pct` | city 26.12% of IPv4 |
| RIPE Atlas probes (connected, public IPv4) | `atlas_country_pct` | 96.32% |
| Geofeed holdout (frozen 10% of feeds left out of a second build) | `holdout_country_pct`, `holdout_city_pct` | city ~6.5% |
| DB-IP Lite city | country and city agreement | — |
| IPinfo Lite | country agreement; consensus with DB-IP | — |

City accuracy by distance (≤100 km, GeoNames) is planned; today city matches are by name.

## In progress and planned

| Item | Status | Expected effect |
| --- | --- | --- |
| Whois `geoloc:` layer (`60-whois`) | written, reviewed, tests pass; not deployed | +~83k /24s with a city, 85.8% within 100 km |
| Layer refactor: one geofeed format, numbered files, one rules table | written, reviewed, tests pass; not deployed | less code; after deploy run `sync-geofeeds` once by hand before `build-geo-data` |
| `build-geo-data` every 4 h | decided | results reach users within ~4 h |
| `resolve-ips`: visitor /24s from nossl.sh `/api/unknown` → traceroute → nearest routers → city → `65-rdns` | design | open: where the Atlas part runs (own workflow or inside `resolve-ips`) |
| Atlas routers ≤1 ms from a probe → `atlas_router_locations`; learn and measure rDNS name → city rules | design | ~7,300 located routers per round; ground truth for rules |
| Atlas probe /24s as data (even ids), odd ids for evaluation | proposed | ~6,500 /24s |
| Per-feed geofeed accuracy against Atlas probes | proposed | demote clearly wrong feeds below whois/rDNS |
| Whois city text from measured-good holders | measured, not built | +34k /24s; all text only 52% right |
