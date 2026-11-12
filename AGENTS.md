# AGENTS.md

## Overview

- This repo holds the openipdata.org data workers, workflows, and builders. There is no website or web interface here.
- Workflows are being moved here from another repo step by step. This repo must stay self-contained: no imports, submodules, URLs, or file references to other repos. Reproduce anything needed here.

## Layout

- `infra/`: VM provisioning (`init-hatchet-runner-vm.sh`), systemd unit, env template. No Go code.
- `hatchet/`: Go module for `openipdata-worker`, the Hatchet worker. `main.go` registers workflows; each workflow lives in its own folder (`hatchet/asn/` for `build-asn-data`, `hatchet/pdb/` for `sync-peeringdb`, `hatchet/domains/` for `update-asn-domains`, `hatchet/geofeeds/` for `sync-geofeeds`, `hatchet/geo/` for `build-geo-data`, `hatchet/rdns/` for `resolve-ips`). `hatchet/schema/` creates and upgrades the shared Postgres tables; `hatchet/publish/` uploads artifacts to R2 and GitHub releases (a file that is already there byte-for-byte is not sent again) and downloads the R2 layer files; `hatchet/rirstats/` parses RIR delegated-extended stats and splits address ranges into prefixes; `hatchet/download/` downloads a file unless the local copy is current (the caller passes the HTTP client); `hatchet/outputs/` prunes old output directories and has the small helpers `asn` and `geo` share (last log lines, file size); `hatchet/ipdata/` has what several workflows need about IP space and `public.asn` (public-address check, country-code check, announced prefixes of `public.asn.json`, the `seen_at` filter). Workflows share this code, never each other's downloaded files.
- `mmdbbuilder/`: Go module for `mmdbbuilder`, a standalone CLI (`package main`) with one subcommand per database (`asn`, `geo`).
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
  `public.visitor_ips` ← external websites (`resolve-ips` only reads it), `public.network_traces`, `public.ip_ptr_names` and
  `public.rdns_geo_layer` ← `resolve-ips`.
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
- `resolve-ips` (cron every 30 minutes, tasks `pick-networks` → `trace` → `resolve-names` → `locate`, own `rdns` limit) only reads
  `public.visitor_ips` (DDL only `schema.EnsureVisitorIPs`: `CREATE TABLE IF NOT EXISTS`, so it never locks the table the websites write),
  `public.generated_rules` (read only; a missing table means no rules; a rule without domains applies to every name) and `public.asn` (only the IPv4 prefixes of `json`, extracted in SQL, kept in memory), and runs
  DDL only for its own tables (`schema.EnsureRDNS`), never `schema.Ensure`. IPv4 only, global unicast only. Visitor IPs and every
  traced IP stay in Postgres and in the batch file under `/var/lib/openipdata/rdns/`: never in Hatchet task inputs, outputs, log
  lines or returned errors (tasks report counts; errors that could quote an IP are replaced by `dbError` or `scrubIPs`).
  `pick-networks` writes the batch file that `trace` reads. `public.network_traces.candidates` (`inet[]`, added by `schema.EnsureRDNS`) holds the candidates of a trace (the target, then the last quarter of the
  public hops, closest first): `trace` stores them with the path, and `resolve-names` and `locate` read only `prefix`, `target_kind`, `target` and `candidates`, never `hops`
  (the stat `reached_asn` is "a tail hop candidate, not the target itself, is in the target's ASN"). Rows written before the column existed (NULL) are filled once from `hops`
  in chunks at the start of `resolve-names` (and of a `locate` that rebuilds). `resolve-names` names the candidates of the newest trace per prefix and kind of the last 7 days, never replacing an `ok`/`not_found` name by an `error`; `locate` reads only the names of its candidates. `locate` replaces `public.rdns_geo_layer` in one transaction and
  refuses a shrink of more than 10% when the table had rows (`ErrShrink`; `force_replace` overrides). `locate` is skipped (`skipped` in its output, table untouched) when a fingerprint of its inputs
  (count and newest timestamp of the traces of the 180-day window, of the usable `ip_ptr_names` and of the current ASNs, and a hash of the `generated_rules` columns, all computed in SQL)
  equals the one saved after the last rebuild and that rebuild is less than 24 hours old; the fingerprint and the rebuild time are in `/var/lib/openipdata/rdns/locate-state.json`
  (`.tmp` plus rename, written only after a committed rebuild; losing it only costs a rebuild). `force_replace` always rebuilds.
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
  clear drop against the last published row (`geo.ErrGate`). All MMDB reading is done by `mmdbbuilder eval`.
  The holdout leaves out a frozen sample of feeds, read from `public.geofeed_urls` (`id % 10 = 0`, `first_seen_at` before the
  freeze date in `hatchet/geo/holdout.go`); a failed read is an evaluation error, never a different sample. Moving the date moves the score once.
  The IPinfo URL holds `IPINFO_TOKEN`: any error from that request is replaced by a fixed message, never passed on.
- Published files are never sent twice: `build-asn-data` and `build-geo-data` upload with the sha256 in R2 user metadata and skip the upload when the object at the same dated key has the same sha256
  (a new day's folder still gets its upload); for the rolling GitHub release they skip replacing an asset whose reported `digest` (`sha256:<hex>`) equals the file's (no digest: replace as before).
  Gates, the `public.geo_eval` published flag and the health-check pings behave as if the file was published (an identical file is the published file); the task outputs say `unchanged`.
- Secrets live only in the env file. Never put secrets or visitor IPs in Hatchet task inputs, outputs, log lines, or returned errors.
- Pass every error from `pgx.Connect` / `ParseConfig` through `schema.HideDSN`: pgx parse errors can echo the whole `PGSQL` value.
- The `R2` setting holds the secret key: errors about it use the fixed `publish.ErrInvalidR2`, never the value.
