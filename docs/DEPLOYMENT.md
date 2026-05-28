# Deployment & Operations Guide

This guide covers every supported way to run **MikroTik NMS** — Docker Compose, production Docker images, Kubernetes, and native LXC / bare-metal — plus continuous deployment, configuration, backups, upgrades, and a security hardening checklist.

The stack is two services:

```
                 ┌──────────────────────────────────────────┐
   browser ────► │  frontend  (Next.js standalone, :3000)    │
                 └──────────────────┬───────────────────────┘
                                    │  REST + WebSocket
                 ┌──────────────────▼───────────────────────┐
                 │  backend  (Go, chi, :8080)                │
                 │   • SQLite (WAL) at MIKROTIK_NMS_DB_PATH  │
                 │   • RouterOS API pollers                  │
                 └───────────────────────────────────────────┘
```

The backend owns all state in a single SQLite file. There is **no external database**.

---

## 1. Prerequisites

| Target | Needs |
|---|---|
| Docker Compose | Docker Engine + Compose v2 (`docker compose`) |
| Production Docker | A container runtime that can pull from `ghcr.io` |
| Kubernetes | A cluster with a default `StorageClass` (for the PVC) and an ingress controller (manifests assume the NGINX ingress) |
| LXC / bare-metal | Debian 13 (trixie) container or VM, run as root. The installer pulls Go, Node.js, and Caddy automatically |
| Development | Go 1.25+, Node.js 22+ |

Network: the backend must be able to reach your RouterOS devices on the API port (`8728` plaintext, `8729` TLS). MNDP discovery uses UDP `5678`.

---

## 2. Quick start (Docker Compose)

The fastest path. From a clone of the repo:

```bash
git clone https://github.com/c0de-ch/mikrotik-c0de.git
cd mikrotik-c0de

# Copy the example env file
cp .env.example .env

# Generate a real JWT secret (32+ random bytes) and write it into .env
JWT=$(openssl rand -hex 32)
sed -i "s/^JWT_SECRET=.*/JWT_SECRET=$JWT/" .env

# Build and start both services
docker compose up -d
```

Open **http://localhost:3000** and create the first admin account (the setup screen calls `POST /api/v1/auth/setup`, which only works while 0 users exist). The backend API is on **http://localhost:8080**.

`docker-compose.yml` reads a few **unprefixed** convenience names from `.env` and maps them onto the backend's `MIKROTIK_NMS_*` variables:

| `.env` name | Maps to | Notes |
|---|---|---|
| `JWT_SECRET` | `MIKROTIK_NMS_JWT_SECRET` | Required. Defaults to `change-me-in-production` if unset — **set it.** |
| `ENCRYPTION_KEY` | `MIKROTIK_NMS_ENCRYPTION_KEY` | Optional (see [§10](#10-security-hardening-checklist)) |
| `DEFAULT_ROS_USER` | `MIKROTIK_NMS_DEFAULT_ROS_USER` | Defaults to `admin` |
| `DEFAULT_ROS_PASS` | `MIKROTIK_NMS_DEFAULT_ROS_PASS` | Optional |

> The frontend's `NEXT_PUBLIC_API_URL` / `NEXT_PUBLIC_WS_URL` are **baked into the JS bundle at build time**. The default Compose values point at `http://backend:8080` / `ws://backend:8080` (the in-network service name). If you put a reverse proxy in front, change these in `docker-compose.yml` and rebuild the frontend image.

A development variant, [`docker-compose.dev.yml`](../docker-compose.dev.yml), bind-mounts the source and uses a throwaway `dev-secret-change-me` JWT — do not use it in production.

---

## 3. Production Docker

Pre-built multi-arch images (`linux/amd64`, `linux/arm64`) are published to GHCR by [`.github/workflows/docker.yml`](../.github/workflows/docker.yml) on push to `main`/`master` and on `v*` tags:

| Image | Registry path |
|---|---|
| Backend | `ghcr.io/c0de-ch/mikrotik-c0de/backend` |
| Frontend | `ghcr.io/c0de-ch/mikrotik-c0de/frontend` |

> The image path is derived from `ghcr.io/${{ github.repository }}/{backend,frontend}` in the workflow. If you fork, your images live under your fork's path; the upstream K8s manifests reference `ghcr.io/c0de-ch/mikrotik-nms/...`, so adjust accordingly.

Tags applied: `latest` (default branch only), `vX.Y.Z` + `vX.Y` (semver tags), and `sha-<short>` (every push).

| Service | Container port | Volume | Healthcheck |
|---|---|---|---|
| backend | `8080` | `/data` (SQLite DB lives here) | `wget --spider http://localhost:8080/api/v1/health`, every 30s |
| frontend | `3000` | — | none in Compose; `depends_on: backend healthy` |

The backend Dockerfile produces a static `CGO_ENABLED=0` binary on `alpine:3.21`. The frontend Dockerfile builds the Next.js **standalone** output and runs `node server.js`.

---

## 4. Kubernetes

Manifests live in [`deploy/k8s/`](../deploy/k8s/):

```
namespace.yaml   configmap.yaml   secret.yaml   pvc.yaml
deployment.yaml  service.yaml     ingress.yaml
```

**Edit these two first:**

1. **`secret.yaml`** — set `MIKROTIK_NMS_JWT_SECRET` (and `MIKROTIK_NMS_ENCRYPTION_KEY` / `MIKROTIK_NMS_DEFAULT_ROS_PASS` if you use them).
2. **`ingress.yaml`** — change the `host: nms.example.com` rule to your domain and wire up TLS. The ingress routes `/api` → backend `:8080` and `/` → frontend `:3000`, with the NGINX annotations needed for WebSocket upgrades and long-lived connections (`proxy-read-timeout: 3600`).

Apply (namespace first, then the rest):

```bash
kubectl apply -f deploy/k8s/namespace.yaml
kubectl apply -f deploy/k8s/
```

Key facts:

- The backend `Deployment` is **fixed at `replicas: 1`** — SQLite has a single writer. Do **not** scale it.
- Non-secret config (intervals, ports, RouterOS defaults) is in `configmap.yaml`; secrets in `secret.yaml`. Both are mounted via `envFrom`.
- Persistence: a **1 GiB `ReadWriteOnce` PVC** (`mikrotik-nms-data`) is mounted at `/data`; the DB path is `/data/mikrotik-nms.db`.
- Liveness and readiness probes hit `GET /api/v1/health`.
- The frontend `Deployment` sets `NEXT_PUBLIC_*` as runtime env — but because these are inlined at **build** time, the values only take effect if the image was built with them. To change the browser-facing URLs you must rebuild the frontend image, not just edit the manifest.

---

## 5. LXC / bare-metal

For a Debian 13 LXC or VM, [`deploy/lxc/install.sh`](../deploy/lxc/install.sh) installs Go, Node.js, and Caddy, builds both services, and registers them as systemd units behind a Caddy reverse proxy. Run it as root from a repo checkout:

```bash
sudo ./deploy/lxc/install.sh --hostname nms.example.com
```

What lands on disk:

| Path | Contents |
|---|---|
| `/opt/mikrotik-nms/bin/mikrotik-nms` | Backend binary |
| `/opt/mikrotik-nms/frontend/` | Next.js standalone server + assets |
| `/var/lib/mikrotik-nms/mikrotik-nms.db` | SQLite database |
| `/etc/mikrotik-nms/env` | Environment file (auto-generates a random JWT secret + encryption key on first run) |
| `/etc/systemd/system/mikrotik-nms-{backend,frontend}.service` | Hardened systemd units |
| `/etc/caddy/Caddyfile` | Reverse proxy config |

Both services run as the unprivileged `mikrotik-nms` user under a strict systemd hardening matrix (`NoNewPrivileges`, `ProtectSystem=strict`, `SystemCallFilter=@system-service`, etc.). The backend binds `127.0.0.1:8080`, the frontend `127.0.0.1:3000`, and Caddy fronts both.

### TLS options

| Flag | Behaviour |
|---|---|
| *(default, with `--hostname`)* | Automatic **Let's Encrypt** cert. Needs ports 80+443 reachable from the internet and public DNS for the hostname. |
| `--tls-internal` | Caddy issues a **self-signed** cert from its local CA. Requires `--hostname`. Good for internal-only hostnames with no public DNS. |
| `--no-tls` | Plain **HTTP on :80**. Use behind another reverse proxy or on a trusted LAN. |

If neither `--hostname` nor `--public-url` is given, the installer falls back to the LXC's primary IPv4 over plain HTTP.

The installer is **idempotent** — re-run it after `git pull` to rebuild and restart. Skip the apt phase with `--skip-deps`. Operations:

```bash
systemctl status mikrotik-nms-backend mikrotik-nms-frontend caddy
journalctl -u mikrotik-nms-backend -f
systemctl restart mikrotik-nms-backend          # after editing /etc/mikrotik-nms/env
```

> Editing `NEXT_PUBLIC_API_URL` / `NEXT_PUBLIC_WS_URL` requires re-running `install.sh` (a frontend rebuild), not just a restart.

A one-shot Proxmox API bootstrap ([`deploy/lxc/proxmox-create.sh`](../deploy/lxc/proxmox-create.sh)) and full operational notes are in [`deploy/lxc/README.md`](../deploy/lxc/README.md).

---

## 6. Continuous deployment

Two independent paths ship in the box. **Pick one.**

| Path | What it is | Use when… |
|---|---|---|
| **HMAC webhook agent** ([`deploy/webhook-agent/`](../deploy/webhook-agent/)) | A tiny Go daemon (`backend/cmd/deploy-agent`) that verifies signed GitHub `push` events and runs a configurable deploy command. | You want the smallest attack surface and no Actions runtime. GitHub holds only an HMAC secret; SSH keys / Proxmox tokens never leave your network. |
| **Self-hosted Actions runner** ([`deploy/lxc/runner-install.sh`](../deploy/lxc/runner-install.sh) + [`.github/workflows/deploy-lxc.yml`](../.github/workflows/deploy-lxc.yml)) | A private GitHub Actions runner inside the LXC; the workflow deploys on push to `main` and via `workflow_dispatch`. | You want the full Actions UX — logs in the Actions tab, a dispatch button, ref selection, job timeouts. |

### HMAC webhook agent

The daemon verifies `X-Hub-Signature-256` with a constant-time compare, caps the body at 5 MiB, de-dupes via `X-GitHub-Delivery`, and only acts on `push` events whose `repository.full_name` matches `ALLOWED_REPO` and whose `ref` matches `ALLOWED_REF` (default `refs/heads/main`). Deploys are serialized by a mutex and killed after `DEPLOY_TIMEOUT` (default 30m).

```bash
sudo ./deploy/webhook-agent/install.sh
sudo $EDITOR /etc/mikrotik-nms-deploy-agent/env    # set ALLOWED_REPO, note WEBHOOK_SECRET
sudo $EDITOR /etc/mikrotik-nms-deploy-agent/run.sh # set LXC_HOST / BRANCH
sudo systemctl start mikrotik-nms-deploy-agent
```

| Env var | Purpose | Default |
|---|---|---|
| `WEBHOOK_SECRET` | HMAC secret shared with the GitHub webbook (`openssl rand -hex 32`) | — (required) |
| `ALLOWED_REPO` | `owner/repo` allowed to trigger deploys | — (required) |
| `DEPLOY_COMMAND` | Shell command run via `/bin/sh -c` on accepted push | — (required) |
| `LISTEN` | Listen address | `127.0.0.1:9000` |
| `ALLOWED_REF` | Git ref to react to | `refs/heads/main` |
| `DEPLOY_TIMEOUT` | Max deploy wall-clock | `30m` |

The agent listens on loopback by default and expects you to terminate TLS upstream (Tailscale Funnel, Cloudflare Tunnel, Caddy, or WireGuard) before exposing `/webhook` to GitHub. The README covers each option, including a self-hosted WireGuard relay.

### Self-hosted Actions runner

Run the main `install.sh` **first** (the runner restarts those systemd units), then:

```bash
sudo /opt/src/mikrotik-c0de/deploy/lxc/runner-install.sh \
    --repo c0de-ch/mikrotik-c0de \
    --token AAA...registration-token...
```

This registers a runner labelled `self-hosted,linux,mikrotik-nms`, installs the deploy wrapper at `/usr/local/sbin/mikrotik-nms-deploy`, and adds a **narrow sudoers grant** (`/etc/sudoers.d/mikrotik-nms-runner`) letting `gh-runner` run only that one wrapper, NOPASSWD, no env passthrough. The wrapper ([`runner-deploy.sh`](../deploy/lxc/runner-deploy.sh)) validates that the workspace is under an allowed runner root and contains `backend/go.mod`, `frontend/package.json`, and the install script before calling `install.sh --skip-deps --source $WORKSPACE`, then asserts the three units are active and `/api/v1/health` returns 200. Set repo variable `DEPLOY_ON_PUSH=false` to disable the auto-on-push trigger while keeping `workflow_dispatch`.

---

## 7. Configuration reference

The backend is configured **entirely** via `MIKROTIK_NMS_*` environment variables, parsed in [`backend/internal/config/config.go`](../backend/internal/config/config.go). Only `MIKROTIK_NMS_JWT_SECRET` is required; the process exits if it is empty.

| Variable | Purpose | Default | Required? |
|---|---|---|---|
| `MIKROTIK_NMS_JWT_SECRET` | Secret used to sign JWTs (32+ chars recommended) | — | **Yes** |
| `MIKROTIK_NMS_LISTEN` | Backend bind address | `:8080` | No |
| `MIKROTIK_NMS_DB_PATH` | SQLite database file path | `mikrotik-nms.db` | No |
| `MIKROTIK_NMS_ENCRYPTION_KEY` | AES key for encrypting device passwords at rest (AES-256-GCM). Any non-empty value works; 32+ random bytes recommended. Existing plaintext rows are migrated on startup. Unset = plaintext + stripped from backups | — | No |
| `MIKROTIK_NMS_ALLOWED_ORIGINS` | Comma-separated CORS / WebSocket origin allow-list (e.g. `https://nms.example.com`). Unset = reflect any origin (logs a warning) | — | No |
| `MIKROTIK_NMS_ROS_TLS_VERIFY` | Verify RouterOS API-TLS device certificates. Off by default (self-signed certs) | `false` | No |
| `MIKROTIK_NMS_HEALTH_INTERVAL` | Liveness ping cadence | `30s` | No |
| `MIKROTIK_NMS_TOPOLOGY_INTERVAL` | `/ip/neighbor` poll + link-graph rebuild cadence | `60s` | No |
| `MIKROTIK_NMS_FIRMWARE_INTERVAL` | RouterOS update-check cadence | `6h` | No |
| `MIKROTIK_NMS_NETWORK_HEALTH_INTERVAL` | Bridge/STP + port-state poll cadence | `60s` | No |
| `MIKROTIK_NMS_RETENTION_INTERVAL` | Cleanup sweep cadence | `1h` | No |
| `MIKROTIK_NMS_RETENTION_DAYS` | Retention window for samples / events | `7` | No |
| `MIKROTIK_NMS_DEFAULT_ROS_USER` | Default RouterOS username for new devices | `admin` | No |
| `MIKROTIK_NMS_DEFAULT_ROS_PASS` | Default RouterOS password for new devices | — | No |
| `MIKROTIK_NMS_DEFAULT_ROS_PORT` | Default RouterOS API port | `8728` | No |
| `MIKROTIK_NMS_DEFAULT_ROS_TLS` | Default new devices to API-TLS (`8729`) | `false` | No |

Frontend (build-time only, inlined into the bundle):

| Variable | Purpose | Default |
|---|---|---|
| `NEXT_PUBLIC_API_URL` | Backend API URL the browser uses | `http://localhost:8080` |
| `NEXT_PUBLIC_WS_URL` | Backend WebSocket URL the browser uses | `ws://localhost:8080` |

### Restart required vs. runtime-tunable

- **Env-var pollers (restart required):** every `MIKROTIK_NMS_*` interval above (`HEALTH`, `TOPOLOGY`, `FIRMWARE`, `NETWORK_HEALTH`, `RETENTION`) is read once at startup. Changing them means editing the env/ConfigMap/Secret and restarting the backend.
- **Runtime-tunable via the Settings page (no restart):** settings stored in the `app_settings` table are picked up on the next poll cycle. These include the WiFi-tracking interval (`wifi_interval`), the client-discovery interval (`client_discovery_interval`), the Kea DHCP Control Agent URL, the OPNsense lease integration (`opnsense_*`), DNS resolvers for client lookups, the offline threshold (`offline_threshold_seconds`), the heavy info-refresh interval (`info_interval`), the TCN storm threshold (`tcn_storm_threshold`), and the port-monitoring filter/thresholds (`port_monitor_*`). Admins edit them on the **Settings** page; nothing in `.env` controls them.

---

## 8. TLS & secrets

- **Generate a real JWT secret:** `openssl rand -hex 32`. Set it via `JWT_SECRET` (Compose), `secret.yaml` (K8s), or the auto-generated `/etc/mikrotik-nms/env` (LXC). If the backend starts with the placeholder, rotate it — changing it invalidates all existing tokens (users must log in again).
- **TLS termination:** the Go backend speaks plain HTTP. Put TLS at the edge — Caddy (LXC, automatic Let's Encrypt or `tls internal`), the Kubernetes ingress, or your own reverse proxy. The LXC Caddyfile already sets HSTS, `X-Content-Type-Options: nosniff`, and a `Referrer-Policy`.
- **Device credentials:** stored in the `devices.password_enc` SQLite column. See the limitation in [§10](#10-security-hardening-checklist).
- **Deploy secrets** (HMAC secret, SSH keys, Proxmox tokens) stay on the agent/runner host and never transit GitHub.

---

## 9. SQLite backup, restore & migrations

The entire application state is one SQLite file (plus WAL/SHM sidecars).

| Deploy path | DB location |
|---|---|
| Docker Compose / production Docker | `mikrotik-data` named volume → `/data/mikrotik-nms.db` in the backend container |
| Kubernetes | `mikrotik-nms-data` PVC → `/data/mikrotik-nms.db` |
| LXC / bare-metal | `/var/lib/mikrotik-nms/mikrotik-nms.db` |

### Backup (online-safe)

Use SQLite's `.backup`, which is consistent even while the backend is running in WAL mode:

```bash
# LXC / bare-metal
sqlite3 /var/lib/mikrotik-nms/mikrotik-nms.db ".backup '/root/nms-$(date +%F).db'"

# Docker Compose (sqlite3 ships in the alpine backend image)
docker compose exec backend \
  sqlite3 /data/mikrotik-nms.db ".backup '/data/nms-$(date +%F).db'"

# Kubernetes
kubectl -n mikrotik-nms exec deploy/mikrotik-nms-backend -- \
  sqlite3 /data/mikrotik-nms.db ".backup '/data/nms-backup.db'"
```

Copy the resulting file off-box (`docker cp`, `kubectl cp`, `scp`).

### Restore

Stop the backend, replace the DB file (and remove stale `-wal` / `-shm` sidecars), then start:

```bash
systemctl stop mikrotik-nms-backend
cp /root/nms-2026-05-28.db /var/lib/mikrotik-nms/mikrotik-nms.db
rm -f /var/lib/mikrotik-nms/mikrotik-nms.db-wal /var/lib/mikrotik-nms/mikrotik-nms.db-shm
chown mikrotik-nms:mikrotik-nms /var/lib/mikrotik-nms/mikrotik-nms.db
systemctl start mikrotik-nms-backend
```

### Migrations

Schema migrations (`001_init.sql` … `013_loop_event_ack.sql`) are **embedded in the binary** and applied automatically with **goose** (`goose.Up`) on every startup. Upgrading is just: deploy the new image/binary and restart — the backend brings the schema forward. Migrations are forward-only; **take a backup before upgrading** so you can roll back by restoring the file if needed.

---

## 10. Security hardening checklist

MikroTik NMS is read-only monitoring intended for a trusted network. Most of the original hardening gaps are now addressed in the application; the items below note what to **configure** for a locked-down deployment.

**Now enforced by the app:**

- [x] **Device passwords encrypted at rest** with AES-256-GCM when `MIKROTIK_NMS_ENCRYPTION_KEY` is set (existing rows migrated on startup). **Set the key in production** — without it, passwords are stored plaintext (and stripped from backups).
- [x] **Backups/exports no longer leak cleartext passwords** — with a key, the export contains ciphertext; without one, `password_enc` is blanked.
- [x] **Auth endpoints are rate-limited** (per-IP, 10/min) against brute force.
- [x] **First-run setup is race-safe** (atomic insert-if-empty).
- [x] **Security headers** (`X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy`, `CSP: frame-ancestors 'none'`) are set by both the backend and the Next.js frontend.
- [x] **WebSocket token is redacted** from access logs.

**Configure for production:**

- [ ] **Set `MIKROTIK_NMS_ALLOWED_ORIGINS`** to your frontend origin(s). Until you do, CORS reflects any origin and the WebSocket accepts any origin (a startup warning is logged). Setting it enforces a strict allow-list for both. *(HIGH)*
- [ ] **Set `MIKROTIK_NMS_ENCRYPTION_KEY`** (see above). *(HIGH)*
- [ ] **Terminate TLS at the proxy** and consider `MIKROTIK_NMS_ROS_TLS_VERIFY=true` if your devices present trusted certs (RouterOS self-signed certs are not verified by default). *(MEDIUM)*
- [ ] **Treat backup files as secrets.** Even with redaction/encryption they contain inventory and bcrypt user hashes; store them encrypted and restrict distribution. *(MEDIUM)*

**Known residual gap (tracked in [../IMPROVEMENTS.md](../IMPROVEMENTS.md)):** the frontend still keeps tokens in `localStorage` (XSS-readable). The short-lived access token and `SameSite=Strict` httpOnly refresh cookie limit the blast radius; a strict CSP at your proxy and same-origin hosting are the recommended mitigations until tokens move out of JS reach.

Other confirmed-good properties: the device password field carries a `json:"-"` tag and is not serialized in `GET /devices`; the systemd units apply a strong hardening matrix; the deploy paths use constant-time HMAC verification and a narrowly-scoped sudoers grant.
