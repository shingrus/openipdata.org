# openipdata-site

The openipdata.org website: one static Go binary, `openipdata-site`. It is small and fast on purpose. It serves the home page with a lookup bar and two panels ("Your connection" or "Lookup", and "Accuracy"), and the ASN pages (one network, one organization, four rankings, and a JSON answer for one network).

The page is rendered on each request from small in-memory data (the scores and two open MMDBs). All CSS and the three tiny scripts (saved theme, theme button, and the one that fills in the second address; plain files `static/head.js`, `static/theme.js` and `static/fill.js`, embedded in the binary) are put inline into the page; the CSP hash of each is computed at start from exactly the text that is put in the page, and an empty script file stops the start, the fonts (Geist, Geist Mono) are embedded in the binary, and there are no third-party requests.

## Settings

| Variable   | Default                                            | Meaning                                         |
|------------|----------------------------------------------------|-------------------------------------------------|
| `LISTEN`   | `127.0.0.1:8080`                                   | address to listen on                            |
| `GEO_MMDB` | `/var/lib/openipdata-site/data/ip2geo-openipdata.mmdb` | the ip2geo MMDB: city and country of an address, and in its metadata the scores |
| `ASN_MMDB` | `/var/lib/openipdata-site/data/ip2asn-openipdata.mmdb` | the ip2asn MMDB: network, AS number and announced prefix |
| `ASN_SQLITE` | `/var/lib/openipdata-site/data/asn-openipdata.sqlite3` | the SQLite file of the ASN pages (published by the worker as `asn-openipdata.sqlite3`); missing or unreadable = the ASN pages answer 503 |
| `REAL_IP_HEADER` | `X-Real-IP` | header that holds the visitor address (the server unit sets it too); set it to empty to use the address of the TCP connection |
| `V4_URL` | `https://v4.openipdata.org/` | URL on a name with an A record only; answers with the visitor's IPv4 address (see "Both addresses"); empty = none |
| `V6_URL` | `https://v6.openipdata.org/` | URL on a name with an AAAA record only; answers with the visitor's IPv6 address; empty = none |
| `BEACON_ZONE` | empty | zone of the one-time names of the DNS row (`<token>.<zone>`, on the server `r.openipdata.org`), a lower-case host name; empty = no DNS row, no script, `/resolver` answers 404 |
| `REDIS_ADDR` | `127.0.0.1:6379` | loopback address of the Redis where our DNS server records the resolver of each name |

An invalid `LISTEN`, a directory as `GEO_MMDB`, `ASN_MMDB` or `ASN_SQLITE`, or a `REAL_IP_HEADER` that is not a header name, a `BEACON_ZONE` that is not a lower-case host name, a `REDIS_ADDR` that is not a loopback `host:port`, or a `V4_URL` / `V6_URL` that is neither empty nor an absolute `https://` URL (`http://` is accepted only for `127.0.0.1` and `localhost`, for local tests) stops the start with a message naming the variable.

## Your connection and lookup

`/` shows the lookup bar (filled with the visitor's address) and the "Your connection" panel. `/?<ip>` (for example `/?1.1.1.1` or `/?2001:db8::1`) shows the same page for that address with the title "Lookup". The query must be exactly the canonical text of the address (IPv6 in lower case and shortest form, an IPv4-mapped address as IPv4); any other spelling (upper case, percent-encoding, spaces, `::ffff:1.2.3.4`) redirects to the canonical URL. The lookup bar is a plain form (`GET /`, field `q`); `/?q=<ip>` answers with a redirect to `/?<ip>`, the same redirect.

The panel has the place (city with the country under it; country alone, with no second line; or a grey "Location unknown") and three rows: Network (organisation, else name, plus `AS<number>`), IPv4 or IPv6 (an IPv4-mapped IPv6 address is shown as IPv4) and Prefix (the announced prefix). Anything the MMDBs do not know is a dash. Behind the place name there is a small outline map (see "The map"). The lookup bar ("IP, AS or name") reads `q` in this order: an IP address; `AS15169` or `as15169`; a bare number (`15169`); a CIDR prefix such as `8.8.8.0/24` (redirects to `/prefix/8.8.8.0/24`; out of range or not a prefix stays 400); a name. The raw form `/?15169` works like `/?q=15169`. A number is an AS number from 1 to 4294967295 (leading zeros are ignored) and redirects to `/as15169`; 0 or a larger number shows the "AS number out of range" page (404). Input that looks like an attempt at an address (it has a `:`, or it is only digits, dots and slashes with at least one dot, such as `1.2.3`, `1.1.1.1:80`, `1.1.1.1/33`, `fe80::1%eth0`) is never a name: status 400 "Not an IP address, AS number or name". An empty `q` (the empty lookup box) redirects to `/`. A lookup page has the address in its title. A loopback, private, link-local, CGNAT, multicast, documentation or other reserved address shows the address and "Not a public address" with no lookup (`isPublic` in `lookup.go`).

Both MMDBs are checked every 30 seconds and opened again when their file changes (the old reader is closed only when no lookup uses it). A missing or unreadable file only gives unknown values. Addresses are never logged.

Name search: the text is made a slug (lower case, every run of characters outside `a-z0-9` becomes one `-`, no `-` at the ends, at most 64 characters). Under 2 characters, or no letters or digits at all: 400 as above. The slug is looked for in the slugs of all organizations in memory (one pass, no SQL; about 1 ms for 100,000 organizations). Order, three groups: (1) the slug equals the term or starts with it, or the organization has a network whose handle is the term (case does not matter, `GOOGLE`); (2) the term starts a later word of the slug (after a `-`); (3) the slug only contains it; inside each group the organization with more IPv4 addresses first (so `telecom` lists the big "Telecom ..." organizations first). An organization that matches twice (slug and handle) counts once. Exactly one match redirects (302) to `/asn-<slug>`. Several matches show a "Search" page at the canonical URL `/?q=<term>` (any other spelling of the term redirects there): at most 50 organizations (name, networks, IPv4 addresses) and, when there are more, one line like "120 more: type more letters"; the lookup bar holds the term. No match: the same page with "Nothing found", status 404. Without ASN data, a name or an out-of-range number answers 503 "ASN data is not loaded" (a number in range still redirects, and the `/as…` page answers 503). These answers hold no visitor data and are `public, max-age=300`; the term is never logged.

Cache: `/` shows visitor data, so it is `private, no-store` with no `ETag`. A lookup page has no visitor data and is `public, max-age=300`. A bad query is `no-store`. Results, nothing found and redirects of the name search are `public, max-age=300`.

The visitor address is the value of the `REAL_IP_HEADER` header when the peer of the TCP connection is loopback (our nginx); a missing, repeated or invalid value means unknown, never the proxy's address. The header is `X-Real-IP` unless the setting says otherwise, so a new binary works under an older unit. A peer that is not loopback is taken as the visitor itself, except a private or link-local peer while a header is set (most likely a proxy moved off loopback): that is unknown too. With `REAL_IP_HEADER=` (empty) no header is trusted and the peer is always the visitor. nginx always sets `X-Real-IP` to `$remote_addr`, which after the Cloudflare real-IP step is the visitor, and replaces any header a client sent. Run locally, the page says "Address unknown" (no nginx sends the header); with `REAL_IP_HEADER=` the visitor is `127.0.0.1`, which is not public.

## The map

The place block of "Your connection" and "Lookup" has a small outline map behind the name. It is drawn on the server, as one inline SVG, from the country outlines in `static/world.bin` (Natural Earth 1:50m, public domain; made by `tools/mapdata`, which has the source, the hash of the input and the command to regenerate it; the file is committed, like the fonts). There is no script, no image and no request; the CSP is unchanged and nothing is added to the head. The ASN pages and the search results have no map.

| The record has | The map |
|----------------|---------|
| a city with `latitude` and `longitude` | centred on the city: two dashed lines cross at the point, a dot with a halo |
| a country but no city | that country's outline in the accent colour, centred on its label point, no lines |
| no country, "Not a public address", "Not an IP address", a country code that is not in the data | no map |
| a city but no coordinates (the ip2geo MMDB published before `-geonames` was used) | no map: the page is as it was before |

The two coordinates are optional MMDB doubles (`latitude`, `longitude`), written by `mmdbbuilder geo -geonames` only for a city it placed. Missing, out of range (latitude outside -90..90, longitude outside -180..180) or not a number: no map. (0, 0) is a real place. A country-only record needs no coordinates, so it gets its map from any MMDB that has the country code.

The frame is 46 x 18.2 units; a unit is one degree of latitude and a degree of longitude is `cos(latitude of the centre)` of it (clamped so the frame never needs more than the whole world), so the frame is centred on the point. The world repeats every 360 degrees, so a city at longitude 179 has its neighbours at -179. Land is clipped to the frame plus one unit (Sutherland-Hodgman), so no line shows along the frame edge, and written as one `<path class="land">` of integer coordinates in tenths of a unit (an absolute `M`, then relative steps, no redundant points); the highlighted country is a second `<path class="here">`. The SVG is decorative (`aria-hidden`, no text), sits absolutely behind the name, and uses `preserveAspectRatio="slice"` so the point stays in the middle of the block whatever its shape. The block keeps its fixed `min-height` with or without a map, so the page does not move. Colours are the existing tokens (land `--raised`, outlines `--line-2`, highlight `--accent` and `--accent-soft`), with a fade at the bottom so the name stays readable; both themes work. Natural Earth draws disputed borders its own way; the owner chose country borders knowingly.

Size: the file is embedded (51 KB) and decoded once at start (under 1 ms, about 130 KB of memory; a damaged file stops the start with `map data: world.bin is not valid`). One map is 1 to 8 KB of markup (Lisbon 4 KB, Tokyo 2.4 KB, London, the densest of the tested cities, 7.7 KB; limit 9 KB, `TestMapSizes`), 0.5 to 3 KB gzipped, and takes 30 to 80 microseconds to draw (`go test -bench DrawMap`). The map CSS adds about 0.7 KB to every page.

## Both addresses

A browser connects over IPv4 or IPv6, so the server sees one address. `GET /ip` (and `HEAD`) answers `text/plain` with the visitor address in canonical form and a newline (`Cache-Control: no-store`, `Access-Control-Allow-Origin: *`, the address is only the requester's own); an unknown address gives status 503 and `unknown`. It uses the same rules as the page (`REAL_IP_HEADER`) and never logs the address. It works through `openipdata.org` too (the address of the Cloudflare visitor).

The other address comes from a second request. On the visitor's own page ("Your connection" with a public address) the panel has an IPv4 row and an IPv6 row, in that order. The family the server saw is filled in by the server. The other row shows a dash and is filled in by one small inline script at the end of the body: after the page is parsed it asks `V4_URL` or `V6_URL` (the one of the missing family, 4 s timeout, no cookies, no referrer), checks that the answer is a plausible address of that family and puts it in the row. On any failure, or without JavaScript, the dash stays; the page is complete without it, and the dash row has the room of a long IPv6 address so nothing moves. A lookup page, "Address unknown" and "Not a public address" have no second row and no script. If the URL of the missing family is empty, the page is as it was before (one family row, no script). The CSP of a page with the script adds the script's hash and `connect-src` for the origins of the two URLs; every other page keeps the old CSP.

### DNS row

On the visitor's own page (public address, `BEACON_ZONE` set) a "DNS" row shows the network of the visitor's DNS resolver; without JavaScript or on any failure it stays a dash (it has the height of two lines, so the page does not move). `static/resolver.js` (the last script, after `fill.js`, own CSP hash) makes a 32-hex token with `crypto.getRandomValues` and fetches `https://<token>.<zone>/` (CORS, no cookies, 3 s abort; first at once, after a 404 again after 0.8, 1.5 and 2.5 s, then it gives up; any other status or error gives up at once). That lookup makes the resolver ask our DNS server, which writes `beacon:<token>` (hash field `resolver_ip`, 120 s) to Redis on loopback; nginx proxies the name to `GET /resolver?t=<token>` (also `HEAD`; `no-store`, `Access-Control-Allow-Origin: *`; the token is a secret and the answer holds no address). The site reads Redis with a small standard-library client (one TCP connection per request, 300 ms in all, reply at most 64 bytes). Answers: bad token (not exactly 32 lower-case hex characters) 400 `{}`; not in Redis yet 404 `{}`; Redis unavailable 503 `{}`; found and public: 200 `{"name":"Google LLC","asn":15169}` from the ip2asn MMDB (same choice of name as the Network row; an unknown field is left out, nothing known gives `{}`); a non-public or unreadable address 200 `{}`. The resolver address is never sent to the page; neither it nor the token is logged (one log line per distinct Redis error kind). The script shows the name as text and `AS<n>` as a link, with strict checks. The CSP of the page with the row adds the script's hash and `https://*.<zone>` to `connect-src`; every other page is unchanged. Tests use a fake Redis listener.

The status page (`status.go`) keeps no visitor statistics and counts nothing: it shows the process start (taken in `newServer`), the build and load time of each file (recorded when the file is loaded; MMDB build time from its metadata, SQLite from the file's modification time), and one Redis check (a command on a key that cannot exist, 300 ms at most, nothing logged). Only base names of files are shown. Ages come from the server's `now` clock, which tests replace.

DNS (the owner keeps these, in Cloudflare, DNS only, grey cloud, NOT proxied): `v4.openipdata.org` has an A record only (the VM's IPv4), `v6.openipdata.org` has an AAAA record only (the VM's IPv6). The two names are served by `infra/site/nginx/v46.openipdata.org.conf` (direct visitors, only `/`, rate limit 5 requests per second per address, own log without addresses). They show the VM's address by design. A request that comes from a Cloudflare address gets 403 on these two names (every path, both ports): if the DNS records were proxied by mistake, the page would show Cloudflare's address as the visitor's second address, so the answer is refused and the dash stays. Check:

    curl -4 https://v4.openipdata.org/     # your IPv4 address
    curl -6 https://v6.openipdata.org/     # your IPv6 address
    curl -6 https://v4.openipdata.org/     # must fail: no AAAA record
    curl -4 https://v6.openipdata.org/     # must fail: no A record
    curl -i https://v4.openipdata.org/x    # 404, never reaches the site

The new nginx file is installed by the init script: rerun `sudo ./init-openipdata-site.sh` on the VM after `git pull` (the file is enabled only once the certificate exists, like the main site), and the unit (`V4_URL`, `V6_URL`) is updated by the same run.

## ASN pages

The data is the SQLite file the worker builds (`asn-openipdata.sqlite3`: tables `asn` and `asn_domain`). The file is opened read-only and immutable and checked every 30 seconds, like the MMDBs; a changed file is read again and swapped in (the old handle is closed only when no request uses it; a broken or vanished file keeps what is loaded). A missing or unreadable file is not fatal: the ASN pages then answer 503 "ASN data is not loaded", the AS text in the lookup panel is plain text, and the rest of the site works.

| URL | Page |
|-----|------|
| `/as15169` | one network: name and handle, a readout (handle, organization, domain, country, category, prefix counts, IPv4 addresses), the other networks of the organization (100 at most), the prefixes (500 per family at most, one line with the total and a link to the JSON when there are more), and the links to the four rankings |
| `/asn-google-llc` | one organization: total IPv4 addresses and number of networks, then its networks, biggest first (500 at most) |
| `/top-asn-by-ip-address`, `/top-asn-by-ipv6` | the 100 networks with the most IPv4 addresses or IPv6 /64 networks |
| `/top-organizations-by-ip-address` | the 100 organizations with the most IPv4 addresses (summed over their networks) |
| `/list-of-countries-by-ipv4-allocation` | all countries with their IPv4 addresses and number of networks |
| `/prefix/8.8.8.0/24`, `/prefix/2001:4860::/32` | one prefix (IPv4 /8 to /32, IPv6 /16 to /128): the announcing networks, the size, the announced prefixes that contain it and those inside it (100 at most), and the place from the geo MMDB (one place in the head, several in a "Places" table with shares, none when more than 20,000 records lie inside). A prefix announced exactly is indexable, every other prefix page has noindex; not announced = 404; another spelling redirects (301) to the canonical one |
| `/api/as15169` | JSON: `{"asn":15169,"domain":"about.google","data":{...}}`, `data` is the `json` column as it is (all prefixes), `domain` is left out when unknown |
| `/status` (`/ss` redirects 301) | the owner's status page: server (start, version), each data file (built, loaded, size; the SQLite file also networks, organizations, prefixes, load time; `not loaded` when absent), the Accuracy numbers and the DNS beacon (on or off, zone, Redis ok or unreachable). `no-store`, noindex, no query used |

`/AS15169` and `/as015169` redirect (301) to `/as15169`. A number that is 0, above 4294967295 or not in the data, and an unknown or invalid organization, give a 404 page in the site design (the API gives a 404 JSON `{"error":"not found"}`). Numbers and slugs are checked strictly (slug: `a-z`, `0-9` and `-`, up to 255 characters) and the database is only queried with parameters. Networks with no organization slug have no organization page and are not in the organization ranking; networks with no country code are not in the country list. IPv6 amounts are shown as a number of /64 networks (addresses / 2^64): a whole number with thousands separators, `<1` below one /64. Lookup, search and error pages carry `<meta name="robots" content="noindex">`; every page has a footer line of links to the four rankings. The Network row of the lookup panel links the organisation name to `/asn-<slug>` when the loaded ASN data has that AS number with a valid slug (plain text otherwise).

Memory and speed: the lists need only a compact record per network (number, handle, organization, slug, country, amounts, prefix counts), read once per load. The prefix counts and the country name come from SQLite's JSON functions during that read, so the 60 MB of JSON is never held in memory. The network page and the JSON read their one row from the file per request. On the sample file (103,885 networks) the load takes about 0.7 seconds and the process uses about 50 MB more memory than without the file (measured on a Mac).

Cache: the four rankings are rendered once per data load and served from memory with an `ETag` (a matching `If-None-Match` gives 304) and `Cache-Control: public, max-age=300`. The network, organization and JSON responses are rendered per request and are `public, max-age=300` too. No ASN page holds visitor data, and the lookup bar on them is empty. They use the same two inline scripts as the home page and add a second inline stylesheet part (`static/asn.css`) only on these pages.

The worker must publish `asn-openipdata.sqlite3` to the release `latest` before the server can show the ASN pages (see "Server"). Until then the puller logs `not published yet` (no failure ping, as long as there is no local copy) and the pages answer 503.

## Where the numbers come from

The geo build stamps its scores into the MMDB metadata (`description.openipdata`). The page shows `atlas_country_pct`, `atlas_city_pct`, `city_pct`, `atlas_probes` and the build time (`built_at`, else the MMDB's own build time). The file is checked every 30 seconds and the page is rendered again when it changed; a failed reload keeps the current page.

Nothing is ever made up. A missing or invalid number shows a dash and an empty bar. If the MMDB is missing, unreadable or has no stamp, all three numbers are dashes, and the "updated" date is left out (an MMDB that opens but has no stamp still gives its own build time).

## Run locally

    cd site
    GEO_MMDB=../ip2geo-openipdata.mmdb ASN_MMDB=../ip2asn-openipdata.mmdb ASN_SQLITE=../2026-11-14_asn.sqlite3 LISTEN=127.0.0.1:8090 go run .

To see the map, build your own tiny geo MMDB with a real prefix and `-geonames` (the commands in "Test" below with a real prefix such as `1.1.1.0/24` as Sydney, AU and a one-line GeoNames file for Sydney) and open `/?1.1.1.1`; a row with a country and no city (for example `8.8.8.0/24`, US) shows the highlighted country. Then open http://127.0.0.1:8090. `../2026-11-14_asn.sqlite3` is a sample of the worker's SQLite file in the repo root (git-ignored; read only, never copy it into `site/`); without `ASN_SQLITE` the ASN pages answer 503. Try `/as15169`, `/asn-google-llc`, `/top-asn-by-ip-address` and `/api/as15169`. A browser caches these pages for 5 minutes: add a query like `?r=2` to see a changed page. Use `/?<ip>` with a public address to see a result (the visitor is unknown locally; to try the visitor path send `curl -H 'X-Real-IP: 1.1.1.1' 127.0.0.1:8090/`). The fixtures in `testdata/` only know documentation addresses, which the page treats as not public, so the tests replace the public check; to see a full panel in a browser build your own tiny MMDBs with a real prefix. `/healthz` answers `ok <version>` (`ok dev` for a local build).

## Test

    cd site
    go vet ./... && go test -race ./...

`testdata/stamped.mmdb` and `testdata/unstamped.mmdb` are tiny fixtures with documentation addresses (192.0.2.0/24, 2001:db8::/32) and made-up numbers, built with the `geo` and `stamp` commands of `mmdbbuilder`.

`testdata/lookup-geo.mmdb` and `testdata/lookup-asn.mmdb` are the lookup fixtures, documentation addresses only. `lookup-geo.mmdb`: 203.0.113.0/24 and 2001:db8::/32 are Lisbon, Portugal, with coordinates (38.7167, -9.1333, so they get the map with the aim); 198.51.100.0/24 is Spain with no city (and so no coordinates: the country map); 192.0.2.0/24 is absent (no map). For the prefix pages it also has 198.18.0.0/25 Lisbon, 198.18.0.128/26 Spain with no city (the rest of 198.18.0.0/24 is absent), and 198.18.1.0/25 Lisbon and 198.18.1.128/25 Porto (no coordinates); the source CSV is `203.0.113.0/24,PT,,Lisbon`, `2001:db8::/32,PT,,Lisbon`, `198.51.100.0/24,ES,,`, `198.18.0.0/25,PT,,Lisbon`, `198.18.0.128/26,ES,,`, `198.18.1.0/25,PT,,Lisbon`, `198.18.1.128/25,PT,,Porto`. `stamped.mmdb` and `unstamped.mmdb` have Lisbon with no coordinates (the file as published before coordinates existed: no map). `lookup-asn.mmdb`: AS64500, handle `EXAMPLE-NET`, organisation "Example Net", for 203.0.113.0/24 and 2001:db8::/32. They were made with the repo's `mmdbbuilder` (built from `mmdbbuilder/`, nothing in `site/` is involved):

    # geo: a gzipped CSV in the layer layout, named like a layer
    printf 'prefix,country,region,city,source\n203.0.113.0/24,PT,,Lisbon,fixture\n2001:db8::/32,PT,,Lisbon,fixture\n198.51.100.0/24,ES,,,fixture\n198.18.0.0/25,PT,,Lisbon,fixture\n198.18.0.128/26,ES,,,fixture\n198.18.1.0/25,PT,,Lisbon,fixture\n198.18.1.128/25,PT,,Porto,fixture\n' | gzip -c > 90-fixture.csv.gz
    # one GeoNames-format line (tab separated, 19 columns) for the city; the real cities1000.txt works the same
    printf '2267057\tLisbon\tLisbon\tLisboa,Lisbonne\t38.71667\t-9.13333\tP\tPPLC\tPT\t\t14\t\t\t\t517802\t\t\tEurope/Lisbon\t2024-01-01\n' > cities1000.txt
    mmdbbuilder geo -out site/testdata/lookup-geo.mmdb -geonames cities1000.txt 90-fixture.csv.gz

    # asn: one ipverse-style directory as/64500/aggregated.json
    echo '{"asn":64500,"metadata":{"handle":"EXAMPLE-NET","description":"Example Net","countryCode":"PT"},"prefixes":{"ipv4":["203.0.113.0/24"],"ipv6":["2001:db8::/32"]}}' > as/64500/aggregated.json
    mmdbbuilder asn -in as -out site/testdata/lookup-asn.mmdb

`.gitignore` lets these four `.mmdb` files into Git.

The map tests (`world_test.go`, `worldmap_test.go`) write a tiny `world.bin` with test code that follows the format comment of `world.go` (round trip, every kind of damage rejected), and cover the embedded file (small states are there), clipping (inside, outside, crossing, a ring that covers the frame), the antimeridian, the projection scale at high latitudes, the path encoding, the three page states, a city without coordinates, invalid coordinates, no map on the ASN pages, the place block layout, and the size limits for the 16 places of the size budget. `go test -bench 'DrawMap|ParseWorld' -run xxx` measures drawing and decoding. `tools/mapdata` has its own tests.

The ASN tests (`asn_test.go`) build a tiny SQLite file in a temp dir with the same `CREATE TABLE` statements as the worker (documentation prefixes and made-up names only), so no SQLite file is in Git. They cover every route and redirect, unknown and out-of-range numbers, the caps (500 prefixes, 100 and 500 networks), missing fields and bad JSON, the JSON endpoint, the lookup redirect and the link in the panel, the 503 state, reload on file change under `-race`, the cache headers with `ETag` and 304, and page size limits. The prefix tests (`prefix_test.go`) cover the index (both families, the same prefix from two networks, skipped entries, the ends of the address space), every redirect and error of `/prefix/`, noindex, the head, one and several places, the 20,000-record limit (made small in the test), the links on the network page and in the lookup panel, and the CIDR in the lookup box.

## Release

The `Publish site binary` workflow (`.github/workflows/site.yml`) runs the checks, builds a static linux/amd64 binary and replaces the assets of the rolling GitHub release `site`: `openipdata-site-linux-amd64` and its `.sha256`. The build is `CGO_ENABLED=0`, so the binary is static; the SQLite driver (`modernc.org/sqlite`) is pure Go. The release is never marked latest.

To release, tag a commit that is on `master` and push the tag:

    git tag site-YYYY-MM-DD && git push origin site-YYYY-MM-DD

Push the master commit first, then the tag. A tag whose commit is not on master yet only tests and builds; after merging, delete and push the tag again: `git push origin :refs/tags/<tag>` then `git push origin <tag>`. Tag names use only letters, digits, `.`, `_` and `-`.

A second release on the same day gets a suffix, like `site-2026-11-15b`. The Run workflow button (Actions tab) still works on `master`; GitHub shows it only once the workflow file is on `master`.

Guard: a run publishes only on `master`, or for a `site-*` tag whose commit is already on `master`. Any other ref (a branch, a tag on an unmerged commit) only tests and builds.

`/healthz` answers `ok <version>`: `ok site-2026-11-15@<sha>` for a tag release, `ok <sha>` for a Run workflow release. The release notes name the tag and the commit.

Rollback: tag an older `master` commit (for example `site-2026-11-16-rollback` on the commit you want) and push the tag; it passes the guard and publishes that older build. The updater then installs it.

## Server

One Ubuntu 24.04 VM runs nginx and the site behind Cloudflare. Only Cloudflare reaches nginx. Everything is installed by one script from a git clone:

    cd openipdata.org/infra/site
    sudo ./init-openipdata-site.sh

The script is idempotent: rerun it after `git pull` to update scripts, units and the nginx file. It never overwrites `/etc/openipdata-site/openipdata-site.env`.

The TLS certificate is the one step the script does not do. Without it, everything else is installed, the nginx site stays disabled and the script prints what to do: put a Cloudflare API token (Zone:DNS:Edit for the zone) into `/root/.secrets/cloudflare.ini` (mode 0600, content `dns_cloudflare_api_token = <token>`), run

    sudo certbot certonly --dns-cloudflare --dns-cloudflare-credentials /root/.secrets/cloudflare.ini -d openipdata.org -d '*.openipdata.org'

The certificate covers `openipdata.org` and `*.openipdata.org`. www.openipdata.org is not served: it gets the default-deny answer (444, or a rejected TLS handshake on 443).

and rerun the script. Renewals run by the certbot timer; the deploy hook `/etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh` runs `nginx -t && systemctl reload nginx`.

DNS beacon (resolver check on `r.openipdata.org`): rerun the script. It takes the VM's public addresses from `v4.openipdata.org` and `v6.openipdata.org` when they are addresses of this machine; `PUBLIC_IPV4` and `PUBLIC_IPV6` in `/etc/openipdata-site/openipdata-site.env` override that (needed only behind NAT). With no IPv4 address found the whole beacon setup is skipped. The script installs Knot DNS, Redis and the unit `openipdata-beacon` (binary from the release `site`, asset `openipdata-beacon-linux-amd64`, installed by `openipdata-site-update`), writes the zone from `infra/site/knot/` and prints the two DNS records to add (`r NS ns-r.openipdata.org`, `ns-r A <ip>`, DNS only) and the one-time certbot command for `*.r.openipdata.org` (the DNS challenge is answered by Knot). Checks on the VM: `sudo knotc conf-check`, `dig @<ip> <32hex>.r.openipdata.org` then `redis-cli hgetall beacon:<32hex>`, and, once the certificate exists, `curl https://<32hex>.r.openipdata.org/`. The site learns the zone from the drop-in `/etc/systemd/system/openipdata-site@.service.d/beacon.conf`, which the script writes only while the nginx site for the one-time names is enabled. Details: `beacon/README.md`.

The release `site` must exist first (Actions tab, `Publish site binary`, Run workflow, on `master`); until then the updater says "no release yet" and the timer installs the binary as soon as it appears.

| Unit (timer)                                  | Runs as            | What it does                                                                                       |
|-----------------------------------------------|--------------------|----------------------------------------------------------------------------------------------------|
| `openipdata-site@8080`, `openipdata-site@8081` | `openipdata-site` | the site on `127.0.0.1:<port>` (template, the instance name is the port), from `/opt/openipdata-site/slot-<port>/openipdata-site`, no secrets; one is live, the other idle |
| `openipdata-site-update` (every 2 min)        | root               | installs a new binary on the idle port, checks it, switches nginx to it; a bad build never touches the live side |
| `openipdata-mmdb-pull` (every 10 min)         | `openipdata-site`  | downloads `ip2geo-openipdata.mmdb`, `ip2asn-openipdata.mmdb` and `asn-openipdata.sqlite3` from the release `latest` |
| `openipdata-cf-ips` (weekly, and at install)  | root               | refreshes the Cloudflare ranges in `/etc/nginx/conf.d/cloudflare-realip.conf`, `nginx -t`, reload  |

Paths: scripts in `/opt/openipdata-site/bin`, binaries in `/opt/openipdata-site/releases/<sha256>` with the links `slot-8080` and `slot-8081` pointing at two of them, upstream files in `/etc/nginx/openipdata-site/` (`upstream-8080.conf`, `upstream-8081.conf`) and the link `/etc/nginx/conf.d/openipdata-site-live.conf` to the live one, the MMDBs and the SQLite file in `/var/lib/openipdata-site/data` (public data, with `<name>.key` change keys), updater state in `/var/lib/openipdata-site/state` (root only: `installed.sha256`, `bad.sha256`), settings in `/etc/openipdata-site/openipdata-site.env`, nginx in `/etc/nginx/sites-available/openipdata.org.conf`, `v46.openipdata.org.conf` and `default-deny.conf`.

How it behaves:

- Updater (blue/green): it fetches the `.sha256` asset. A missing or empty one means no release yet or an upload in progress: try later, no ping. The same hash as the installed one: nothing to do. A hash in `bad.sha256` is never tried again (the alert is repeated each run) until a different build is released. Every run first starts the live instance if it is not active. A deploy:
  1. The idle port is the other one than the live port; the live port is always read from the link `openipdata-site-live.conf` (nothing else records it). The binary is downloaded into `releases/<sha>.tmp`, and `-version` is run unprivileged with a 10 second limit.
  2. `slot-<idle>` is pointed at the new release, `openipdata-site@<idle>` is restarted, and `/healthz` on the idle port must answer exactly `ok <version>` within 20 seconds. If not, the idle instance is stopped, the slot is put back, the hash goes to `bad.sha256` and `/fail` is pinged. The live side is never touched.
  3. The link is switched to `upstream-<idle>.conf`, `nginx -t` runs and nginx is reloaded gracefully (old workers finish their requests on the old port). If `nginx -t` or the reload fails, the link is put back, the idle instance is stopped and `/fail` is pinged, but the build is not marked bad. If nginx is not running (site not enabled before the certificate), the link is just kept.
  4. Only the live instance is enabled (the new one is enabled, the old one disabled). After 10 seconds of draining the old instance is stopped (graceful shutdown finishes in-flight requests), the hash is recorded and `live: <port> (version ...)` is logged.
  5. The next deploy uses the other port. It keeps the directories both slots point to plus the 3 newest.
- Data puller (`openipdata-mmdb-pull`; the two MMDBs and `asn-openipdata.sqlite3`): one unauthenticated, conditional GitHub API call per run: the response ETag and body are saved (`.release.etag`, `.release.json`, written only after a fully successful run) and sent back as `If-None-Match`; on 304 the saved JSON is used, and every file is still checked against its key, so a deleted MMDB is downloaded again. A file is downloaded when its asset `digest` (or `updated_at|size` when there is no digest) differs from the saved key; the size and digest are checked, then it is renamed over the old file. The site notices a new file within 30 seconds. A failure, or an MMDB asset that is not in the release, keeps the old file and pings `/fail`; the other files are still pulled. The SQLite file is the one asset that may be absent: with no local copy yet the puller logs `asn-openipdata.sqlite3 is not published yet` and the run counts as successful; once a local copy exists, an absent asset is a failure like the others.
- Cloudflare: `cf-ips` refuses an empty list or an invalid CIDR, writes through `.tmp` plus rename, and restores the old file when `nginx -t` fails. nginx answers 444 to any peer that is not a Cloudflare range, and takes the visitor address from `CF-Connecting-IP`, so `$remote_addr` and the access log (`/var/log/nginx/openipdata.org.access.log`) hold the visitor. `/healthz` is a 404 through nginx; the updater asks the instance port directly.
- Firewall: ufw allows the ports sshd really listens on (`sshd -T`; with none found it falls back to the OpenSSH profile or 22/tcp and warns), 80/tcp and 443/tcp, denies the rest of incoming.
- nginx default-deny: `default-deny.conf` (catch-all servers: 444 on port 80, rejected TLS handshake on 443) needs no certificate and is always enabled, so the origin is closed even before the certificate exists.
- Pings: set `HC_PING_SITE` and `HC_PING_MMDB` (healthchecks.io URLs) in the env file. A normal run pings the URL, a failure pings `<url>/fail`, an empty value does nothing. The URLs are never logged.

Settings (`/etc/openipdata-site/openipdata-site.env`): `GITHUB_REPO` (default `shingrus/openipdata.org`), `HC_PING_SITE`, `HC_PING_MMDB`. The site itself is configured in its unit (`LISTEN=127.0.0.1:<port>`, `GEO_MMDB`, `ASN_MMDB`, `ASN_SQLITE`, `REAL_IP_HEADER=X-Real-IP`, `V4_URL`, `V6_URL`).

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

Builds on push to `master`, `iplistener` on this VM (the access log it would tail is already written).
