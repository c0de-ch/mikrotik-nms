#!/usr/bin/env bash
#
# runner-deploy.sh — narrow-scope deploy entrypoint for the GitHub Actions
# self-hosted runner.
#
# This script is the *only* command the gh-runner user is allowed to run via
# sudo (see deploy/lxc/runner-sudoers). It takes a workspace directory
# (typically $GITHUB_WORKSPACE), validates that it looks like a checkout of
# this repo, then runs the standard install.sh against it with --skip-deps.
#
# Why this exists:
#   - Granting `sudo deploy/lxc/install.sh` from any path would give the
#     runner user effective root for any script named install.sh.
#   - This wrapper validates the source dir and pins the install args, so
#     the runner can only run the real installer against a real checkout.
#
# Usage (from the workflow):
#   sudo /usr/local/sbin/mikrotik-nms-deploy "$GITHUB_WORKSPACE"
#
# Exit codes:
#   0  deploy succeeded
#   2  source dir failed validation
#   3  install.sh failed
#   4  health check failed

set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: $0 <workspace-dir>" >&2
    exit 64
fi

WORKSPACE="$1"

# Resolve and validate the workspace.
if [[ ! -d "$WORKSPACE" ]]; then
    echo "[runner-deploy] workspace not a directory: $WORKSPACE" >&2
    exit 2
fi
WORKSPACE="$(cd -- "$WORKSPACE" && pwd)"

# Refuse paths outside the runner's home + work tree, to keep the surface tight.
case "$WORKSPACE" in
    /home/gh-runner/*|/var/lib/gh-runner/*|/opt/actions-runner/*) ;;
    *)
        echo "[runner-deploy] workspace not under an allowed runner root: $WORKSPACE" >&2
        exit 2
        ;;
esac

# Sanity-check the checkout looks like our repo.
for required in backend/go.mod frontend/package.json deploy/lxc/install.sh; do
    if [[ ! -f "$WORKSPACE/$required" ]]; then
        echo "[runner-deploy] missing $required in workspace" >&2
        exit 2
    fi
done

# Forward to the real installer with a fixed flag set. We deliberately do not
# pass through any caller-supplied flags — anything operator-tunable belongs
# in the env file (/etc/mikrotik-nms/env).
echo "[runner-deploy] running install.sh --skip-deps from $WORKSPACE"
if ! "$WORKSPACE/deploy/lxc/install.sh" --skip-deps --source "$WORKSPACE"; then
    echo "[runner-deploy] install.sh exited non-zero" >&2
    exit 3
fi

# Smoke test: backend health endpoint.
for unit in mikrotik-nms-backend mikrotik-nms-frontend caddy; do
    if ! systemctl is-active --quiet "$unit"; then
        echo "[runner-deploy] $unit is not active after deploy" >&2
        systemctl --no-pager --lines=20 status "$unit" || true
        exit 4
    fi
done

if ! curl -fsSL --max-time 5 http://127.0.0.1:8080/api/v1/health > /dev/null; then
    echo "[runner-deploy] /api/v1/health smoke test failed" >&2
    exit 4
fi

echo "[runner-deploy] deploy succeeded"
