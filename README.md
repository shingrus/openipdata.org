# openipdata.org

Workers, jobs, and builders for the openipdata.org IP databases.

They run as a single Go [Hatchet](https://hatchet.run) worker on a VM.
Hatchet Cloud keeps the schedules, run history, and dashboard; the VM runs the jobs.

## Layout

| Path | Purpose |
|---|---|
| `cmd/openipdata-worker/` | worker entry point |
| `internal/asn/` | `build-asn-data` workflow: asn-ip source → Postgres → ip2asn MMDB + `asn.sqlite3` |
| `internal/mmdb/` | MMDB writers |
| `init/` | VM provisioning script, systemd unit, env template |

## Workflows

### `build-asn-data`

1. Sync `ipverse/asn-ip` into `/var/lib/openipdata/repos/asn-ip`.
2. Upsert changed rows into `public.asn` in Postgres.
3. Fill missing `public.asn_domain` rows (PeeringDB first, then ARIN RDAP).
4. Build `ip2asn-openipdata-<date>.mmdb`.
5. Export `asn.sqlite3`.

Outputs land in `/var/lib/openipdata/out/asn/<date>/`; the newest 3 builds are kept.

## VM setup

See `init/README.md`.
