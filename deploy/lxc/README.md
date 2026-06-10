# MikroTik NMS — LXC deploy

Native (no Docker) install for a Proxmox LXC container running **Debian 13
(trixie)**. The installer compiles the Go backend, builds the Next.js
standalone frontend, and registers both as systemd units behind a Caddy
reverse proxy.

There are two supported deploy paths:

| Path                                                         | When to use                                                                |
|--------------------------------------------------------------|----------------------------------------------------------------------------|
| **A. One-shot Proxmox API bootstrap** (`proxmox-create.sh`)  | You have a Proxmox host and an API token. Easiest, fully automated.        |
| **B. Manual / `pct create`**                                 | You already have a container, or you want to provision it some other way.  |

Both paths end with `install.sh` running inside the LXC.

## What gets installed

| Path                                  | Purpose                                  |
|---------------------------------------|------------------------------------------|
| `/opt/mikrotik-nms/bin/mikrotik-nms`  | Backend binary                           |
| `/opt/mikrotik-nms/frontend/`         | Next.js standalone server + assets       |
| `/var/lib/mikrotik-nms/`              | SQLite database (writable by service)    |
| `/etc/mikrotik-nms/env`               | Environment file (secrets, ports, URLs)  |
| `/etc/systemd/system/mikrotik-nms-*.service` | Systemd units                     |
| `/etc/caddy/Caddyfile`                | Reverse proxy config                     |

System-wide deps installed: Go (from upstream tarball), Node.js (NodeSource),
Caddy (official apt repo), build-essential, sqlite3.

---

## Path A — one-shot Proxmox API bootstrap

`deploy/lxc/proxmox-create.sh` is a single bash script that uses the Proxmox
REST API to create a fresh Debian 13 LXC, waits for it to come up, SSHes in,
clones the repo, and runs `install.sh` inside it. End result: a fully running
NMS on a brand-new container, started from nothing but a Proxmox API token.

### Prerequisites on the machine running the script

```
bash, curl, jq, ssh
```

`sshpass` is only required if you authenticate with a root password instead
of an SSH key (using a key is strongly recommended).

### Required token permissions

Create a Proxmox API token in **Datacenter → Permissions → API Tokens**.
Easiest setup: assign role **`PVEAdmin`** on path `/` to the token's user
(or directly to the token if you uncheck *Privilege Separation*).

If you want least-privilege instead, the token needs:

| Path                              | Role / Privilege                                                                                                  |
|-----------------------------------|-------------------------------------------------------------------------------------------------------------------|
| `/nodes/<node>`                   | `Sys.Audit`, `Sys.Modify`                                                                                         |
| `/storage/<rootfs storage>`       | `Datastore.AllocateSpace`                                                                                         |
| `/storage/<template storage>`     | `Datastore.AllocateTemplate`, `Datastore.Audit`                                                                   |
| `/vms/<vmid>` (or `/vms`)         | `VM.Allocate`, `VM.Config.Disk`, `VM.Config.Memory`, `VM.Config.CPU`, `VM.Config.Network`, `VM.Config.Options`, `VM.PowerMgmt`, `VM.Audit` |

### Configure

1. Copy the example env file and edit it:

   ```sh
   cd deploy/lxc
   cp proxmox-create.env.example proxmox-create.env
   $EDITOR proxmox-create.env
   ```

   At minimum you must set:

   - `PROXMOX_URL` — e.g. `https://pve.lan:8006`
   - `PROXMOX_TOKEN` — full token string `USER@REALM!TOKENID=SECRET`
   - `PROXMOX_NODE` — node name (e.g. `pve`)
   - `LXC_VMID` — must NOT already exist
   - `LXC_HOSTNAME` — e.g. `mikrotik-nms`
   - One of: `LXC_SSH_PUBKEY` (+ `LXC_SSH_PRIVKEY`) **or** `LXC_PASSWORD`

   Sensible defaults are provided for storage (`local-lvm`, `local`),
   bridge (`vmbr0`), DHCP networking, 2 cores / 1 GB RAM / 8 GB disk, and
   the `debian-13-standard` template.

2. `proxmox-create.env` is gitignored — your secrets stay on disk only.

### Run

```sh
./proxmox-create.sh
```

What it does, step by step:

1. **Verifies** the API token by reading node status.
2. **Refuses** to continue if the requested VMID already exists.
3. **Finds or downloads** a `debian-13-standard` template into your template
   storage. Download polls the storage listing until the template appears
   (up to 10 minutes).
4. **Creates the LXC** with `unprivileged=1`, `features=nesting=1`,
   `onboot=1`, `start=1`, the chosen network/storage/resources, and your
   SSH key (or root password) injected.
5. **Waits** for the LXC to enter `running` state and `eth0` to acquire an
   IPv4 address.
6. **Waits** for SSH on `:22`.
7. **SSHes in as root**, runs `apt-get install git`, clones the repo into
   `/opt/src/mikrotik-nms`, then runs
   `deploy/lxc/install.sh $NMS_INSTALL_FLAGS`.
8. Prints a summary with the LXC IP, the URL to open, and a one-liner for
   updating the deployment later.

The script is **not** idempotent on the create step — re-running with the
same VMID errors out. To redeploy app code into an existing LXC, SSH into it
directly and re-run `deploy/lxc/install.sh --skip-deps`.

### Common customisations

| Want to…                                  | Set in `proxmox-create.env`                                  |
|-------------------------------------------|--------------------------------------------------------------|
| Static IP instead of DHCP                 | `LXC_NETWORK=ip=192.0.2.50/24,gw=192.0.2.1`            |
| Different Proxmox node                    | `PROXMOX_NODE=pve02`                                         |
| Bigger container                          | `LXC_CORES=4`, `LXC_MEMORY=2048`, `LXC_DISK_GB=16`           |
| Different storage                         | `LXC_STORAGE_ROOTFS=ssd-zfs`                                 |
| Self-signed Proxmox cert                  | `PROXMOX_VERIFY_TLS=0`                                       |
| Deploy from a fork / branch               | `NMS_REPO_URL=...`, `NMS_REPO_REF=feat/whatever`             |
| Hostname + auto Let's Encrypt instead of plain HTTP | `NMS_INSTALL_FLAGS=--hostname nms.example.com`     |

### Security notes

- The Proxmox API token and (if used) the LXC root password are read from
  `proxmox-create.env`. Keep that file readable only by your user
  (`chmod 600 proxmox-create.env`) and rotate the token after deployment if
  it's a one-shot.
- Prefer `LXC_SSH_PUBKEY` over `LXC_PASSWORD` — keys aren't visible in
  process listings, password auth requires `sshpass` which exposes the
  password via `argv` on the host running the script.
- The script never echoes the token or password to stdout/stderr.

---

## Path B — manual / `pct create`

Use this when you don't want the script to talk to Proxmox, or when you've
already got a Debian 13 LXC ready.

### Proxmox LXC prerequisites

1. **Create the container** on the Proxmox host:

   ```sh
   pct create <vmid> local:vztmpl/debian-13-standard_<...>.tar.zst \
       --hostname mikrotik-nms \
       --cores 2 --memory 1024 --swap 512 \
       --rootfs local-lvm:8 \
       --net0 name=eth0,bridge=vmbr0,ip=dhcp \
       --features nesting=1 \
       --unprivileged 1 \
       --onboot 1
   pct start <vmid>
   ```

   `nesting=1` is needed so systemd inside the container can apply the
   hardening directives in the unit files. An unprivileged container is fine.

2. **Install git inside the container** and clone the repo:

   ```sh
   pct exec <vmid> -- bash -c 'apt-get update && apt-get install -y git'
   pct exec <vmid> -- git clone https://github.com/c0de-ch/mikrotik-nms.git /opt/src/mikrotik-nms
   ```

### Run the installer

From inside the LXC (as root):

```sh
cd /opt/src/mikrotik-nms
./deploy/lxc/install.sh --hostname nms.example.com
```

### Common invocations

```sh
# Plain HTTP on the LXC's primary IP (no hostname, no TLS)
./deploy/lxc/install.sh --no-tls

# Public hostname with auto Let's Encrypt (needs ports 80+443 reachable)
./deploy/lxc/install.sh --hostname nms.example.com

# Internal hostname with self-signed TLS (Caddy "tls internal")
./deploy/lxc/install.sh --hostname nms.lan
# then edit /etc/caddy/Caddyfile and add:  tls internal

# Force a specific public URL (e.g. behind another reverse proxy)
./deploy/lxc/install.sh --no-tls --public-url http://192.0.2.50

# Skip apt installs (when re-running after editing env or rebuilding)
./deploy/lxc/install.sh --skip-deps
```

The installer is idempotent — re-run it any time you pull new code or change
the env file. It will rebuild the binaries and restart the services.

## Configuration

All runtime config lives in `/etc/mikrotik-nms/env`. The installer generates
sensible defaults including a random `MIKROTIK_NMS_JWT_SECRET` and
`MIKROTIK_NMS_ENCRYPTION_KEY`.

> ⚠️ `NEXT_PUBLIC_API_URL` and `NEXT_PUBLIC_WS_URL` are **baked into the
> frontend bundle at build time**. **Leave them empty** (the default unless
> you pass `--public-url`): the frontend then talks to `/api/*` on whatever
> hostname the site was loaded from, and Caddy proxies it to the backend —
> so the site works via IP, internal DNS, or any public name. Only set an
> absolute URL when the API genuinely lives on a different origin, and
> re-run `install.sh` after changing it to rebuild the frontend. A stale
> baked hostname makes every page fail with "Failed to fetch".

## First-run setup

Open the public URL in a browser and create the initial admin user via the
setup screen. This calls `POST /api/v1/auth/setup` which only succeeds when
the database has zero users.

## Operations

```sh
# Status
systemctl status mikrotik-nms-backend mikrotik-nms-frontend caddy

# Logs (live)
journalctl -u mikrotik-nms-backend -f
journalctl -u mikrotik-nms-frontend -f
journalctl -u caddy -f

# Restart after editing /etc/mikrotik-nms/env (backend only — frontend
# NEXT_PUBLIC_* vars are baked at build time, see warning above)
systemctl restart mikrotik-nms-backend

# Backup the database
sqlite3 /var/lib/mikrotik-nms/mikrotik-nms.db ".backup '/root/nms-$(date +%F).db'"
```

## Updating to a new version

```sh
cd /opt/src/mikrotik-nms
git pull
./deploy/lxc/install.sh --skip-deps
```

## Continuous deploy via a self-hosted GitHub Actions runner

Two options ship in this repo for letting GitHub trigger a deploy:

| Mechanism | Surface | Path |
|---|---|---|
| **Webhook agent** (existing) | Tiny Go HTTP daemon, HMAC-verified push events | [`deploy/webhook-agent/`](../webhook-agent/) |
| **Self-hosted Actions runner** (this section) | Full GitHub Actions executor with workflow steps | [`deploy/lxc/runner-install.sh`](runner-install.sh) + [`.github/workflows/deploy-lxc.yml`](../../.github/workflows/deploy-lxc.yml) |

Pick one — they are independent. The webhook agent is the smallest possible
attack surface; the self-hosted runner gives you the full GitHub Actions
context (logs in the Actions tab, `workflow_dispatch` button, ref selection,
job timeouts, etc.).

### Self-hosted runner setup

1. **Get a registration token** at
   `https://github.com/<owner>/<repo>/settings/actions/runners/new`. Tokens
   are one-shot and expire ~1h after issue.

2. **Run the installer inside the LXC** (after the main `install.sh` has
   already provisioned the NMS — the runner needs the systemd units to
   exist so the deploy can restart them):

   ```sh
   sudo /opt/src/mikrotik-nms/deploy/lxc/runner-install.sh \
       --repo c0de-ch/mikrotik-nms \
       --token AAA...your-registration-token...
   ```

   What it does:
   - Creates user `gh-runner` (system, no shell).
   - Downloads `actions/runner` v2.319.1 to `/home/gh-runner/runner`.
   - Registers the runner with labels `self-hosted,linux,mikrotik-nms`.
   - Installs as a systemd service (`actions.runner.<repo>.<name>.service`).
   - Drops `runner-deploy.sh` at `/usr/local/sbin/mikrotik-nms-deploy`.
   - Adds a **narrow** sudoers fragment (`/etc/sudoers.d/mikrotik-nms-runner`)
     letting `gh-runner` invoke that one wrapper, nothing else.

3. **Verify the runner is online** at
   `Settings → Actions → Runners`. The workflow `.github/workflows/deploy-lxc.yml`
   ships in this repo and triggers on push to `main` (path-filtered to
   `backend/**`, `frontend/**`, `deploy/lxc/**`) plus on-demand via
   `workflow_dispatch`.

4. **Optional — disable auto-deploy on push** by setting the repo variable
   `DEPLOY_ON_PUSH=false` (Settings → Secrets and variables → Actions →
   Variables). The `workflow_dispatch` trigger keeps working.

### What runs during a deploy

The deploy wrapper at `/usr/local/sbin/mikrotik-nms-deploy`:

1. Validates that the workspace path is under an allowed runner root and
   contains `backend/go.mod`, `frontend/package.json`, and the install
   script — refusing to run otherwise.
2. Calls `install.sh --skip-deps --source $WORKSPACE`. The installer
   rebuilds the backend binary and the Next.js frontend and restarts the
   systemd services.
3. Asserts that `mikrotik-nms-backend`, `mikrotik-nms-frontend` and
   `caddy` are `is-active` and that `GET /api/v1/health` returns 200.

Logs are visible both in the Actions tab and via
`journalctl -u 'actions.runner.*' -f` on the LXC.

### Hardening notes

- The runner runs as an unprivileged user. The only sudo grant is to
  `/usr/local/sbin/mikrotik-nms-deploy`, NOPASSWD, with no env passthrough.
- The deploy wrapper refuses any workspace path outside the runner's
  expected work tree, so a malicious workflow that tampers with
  `$GITHUB_WORKSPACE` cannot redirect the installer to an attacker-supplied
  source.
- Repo Settings → Actions → "Require approval for first-time contributors"
  is strongly recommended so a malicious PR from an outside fork cannot
  trigger the workflow.

### Re-registering or removing the runner

```sh
# stop + uninstall the systemd service
sudo /home/gh-runner/runner/svc.sh stop
sudo /home/gh-runner/runner/svc.sh uninstall

# tell GitHub to forget the runner (requires a fresh removal token)
sudo -u gh-runner /home/gh-runner/runner/config.sh remove --token AAA...

# remove the sudoers grant + wrapper
sudo rm -f /etc/sudoers.d/mikrotik-nms-runner /usr/local/sbin/mikrotik-nms-deploy
```

## Uninstall

```sh
systemctl disable --now mikrotik-nms-backend mikrotik-nms-frontend
rm -f /etc/systemd/system/mikrotik-nms-*.service
systemctl daemon-reload
rm -rf /opt/mikrotik-nms /var/lib/mikrotik-nms /etc/mikrotik-nms
userdel mikrotik-nms
# (Caddy/Go/Node remain installed; remove with apt if no longer needed.)
```
