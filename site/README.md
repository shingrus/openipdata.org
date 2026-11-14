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

Then open http://127.0.0.1:8080. `/healthz` answers `ok`.

## Test

    cd site
    go vet ./... && go test -race ./...

`testdata/stamped.mmdb` and `testdata/unstamped.mmdb` are tiny fixtures with documentation addresses (192.0.2.0/24, 2001:db8::/32) and made-up numbers, built with the `geo` and `stamp` commands of `mmdbbuilder`.

## Not implemented yet

Deployment (nginx, systemd, automatic rollout) and the data puller that replaces the MMDB are not implemented yet. The home page is `public, max-age=60` only because it shows no visitor data; a response that does must be `private, no-store`.
