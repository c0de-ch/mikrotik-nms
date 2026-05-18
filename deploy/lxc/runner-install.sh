#!/usr/bin/env bash
#
# runner-install.sh — register a GitHub Actions self-hosted runner inside this
# LXC and install it as a systemd service. After this finishes, push or
# workflow_dispatch on the configured workflow will trigger a deploy.
#
# Usage:
#   sudo ./runner-install.sh \
#       --repo c0de-ch/mikrotik-nms \
#       --token AAA...                  # registration token from GitHub
#       [--name nms-lxc-runner]
#       [--labels self-hosted,linux,mikrotik-nms]
#       [--runner-version 2.319.1]
#
# Get the registration token (one-shot, valid ~1h):
#   GitHub → Settings → Actions → Runners → New self-hosted runner
#   (copy the token after `--token`)
#
# What it does:
#   1. Creates user `gh-runner` (no shell, home /home/gh-runner) if missing.
#   2. Downloads the actions-runner tarball under /home/gh-runner/runner.
#   3. Runs `config.sh` against the repo with the supplied token + labels.
#   4. Installs runner-deploy.sh at /usr/local/sbin/mikrotik-nms-deploy.
#   5. Drops a sudoers fragment letting gh-runner run that one wrapper.
#   6. Registers the runner as a systemd service (`actions.runner.*.service`).
#
# Re-running the script is idempotent for steps 1, 4, 5, 6. To re-register
# the runner against a different repo, first remove the old one via
#   sudo /home/gh-runner/runner/svc.sh uninstall
#   sudo -u gh-runner /home/gh-runner/runner/config.sh remove --token <removal-token>
# then re-run this script.
#
# Security notes:
#   - The runner runs as a dedicated unprivileged user.
#   - The sudoers fragment grants ONLY /usr/local/sbin/mikrotik-nms-deploy
#     and is restricted to NOPASSWD with no env passthrough.
#   - Workflows using the runner must set `runs-on: [self-hosted, ...]` and
#     for safety, the repo should require admin approval for first-time
#     contributors so a malicious PR cannot trigger a deploy.

set -euo pipefail

# ---- defaults ---------------------------------------------------------------

RUNNER_USER="gh-runner"
RUNNER_HOME="/home/${RUNNER_USER}"
RUNNER_DIR="${RUNNER_HOME}/runner"
RUNNER_VERSION="2.319.1"
RUNNER_NAME="$(hostname)-nms"
RUNNER_LABELS="self-hosted,linux,mikrotik-nms"
RUNNER_REPO=""
RUNNER_TOKEN=""

# ---- helpers ---------------------------------------------------------------

log()  { printf '\033[1;34m[+]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

require_root() {
    [[ $EUID -eq 0 ]] || die "must run as root (try: sudo $0 ...)"
}

usage() { sed -n '2,30p' "$0"; exit "${1:-0}"; }

# ---- arg parsing ------------------------------------------------------------

while [[ $# -gt 0 ]]; do
    case "$1" in
        --repo)            RUNNER_REPO="$2";    shift 2 ;;
        --token)           RUNNER_TOKEN="$2";   shift 2 ;;
        --name)            RUNNER_NAME="$2";    shift 2 ;;
        --labels)          RUNNER_LABELS="$2";  shift 2 ;;
        --runner-version)  RUNNER_VERSION="$2"; shift 2 ;;
        -h|--help)         usage 0 ;;
        *)                 warn "unknown flag: $1"; usage 1 ;;
    esac
done

require_root
[[ -n "$RUNNER_REPO"  ]] || die "--repo is required (e.g. owner/repo)"
[[ -n "$RUNNER_TOKEN" ]] || die "--token is required (registration token from GitHub)"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"

# ---- 1. user --------------------------------------------------------------

if ! id "$RUNNER_USER" &>/dev/null; then
    log "creating user '$RUNNER_USER'"
    useradd --system --create-home --home-dir "$RUNNER_HOME" \
            --shell /usr/sbin/nologin "$RUNNER_USER"
else
    log "user '$RUNNER_USER' already exists"
fi

# ---- 2. dependencies ------------------------------------------------------

log "ensuring runner runtime deps (curl, tar, libicu)"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl tar libicu-dev jq

# ---- 3. download runner ---------------------------------------------------

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)  RUNNER_ARCH="x64" ;;
    aarch64) RUNNER_ARCH="arm64" ;;
    *)       die "unsupported arch: $ARCH" ;;
esac

if [[ ! -x "$RUNNER_DIR/run.sh" ]]; then
    log "downloading actions-runner ${RUNNER_VERSION} (${RUNNER_ARCH})"
    install -d -o "$RUNNER_USER" -g "$RUNNER_USER" "$RUNNER_DIR"
    tarball="actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz"
    url="https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/${tarball}"
    sudo -u "$RUNNER_USER" curl -fsSL -o "$RUNNER_DIR/$tarball" "$url"
    sudo -u "$RUNNER_USER" tar -C "$RUNNER_DIR" -xzf "$RUNNER_DIR/$tarball"
    rm -f "$RUNNER_DIR/$tarball"
else
    log "actions-runner already extracted at $RUNNER_DIR"
fi

# ---- 4. configure runner --------------------------------------------------

if [[ ! -f "$RUNNER_DIR/.runner" ]]; then
    log "registering runner with $RUNNER_REPO"
    sudo -u "$RUNNER_USER" \
        "$RUNNER_DIR/config.sh" \
        --unattended \
        --replace \
        --url "https://github.com/$RUNNER_REPO" \
        --token "$RUNNER_TOKEN" \
        --name "$RUNNER_NAME" \
        --labels "$RUNNER_LABELS" \
        --work "_work"
else
    log "runner already configured ($(jq -r .agentName "$RUNNER_DIR/.runner" 2>/dev/null || echo unknown))"
    log "  to re-register, run: sudo $RUNNER_DIR/svc.sh uninstall && sudo -u $RUNNER_USER $RUNNER_DIR/config.sh remove"
fi

# ---- 5. systemd service ---------------------------------------------------

if ! systemctl list-unit-files 'actions.runner.*.service' --no-legend | grep -q .; then
    log "installing systemd service via svc.sh"
    (cd "$RUNNER_DIR" && ./svc.sh install "$RUNNER_USER")
fi
(cd "$RUNNER_DIR" && ./svc.sh start) || true

# ---- 6. deploy wrapper + sudoers ------------------------------------------

log "installing runner-deploy wrapper at /usr/local/sbin/mikrotik-nms-deploy"
install -m 0755 "$SCRIPT_DIR/runner-deploy.sh" /usr/local/sbin/mikrotik-nms-deploy

log "installing sudoers fragment for $RUNNER_USER"
SUDOERS_FILE="/etc/sudoers.d/mikrotik-nms-runner"
cat >"$SUDOERS_FILE" <<EOF
# Allow the GitHub Actions self-hosted runner to invoke the deploy wrapper.
# The wrapper validates its argument and refuses paths outside the runner's
# work directory; nothing else is grantable to this user via sudo.
$RUNNER_USER ALL=(root) NOPASSWD: /usr/local/sbin/mikrotik-nms-deploy
Defaults:$RUNNER_USER !env_keep, !env_check
EOF
chmod 0440 "$SUDOERS_FILE"
visudo -cf "$SUDOERS_FILE" >/dev/null || die "sudoers fragment failed validation"

# ---- 7. summary -----------------------------------------------------------

cat <<EOF

============================================================================
  GitHub Actions self-hosted runner installed.

  Repo       : $RUNNER_REPO
  Runner     : $RUNNER_NAME
  Labels     : $RUNNER_LABELS
  Service    : $(systemctl list-unit-files 'actions.runner.*.service' --no-legend | awk '{print $1}' | head -n1)
  User       : $RUNNER_USER
  Workspace  : $RUNNER_HOME/runner/_work
  Wrapper    : /usr/local/sbin/mikrotik-nms-deploy

  Verify the runner shows online at:
    https://github.com/$RUNNER_REPO/settings/actions/runners

  Logs:
    journalctl -u 'actions.runner.*' -f
============================================================================
EOF
