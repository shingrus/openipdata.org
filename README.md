# openipdata.org

Workers, workflows, and builders for the openipdata.org IP databases.

A Go [Hatchet](https://hatchet.run) worker runs the workflows on a VM.
Hatchet Cloud keeps the schedules, run history, and dashboard; the VM runs the tasks.

## Layout

| Path | Purpose |
|---|---|
| `infra/` | VM provisioning script, systemd unit, env template |
| `hatchet/` | `openipdata-worker`: the Hatchet worker; one folder per workflow |
| `mmdbbuilder/` | `mmdbbuilder`: standalone MMDB build tool |

Shared code in `hatchet/`: `schema/` (Postgres tables), `publish/` (R2 and GitHub
releases), `rirstats/` (RIR delegated-extended stats parsing, address range to
prefixes), `download/` (conditional HTTP download) and `outputs/` (pruning old output
directories, last log lines, file size). `sync-geofeeds` and
`build-geo-data` share `rirstats` and `download` but keep their own downloaded files.

`hatchet/` and `mmdbbuilder/` are separate Go modules and build separately.
The worker runs `mmdbbuilder` as an external binary; it does not import it.
The repo root has no Go code.

## Workflows

Each Postgres table has exactly one writer; the other workflows only read it:

| Table | Written by |
|---|---|
| `public.asn` | `build-asn-data` |
| `public.asn_geo_pdb` | `sync-peeringdb` |
| `public.geo_ix_pdb` | `sync-peeringdb` |
| `public.asn_domain` | `update-asn-domains` |
| `public.geo_eval` | `build-geo-data` |

All three share one concurrency limit, so only one runs at a time. Every morning (UTC):
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
| `publish` | `build-mmdb`, `export-sqlite` | uploads both files to R2 under `<YYYY-MM-DD>/`, replaces the MMDB on the rolling GitHub release, pings `HC_PING_ASN` if set |

- Every task checks that the asn-ip checkout is still at the synced commit.
- Outputs land in `/var/lib/openipdata/out/asn/<date>/`; the newest 3 builds are kept.
- Published copies: `r2://<bucket>/<YYYY-MM-DD>/ip2asn-openipdata-<YYYYMMDD>.mmdb` and `.../asn.sqlite3`;
  the MMDB is always at `https://github.com/shingrus/openipdata.org/releases/latest/download/ip2asn-openipdata.mmdb`.
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
geo database build through the private R2 bucket. Nothing is published.

| Task | Runs after | Does |
|---|---|---|
| `rdap-lookups` | | checks the R2 settings (unless `skip_upload`), then looks up due ARIN, Brazil (registro.br) and Mexico (LACNIC) blocks over RDAP, within a budget per run; runs beside the other tasks |
| `probe-domains` | | needs `PGSQL` (and checks the R2 settings, unless `skip_upload`); reads the ASN website domains from `public.asn_domain` and probes the due ones for a geofeed at the usual paths, within a budget per run; runs beside the other tasks |
| `discover` | | reads the whois and IRR dumps, the RDAP results saved so far, the domain-found feeds in `domain-prefixes.csv`, the built-in feed list, and CAIDA's lists; writes the URLs and the blocks that point to them (also checks the R2 settings first, unless `skip_upload`) |
| `check-feeds` | `discover` | fetches every due feed, keeps a checked copy of each working one, and writes the lists |
| `export-rows` | `check-feeds` | writes the rows of all working feeds that pass the ownership check, and uploads them to R2 |

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
  `export-rows` reads that file and never queries Postgres.
- `discover` adds each URL in `domain-prefixes.csv` (not `domains.jsonl`, which `probe-domains` appends to
  while the other tasks run) with source `domain`, and no whois blocks. A found feed therefore reaches the
  rows after the `probe-domains` run that found it has finished, usually the next `sync-geofeeds` run. The ownership check
  works exactly as for every other feed. A feed whose only source is `domain` keeps just the rows inside
  the announced prefixes of its domain's ASNs; the other rows are dropped and counted as `outside_asn`
  (a feed with no prefixes on record loses all its rows). A feed with another source (whois, RDAP,
  IRR, list, CAIDA) is exported as before.
- `hatchet/geofeeds/feeds.txt` lists operator feeds that no whois record points to (source `list`).
- CAIDA's lists (union of the last 7 days) add ARIN and NIR feeds, which have no public dumps.
  Their use is governed by the CAIDA AUA for public datasets: publications must cite
  "The CAIDA UCSD Geofeed WHOIS dataset", https://catalog.caida.org/dataset/geofeed_whois.
- Dumps are cached in `/var/lib/openipdata/geofeeds/dumps/` and downloaded again only when the
  server's `Last-Modified` changes. A failed download falls back to the cached copy. If a whois
  dump can be read neither way, `discover` fails and the previous outputs stay: without a
  registry's blocks, its space would pass the ownership check unchecked. A missing IRR dump is
  only reported.
- Outputs, replaced each run: `/var/lib/openipdata/geofeeds/urls.csv` (`url,sources,blocks`) and
  `blocks.csv` (`source,prefix,url`). IRR rows have source `irr`: route objects show who announces
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
  cannot be fetched keeps its copy for 7 days; one that answers with something else loses it
  at once.
- A dump that cannot be read to its end adds nothing. A local file error (such as a full disk)
  fails the task instead of counting against the feeds.
- `feeds.json` holds each URL's state and is saved every 200 feeds: a retried task skips feeds
  checked in the last hour. Failing feeds are retried every run for 3 days, then weekly, then
  monthly after 60 days. URLs no source has listed for 30 days are dropped.
- Lists in `/var/lib/openipdata/geofeeds/out/`, replaced each run:
  - `geofeeds-openipdata.txt`: URLs that worked in the last 7 days, one per line
  - `geofeeds-openipdata.csv`: every URL with `sources,blocks,status,http_status,rows,last_ok`
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
- `export-rows` writes `/var/lib/openipdata/geofeeds/out/geofeed-rows.csv.gz`
  (`prefix,country,region,city,feed_url`) and uploads it to the **private** R2 bucket at the
  fixed key `internal/geofeeds/geofeed-rows.csv.gz`, replacing the last upload. `build-geo-data`
  reads it from there. An upload with 10% fewer rows than the last one (or under 1,000,000 on the
  first upload) is refused without retries, as is a damaged `upload.json`. After checking that a
  drop is real, a manual run with `{"force_upload": true}` (or `run-geofeeds -force-upload`)
  uploads anyway and resets the baseline. Manual runs can pass `{"skip_upload": true}` (or
  `run-geofeeds -skip-upload`) to write the file without uploading it.
- No Postgres tables written (`probe-domains` only reads two). Dumps and feeds are streamed, never held whole in memory; local full runs
  peaked at 240–360 MB, took about 5–10 minutes, kept about 240 MB of checked feeds on disk,
  and exported about 6.4 million rows (36 MB gzipped).

### `build-geo-data` (`hatchet/geo/`, cron `0 7,19 * * *` UTC)

Builds the ip2geo MMDB (country and city per prefix) in layers; each layer replaces what the
layers before it put under its prefixes.

| Task | Runs after | Does |
|---|---|---|
| `download` | | rebuilds the layer files: RIR stats and geo-ip-blocks from the web, PeeringDB rows and exchange prefixes from Postgres, geofeed rows from R2 |
| `build-mmdb` | `download` | runs `mmdbbuilder geo` on the layer files → `ip2geo-openipdata-<date>.mmdb` |
| `evaluate` | `download`, `build-mmdb` | scores the MMDB against DB-IP, IPinfo, RIPE Atlas and a geofeed holdout (`mmdbbuilder eval`) and stores the scores in `public.geo_eval` |
| `publish` | `download`, `build-mmdb`, `evaluate` | checks the scores against the last published ones, uploads the MMDB to R2 under `<YYYY-MM-DD>/`, replaces it on the rolling GitHub release, marks the scores as published, pings `HC_PING_GEO` if set |

A problem with the evaluation itself (holdout, `mmdbbuilder eval`, Postgres) does not fail the
`evaluate` task: it is logged and stored in its output as `error`, and `publish` then refuses with
a non-retryable error until a run with `{"force_publish": true}`. A retry of `evaluate` replaces the
unpublished `public.geo_eval` row of the same date.

- Layers, in order:
  1. the five RIRs' delegated stats: the country each block was delegated to (IPv4 and IPv6);
  2. ipverse/geo-ip-blocks (CC0): the country in registration records (IPv4 only);
  3. PeeringDB: country and city for every announced prefix of an ASN that has one dominant
     city in `public.asn_geo_pdb` and the same country in `public.asn`. An ASN that
     `build-asn-data` has not seen in its source for 7 days (`public.asn.seen_at`) is left out;
     a NULL `seen_at` counts only until the first run has stamped some row. This layer only refines:
     its rows apply where layers 1 and 2 gave the same country or none, so an ASN's city does
     not replace more specific networks registered in another country. Global-scope networks
     are kept: leaving them out (tried on 2026-11-10) halved the holdout city score;
  4. hosting networks (`hosting.csv`): every announced prefix (same `seen_at` rule as layer 3) of an
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
  5. PeeringDB internet exchange peering LAN prefixes (`public.geo_ix_pdb`): the exchange's country
     and city. A plain layer: it replaces what layers 1 to 4 put under those prefixes. An exchange
     without a city is left out of the layer (a country-only row would erase a city set below it).
     While the table does not exist or has no rows (before `sync-peeringdb` has run once with the
     exchange prefixes), `download` logs a warning and skips this layer;
  6. the geofeed rows that `sync-geofeeds` left in R2. A row broader than one RIR block (layer 1)
     is skipped (`-within-first`), so a feed cannot change the country of networks that belong
     to someone else; a row inside a block, or equal to it, is used.
- A skipped optional layer (`hosting.csv`, `ix.csv`) has its file removed, is listed in
  `skipped_layers` in the task output, and is left out of both the real build and the holdout build,
  because `mmdbbuilder` rejects an empty layer.
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
  rows at all is an exception (skipped, see layers 4 and 5). `build-geo-data` only reads
  `public.asn_geo_pdb` and `public.geo_ix_pdb`; it only creates `public.geo_eval` (its own table) and changes no other table.
- Geofeed rows older than 3 days are still used, with a warning, but `publish` then skips the
  `HC_PING_GEO` ping, so the health check reports that `sync-geofeeds` has stopped delivering.
- Outputs land in `/var/lib/openipdata/out/geo/<date>/`; the newest 3 builds are kept. The MMDB
  is always at `https://github.com/shingrus/openipdata.org/releases/latest/download/ip2geo-openipdata.mmdb`.
- Needs `PGSQL` and `R2` always, and `GITHUB_TOKEN` to publish; a missing setting fails
  `download` without retries. `IPINFO_TOKEN` is optional (see below). Manual runs can pass
  `{"skip_publish": true}` (or `run-geo -skip-publish`) to build and evaluate without uploading.
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
| IPinfo Lite (country) | `https://ipinfo.io/data/ipinfo_lite.mmdb?token=...`, needs `IPINFO_TOKEN`; skipped without it | country agreement |
| RIPE Atlas probes | `https://ftp.ripe.net/ripe/atlas/probes/archive/meta-latest` (daily JSON, bzip2): connected probes with a public IPv4 address and a country, kept as `atlas-probes.csv` (`ip,country,lat,lon`) | per-probe country accuracy |
| Geofeed holdout | the feeds whose `sha256(feed_url)` falls in a fixed 10% bucket are left out of a second build, `holdout.mmdb`, with the real build's arguments | how well the lower layers guess what operators declared |

The IPinfo token is a secret and sits in the download URL: every error of that request is replaced
by a fixed message, so it never reaches a log, a task output or a returned error.

The holdout is scored on the held-out IPv4 rows (`holdout-truth.csv`: `prefix,country,city`) that no
kept row covers or lies inside, so it measures the lower layers, not the overlap of two feeds. The
score looks up the first address of each prefix.

Metrics (percentages with 2 decimals; address-weighted unless said otherwise):

| Metric | Meaning |
|---|---|
| `country_pct`, `city_pct` | share of the IPv4 space in the RIR layer (`rir.csv`) that has a country, a city |
| `dbip_country_agree_pct` | our country equals DB-IP's, over addresses both have a country for |
| `dbip_city_agree_pct` | our city equals DB-IP's (case-insensitive, trimmed), over addresses where both have a city and the same country |
| `ipinfo_country_agree_pct` | our country equals IPinfo's |
| `consensus_country_agree_pct` | over addresses where DB-IP and IPinfo agree with each other, how often we agree with them |
| `atlas_probes`, `atlas_country_pct` | per probe: probes in the file; share whose country we have right (no answer counts as wrong) |
| `holdout_prefixes`, `holdout_country_pct`, `holdout_city_pct` | per held-out prefix: prefixes scored; share with the right country; share with the right city among those that name one |

Publish gate: before publishing, the new `atlas_country_pct` and `holdout_country_pct` are compared
with the latest row where `published` is true. If either fell by more than 2.0 points (only scores
present in both rows), `publish` fails without retries (`geo.ErrGate`). With no published row there is
no baseline and it publishes. A manual run with `{"force_publish": true}` (or `run-geo -force-publish`)
skips the gate, after checking that the drop is real; the forced row becomes the new baseline.

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
openipdata-worker run-geo              # run build-geo-data once locally, without Hatchet
openipdata-worker run-geo -skip-publish  # same, without uploading
openipdata-worker run-geo -force-publish # same, publishing even if the scores dropped more than the gate allows
mmdbbuilder asn -in <asn-ip/as> -out <file.mmdb>
mmdbbuilder geo -out <file.mmdb> [-names <names.tsv>] [-same-country <layer,...>] [-within-first <layer,...>] [-first-country <layer,...>] <layer.csv[.gz]>...
mmdbbuilder eval -db <ip2geo.mmdb> -rir <rir.csv> [-dbip <f>] [-ipinfo <f>] [-atlas <probes.csv>] [-holdout-db <f> -holdout-truth <rows.csv>]  # scores as JSON
mmdbbuilder lookup -db <file.mmdb>             # look up builtin public resolver IPs, print a summary
mmdbbuilder lookup -db <file.mmdb> -ip 1.1.1.1
mmdbbuilder lookup -db <file.mmdb> -ips ips.txt # one IP per line, plus the builtin IPs
```

## Build

The worker looks for `mmdbbuilder` next to its own binary, so build both into `bin/`:

```bash
cd hatchet && CGO_ENABLED=0 go build -trimpath -o ../bin/openipdata-worker .
cd mmdbbuilder && CGO_ENABLED=0 go build -trimpath -o ../bin/mmdbbuilder .
```

The `run-*` commands need `PGSQL` (`run-geofeeds` for its `probe-domains` task), and `run-geofeeds`
also needs `R2` unless `-skip-upload` is given; `run-pdb` also needs `PDB_KEY`, and `run-geo` also needs `R2`. Set `OPENIPDATA_DATA_DIR`
to a local folder when running off the VM.

## VM setup

See `infra/README.md`.
