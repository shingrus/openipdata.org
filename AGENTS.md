# AGENTS.md

## Overview

- This repo holds the openipdata.org data workers, workflows, and builders. There is no website or web interface here.
- Workflows are being moved here from another repo step by step. This repo must stay self-contained: no imports, submodules, URLs, or file references to other repos. Reproduce anything needed here.

## Layout

- `infra/`: VM provisioning (`init-hatchet-runner-vm.sh`), systemd unit, env template. No Go code.
- `hatchet/`: Go module for `openipdata-worker`, the Hatchet worker. `main.go` registers workflows; each workflow lives in its own folder (`hatchet/asn/` for `build-asn-data`, `hatchet/pdb/` for `sync-peeringdb`, `hatchet/domains/` for `update-asn-domains`). `hatchet/schema/` creates and upgrades the shared Postgres tables.
- `mmdbbuilder/`: Go module for `mmdbbuilder`, a standalone CLI (`package main`) with one subcommand per database (`asn` now).
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
  `public.asn` ← `build-asn-data`, `public.asn_geo_pdb` ← `sync-peeringdb`, `public.asn_domain` ← `update-asn-domains`.
- Schema changes go in `hatchet/schema` (idempotent `IF NOT EXISTS` statements), never by hand.
- Do not delete rows from `public.asn`; ASNs missing from the source are kept for now.
- `public.asn_geo_pdb` mirrors PeeringDB: `sync-peeringdb` replaces it in one transaction each run.
- `public.asn_domain` holds the final domain per ASN (`source` = `pdb` or `rdap`); PeeringDB wins over RDAP.
  Domain cleanup rules (registered domain, rejection list) live in `hatchet/domains/clean.go`.
- Workflows that write the ASN tables share the `asn-db` concurrency limit.
- Secrets live only in the env file. Never put secrets or visitor IPs in Hatchet task inputs, outputs, or log lines.
