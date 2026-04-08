#!/usr/bin/env bash
#
# expose.sh — configure GitHub-facing exposure for the deploy webhook agent.
#
# GitHub webhooks need to reach the agent over HTTPS. The agent itself only
# listens on 127.0.0.1:9000 by default; this script wires up one of four
# possible front doors:
#
#   1) tailscale  — Tailscale Funnel (recommended, easiest)
#   2) caddy      — Caddy reverse proxy + your own port-forward
#   3) wireguard  — WireGuard tunnel to a public-IP host you control
#   4) cloudflare — Cloudflare Tunnel
#
# Usage:
#   sudo ./expose.sh                                          # interactive menu
#   sudo ./expose.sh tailscale [--authkey KEY]
#   sudo ./expose.sh caddy --hostname HOST
#   sudo ./expose.sh wireguard --endpoint HOST:PORT --server-pubkey KEY \
#                              [--server-ip 10.88.0.1] [--agent-ip 10.88.0.2]
#   sudo ./expose.sh cloudflare
#   sudo ./expose.sh none           # do nothing, print docs pointer
#
# Re-runnable: pick a different mode any time. The script does NOT undo a
# previously chosen mode; if you switch from caddy to wireguard you should
# also stop/uninstall the previous one.
#
# This script is intentionally focused — it sets up one front door at a
# time. For full architecture, see deploy/webhook-agent/README.md.

set -euo pipefail

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

log()  { printf '\033[1;34m[+]\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "must run as root (try: sudo $0 $*)"

is_tty() { [[ -t 0 || -e /dev/tty ]]; }

# Read a line from the user (always from /dev/tty so this works even when
# the script is being piped). $1 = question, $2 = optional default.
prompt() {
    local question="$1" default="${2:-}" answer
    if [[ -n "$default" ]]; then
        printf '%s [%s]: ' "$question" "$default" >/dev/tty
    else
        printf '%s: ' "$question" >/dev/tty
    fi
    IFS= read -r answer </dev/tty
    if [[ -z "$answer" && -n "$default" ]]; then
        answer="$default"
    fi
    printf '%s\n' "$answer"
}

prompt_required() {
    local question="$1" answer
    while :; do
        answer=$(prompt "$question")
        if [[ -n "$answer" ]]; then
            printf '%s\n' "$answer"
            return
        fi
        printf 'value is required\n' >/dev/tty
    done
}

CONFIG_DIR="/etc/mikrotik-nms-deploy-agent"
ENV_FILE="$CONFIG_DIR/env"
AGENT_SERVICE="mikrotik-nms-deploy-agent"

# Patch the agent's env file in-place (idempotent — replace existing key or
# append if missing).
set_env() {
    local key="$1" value="$2"
    [[ -f "$ENV_FILE" ]] || die "agent env file not found at $ENV_FILE — run install.sh first"
    if grep -q "^${key}=" "$ENV_FILE"; then
        # Use a delimiter unlikely to appear in values.
        sed -i "s|^${key}=.*|${key}=${value}|" "$ENV_FILE"
    else
        printf '%s=%s\n' "$key" "$value" >> "$ENV_FILE"
    fi
}

require_cmd_or_install() {
    local cmd="$1" pkg="$2"
    if ! command -v "$cmd" >/dev/null 2>&1; then
        log "installing $pkg"
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        apt-get install -y -qq "$pkg"
    fi
}

# Read the WEBHOOK_SECRET from the env file so we can echo it to the user
# at the end of each setup.
read_secret() {
    grep '^WEBHOOK_SECRET=' "$ENV_FILE" 2>/dev/null | cut -d= -f2- | tr -d '"' || true
}

# ---------------------------------------------------------------------------
# 1) Tailscale Funnel
# ---------------------------------------------------------------------------

setup_tailscale() {
    local authkey="${TS_AUTHKEY:-}"
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --authkey) authkey="$2"; shift 2 ;;
            *) die "tailscale: unknown flag $1" ;;
        esac
    done

    if ! command -v tailscale >/dev/null 2>&1; then
        log "installing Tailscale"
        curl -fsSL https://tailscale.com/install.sh | sh
    else
        log "Tailscale already installed"
    fi

    if ! tailscale status >/dev/null 2>&1; then
        if [[ -n "$authkey" ]]; then
            log "running 'tailscale up' with provided auth key"
            tailscale up --authkey "$authkey" --ssh=false
        else
            log "Tailscale needs to authenticate. Running 'tailscale up' — open the URL it prints in a browser."
            tailscale up --ssh=false
        fi
    else
        log "Tailscale already authenticated"
    fi

    log "enabling Funnel for 127.0.0.1:9000 → public :443"
    tailscale funnel --bg --https=443 127.0.0.1:9000

    sleep 2
    local fqdn
    fqdn=$(tailscale status --json 2>/dev/null | grep -oE '"DNSName":"[^"]+"' | head -n1 | cut -d'"' -f4 | sed 's/\.$//')
    [[ -z "$fqdn" ]] && fqdn="<your-tailnet-host>.ts.net"

    cat <<EOF

============================================================================
  Tailscale Funnel is now serving the agent.

  Webhook URL for GitHub:
      https://$fqdn/webhook

  GitHub webhook secret (paste into repo settings):
      $(read_secret)

  Funnel status:
      tailscale funnel status

  To stop:
      tailscale funnel --https=443 off
============================================================================
EOF
}

# ---------------------------------------------------------------------------
# 2) Caddy reverse proxy + user's own port-forward
# ---------------------------------------------------------------------------

setup_caddy() {
    local hostname="${CADDY_HOSTNAME:-}"
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --hostname) hostname="$2"; shift 2 ;;
            *) die "caddy: unknown flag $1" ;;
        esac
    done

    if [[ -z "$hostname" ]]; then
        if is_tty; then
            hostname=$(prompt_required "Public hostname (e.g. nms-deploy.example.com)")
        else
            die "caddy: --hostname is required (or set CADDY_HOSTNAME)"
        fi
    fi

    if ! command -v caddy >/dev/null 2>&1; then
        log "installing Caddy from the official apt repo"
        export DEBIAN_FRONTEND=noninteractive
        apt-get install -y -qq curl debian-keyring debian-archive-keyring apt-transport-https
        curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key \
            | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
        curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt \
            > /etc/apt/sources.list.d/caddy-stable.list
        apt-get update -qq
        apt-get install -y -qq caddy
    fi

    log "writing /etc/caddy/Caddyfile for $hostname"
    cat >/etc/caddy/Caddyfile <<EOF
{
	admin off
}

$hostname {
	encode gzip zstd

	handle /webhook* { reverse_proxy 127.0.0.1:9000 }
	handle /healthz  { reverse_proxy 127.0.0.1:9000 }
	handle           { respond 404 }

	header {
		Strict-Transport-Security "max-age=31536000"
		X-Content-Type-Options    "nosniff"
		-Server
	}
}
EOF

    systemctl restart caddy

    cat <<EOF

============================================================================
  Caddy is now configured for $hostname.

  REMAINING MANUAL STEPS:
    1. Point an A record for $hostname at this host's public IP.
    2. Forward TCP/80 (for ACME) and TCP/443 from your router to this host.
    3. Caddy will provision a Let's Encrypt cert on the first request.

  Webhook URL for GitHub:
      https://$hostname/webhook

  GitHub webhook secret (paste into repo settings):
      $(read_secret)

  Caddy logs:
      journalctl -u caddy -f
============================================================================
EOF
}

# ---------------------------------------------------------------------------
# 3) WireGuard tunnel to a public-IP host (self-hosted relay)
# ---------------------------------------------------------------------------

setup_wireguard() {
    local endpoint="${WG_SERVER_ENDPOINT:-}"
    local server_pubkey="${WG_SERVER_PUBKEY:-}"
    local server_ip="${WG_SERVER_IP:-10.88.0.1}"
    local agent_ip="${WG_AGENT_IP:-10.88.0.2}"
    local bind_listen="${WG_BIND_LISTEN:-1}"
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --endpoint)      endpoint="$2"; shift 2 ;;
            --server-pubkey) server_pubkey="$2"; shift 2 ;;
            --server-ip)     server_ip="$2"; shift 2 ;;
            --agent-ip)      agent_ip="$2"; shift 2 ;;
            --no-bind)       bind_listen=0; shift ;;
            *) die "wireguard: unknown flag $1" ;;
        esac
    done

    if [[ -z "$endpoint" ]]; then
        if is_tty; then
            endpoint=$(prompt_required "WireGuard server endpoint (host:port)")
        else
            die "wireguard: --endpoint is required (or set WG_SERVER_ENDPOINT)"
        fi
    fi
    if [[ -z "$server_pubkey" ]]; then
        if is_tty; then
            server_pubkey=$(prompt_required "WireGuard server public key")
        else
            die "wireguard: --server-pubkey is required (or set WG_SERVER_PUBKEY)"
        fi
    fi
    if is_tty && [[ "${WG_SERVER_IP:-}" == "" && "${WG_AGENT_IP:-}" == "" ]]; then
        server_ip=$(prompt "Server WG IP (inside the tunnel)" "$server_ip")
        agent_ip=$(prompt "Agent  WG IP (inside the tunnel)" "$agent_ip")
    fi

    log "installing wireguard-tools"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq wireguard wireguard-tools

    install -d -m 0700 /etc/wireguard
    if [[ ! -f /etc/wireguard/agent.key ]]; then
        log "generating WireGuard keypair"
        ( umask 077; wg genkey | tee /etc/wireguard/agent.key | wg pubkey > /etc/wireguard/agent.pub )
    else
        log "WireGuard keypair already exists, reusing"
    fi
    local agent_privkey agent_pubkey
    agent_privkey=$(cat /etc/wireguard/agent.key)
    agent_pubkey=$(cat /etc/wireguard/agent.pub)

    log "writing /etc/wireguard/wg0.conf"
    cat >/etc/wireguard/wg0.conf <<EOF
[Interface]
Address = $agent_ip/24
PrivateKey = $agent_privkey

[Peer]
PublicKey = $server_pubkey
Endpoint = $endpoint
AllowedIPs = $server_ip/32
PersistentKeepalive = 25
EOF
    chmod 0600 /etc/wireguard/wg0.conf

    systemctl enable --now wg-quick@wg0
    sleep 1
    if ! ping -c 1 -W 2 "$server_ip" >/dev/null 2>&1; then
        warn "could not ping $server_ip — check the server side, then 'systemctl restart wg-quick@wg0'"
    else
        log "tunnel up: $agent_ip ↔ $server_ip"
    fi

    if [[ "$bind_listen" == "1" ]]; then
        log "binding agent to $agent_ip:9000 (so it's only reachable via the tunnel)"
        set_env LISTEN "$agent_ip:9000"
        systemctl restart "$AGENT_SERVICE" || warn "agent restart failed; check journalctl -u $AGENT_SERVICE"
    fi

    cat <<EOF

============================================================================
  WireGuard tunnel established.

  Agent host  WG address : $agent_ip
  Server host WG address : $server_ip
  Server endpoint        : $endpoint
  Listener bound to      : $(if [[ "$bind_listen" == "1" ]]; then echo "$agent_ip:9000"; else echo "127.0.0.1:9000 (unchanged)"; fi)

  ----------------------------------------------------------------------
  COPY THIS TO THE WG SERVER (the public-IP host):

  Add to /etc/wireguard/wg0.conf on the server, under [Interface]:
      PrivateKey = <server's private key>
      Address    = $server_ip/24
      ListenPort = ${endpoint##*:}

  And add this peer block:
      [Peer]
      PublicKey = $agent_pubkey
      AllowedIPs = $agent_ip/32
      PersistentKeepalive = 25

  Then on the server:
      systemctl enable --now wg-quick@wg0

  And install Caddy on the server with this Caddyfile (replace HOSTNAME):
      HOSTNAME {
          handle /webhook* { reverse_proxy $agent_ip:9000 }
          handle /healthz  { reverse_proxy $agent_ip:9000 }
          handle           { respond 404 }
      }
  ----------------------------------------------------------------------

  Webhook URL for GitHub (after the server side is up):
      https://<your-public-hostname>/webhook

  GitHub webhook secret (paste into repo settings):
      $(read_secret)
============================================================================
EOF
}

# ---------------------------------------------------------------------------
# 4) Cloudflare Tunnel
# ---------------------------------------------------------------------------

setup_cloudflare() {
    if ! command -v cloudflared >/dev/null 2>&1; then
        log "installing cloudflared from the official apt repo"
        export DEBIAN_FRONTEND=noninteractive
        apt-get install -y -qq curl
        mkdir -p --mode=0755 /usr/share/keyrings
        curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg \
            | tee /usr/share/keyrings/cloudflare-main.gpg >/dev/null
        echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared $(lsb_release -cs 2>/dev/null || echo bookworm) main" \
            > /etc/apt/sources.list.d/cloudflared.list
        apt-get update -qq
        apt-get install -y -qq cloudflared
    fi

    cat <<EOF

============================================================================
  cloudflared installed.

  Cloudflare Tunnel needs an interactive login to your Cloudflare account
  and a tunnel/route created against a domain you own. The script can't
  fully automate that — follow these steps:

    1. cloudflared tunnel login
       (opens a browser, asks you to pick the zone)

    2. cloudflared tunnel create mikrotik-nms-deploy
       (note the tunnel UUID it prints)

    3. echo 'tunnel: mikrotik-nms-deploy
ingress:
  - hostname: nms-deploy.example.com
    service: http://127.0.0.1:9000
  - service: http_status:404
' | sudo tee /etc/cloudflared/config.yml

    4. cloudflared tunnel route dns mikrotik-nms-deploy nms-deploy.example.com

    5. sudo cloudflared service install

  Webhook URL for GitHub:
      https://nms-deploy.example.com/webhook

  GitHub webhook secret (paste into repo settings):
      $(read_secret)
============================================================================
EOF
}

# ---------------------------------------------------------------------------
# interactive menu
# ---------------------------------------------------------------------------

interactive_pick() {
    cat <<'EOF' >/dev/tty

GitHub needs a way to reach the deploy webhook agent on this host.
Pick how you want to expose it:

  1) Tailscale Funnel  — easiest, free, no DNS or port-forwarding needed
  2) Caddy + your own port-forward — needs a domain and router access
  3) WireGuard tunnel  — needs a public-IP host you control (VPS / gateway)
  4) Cloudflare Tunnel — needs a Cloudflare account and a domain there
  5) Skip — I'll set it up later

EOF
    while :; do
        local choice
        choice=$(prompt "Choice" "1")
        case "$choice" in
            1) MODE=tailscale;  return ;;
            2) MODE=caddy;      return ;;
            3) MODE=wireguard;  return ;;
            4) MODE=cloudflare; return ;;
            5) MODE=none;       return ;;
            *) printf 'invalid choice — pick 1-5\n' >/dev/tty ;;
        esac
    done
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

usage() { sed -n '2,32p' "$0"; exit "${1:-0}"; }

MODE="${1:-}"
[[ -n "$MODE" ]] && shift || true

case "$MODE" in
    -h|--help)              usage 0 ;;
    "")                     interactive_pick ;;
    tailscale|caddy|wireguard|cloudflare|none) ;;
    *) die "unknown mode: $MODE (expected tailscale|caddy|wireguard|cloudflare|none)" ;;
esac

case "$MODE" in
    tailscale)  setup_tailscale  "$@" ;;
    caddy)      setup_caddy      "$@" ;;
    wireguard)  setup_wireguard  "$@" ;;
    cloudflare) setup_cloudflare "$@" ;;
    none)
        cat <<EOF
no exposure mode configured. The agent is running on 127.0.0.1:9000 but
GitHub cannot reach it yet. Re-run this script when you're ready:
    sudo $0
or pick a mode directly:
    sudo $0 tailscale
    sudo $0 caddy --hostname nms-deploy.example.com
    sudo $0 wireguard --endpoint vps.example.com:51820 --server-pubkey ...
    sudo $0 cloudflare
EOF
        ;;
esac
