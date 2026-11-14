#!/usr/bin/env bash
# Provision one Ubuntu 24.04 VM to serve openipdata.org behind Cloudflare: nginx, the site binary
# (from the GitHub release "site"), the MMDB files (release "latest"), updater timers, firewall.
# Run from a git clone: cd openipdata.org/infra/site && sudo ./init-openipdata-site.sh
# Idempotent: rerun after `git pull` to update scripts, units and the nginx file; never overwrites an
# existing env file. It never obtains the TLS certificate (certbot, Cloudflare DNS challenge, by hand):
# without it everything else is installed, the nginx site stays disabled and the command is printed.
set -euo pipefail

RUN_AS=openipdata-site
OPT_DIR=/opt/openipdata-site
BIN_DIR=$OPT_DIR/bin
DATA_DIR=/var/lib/openipdata-site/data
STATE_DIR=/var/lib/openipdata-site/state
ETC_DIR=/etc/openipdata-site
ENV_FILE=$ETC_DIR/openipdata-site.env
DOMAIN=openipdata.org
CERT_DIR=/etc/letsencrypt/live/$DOMAIN
CF_CREDENTIALS=/root/.secrets/cloudflare.ini
NGINX_SITE=/etc/nginx/sites-available/$DOMAIN.conf
NGINX_LINK=/etc/nginx/sites-enabled/$DOMAIN.conf
NGINX_DENY=/etc/nginx/sites-available/default-deny.conf
NGINX_DENY_LINK=/etc/nginx/sites-enabled/default-deny.conf
SCRIPTS=(openipdata-site-update openipdata-mmdb-pull openipdata-cf-ips)
UNITS=(
    openipdata-site.service
    openipdata-site-update.service openipdata-site-update.timer
    openipdata-mmdb-pull.service openipdata-mmdb-pull.timer
    openipdata-cf-ips.service openipdata-cf-ips.timer
)
TIMERS=(openipdata-site-update.timer openipdata-mmdb-pull.timer openipdata-cf-ips.timer)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log() {
    printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"
}

fail() {
    log "error: $*"
    exit 1
}

[[ "${EUID}" -eq 0 ]] || fail "run this script as root"
command -v systemctl >/dev/null 2>&1 || fail "systemd not found"
command -v apt-get >/dev/null 2>&1 || fail "apt-get not found (Ubuntu or Debian only)"
for f in "${SCRIPTS[@]}" "${UNITS[@]/#/systemd/}" nginx/openipdata.org.conf nginx/default-deny.conf openipdata-site.env.example; do
    [[ -f "$SCRIPT_DIR/$f" ]] || fail "missing $SCRIPT_DIR/$f (run from a full git clone)"
done

log "installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq nginx curl jq ufw certbot python3-certbot-dns-cloudflare ca-certificates >/dev/null

if ! getent group "$RUN_AS" >/dev/null 2>&1; then
    log "creating group: $RUN_AS"
    groupadd --system "$RUN_AS"
fi
if ! id -u "$RUN_AS" >/dev/null 2>&1; then
    log "creating user: $RUN_AS"
    useradd --system --gid "$RUN_AS" --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$RUN_AS"
fi

install -d -m 0755 -o root -g root "$OPT_DIR" "$BIN_DIR" "$OPT_DIR/releases" /var/lib/openipdata-site
install -d -m 0755 -o "$RUN_AS" -g "$RUN_AS" "$DATA_DIR"
install -d -m 0700 -o root -g root "$STATE_DIR"
install -d -m 0750 -o root -g "$RUN_AS" "$ETC_DIR"

if [[ -f "$ENV_FILE" ]]; then
    log "keeping existing $ENV_FILE"
else
    log "installing env template: $ENV_FILE"
    install -m 0640 -o root -g "$RUN_AS" "$SCRIPT_DIR/openipdata-site.env.example" "$ENV_FILE"
fi

for s in "${SCRIPTS[@]}"; do
    install -m 0755 -o root -g root "$SCRIPT_DIR/$s" "$BIN_DIR/$s"
done
log "installed scripts in $BIN_DIR"

for u in "${UNITS[@]}"; do
    install -m 0644 -o root -g root "$SCRIPT_DIR/systemd/$u" "/etc/systemd/system/$u"
done
systemctl daemon-reload
log "installed systemd units"

# Firewall: SSH and web only; Cloudflare-only access is enforced by nginx.
# The real sshd ports are allowed (sshd may not listen on 22); with none found, OpenSSH or 22/tcp.
ssh_ports=()
while IFS= read -r p; do
    [[ "$p" =~ ^[0-9]+$ ]] && ssh_ports+=("$p")
done < <(sshd -T 2>/dev/null | awk '$1=="port"{print $2}' | sort -un)
if ((${#ssh_ports[@]})); then
    for p in "${ssh_ports[@]}"; do
        ufw allow "$p/tcp" >/dev/null
    done
    log "ufw: SSH ports allowed: ${ssh_ports[*]}"
else
    log "warning: could not read the sshd ports (sshd -T); allowing the OpenSSH profile or 22/tcp"
    if ufw app info OpenSSH >/dev/null 2>&1; then
        ufw allow OpenSSH >/dev/null
    else
        ufw allow 22/tcp >/dev/null
    fi
fi
ufw allow 80/tcp >/dev/null
ufw allow 443/tcp >/dev/null
ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null
ufw --force enable >/dev/null
log "ufw: SSH, 80/tcp, 443/tcp allowed; other incoming denied"

# nginx: our files, no Ubuntu default site, the default-deny servers (always on, no certificate needed,
# so the origin is closed before the certificate exists), the Cloudflare ranges, then the site itself
# if the certificate exists.
install -m 0644 -o root -g root "$SCRIPT_DIR/nginx/openipdata.org.conf" "$NGINX_SITE"
install -m 0644 -o root -g root "$SCRIPT_DIR/nginx/default-deny.conf" "$NGINX_DENY"
rm -f /etc/nginx/sites-enabled/default
ln -sfn "$NGINX_DENY" "$NGINX_DENY_LINK"
"$BIN_DIR/openipdata-cf-ips"

# certbot's own renewal timer comes with the package; the hook reloads nginx after a renewal.
install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy
cat >/etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh.tmp <<'HOOK'
#!/bin/sh
nginx -t && systemctl reload nginx
HOOK
chmod 0755 /etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh.tmp
mv -f /etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh.tmp /etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh
systemctl enable --now certbot.timer >/dev/null 2>&1 || log "warning: certbot.timer could not be enabled"

have_cert=0
if [[ -e "$CERT_DIR/fullchain.pem" && -e "$CERT_DIR/privkey.pem" ]]; then
    have_cert=1
    ln -sfn "$NGINX_SITE" "$NGINX_LINK"
    nginx -t || fail "nginx -t failed; see above"
    systemctl enable nginx >/dev/null 2>&1 || true
    if systemctl is-active --quiet nginx; then
        systemctl reload nginx
    else
        systemctl start nginx
    fi
    log "nginx site enabled"
else
    rm -f "$NGINX_LINK"
    nginx -t || fail "nginx -t failed; see above"
    systemctl enable nginx >/dev/null 2>&1 || true
    if systemctl is-active --quiet nginx; then
        systemctl reload nginx
    else
        systemctl start nginx
    fi
    log "no certificate in $CERT_DIR yet: the nginx site stays disabled (default-deny is active)"
fi

# Data and binary. Each unit is a oneshot: start waits until it has finished.
systemctl enable openipdata-site.service >/dev/null 2>&1
log "pulling the MMDB files"
systemctl start openipdata-mmdb-pull.service || log "warning: the MMDB pull failed: journalctl -u openipdata-mmdb-pull"
log "installing the site binary"
systemctl start openipdata-site-update.service || log "warning: the binary update failed: journalctl -u openipdata-site-update"
if [[ ! -e "$OPT_DIR/current/openipdata-site" ]]; then
    log "no site binary installed: the GitHub release 'site' does not exist yet or the build was rejected."
    log "run the 'Publish site binary' workflow (Actions tab, Run workflow, on master) first; the timer installs it within 2 minutes."
fi

systemctl enable --now "${TIMERS[@]}" >/dev/null

# Status.
log "status"
if [[ -x "$OPT_DIR/current/openipdata-site" ]]; then
    printf 'binary version: %s\n' "$(setpriv --reuid="$RUN_AS" --regid="$RUN_AS" --clear-groups "$OPT_DIR/current/openipdata-site" -version 2>&1 || true)"
    printf 'healthz: %s\n' "$(curl -fsS -m 3 http://127.0.0.1:8080/healthz 2>&1 || echo 'not answering')"
else
    echo "binary version: not installed"
fi
echo "MMDB files:"
shopt -s nullglob
mmdbs=("$DATA_DIR"/*.mmdb)
if ((${#mmdbs[@]})); then ls -l "${mmdbs[@]}"; else echo "  none yet"; fi
echo "timers:"
systemctl list-timers --no-pager 'openipdata-*' || true

if ((!have_cert)); then
    cat <<MSG

The TLS certificate is missing, so the nginx site is not enabled yet. Do this once:

1. Create a Cloudflare API token (My Profile, API Tokens) with the permission Zone:DNS:Edit
   for the zone $DOMAIN, and put it in $CF_CREDENTIALS (mode 0600):

       install -d -m 0700 /root/.secrets
       printf 'dns_cloudflare_api_token = <the token>\n' > $CF_CREDENTIALS
       chmod 0600 $CF_CREDENTIALS

2. Get the certificate:

       certbot certonly --dns-cloudflare --dns-cloudflare-credentials $CF_CREDENTIALS -d $DOMAIN -d www.$DOMAIN

3. Rerun this script:  sudo $SCRIPT_DIR/init-openipdata-site.sh
MSG
fi
log "provisioning complete"
