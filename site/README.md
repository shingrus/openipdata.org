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

    sudo certbot certonly --dns-cloudflare --dns-cloudflare-credentials /root/.secrets/cloudflare.ini -d openipdata.org -d '*.openipdata.org'

The certificate covers `openipdata.org` and `*.openipdata.org`. www.openipdata.org is not served: it gets the default-deny answer (444, or a rejected TLS handshake on 443).

and rerun the script. Renewals run by the certbot timer; the deploy hook `/etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh` runs `nginx -t && systemctl reload nginx`.

The release `site` must exist first (Actions tab, `Publish site binary`, Run workflow, on `master`); until then the updater says "no release yet" and the timer installs the binary as soon as it appears.

| Unit (timer)                                  | Runs as            | What it does                                                                                       |
|-----------------------------------------------|--------------------|----------------------------------------------------------------------------------------------------|
| `openipdata-site@8080`, `openipdata-site@8081` | `openipdata-site` | the site on `127.0.0.1:<port>` (template, the instance name is the port), from `/opt/openipdata-site/slot-<port>/openipdata-site`, no secrets; one is live, the other idle |
| `openipdata-site-update` (every 2 min)        | root               | installs a new binary on the idle port, checks it, switches nginx to it; a bad build never touches the live side |
| `openipdata-mmdb-pull` (every 10 min)         | `openipdata-site`  | downloads `ip2geo-openipdata.mmdb` and `ip2asn-openipdata.mmdb` from the release `latest`           |
| `openipdata-cf-ips` (weekly, and at install)  | root               | refreshes the Cloudflare ranges in `/etc/nginx/conf.d/cloudflare-realip.conf`, `nginx -t`, reload  |

Paths: scripts in `/opt/openipdata-site/bin`, binaries in `/opt/openipdata-site/releases/<sha256>` with the links `slot-8080` and `slot-8081` pointing at two of them, upstream files in `/etc/nginx/openipdata-site/` (`upstream-8080.conf`, `upstream-8081.conf`) and the link `/etc/nginx/conf.d/openipdata-site-live.conf` to the live one, MMDB files in `/var/lib/openipdata-site/data` (public data, with `<name>.key` change keys), updater state in `/var/lib/openipdata-site/state` (root only: `installed.sha256`, `bad.sha256`), settings in `/etc/openipdata-site/openipdata-site.env`, nginx in `/etc/nginx/sites-available/openipdata.org.conf` and `default-deny.conf`.

How it behaves:

- Updater (blue/green): it fetches the `.sha256` asset. A missing or empty one means no release yet or an upload in progress: try later, no ping. The same hash as the installed one: nothing to do. A hash in `bad.sha256` is never tried again (the alert is repeated each run) until a different build is released. Every run first starts the live instance if it is not active. A deploy:
  1. The idle port is the other one than the live port; the live port is always read from the link `openipdata-site-live.conf` (nothing else records it). The binary is downloaded into `releases/<sha>.tmp`, and `-version` is run unprivileged with a 10 second limit.
  2. `slot-<idle>` is pointed at the new release, `openipdata-site@<idle>` is restarted, and `/healthz` on the idle port must answer exactly `ok <version>` within 20 seconds. If not, the idle instance is stopped, the slot is put back, the hash goes to `bad.sha256` and `/fail` is pinged. The live side is never touched.
  3. The link is switched to `upstream-<idle>.conf`, `nginx -t` runs and nginx is reloaded gracefully (old workers finish their requests on the old port). If `nginx -t` or the reload fails, the link is put back, the idle instance is stopped and `/fail` is pinged, but the build is not marked bad. If nginx is not running (site not enabled before the certificate), the link is just kept.
  4. Only the live instance is enabled (the new one is enabled, the old one disabled). After 10 seconds of draining the old instance is stopped (graceful shutdown finishes in-flight requests), the hash is recorded and `live: <port> (version ...)` is logged.
  5. The next deploy uses the other port. It keeps the directories both slots point to plus the 3 newest.
- MMDB puller: one unauthenticated, conditional GitHub API call per run: the response ETag and body are saved (`.release.etag`, `.release.json`, written only after a fully successful run) and sent back as `If-None-Match`; on 304 the saved JSON is used, and every file is still checked against its key, so a deleted MMDB is downloaded again. A file is downloaded when its asset `digest` (or `updated_at|size` when there is no digest) differs from the saved key; the size and digest are checked, then it is renamed over the old file. The site notices the new `ip2geo` file within 30 seconds. A failure keeps the old file and pings `/fail`.
- Cloudflare: `cf-ips` refuses an empty list or an invalid CIDR, writes through `.tmp` plus rename, and restores the old file when `nginx -t` fails. nginx answers 444 to any peer that is not a Cloudflare range, and takes the visitor address from `CF-Connecting-IP`, so `$remote_addr` and the access log (`/var/log/nginx/openipdata.org.access.log`) hold the visitor. `/healthz` is a 404 through nginx; the updater asks the instance port directly.
- Firewall: ufw allows the ports sshd really listens on (`sshd -T`; with none found it falls back to the OpenSSH profile or 22/tcp and warns), 80/tcp and 443/tcp, denies the rest of incoming.
- nginx default-deny: `default-deny.conf` (catch-all servers: 444 on port 80, rejected TLS handshake on 443) needs no certificate and is always enabled, so the origin is closed even before the certificate exists.
- Pings: set `HC_PING_SITE` and `HC_PING_MMDB` (healthchecks.io URLs) in the env file. A normal run pings the URL, a failure pings `<url>/fail`, an empty value does nothing. The URLs are never logged.

Settings (`/etc/openipdata-site/openipdata-site.env`): `GITHUB_REPO` (default `shingrus/openipdata.org`), `HC_PING_SITE`, `HC_PING_MMDB`. The site itself is configured in its unit (`LISTEN=127.0.0.1:<port>`, `GEO_MMDB`).

Which side is live: `readlink /etc/nginx/conf.d/openipdata-site-live.conf` (the port is in the name).

Logs: `journalctl -u 'openipdata-site@*' -f`, `-u openipdata-site-update`, `-u openipdata-mmdb-pull`, `-u openipdata-cf-ips`; nginx in `/var/log/nginx/`. Timers: `systemctl list-timers 'openipdata-*'`.

Roll back by hand (the idle instance still points at the previous release). Stop the update timer first if the newer build must not come back:

    systemctl stop openipdata-site-update.timer
    readlink /etc/nginx/conf.d/openipdata-site-live.conf        # live port, say 8081; the other one is idle (8080)
    readlink /opt/openipdata-site/slot-8080                      # the previous release
    systemctl start openipdata-site@8080
    curl -fsS http://127.0.0.1:8080/healthz                      # must answer ok <version>
    ln -sfn /etc/nginx/openipdata-site/upstream-8080.conf /etc/nginx/conf.d/openipdata-site-live.conf.tmp
    mv -Tf /etc/nginx/conf.d/openipdata-site-live.conf.tmp /etc/nginx/conf.d/openipdata-site-live.conf
    nginx -t && systemctl reload nginx
    systemctl enable openipdata-site@8080 && systemctl disable openipdata-site@8081
    systemctl stop openipdata-site@8081
    echo <older-sha> > /var/lib/openipdata-site/state/installed.sha256

The updater installs whatever the release `site` has when it differs from `installed.sha256`, so a manual rollback holds only while the timer is stopped; start it again after a fixed build is released.

To stop the site on purpose: `systemctl disable --now openipdata-site@<live port>` (the updater only restarts an enabled live instance), or stop `openipdata-site-update.timer`.

## Not implemented yet

The data puller for the SQLite artifacts, builds on push to `master`, and `iplistener` on this VM (the access log it would tail is already written). The home page is `public, max-age=60` only because it shows no visitor data; a response that does must be `private, no-store`.
