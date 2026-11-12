#!/usr/bin/env bash
# Prepare an Ubuntu (x86_64) VM to run openipdata-worker, and build and install
# openipdata-worker and mmdbbuilder from this checkout, and scamper from a pinned upstream tarball.
# Run from a checkout of this repo: sudo bash infra/init-hatchet-runner-vm.sh
# Idempotent: re-run after `git pull` to redeploy; never overwrites an existing env file.
set -euo pipefail

RUN_AS=openipdata
BIN_DIR=/opt/openipdata/bin
DATA_DIR=/var/lib/openipdata
ETC_DIR=/etc/openipdata
ENV_FILE=$ETC_DIR/worker.env
SERVICE_NAME=openipdata-worker

# Go toolchain used only for the build. It and the module/build caches are kept in
# CACHE_DIR so redeploys only recompile what changed; delete CACHE_DIR to start clean.
CACHE_DIR=/var/cache/openipdata-build
GO_VERSION="${GO_VERSION:-1.27.3}"
GO_SHA256="${GO_SHA256:-65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40}"

# scamper (the resolve-ips traceroutes) is built from the upstream tarball without privilege
# separation: Ubuntu's setuid package calls mkdir /var/empty, chroot and setuid at start, which
# the unit (non-root, NoNewPrivileges, CAP_NET_RAW only) forbids. Rebuilt only when
# /opt/openipdata/bin/scamper does not report SCAMPER_VERSION.
SCAMPER_VERSION="${SCAMPER_VERSION:-20261019}"
SCAMPER_SHA256="${SCAMPER_SHA256:-83fedacbea65c03d8ef754e0240d4dbfc2efb0cf3f62a5980d0d213c807c7698}"

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
# build-essential: only to build scamper below.
apt-get install -y -q --no-install-recommends ca-certificates curl git build-essential
# The apt scamper is setuid root and cannot start under the unit; keep it off the PATH.
if [[ "$(dpkg-query -W -f='${Status}' scamper 2>/dev/null)" == "install ok installed" ]]; then
    log "removing the apt package scamper"
    apt-get remove -y -q scamper
fi

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
GO_ROOT="$CACHE_DIR/go$GO_VERSION"
install -d -m 0700 -o root -g root "$CACHE_DIR"
if [[ -x "$GO_ROOT/bin/go" ]]; then
    log "using cached Go $GO_VERSION"
else
    log "downloading Go $GO_VERSION"
    curl -fsSL --retry 3 -o "$BUILD_TMP/go.tar.gz" "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
    echo "$GO_SHA256  $BUILD_TMP/go.tar.gz" | sha256sum --check --status || fail "Go download checksum mismatch"
    # Extract next to GO_ROOT so the final rename is atomic: no half-installed toolchain.
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

build() { # build <module dir> <binary name>
    log "building $2 from $REPO_DIR/$1"
    (
        cd "$REPO_DIR/$1"
        GOTOOLCHAIN=local GOPATH="$CACHE_DIR/gopath" GOCACHE="$CACHE_DIR/cache" CGO_ENABLED=0 \
            "$GO_ROOT/bin/go" build -buildvcs=false -trimpath -ldflags "-s -w" -o "$BUILD_TMP/$2" .
    )
    install -m 0755 -o root -g root "$BUILD_TMP/$2" "$BIN_DIR/$2"
    log "installed $BIN_DIR/$2"
}
build hatchet openipdata-worker
build mmdbbuilder mmdbbuilder

# scamper installed here, not on the PATH: the worker runs the one next to itself.
if [[ -x "$BIN_DIR/scamper" ]] && [[ "$("$BIN_DIR/scamper" -v 2>&1)" == "scamper version $SCAMPER_VERSION" ]]; then
    log "using installed scamper $SCAMPER_VERSION"
else
    log "downloading scamper $SCAMPER_VERSION"
    curl -fsSL --retry 3 -o "$BUILD_TMP/scamper.tar.gz" \
        "https://www.caida.org/catalog/software/scamper/code/scamper-cvs-${SCAMPER_VERSION}.tar.gz"
    echo "$SCAMPER_SHA256  $BUILD_TMP/scamper.tar.gz" | sha256sum --check --status || fail "scamper download checksum mismatch"
    SCAMPER_SRC="$CACHE_DIR/scamper-src"
    SCAMPER_PREFIX="$CACHE_DIR/scamper-$SCAMPER_VERSION"
    rm -rf "$SCAMPER_SRC" "$SCAMPER_PREFIX"
    mkdir "$SCAMPER_SRC"
    tar -C "$SCAMPER_SRC" -xzf "$BUILD_TMP/scamper.tar.gz"
    log "building scamper $SCAMPER_VERSION (no privilege separation)"
    (
        cd "$SCAMPER_SRC/scamper-cvs-$SCAMPER_VERSION"
        ./configure --quiet --disable-scamper-privsep --prefix="$SCAMPER_PREFIX"
        make -j"$(nproc)" >/dev/null
        make install >/dev/null
    )
    install -m 0755 -o root -g root "$SCAMPER_PREFIX/bin/scamper" "$BIN_DIR/scamper"
    rm -rf "$SCAMPER_SRC" "$SCAMPER_PREFIX"
    [[ "$("$BIN_DIR/scamper" -v 2>&1)" == "scamper version $SCAMPER_VERSION" ]] || fail "installed scamper does not report version $SCAMPER_VERSION"
    log "installed $BIN_DIR/scamper"
fi

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

# One trace under the unit's own limits: scamper exits 0 even when it cannot open its raw socket,
# so only a trace with a hop proves it works. The -p properties must match the sandbox of the unit.
log "smoke test: scamper trace under the unit's limits"
if ! SMOKE_OUT="$(systemd-run --wait --pipe --quiet --collect \
    -p User="$RUN_AS" -p Group="$RUN_AS" -p NoNewPrivileges=true \
    -p AmbientCapabilities=CAP_NET_RAW -p CapabilityBoundingSet=CAP_NET_RAW \
    -p ProtectSystem=strict -p ProtectHome=true -p PrivateTmp=true -p ReadWritePaths="$DATA_DIR" \
    -p RuntimeMaxSec=120 \
    "$BIN_DIR/scamper" -O json -c "trace -P icmp-paris" -i 1.1.1.1 2>&1)"; then
    fail "scamper smoke test did not run: $(printf '%s' "$SMOKE_OUT" | tail -n 3)"
fi
if ! grep -Eq '"type":"trace".*"hops":\[' <<<"$SMOKE_OUT"; then
    fail "scamper smoke test: no trace with a hop in the output: $(printf '%s' "$SMOKE_OUT" | tail -n 3 | head -c 500)"
fi
log "smoke test passed: scamper traced 1.1.1.1 as $RUN_AS"

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
