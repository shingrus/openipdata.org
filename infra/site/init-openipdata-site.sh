#!/usr/bin/env bash
# Provision one Ubuntu 24.04 VM to serve openipdata.org behind Cloudflare: nginx, the site binary
# (from the GitHub release "site", two instances openipdata-site@8080/@8081 that take turns),
# the data files (release "latest"), updater timers, firewall.
# Run from a git clone: cd openipdata.org/infra/site && sudo ./init-openipdata-site.sh
# Idempotent: rerun after `git pull` to update scripts, units and the nginx file; never overwrites an
# existing env file. It never obtains the TLS certificate (certbot, Cloudflare DNS challenge, by hand):
# without it everything else is installed, the nginx site stays disabled and the command is printed.
# The DNS beacon (Knot DNS, Redis, openipdata-beacon, the zone r.openipdata.org) is set up only when
# the VM's public IPv4 is known (found from v4.openipdata.org, or PUBLIC_IPV4 in the env file); see beacon_prepare and beacon_start below.
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
# The two direct names (v4. and v6.): same certificate, enabled only with it.
NGINX_V46=/etc/nginx/sites-available/v46.$DOMAIN.conf
NGINX_V46_LINK=/etc/nginx/sites-enabled/v46.$DOMAIN.conf
NGINX_DENY=/etc/nginx/sites-available/default-deny.conf
NGINX_DENY_LINK=/etc/nginx/sites-enabled/default-deny.conf
# Upstream files live outside conf.d (it includes every *.conf); the link in conf.d is the live record.
UPSTREAM_DIR=/etc/nginx/openipdata-site
LIVE_LINK=/etc/nginx/conf.d/openipdata-site-live.conf
OLD_UNIT=/etc/systemd/system/openipdata-site.service
SCRIPTS=(openipdata-site-update openipdata-mmdb-pull openipdata-cf-ips)
UNITS=(
    openipdata-site@.service
    openipdata-beacon.service
    openipdata-site-update.service openipdata-site-update.timer
    openipdata-mmdb-pull.service openipdata-mmdb-pull.timer
    openipdata-cf-ips.service openipdata-cf-ips.timer
)
TIMERS=(openipdata-site-update.timer openipdata-mmdb-pull.timer openipdata-cf-ips.timer)

# The DNS beacon.
BEACON_ZONE=r.openipdata.org
R_CERT_DIR=/etc/letsencrypt/live/$BEACON_ZONE
NGINX_R=/etc/nginx/sites-available/$BEACON_ZONE.conf
NGINX_R_LINK=/etc/nginx/sites-enabled/$BEACON_ZONE.conf
KNOT_CONF=/etc/knot/knot.conf
KNOT_KEY=/etc/knot/openipdata-acme-key.conf
KNOT_ZONE=/var/lib/knot/zones/$BEACON_ZONE.zone
ACME_INI=/etc/letsencrypt/openipdata-r-rfc2136.ini
REDIS_CONF=/etc/redis/redis.conf
SITE_DROPIN_DIR=/etc/systemd/system/openipdata-site@.service.d
BEACON_RUN_AS=openipdata-beacon

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log() {
    printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"
}

# set_link TARGET LINK: atomic (temporary link, then rename over it).
set_link() {
    ln -sfn "$1" "$2.tmp"
    mv -Tf "$2.tmp" "$2"
}

# nginx_apply: nginx -t, then reload when nginx runs.
nginx_apply() {
    nginx -t >/dev/null 2>&1 || return 1
    if systemctl is-active --quiet nginx; then systemctl reload nginx || return 1; fi
    return 0
}

# Moves the site from the old single service (127.0.0.1:8080, /opt/openipdata-site/current) to the
# blue/green instances without a gap: the old service keeps 8080 until @8081 answers.
migrate_old_service() {
    [[ -f "$OLD_UNIT" ]] || return 0
    log "migrating from openipdata-site.service to openipdata-site@8081"
    local target i body ok=0
    target="$(readlink "$OPT_DIR/current" 2>/dev/null || true)"
    if [[ -n "$target" && -e "$OPT_DIR/$target/openipdata-site" ]]; then
        set_link "$target" "$OPT_DIR/slot-8081"
        systemctl start openipdata-site@8081 || true
        for ((i = 0; i < 30; i++)); do
            body="$(curl -fsS -m 2 http://127.0.0.1:8081/healthz 2>/dev/null || true)"
            if [[ "$body" == ok\ * ]]; then ok=1; break; fi
            sleep 1
        done
        if ((!ok)); then
            systemctl stop openipdata-site@8081 || true
            # A rerun may have switched the link already: give 8080 back.
            if [[ "$(readlink "$LIVE_LINK")" == */upstream-8081.conf ]]; then
                set_link "$UPSTREAM_DIR/upstream-8080.conf" "$LIVE_LINK"
                nginx_apply || true
            fi
            log "error: openipdata-site@8081 did not answer /healthz in 30 s; the old openipdata-site.service keeps running on 8080 and nothing was changed. See: journalctl -u openipdata-site@8081"
            exit 1
        fi
        set_link "$UPSTREAM_DIR/upstream-8081.conf" "$LIVE_LINK"
        if ! nginx_apply; then
            set_link "$UPSTREAM_DIR/upstream-8080.conf" "$LIVE_LINK"
            systemctl stop openipdata-site@8081 || true
            log "error: nginx -t or reload failed with the new link; link restored to 8080, the old service keeps running"
            exit 1
        fi
        systemctl enable openipdata-site@8081 >/dev/null 2>&1 || log "warning: could not enable openipdata-site@8081"
    else
        log "no current release: only removing the old service (the updater installs the build)"
    fi
    systemctl stop openipdata-site.service || true
    systemctl disable openipdata-site.service >/dev/null 2>&1 || true
    rm -f "$OLD_UNIT" "$OPT_DIR/current" "$OPT_DIR/current.tmp"
    systemctl daemon-reload
    log "migration done: the old openipdata-site.service is gone"
}

fail() {
    log "error: $*"
    exit 1
}


# env_value NAME: the last NAME=value line of the env file, without quotes and spaces.
env_value() {
    sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1 | tr -d ' \t\r"'"'"
}

beacon=0          # 1 when the public IPv4 is known: the beacon is installed
beacon_changed=0  # 1 when Knot's files changed and Knot must be restarted
beacon_unit_changed=0 # 1 when the installed openipdata-beacon.service differs from the one in the repo (restart it)
PUBLIC_IPV4=""
PUBLIC_IPV6=""

# own_addr 4|6: the address of v4.$DOMAIN or v6.$DOMAIN when it is a global address of an interface
# of this machine; nothing otherwise (a VM behind NAT needs the setting in the env file).
own_addr() {
    local a
    for a in $(getent "ahostsv$1" "v$1.$DOMAIN" 2>/dev/null | awk '{print $1}' | sort -u); do
        if ip -o "-$1" addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | grep -qxF "$a"; then
            printf '%s\n' "$a"
            return 0
        fi
    done
    return 0
}

# render_zone SERIAL: the zone file from the template (a line with an empty address is dropped).
render_zone() {
    local f="$SCRIPT_DIR/knot/$BEACON_ZONE.zone"
    sed -e "s/__SERIAL__/$1/" -e "s/__PUBLIC_IPV4__/$PUBLIC_IPV4/g" "$f" |
        if [[ -n "$PUBLIC_IPV6" ]]; then sed "s/__PUBLIC_IPV6__/$PUBLIC_IPV6/g"; else grep -v '__PUBLIC_IPV6__'; fi
}

# install_if_changed SRC DEST MODE OWNER:GROUP: copy only when the content differs; sets beacon_changed.
install_if_changed() {
    if [[ -f "$2" ]] && cmp -s "$1" "$2"; then
        chmod "$3" "$2"
        chown "$4" "$2"
        return 0
    fi
    install -m "$3" -o "${4%%:*}" -g "${4##*:}" "$1" "$2"
    beacon_changed=1
}

# Everything for the beacon that does not need its binary: packages, Redis, user, TSIG key, Knot
# configuration and zone, firewall, the nginx site for the one-time names.
beacon_prepare() {
    PUBLIC_IPV4="$(env_value PUBLIC_IPV4)"
    PUBLIC_IPV6="$(env_value PUBLIC_IPV6)"
    # Not set in the env file: the names v4.$DOMAIN and v6.$DOMAIN point straight at this VM, so their
    # addresses are used when they are really addresses of this machine.
    [[ -n "$PUBLIC_IPV4" ]] || PUBLIC_IPV4="$(own_addr 4)"
    [[ -n "$PUBLIC_IPV6" ]] || PUBLIC_IPV6="$(own_addr 6)"
    if [[ -z "$PUBLIC_IPV4" ]]; then
        log "DNS beacon skipped: v4.$DOMAIN does not resolve to an address of this machine (set PUBLIC_IPV4, and PUBLIC_IPV6 if the VM has one, in $ENV_FILE, then rerun)"
        return 0
    fi
    log "DNS beacon addresses: $PUBLIC_IPV4 ${PUBLIC_IPV6:-(no IPv6)}"
    [[ "$PUBLIC_IPV4" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || fail "PUBLIC_IPV4 in $ENV_FILE is not an IPv4 address"
    if [[ -n "$PUBLIC_IPV6" ]]; then
        [[ "$PUBLIC_IPV6" =~ ^[0-9a-fA-F:]+$ && "$PUBLIC_IPV6" == *:* ]] || fail "PUBLIC_IPV6 in $ENV_FILE is not an IPv6 address"
    fi
    beacon=1
    log "DNS beacon: installing knot, redis, the zone $BEACON_ZONE"
    # knot-module-dnstap: Ubuntu ships Knot's dnstap module as its own package (without it Knot
    # rejects the configuration: unknown module 'mod-dnstap').
    apt-get install -y -qq knot knot-module-dnstap redis-server python3-certbot-dns-rfc2136 openssl >/dev/null

    # Redis: loopback only. It holds two kinds of keys: the beacon keys of the DNS row (short lived, a
    # flood of random names must not fill the memory) and the shared reports of the website (30 days,
    # visitor addresses included; see AGENTS.md, "Share"), which must survive a restart and must not be
    # pushed out by beacon keys. So: RDB snapshots on (appendonly stays off), and a memory limit with
    # volatile-ttl, which drops the keys that expire soonest first (beacons, an hour or less, before
    # reports, 30 days). (save lines add up, so the empty save first clears the distribution's own list.)
    # A marked block at the end of the file (the last setting wins); rewritten on every run.
    local tmp
    tmp="$(mktemp)"
    sed '/^# BEGIN openipdata-beacon$/,/^# END openipdata-beacon$/d' "$REDIS_CONF" >"$tmp"
    cat >>"$tmp" <<'REDIS'
# BEGIN openipdata-beacon
bind 127.0.0.1
port 6379
protected-mode yes
save ""
save 900 1
save 300 100
appendonly no
maxmemory 256mb
maxmemory-policy volatile-ttl
# END openipdata-beacon
REDIS
    if ! cmp -s "$tmp" "$REDIS_CONF"; then
        cat "$tmp" >"$REDIS_CONF"
        systemctl restart redis-server
        log "redis: settings written (loopback only, RDB snapshots, 256mb volatile-ttl)"
    fi
    rm -f "$tmp"
    systemctl enable --now redis-server >/dev/null 2>&1 || log "warning: could not enable redis-server"

    if ! id -u "$BEACON_RUN_AS" >/dev/null 2>&1; then
        log "creating user: $BEACON_RUN_AS"
        useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$BEACON_RUN_AS"
    fi

    # The TSIG key for the certbot DNS challenge: made once, never printed, never overwritten. If one of
    # the two files is lost, both are made again (the new key is then used by both).
    if [[ ! -s "$KNOT_KEY" || ! -s "$ACME_INI" ]]; then
        local secret
        secret="$(openssl rand -base64 32)"
        (
            umask 077
            printf 'key:\n  - id: acme\n    algorithm: hmac-sha256\n    secret: %s\n' "$secret" >"$KNOT_KEY.tmp"
            printf 'dns_rfc2136_server = 127.0.0.1\ndns_rfc2136_port = 53\ndns_rfc2136_name = acme\ndns_rfc2136_secret = %s\ndns_rfc2136_algorithm = HMAC-SHA256\n' "$secret" >"$ACME_INI.tmp"
        )
        chown root:knot "$KNOT_KEY.tmp"
        chmod 0640 "$KNOT_KEY.tmp"
        chown root:root "$ACME_INI.tmp"
        chmod 0600 "$ACME_INI.tmp"
        mv -f "$KNOT_KEY.tmp" "$KNOT_KEY"
        mv -f "$ACME_INI.tmp" "$ACME_INI"
        beacon_changed=1
        log "created the TSIG key for the certificate challenge ($KNOT_KEY, $ACME_INI; not shown)"
    fi

    # Knot configuration.
    sed -e "s/__PUBLIC_IPV4__/$PUBLIC_IPV4/" "$SCRIPT_DIR/knot/knot.conf" |
        if [[ -n "$PUBLIC_IPV6" ]]; then sed "s/__PUBLIC_IPV6__/$PUBLIC_IPV6/"; else grep -v '__PUBLIC_IPV6__'; fi >"$tmp.knot"
    install_if_changed "$tmp.knot" "$KNOT_CONF" 0640 root:knot

    # The zone: the serial changes only when the content (serial aside) changes.
    install -d -m 0750 -o knot -g knot /var/lib/knot/zones
    local norm='s/ [0-9]+ (3600 600 1209600 60)$/ 0 \1/'
    render_zone 0 >"$tmp.zone0"
    if [[ -f "$KNOT_ZONE" ]] && cmp -s "$tmp.zone0" <(sed -E "$norm" "$KNOT_ZONE"); then
        log "zone $BEACON_ZONE unchanged"
    else
        render_zone "$(date -u +%s)" >"$tmp.zone"
        if command -v kzonecheck >/dev/null 2>&1; then
            kzonecheck -o "$BEACON_ZONE" "$tmp.zone" >/dev/null || fail "kzonecheck rejected the generated zone (see above)"
        fi
        install -m 0640 -o root -g knot "$tmp.zone" "$KNOT_ZONE"
        beacon_changed=1
        log "zone $BEACON_ZONE written"
    fi
    rm -f "$tmp.knot" "$tmp.zone0" "$tmp.zone"
    if command -v knotc >/dev/null 2>&1; then
        knotc -c "$KNOT_CONF" conf-check >/dev/null || fail "knotc conf-check failed for $KNOT_CONF (run it by hand to see why)"
    fi

    # Knot may read the dnstap socket only if AppArmor lets it (Ubuntu ships a profile for knotd).
    if [[ -d /etc/apparmor.d/local ]] && [[ -f /etc/apparmor.d/usr.sbin.knotd ]]; then
        local aa=/etc/apparmor.d/local/usr.sbin.knotd rule='/run/openipdata-beacon/dnstap.sock rw,'
        if ! grep -qxF "$rule" "$aa" 2>/dev/null; then
            printf '%s\n' "$rule" >>"$aa"
            apparmor_parser -r /etc/apparmor.d/usr.sbin.knotd >/dev/null 2>&1 || log "warning: could not reload the AppArmor profile of knotd"
            beacon_changed=1
        fi
    fi

    # Knot wants the beacon (and starts after it: Before= in the beacon unit). A missing beacon binary
    # only skips the beacon unit; Knot starts anyway.
    install -d -m 0755 /etc/systemd/system/knot.service.d
    printf '[Unit]\nWants=openipdata-beacon.service\n' >/etc/systemd/system/knot.service.d/openipdata-beacon.conf
    systemctl daemon-reload
    systemctl enable openipdata-beacon.service >/dev/null 2>&1 || log "warning: could not enable openipdata-beacon"

    ufw allow 53/udp >/dev/null
    ufw allow 53/tcp >/dev/null
    log "ufw: 53/udp and 53/tcp allowed"

    # The nginx site for <token>.r.openipdata.org: only with its own certificate.
    install -m 0644 -o root -g root "$SCRIPT_DIR/nginx/$BEACON_ZONE.conf" "$NGINX_R"
    if [[ -e "$R_CERT_DIR/fullchain.pem" && -e "$R_CERT_DIR/privkey.pem" ]]; then
        ln -sfn "$NGINX_R" "$NGINX_R_LINK"
        if ! nginx_apply; then
            # Never leave a configuration nginx rejects: it would block every later reload (deploys,
            # certificate renewals).
            rm -f "$NGINX_R_LINK"
            fail "nginx -t failed with $NGINX_R (the site was disabled again); enable it by hand and run nginx -t to see why"
        fi
        log "nginx site $BEACON_ZONE enabled"
    else
        rm -f "$NGINX_R_LINK"
        nginx_apply || true
        log "no certificate in $R_CERT_DIR yet: the nginx site $BEACON_ZONE stays disabled"
    fi
}

# knot_stale: true when Knot was started before one of its files was last written, so it still runs
# with older settings (a run of this script that stopped after writing them, for example).
knot_stale() {
    local since f
    since="$(systemctl show -p ActiveEnterTimestamp --value knot 2>/dev/null)"
    [[ -n "$since" ]] || return 1
    since="$(date -d "$since" +%s 2>/dev/null)" || return 1
    for f in "$KNOT_CONF" "$KNOT_KEY" "$KNOT_ZONE"; do
        [[ -e "$f" && "$(stat -c %Y "$f")" -gt "$since" ]] && return 0
    done
    return 1
}

# After the binaries are installed: start Knot, switch the site's DNS row on or off, print the checks.
beacon_start() {
    ((beacon)) || return 0
    systemctl enable knot >/dev/null 2>&1 || log "warning: could not enable knot"
    if ((beacon_changed)) || knot_stale || ! systemctl is-active --quiet knot; then
        systemctl restart knot || log "warning: knot did not start: journalctl -u knot"
    fi
    if [[ -x "$BIN_DIR/openipdata-beacon" ]]; then
        if ((beacon_unit_changed)) && systemctl is-active --quiet openipdata-beacon.service; then
            systemctl restart openipdata-beacon.service || log "warning: openipdata-beacon did not restart: journalctl -u openipdata-beacon"
        else
            systemctl start openipdata-beacon.service || log "warning: openipdata-beacon did not start: journalctl -u openipdata-beacon"
        fi
    else
        log "the beacon binary is not installed yet (release asset openipdata-beacon-linux-amd64; the updater installs it within 2 minutes after the workflow ran)"
    fi

    # The site's DNS row is off by default (BEACON_ZONE empty in the binary). It is switched on by this
    # drop-in only when the whole chain exists: the nginx site of the one-time names is enabled. The live
    # instance is restarted only when the drop-in changed.
    local want=0 changed=0 drop="$SITE_DROPIN_DIR/beacon.conf"
    [[ -L "$NGINX_R_LINK" ]] && want=1
    if ((want)); then
        install -d -m 0755 "$SITE_DROPIN_DIR"
        printf '[Service]\nEnvironment=BEACON_ZONE=%s\nEnvironment=REDIS_ADDR=127.0.0.1:6379\n' "$BEACON_ZONE" >"$drop.tmp"
        if ! cmp -s "$drop.tmp" "$drop" 2>/dev/null; then mv -f "$drop.tmp" "$drop"; changed=1; else rm -f "$drop.tmp"; fi
    elif [[ -e "$drop" ]]; then
        rm -f "$drop"
        changed=1
    fi
    if ((changed)); then
        systemctl daemon-reload
        if [[ -n "${live_port:-}" ]] && systemctl is-active --quiet "openipdata-site@$live_port"; then
            systemctl restart "openipdata-site@$live_port" || log "warning: could not restart openipdata-site@$live_port"
        fi
        log "site DNS row: $( ((want)) && echo on || echo off ) (drop-in $drop)"
    fi

    printf 'beacon services (active):\n'
    for u in redis-server knot openipdata-beacon; do
        printf '  %s: %s\n' "$u" "$(systemctl is-active "$u" 2>&1 || true)"
    done
    cat <<MSG

DNS beacon: add these records in the openipdata.org zone at Cloudflare (DNS only, grey cloud):

    r         NS    ns-r.openipdata.org
    ns-r      A     $PUBLIC_IPV4$( [[ -n "$PUBLIC_IPV6" ]] && printf '\n    ns-r      AAAA  %s' "$PUBLIC_IPV6" )

Test the zone (use any 32 hex characters), then the stored address:

    dig @$PUBLIC_IPV4 0123456789abcdef0123456789abcdef.r.$DOMAIN
    redis-cli hgetall beacon:0123456789abcdef0123456789abcdef

MSG
    if [[ -L "$NGINX_R_LINK" ]]; then
        echo "With the certificate in place, this answers JSON: curl https://0123456789abcdef0123456789abcdef.r.$DOMAIN/"
    else
        cat <<MSG
The certificate for *.r.$DOMAIN is missing, so its nginx site is not enabled. After the DNS records above
resolve (dig NS r.$DOMAIN), get it once (Knot answers the DNS challenge, nothing else to set up):

    sudo certbot certonly --cert-name $BEACON_ZONE --dns-rfc2136 --dns-rfc2136-credentials $ACME_INI --dns-rfc2136-propagation-seconds 5 -d '*.$BEACON_ZONE'

(add --dry-run to try it first), then rerun this script. Renewal is automatic (certbot timer); the
existing renewal hook reloads nginx. A rerun of this script while a challenge is in flight is safe
for the files, but a restart of Knot drops the challenge record: that renewal attempt fails and the
next one works.
MSG
    fi
}

[[ "${EUID}" -eq 0 ]] || fail "run this script as root"
command -v systemctl >/dev/null 2>&1 || fail "systemd not found"
command -v apt-get >/dev/null 2>&1 || fail "apt-get not found (Ubuntu or Debian only)"
for f in "${SCRIPTS[@]}" "${UNITS[@]/#/systemd/}" nginx/openipdata.org.conf nginx/v46.openipdata.org.conf nginx/r.openipdata.org.conf knot/knot.conf knot/r.openipdata.org.zone nginx/default-deny.conf nginx/upstream-8080.conf nginx/upstream-8081.conf openipdata-site.env.example; do
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
    # A changed beacon unit (e.g. BEACON_TTL) only takes effect after a restart: beacon_start does it.
    if [[ "$u" == openipdata-beacon.service ]] && ! cmp -s "$SCRIPT_DIR/systemd/$u" "/etc/systemd/system/$u"; then
        beacon_unit_changed=1
    fi
    install -m 0644 -o root -g root "$SCRIPT_DIR/systemd/$u" "/etc/systemd/system/$u"
done
systemctl daemon-reload
log "installed systemd units"

# Firewall: SSH and web only; Cloudflare-only access for openipdata.org is enforced by nginx (v4. and v6. are direct).
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
# The upstream must exist before any nginx -t that loads openipdata.org.conf. The live link is the only
# record of the live port: created once, never changed here (except by the migration below).
install -d -m 0755 -o root -g root "$UPSTREAM_DIR"
install -m 0644 -o root -g root "$SCRIPT_DIR/nginx/upstream-8080.conf" "$UPSTREAM_DIR/upstream-8080.conf"
install -m 0644 -o root -g root "$SCRIPT_DIR/nginx/upstream-8081.conf" "$UPSTREAM_DIR/upstream-8081.conf"
if [[ ! -e "$LIVE_LINK" && ! -L "$LIVE_LINK" ]]; then
    ln -s "$UPSTREAM_DIR/upstream-8080.conf" "$LIVE_LINK"
    log "created $LIVE_LINK -> upstream-8080.conf"
fi
install -m 0644 -o root -g root "$SCRIPT_DIR/nginx/openipdata.org.conf" "$NGINX_SITE"
install -m 0644 -o root -g root "$SCRIPT_DIR/nginx/v46.openipdata.org.conf" "$NGINX_V46"
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
    ln -sfn "$NGINX_V46" "$NGINX_V46_LINK"
    nginx -t || fail "nginx -t failed; see above"
    systemctl enable nginx >/dev/null 2>&1 || true
    if systemctl is-active --quiet nginx; then
        systemctl reload nginx
    else
        systemctl start nginx
    fi
    log "nginx site enabled"
else
    rm -f "$NGINX_LINK" "$NGINX_V46_LINK"
    nginx -t || fail "nginx -t failed; see above"
    systemctl enable nginx >/dev/null 2>&1 || true
    if systemctl is-active --quiet nginx; then
        systemctl reload nginx
    else
        systemctl start nginx
    fi
    log "no certificate in $CERT_DIR yet: the nginx site stays disabled (default-deny is active)"
fi

beacon_prepare

# The migration must not interleave with a timer run of the updater: same lock.
exec 9>/run/openipdata-site-update.lock
if ! flock -w 300 9; then
    log "error: openipdata-site-update holds /run/openipdata-site-update.lock for more than 300 s; try again later"
    exit 1
fi
migrate_old_service
exec 9>&-

# Data and binary. Each unit is a oneshot: start waits until it has finished.
log "pulling the data files"
systemctl start openipdata-mmdb-pull.service || log "warning: the data pull failed: journalctl -u openipdata-mmdb-pull"
log "installing the site binary"
systemctl start openipdata-site-update.service || log "warning: the binary update failed: journalctl -u openipdata-site-update"
live_port="$(readlink "$LIVE_LINK" | sed -n 's/.*upstream-\([0-9]*\)\.conf$/\1/p')"
if [[ ! -e "$OPT_DIR/slot-${live_port:-8080}/openipdata-site" ]]; then
    log "no site binary installed: the GitHub release 'site' does not exist yet or the build was rejected."
    log "run the 'Publish site binary' workflow (Actions tab, Run workflow, on master) first; the timer installs it within 2 minutes."
fi

systemctl enable --now "${TIMERS[@]}" >/dev/null
beacon_start

# Status.
log "status"
printf 'live port (%s): %s\n' "$LIVE_LINK" "${live_port:-unknown}"
if [[ -n "$live_port" && -x "$OPT_DIR/slot-$live_port/openipdata-site" ]]; then
    printf 'binary version: %s\n' "$(timeout 5 setpriv --reuid="$RUN_AS" --regid="$RUN_AS" --clear-groups "$OPT_DIR/slot-$live_port/openipdata-site" -version 2>&1 || echo 'no answer')"
    printf 'healthz: %s\n' "$(curl -fsS -m 3 "http://127.0.0.1:$live_port/healthz" 2>&1 || echo 'not answering')"
else
    echo "binary version: not installed"
fi
echo "site instances (active / enabled):"
for p in 8080 8081; do
    printf '  openipdata-site@%s: %s / %s\n' "$p" "$(systemctl is-active "openipdata-site@$p" 2>&1 || true)" "$(systemctl is-enabled "openipdata-site@$p" 2>&1 || true)"
done
echo "data files:"
shopt -s nullglob
files=("$DATA_DIR"/*.mmdb "$DATA_DIR"/*.sqlite3)
if ((${#files[@]})); then ls -l "${files[@]}"; else echo "  none yet"; fi
echo "timers:"
systemctl list-timers --no-pager 'openipdata-*' || true

if ((!have_cert)); then
    cat <<MSG

The TLS certificate is missing, so the nginx site is not enabled yet. Do this once:

1. Create a Cloudflare API token (My Profile, API Tokens) with the permission Zone:DNS:Edit
   for the zone $DOMAIN, and put it in $CF_CREDENTIALS (mode 0600):

       sudo install -d -m 0700 /root/.secrets
       printf 'dns_cloudflare_api_token = <the token>\n' | sudo tee $CF_CREDENTIALS >/dev/null
       sudo chmod 0600 $CF_CREDENTIALS

2. Get the certificate:

       sudo certbot certonly --dns-cloudflare --dns-cloudflare-credentials $CF_CREDENTIALS -d $DOMAIN -d '*.$DOMAIN'

3. Rerun this script:  sudo $SCRIPT_DIR/init-openipdata-site.sh
MSG
fi
log "provisioning complete"
