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

The `site` workflow (Actions tab, site, Run workflow) runs the checks, builds a static linux/amd64 binary stamped with the commit, and replaces the assets of the rolling GitHub release `site`: `openipdata-site-linux-amd64` and its `.sha256`. The release is never marked latest. Only runs on `master` publish; on other branches the workflow only tests and builds. GitHub shows the Run workflow button only once the workflow file is on the default branch (`master`). `/healthz` answers `ok <commit>`, so you can tell which build is running.

## Not implemented yet

The updater on the web servers (a systemd timer that downloads the binary, checks the sha256, swaps it in, restarts, checks `/healthz` and rolls back), nginx, and the data puller that replaces the MMDB. nginx must keep `/healthz` local (localhost only), since it shows the commit. Builds on push to `master` are not set up either. The home page is `public, max-age=60` only because it shows no visitor data; a response that does must be `private, no-store`.
