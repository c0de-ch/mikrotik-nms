#!/usr/bin/env bash
#
# Install the MikroTik NMS deploy webhook agent on a Debian/Ubuntu host.
#
# What it does:
#   - Installs Go (if missing) and builds backend/cmd/deploy-agent
#   - Creates a system user mikrotik-nms-agent
#   - Installs the binary to /opt/mikrotik-nms-deploy-agent/bin/
#   - Drops env, run.sh, and the SSH key into /etc/mikrotik-nms-deploy-agent/
#     (only if they don't already exist; safe to re-run)
#   - Installs and enables the systemd unit
#
# Run as root from a checkout of the mikrotik-c0de repo.
#
# Usage:
#   sudo ./deploy/webhook-agent/install.sh
#   sudo ./deploy/webhook-agent/install.sh --skip-go    # if Go already installed

set -euo pipefail

GO_VERSION="1.25.3"
INSTALL_PREFIX="/opt/mikrotik-nms-deploy-agent"
DATA_DIR="/var/lib/mikrotik-nms-deploy-agent"
CONFIG_DIR="/etc/mikrotik-nms-deploy-agent"
SERVICE_USER="mikrotik-nms-agent"

SKIP_GO=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-go) SKIP_GO=1; shift ;;
        -h|--help) sed -n '2,18p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 1 ;;
    esac
done

log()  { printf '\033[1;34m[+]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "must run as root"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
REPO_DIR="$(cd -- "$SCRIPT_DIR/../.." &>/dev/null && pwd)"
[[ -f "$REPO_DIR/backend/cmd/deploy-agent/main.go" ]] || die "could not find deploy-agent source under $REPO_DIR"

# --- Go ---
if [[ $SKIP_GO -eq 0 ]]; then
    if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION}"; then
        log "installing Go ${GO_VERSION}"
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        apt-get install -y -qq curl ca-certificates
        arch=$(uname -m)
        case "$arch" in
            x86_64)  goarch=amd64 ;;
            aarch64) goarch=arm64 ;;
            *) die "unsupported arch: $arch" ;;
        esac
        tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
        curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${goarch}.tar.gz" -o "$tmp/go.tgz"
        rm -rf /usr/local/go
        tar -C /usr/local -xzf "$tmp/go.tgz"
    else
        log "Go ${GO_VERSION} already installed"
    fi
fi
export PATH="/usr/local/go/bin:$PATH"

# --- Build ---
log "building deploy-agent"
(
    cd "$REPO_DIR/backend"
    CGO_ENABLED=0 /usr/local/go/bin/go build \
        -trimpath -ldflags="-s -w" \
        -o "$INSTALL_PREFIX/bin/deploy-agent" \
        ./cmd/deploy-agent
)

# --- User + dirs ---
if ! id "$SERVICE_USER" &>/dev/null; then
    log "creating system user '$SERVICE_USER'"
    useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$SERVICE_USER"
fi
install -d -m 0755 -o "$SERVICE_USER" -g "$SERVICE_USER" "$INSTALL_PREFIX" "$INSTALL_PREFIX/bin"
install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_USER" "$DATA_DIR"
install -d -m 0750 -o root           -g "$SERVICE_USER" "$CONFIG_DIR"
chown "$SERVICE_USER":"$SERVICE_USER" "$INSTALL_PREFIX/bin/deploy-agent"
chmod 0755 "$INSTALL_PREFIX/bin/deploy-agent"

# --- Config files (only if absent) ---
if [[ ! -f "$CONFIG_DIR/env" ]]; then
    log "installing $CONFIG_DIR/env (template — EDIT BEFORE STARTING)"
    install -m 0640 -o root -g "$SERVICE_USER" \
        "$SCRIPT_DIR/agent.env.example" "$CONFIG_DIR/env"
    secret=$(openssl rand -hex 32 2>/dev/null || head -c32 /dev/urandom | xxd -p -c64)
    sed -i "s|^WEBHOOK_SECRET=.*|WEBHOOK_SECRET=$secret|" "$CONFIG_DIR/env"
    log "  generated random WEBHOOK_SECRET — copy it into the GitHub webhook config"
else
    log "$CONFIG_DIR/env already exists, leaving it untouched"
fi

if [[ ! -f "$CONFIG_DIR/run.sh" ]]; then
    log "installing $CONFIG_DIR/run.sh (sample — EDIT BEFORE STARTING)"
    install -m 0750 -o root -g "$SERVICE_USER" \
        "$SCRIPT_DIR/run.sh.example" "$CONFIG_DIR/run.sh"
fi

# --- systemd unit ---
log "installing systemd unit"
install -m 0644 "$SCRIPT_DIR/systemd/mikrotik-nms-deploy-agent.service" \
    /etc/systemd/system/

systemctl daemon-reload
systemctl enable mikrotik-nms-deploy-agent.service

cat <<EOF

============================================================================
  Deploy webhook agent installed.

  Binary  : $INSTALL_PREFIX/bin/deploy-agent
  Config  : $CONFIG_DIR/env       <-- EDIT THIS
  Script  : $CONFIG_DIR/run.sh    <-- EDIT THIS
  Service : mikrotik-nms-deploy-agent

  Next steps:
    1. Edit $CONFIG_DIR/env (set ALLOWED_REPO; review LISTEN, etc.)
    2. Edit $CONFIG_DIR/run.sh (set LXC_HOST, install an SSH key)
    3. Generate an SSH keypair the agent will use to log into the LXC:
         sudo -u $SERVICE_USER ssh-keygen -t ed25519 -N '' \\
             -f $CONFIG_DIR/id_ed25519
       Then copy the .pub to the LXC's /root/.ssh/authorized_keys
    4. Start the agent:
         systemctl start mikrotik-nms-deploy-agent
         journalctl -u mikrotik-nms-deploy-agent -f
    5. Expose the listener to GitHub (Tailscale Funnel / Cloudflare Tunnel /
       reverse proxy). See deploy/webhook-agent/README.md for the recipes.
    6. Add a webhook in GitHub:
         repo settings -> Webhooks -> Add webhook
         Payload URL  = the public URL from step 5
         Content type = application/json
         Secret       = the WEBHOOK_SECRET from $CONFIG_DIR/env
         Events       = "Just the push event"
============================================================================
EOF
