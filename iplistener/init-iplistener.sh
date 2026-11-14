#!/usr/bin/env bash
# Build and install iplistener on a web server (Ubuntu/Debian). Needs Go 1.21+ installed (e.g. apt install golang-go).
# Run from a git clone: cd openipdata.org/iplistener && sudo ./init-iplistener.sh
# Idempotent: re-run after `git pull` to redeploy; never overwrites an existing env file.
set -euo pipefail

RUN_AS=iplistener
BIN_DIR=/opt/iplistener/bin
ETC_DIR=/etc/iplistener
ENV_FILE=$ETC_DIR/iplistener.env
SERVICE_NAME=iplistener

# Module and build caches of the Go build; safe to delete.
CACHE_DIR=/var/cache/iplistener-build

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_TMP=""
trap '[[ -n "$BUILD_TMP" ]] && rm -rf "$BUILD_TMP"' EXIT

log() {
    printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"
}

fail() {
    log "error: $*"
    exit 1
}

[[ "${EUID}" -eq 0 ]] || fail "run this script as root"
command -v systemctl >/dev/null 2>&1 || fail "systemd not found"
# Go must already be installed (1.21 or newer); this script never downloads it. When go.mod asks
# for a newer Go, the installed go fetches that toolchain itself (checksummed, into CACHE_DIR).
GO_BIN="$(command -v go || true)"
[[ -z "$GO_BIN" && -x /usr/local/go/bin/go ]] && GO_BIN=/usr/local/go/bin/go
GO_HINT="install Go first, e.g.: sudo apt install golang-go (then rerun this script)"
[[ -n "$GO_BIN" ]] || fail "Go is not installed; $GO_HINT"
# GOTOOLCHAIN=local: the version of the installed go itself, not of a toolchain it would switch to.
GO_HAVE="$(cd / && GOTOOLCHAIN=local "$GO_BIN" env GOVERSION 2>/dev/null | sed 's/^go//')"
GO_MIN=1.21
[[ -n "$GO_HAVE" ]] || fail "cannot read the Go version; $GO_HINT"
[[ "$(printf '%s\n%s\n' "$GO_MIN" "$GO_HAVE" | sort -V | head -n1)" == "$GO_MIN" ]] ||
    fail "Go $GO_HAVE is older than $GO_MIN; $GO_HINT"
GO_NEED="$(awk '$1 == "go" {print $2; exit}' "$SCRIPT_DIR/go.mod")"
log "using $GO_BIN (Go $GO_HAVE; go.mod needs $GO_NEED, fetched by go itself if newer)"

if ! getent group "$RUN_AS" >/dev/null 2>&1; then
    log "creating group: $RUN_AS"
    groupadd --system "$RUN_AS"
fi
if ! id -u "$RUN_AS" >/dev/null 2>&1; then
    log "creating user: $RUN_AS"
    useradd --system --gid "$RUN_AS" --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$RUN_AS"
fi
# adm can read the nginx logs (Debian/Ubuntu: root:adm 0640, or www-data:adm).
getent group adm >/dev/null 2>&1 || fail "group adm does not exist; nginx logs are not readable that way here"
if ! id -nG "$RUN_AS" | tr ' ' '\n' | grep -qx adm; then
    log "adding $RUN_AS to group adm"
    usermod -aG adm "$RUN_AS"
fi

install -d -m 0755 -o root -g root "$BIN_DIR"
install -d -m 0750 -o root -g "$RUN_AS" "$ETC_DIR"

BUILD_TMP="$(mktemp -d)"
install -d -m 0700 -o root -g root "$CACHE_DIR"
log "building iplistener from $SCRIPT_DIR"
(
    cd "$SCRIPT_DIR"
    GOTOOLCHAIN=auto GOPATH="$CACHE_DIR/gopath" GOMODCACHE="$CACHE_DIR/gopath/pkg/mod" GOCACHE="$CACHE_DIR/cache" CGO_ENABLED=0 \
        "$GO_BIN" build -buildvcs=false -trimpath -ldflags "-s -w" -o "$BUILD_TMP/iplistener" .
)
install -m 0755 -o root -g root "$BUILD_TMP/iplistener" "$BIN_DIR/iplistener"
log "installed $BIN_DIR/iplistener"

new_env=0
if [[ -f "$ENV_FILE" ]]; then
    log "keeping existing $ENV_FILE"
else
    log "installing env template: $ENV_FILE"
    install -m 0640 -o root -g "$RUN_AS" "$SCRIPT_DIR/iplistener.env.example" "$ENV_FILE"
    new_env=1
fi

# is_set NAME: the env file has a non-empty value for NAME.
is_set() {
    grep -Eq "^${1}=([^'\"]|'[^']|\"[^\"])" "$ENV_FILE"
}

# set_value NAME VALUE: writes NAME='VALUE' (single quotes: systemd keeps \ and " literally).
set_value() {
    local tmp
    tmp="$(mktemp "$ETC_DIR/.env.XXXXXX")"
    NAME="$1" VAL="'$2'" awk '
        $0 ~ "^" ENVIRON["NAME"] "=" { print ENVIRON["NAME"] "=" ENVIRON["VAL"]; done = 1; next }
        { print }
        END { if (!done) print ENVIRON["NAME"] "=" ENVIRON["VAL"] }' "$ENV_FILE" >"$tmp"
    chown root:"$RUN_AS" "$tmp"
    chmod 0640 "$tmp"
    mv "$tmp" "$ENV_FILE"
}

# ask NAME PROMPT [DEFAULT] [secret]: asks on the terminal until the answer has no single quote.
ask() {
    local answer
    while true; do
        if [[ "${4:-}" == secret ]]; then
            read -rs -p "$2: " answer </dev/tty
            echo >/dev/tty
        else
            read -r -p "$2${3:+ [$3]}: " answer </dev/tty
        fi
        answer="${answer:-${3:-}}"
        if [[ "$answer" == *"'"* ]]; then
            echo "a value cannot contain a single quote (') here, try again" >/dev/tty
            continue
        fi
        [[ -n "$answer" || "$1" == EXCLUDE ]] && break
    done
    [[ -n "$answer" ]] && set_value "$1" "$answer"
    return 0
}

# Run in a terminal, ask for every empty setting, so one command installs and starts the service.
if [[ -t 0 ]]; then
    is_set PGSQL || ask PGSQL "Postgres connection string of the iplistener role (hidden)" "" secret
    is_set LOG_FILE || ask LOG_FILE "nginx access log" "/var/log/nginx/access.log"
    is_set FILTER || ask FILTER "regexp of the lines to keep, e.g. \"POST /api/\\w+ or \"GET /api/beacon"
    ((new_env)) && ask EXCLUDE "regexp of the lines to drop (optional)" "cc=T1|bot|crawl|spider"
    is_set SOURCE || ask SOURCE "short label for visitor_ips.source" "$(hostname -s)"
fi
if is_set LOG_FILE; then
    log_file="$(sed -n "s/^LOG_FILE='\{0,1\}\([^']*\)'\{0,1\}$/\1/p" "$ENV_FILE" | head -n1)"
    [[ -f "$log_file" ]] || log "warning: $log_file does not exist (yet)"
fi

log "installing systemd unit: $SERVICE_NAME"
install -m 0644 -o root -g root "$SCRIPT_DIR/$SERVICE_NAME.service" "/etc/systemd/system/$SERVICE_NAME.service"
systemctl daemon-reload
systemctl enable "$SERVICE_NAME" >/dev/null

missing=()
for name in PGSQL LOG_FILE FILTER SOURCE; do
    is_set "$name" || missing+=("$name")
done
if ((${#missing[@]} == 0)); then
    log "restarting $SERVICE_NAME"
    systemctl restart "$SERVICE_NAME"
    sleep 3
    if systemctl is-active --quiet "$SERVICE_NAME"; then
        log "$SERVICE_NAME is running; its log: journalctl -u $SERVICE_NAME -f"
    else
        journalctl -u "$SERVICE_NAME" -n 20 --no-pager || true
        fail "$SERVICE_NAME did not start; fix $ENV_FILE (sudoedit) and rerun this script"
    fi
else
    log "empty in $ENV_FILE: ${missing[*]}; service enabled but not started"
    log "fill them in: sudoedit $ENV_FILE, then: systemctl restart $SERVICE_NAME && journalctl -u $SERVICE_NAME -f"
fi
log "provisioning complete"
