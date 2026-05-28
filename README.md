# MikroTik NMS

A web-based network management system for MikroTik devices. Provides real-time topology visualization, traffic monitoring, firmware management, automatic device discovery via the MikroTik Neighbor Discovery Protocol (MNDP), and L2 loop detection via bridge/STP polling.

![MikroTik NMS Dashboard](docs/screenshot.png)
<!-- Replace the path above with an actual screenshot -->

## Documentation

| Doc | What's in it |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | System overview, components, polling model, WebSocket topics, data flow, schema, design tradeoffs |
| [docs/API.md](docs/API.md) | Complete REST + WebSocket API reference (auth, every endpoint, topic catalog, examples) |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | Docker, Kubernetes, LXC/bare-metal, continuous deploy, config reference, backups, hardening |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | Local setup, build/test/lint, and how-tos for adding migrations, endpoints, pollers, WS topics, and pages |
| [IMPROVEMENTS.md](IMPROVEMENTS.md) | Prioritized review backlog (security, performance, correctness, testing, UX) |

## Features

- **Topology Map** — device/link view built from MNDP neighbor discovery (card-grid layout)
- **Device Discovery** — automatic scanning via MNDP (UDP 5678)
- **Traffic Monitoring** — real-time interface bandwidth graphs with Recharts
- **Firmware Management** — view and upgrade RouterOS across your fleet
- **Network Health** — bridge/STP poller with L2 loop detection (`stp_disabled`, `tcn_storm`, `loop_detected`, `mac_flap`, `bpdu_on_edge`) plus per-interface port monitoring (`port_disabled`, `port_link_down`, `port_link_flap`)
- **WiFi Tracking** — per-client AP positions, roam history and live join/leave events from CAPsMAN/WiFi logs
- **Client Discovery** — ARP/DHCP/CAPsMAN scans with optional Kea DHCP Control Agent integration for IP/hostname enrichment
- **WebSocket Updates** — live device health, topology, traffic, wifi, and `network.health.event` topics
- **Role-based Access** — JWT authentication with admin and viewer roles
- **First-run Setup** — guided admin account creation on initial launch
- **SQLite Storage** — single-file database in WAL mode, zero external dependencies

## Quick Start with Docker Compose

```bash
# Clone the repository
git clone https://github.com/c0de-ch/mikrotik-nms.git
cd mikrotik-nms

# Copy the example environment file and edit it
cp .env.example .env
# At minimum, set a strong JWT_SECRET (32+ characters)

# Build and start
docker compose up -d
```

The frontend is available at **http://localhost:3000** and the backend API at **http://localhost:8080**. On first launch you will be prompted to create an admin account.

## Quick Start for Development

### Prerequisites

| Tool | Version |
|------|---------|
| Go | 1.25+ |
| Node.js | 22+ |

### Backend

```bash
cd backend
MIKROTIK_NMS_JWT_SECRET=dev-secret \
MIKROTIK_NMS_DB_PATH=mikrotik-nms.db \
  go run ./cmd/mikrotik-nms/
```

The API server starts on **http://localhost:8080**.

### Frontend

```bash
cd frontend
npm ci
NEXT_PUBLIC_API_URL=http://localhost:8080 \
NEXT_PUBLIC_WS_URL=ws://localhost:8080 \
  npm run dev
```

The dev server starts on **http://localhost:3000**.

> **Tip:** You can also run `make dev-backend` and `make dev-frontend` in separate terminals, or use `make dev` to start both.

## Environment Variables

The backend is configured entirely via `MIKROTIK_NMS_*` environment variables. The Docker Compose file maps a few unprefixed convenience names (`JWT_SECRET`, `ENCRYPTION_KEY`, `DEFAULT_ROS_USER`, `DEFAULT_ROS_PASS`) onto their prefixed equivalents so the `.env` file stays compact — see [`.env.example`](.env.example) and [`docker-compose.yml`](docker-compose.yml).

### Backend

| Variable | Required | Default | Description |
|---|---|---|---|
| `MIKROTIK_NMS_JWT_SECRET` | Yes | — | Secret used to sign JWT tokens (32+ chars recommended) |
| `MIKROTIK_NMS_LISTEN` | No | `:8080` | Address and port the backend binds to |
| `MIKROTIK_NMS_DB_PATH` | No | `mikrotik-nms.db` | Path to the SQLite database file |
| `MIKROTIK_NMS_ENCRYPTION_KEY` | No | — | AES key for encrypting device passwords at rest (AES-256-GCM). Set in production; existing rows migrate on startup. Unset = plaintext + redacted from backups |
| `MIKROTIK_NMS_ALLOWED_ORIGINS` | No | — | Comma-separated CORS / WebSocket origin allow-list. Unset = accept any origin (warns) |
| `MIKROTIK_NMS_ROS_TLS_VERIFY` | No | `false` | Verify RouterOS API-TLS device certificates (off by default; self-signed certs) |
| `MIKROTIK_NMS_HEALTH_INTERVAL` | No | `30s` | How often to poll `/system/resource` |
| `MIKROTIK_NMS_TOPOLOGY_INTERVAL` | No | `60s` | How often to poll `/ip/neighbor` and rebuild the link graph |
| `MIKROTIK_NMS_FIRMWARE_INTERVAL` | No | `6h` | How often to check for RouterOS firmware updates |
| `MIKROTIK_NMS_NETWORK_HEALTH_INTERVAL` | No | `60s` | How often to poll bridge/STP state and parse bridge logs |
| `MIKROTIK_NMS_RETENTION_INTERVAL` | No | `1h` | How often to sweep old samples / events |
| `MIKROTIK_NMS_RETENTION_DAYS` | No | `7` | How long to keep traffic samples, wifi/client history, and loop events |
| `MIKROTIK_NMS_DEFAULT_ROS_USER` | No | `admin` | Default RouterOS username for discovered devices |
| `MIKROTIK_NMS_DEFAULT_ROS_PASS` | No | — | Default RouterOS password for discovered devices |
| `MIKROTIK_NMS_DEFAULT_ROS_PORT` | No | `8728` | Default RouterOS API port |
| `MIKROTIK_NMS_DEFAULT_ROS_TLS` | No | `false` | Default to TLS (`8729`) for new devices |

WiFi tracking and client discovery intervals, the Kea DHCP Control Agent URL, and DNS resolvers for client lookups are configured via the **Settings** page in the UI (stored in the `app_settings` table) rather than env vars, so they can be changed without a restart.

### Frontend (build-time, baked into the bundle)

| Variable | Default | Description |
|---|---|---|
| `NEXT_PUBLIC_API_URL` | `http://localhost:8080` | Backend API URL seen by the browser |
| `NEXT_PUBLIC_WS_URL` | `ws://localhost:8080` | Backend WebSocket URL seen by the browser |

Because Next.js inlines `NEXT_PUBLIC_*` at build time, changing these values requires rebuilding the frontend image (Compose / K8s) or re-running `install.sh` (LXC).

## Native Linux Install (LXC / VM / bare metal)

For an LXC container or VM running Debian 13 (trixie), `deploy/lxc/install.sh` builds the backend, builds the standalone Next.js frontend, and registers them as systemd units behind a Caddy reverse proxy:

```bash
sudo ./deploy/lxc/install.sh --hostname nms.example.com
```

For a one-shot Proxmox API bootstrap that creates the LXC and runs the installer in a single step, see [`deploy/lxc/README.md`](deploy/lxc/README.md).

### Continuous deploy from GitHub

Two ship in the box, pick whichever fits:

- **Self-hosted Actions runner** ([`deploy/lxc/runner-install.sh`](deploy/lxc/runner-install.sh)) — registers a private GitHub Actions runner inside the LXC. The provided [`.github/workflows/deploy-lxc.yml`](.github/workflows/deploy-lxc.yml) workflow then auto-deploys on push to `main` (and via `workflow_dispatch`). The runner has a narrow sudoers grant to one validated wrapper and authenticates to GitHub with its own registration token.
- **HMAC webhook agent** ([`deploy/webhook-agent/`](deploy/webhook-agent/)) — a tiny Go HTTP daemon that verifies signed GitHub push events and runs a configurable deploy command. Smallest possible attack surface; no Actions runtime needed.

Both are documented in [`deploy/lxc/README.md`](deploy/lxc/README.md).

## Kubernetes Deployment

Production-ready Kubernetes manifests are provided in the [`deploy/k8s/`](deploy/k8s/) directory:

```
deploy/k8s/
  namespace.yaml
  configmap.yaml
  secret.yaml
  pvc.yaml
  deployment.yaml
  service.yaml
  ingress.yaml
```

Apply them in order:

```bash
kubectl apply -f deploy/k8s/namespace.yaml
kubectl apply -f deploy/k8s/
```

Before applying:

- Edit `secret.yaml` to supply your `MIKROTIK_NMS_JWT_SECRET` (and `MIKROTIK_NMS_ENCRYPTION_KEY`, `MIKROTIK_NMS_DEFAULT_ROS_PASS` if used).
- Adjust `ingress.yaml` to match your domain and TLS setup.
- The deployment pulls images from `ghcr.io/c0de-ch/mikrotik-nms/{backend,frontend}:latest` (published by the `Build & Push Docker Images` workflow). If you fork, change those references to your fork's GHCR path.

The backend `Deployment` is fixed at `replicas: 1` because SQLite does not support multiple writers. Liveness/readiness probes hit `/api/v1/health` and a 1 GiB PVC backs `/data`.

## Container Images & GitHub Workflow

`.github/workflows/docker.yml` builds multi-arch (`linux/amd64`, `linux/arm64`) images on push to `main`/`master` and on `v*` tags, publishing to GitHub Container Registry:

| Image | Path |
|---|---|
| Backend | `ghcr.io/c0de-ch/mikrotik-nms/backend` |
| Frontend | `ghcr.io/c0de-ch/mikrotik-nms/frontend` |

Tags applied per build:

- `latest` (only on the default branch)
- `v1.2.3` and `v1.2` (on semver tag pushes)
- `sha-<short>` (on every push)

The workflow uses Buildx with GitHub Actions cache (`type=gha`) so subsequent builds are incremental.

## Deploy on MikroTik CHR (Container)

RouterOS v7.4+ supports running Docker containers directly on MikroTik devices via the **Container** package. This lets you run MikroTik NMS on a CHR or hardware router with enough resources.

### Prerequisites

- RouterOS v7.4+ with the **container** package installed
- At least 256MB RAM and 512MB disk free
- A VETH interface and bridge for container networking

### Setup

1. **Enable containers** on the router:

```routeros
/system/device-mode/update container=yes
# Router will reboot
```

2. **Create a VETH interface and bridge** for the container:

```routeros
/interface/veth/add name=veth-nms address=172.17.0.2/24 gateway=172.17.0.1
/interface/bridge/add name=br-containers
/ip/address/add address=172.17.0.1/24 interface=br-containers
/interface/bridge/port/add bridge=br-containers interface=veth-nms
```

3. **Add environment variables**:

```routeros
/container/envs/add name=nms-env key=MIKROTIK_NMS_JWT_SECRET value="your-secret-here"
/container/envs/add name=nms-env key=MIKROTIK_NMS_DB_PATH value="/data/mikrotik-nms.db"
/container/envs/add name=nms-env key=MIKROTIK_NMS_LISTEN value=":8080"
```

4. **Create mount point** for persistent data:

```routeros
/container/mounts/add name=nms-data src=disk1/nms-data dst=/data
```

5. **Pull and create the backend container**:

```routeros
/container/add remote-image=ghcr.io/c0de-ch/mikrotik-nms/backend:latest \
  interface=veth-nms envlist=nms-env mounts=nms-data \
  hostname=mikrotik-nms start-on-boot=yes
```

6. **Start the container**:

```routeros
/container/start 0
```

The backend API will be available at `http://172.17.0.2:8080`. You can add NAT rules or a web proxy to expose it on the router's management IP.

> **Note:** For the frontend, it's recommended to run it on a separate host or VM since Node.js containers need more resources. Alternatively, build the frontend as a static export and serve it from any web server.

## Tech Stack

| Layer | Technology |
|---|---|
| Backend | Go, chi router, SQLite (WAL mode) |
| Frontend | Next.js 16, React 19, shadcn/ui (base-ui), Recharts |
| Auth | JWT (access + refresh), admin/viewer roles |
| Real-time | WebSocket |
| Discovery | MNDP (UDP 5678) |
| Containers | Docker, Docker Compose |
| Orchestration | Kubernetes |

## License

This project is licensed under the [MIT License](LICENSE).
