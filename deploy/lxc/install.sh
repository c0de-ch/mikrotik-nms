#!/usr/bin/env bash
#
# MikroTik NMS — LXC installer for Debian 13 (trixie)
#
# Installs Go, Node.js, Caddy, builds the backend + frontend, and registers
# them as systemd services behind a Caddy reverse proxy.
#
# Usage:
#   sudo ./install.sh [--hostname HOST] [--public-url URL] [--source DIR]
#                     [--no-tls] [--tls-internal] [--skip-deps]
#
# Examples:
#   # Internal LAN, plain HTTP on the LXC IP
#   sudo ./install.sh --no-tls
#
#   # Public hostname with auto Let's Encrypt
#   sudo ./install.sh --hostname nms.example.com
#
#   # Internal hostname with self-signed TLS (Caddy "tls internal")
#   sudo ./install.sh --hostname nms.lan --tls-internal
#
# Do NOT pass --public-url unless the API genuinely lives on a different
# origin than the frontend: it bakes an absolute API URL into the JS bundle,
# and the UI then breaks under every other hostname. By default the frontend
# is same-origin (Caddy proxies /api/* to the backend), which works no matter
# how the site is reached.
#
# Re-running this script is safe; it will rebuild and restart services.

set -euo pipefail

# ---- defaults ---------------------------------------------------------------

GO_VERSION="1.25.3"
NODE_MAJOR="22"

INSTALL_PREFIX="/opt/mikrotik-nms"
DATA_DIR="/var/lib/mikrotik-nms"
CONFIG_DIR="/etc/mikrotik-nms"
SERVICE_USER="mikrotik-nms"

HOSTNAME_ARG=""
PUBLIC_URL=""
PUBLIC_URL_EXPLICIT=0
SOURCE_DIR=""
NO_TLS=0
TLS_INTERNAL=0
SKIP_DEPS=0

# ---- helpers ----------------------------------------------------------------

log()  { printf '\033[1;34m[+]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

require_root() {
    if [[ $EUID -ne 0 ]]; then
        die "must run as root (try: sudo $0 $*)"
    fi
}

usage() {
    sed -n '2,22p' "$0"
    exit "${1:-0}"
}

# ---- arg parsing ------------------------------------------------------------

while [[ $# -gt 0 ]]; do
    case "$1" in
        --hostname)   HOSTNAME_ARG="$2"; shift 2 ;;
        --public-url) PUBLIC_URL="$2"; PUBLIC_URL_EXPLICIT=1; shift 2 ;;
        --source)     SOURCE_DIR="$2";   shift 2 ;;
        --no-tls)        NO_TLS=1;       shift   ;;
        --tls-internal)  TLS_INTERNAL=1; shift   ;;
        --skip-deps)     SKIP_DEPS=1;    shift   ;;
        -h|--help)    usage 0 ;;
        *)            warn "unknown flag: $1"; usage 1 ;;
    esac
done

require_root

if [[ $TLS_INTERNAL -eq 1 && $NO_TLS -eq 1 ]]; then
    die "--tls-internal and --no-tls are mutually exclusive"
fi
if [[ $TLS_INTERNAL -eq 1 && -z "$HOSTNAME_ARG" ]]; then
    die "--tls-internal requires --hostname"
fi

# Determine repo root: explicit --source or the parent of this script.
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
if [[ -z "$SOURCE_DIR" ]]; then
    SOURCE_DIR="$(cd -- "$SCRIPT_DIR/../.." &>/dev/null && pwd)"
fi

if [[ ! -f "$SOURCE_DIR/backend/go.mod" || ! -f "$SOURCE_DIR/frontend/package.json" ]]; then
    die "could not find backend/ and frontend/ under '$SOURCE_DIR' (use --source)"
fi

log "source dir: $SOURCE_DIR"

# Default public URL if not given.
if [[ -z "$PUBLIC_URL" ]]; then
    if [[ -n "$HOSTNAME_ARG" ]]; then
        PUBLIC_URL=$([[ $NO_TLS -eq 1 ]] && printf 'http://%s' "$HOSTNAME_ARG" || printf 'https://%s' "$HOSTNAME_ARG")
    else
        # Fall back to the LXC's primary IPv4 address.
        ip4="$(ip -4 -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n1)"
        [[ -z "$ip4" ]] && die "could not detect an IPv4 address; pass --public-url"
        PUBLIC_URL="http://$ip4"
        NO_TLS=1
    fi
fi

log "public URL: $PUBLIC_URL"

# Derive WS URL from public URL.
case "$PUBLIC_URL" in
    https://*) PUBLIC_WS_URL="wss://${PUBLIC_URL#https://}" ;;
    http://*)  PUBLIC_WS_URL="ws://${PUBLIC_URL#http://}" ;;
    *)         die "--public-url must start with http:// or https://" ;;
esac

# Bake absolute API/WS URLs into the JS bundle only when the operator
# explicitly passed --public-url. The frontend defaults to same-origin via
# the reverse proxy, which keeps the site working under every hostname it
# is reached on (IP, internal DNS name, public name). A baked URL silently
# breaks all of them the day that one hostname moves or dies.
if [[ $PUBLIC_URL_EXPLICIT -eq 1 ]]; then
    BAKED_API_URL="$PUBLIC_URL"
    BAKED_WS_URL="$PUBLIC_WS_URL"
else
    BAKED_API_URL=""
    BAKED_WS_URL=""
fi

# ---- 1. system packages -----------------------------------------------------

if [[ $SKIP_DEPS -eq 0 ]]; then
    log "installing system packages"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq \
        ca-certificates curl gnupg debian-keyring debian-archive-keyring \
        apt-transport-https git build-essential pkg-config \
        sqlite3
fi

# ---- 2. Go ------------------------------------------------------------------

install_go() {
    local current=""
    if command -v /usr/local/go/bin/go &>/dev/null; then
        current="$(/usr/local/go/bin/go version | awk '{print $3}')"
    fi
    if [[ "$current" == "go${GO_VERSION}" ]]; then
        log "go ${GO_VERSION} already installed"
        return
    fi

    log "installing go ${GO_VERSION}"
    local arch
    case "$(uname -m)" in
        x86_64)  arch="amd64" ;;
        aarch64) arch="arm64" ;;
        *)       die "unsupported arch: $(uname -m)" ;;
    esac

    local tmp
    tmp="$(mktemp -d)"
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o "$tmp/go.tgz"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$tmp/go.tgz"
    rm -rf "$tmp"
}

install_go
export PATH="/usr/local/go/bin:$PATH"

# ---- 3. Node.js -------------------------------------------------------------

install_node() {
    if command -v node &>/dev/null; then
        local v
        v="$(node -v | sed 's/^v//;s/\..*//')"
        if [[ "$v" -ge "$NODE_MAJOR" ]]; then
            log "node $(node -v) already installed"
            return
        fi
    fi
    log "installing node ${NODE_MAJOR}.x via NodeSource"
    install -d -m 0755 /etc/apt/keyrings
    curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
        | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
    chmod a+r /etc/apt/keyrings/nodesource.gpg
    echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
        > /etc/apt/sources.list.d/nodesource.list
    apt-get update -qq
    apt-get install -y -qq nodejs
}

[[ $SKIP_DEPS -eq 0 ]] && install_node

# ---- 4. Caddy ---------------------------------------------------------------

install_caddy() {
    if command -v caddy &>/dev/null; then
        log "caddy $(caddy version | awk '{print $1}') already installed"
        return
    fi
    log "installing caddy from official apt repo"
    curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key \
        | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt \
        > /etc/apt/sources.list.d/caddy-stable.list
    apt-get update -qq
    apt-get install -y -qq caddy
}

[[ $SKIP_DEPS -eq 0 ]] && install_caddy

# ---- 5. service user + dirs -------------------------------------------------

if ! id "$SERVICE_USER" &>/dev/null; then
    log "creating system user '$SERVICE_USER'"
    useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$SERVICE_USER"
fi

install -d -m 0755 -o "$SERVICE_USER" -g "$SERVICE_USER" "$INSTALL_PREFIX" "$INSTALL_PREFIX/bin"
install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_USER" "$DATA_DIR"
install -d -m 0750 -o root           -g "$SERVICE_USER" "$CONFIG_DIR"

# ---- 6. env file ------------------------------------------------------------

ENV_FILE="$CONFIG_DIR/env"
if [[ ! -f "$ENV_FILE" ]]; then
    log "generating $ENV_FILE"
    JWT_SECRET="$(openssl rand -hex 32 2>/dev/null || head -c32 /dev/urandom | xxd -p -c64)"
    ENC_KEY="$(openssl rand -hex 32 2>/dev/null || head -c32 /dev/urandom | xxd -p -c64)"
    cat >"$ENV_FILE" <<EOF
# MikroTik NMS — runtime configuration
# Generated by install.sh on $(date -Iseconds)
# Edit and re-run install.sh (or 'systemctl restart mikrotik-nms-*') to apply.

# --- backend (required) ---
MIKROTIK_NMS_JWT_SECRET=${JWT_SECRET}
MIKROTIK_NMS_ENCRYPTION_KEY=${ENC_KEY}
MIKROTIK_NMS_LISTEN=127.0.0.1:8080
MIKROTIK_NMS_DB_PATH=${DATA_DIR}/mikrotik-nms.db

# --- backend (optional defaults for new devices) ---
MIKROTIK_NMS_DEFAULT_ROS_USER=admin
MIKROTIK_NMS_DEFAULT_ROS_PASS=
MIKROTIK_NMS_DEFAULT_ROS_PORT=8728
MIKROTIK_NMS_DEFAULT_ROS_TLS=false

# --- frontend (Next.js standalone) ---
PORT=3000
HOSTNAME=127.0.0.1
NODE_ENV=production

# --- frontend build-time vars (baked into the JS bundle) ---
# These are read by install.sh during 'npm run build'.
# Leave EMPTY for same-origin (recommended): the browser calls /api/* on
# whatever hostname the site was loaded from and Caddy proxies it to the
# backend. Only set an absolute URL if the API lives on a different origin
# than the frontend; a wrong value here makes every page fail with
# "Failed to fetch" after the next rebuild.
NEXT_PUBLIC_API_URL=${BAKED_API_URL}
NEXT_PUBLIC_WS_URL=${BAKED_WS_URL}

# --- caddy ---
NMS_HOSTNAME=${HOSTNAME_ARG:-:80}
EOF
    chmod 0640 "$ENV_FILE"
    chown root:"$SERVICE_USER" "$ENV_FILE"
else
    log "$ENV_FILE already exists, leaving it untouched"
    log "  (delete it if you want install.sh to regenerate)"
fi

# Source the file so we can use its values during build.
set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a

# Reconcile baked frontend URLs on EXISTING installs. Historic env files
# always carried an absolute NEXT_PUBLIC_API_URL (the old installer wrote one
# unconditionally), which gets re-baked into the bundle on every deploy and
# breaks the UI under any other hostname. This installer serves the API on
# the same origin via Caddy, so unless the operator explicitly asks for an
# absolute URL this run, blank the baked values — in the env file and for
# this build.
if [[ $PUBLIC_URL_EXPLICIT -eq 1 ]]; then
    NEXT_PUBLIC_API_URL="$PUBLIC_URL"
    NEXT_PUBLIC_WS_URL="$PUBLIC_WS_URL"
    sed -i \
        -e "s|^NEXT_PUBLIC_API_URL=.*|NEXT_PUBLIC_API_URL=${PUBLIC_URL}|" \
        -e "s|^NEXT_PUBLIC_WS_URL=.*|NEXT_PUBLIC_WS_URL=${PUBLIC_WS_URL}|" \
        "$ENV_FILE"
    log "baking explicit public URL into the frontend: $PUBLIC_URL (persisted to $ENV_FILE)"
elif [[ -n "${NEXT_PUBLIC_API_URL:-}${NEXT_PUBLIC_WS_URL:-}" ]]; then
    warn "migrating $ENV_FILE to same-origin: blanking baked NEXT_PUBLIC_API_URL='${NEXT_PUBLIC_API_URL:-}'"
    warn "  (a baked absolute URL only works via that exact origin; pass --public-url"
    warn "   to this script if you really need one)"
    sed -i \
        -e "s|^NEXT_PUBLIC_API_URL=.*|NEXT_PUBLIC_API_URL=|" \
        -e "s|^NEXT_PUBLIC_WS_URL=.*|NEXT_PUBLIC_WS_URL=|" \
        "$ENV_FILE"
    NEXT_PUBLIC_API_URL=""
    NEXT_PUBLIC_WS_URL=""
fi

# ---- 7. build backend -------------------------------------------------------

log "building backend"
(
    cd "$SOURCE_DIR/backend"
    CGO_ENABLED=0 /usr/local/go/bin/go build \
        -trimpath -ldflags="-s -w" \
        -o "$INSTALL_PREFIX/bin/mikrotik-nms" \
        ./cmd/mikrotik-nms
)
chown "$SERVICE_USER":"$SERVICE_USER" "$INSTALL_PREFIX/bin/mikrotik-nms"

# ---- 8. build frontend ------------------------------------------------------

log "building frontend (this can take a few minutes on first run)"

# Version stamp for the UI footer (sidebar): nearest release tag + exact
# commit, so a running deploy is identifiable at a glance. Falls back to the
# UI defaults (dev/local) outside a git checkout. Same vars the Docker build
# passes as build args.
APP_VERSION="$(git -C "$SOURCE_DIR" describe --tags --abbrev=0 2>/dev/null || true)"
APP_VERSION="${APP_VERSION#v}"
COMMIT_SHA="$(git -C "$SOURCE_DIR" rev-parse HEAD 2>/dev/null || true)"
log "frontend version stamp: v${APP_VERSION:-dev} (${COMMIT_SHA:-local})"

(
    cd "$SOURCE_DIR/frontend"
    # --include=dev is required because the sourced env file sets
    # NODE_ENV=production (for the runtime systemd unit), which would
    # otherwise make npm skip devDependencies — including the
    # Tailwind v4 PostCSS plugin that the build needs.
    npm ci --no-audit --no-fund --include=dev
    NEXT_PUBLIC_API_URL="$NEXT_PUBLIC_API_URL" \
    NEXT_PUBLIC_WS_URL="$NEXT_PUBLIC_WS_URL" \
    NEXT_PUBLIC_APP_VERSION="${APP_VERSION:-dev}" \
    NEXT_PUBLIC_COMMIT_SHA="${COMMIT_SHA:-local}" \
        npm run build
)

log "installing frontend standalone bundle"
FRONTEND_DEST="$INSTALL_PREFIX/frontend"
rm -rf "$FRONTEND_DEST"
install -d -m 0755 "$FRONTEND_DEST"
# Next.js standalone layout: standalone/server.js + .next/static + public
cp -a "$SOURCE_DIR/frontend/.next/standalone/." "$FRONTEND_DEST/"
install -d "$FRONTEND_DEST/.next"
cp -a "$SOURCE_DIR/frontend/.next/static" "$FRONTEND_DEST/.next/static"
if [[ -d "$SOURCE_DIR/frontend/public" ]]; then
    cp -a "$SOURCE_DIR/frontend/public" "$FRONTEND_DEST/public"
fi
chown -R "$SERVICE_USER":"$SERVICE_USER" "$FRONTEND_DEST"

# ---- 9. systemd units -------------------------------------------------------

log "installing systemd units"
install -m 0644 "$SCRIPT_DIR/systemd/mikrotik-nms-backend.service"  /etc/systemd/system/
install -m 0644 "$SCRIPT_DIR/systemd/mikrotik-nms-frontend.service" /etc/systemd/system/

# ---- 10. caddy --------------------------------------------------------------

log "installing Caddyfile"
CADDY_TEMPLATE="$SCRIPT_DIR/caddy/Caddyfile.example"
[[ -f "$CADDY_TEMPLATE" ]] || die "missing $CADDY_TEMPLATE"

# Substitute the hostname; for plain HTTP/auto-IP we use ':80'.
caddy_site="${HOSTNAME_ARG:-:80}"
if [[ -n "$HOSTNAME_ARG" && $NO_TLS -eq 1 ]]; then
    caddy_site="http://$HOSTNAME_ARG"
fi

sed "s|@@SITE@@|$caddy_site|g" "$CADDY_TEMPLATE" > /etc/caddy/Caddyfile
if [[ $TLS_INTERNAL -eq 1 ]]; then
    # Inject `tls internal` right after the opening brace of the site
    # block. Caddy will then issue a self-signed cert from its local CA
    # instead of trying to reach Let's Encrypt for a hostname that has
    # no public DNS record.
    sed -i "0,/^${caddy_site//./\\.} {\$/{s||${caddy_site//./\\.} {\n\ttls internal|}" /etc/caddy/Caddyfile
fi
chmod 0644 /etc/caddy/Caddyfile

# ---- 11. start everything ---------------------------------------------------

log "reloading systemd and (re)starting services"
systemctl daemon-reload
systemctl enable mikrotik-nms-backend.service mikrotik-nms-frontend.service
# Use `restart` so re-runs of install.sh actually pick up the rebuilt
# binary / frontend bundle. `enable --now` is a no-op when the unit is
# already active and would silently keep the old process alive.
systemctl restart mikrotik-nms-backend.service
systemctl restart mikrotik-nms-frontend.service
systemctl restart caddy.service || systemctl enable --now caddy.service

sleep 1
log "service status:"
systemctl --no-pager --lines=0 status mikrotik-nms-backend.service mikrotik-nms-frontend.service caddy.service || true

cat <<EOF

============================================================================
  MikroTik NMS installed.

  Public URL : $PUBLIC_URL
  Backend    : 127.0.0.1:8080  (systemd: mikrotik-nms-backend)
  Frontend   : 127.0.0.1:3000  (systemd: mikrotik-nms-frontend)
  Reverse pr.: caddy           (config: /etc/caddy/Caddyfile)

  Config     : $ENV_FILE
  Database   : $DATA_DIR/mikrotik-nms.db
  Binaries   : $INSTALL_PREFIX/

  First-run admin setup:
    Open $PUBLIC_URL in a browser and create the initial admin user.

  Logs:
    journalctl -u mikrotik-nms-backend  -f
    journalctl -u mikrotik-nms-frontend -f
    journalctl -u caddy                 -f
============================================================================
EOF
