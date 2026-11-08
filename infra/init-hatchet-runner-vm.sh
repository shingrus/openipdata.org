#!/usr/bin/env bash
# Prepare an Ubuntu (x86_64) VM to run openipdata-worker, and build and install
# openipdata-worker and mmdbbuilder from this checkout.
# Run from a checkout of this repo: sudo bash infra/init-hatchet-runner-vm.sh
# Idempotent: re-run after `git pull` to redeploy; never overwrites an existing env file.
set -euo pipefail

RUN_AS=openipdata
BIN_DIR=/opt/openipdata/bin
DATA_DIR=/var/lib/openipdata
ETC_DIR=/etc/openipdata
ENV_FILE=$ETC_DIR/worker.env
SERVICE_NAME=openipdata-worker

# Go toolchain used only for the build, in a temp dir that is removed afterwards.
GO_VERSION="${GO_VERSION:-1.27.3}"
GO_SHA256="${GO_SHA256:-65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
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
command -v apt-get >/dev/null 2>&1 || fail "apt-get not found; this script targets Ubuntu/Debian"
[[ "$(uname -m)" == "x86_64" ]] || fail "unsupported architecture $(uname -m); x86_64 required"

log "installing OS packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
# git: the worker clones source repos into the data dir. curl: Go download.
apt-get install -y -q --no-install-recommends ca-certificates curl git

if ! getent group "$RUN_AS" >/dev/null 2>&1; then
    log "creating group: $RUN_AS"
    groupadd --system "$RUN_AS"
fi
if ! id -u "$RUN_AS" >/dev/null 2>&1; then
    log "creating user: $RUN_AS"
    useradd --system --gid "$RUN_AS" --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$RUN_AS"
fi

log "creating directories"
install -d -m 0755 -o root -g root "$BIN_DIR"
install -d -m 0750 -o root -g "$RUN_AS" "$ETC_DIR"
# Worker state (source clones, build outputs); the worker creates its subdirectories.
install -d -m 0750 -o "$RUN_AS" -g "$RUN_AS" "$DATA_DIR"

BUILD_TMP="$(mktemp -d)"
log "downloading Go $GO_VERSION"
curl -fsSL --retry 3 -o "$BUILD_TMP/go.tar.gz" "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
echo "$GO_SHA256  $BUILD_TMP/go.tar.gz" | sha256sum --check --status || fail "Go download checksum mismatch"
tar -C "$BUILD_TMP" -xzf "$BUILD_TMP/go.tar.gz"

build() { # build <module dir> <binary name>
    log "building $2 from $REPO_DIR/$1"
    (
        cd "$REPO_DIR/$1"
        GOTOOLCHAIN=local GOPATH="$BUILD_TMP/gopath" GOCACHE="$BUILD_TMP/cache" CGO_ENABLED=0 \
            "$BUILD_TMP/go/bin/go" build -buildvcs=false -trimpath -ldflags "-s -w" -o "$BUILD_TMP/$2" .
    )
    install -m 0755 -o root -g root "$BUILD_TMP/$2" "$BIN_DIR/$2"
    log "installed $BIN_DIR/$2"
}
build hatchet openipdata-worker
build mmdbbuilder mmdbbuilder

if [[ -f "$ENV_FILE" ]]; then
    log "keeping existing $ENV_FILE"
else
    log "installing env template: $ENV_FILE"
    install -m 0640 -o root -g "$RUN_AS" "$SCRIPT_DIR/$SERVICE_NAME.env.example" "$ENV_FILE"
fi

log "installing systemd unit: $SERVICE_NAME"
install -m 0644 -o root -g root "$SCRIPT_DIR/$SERVICE_NAME.service" "/etc/systemd/system/$SERVICE_NAME.service"
systemctl daemon-reload
systemctl enable "$SERVICE_NAME" >/dev/null

if grep -Eq '^HATCHET_CLIENT_TOKEN=.+' "$ENV_FILE"; then
    log "restarting $SERVICE_NAME"
    systemctl restart "$SERVICE_NAME"
else
    log "HATCHET_CLIENT_TOKEN is empty in $ENV_FILE; service enabled but not started"
fi

log "provisioning complete"
log "next steps:"
log "  1. Fill in secrets: sudoedit $ENV_FILE"
log "  2. Start it: systemctl restart $SERVICE_NAME && journalctl -u $SERVICE_NAME -f"
