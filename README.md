# MikroTik NMS

A web-based network management system for MikroTik devices. Provides real-time topology visualization, traffic monitoring, firmware management, and automatic device discovery via the MikroTik Neighbor Discovery Protocol (MNDP).

![MikroTik NMS Dashboard](docs/screenshot.png)
<!-- Replace the path above with an actual screenshot -->

## Features

- **Topology Map** -- interactive network topology powered by Cytoscape.js
- **Device Discovery** -- automatic scanning via MNDP (UDP 5678)
- **Traffic Monitoring** -- real-time interface bandwidth graphs with Recharts
- **Firmware Management** -- view and upgrade RouterOS across your fleet
- **WebSocket Updates** -- live device health, topology changes, and traffic data
- **Role-based Access** -- JWT authentication with admin and viewer roles
- **First-run Setup** -- guided admin account creation on initial launch
- **SQLite Storage** -- single-file database in WAL mode, zero external dependencies

## Quick Start with Docker Compose

```bash
# Clone the repository
git clone https://github.com/<owner>/mikrotik-c0de.git
cd mikrotik-c0de

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

| Variable | Required | Default | Description |
|---|---|---|---|
| `JWT_SECRET` | Yes | -- | Secret used to sign JWT tokens (32+ chars recommended) |
| `ENCRYPTION_KEY` | No | -- | AES key for encrypting device passwords at rest |
| `DEFAULT_ROS_USER` | No | `admin` | Default RouterOS username for discovered devices |
| `DEFAULT_ROS_PASS` | No | -- | Default RouterOS password for discovered devices |
| `NEXT_PUBLIC_API_URL` | No | `http://localhost:8080` | Backend API URL seen by the browser |
| `NEXT_PUBLIC_WS_URL` | No | `ws://localhost:8080` | Backend WebSocket URL seen by the browser |
| `MIKROTIK_NMS_DB_PATH` | No | `mikrotik-nms.db` | Path to the SQLite database file |
| `MIKROTIK_NMS_LISTEN` | No | `:8080` | Address and port the backend binds to |

See [`.env.example`](.env.example) for a ready-to-copy template.

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

Edit `secret.yaml` to supply your `JWT_SECRET` and any other credentials before applying. Adjust the `Ingress` resource to match your domain and TLS setup.

## Tech Stack

| Layer | Technology |
|---|---|
| Backend | Go, chi router, SQLite (WAL mode) |
| Frontend | Next.js 15, React, shadcn/ui, Cytoscape.js, Recharts |
| Auth | JWT (access + refresh), admin/viewer roles |
| Real-time | WebSocket |
| Discovery | MNDP (UDP 5678) |
| Containers | Docker, Docker Compose |
| Orchestration | Kubernetes |

## License

This project is licensed under the [MIT License](LICENSE).
