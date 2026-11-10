# AGENTS.md

## Overview

- This repo holds the openipdata.org data workers, workflows, and builders. There is no website or web interface here.
- Workflows are being moved here from another repo step by step. This repo must stay self-contained: no imports, submodules, URLs, or file references to other repos. Reproduce anything needed here.

## Layout

- `infra/`: VM provisioning (`init-hatchet-runner-vm.sh`), systemd unit, env template. No Go code.
- `hatchet/`: Go module for `openipdata-worker`, the Hatchet worker. `main.go` registers workflows; each workflow lives in its own folder (`hatchet/asn/` for `build-asn-data`, `hatchet/pdb/` for `sync-peeringdb`, `hatchet/domains/` for `update-asn-domains`, `hatchet/geofeeds/` for `sync-geofeeds`, `hatchet/geo/` for `build-geo-data`). `hatchet/schema/` creates and upgrades the shared Postgres tables; `hatchet/publish/` uploads artifacts to R2 and GitHub releases; `hatchet/rirstats/` parses RIR delegated-extended stats and splits address ranges into prefixes; `hatchet/download/` downloads a file unless the local copy is current (the caller passes the HTTP client); `hatchet/outputs/` prunes old output directories and has the small helpers `asn` and `geo` share (last log lines, file size). Workflows share this code, never each other's downloaded files.
- `mmdbbuilder/`: Go module for `mmdbbuilder`, a standalone CLI (`package main`) with one subcommand per database (`asn`, `geo`).
- No `go.mod` or Go code at the repo root. Each module builds on its own.
- `hatchet` must not import `mmdbbuilder`; it runs the `mmdbbuilder` binary found next to its own executable.

## Hatchet terms

- Use Hatchet's current terms: workflow, task, worker, run. Do not use "job" or "step".

## VM

- System user/group `openipdata`, no login shell.
- Binaries: `/opt/openipdata/bin/openipdata-worker` and `/opt/openipdata/bin/mmdbbuilder` (root, 0755).
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
  `public.geo_eval` ← `build-geo-data`.
- Schema changes go in `hatchet/schema` (idempotent `IF NOT EXISTS` statements), never by hand.
- Do not delete rows from `public.asn`; ASNs missing from the source are kept for now.
  `build-asn-data` sets `public.asn.seen_at` for every ASN in the source each run; readers use it to ignore old rows
  (NULL = not yet seen since the column was added).
- `public.asn_geo_pdb` mirrors PeeringDB: `sync-peeringdb` replaces it in one transaction each run.
- `public.asn_domain` holds the final domain per ASN (`source` = `pdb` or `rdap`); PeeringDB wins over RDAP.
  Domain cleanup rules (registered domain, rejection list) live in `hatchet/domains/clean.go`.
- Workflows that write the ASN tables share the `asn-db` concurrency limit. `sync-geofeeds` touches no ASN table and has
  its own `geofeeds` limit; `build-geo-data` only reads them and has its own `geo` limit.
- Data between workflows goes through Postgres tables, except the geofeed rows (millions of rows):
  `sync-geofeeds` uploads them to one fixed key in the private R2 bucket and `build-geo-data` reads them from there.
  A workflow's own caches and state on the VM (e.g. `/var/lib/openipdata/geofeeds/`) are never read by another workflow.
- `sync-geofeeds` HTTP and FTP requests use IPv4 only and must never reach non-public addresses (feed URLs come from
  whois records anyone can write). Nothing it builds is published. Its RDAP results live in `rdap.jsonl` on the VM,
  not in Postgres.
- `build-geo-data` scores each MMDB it builds (`evaluate` task, results in `public.geo_eval`) and `publish` refuses a
  clear drop against the last published row (`geo.ErrGate`). All MMDB reading is done by `mmdbbuilder eval`.
  The IPinfo URL holds `IPINFO_TOKEN`: any error from that request is replaced by a fixed message, never passed on.
- Secrets live only in the env file. Never put secrets or visitor IPs in Hatchet task inputs, outputs, log lines, or returned errors.
- Pass every error from `pgx.Connect` / `ParseConfig` through `schema.HideDSN`: pgx parse errors can echo the whole `PGSQL` value.
- The `R2` setting holds the secret key: errors about it use the fixed `publish.ErrInvalidR2`, never the value.
