# openipdata-site

The openipdata.org website: one static Go binary, `openipdata-site`. It is small and fast on purpose. Right now it serves the home page with one panel, "Accuracy".

The page is rendered once into memory and written as is on each request. All CSS and the two tiny theme scripts are inline, the fonts (Geist, Geist Mono) are embedded in the binary, and there are no third-party requests.

## Settings

| Variable   | Default                                            | Meaning                                         |
|------------|----------------------------------------------------|-------------------------------------------------|
| `LISTEN`   | `127.0.0.1:8080`                                   | address to listen on                            |
| `GEO_MMDB` | `/var/lib/openipdata-site/data/ip2geo-openipdata.mmdb` | the ip2geo MMDB whose metadata holds the scores |

An invalid `LISTEN` or `GEO_MMDB` stops the start with a message naming the variable.

## Where the numbers come from

The geo build stamps its scores into the MMDB metadata (`description.openipdata`). The page shows `atlas_country_pct`, `atlas_city_pct`, `city_pct`, `atlas_probes` and the build time (`built_at`, else the MMDB's own build time). The file is checked every 30 seconds and the page is rendered again when it changed; a failed reload keeps the current page.

Nothing is ever made up. A missing or invalid number shows a dash and an empty bar. If the MMDB is missing, unreadable or has no stamp, all three numbers are dashes, and the "updated" date is left out (an MMDB that opens but has no stamp still gives its own build time).

## Run locally

    cd site
    GEO_MMDB=../ip2geo-openipdata.mmdb LISTEN=127.0.0.1:8090 go run .

Then open http://127.0.0.1:8080. `/healthz` answers `ok <version>` (`ok dev` for a local build).

## Test

    cd site
    go vet ./... && go test -race ./...

`testdata/stamped.mmdb` and `testdata/unstamped.mmdb` are tiny fixtures with documentation addresses (192.0.2.0/24, 2001:db8::/32) and made-up numbers, built with the `geo` and `stamp` commands of `mmdbbuilder`.

## Release

The `Publish site binary` workflow (Actions tab, Publish site binary, Run workflow; file `.github/workflows/site.yml`) runs the checks, builds a static linux/amd64 binary stamped with the commit, and replaces the assets of the rolling GitHub release `site`: `openipdata-site-linux-amd64` and its `.sha256`. The release is never marked latest. Only runs on `master` publish; on other branches the workflow only tests and builds. GitHub shows the Run workflow button only once the workflow file is on the default branch (`master`). `/healthz` answers `ok <commit>`, so you can tell which build is running.

## Server

One Ubuntu 24.04 VM runs nginx and the site behind Cloudflare. Only Cloudflare reaches nginx. Everything is installed by one script from a git clone:

    cd openipdata.org/infra/site
    sudo ./init-openipdata-site.sh

The script is idempotent: rerun it after `git pull` to update scripts, units and the nginx file. It never overwrites `/etc/openipdata-site/openipdata-site.env`.

The TLS certificate is the one step the script does not do. Without it, everything else is installed, the nginx site stays disabled and the script prints what to do: put a Cloudflare API token (Zone:DNS:Edit for the zone) into `/root/.secrets/cloudflare.ini` (mode 0600, content `dns_cloudflare_api_token = <token>`), run

    certbot certonly --dns-cloudflare --dns-cloudflare-credentials /root/.secrets/cloudflare.ini -d openipdata.org -d www.openipdata.org

and rerun the script. Renewals run by the certbot timer; the deploy hook `/etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh` runs `nginx -t && systemctl reload nginx`.

The release `site` must exist first (Actions tab, `Publish site binary`, Run workflow, on `master`); until then the updater says "no release yet" and the timer installs the binary as soon as it appears.

| Unit (timer)                                  | Runs as            | What it does                                                                                       |
|-----------------------------------------------|--------------------|----------------------------------------------------------------------------------------------------|
| `openipdata-site.service`                     | `openipdata-site`  | the site on `127.0.0.1:8080`, from `/opt/openipdata-site/current/openipdata-site`, no secrets       |
| `openipdata-site-update` (every 2 min)        | root               | installs a new binary from the release `site`, checks it, rolls back on failure                    |
| `openipdata-mmdb-pull` (every 10 min)         | `openipdata-site`  | downloads `ip2geo-openipdata.mmdb` and `ip2asn-openipdata.mmdb` from the release `latest`           |
| `openipdata-cf-ips` (weekly, and at install)  | root               | refreshes the Cloudflare ranges in `/etc/nginx/conf.d/cloudflare-realip.conf`, `nginx -t`, reload  |

Paths: scripts in `/opt/openipdata-site/bin`, binaries in `/opt/openipdata-site/releases/<sha256>` with `current` pointing at the live one, MMDB files in `/var/lib/openipdata-site/data` (public data, with `<name>.key` change keys), updater state in `/var/lib/openipdata-site/state` (root only: `installed.sha256`, `bad.sha256`), settings in `/etc/openipdata-site/openipdata-site.env`, nginx in `/etc/nginx/sites-available/openipdata.org.conf` and `default-deny.conf`.

How it behaves:

- Updater: it fetches the `.sha256` asset. A missing or empty one means no release yet or an upload in progress: try later, no ping. The same hash as the installed one: nothing to do. Otherwise it downloads the binary into `releases/<sha>.tmp`; a checksum mismatch also means an upload in progress (try later). It asks the new binary for `-version`, switches `current` atomically, restarts the site and waits up to 20 seconds for `/healthz` to answer exactly `ok <version>`. On success it records the hash and keeps the 3 newest directories plus the current and the previous one. On failure it switches back to the previous one, records the hash in `bad.sha256` and pings `/fail`; a build in `bad.sha256` is never tried again (the alert is repeated each run) until a different build is released.
- MMDB puller: one unauthenticated, conditional GitHub API call per run: the response ETag and body are saved (`.release.etag`, `.release.json`, written only after a fully successful run) and sent back as `If-None-Match`; on 304 the saved JSON is used, and every file is still checked against its key, so a deleted MMDB is downloaded again. A file is downloaded when its asset `digest` (or `updated_at|size` when there is no digest) differs from the saved key; the size and digest are checked, then it is renamed over the old file. The site notices the new `ip2geo` file within 30 seconds. A failure keeps the old file and pings `/fail`.
- Cloudflare: `cf-ips` refuses an empty list or an invalid CIDR, writes through `.tmp` plus rename, and restores the old file when `nginx -t` fails. nginx answers 444 to any peer that is not a Cloudflare range, and takes the visitor address from `CF-Connecting-IP`, so `$remote_addr` and the access log (`/var/log/nginx/openipdata.org.access.log`) hold the visitor. `/healthz` is a 404 through nginx; the updater asks `127.0.0.1:8080` directly.
- Firewall: ufw allows the ports sshd really listens on (`sshd -T`; with none found it falls back to the OpenSSH profile or 22/tcp and warns), 80/tcp and 443/tcp, denies the rest of incoming.
- nginx default-deny: `default-deny.conf` (catch-all servers: 444 on port 80, rejected TLS handshake on 443) needs no certificate and is always enabled, so the origin is closed even before the certificate exists.
- Pings: set `HC_PING_SITE` and `HC_PING_MMDB` (healthchecks.io URLs) in the env file. A normal run pings the URL, a failure pings `<url>/fail`, an empty value does nothing. The URLs are never logged.

Settings (`/etc/openipdata-site/openipdata-site.env`): `GITHUB_REPO` (default `shingrus/openipdata.org`), `HC_PING_SITE`, `HC_PING_MMDB`. The site itself is configured in its unit (`LISTEN`, `GEO_MMDB`).

Logs: `journalctl -u openipdata-site -f`, `-u openipdata-site-update`, `-u openipdata-mmdb-pull`, `-u openipdata-cf-ips`; nginx in `/var/log/nginx/`. Timers: `systemctl list-timers 'openipdata-*'`.

Roll back by hand:

    cd /opt/openipdata-site
    systemctl stop openipdata-site-update.timer
    ls -t releases
    ln -sfn releases/<older-sha> current.tmp && mv -Tf current.tmp current
    systemctl restart openipdata-site
    echo <older-sha> > /var/lib/openipdata-site/state/installed.sha256

The updater installs whatever the release `site` has when it differs from `installed.sha256`, so a manual rollback holds only while the timer is stopped: run `systemctl stop openipdata-site-update.timer` first, and start it again after a fixed build is released.

## Not implemented yet

The data puller for the SQLite artifacts, builds on push to `master`, and `iplistener` on this VM (the access log it would tail is already written). The home page is `public, max-age=60` only because it shows no visitor data; a response that does must be `private, no-store`.
