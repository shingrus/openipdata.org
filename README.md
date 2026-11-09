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

`hatchet/` and `mmdbbuilder/` are separate Go modules and build separately.
The worker runs `mmdbbuilder` as an external binary; it does not import it.
The repo root has no Go code.

## Workflows

Each Postgres table has exactly one writer; the other workflows only read it:

| Table | Written by |
|---|---|
| `public.asn` | `build-asn-data` |
| `public.asn_geo_pdb` | `sync-peeringdb` |
| `public.asn_domain` | `update-asn-domains` |

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
| `download` | | fetches PeeringDB `org`, `net`, `fac`, `ix`, `netfac`, `netixlan` fresh into `/var/lib/openipdata/pdb/*.jsonl` |
| `compute-and-store` | `download` | derives one row per ASN and replaces `public.asn_geo_pdb` in one transaction |

Per ASN, every facility and exchange link is one presence point:

- `country`: most common country among points with a known location, else the org's HQ country
- `city`: most common city within that country, if it holds at least 30% of all located points
- `dominance`: that city's share of all located points
- `domain`: host of the network's website, else the org's website

The replace is refused if it would shrink the table by more than 10%. Needs `PDB_KEY`.

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

### `sync-geofeeds` (`hatchet/geofeeds/`, no schedule yet: trigger it by hand)

Finds every geofeed (RFC 8805) URL it can, checks each feed, and hands the checked rows to the
geo database build through the private R2 bucket. Nothing is published.

| Task | Runs after | Does |
|---|---|---|
| `discover` | | reads the whois and IRR dumps, the built-in feed list, and CAIDA's lists; writes the URLs and the blocks that point to them |
| `check-feeds` | `discover` | fetches every due feed, keeps a checked copy of each working one, and writes the lists |
| `export-rows` | `check-feeds` | writes the rows of all working feeds that pass the ownership check, and uploads them to R2 |

- Whois dumps: RIPE and APNIC (`inetnum`, `inet6num`), AFRINIC, LACNIC. IRR dumps: RIPE, APNIC
  (`route`, `route6`), ARIN, LACNIC. A feed URL counts if it is in a `geofeed:` attribute or in any
  value that mentions "geofeed" (such as `remarks: Geofeed <url>`); fetching will weed out the rest.
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
- A row broader than another owner's block (a /16 over their /24) is kept and counted as
  `broad_rows`: in the MMDB, that owner's more specific rows win where they exist.
- A stored feed copy that cannot be read is skipped (`unreadable_feeds`); the next check
  rewrites it.
- `export-rows` writes `/var/lib/openipdata/geofeeds/out/geofeed-rows.csv.gz`
  (`prefix,country,region,city,feed_url`) and uploads it to the **private** R2 bucket at the
  fixed key `internal/geofeeds/geofeed-rows.csv.gz`, replacing the last upload. `build-geo-data`
  reads it from there. An upload with 10% fewer rows than the last one (or under 1,000,000 on the
  first upload) is refused without retries, as is a damaged `upload.json`. After checking that a
  drop is real, a manual run with `{"force_upload": true}` (or `run-geofeeds -force-upload`)
  uploads anyway and resets the baseline. Manual runs can pass `{"skip_upload": true}` (or
  `run-geofeeds -skip-upload`) to write the file without uploading it.
- No Postgres tables. Dumps and feeds are streamed, never held whole in memory; local full runs
  peaked at 240–360 MB, took about 5–10 minutes, kept about 240 MB of checked feeds on disk,
  and exported about 6.4 million rows (36 MB gzipped).

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
mmdbbuilder asn -in <asn-ip/as> -out <file.mmdb>
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

The `run-*` commands need `PGSQL`, except `run-geofeeds`, which needs `R2` unless `-skip-upload`
is given; `run-pdb` also needs `PDB_KEY`. Set `OPENIPDATA_DATA_DIR`
to a local folder when running off the VM.

## VM setup

See `infra/README.md`.
