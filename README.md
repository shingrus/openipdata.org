# openipdata.org

Workers, workflows, builders, and the website for openipdata.org.

The website lives in [`site/`](site/README.md). It is a Go module (`openipdata-site`):
one static Go binary that renders pages with `html/template`, served behind nginx and
Cloudflare. For now it serves the home page with the Accuracy panel, read from the
scores stamped into the ip2geo MMDB's metadata. Deployment and the data puller are
not implemented yet. It builds on its own, like the other Go modules.

A Go [Hatchet](https://hatchet.run) worker runs the workflows on a VM.
Hatchet Cloud keeps the schedules, run history, and dashboard; the VM runs the tasks.

The published data files are CC0 (see [`LICENSE-DATA`](LICENSE-DATA)); the code is MIT ([`LICENSE`](LICENSE)).

## Layout

| Path | Purpose |
|---|---|
| `site/` | `openipdata-site`: Go website binary (`html/template`), home page with the Accuracy panel |
| `docs/pipeline.md` | the whole pipeline on one page: workflows, data sources, tables, layers |
| `infra/` | VM provisioning script, systemd unit, env template; `infra/site/` website VM setup |
| `hatchet/` | `openipdata-worker`: the Hatchet worker; one folder per workflow |
| `iplistener/` | `iplistener`: runs on web servers, tails the nginx log into `public.visitor_ips` ([README](iplistener/README.md)) |
| `mmdbbuilder/` | `mmdbbuilder`: standalone MMDB build tool |
| `tools/llm-codes-test/` | scratch LLM comparison for rDNS city codes, with results ([README](tools/llm-codes-test/README.md)) |

Shared code in `hatchet/`: `schema/` (Postgres tables), `publish/` (R2 and GitHub
releases), `rirstats/` (RIR delegated-extended stats parsing, address range to
prefixes), `download/` (conditional HTTP download) and `outputs/` (pruning old output
directories, last log lines, file size). `sync-geofeeds`, `sync-bgp`, `import-mlab` and
`build-geo-data` share `rirstats` and `download` but keep their own downloaded files.

`hatchet/` and `mmdbbuilder/` are separate Go modules and build separately.
The worker runs `mmdbbuilder` as an external binary; it does not import it.
The repo root has no Go code.

## Worker deploy

A push to `master` that changes `hatchet/` or `mmdbbuilder/` is tested and built by GitHub
(`.github/workflows/worker.yml`), which publishes `openipdata-worker`, `mmdbbuilder` and a
checksum manifest to the rolling release `worker`. A timer on the worker VM checks the release every
2 minutes, installs a new build when no task is running and puts the old one back if the worker
does not stay up. Details and rollback: [`infra/README.md`](infra/README.md).

## Workflows

Each Postgres table has exactly one writer; the other workflows only read it:

| Table | Written by |
|---|---|
| `public.asn` | `build-asn-data` |
| `public.asn_geo_pdb` | `sync-peeringdb` |
| `public.geo_ix_pdb` | `sync-peeringdb` |
| `public.asn_domain` | `update-asn-domains` |
| `public.geo_eval` | `build-geo-data` |
| `public.network_traces`, `public.traced_networks`, `public.ip_ptr_names`, `public.customer_name_networks`, `public.rdns_geo_layer` | `resolve-ips` |
| `public.rdns_city_rules`, `public.rdns_llm_answers` | `learn-rdns-rules` |
| `public.mlab_hop_names` | `import-mlab` |

The three ASN workflows share one concurrency limit, so only one runs at a time. Every morning (UTC):
`sync-peeringdb` (03:00) → `update-asn-domains` (04:00) → `build-asn-data` (06:00).
ipverse/asn-ip publishes its daily update at about 02:30 UTC, so the 06:00 build has fresh
asn-ip data plus the latest PeeringDB data and domains. `update-asn-domains` and
`build-asn-data` run again 12 hours later (16:00, 18:00). Tasks retry twice.

### `build-asn-data` (`hatchet/asn/`, cron `0 6,18 * * *` UTC)

| Task | Runs after | Does |
|---|---|---|
| `sync-source` | | clones `ipverse/asn-ip` into `/var/lib/openipdata/repos/asn-ip` once, then updates it; records the commit and date |
| `ingest` | `sync-source` | parses `as/*/aggregated.json` and upserts changed rows into `public.asn` |
| `build-mmdb` | `ingest` | runs `mmdbbuilder asn` on the synced files → `ip2asn-openipdata-<date>.mmdb` |
| `export-sqlite` | `ingest` | writes a fresh `asn.sqlite3` (`asn`, `asn_domain`, `asn_geo_pdb`) from Postgres-backed data |
| `publish` | `build-mmdb`, `export-sqlite` | uploads both files to R2 under `<YYYY-MM-DD>/`, replaces the MMDB and the SQLite file on the rolling GitHub release, pings `HC_PING_ASN` if set |

- Every task checks that the asn-ip checkout is still at the synced commit.
- Outputs land in `/var/lib/openipdata/out/asn/<date>/`; the newest 3 builds are kept.
- Published copies: `r2://<bucket>/<YYYY-MM-DD>/ip2asn-openipdata-<YYYYMMDD>.mmdb` and `.../asn.sqlite3`;
  the MMDB is always at `https://github.com/shingrus/openipdata.org/releases/latest/download/ip2asn-openipdata.mmdb` and the SQLite file at `.../releases/latest/download/asn-openipdata.sqlite3`.
- Publishing needs `R2` and `GITHUB_TOKEN`; a missing setting fails `sync-source`, before the build, without retries. Manual runs can pass
  `{"skip_publish": true}` (or `run-asn -skip-publish`) to build without uploading.
- Old R2 date folders are removed by a lifecycle rule on the bucket, not by the worker.
- ASNs that disappear from the source are kept in Postgres and in `asn.sqlite3`.
- The exported `asn_domain` has `(asn, domain, source)` and only usable domains (see below).
- In the MMDB, overlapping prefixes of different ASNs resolve like routing: the most specific
  prefix wins, and an identical prefix announced by several ASNs goes to the lowest ASN.
  `mmdbbuilder` writes through a temporary file, so an existing MMDB is never left half written.

### `sync-peeringdb` (`hatchet/pdb/`, cron `0 3 * * *` UTC)

| Task | Runs after | Does |
|---|---|---|
| `download` | | fetches PeeringDB `org`, `net`, `fac`, `ix`, `netfac`, `netixlan`, `ixlan`, `ixpfx` fresh into `/var/lib/openipdata/pdb/*.jsonl` |
| `compute-and-store` | `download` | derives one row per ASN and one row per exchange prefix, and replaces `public.asn_geo_pdb` and `public.geo_ix_pdb` in one transaction |

Per ASN, every facility and exchange link is one presence point:

- `country`: most common country among points with a known location, else the org's HQ country
- `city`: most common city within that country, if it holds at least 30% of all located points
- `dominance`: that city's share of all located points
- `domain`: host of the network's website, else the org's website
- `info_scope`, `info_type`: the network's own "scope" (`Global`, `Europe`, ...) and "type" in PeeringDB,
  as given; NULL when empty. `build-geo-data` uses both for the hosting layer.
- `fac_count`, `fac_city`, `fac_country`: facilities only (`netfac`; exchanges do not count here).
  `fac_count` is the number of distinct facilities (0 for none). `fac_city` and `fac_country` are set only
  if ALL of them are in the same city (city names compared case-insensitively after trimming, and together
  with the country); NULL when the facilities are in several cities, when there are none, or when one has
  no city or no 2-letter country. NULL in all three until `sync-peeringdb` has run with these columns.

`public.geo_ix_pdb` (`prefix` primary key, `ix_id`, `country`, `city`) has one row per internet
exchange peering LAN prefix (`ixpfx` → `ixlan` → `ix`), as masked CIDR text with the exchange's
country and city. Prefixes that are not global unicast (RFC 1918, ULA, link-local, documentation,
shared address space), IPv4 prefixes shorter than /16, IPv6 shorter than /32, rows whose exchange has no
2-letter country, and repeated prefixes (first wins) are left out.

Both tables are replaced in the same transaction. A replace of `public.asn_geo_pdb` is refused (and
nothing changes) if it would shrink the table by more than 10% or leave it empty. If `public.geo_ix_pdb`
would shrink by more than 10% or be empty, it keeps its old rows instead: `asn_geo_pdb` is still replaced,
the task output has `ix_kept` with the reason, and the log has a warning. Needs `PDB_KEY`.

### `update-asn-domains` (`hatchet/domains/`, cron `0 4,16 * * *` UTC)

| Task | Runs after | Does |
|---|---|---|
| `pdb-domains` | | stores the PeeringDB website domain of every ASN that has one (`source = 'pdb'`), replacing RDAP values; ASNs that left PeeringDB go back to RDAP |
| `rdap-domains` | `pdb-domains` | looks up to 3,000 due ASNs in RDAP (`source = 'rdap'`), storing each result immediately |

- Every domain is cut to its registered domain (`wholesale.telekom.com` → `telekom.com`).
- RDAP contacts are tried by role: registrant, administrative, technical, abuse, then others;
  within a contact the vCard URL comes before emails.
- RDAP candidates on the rejection list (free mail providers, internet registries) are skipped.
  PeeringDB websites are never rejected.
- Due for RDAP, most urgent first: ASNs with no row, older rows with a rejected domain, other
  older rows, then RDAP results past their re-check (found: 180 days, not found: 30 days).
- `checked_at` records the last check; "not found" is stored as `domain = NULL`.

### `sync-geofeeds` (`hatchet/geofeeds/`, cron `0 0 * * *` UTC)

Finds every geofeed (RFC 8805) URL it can, checks each feed, and hands the checked rows to the
geo database build through the private R2 bucket. It also turns the `geoloc:` points of whois
inetnums into city rows for the same build. Nothing is published.

| Task | Runs after | Does |
|---|---|---|
| `rdap-lookups` | | checks the R2 settings (unless `skip_upload`), then looks up due ARIN, Brazil (registro.br) and Mexico (LACNIC) blocks over RDAP, within a budget per run; runs beside the other tasks |
| `probe-domains` | | needs `PGSQL` (and checks the R2 settings, unless `skip_upload`); reads the ASN website domains from `public.asn_domain` and probes the due ones for a geofeed at the usual paths, within a budget per run; runs beside the other tasks |
| `discover` | | needs `PGSQL`; reads the whois and IRR dumps (and builds the whois geoloc layer from their `geoloc` inetnums), the RDAP results saved so far, the domain-found feeds in `domain-prefixes.csv`, the built-in feed list (and the feed links on its pages), and CAIDA's lists; adds the URLs to `public.geofeed_urls` and writes the blocks that point to them and `out/60-whois.csv.gz` (also checks the R2 settings first, unless `skip_upload`) |
| `check-feeds` | `discover` | needs `PGSQL`; fetches every due feed in `public.geofeed_urls`, keeps a checked copy of each working one, stores the results in the table, and writes the lists |
| `export-rows` | `check-feeds` | needs `PGSQL`; writes the rows of all working feeds (from `public.geofeed_urls`) that pass the ownership check, and uploads them as `70-geofeeds.csv.gz` to R2; also uploads `60-whois.csv.gz` (a problem with that one never fails the task) |

`rdap-lookups` and `probe-domains` have no child task: they run at the same time as `discover` → `check-feeds` →
`export-rows` (about 40 minutes), so the rows reach R2 without waiting for the RDAP pass (up to
2 hours). `discover` uses the results already in `rdap.jsonl` and the feeds in `domain-prefixes.csv`; what
this run finds is picked up by the next one (a domain-found feed only after the `probe-domains` run that
found it has finished). The run ends when all tasks have ended. `run-geofeeds` does the same.

- Whois dumps: RIPE and APNIC (`inetnum`, `inet6num`), AFRINIC, LACNIC. IRR dumps: RIPE, APNIC
  (`route`, `route6`), ARIN, LACNIC, and over FTP from RADB: RADB itself plus its copies of ALTDB,
  NTTCOM, JPIRR, TC, REACH, BELL, BBOI, CANARIE, PANIX and NESTEGG (Level3 has no reachable dump). A feed URL counts if it is in a `geofeed:` attribute or in any
  value that mentions "geofeed" (such as `remarks: Geofeed <url>`); fetching will weed out the rest.
- RDAP covers the space no public dump holds: the blocks ARIN delegates directly (networks
  reassigned inside them are not looked up), and the LACNIC blocks of Brazil
  and Mexico, whose national registries keep them. Blocks come from the RIRs' delegated stats
  files. Each run looks up the due ones (never checked first), at most 20,000 for ARIN (4 a second),
  3,000 for registro.br and 2,000 for LACNIC (1 a second). A block with a feed is checked again
  after 7 days, one without after 30, a failed lookup after 1. A 429 waits (`Retry-After`, or 15 s
  doubling up to 5 minutes) and asks again; after 5 in a row that registry stops for the run.
  So does a registry after 10 failed lookups in a row, and every registry after 2 hours: the
  rest is due next run. A failed lookup keeps the block's last answer, which counts for 45 days
  after its last good lookup; a result not looked up for 90 days is removed.
  A feed is a `Geofeed` remark or a `geo` link, like in whois.
- RDAP results are appended to `/var/lib/openipdata/geofeeds/rdap.jsonl` as they come in (a
  retried task resumes) and compacted after each run (a new file renamed over the old one).
  `discover` may read it at any time: it skips a line still being written. It is this workflow's own state: losing it
  costs a few days of lookups, while CAIDA's lists still cover ARIN. Manual runs can pass
  `{"rdap_budget": N}` (or `run-geofeeds -rdap-budget N`) to cap the lookups per registry.
- `probe-domains` finds feeds that no whois record points to, on the websites of the ASNs. Once per
  run it reads `SELECT asn, domain FROM public.asn_domain` (read only, no `schema.Ensure`; a missing
  `PGSQL` fails the task without retries) and groups the ASNs by domain (an ASN's domain is already
  a registered domain). For each domain it tries, in order, and stops at the first hit:
  `https://<domain>/geofeed.csv`, `https://geofeed.<domain>/geofeed.csv`,
  `https://www.<domain>/geofeed.csv`, `https://<domain>/.well-known/geofeed.csv`. A hit is HTTP 200,
  a body that is not HTML, and at least one valid row (the geofeed row check below) among the first 50
  data lines; at most 1 MB is read per request. Requests use the same IPv4-only, public-address-only
  client as the feed checks, 15 s timeout, 8 domains at a time, one request at a time per domain; when
  a host gives no HTTP response, the domain's other URLs on that host are skipped.
  A domain is `found`, `none` (every host answered, or does not exist, or refused the connection, and
  none had a feed) or `failed` (a timeout, 429 or 5xx, anything that says nothing about the site).
  Found is checked again after 7 days, none after 30, failed after 1; a failed recheck keeps the last
  feed, which counts for 45 days after its last good probe. Each run probes at most 5,000 domains
  (never probed first, then the most overdue); manual runs can pass `{"domain_budget": N}` (or
  `run-geofeeds -domain-budget N`). The run stops starting domains after 2 hours.
  A line per probed domain is appended to `/var/lib/openipdata/geofeeds/domains.jsonl`
  (`domain,asns,checked,next,ok,status,url`) and flushed at once, so a retried task resumes; the file is
  compacted at the end, which also forgets domains that left `public.asn_domain`. A progress line comes
  every 5 minutes, and a summary at the end.
- At the end of the run, `probe-domains` reads `public.asn.json` for the ASNs of the domains that have a
  feed (not for all ASNs: that keeps the database egress small; ASNs not seen by `build-asn-data` for
  7 days are left out) and replaces `domain-prefixes.csv` (`url,prefix`) with their announced prefixes.
  `export-rows` reads that file and queries no ASN table.
- `discover` adds each URL in `domain-prefixes.csv` (not `domains.jsonl`, which `probe-domains` appends to
  while the other tasks run) with source `domain`, and no whois blocks. A found feed therefore reaches the
  rows after the `probe-domains` run that found it has finished, usually the next `sync-geofeeds` run. The ownership check
  works exactly as for every other feed. A feed whose only source is `domain` keeps just the rows inside
  the announced prefixes of its domain's ASNs; the other rows are dropped and counted as `outside_asn`
  (a feed with no prefixes on record loses all its rows). A feed with another source (whois, RDAP,
  IRR, list, CAIDA) is exported as before.
- `hatchet/geofeeds/feeds.txt` lists operator feeds that no whois record points to (source `list`). A `page <url>` line names an
  HTML page (Microsoft's download page) whose `.csv` links `discover` reads on each run; a page that fails or has no links is
  reported in the `list` source's `error` and does not fail the run.
- CAIDA's lists (union of the last 7 days) add ARIN and NIR feeds, which have no public dumps.
  Their use is governed by the CAIDA AUA for public datasets: publications must cite
  "The CAIDA UCSD Geofeed WHOIS dataset", https://catalog.caida.org/dataset/geofeed_whois.
- Dumps are cached in `/var/lib/openipdata/geofeeds/dumps/` and downloaded again only when the
  server's `Last-Modified` changes. A failed download falls back to the cached copy. If a whois
  dump can be read neither way, `discover` fails and the previous outputs stay: without a
  registry's blocks, its space would pass the ownership check unchecked. A missing IRR dump is
  only reported.
- `discover` adds every URL it finds to `public.geofeed_urls`, the one list of every geofeed URL ever
  found (written only by `sync-geofeeds`; the table already existed, made by an older tool, and
  `schema.EnsureGeofeedURLs` creates it only on a fresh database). A new URL is inserted with its sources;
  a known one gets `last_seen_at = now()` and its `sources` become the sorted distinct union of the old and new
  ones (a source is never removed). Rows are never deleted. Source labels: `ripe`, `apnic`, `afrinic`,
  `lacnic` (whois dumps), `irr` (all IRR dumps), `arin`, `registro.br` and `lacnic-mx` (RDAP), `domain`, `list` and `caida`.
  Written again each run, replaced via a `.tmp` file: `/var/lib/openipdata/geofeeds/blocks.csv`
  (`source,prefix,url`). IRR rows have source `irr`: route objects show who announces
  a prefix, not who holds it.
- Feeds are fetched 16 at a time, at most 2 per host, with a 3-minute limit and 256 MB cap each.
  Requests use IPv4 only (on the VM, IPv6 egress stalls for many hosts) and never reach a
  non-public address, since anyone can put a URL in a whois record. After 5 fetches in a row
  that cannot reach a host, its other feeds wait for the next run with status `host_down`,
  which counts as a failure for backoff and for dropping the stored copy.
- A feed works if it has at least one valid row: a prefix (or single address) no broader than
  /8 or /16 and not private or reserved (including shared `100.64.0.0/10` and documentation
  ranges), and an ISO 3166-1 country. A malformed region is dropped
  from the row, and a line over 64 KB is rejected. Web pages, empty files, and files with no
  valid row do not count.
- Each working feed's valid rows are kept in `/var/lib/openipdata/geofeeds/feeds/`, with its
  ETag and Last-Modified in the first line, so the next run asks for changes only. A feed that
  cannot be fetched keeps its copy for 30 days; one that answers with something else loses it
  at once. The copies are caches: the table is the record.
- A dump that cannot be read to its end adds nothing. A local file error (such as a full disk)
  fails the task instead of counting against the feeds.
- `check-feeds` reads every row of `public.geofeed_urls` and writes back, every 200 feeds (a retried task
  skips feeds checked in the last hour), `last_checked_at`, `last_success_at`, `last_fetch_status` and
  `result_text`. The status is one of ours (`ok`, `http_error`, `timeout`, `host_down`, ...);
  `result_text` is `HTTP 200, 3412 rows` for a working feed (`HTTP 304, ...` when the stored copy
  was still current), `HTTP 404` for another answer, `HTTP 200, web page` (or `no data lines`, `no valid rows`,
  `over 256 MB`) for something that is not a feed, and the cut-short error for no answer (`host_down`:
  `not fetched: ...`). Failing feeds are retried every run for 3 days, then weekly, then monthly after
  60 days; a feed counts as failing since its `last_success_at` (its `first_seen_at` if it never worked).
  Only the timestamps decide, never the status text: rows from the older tool, with other statuses and old
  times, come due and are overwritten. A feed works while its last success is at most 30 days old.
- Lists in `/var/lib/openipdata/geofeeds/out/`, replaced each run:
  - `geofeeds-openipdata.txt`: URLs that worked in the last 30 days, one per line
  - `geofeeds-openipdata.csv`: every URL with `sources,status,result,last_ok` (`result` is `result_text`)
- Ownership check: a row is dropped only if the most specific whois block covering it links
  feeds on other sites only. Operators link their feed from a few whois records but describe all
  their space in it, so rows outside every block stay. A site is the registered domain of the
  feed's host (`api.cloudflare.com` and `www.cloudflare.com` are one owner), except on shared
  hosts, listed in `export.go`: there it is the first path segment (a GitHub user, an S3 or GCS
  bucket) or, on feed hosting services such as `geofeed.info`, the file itself.
- A row must be public space: an IPv6 prefix outside `2000::/3`, or inside a reserved range
  (`2001::/23` with Teredo, the documentation ranges `2001:db8::/32` and `3fff::/20`, and 6to4
  `2002::/16`), is rejected as reserved. A city over 100 bytes, or with control characters or markup, is
  dropped from the row (`bad_city`).
- A row broader than another owner's block (a /16 over their /24) is kept and counted as
  `broad_rows`: in the MMDB, that owner's more specific rows win where they exist.
- A stored feed copy that cannot be read is skipped (`unreadable_feeds`); the next check
  rewrites it.
- `export-rows` reads each stored feed twice (a check, then the data). Rows a whois block backs
  go to a temporary file and are copied after the others.
- `export-rows` writes `/var/lib/openipdata/geofeeds/out/70-geofeeds.csv.gz` and uploads it to the
  **private** R2 bucket at the fixed key `internal/layers/70-geofeeds.csv.gz`, replacing the last upload.
  It is a layer file of `build-geo-data` (see there): gzipped, in the geofeed (RFC 8805) layout with the
  header `prefix,country,region,city,source`, where `source` is the feed URL. `build-geo-data`
  reads it from there. An upload with 10% fewer rows than the last one (or under 1,000,000 on the
  first upload) is refused without retries, as is a damaged `upload.json`. After checking that a
  drop is real, a manual run with `{"force_upload": true}` (or `run-geofeeds -force-upload`)
  uploads anyway and resets the baseline. Manual runs can pass `{"skip_upload": true}` (or
  `run-geofeeds -skip-upload`) to write the file without uploading it.
- Whois geoloc layer (`60-whois.csv.gz`, one of the layers of `build-geo-data`). While `discover` reads the
  RIPE `inetnum`, APNIC `inetnum` and AFRINIC dumps (not LACNIC, not IRR, not `inet6num`), it also collects
  every IPv4 `inetnum` with a `geoloc: <lat> <lon>` whose first two fields are a latitude in [-90, 90]
  and a longitude in [-180, 180] (0 0 is skipped). After the dumps are read, `discover` drops blocks
  with more than 65,536 addresses (`too_large`) and blocks whose `country:` is not a real country, which
  includes `EU`, `ZZ`, `AP` and none (`bad_country`); names the city as the most populous GeoNames place of
  that country within 25 km of the point (the GeoNames `name`; places that are a section of a populated place
  or no longer exist are not used; no such place: `no_place`); and writes
  `/var/lib/openipdata/geofeeds/out/60-whois.csv.gz` (via a `.tmp` file) in the same format as the geofeed rows:
  region empty, `source` `whois:<registry>`, one row per CIDR prefix of the block; nested blocks are kept,
  the more specific prefix wins in the build. A GeoNames problem (not downloadable with no earlier copy) or
  no row at all never fails `discover` or touches the geofeed rows: the previous file is kept and the reason is
  in the task output (`whois.skipped`), with the blocks with a geoloc, the dropped blocks by reason, and the
  rows written. `export-rows` uploads the file to the private R2 key `internal/layers/60-whois.csv.gz`, only
  if `discover` rewrote it since the last upload (a kept old file is not sent again: its new upload time
  would hide that `discover` could not build it). The gate is the geofeed rows': more than 10% fewer rows than
  the last upload (`whois-upload.json`), or fewer than 20,000 rows on the first upload, is refused;
  `force_upload` skips it. A refusal or any other problem with this upload is a warning, not a failure: the
  reason is in the task output (`whois.skipped`) and the geofeed rows are not affected.
- GeoNames `cities1000.zip` (places with 1,000 or more people, CC BY) is downloaded by `discover` itself
  into `/var/lib/openipdata/geofeeds/dumps/`, with the shared conditional download. GeoNames rebuilds the file
  daily, so a copy younger than 7 days is used without contacting the server; an older one is refreshed, and
  if that fails the existing copy is used, however old, with a warning. With no copy at all the whois layer is not built.
- Deploy note: the layers moved from `internal/geofeeds/` to `internal/layers/` and `build-geo-data` has no
  fallback to the old keys. After deploying, run `sync-geofeeds` once by hand before the next `build-geo-data`
  run, or its `download` task fails on the missing `70-geofeeds.csv.gz`.
- One Postgres table written, `public.geofeed_urls` (`probe-domains` only reads two others). Dumps and feeds are streamed, never held whole in memory; local full runs
  peaked at 240–360 MB, took about 5–10 minutes, kept about 240 MB of checked feeds on disk,
  and exported about 6.4 million rows (36 MB gzipped).

### `sync-bgp` (`hatchet/bgp/`, cron `40 3 * * *` UTC)

Reads the BGP community values of every IPv4 prefix in a public routing table and hands them to the geo
database build through the private R2 bucket. Some networks tag the routes they learn with a community value
that means "learned in city X"; `build-geo-data` learns which values mean which city and uses it for
the optional layer `25-bgp`. This workflow reads no database and publishes nothing; it has its own `bgp` concurrency limit.

| Task | Runs after | Does |
|---|---|---|
| `download` | | checks the R2 settings (unless `skip_upload`), then downloads the newest full table of the RIPE RIS collector rrc00 (`https://data.ris.ripe.net/rrc00/latest-bview.gz`, about 430 MB, public data) to `/var/lib/openipdata/bgp/latest-bview.gz`, replacing the old one (only one is kept). It uses the shared conditional download and does not ask again within 6 hours of the last check; a plain HTTP client with timeouts and a 2 GB size cap |
| `extract` | `download` | streams the file (gzip, MRT TABLE_DUMP_V2, IPv4 unicast RIB records) in two passes and writes `/var/lib/openipdata/bgp/bgp-communities.csv.gz` through a `.tmp` file |
| `upload` | `extract` | puts the file at the fixed key `internal/bgp/communities.csv.gz` in the private R2 bucket (sha256 in the object's metadata; an identical object is not sent again, its upload time is renewed; `force_upload` always sends), then pings `HC_PING_BGP` if set |

- The file: header `prefix,communities`, one row for every IPv4 prefix of /8 to /24 in the table (also a prefix whose
  values were all dropped, because `build-geo-data` needs every prefix to know which addresses a broader prefix still
  owns), values separated by a space: `a:b` for a standard community, `a:b:c` for a large one. The values of
  all peers of a prefix are joined. A value carried by more than 5% of all prefixes (what every route gets) or by
  fewer than 3 prefixes (nothing to learn from) is dropped. Measured on a real table: 1,164,610 prefixes, 109 peers,
  83,690 values of which 332 are common and 26,760 rare, 30.8 million (prefix, value) pairs, 27.7 MB gzipped, 30 seconds,
  55 MB of memory (nothing but the value counts is kept in memory).
- The reader checks every length of the MRT data: a damaged or truncated file is an error (`damaged MRT data`), never a panic.
  A table with fewer than 500,000 prefixes is refused and not retried.
- `run-bgp` runs the three tasks once locally; `-skip-upload` writes the file only, `-force-upload` always sends the
  bytes, and `-mrt <file>` reads a local dump instead of downloading one and uploads nothing (no `R2` needed).
  Task input: `{"skip_upload": true}`, `{"force_upload": true}`.
- It runs at 03:40, well before `build-geo-data` (07:00 and 19:00), which skips the layer when the object is older than 3 days.

### `import-mlab` (`hatchet/mlab/`, no cron, started by hand)

M-Lab (Measurement Lab) publishes, under CC0, a traceroute from its speed-test server back to every client.
The owner exports the last 3 hops before each client /24 from BigQuery about once a month (see "M-Lab export"
below) and copies the CSV into the inbox. The workflow gives each client /24 the city of its hops: layer
`36-mlab`, which `build-geo-data` reads from the private R2 bucket. Measured on RIPE Atlas it adds about 238 right
probes (+1.7 points) at about 41% precision. Own concurrency limit `mlab`.

| Task | Runs after | Does |
|---|---|---|
| `import` | | reads the CSV (plain or gzip, found by its first bytes) of the inbox `/var/lib/openipdata/mlab/inbox/`: the input `file` is a base name there (no path, `.csv` or `.csv.gz`), empty = the newest file. The file is untrusted: columns are found by the header (`client24,hop_ip,from_end,traces`, `first_day` optional), a bad row is counted by reason and never echoed (wrong column count, line over 4 KB, bad client /24, bad hop, `from_end` not 0 to 2, `traces` not a number of at least 1, a client or hop that is not public IPv4), `traces` above 1,000,000 count as 1,000,000, at most 200M rows are read and at least 100,000 must be valid (else a clear error that is not retried). Rows of the same (client /24, hop) are merged (smallest `from_end`, traces added). Writes `/var/lib/openipdata/mlab/pairs.csv.gz` (sorted by /24 then hop, through a `.tmp` file), replacing the last one: each export is the whole current window. At most 40M distinct pairs (12 bytes each in memory) |
| `resolve-names` | `import` | PTR names of the distinct hop IPs with the same resolver as `resolve-ips` (system resolver, or the local resolver of `PTR_RESOLVER`, then DoH), stored in `public.mlab_hop_names` (`ip`, `ptr`, `status` ok / not_found / error, `resolved_at`; created by this task with `CREATE TABLE IF NOT EXISTS`). An ok or not_found answer younger than 60 days is skipped; errors are asked again every run (an error never replaces an older ok or not_found row). Answers are stored 2000 at a time, so a retry or the next run goes on where this one stopped. A soft budget of 3 hours ends it cleanly (`stopped: time`, `left`); a DNS problem never fails the task, only a database error or a cancellation does. Rows of IPs that are not in the current pairs file and older than 200 days are deleted |
| `locate` | `resolve-names` | per client /24 two rounds. R: the hops whose name the production rules decode to a city (the legacy `public.generated_rules`, then the candidate rules of `public.rdns_city_rules`, a domain's ranks in order, as `resolve-ips` does) vote, each with the traces it has there (`from_end` 0 to 2 count the same); the city (country and normalised name) with the largest weight wins. No winner (no rule city, or a tie): M. The hops with a city in a record of our published MMDB vote the same way, but only when the record's `city_source` is `70-geofeeds`, `60-whois`, `40-hosting`, `50-ix` or `38-asn-geofeed` (`mmdbStrongSources`; never a guess, `25-bgp`, `30-pdb`, `35-rdns-wide`, `36-mlab`, `65-rdns`, `67-rdns-names`), and not a hop inside the client /24 itself. Only a city in the client /24's country (the country of its `.1` in the MMDB) votes; a client with no country does not vote. A tie in both rounds gives none. The MMDB is this workflow's own copy from the private R2 bucket in `/var/lib/openipdata/mlab/` (same dated key, ETag and saved-copy rule as `locate` of `resolve-ips`) and is read by `mmdbbuilder lookup -in ... -out ... -with-source -countries`; without a usable MMDB the M round and the country check are skipped (reported in `mmdb` and `mmdb_reason`) and the rules still work. Writes `/var/lib/openipdata/mlab/36-mlab.csv.gz` (the layer format, source `mlab`, sorted by prefix, through a `.tmp` file). Fewer than 1,000 rows fail the task without replacing the old file |
| `upload` | `locate` | puts the layer at the fixed key `internal/layers/36-mlab.csv.gz` in the private R2 bucket (sha256 in the object's metadata; an identical object is not sent again, its upload time is renewed; `force_upload` always sends). No health-check ping: the workflow is manual |

- The task outputs have counts only (rows, bad rows by reason, pairs, client /24s, hops, days; hops looked up, ok, not found, errors, left, pruned; client /24s, hops named, hops with a rule city, hops with an MMDB city, /24s located by rule and by MMDB, ties). No IP, name or secret is logged or returned.
- `run-mlab [-file <path>] [-skip-upload] [-force-upload]` runs the four tasks once locally; `-file` imports any local CSV (the CLI only: the workflow input is a base name in the inbox). Workflow input: `{"file": "mlab-2026-11.csv", "force_upload": true}`.
- Measured on the real 7-day export (336 MB, 8.45M rows): import 6 s and 300 MB of memory, 8.11M pairs, 1.44M client /24s, 445k hop IPs, `pairs.csv.gz` 49 MB; locate 5 s and 440 MB, 547k /24s located (28k hops decoded by a rule, 56k with a strong MMDB city).
- `build-geo-data` puts the layer in as `36-mlab` (see its section); `learn-rdns-rules` also reads the ok hop names as router names.

### M-Lab export

Run this query in the BigQuery console (project `measurement-lab`; the owner's Google account is in the M-Lab Discuss group, which makes the query free). Change only the two dates; the last 21 days is a good window (about 8.5M rows and 0.3 GB a week; a longer result can pass the size BigQuery saves to Google Drive in one file). Use one window per export: the import replaces the last one.

```sql
SELECT
  REGEXP_EXTRACT(raw.Tracelb.dst, r'^(\d+\.\d+\.\d+)\.') AS client24,
  hop.addr AS hop_ip,
  MIN(ARRAY_LENGTH(raw.Tracelb.nodes) - 1 - pos) AS from_end,
  COUNT(*) AS traces
FROM `measurement-lab.ndt_raw.scamper1`,
     UNNEST(raw.Tracelb.nodes) AS hop WITH OFFSET AS pos
WHERE date BETWEEN '2026-11-07' AND '2026-11-13'
  AND STRPOS(raw.Tracelb.dst, ':') = 0
  AND pos >= ARRAY_LENGTH(raw.Tracelb.nodes) - 3
  AND hop.addr != raw.Tracelb.dst
  AND hop.addr != '*'
GROUP BY client24, hop_ip
HAVING client24 IS NOT NULL
```

1. Save the result: "Save results" -> CSV (Google Drive), then download the file from Drive (a large result is split into several files by BigQuery; if so, run the query again for a shorter window, or concatenate the files without their repeated header lines).
2. Copy it to the worker VM into the inbox, owned by the `openipdata` user:

   ```bash
   scp bq-results-*.csv vm:/tmp/mlab.csv
   ssh vm 'sudo install -d -o openipdata -g openipdata -m 0750 /var/lib/openipdata/mlab/inbox &&
           sudo install -o openipdata -g openipdata -m 0640 /tmp/mlab.csv /var/lib/openipdata/mlab/inbox/mlab-2026-11.csv && rm /tmp/mlab.csv'
   ```

   Gzip is fine too (`mlab-2026-11.csv.gz`).
3. Start `import-mlab` in Hatchet (input `{}` takes the newest file of the inbox, or `{"file": "mlab-2026-11.csv"}`), or run `sudo -u openipdata openipdata-worker run-mlab` on the VM (it needs the env file: `PGSQL` and `R2`).
4. The next `build-geo-data` run uses the layer. The layer is used for 45 days; export again about once a month.

### `build-geo-data` (`hatchet/geo/`, cron `0 7,19 * * *` UTC)

Builds the ip2geo MMDB (country and city per prefix) in layers; each layer replaces what the
layers before it put under its prefixes. A record has `country_code`, `country_name`, `city_name` and,
when it has a city, `city_source`: the name of the layer file without `.csv.gz` (e.g. `70-geofeeds`) that wrote
the city (a record without a city has none). When the build has GeoNames, a record whose city GeoNames
places (country + name, the most populous place on a clash: the same rule `eval` uses) also has `latitude` and
`longitude`, doubles rounded to 4 decimals; a city that is not placed and a record without a city have neither.
`eval` scores our cities per `city_source` and does not read the coordinates.

After all layers, `mmdbbuilder geo -asn-main` adds one guessed fill, `80-asn-main` (it is not a layer file: it needs the
merged result). For each ASN and country, the addresses that already have a city are counted per city. When one city has at
least 90% of them and there are at least 256 such addresses (with GeoNames, only cities it places are counted; others are ignored for the counts and never overwritten), the ASN's addresses in that country that have no city get that
city (`city_source` `80-asn-main`, with coordinates when GeoNames places it). It never replaces a city or changes a country.
IPv4 only; an address belongs to the ASN of its longest announced prefix, and a prefix announced by two ASNs belongs to nobody.
Measured on RIPE Atlas it is right 48% of the time (520 of 1,083 gap probes) and fills about 25% of the gap.
`mmdbbuilder gaps` and `mmdbbuilder lookup -in` ignore these cities, so guesses never feed the rDNS pipeline.
`city_pct`, `atlas_city_*` and the holdout city score count the guessed cities like any other (`layer_80_asn_main_*` show the fill alone), so the first build with the fill moves those scores; the publish gate checks only country scores, and the fill never changes a country.

`mmdbbuilder geo -fallback` (every `build-mmdb` and the holdout build pass it) adds three more guesses for the addresses
that still have a country and no city after `80-asn-main`. Each address gets the first guess that applies, in this order:
`82-asn-likely` (the ASN's main city in that country when it holds at least 70% of the addresses with a city, with no
minimum number of addresses), `84-asn-near` (the nearest city of the same ASN and country, only when it lies in the same
/20; decided per /24 of the cityless part, by the middle address of the /24, the nearer of the piece before and the piece
after wins, on equal distance the bigger piece) and `86-country-main` (the country's main city over all addresses with a
city, with or without an ASN, when it holds at least 30% of them; it also fills addresses that belong to no ASN).
Countries where no city holds 30% are left without a guess (there the country's main city was right for 14 of 273 probes).
All the evidence comes from one snapshot of the merged result taken before the first guess is written, so a guess never
counts as evidence for another guess; with GeoNames only cities it places count, as for `80-asn-main`. Like `80-asn-main`
they only fill, never replace a city or change a country, are IPv4 only, and are ignored by `gaps` and `lookup -in`.
Without the ASN prefixes only `86-country-main` runs. Each guess prints its own `layer <name>: ...` line (so it is in `prefixes`
of the task output) and a note line (`asn-likely: G groups qualified of T, A addresses filled`, `asn-near: A addresses filled`,
`country-main: C countries qualified of T, A addresses filled`). Measured on RIPE Atlas (build of 2026-11-16, within 50 km):
`82-asn-likely` right for 161 of 367 probes (44%), `84-asn-near` 134 of 396 (34%), `86-country-main` 587 of 1,331 (44%);
together +882 right and +1,212 wrong probes (right city of all probes 48.1% to 54.3%, precision of all answers 59.0% to 56.4%).

Every layer is one file in one format: gzipped CSV in the geofeed (RFC 8805) layout with the header
`prefix,country,region,city,source` (`source` is free text: the feed URL, `whois:<registry>`, or empty).
The file name carries the order: `10-rir.csv.gz`, `20-ipverse.csv.gz`, `25-bgp.csv.gz`, `30-pdb.csv.gz`,
`36-mlab.csv.gz`, `38-asn-geofeed.csv.gz`, `40-hosting.csv.gz`, `50-ix.csv.gz`, `60-whois.csv.gz`, `70-geofeeds.csv.gz`. `download` empties
`/var/lib/openipdata/geo/layers/` and writes each source's file there through one shared writer (via a
`.tmp` file); the two R2 layers are downloaded as they are. The build passes every `*.csv.gz` in the
directory, sorted by name, to `mmdbbuilder geo`, which has one reader for the format (columns found by
header). There is no list of layers in the build: the only per-layer rules are in one table keyed by
layer name (`layerRules`): `25-bgp`, `30-pdb`, `36-mlab` and `38-asn-geofeed` `-same-country`, `40-hosting` `-first-country`, `60-whois` and
`70-geofeeds` `-within-first`; a rule for a file that is not there is not passed.

| Task | Runs after | Does |
|---|---|---|
| `download` | | empties the layers directory and rebuilds the layer files: RIR stats and geo-ip-blocks from the web, PeeringDB rows and exchange prefixes from Postgres, the whois and geofeed layers from R2 |
| `build-mmdb` | `download` | makes the GeoNames copy available, runs `mmdbbuilder geo` on all the layer files (with `-geonames` when the copy is there, `-asn-main` when `download` saved the ASN prefixes, and always `-fallback`) → `ip2geo-openipdata-<date>.mmdb`; the output says `coordinates: on` or `skipped: <reason>`, with the counts of placed and unplaced cities, `asn_main`: `on`, `skipped: no ASN prefixes` or `skipped: ASN prefixes unreadable` (a bad file never fails the build), and `fallback`: `on` (the guesses `82-asn-likely`, `84-asn-near` and `86-country-main`; without the ASN prefixes only the last one runs) |
| `evaluate` | `download`, `build-mmdb` | scores the MMDB and the reference databases (DB-IP, MaxMind GeoLite2) against RIPE Atlas (country and city) and our MMDB against a geofeed holdout (`mmdbbuilder eval`) and stores the scores in `public.geo_eval` |
| `publish` | `download`, `build-mmdb`, `evaluate` | checks the scores against the last published ones, uploads the MMDB to R2 under `<YYYY-MM-DD>/`, replaces it on the rolling GitHub release, marks the scores as published, pings `HC_PING_GEO` if set |

A problem with the evaluation itself (holdout, `mmdbbuilder eval`, Postgres) does not fail the
`evaluate` task: it is logged and stored in its output as `error`, and `publish` then refuses with
a non-retryable error until a run with `{"force_publish": true}`. A retry of `evaluate` replaces the
unpublished `public.geo_eval` row of the same date.

Metadata: at its end `evaluate` stamps the built MMDB in place (`mmdbbuilder stamp`, through a `.tmp`
file and a rename), so the file that `publish` uploads carries its own numbers. `description.en` becomes
`openipdata.org ip2geo, built <date> <time> UTC` and `description.openipdata` holds a compact JSON
object: `built_at`, `date_tag`, `eval_id`, `metrics` (the `public.geo_eval` scores) and `eval_error` when the evaluation failed. Only the metadata
section is rewritten; the search tree and the data are byte for byte the same, so lookups are
unchanged. Read it with Node `maxmind`: `JSON.parse(reader.metadata.description.openipdata)`, or Go:
`reader.Metadata.Description["openipdata"]`. A stamp failure does not block publishing: the task
output has `stamp` (`stamped` or `failed: <reason>`) and the unstamped file is published.

- Layers, in order (`38-asn-geofeed`, optional, is described after the list):
  1. `10-rir`: the five RIRs' delegated stats: the country each block was delegated to (IPv4 and IPv6);
  2. `20-ipverse`: ipverse/geo-ip-blocks (CC0): the country in registration records (IPv4 only);
  3. `30-pdb`: PeeringDB: country and city for every announced prefix of an ASN that has one dominant
     city in `public.asn_geo_pdb` and the same country in `public.asn`. An ASN that
     `build-asn-data` has not seen in its source for 7 days (`public.asn.seen_at`) is left out;
     a NULL `seen_at` counts only until the first run has stamped some row. This layer only refines:
     its rows apply where layers 1 and 2 gave the same country or none, so an ASN's city does
     not replace more specific networks registered in another country. Global-scope networks
     are kept: leaving them out (tried on 2026-11-10) halved the holdout city score;
  4. `40-hosting`: hosting networks: every announced prefix (same `seen_at` rule as layer 3) of an
     ASN with PeeringDB type `Content` (PeeringDB's type for hosting, cloud and content networks),
     a scope other than `Global`, and facilities that are all in one city (`public.asn_geo_pdb.fac_city`
     and `fac_country`) gets that facility city and country. It may change the country (it is not a
     `-same-country` layer), because a hoster's small blocks are often registered to a customer's country
     (an OVH /24 registered in IT whose servers are in FR), but only back to the delegated one
     (`-first-country`): a row applies only to the addresses where layer 1 gives the facility's country
     (a broader row is split), because PeeringDB facility lists can be incomplete and the layer must not
     move a network to a country its RIR did not delegate it to. It is narrow on purpose: measured, the
     registration country is still right in most cases, so only networks that sit in a single place
     qualify, and the dominant-city rule of layer 3 (which also counts exchanges) is not used here.
     While the facility columns do not exist or no network matches (before `sync-peeringdb` has run
     with them), `download` skips this layer like the exchange layer; there is no minimum otherwise;
  5. `50-ix`: PeeringDB internet exchange peering LAN prefixes (`public.geo_ix_pdb`): the exchange's country
     and city. A plain layer: it replaces what layers 1 to 4 put under those prefixes. An exchange
     without a city is left out of the layer (a country-only row would erase a city set below it).
     While the table does not exist or has no rows (before `sync-peeringdb` has run once with the
     exchange prefixes), `download` logs a warning and skips this layer;
  6. `60-whois`: whois geoloc rows that `sync-geofeeds` left in R2 at
     `internal/layers/60-whois.csv.gz`: the most populous place within 25 km of the `geoloc:`
     point of RIPE, APNIC and AFRINIC inetnums of at most 65,536 addresses. It is downloaded as it is.
     A plain layer, below the geofeeds, with the same guard (`-within-first`): a row
     broader than one RIR block is skipped. Like the geofeeds, it may change a network's country
     (no `-first-country`): the inetnum's own country is usually right (measured: 4% of geoloc /24s
     lie in another country than the geofeed truth). A missing object skips the layer like the
     exchange layer; there is no minimum row count. A file without a usable row would fail the build
     (`mmdbbuilder` rejects an empty layer), but `sync-geofeeds` never uploads one;
  7. `70-geofeeds`: the geofeed rows that `sync-geofeeds` left in R2 at `internal/layers/70-geofeeds.csv.gz`.
     A row broader than one RIR block (layer 1)
     is skipped (`-within-first`), so a feed cannot change the country of networks that belong
     to someone else; a row inside a block, or equal to it, is used. Required.
- `38-asn-geofeed` (optional, `-same-country`: it adds cities, never changes a country) extends an ASN's own
  geofeed evidence to its other prefixes. After the R2 layers are in, `download` reads the announced prefixes of
  `public.asn` once (same `seen_at` rule as layer 3; saved as `geo/asn-prefixes.csv.gz`, outside the layers dir),
  gives each `70-geofeeds` row that has a city to the ASN of the longest announced prefix containing it (a
  prefix announced by more than one ASN is ignored; a row broader than the announcement is ignored), and weighs
  the rows per ASN and address family by addresses (IPv4) or /64s (IPv6). When one city has at least 80% of the
  weight of all its attributed rows (rows without a city count in the total), comes from at least 2 rows, and those rows cover at least half of the ASN's announced addresses in that family, every announced prefix of the ASN in that family gets it (source
  `asn:<number>`). The holdout build recomputes the layer from the kept feeds, so held-out feeds do not leak into it.
- `25-bgp` (optional, `-same-country`, below `30-pdb` on purpose: every other evidence layer replaces it where it has rows, so it
  only shows where nothing else gives a city; measured, where both answer the community city is not better than PeeringDB or
  the geofeeds) is the city that BGP community values stand for. `download` fetches `internal/bgp/communities.csv.gz`
  (written by `sync-bgp`) with the same Stat, ETag and saved-copy rule as the R2 layers (saved as
  `geo/downloads/bgp-communities.csv.gz`; it is not a layer file) and, once the geofeed layer is there, computes the layer:
  1. a prefix of the file owns its addresses minus the more specific prefixes of the file;
  2. a prefix has a known city when `70-geofeeds` rows that have a city cover at least 50% of its owned addresses and at
     least 80% of those covered addresses are in one city (country and normalised name; a more specific feed row wins,
     of a repeated prefix the last row);
  3. a value is a rule when at least 3 known prefixes carry it, at least 80% of them are in its top city and at least 90%
     in its top country;
  4. every prefix that has at least one rule among its values and whose rules all name the same city gets that city
     (the city's country); a disagreement gives nothing;
  5. rows are written for the owned parts only, split into prefixes, in address order (source `bgp`).
  The object missing, older than 3 days, any R2 error, an unreadable file or no rows skip the layer (`skipped_layers`, a
  warning); unlike `60-whois` and `70-geofeeds` this never skips the `HC_PING_GEO` ping. The holdout build recomputes
  the layer from the kept feeds, so held-out feeds do not leak into it. The task output has `bgp_rules`,
  `bgp_known_prefixes` and `bgp_prefixes`. Measured on RIPE Atlas (a stand-in geofeed layer, only where the
  published MMDB had no city): 739 gap probes answered, 389 within 50 km (52.6%), 195 million addresses filled, in about 3 seconds
  and 460 MB of memory.
- `36-mlab` (optional, `-same-country`, between `35-rdns-wide` and `38-asn-geofeed`, so every later layer replaces it) is the city of the router hops
  before each client /24, written by `import-mlab` (above) at `internal/layers/36-mlab.csv.gz` in the private R2 bucket.
  `download` fetches it with the same Stat, ETag and saved-copy rule as the other R2 layers (saved in `geo/downloads/`) and puts
  it in the layers dir as it is. The object missing, older than 45 days (the export is made by hand about monthly) or any fetch error
  skips the layer (`skipped_layers`, a warning); this never skips the `HC_PING_GEO` ping. The holdout build passes the layer as it is: it
  does not learn from the geofeed rows directly, but its MMDB-hop part reads the published MMDB (which holds geofeed cities), like the
  MMDB hop votes of `35-rdns-wide`.
- The optional layers are `25-bgp`, `35-rdns-wide`, `36-mlab`, `38-asn-geofeed`, `40-hosting`, `50-ix`, `60-whois`, `65-rdns` and `67-rdns-names`. One that is not in the build this run (no rows,
  no R2 object, or named in `skip_layers`) has no file, is listed in `skipped_layers` in the task output with a
  warning in the log, and is not passed to `mmdbbuilder` in the real build or the holdout build (both
  list the same directory). The rest of the sources are required and fail the run as before.
  An R2 layer (`60-whois`, `70-geofeeds`) that is missing or older than 3 days (`stale_layers` in the output)
  is still used, with a warning, but `publish` then skips the `HC_PING_GEO` ping.
- The RIR stats' `ZZ`, `EU` and `AP` are not countries: those blocks give no country, but they
  still count as delegated blocks for the `-within-first` rule.
- Within a layer a more specific prefix wins, and for a repeated prefix the last row wins.
  `export-rows` writes the rows that a whois block backs last, so they win over the same
  prefix in another feed.
- `mmdbbuilder geo` skips, with a warning, a row with an IPv6 prefix outside `2000::/3` (the
  tree keeps IPv4 space at `::/96`), and leaves out a city that is over 100 bytes or not plain
  text, and prints how many per layer (`layer <name>: <n> cities left out`).
- Downloads are cached in `/var/lib/openipdata/geo/downloads/` (`Last-Modified` and ETag). When
  a server fails, a copy it gave or confirmed in the last 3 days is used; otherwise the task
  fails. A source that is too small (under 1,000 blocks per RIR, 200 countries, 100 PeeringDB
  ASNs, 200 exchange prefixes written to the layer) also fails the task; an exchange table with no
  rows at all is an exception (skipped, see layers 4 and 5), and so is a missing whois geoloc object (layer 6). `build-geo-data` only reads
  `public.asn_geo_pdb` and `public.geo_ix_pdb`; it only creates `public.geo_eval` (its own table) and changes no other table.
- The health check ping skipped for a missing or old R2 layer (see above) makes the health check report
  that `sync-geofeeds` has stopped delivering.
- Outputs land in `/var/lib/openipdata/out/geo/<date>/`; the newest 3 builds are kept. The MMDB
  is always at `https://github.com/shingrus/openipdata.org/releases/latest/download/ip2geo-openipdata.mmdb`.
- Needs `PGSQL` and `R2` always, and `GITHUB_TOKEN` to publish; a missing setting fails
  `download` without retries. `MAXMIND_LICENSE_KEY` is optional (see below). Manual runs can pass
  `{"skip_publish": true}` (or `run-geo -skip-publish`) to build and evaluate without uploading,
  and `{"skip_layers": ["40-hosting.csv.gz"]}` to leave an optional layer (`40-hosting.csv.gz`, `50-ix.csv.gz`, `60-whois.csv.gz`) out of the build
  and the holdout build; any other name fails `download` without retries.
- It only reads the ASN tables, so it has its own `geo` concurrency limit.

#### Evaluation and publish gate

`evaluate` runs `mmdbbuilder eval` (IPv4 only; all MMDB reading is there) on the MMDB that
`build-mmdb` wrote and inserts one row into `public.geo_eval` (`id`, `run_at`, `date_tag`, `metrics`
as JSON, `published`), with `published = false`. `publish` sets it to true after a successful publish.
`build-geo-data` is the table's only writer.

References, refreshed into `/var/lib/openipdata/geo/eval/` with conditional downloads; a reference
that cannot be fetched falls back to its cached copy, and with none it is skipped and reported in the
task output (its scores are then left out of `metrics`, never zero):

| Reference | Source | Used for |
|---|---|---|
| DB-IP Lite city (CC BY 4.0) | `https://download.db-ip.com/free/dbip-city-lite-YYYY-MM.mmdb.gz`, this month's, last month's if not out yet | country and city agreement |
| MaxMind GeoLite2 City (GeoLite EULA, attribution required) | `https://download.maxmind.com/app/geoip_download?edition_id=GeoLite2-City&license_key=...&suffix=tar.gz`, needs `MAXMIND_LICENSE_KEY`; skipped without it; no request while the local archive was checked less than 3 days ago (30 downloads a day) | country and city agreement, rating scores |
| RIPE Atlas probes | `https://ftp.ripe.net/ripe/atlas/probes/archive/meta-latest` (daily JSON, bzip2): connected probes with a public IPv4 address and a country, kept as `atlas-probes.csv` (`ip,country,lat,lon`) | per-probe country and city accuracy |
| GeoNames `cities1000` (CC BY 4.0, attribution: GeoNames, geonames.org) | `https://download.geonames.org/export/dump/cities1000.zip`, `cities1000.txt` unpacked; own copy in the eval dir, no request while the local archive was checked less than 7 days ago | places our city names for `atlas_city_pct`, and gives the MMDB records their `latitude` and `longitude` (the `build-mmdb` task fetches it first; a failed download only skips the coordinates) |
| Geofeed holdout | a frozen 10% of the feeds (`public.geofeed_urls` rows with `id % 10 = 0` first seen before 2026-11-11, so the sample does not move when the feed list changes) are left out of a second build, `holdout.mmdb`, with the real build's arguments | how well the lower layers guess what operators declared |

The MaxMind key is a secret and sits in the download URL: every error of that
request is replaced by a fixed message (an HTTP status stays), so it never reaches a log, a task
output or a returned error. Attribution for the references when their numbers are shown: DB-IP Lite by
DB-IP.com (CC BY 4.0); this product includes GeoLite2 data created by MaxMind; city places from GeoNames (geonames.org, CC BY 4.0).

The holdout is scored on the held-out IPv4 rows (`holdout-truth.csv`: `prefix,country,city`) that no
kept row covers or lies inside, so it measures the lower layers, not the overlap of two feeds. The
score looks up the first address of each prefix.

Metrics (percentages with 2 decimals; address-weighted unless said otherwise):

| Metric | Meaning |
|---|---|
| `country_pct`, `city_pct` | share of the IPv4 space in the RIR layer (`10-rir.csv.gz`) that has a country, a city |
| `dbip_country_agree_pct` | our country equals DB-IP's, over addresses both have a country for |
| `dbip_city_agree_pct` | our city equals DB-IP's (case-insensitive, trimmed), over addresses where both have a city and the same country |
| `maxmind_country_agree_pct`, `maxmind_city_agree_pct` | the same for MaxMind GeoLite2 City |
| `vendor_consensus_country_agree_pct` | over addresses where at least two of the references given have a country and a strict majority of them agree, how often we have that country |
| `atlas_probes`, `atlas_country_pct` | per probe: probes in the file; share whose country we have right (no answer counts as wrong) |
| `atlas_city_probes`, `atlas_city_pct`, `atlas_city_unresolved` | per probe with coordinates: probes scored; share where we name a city whose GeoNames place (country + name, the most populous on a clash; name, ascii name and alternate names) is within 50 km of the probe (no answer, no city or an unplaced city is a miss); probes where we named a city GeoNames could not place. Left out without `-geonames` |
| `holdout_prefixes`, `holdout_country_pct`, `holdout_city_pct` | per held-out prefix: prefixes scored; share with the right country; share with the right city among those that name one |

Rating scores: every reference `X` (`dbip`, `maxmind`) is scored like ours, against the
same truth, with its own data as it is:

| Metric | Meaning |
|---|---|
| `X_country_pct`, `X_city_pct` | share of the IPv4 space in the RIR layer that `X` has a country, a city for |
| `X_atlas_country_pct` | per probe: share whose country `X` has right (same probes as `atlas_probes`) |
| `atlas_city_answered`, `atlas_city_precision_pct` | coverage versus accuracy: probes where we named a city GeoNames could place (an unplaced city is not answered), and the share of those within 50 km (`atlas_city_pct` is hits over all probes, so it mixes coverage and precision; precision is hits over answered). Left out without `-geonames` |
| `X_atlas_city_answered`, `X_atlas_city_precision_pct` | same for `X`: probes where `X` names a city with its own coordinates, and the share of those within 50 km |
| `layer_<src>_atlas_city_answered`, `_precision_pct`, `_unresolved` | ours only, per `city_source` (the layer that wrote the city, e.g. `70_geofeeds`; characters outside `[a-z0-9]` become `_`; a city without `city_source`, from an older MMDB, is `unknown`): probes answered with a placed city, the share within 50 km, and cities GeoNames could not place (only when above 0) |
| `X_atlas_city_pct` | per probe with coordinates (`atlas_city_probes`): share where `X` names a city whose own `location` is within 50 km of the probe; a record without a city (its coordinates are a country centroid) is a miss |

The references have no holdout scores (`dbip_holdout_country_pct`, `dbip_holdout_city_pct`,
`maxmind_holdout_country_pct`, `maxmind_holdout_city_pct` were removed): vendors ingest the same
geofeeds, so the holdout is not held out from them. Our own `holdout_*` scores stay. Atlas city for ours
depends on placing our city names (see `atlas_city_unresolved`), while a reference uses its own
coordinates; vendors may also use Atlas probe locations. Keys of older runs stay in `public.geo_eval` as they were.

Publish gate: before publishing, the new `atlas_country_pct` and `holdout_country_pct` are compared
with the latest row where `published` is true. If either fell by more than 2.0 points (only scores
present in both rows), `publish` fails without retries (`geo.ErrGate`). With no published row there is
no baseline and it publishes. A manual run with `{"force_publish": true}` (or `run-geo -force-publish`)
skips the gate, after checking that the drop is real; the forced row becomes the new baseline.

### `learn-rdns-rules` (`hatchet/rdnsrules/`, daily 05:00 UTC)

Learns extraction rules per registered router domain from the last 180 days of traces: the PTR names
of every traced IP (the target, the candidates and every public answering hop, `network_traces.hop_ips`;
each IP once) and the dictionaries below. Up to 3 extractions per domain: rank 1 is the best on all
names, rank 2 the best on the names rank 1 does not decode, rank 3 likewise on what is left; each must
meet the floor of 3 distinct IPs with distinct decodable names on those names, with at least 2 distinct
decoded token values (no single-valued rules). A stop list (router roles, network/product words such as
`ip`, `pool`, `sub`, `isp`, and all US state and Canadian province codes) is never decoded from any
dictionary; GeoNames only gives tokens of 4+ letters and places of at least 15,000 people. For a domestic
operator (at least 80% of its routers with a known country in one country) a rule also needs a country
agreement of at least 0.7; global carriers are ungated. OurAirports, UN/LOCODE and GeoNames dictionaries have their own monthly cache
under `rdnsrules/dictionaries/`. Rules and their resolved token-to-city mappings are published
atomically in `public.rdns_city_rules` (one row per `(domain, rank)`); a failed refresh keeps the old
rules. A stored learned row that a run no longer reproduces is kept while its domain has no current names
(`lost_support`); when the domain has current names it is retired (`retired`): marked `rejected`, its
version remembered so it cannot return unchanged (manual/LLM rows are never retired). Scoped manual/LLM overrides of any
rank of a domain apply to whichever rank now has their extraction; a version withdrawn at one rank
cannot come back at another.

`resolve-ips` uses candidate rules immediately, for domains it has a rule for, in this order: legacy
`generated_rules` matches and carried traced rows first; then the candidate rules, a domain's ranks in
order, on the PTR name of the target (traces by kind) and after that on router hops (same-ASN hops
first, closest first; a hop needs a known origin ASN). A match casts the one wide vote of the /24 and,
when it is a target or a router of the target's ASN, also gives the /24 a traced row in `65-rdns` with
method `candidate_rule` (rule name `<domain>#<rank>`). These rows are not sticky: every rebuild
recomputes them from the current rules and traces, they are never carried over, and they never mark a
/24 located (it keeps its silent/unmatched schedule and is traced again). Only `generated_rules`
matches stay sticky and located. The MMDB hop fallback comes last. The existing `35-rdns-wide`
same-country guard and layer order still apply. The output reports candidate rows and votes, winning
wide rows, and agreement with existing rules where both decode the same winning router. These counts
are diagnostics, not measured accuracy.

Reject a rule with `run-rdns-rules -reject example.net` (comma-separated domains), or trigger
`learn-rdns-rules` with `{"reject_domains":["example.net"]}`. This withdraws the current versions of
all ranks of the domain, without downloads or learning. Rejected versions cannot be silently revived by
a later learning run. The next `locate` rebuild removes the rows and votes of withdrawn rules; an
withdrawal never needs `force_replace`: the shrink guard counts only the sticky rows (legacy-located
traced rows), and `previous_volatile`/`stored_volatile` in the `locate` output show how many candidate
and wide rows were replaced. A `public.rdns_city_rules` table with the old per-domain layout (no `rank`
column) makes the learner stop with a fixed error: drop it once (it holds only learned rules) and rerun. `active` promotion (sticky learned rows with provenance), Atlas validation and manual
override entry are future work; v1 consumes only `candidate` rows. See
[the rule design](docs/rdns-rules-plan.md).

The task `propose-codes` runs after `learn` (optional: needs `OPENROUTER_API_KEY`). It sends an LLM digit-free name
patterns only (never a name or an IP) and stores each checked answer as the domain's rank-4 rule; learned codes it
contradicts are overridden only when a second model agrees. Questions and verdicts are in `public.rdns_llm_answers`;
API trouble or an used-up credit limit never fails the run.

### Customer names in the gap (`sample-names`)

About 69% of IPv4 has a country but no city (the gap). The PTR names of customer addresses often carry
the customer's own city (`...nycmny.fios.verizon.net`). The task `sample-names` (after `resolve-names`,
before `locate`) takes the current published ip2geo MMDB the way `locate` does (R2, ETag, saved copy) and runs
`mmdbbuilder gaps` on it when its ETag changed (`gaps.txt` and its ETag in the rdns dir). It walks a fixed shuffled
order of all 2^24 /24 indexes from a saved cursor (`sample-state.json`), takes the next 2000 /24s that are in the gap,
resolves the `.1` and two random hosts of each with the `resolve-names` resolver (nothing goes to `public.ip_ptr_names`)
and records the /24s that have at least one name in `public.customer_name_networks` (`prefix`, `country`, `hosts` and
`names` of the hosts with a name, `sampled_at`). A run less than 25 minutes after the last does nothing; after one
pass over all indexes it waits until 180 days after the pass began. Rows older than 200 days are deleted. A DNS
problem never fails the task; with no usable MMDB it reports `skipped`.
`mmdbbuilder gaps` counts a /24 whose city comes from `25-bgp` or `36-mlab` as no city too (like the four guesses `80-asn-main`, `82-asn-likely`, `84-asn-near` and `86-country-main`), so customer names keep being sampled there.
`learn-rdns-rules` (and so `propose-codes`) also reads those names (at most 5000 per registered domain, the
smallest hash of the IP; router names win) with the country of the row. It also reads the ok names of the M-Lab router
hops in `public.mlab_hop_names` (at most 5000 per registered domain, the same smallest-hash rule; no country yet, so the
announced prefix gives it as for other routers; a traced name of the same IP wins, and a hop name wins over a customer name). `locate` tries the legacy rules, then the
candidate rules, on those names: a /24 gets the city most of its names decode to (a tie gives none; the city's
country must be the row's) as a row of scope `named` in `public.rdns_geo_layer`, which `build-geo-data` writes as
layer `67-rdns-names` (same-country). Named rows are recomputed every rebuild: never sticky, carried, counted by the
shrink guard (they show in `previous_named`/`stored_named`), voting for a wide row, or marking a /24 located. The
`locate` fingerprint holds only the UTC day of the newest sample.

### PTR failures in `resolve-ips`

`resolve-names` looks up every public answering hop of the newest traces, not only the candidates;
`trace` stores them in `network_traces.hop_ips` (older rows are filled once from `hops`), so no run reads
the full path. `locate` matching still uses the candidates.

Consecutive failed lookups wait 1 day, then 2 days, then 7 days between attempts. Success or
`not_found` resets the counter; errors preserve any stored successful answer. Each run reports
`errors_timeout`, `errors_servfail` and `errors_other` from the final resolver attempt, plus
`backed_off` skips. A bounded retry pass (up to 500 IPs per run, oldest attempt first) keeps due
failures retrying after the normal 7-day trace window while their candidates remain relevant
within 180 days or are routers of carried traced rows. Located-name refreshes use the same backoff.

## Commands

```bash
openipdata-worker                      # serve: connect to Hatchet using HATCHET_CLIENT_TOKEN
openipdata-worker run-asn              # run build-asn-data once locally, without Hatchet
openipdata-worker run-asn -skip-publish  # same, without uploading
openipdata-worker run-pdb              # run sync-peeringdb once locally, without Hatchet
openipdata-worker run-domains          # run update-asn-domains once locally, without Hatchet
openipdata-worker run-geofeeds         # run sync-geofeeds once locally, without Hatchet
openipdata-worker run-geofeeds -skip-upload  # same, without uploading the rows
openipdata-worker run-geofeeds -force-upload # same, uploading even after a drop of over 10%
openipdata-worker run-geofeeds -skip-upload -rdap-budget 100 -domain-budget 100  # a quick local test run
openipdata-worker run-bgp              # run sync-bgp once locally, without Hatchet
openipdata-worker run-bgp -skip-upload # same, without uploading the file
openipdata-worker run-bgp -mrt dump.gz # read a local MRT dump, write the file, upload nothing
openipdata-worker run-mlab             # run import-mlab once locally (newest file of the inbox)
openipdata-worker run-mlab -file export.csv -skip-upload # import a local CSV, write the layer, upload nothing
openipdata-worker run-geo              # run build-geo-data once locally, without Hatchet
openipdata-worker run-geo -skip-publish  # same, without uploading
openipdata-worker run-geo -force-publish # same, publishing even if the scores dropped more than the gate allows
openipdata-worker run-rdns             # run resolve-ips once locally
openipdata-worker run-rdns -force-replace # allow an intentional layer shrink above 10%
openipdata-worker run-rdns-rules       # learn candidate rules once locally
openipdata-worker run-rdns-rules -reject example.net # withdraw the current rule, without learning
openipdata-worker run-rdns-rules -codes [-domains a.net,b.net] [-force] # only the LLM task propose-codes (needs OPENROUTER_API_KEY)
mmdbbuilder asn -in <asn-ip/as> -out <file.mmdb>
mmdbbuilder geo -out <file.mmdb> [-names <names.tsv>] [-geonames <cities1000.txt>] [-same-country <layer,...>] [-within-first <layer,...>] [-first-country <layer,...>] [-asn-main <asn-prefixes.csv.gz>] [-fallback] <layer.csv.gz>...
mmdbbuilder eval -db <ip2geo.mmdb> -rir <10-rir.csv.gz> [-dbip <f>] [-maxmind <f.mmdb>] [-atlas <probes.csv> [-geonames <cities1000.txt>]] [-holdout-db <f> -holdout-truth <rows.csv>]  # scores as JSON
mmdbbuilder lookup -db <file.mmdb>             # look up builtin public resolver IPs, print a summary
mmdbbuilder lookup -db <file.mmdb> -ip 1.1.1.1
mmdbbuilder lookup -db <file.mmdb> -ips ips.txt # one IP per line, plus the builtin IPs
mmdbbuilder lookup -db <ip2geo.mmdb> -in ips.txt -out cities.tsv  # ip<TAB>country<TAB>city for each IP of the file that has a city (used by resolve-ips)
mmdbbuilder lookup -db <ip2geo.mmdb> -in ips.txt -out cities.tsv -with-source  # same, with a fourth column: the record's city_source (used by import-mlab); add -countries to also write IPs that have a country but no city (empty city)
mmdbbuilder gaps -db <ip2geo.mmdb> -out gaps.txt  # the IPv4 /24s with a country and no city (a city from one of the four guesses 80-asn-main, 82-asn-likely, 84-asn-near, 86-country-main, or from 25-bgp or 36-mlab counts as none), as runs "<first /24> <count> <country>" (used by resolve-ips sample-names)
mmdbbuilder stamp -db <ip2geo.mmdb> -meta meta.json -out <stamped.mmdb>  # rewrites only the metadata: description.openipdata = the compact JSON of meta.json (used by evaluate)
```

## Build

The worker looks for `mmdbbuilder` next to its own binary, so build both into `bin/`:

```bash
cd hatchet && CGO_ENABLED=0 go build -trimpath -o ../bin/openipdata-worker .
cd mmdbbuilder && CGO_ENABLED=0 go build -trimpath -o ../bin/mmdbbuilder .
```

The `run-*` commands need `PGSQL` (`run-geofeeds` for `public.geofeed_urls` and its `probe-domains` task), and `run-geofeeds`
also needs `R2` unless `-skip-upload` is given (so does `run-bgp`, unless `-skip-upload` or `-mrt`); `run-pdb` also needs `PDB_KEY`, and `run-geo` also needs `R2`. Set `OPENIPDATA_DATA_DIR`
to a local folder when running off the VM.

`PTR_RESOLVER` (optional, `host:port` with a loopback IP, e.g. `127.0.0.1:5335`) makes the PTR lookups of `resolve-ips` and
`import-mlab` ask a local recursive resolver (`unbound` on the VM, see `infra/README.md`) first and DoH second; empty uses the
system resolver. A bad value stops `serve`, `run-rdns` and `run-mlab` at start.

## VM setup

See `infra/README.md`.
