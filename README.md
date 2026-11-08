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

### `build-asn-data` (`hatchet/asn/`, cron `15 */12 * * *` UTC)

| Task | Runs after | Does |
|---|---|---|
| `sync-source` | | clones `ipverse/asn-ip` into `/var/lib/openipdata/repos/asn-ip` once, then updates it; records the commit and date |
| `ingest` | `sync-source` | parses `as/*/aggregated.json` and upserts changed rows into `public.asn` |
| `domains` | `ingest` | fills missing `public.asn_domain` rows: PeeringDB domain from `public.asn_geo_pdb` first, else ARIN RDAP |
| `build-mmdb` | `ingest` | runs `mmdbbuilder asn` on the synced files → `ip2asn-openipdata-<date>.mmdb` |
| `export-sqlite` | `domains` | writes a fresh `asn.sqlite3` (`asn`, `asn_domain`, `asn_geo_pdb`) from Postgres-backed data |

- Every task checks that the asn-ip checkout is still at the synced commit.
- One run at a time; tasks retry twice.
- Outputs land in `/var/lib/openipdata/out/asn/<date>/`; the newest 3 builds are kept.
- ASNs that disappear from the source are kept in Postgres and in `asn.sqlite3`.

## Commands

```bash
openipdata-worker                      # serve: connect to Hatchet using HATCHET_CLIENT_TOKEN
openipdata-worker run-asn              # run build-asn-data once locally, without Hatchet
openipdata-worker run-asn -skip-domains
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

`run-asn` needs `PGSQL`; set `OPENIPDATA_DATA_DIR` to a local folder when running off the VM.

## VM setup

See `infra/README.md`.
