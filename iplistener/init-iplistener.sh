#!/usr/bin/env bash
# Build and install iplistener on a web server (Ubuntu/Debian, amd64 or arm64).
# Run from a git clone: cd openipdata.org/iplistener && sudo ./init-iplistener.sh
# Idempotent: re-run after `git pull` to redeploy; never overwrites an existing env file.
set -euo pipefail

RUN_AS=iplistener
BIN_DIR=/opt/iplistener/bin
ETC_DIR=/etc/iplistener
ENV_FILE=$ETC_DIR/iplistener.env
SERVICE_NAME=iplistener

# Go toolchain used only for the build, with its module/build caches; safe to delete.
CACHE_DIR=/var/cache/iplistener-build
GO_VERSION="${GO_VERSION:-1.27.3}"
# Checksum of the pinned tarball for this machine; set GO_SHA256 when you change GO_VERSION.
GO_SHA256_AMD64="${GO_SHA256_AMD64:-65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40}"
GO_SHA256_ARM64="${GO_SHA256_ARM64:-a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49}"

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
case "$(uname -m)" in
    x86_64) GO_ARCH=amd64; GO_SHA256="${GO_SHA256:-$GO_SHA256_AMD64}" ;;
    aarch64 | arm64) GO_ARCH=arm64; GO_SHA256="${GO_SHA256:-$GO_SHA256_ARM64}" ;;
    *) fail "unsupported architecture $(uname -m)" ;;
esac
[[ -n "$GO_SHA256" ]] || fail "no pinned sha256 for Go $GO_VERSION linux-$GO_ARCH; set GO_SHA256 (from https://go.dev/dl/)"

if ! command -v curl >/dev/null 2>&1; then
    log "installing curl"
    DEBIAN_FRONTEND=noninteractive apt-get update -q
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends ca-certificates curl
fi

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
GO_ROOT="$CACHE_DIR/go$GO_VERSION"
install -d -m 0700 -o root -g root "$CACHE_DIR"
if [[ -x "$GO_ROOT/bin/go" ]]; then
    log "using cached Go $GO_VERSION"
else
    log "downloading Go $GO_VERSION ($GO_ARCH)"
    curl -fsSL --retry 3 -o "$BUILD_TMP/go.tar.gz" "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
    echo "$GO_SHA256  $BUILD_TMP/go.tar.gz" | sha256sum --check --status || fail "Go download checksum mismatch"
    rm -rf "$CACHE_DIR/go.partial"
    mkdir "$CACHE_DIR/go.partial"
    tar -C "$CACHE_DIR/go.partial" -xzf "$BUILD_TMP/go.tar.gz"
    mv "$CACHE_DIR/go.partial/go" "$GO_ROOT"
    rm -rf "$CACHE_DIR/go.partial"
fi
for old in "$CACHE_DIR"/go1.*; do
    if [[ -d "$old" && "$old" != "$GO_ROOT" ]]; then
        log "removing old toolchain $old"
        rm -rf "$old"
    fi
done

log "building iplistener from $SCRIPT_DIR"
(
    cd "$SCRIPT_DIR"
    GOTOOLCHAIN=local GOPATH="$CACHE_DIR/gopath" GOCACHE="$CACHE_DIR/cache" CGO_ENABLED=0 \
        "$GO_ROOT/bin/go" build -buildvcs=false -trimpath -ldflags "-s -w" -o "$BUILD_TMP/iplistener" .
)
install -m 0755 -o root -g root "$BUILD_TMP/iplistener" "$BIN_DIR/iplistener"
log "installed $BIN_DIR/iplistener"

if [[ -f "$ENV_FILE" ]]; then
    log "keeping existing $ENV_FILE"
else
    log "installing env template: $ENV_FILE"
    install -m 0640 -o root -g "$RUN_AS" "$SCRIPT_DIR/iplistener.env.example" "$ENV_FILE"
fi

log "installing systemd unit: $SERVICE_NAME"
install -m 0644 -o root -g root "$SCRIPT_DIR/$SERVICE_NAME.service" "/etc/systemd/system/$SERVICE_NAME.service"
systemctl daemon-reload
systemctl enable "$SERVICE_NAME" >/dev/null

missing=()
for name in PGSQL LOG_FILE FILTER SOURCE; do
    grep -Eq "^${name}=([^'\"]|'[^']|\"[^\"])" "$ENV_FILE" || missing+=("$name")
done
if ((${#missing[@]} == 0)); then
    log "restarting $SERVICE_NAME"
    systemctl restart "$SERVICE_NAME"
    log "check: journalctl -u $SERVICE_NAME -f"
else
    log "empty in $ENV_FILE: ${missing[*]}; service enabled but not started"
    log "fill them in: sudoedit $ENV_FILE, then: systemctl restart $SERVICE_NAME && journalctl -u $SERVICE_NAME -f"
fi
log "provisioning complete"
