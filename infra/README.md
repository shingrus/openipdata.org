# VM init

`init-hatchet-runner-vm.sh` sets up everything the VM needs to run `openipdata-worker`:
packages, the `openipdata` user, directories, both binaries (built from this checkout),
the env file, and the systemd unit. Hatchet Cloud holds schedules, run history, and the dashboard.

The website VM is set up from `infra/site/`; see the Server section of `site/README.md`.

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

The script is for the first setup and for changes to scamper, unbound, the units and the script itself.
It never overwrites an existing `/etc/openipdata/worker.env`. New worker code is installed by the
updater (next section), not by re-running the script. A re-run still rebuilds both binaries from the
checkout and restarts the service, which interrupts running tasks (Hatchet retries them); the updater
then replaces those binaries with the release build again.

## Auto-deploy of worker code

1. A push to `master` that changes `hatchet/`, `mmdbbuilder/` or `.github/workflows/worker.yml` starts
   the workflow "Publish worker binaries": gofmt, `go vet`, `go test -race`, a static build of both
   binaries (version = commit sha). Only a run on `master` publishes.
2. It replaces the assets of the GitHub release `worker` (never marked latest): `openipdata-worker-linux-amd64`
   and `mmdbbuilder-linux-amd64` first, then the manifest `worker.sha256` last (two `sha256sum` lines and
   `version <sha>`).
3. `openipdata-worker-update.timer` runs `/opt/openipdata/bin/openipdata-worker-update` every 2 minutes.
   It reads the manifest, compares it with the hashes of the installed binaries and, for a new build,
   downloads and checks both files, runs `version` of each as `nobody` (10 s limit, must print the
   manifest's version), waits until no task runs, saves the old binaries as `<name>.prev`, installs the new ones
   (temp name plus `mv`) and restarts `openipdata-worker`. It then watches the service for 30 seconds.
   A service that is not active, or restarts by itself, gets the `.prev` binaries back and the build is
   marked bad.
4. No task running: the worker keeps `/var/lib/openipdata/worker-busy` while any task runs. The updater waits while
   it exists, at most 45 minutes, then installs anyway (Hatchet retries interrupted tasks).
5. The restart happens only when the service is enabled and `HATCHET_CLIENT_TOKEN` is set in the env file;
   otherwise the binaries are installed and nothing is started.
6. The release always wins: binaries that the init script builds from a checkout are replaced by the
   release build at the updater's next run. When the tests of the newest commit fail, nothing is published,
   so the VM stays on (or goes back to) the last build that passed. Pause the timer to keep a source build.

A missing or half-uploaded release is not an error: the updater logs one line and tries again later.
The init script and the updater share the lock `/run/openipdata-worker-deploy.lock`, so they never run
together (a locked updater run exits quietly).

| Path | Purpose |
|---|---|
| `/opt/openipdata/bin/openipdata-worker-update` | the updater (root `0755`), installed by the init script |
| `/etc/systemd/system/openipdata-worker-update.{service,timer}` | oneshot unit and the 2 minute timer |
| `/etc/openipdata/worker-update.env` | optional (root `0600`, created from `openipdata-worker-update.env.example` only if absent): `GITHUB_REPO`, `HC_PING_WORKER_UPDATE` |
| `/var/lib/openipdata-update/` | state (root `0700`): `bad.sha256`, `waiting-since` |

Log: `journalctl -u openipdata-worker-update` (one line per decision; URLs and secrets are never logged).
Installed version: `/opt/openipdata/bin/openipdata-worker version`.

Pause: `sudo systemctl disable --now openipdata-worker-update.timer` (enable it again with `enable --now`).

Roll back: push a revert of the bad change to `master`; the next build is installed the same way. For an urgent
rollback, pause the timer, check out an older commit on the VM and re-run the init script, or copy
`/opt/openipdata/bin/openipdata-worker.prev` (and `mmdbbuilder.prev`) back by hand and restart the service.

`/var/lib/openipdata-update/bad.sha256` holds the checksum of a manifest whose build failed its check (it
did not print its version, or the worker did not stay up after the install). The updater never tries that
manifest again and pings `/fail` on every run. A newer build is tried as usual and clears the file when it
installs. To try the same build again (for example after fixing the VM), delete the file:
`sudo rm /var/lib/openipdata-update/bad.sha256`.

## What it sets up

| Path | Owner | Purpose |
|---|---|---|
| `/opt/openipdata/bin/openipdata-worker` | root `0755` | worker, built from `hatchet/` |
| `/opt/openipdata/bin/mmdbbuilder` | root `0755` | MMDB builder, built from `mmdbbuilder/`; the worker runs it |
| `/opt/openipdata/bin/scamper` | root `0755` (not setuid) | traceroute tool of `resolve-ips`, built from the pinned upstream tarball without privilege separation; the worker runs it |
| `/etc/openipdata/worker.env` | root:openipdata `0640` | secrets and config (template: `openipdata-worker.env.example`) |
| `/var/lib/openipdata/` | openipdata `0750` | worker state: source clones, the last few builds, `rdns/` (the trace batch of `resolve-ips`: visitor IPs, so keep it private), `worker-busy` (exists while a task runs) |
| `/var/cache/openipdata-build/` | root `0700` | Go toolchain and module/build caches for the build |
| `/etc/systemd/system/openipdata-worker.service` | root | runs the worker as `openipdata`; starts once `HATCHET_CLIENT_TOKEN` is set |

Packages: `ca-certificates`, `curl`, `git`, `build-essential` (only to build `scamper`), `unbound` and `bind9-dnsutils` (`dig`, only for the smoke test of unbound). The apt package `scamper` is removed if installed.

The unit keeps `NoNewPrivileges=true` and grants only `CAP_NET_RAW` (`AmbientCapabilities` and
`CapabilityBoundingSet`): `scamper` sends ICMP probes over a raw socket as the `openipdata` user.
Ubuntu's `scamper` package is setuid root with privilege separation (at start it runs mkdir `/var/empty`,
chroot and setuid) and dies under those limits, so the script builds upstream scamper
(`SCAMPER_VERSION`, checked against `SCAMPER_SHA256`) with `--disable-scamper-privsep` in
`/var/cache/openipdata-build` and installs only the binary. It rebuilds only when
`/opt/openipdata/bin/scamper -v` does not report the pinned version. Without `CAP_NET_RAW` scamper
prints "could not open ICMP socket" and still exits 0, so after installing the unit the script runs a
smoke test on every run: `systemd-run` starts one `scamper -O json -c "trace -P icmp-paris" -i 1.1.1.1`
with the unit's user and limits (keep its `-p` list in step with the unit), and the script fails unless the
output holds a `trace` with at least one hop. The `trace` task also fails on that socket error or on output
without any trace, and stores nothing.
Run the VM with a PTR name and a page that explains the traces; abuse mail will come.

The build uses its own Go (`GO_VERSION`, checked against `GO_SHA256`), kept with the
module and build caches in `/var/cache/openipdata-build` (root, `0700`, not on `PATH`).
Go is downloaded only when `GO_VERSION` changes, modules only when `go.sum` changes,
and only changed code is recompiled. `sudo rm -rf /var/cache/openipdata-build` gives a
clean build next time. Override both to change Go:
`sudo GO_VERSION=1.27.4 GO_SHA256=<sha> bash init-hatchet-runner-vm.sh`
(checksums at `https://go.dev/dl/?mode=json`); the old toolchain is removed.

## Local resolver (unbound)

The PTR lookups of `resolve-ips` (`resolve-names`, `sample-names`) and `import-mlab` go through a
local `unbound` recursive resolver instead of the system DNS, which rate limits bulk lookups and
would see the PTR questions about visitor IPs. The script writes `/etc/unbound/unbound.conf.d/openipdata.conf`
on every run: it listens on `127.0.0.1:5335` only, has no query log, and does no DNSSEC validation. The
script validates it with `unbound-checkconf` (on failure it removes the file and warns, the worker keeps
the system DNS), restarts unbound only when the file changed or it was not active, disables the packaged
`unbound-resolvconf.service`, and never touches systemd-resolved or `/etc/resolv.conf`.

After a passing smoke test the script writes the drop-in
`/etc/systemd/system/openipdata-worker.service.d/ptr-resolver.conf` (`Environment=PTR_RESOLVER=127.0.0.1:5335`,
`Wants=` and `After=unbound.service`); without a passing test it removes the drop-in. The re-run restarts the
worker, which applies a changed drop-in. A `PTR_RESOLVER=` line in the env file overrides the drop-in:
`PTR_RESOLVER=` (empty) turns it off, and the worker then uses the system DNS.

Check:

```bash
dig +short +time=3 +tries=2 @127.0.0.1 -p 5335 -x 1.1.1.1
systemctl status unbound
```

To turn it off: put `PTR_RESOLVER=` in `/etc/openipdata/worker.env` and restart the worker.

## Secrets

All secrets (`HATCHET_CLIENT_TOKEN`, `PGSQL`, `PDB_KEY`, `R2`, `GITHUB_TOKEN`, `HC_PING_ASN`, `MAXMIND_LICENSE_KEY`) live in `/etc/openipdata/worker.env`
on the VM, and Hatchet Cloud never sees them. Hatchet does store task inputs, outputs, and
log lines, so keep secrets and visitor IPs out of all three.

The script never overwrites an existing env file, so when the template gains a variable,
add it to `/etc/openipdata/worker.env` by hand.
