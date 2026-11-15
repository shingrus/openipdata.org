# beacon

A small daemon for the web VM (`openipdata-beacon`). It lets the website show a visitor which DNS
resolver they use.

How it works: the page makes the browser look up a one-time name `<token>.r.openipdata.org`
(`<token>` = 32 lower-case hex characters). The zone `r.openipdata.org` is served by Knot DNS on
this VM. Knot sends every query to this daemon through dnstap (a unix socket, Knot connects as the
client). For a query of exactly `<token>.<zone>` the daemon writes the address of the resolver that
asked into Redis. The website reads it. Setup of Knot, Redis, the unit and the zone is in
`infra/site/` (`init-openipdata-site.sh`).

## What it stores

Redis hash `beacon:<token>` with ONE field `resolver_ip` (canonical text, IPv4-mapped IPv6 shown as
IPv4). Written with `HSETNX`, so the first query wins, and `EXPIRE` is set only when the key was
created (one atomic Lua script), so repeated queries cannot extend the life. Default life 120
seconds. Nothing else is stored: no EDNS client subnet, no query type, no name.

Ignored: any message that is not an authoritative query (`AUTH_QUERY`), a first label that is not
32 hex characters (upper case is folded: resolvers may randomise case), a name with more labels
between the token and the zone, another zone, garbage frames.

Nothing about a visitor is logged: no address, token or name. The log has the start settings, a
Redis warning at most once a minute, and every 10 minutes counts (written, dropped because the
queue was full, dropped because Redis failed). Redis down never stops the daemon: the write is
dropped and counted.

## Settings

Environment variables (set in the unit):

| Variable | Default |
|---|---|
| `DNSTAP_SOCKET` | `/run/openipdata-beacon/dnstap.sock` |
| `BEACON_ZONE` | `r.openipdata.org` |
| `REDIS_ADDR` | `127.0.0.1:6379` (no password, loopback only) |
| `BEACON_TTL` | `120` (seconds, 1 to 86400) |

`openipdata-beacon -version` prints the version stamped with `-X main.version` and exits.

## Socket permissions

The unit runs as the user `openipdata-beacon` with the group `knot`, and has
`RuntimeDirectory=openipdata-beacon` with mode `0750`: the directory belongs to that user and the
group `knot`. The daemon removes a stale socket at start and makes the new one `0660`. So only the
beacon user and the `knot` group (Knot) can reach it. Stop (SIGTERM) removes the socket.

## Build and test

    cd beacon
    gofmt -l . && go vet ./... && go test -race ./...
    CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=dev" -o /tmp/openipdata-beacon .

The release binary is built by `.github/workflows/site.yml` and delivered by
`openipdata-site-update` (asset `openipdata-beacon-linux-amd64` of the release `site`).

## Testing by hand

On the VM, after the init script and the DNS records:

    dig @<public ipv4> 0123456789abcdef0123456789abcdef.r.openipdata.org
    redis-cli hgetall beacon:0123456789abcdef0123456789abcdef   # resolver_ip = the address asking us
    redis-cli ttl beacon:0123456789abcdef0123456789abcdef       # 120 or less

(`dig @<ip>` asks our server directly from your own address, so the stored address is yours.)
Without Knot, a manual check needs a dnstap client; the unit tests cover the frame handling.
