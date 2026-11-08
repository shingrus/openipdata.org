# AGENTS.md

## Overview

- This repo holds the openipdata.org data workers, jobs, and builders. There is no website or web interface here.
- One Go module at the repo root; one Hatchet worker binary: `cmd/openipdata-worker`.
- Jobs are being moved here from another repo step by step. This repo must stay self-contained: no imports, submodules, URLs, or file references to other repos. Reproduce anything needed here.

## Layout

- Worker entry point: `cmd/openipdata-worker/main.go`
- Workflows: one package per job under `internal/` (e.g. `internal/asn`)
- MMDB writers: `internal/mmdb`
- VM provisioning: `init/`

## VM

- System user/group `openipdata`, no login shell.
- Binary: `/opt/openipdata/bin/openipdata-worker` (root, 0755).
- Env file: `/etc/openipdata/worker.env` (root:openipdata, 0640); template in `init/`.
- State: `/var/lib/openipdata` (openipdata, 0750); the only writable path for the service.
- `init/init-hatchet-runner-vm.sh` is idempotent and never overwrites an existing env file.

## Rules

- Keep it simple: read env vars directly in `main.go`; no config packages or frameworks.
- Postgres is the source of truth; the VM only keeps caches and the last few builds.
- Secrets live only in the env file. Never put secrets or visitor IPs in Hatchet task inputs, outputs, or log lines.
- Use the standard library where possible (`encoding/json`, `database/sql`).
