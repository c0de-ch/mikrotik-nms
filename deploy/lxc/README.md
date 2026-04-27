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
> frontend bundle at build time**. If you change them, re-run `install.sh`
> to rebuild the frontend.

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

## Uninstall

```sh
systemctl disable --now mikrotik-nms-backend mikrotik-nms-frontend
rm -f /etc/systemd/system/mikrotik-nms-*.service
systemctl daemon-reload
rm -rf /opt/mikrotik-nms /var/lib/mikrotik-nms /etc/mikrotik-nms
userdel mikrotik-nms
# (Caddy/Go/Node remain installed; remove with apt if no longer needed.)
```
