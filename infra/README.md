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

To deploy a new version, `git pull` and re-run the script: it rebuilds both binaries
and restarts the service. It never overwrites an existing `/etc/openipdata/worker.env`.
A restart interrupts running tasks, which Hatchet retries, so deploy between runs.

## What it sets up

| Path | Owner | Purpose |
|---|---|---|
| `/opt/openipdata/bin/openipdata-worker` | root `0755` | worker, built from `hatchet/` |
| `/opt/openipdata/bin/mmdbbuilder` | root `0755` | MMDB builder, built from `mmdbbuilder/`; the worker runs it |
| `/opt/openipdata/bin/scamper` | root `0755` (not setuid) | traceroute tool of `resolve-ips`, built from the pinned upstream tarball without privilege separation; the worker runs it |
| `/etc/openipdata/worker.env` | root:openipdata `0640` | secrets and config (template: `openipdata-worker.env.example`) |
| `/var/lib/openipdata/` | openipdata `0750` | worker state: source clones, the last few builds, `rdns/` (the trace batch of `resolve-ips`: visitor IPs, so keep it private) |
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
