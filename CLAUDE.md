# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

**MikroTik NMS** — a web-based network management tool for MikroTik RouterOS devices. Provides network topology visualization, real-time traffic monitoring, device health overview, and mass firmware management. Read-only monitoring with WinBox deep linking (`winbox://`) for device configuration. Full user management with admin/viewer roles.

## Build / Test / Lint Commands

- **Build all:** `make build` (outputs `bin/mikrotik-nms` + frontend `.next`)
- **Build backend only:** `make build-backend` or `cd backend && go build ./cmd/mikrotik-nms/`
- **Build frontend only:** `make build-frontend` or `cd frontend && npm run build`
- **Run backend (dev):** `make dev-backend` (requires `MIKROTIK_NMS_JWT_SECRET` env var)
- **Run frontend (dev):** `make dev-frontend`
- **Test:** `make test` or `cd backend && go test ./...`
- **Test single package:** `cd backend && go test -v ./internal/topology/ -run TestBuildLinks`
- **Lint:** `make lint` (runs `go vet` + `npm run lint`)
- **Docker:** `docker compose build` then `docker compose up -d`
- **K8s:** `kubectl apply -f deploy/k8s/` (edit `secret.yaml` and `ingress.yaml` first)
- **Dev (both):** `./scripts/dev.sh` starts backend + frontend concurrently

## Architecture

Monorepo with Go backend + Next.js frontend communicating via REST API and WebSocket.

### Backend (`backend/`)

Go with `chi` router, SQLite (WAL mode) via `modernc.org/sqlite`, no ORM (raw SQL in `internal/database/queries/`).

- **`cmd/mikrotik-nms/main.go`** — entrypoint, wires config → DB → WebSocket hub → poller manager → HTTP server
- **`internal/config/`** — env-based configuration (`MIKROTIK_NMS_*` prefix)
- **`internal/database/`** — SQLite setup, goose migrations (001_init.sql, 002_upgrade_jobs.sql), query functions. 9 tables: `users`, `devices`, `interfaces`, `neighbors`, `links`, `traffic_samples`, `firmware_status`, `upgrade_jobs`, `upgrade_job_devices`
- **`internal/auth/`** — JWT (access 15min + refresh 7d), bcrypt passwords, chi middleware (`RequireAuth`, `RequireRole`)
- **`internal/routeros/`** — RouterOS API client pool wrapping `go-routeros/routeros/v3`. Functions for `/system/resource`, `/ip/neighbor`, `/interface/monitor-traffic`, `/system/package/update`
- **`internal/poller/manager.go`** — orchestrates background polling goroutines:
  - Health: 30s, polls `/system/resource` for all devices
  - Topology: 60s, polls `/ip/neighbor`, runs topology builder, broadcasts graph via WS
  - Traffic: on-demand via `TrafficManager` — starts/stops per-interface 1s polling when WS clients subscribe to `traffic.*` topics
  - Firmware: 6h, checks for RouterOS updates
  - Retention: 1h, deletes old traffic samples and stale neighbors
- **`internal/poller/firmware.go`** — `UpgradeExecutor` runs async firmware upgrade jobs with per-device progress tracking (download → install → reboot → verify), broadcasting status via WS `upgrade.progress.<jobId>`
- **`internal/topology/builder.go`** — builds deduplicated bidirectional link graph from raw neighbor data. Resolution priority: MAC → IP → identity. Canonical link ordering `(min_id, max_id)` prevents duplicates.
- **`internal/ws/`** — topic-based WebSocket pub/sub hub. Topics: `device.health`, `topology.update`, `traffic.<id>.<iface>`, `firmware.update`, `upgrade.progress.<jobId>`
- **`internal/api/`** — chi router, REST endpoints under `/api/v1/`, WebSocket at `/api/v1/ws`

### Frontend (`frontend/`)

Next.js 15 (App Router) + shadcn/ui (base-ui, not Radix) + Tailwind CSS v4.

- **Route group `(authenticated)/`** — protected layout with sidebar, redirects to `/login` if no token
- **Pages:** dashboard, topology (Cytoscape.js), devices (data table + CRUD), device detail (`/devices/[id]` — tabs for interfaces/neighbors, CPU/mem gauges), traffic (Recharts area charts with live WS updates), firmware (bulk upgrade with real-time progress via WS), users (admin)
- **`src/lib/api.ts`** — typed API client for all REST endpoints
- **`src/lib/ws.ts`** — WebSocket client with auto-reconnect and topic subscription
- **`src/context/auth.tsx`** — auth state management (login, setup, refresh, logout)
- **`src/hooks/use-websocket.ts`** — React hook for subscribing to WS topics with shared connection

### Key patterns

- shadcn/ui in this project uses **base-ui** (not Radix). Use `render={<Component />}` instead of `asChild` for composition.
- Device credentials stored in SQLite `devices.password_enc` field. TODO: encrypt with `MIKROTIK_NMS_ENCRYPTION_KEY`.
- Topology links are **derived** from raw neighbor data in the `neighbors` table, stored in `links` table. Rebuilt on each topology poll cycle.
- Traffic monitoring is on-demand: streaming starts when a WebSocket client subscribes to a `traffic.*` topic.
- First-run: `POST /api/v1/auth/setup` creates the initial admin user (only works when 0 users exist).
