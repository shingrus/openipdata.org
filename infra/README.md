# VM init

`init-hatchet-runner-vm.sh` sets up everything the VM needs to run `openipdata-worker`:
packages, the `openipdata` user, directories, both binaries (built from this checkout),
the env file, and the systemd unit. Hatchet Cloud holds schedules, run history, and the dashboard.

## Requirements

- Ubuntu, x86_64, with outbound IPv4
- A checkout of this repo on the VM and a user with `sudo`
- No inbound ports beyond SSH

## Run

```bash
git clone <this repo> openipdata.org
sudo bash openipdata.org/infra/init-hatchet-runner-vm.sh
sudoedit /etc/openipdata/worker.env
sudo systemctl restart openipdata-worker
journalctl -u openipdata-worker -f
```

To deploy a new version, `git pull` and re-run the script: it rebuilds both binaries
and restarts the service. It never overwrites an existing `/etc/openipdata/worker.env`.
A restart interrupts running tasks, which Hatchet retries, so deploy between runs.

## What it sets up

| Path | Owner | Purpose |
|---|---|---|
| `/opt/openipdata/bin/openipdata-worker` | root `0755` | worker, built from `hatchet/` |
| `/opt/openipdata/bin/mmdbbuilder` | root `0755` | MMDB builder, built from `mmdbbuilder/`; the worker runs it |
| `/etc/openipdata/worker.env` | root:openipdata `0640` | secrets and config (template: `openipdata-worker.env.example`) |
| `/var/lib/openipdata/` | openipdata `0750` | worker state: source clones and the last few builds |
| `/var/cache/openipdata-build/` | root `0700` | Go toolchain and module/build caches for the build |
| `/etc/systemd/system/openipdata-worker.service` | root | runs the worker as `openipdata`; starts once `HATCHET_CLIENT_TOKEN` is set |

Packages: `ca-certificates`, `curl`, `git`.

The build uses its own Go (`GO_VERSION`, checked against `GO_SHA256`), kept with the
module and build caches in `/var/cache/openipdata-build` (root, `0700`, not on `PATH`).
Go is downloaded only when `GO_VERSION` changes, modules only when `go.sum` changes,
and only changed code is recompiled. `sudo rm -rf /var/cache/openipdata-build` gives a
clean build next time. Override both to change Go:
`sudo GO_VERSION=1.27.4 GO_SHA256=<sha> bash init-hatchet-runner-vm.sh`
(checksums at `https://go.dev/dl/?mode=json`); the old toolchain is removed.

## Secrets

All secrets (`HATCHET_CLIENT_TOKEN`, `PGSQL`, `PDB_KEY`, `R2`, `GITHUB_TOKEN`, `HC_PING_ASN`, `IPINFO_TOKEN`) live in `/etc/openipdata/worker.env`
on the VM, and Hatchet Cloud never sees them. Hatchet does store task inputs, outputs, and
log lines, so keep secrets and visitor IPs out of all three.

The script never overwrites an existing env file, so when the template gains a variable,
add it to `/etc/openipdata/worker.env` by hand.
