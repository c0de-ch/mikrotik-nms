#!/usr/bin/env bash
#
# MikroTik NMS — one-shot Proxmox API bootstrap
#
# Creates a fresh Debian 13 LXC container on a remote Proxmox host using the
# REST API, waits for it to come up, SSHes in as root, clones the repo, and
# runs deploy/lxc/install.sh inside it. End result: a fully running NMS on a
# brand new container, started from nothing but a Proxmox API token.
#
# Usage:
#   1. Copy proxmox-create.env.example to proxmox-create.env and fill in
#      the values.
#   2. Run:  ./proxmox-create.sh
#
#   Or pass settings as environment variables / a custom env file:
#       ./proxmox-create.sh --env /path/to/my.env
#       PROXMOX_URL=... LXC_VMID=200 ./proxmox-create.sh
#
# Required tools on the machine running this script:  bash, curl, jq, ssh.
# (sshpass is only required if you authenticate with a password instead of
# an SSH key — using a key is strongly recommended.)
#
# Idempotency: this script refuses to run if the requested VMID already
# exists on the target node. To redeploy app code into an existing LXC, SSH
# into it directly and re-run deploy/lxc/install.sh --skip-deps.

set -euo pipefail

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

log()  { printf '\033[1;34m[+]\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    sed -n '2,28p' "$0"
    exit "${1:-0}"
}

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

# ---------------------------------------------------------------------------
# arg parsing — only --env <file> and --help; everything else is env vars
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
ENV_FILE_DEFAULT="$SCRIPT_DIR/proxmox-create.env"
ENV_FILE=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --env)     ENV_FILE="$2"; shift 2 ;;
        -h|--help) usage 0 ;;
        *)         warn "unknown flag: $1"; usage 1 ;;
    esac
done

if [[ -z "$ENV_FILE" && -f "$ENV_FILE_DEFAULT" ]]; then
    ENV_FILE="$ENV_FILE_DEFAULT"
fi
if [[ -n "$ENV_FILE" ]]; then
    [[ -f "$ENV_FILE" ]] || die "env file not found: $ENV_FILE"
    log "loading $ENV_FILE"
    set -a
    # shellcheck disable=SC1090
    . "$ENV_FILE"
    set +a
fi

# ---------------------------------------------------------------------------
# required + optional config
# ---------------------------------------------------------------------------

: "${PROXMOX_URL:?PROXMOX_URL must be set, e.g. https://pve.lan:8006}"
: "${PROXMOX_TOKEN:?PROXMOX_TOKEN must be set, format USER@REALM!TOKENID=SECRET}"
: "${PROXMOX_NODE:?PROXMOX_NODE must be set, e.g. pve}"
: "${LXC_VMID:?LXC_VMID must be set, e.g. 200}"
: "${LXC_HOSTNAME:?LXC_HOSTNAME must be set, e.g. mikrotik-nms}"

LXC_STORAGE_ROOTFS=${LXC_STORAGE_ROOTFS:-local-lvm}
LXC_STORAGE_TEMPLATES=${LXC_STORAGE_TEMPLATES:-local}
LXC_NETWORK_BRIDGE=${LXC_NETWORK_BRIDGE:-vmbr0}
LXC_NETWORK=${LXC_NETWORK:-dhcp}     # "dhcp" or "ip=A.B.C.D/24,gw=A.B.C.1"
LXC_CORES=${LXC_CORES:-2}
LXC_MEMORY=${LXC_MEMORY:-1024}
LXC_SWAP=${LXC_SWAP:-512}
LXC_DISK_GB=${LXC_DISK_GB:-8}
LXC_TEMPLATE_PATTERN=${LXC_TEMPLATE_PATTERN:-debian-13-standard}

NMS_REPO_URL=${NMS_REPO_URL:-https://github.com/c0de-ch/mikrotik-nms.git}
NMS_REPO_REF=${NMS_REPO_REF:-main}
NMS_INSTALL_FLAGS=${NMS_INSTALL_FLAGS:---no-tls}
NMS_SRC_DIR=${NMS_SRC_DIR:-/opt/src/mikrotik-nms}

PROXMOX_VERIFY_TLS=${PROXMOX_VERIFY_TLS:-1}   # set to 0 to skip cert verification

LXC_SSH_PUBKEY=${LXC_SSH_PUBKEY:-}            # path to a public key file
LXC_SSH_PRIVKEY=${LXC_SSH_PRIVKEY:-}          # matching private key for SSH login
LXC_PASSWORD=${LXC_PASSWORD:-}                # fallback if no key provided

if [[ -z "$LXC_SSH_PUBKEY" && -z "$LXC_PASSWORD" ]]; then
    die "set LXC_SSH_PUBKEY (recommended) or LXC_PASSWORD so the script can SSH into the new LXC"
fi
if [[ -n "$LXC_SSH_PUBKEY" && ! -f "$LXC_SSH_PUBKEY" ]]; then
    die "LXC_SSH_PUBKEY=$LXC_SSH_PUBKEY does not exist"
fi
if [[ -n "$LXC_SSH_PRIVKEY" && ! -f "$LXC_SSH_PRIVKEY" ]]; then
    die "LXC_SSH_PRIVKEY=$LXC_SSH_PRIVKEY does not exist"
fi

require_cmd curl
require_cmd jq
require_cmd ssh
[[ -z "$LXC_SSH_PUBKEY" ]] && require_cmd sshpass

# ---------------------------------------------------------------------------
# Proxmox API helper
# ---------------------------------------------------------------------------

CURL_OPTS=(--silent --show-error --fail-with-body
    -H "Authorization: PVEAPIToken=$PROXMOX_TOKEN")
[[ "$PROXMOX_VERIFY_TLS" == "0" ]] && CURL_OPTS+=(--insecure)

api() {
    local method=$1; shift
    local path=$1; shift
    curl "${CURL_OPTS[@]}" -X "$method" "$PROXMOX_URL/api2/json$path" "$@"
}

# ---------------------------------------------------------------------------
# 1. sanity-check the API token and target node
# ---------------------------------------------------------------------------

log "verifying API token and node $PROXMOX_NODE"
node_status=$(api GET "/nodes/$PROXMOX_NODE/status" || true)
if [[ -z "$node_status" ]]; then
    die "could not reach $PROXMOX_URL/api2/json/nodes/$PROXMOX_NODE/status — check URL/token/permissions"
fi

# ---------------------------------------------------------------------------
# 2. refuse if VMID already exists
# ---------------------------------------------------------------------------

existing=$(api GET "/cluster/resources?type=vm" \
    | jq -r --argjson id "$LXC_VMID" '.data[] | select(.vmid == $id) | .vmid' 2>/dev/null || true)
if [[ -n "$existing" ]]; then
    die "VMID $LXC_VMID already exists in the cluster — pick another or remove it first"
fi

# ---------------------------------------------------------------------------
# 3. find or download the Debian template
# ---------------------------------------------------------------------------

log "looking for $LXC_TEMPLATE_PATTERN on storage $LXC_STORAGE_TEMPLATES"
template=$(api GET "/nodes/$PROXMOX_NODE/storage/$LXC_STORAGE_TEMPLATES/content?content=vztmpl" \
    | jq -r --arg pat "$LXC_TEMPLATE_PATTERN" \
        '.data | map(select(.volid | contains($pat))) | sort_by(.volid) | last(.[]?).volid // empty')

if [[ -z "$template" ]]; then
    log "template not present, fetching list of available templates from Proxmox"
    api POST "/nodes/$PROXMOX_NODE/aplinfo/update" >/dev/null 2>&1 || true
    avail=$(api GET "/nodes/$PROXMOX_NODE/aplinfo" \
        | jq -r --arg pat "$LXC_TEMPLATE_PATTERN" \
            '.data | map(select(.template | contains($pat))) | sort_by(.template) | last(.[]?).template // empty')
    [[ -n "$avail" ]] || die "no template matching '$LXC_TEMPLATE_PATTERN' is available in aplinfo on $PROXMOX_NODE"
    log "downloading $avail to $LXC_STORAGE_TEMPLATES (this can take a few minutes)"
    api POST "/nodes/$PROXMOX_NODE/aplinfo" \
        --data-urlencode "storage=$LXC_STORAGE_TEMPLATES" \
        --data-urlencode "template=$avail" >/dev/null
    # Poll until the template appears in the storage content listing.
    for _ in $(seq 1 120); do
        sleep 5
        template=$(api GET "/nodes/$PROXMOX_NODE/storage/$LXC_STORAGE_TEMPLATES/content?content=vztmpl" \
            | jq -r --arg pat "$LXC_TEMPLATE_PATTERN" \
                '.data | map(select(.volid | contains($pat))) | sort_by(.volid) | last(.[]?).volid // empty')
        [[ -n "$template" ]] && break
    done
    [[ -n "$template" ]] || die "template download did not finish in 10 minutes"
fi
log "using template: $template"

# ---------------------------------------------------------------------------
# 4. build the network spec and create the LXC
# ---------------------------------------------------------------------------

case "$LXC_NETWORK" in
    dhcp) net0="name=eth0,bridge=$LXC_NETWORK_BRIDGE,ip=dhcp" ;;
    *)    net0="name=eth0,bridge=$LXC_NETWORK_BRIDGE,$LXC_NETWORK" ;;
esac

log "creating LXC $LXC_VMID ($LXC_HOSTNAME) on $PROXMOX_NODE"
create_args=(
    --data-urlencode "vmid=$LXC_VMID"
    --data-urlencode "ostemplate=$template"
    --data-urlencode "hostname=$LXC_HOSTNAME"
    --data-urlencode "cores=$LXC_CORES"
    --data-urlencode "memory=$LXC_MEMORY"
    --data-urlencode "swap=$LXC_SWAP"
    --data-urlencode "rootfs=$LXC_STORAGE_ROOTFS:$LXC_DISK_GB"
    --data-urlencode "net0=$net0"
    --data-urlencode "unprivileged=1"
    --data-urlencode "features=nesting=1"
    --data-urlencode "onboot=1"
    --data-urlencode "start=1"
)
[[ -n "$LXC_PASSWORD"   ]] && create_args+=(--data-urlencode "password=$LXC_PASSWORD")
[[ -n "$LXC_SSH_PUBKEY" ]] && create_args+=(--data-urlencode "ssh-public-keys=$(cat "$LXC_SSH_PUBKEY")")

api POST "/nodes/$PROXMOX_NODE/lxc" "${create_args[@]}" >/dev/null

# ---------------------------------------------------------------------------
# 5. wait for it to be running and to obtain an IP
# ---------------------------------------------------------------------------

log "waiting for LXC to enter running state"
status=""
for _ in $(seq 1 60); do
    status=$(api GET "/nodes/$PROXMOX_NODE/lxc/$LXC_VMID/status/current" 2>/dev/null \
        | jq -r '.data.status // empty')
    [[ "$status" == "running" ]] && break
    sleep 1
done
[[ "$status" == "running" ]] || die "LXC did not start within 60 s"

log "waiting for eth0 to acquire an IPv4 address"
ip=""
for _ in $(seq 1 60); do
    ip=$(api GET "/nodes/$PROXMOX_NODE/lxc/$LXC_VMID/interfaces" 2>/dev/null \
        | jq -r '.data[]? | select(.name=="eth0") | .inet // empty' \
        | head -n1 | cut -d/ -f1)
    [[ -n "$ip" && "$ip" != "null" ]] && break
    sleep 2
done
[[ -n "$ip" ]] || die "LXC did not get an IPv4 address on eth0 within 2 minutes"
log "LXC IP: $ip"

# ---------------------------------------------------------------------------
# 6. wait for SSH and run the installer inside the container
# ---------------------------------------------------------------------------

log "waiting for SSH on $ip:22"
for _ in $(seq 1 60); do
    if (echo > /dev/tcp/"$ip"/22) >/dev/null 2>&1; then break; fi
    sleep 2
done

ssh_opts=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
          -o LogLevel=ERROR -o ConnectTimeout=10)
[[ -n "$LXC_SSH_PRIVKEY" ]] && ssh_opts+=(-i "$LXC_SSH_PRIVKEY")

run_remote() {
    if [[ -n "$LXC_SSH_PUBKEY" ]]; then
        ssh "${ssh_opts[@]}" "root@$ip" "$@"
    else
        SSHPASS="$LXC_PASSWORD" sshpass -e ssh "${ssh_opts[@]}" "root@$ip" "$@"
    fi
}

log "installing git inside the LXC"
run_remote "apt-get update -qq && apt-get install -y -qq git ca-certificates"

log "cloning $NMS_REPO_URL ($NMS_REPO_REF) into $NMS_SRC_DIR"
run_remote "git clone --branch '$NMS_REPO_REF' '$NMS_REPO_URL' '$NMS_SRC_DIR'"

log "running install.sh $NMS_INSTALL_FLAGS inside the LXC (this takes a few minutes)"
run_remote "$NMS_SRC_DIR/deploy/lxc/install.sh $NMS_INSTALL_FLAGS"

# ---------------------------------------------------------------------------
# 7. summary
# ---------------------------------------------------------------------------

cat >&2 <<EOF

============================================================================
  MikroTik NMS deployed.

  LXC vmid : $LXC_VMID
  Hostname : $LXC_HOSTNAME
  Address  : http://$ip
  Node     : $PROXMOX_NODE

  First-run admin setup:
    Open http://$ip in a browser and create the initial admin user.

  Logs (run on the LXC):
    journalctl -u mikrotik-nms-backend  -f
    journalctl -u mikrotik-nms-frontend -f
    journalctl -u caddy                 -f

  To re-deploy app code later:
    ssh root@$ip
    cd $NMS_SRC_DIR && git pull && ./deploy/lxc/install.sh --skip-deps
============================================================================
EOF
