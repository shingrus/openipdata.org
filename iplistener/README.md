# iplistener

A small daemon for a web server. It follows the nginx access log and records the visitor
IPs of the lines you select in `public.visitor_ips` (Postgres). The `resolve-ips` workflow
reads that table. The table is created by `hatchet/schema`, never by this module.

- Starts at the end of `LOG_FILE` (no replay, no state file), reads complete lines only,
  follows logrotate (rename + create) and truncation.
- A line is kept when it matches `FILTER` and not `EXCLUDE`. The IP is the first field
  (`$remote_addr`). Only public IPv4 is kept (IPv4-mapped IPv6 is unmapped first; private,
  CGNAT, loopback, link-local, multicast, reserved and other IPv6 are dropped).
- An IP sent in the last hour is not queued again. Batches go out at 100 IPs (never more than
  once per 2 s) or 60 s after the first queued IP, as one `INSERT ... ON CONFLICT` statement.
  A new IP is inserted; a known one only gets `last_seen_at = now()`, and only when that is
  more than an hour old.
- A failed write is retried with backoff (2 s, doubling, up to 5 min). At most 50,000 IPs wait;
  beyond that the oldest are dropped (counted).
- stdout (journald) has a start line and every 10 minutes a summary of counts. Never an IP
  or the connection string.

## Settings

Env file `/etc/iplistener/iplistener.env` (see `iplistener.env.example`):

| Variable | Required | Example |
|---|---|---|
| `PGSQL` | yes, secret | `postgresql://iplistener.<project-ref>:<password>@aws-0-<region>.pooler.supabase.com:5432/postgres?sslmode=verify-full` |
| `LOG_FILE` | yes | `/var/log/nginx/access.log` |
| `FILTER` | yes, Go (RE2) regexp | `'"POST /api/\w+'` or `'"GET /api/beacon'` |
| `EXCLUDE` | no, Go regexp | `'cc=T1\|bot\|crawl\|spider'` |
| `SOURCE` | yes | short label stored in `visitor_ips.source` |

Supabase direct connections may be IPv6-only: use the session pooler connection string
with the role's pooler user, and `sslmode=verify-full` (or `require`). The daemon uses the
simple query protocol, so it also works through a transaction pooler.
Write regexps in single quotes in the env file: systemd parses it and keeps backslashes and
double quotes literally only inside single quotes (`FILTER='"POST /api/\w+'`).
Invalid or missing settings stop the daemon with a message that never shows `PGSQL`.

## Install

On the web server (Ubuntu/Debian), two commands. Go must be installed (the version in `go.mod` or
newer); the script never downloads it and stops with a hint when it is missing or too old:

```
sudo snap install go --classic
git clone https://github.com/shingrus/openipdata.org && cd openipdata.org/iplistener && sudo ./init-iplistener.sh
```

The script asks for every empty setting (the connection string is read hidden), writes the env
file, starts the service and checks that it runs. To change a setting later:
`sudoedit /etc/iplistener/iplistener.env` and rerun the script. Logs: `journalctl -u iplistener -f`.

The script is idempotent (re-run it after `git pull`). It builds with the installed Go (module and build
caches in `/var/cache/iplistener-build`), installs `/opt/iplistener/bin/iplistener`, creates the
system user `iplistener` (in group `adm` to read the nginx logs), installs the systemd unit,
and starts it only when the env file has all required values. It never overwrites the env file.

nginx's default logrotate works: it renames the log, creates a new one and sends `USR1`;
iplistener reads the new file from its start and keeps reading the renamed one for 30 s, because nginx writes to it until it reopens its logs.

## Resources

The unit keeps it out of the site's way: `Nice=10`, idle I/O priority, `CPUQuota=10%`,
`GOMAXPROCS=1`, `GOMEMLIMIT=40MiB`, `MemoryHigh=48M`, `MemoryMax=96M`. Every buffer is capped
(256 queued lines, 50k pending IPs, 200k deduped IPs), so memory cannot grow without bound; if
the limit is ever hit, systemd restarts only this service. nginx never waits on it: it only reads the log.

## Postgres role

Run this yourself in the Supabase SQL editor (choose a strong password):

```sql
CREATE ROLE iplistener LOGIN PASSWORD '<password>';
GRANT USAGE ON SCHEMA public TO iplistener;
GRANT INSERT (ip, source) ON public.visitor_ips TO iplistener;
-- ON CONFLICT DO UPDATE reads ip and last_seen_at and writes last_seen_at
GRANT SELECT (ip, last_seen_at), UPDATE (last_seen_at) ON public.visitor_ips TO iplistener;
```

Nothing else is granted.

Supabase notes (run yourself). Check Row Level Security on `public.visitor_ips`; if it is enabled,
the role needs policies, because `ON CONFLICT DO UPDATE` needs insert, select and update:

```sql
CREATE POLICY iplistener_insert ON public.visitor_ips FOR INSERT TO iplistener WITH CHECK (true);
CREATE POLICY iplistener_select ON public.visitor_ips FOR SELECT TO iplistener USING (true);
CREATE POLICY iplistener_update ON public.visitor_ips FOR UPDATE TO iplistener USING (true) WITH CHECK (true);
```

Supabase also grants new `public` tables to `anon` and `authenticated` by default, which exposes
them through the Data API. Visitor IPs must not be readable there:

```sql
REVOKE ALL ON public.visitor_ips FROM anon, authenticated;
```

## Test

`go build ./... && go vet ./... && go test -race ./...` (no network, no database).
