# MikroTik NMS — LXC deploy

Native (no Docker) install for a Proxmox LXC container running **Debian 13
(trixie)**. The installer compiles the Go backend, builds the Next.js
standalone frontend, and registers both as systemd units behind a Caddy
reverse proxy.

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

## Proxmox LXC prerequisites

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
   pct exec <vmid> -- git clone https://github.com/c0de-ch/mikrotik-c0de.git /opt/src/mikrotik-c0de
   ```

## Run the installer

From inside the LXC (as root):

```sh
cd /opt/src/mikrotik-c0de
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
cd /opt/src/mikrotik-c0de
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
