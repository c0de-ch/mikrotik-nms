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
- **`internal/database/`** — SQLite setup, goose migrations (`001_init.sql` … `014_traffic_retention_index.sql`), query functions. Tables: `users`, `devices`, `interfaces`, `neighbors`, `links`, `traffic_samples`, `firmware_status`, `upgrade_jobs`, `upgrade_job_devices`, `dns_servers`, `wifi_history`, `client_history`, `mac_lookup`, `app_settings`, `bridge_status`, `bridge_port_status`, `loop_events`, `interface_state`, `bridge_vlans`, `vlan_labels`
- **`internal/auth/`** — JWT (access 15min + refresh 7d), bcrypt passwords, chi middleware (`RequireAuth`, `RequireRole`)
- **`internal/routeros/`** — RouterOS API client pool wrapping `go-routeros/routeros/v3`. Functions for `/system/resource`, `/ip/neighbor`, `/interface/monitor-traffic`, `/system/package/update`, `/interface/bridge` + `monitor`, `/interface/bridge/port` + `monitor`, CAPsMAN/wifi registration tables, ARP/DHCP, and parsed `/log/print` events (wireless and bridge topics)
- **`internal/kea/`** — minimal client for the Kea DHCP Control Agent (`lease4-get-all`); used by client discovery to enrich MAC → IP/hostname lookups when `kea_url` is set in app settings
- **`internal/poller/manager.go`** — orchestrates background polling goroutines:
  - Health (liveness): 30s (env), a lightweight TCP "ping" (`routeros.Ping`, plain dial to the API port — bypasses the connection pool/mutex so it never contends with heavier pollers and doesn't flap on transient API slowness). Owns device `status`: a device is only marked `offline` after it has missed pings for longer than `offline_threshold_seconds` (app_setting, default 120s); within that grace window it reports `unknown` (surfaced as the gray "not responding" state) rather than flapping straight to `offline`. `last_seen` means *last successful contact* (not bumped on failed pings). The frontend renders the status triad — green `online` / red `offline` / gray `unknown`→"not responding" — via the shared helpers in `frontend/src/lib/status.ts`.
  - Info refresh: `info_interval` (app_setting, default 60m), the heavy pass — full `/system/resource` (cpu/memory/uptime + version/board/platform/arch) and the interface list, cached in the DB. Static details rarely change, so this runs rarely while the liveness ping keeps status fresh; the UI shows the cached values between refreshes. A freshly-added device (empty board/version) is enriched immediately on its first successful ping instead of waiting a full interval. The info pass never downgrades status — that's the liveness poll's job.
  - Topology: 60s (env), polls `/ip/neighbor`, runs topology builder, broadcasts graph via WS
  - Traffic: on-demand via `TrafficManager` — starts/stops per-interface 1s polling when WS clients subscribe to `traffic.*` topics
  - Firmware: 6h (env), checks for RouterOS updates
  - WiFi tracking: default 30s, runtime-tunable via the `wifi_interval` setting (seconds, re-read each cycle), drains wireless logs + registration tables and writes `wifi_history` join/leave/roam rows
  - Client discovery: default 15m, runtime-tunable via the `client_discovery_interval` setting (seconds, re-read each cycle), pulls ARP/DHCP/CAPsMAN + optional Kea leases, refreshes `mac_lookup` and `client_history`
  - Network health: 60s (env), polls bridges + bridge ports + the bridge VLAN table (`/interface/bridge/vlan`) + bridge logs + per-device interface runtime state (running/disabled/last-link-up/last-link-down). Writes `bridge_status` / `bridge_port_status` / `bridge_vlans` / `interface_state` / `loop_events`. The **VLANs** page renders a VLAN×device matrix (tagged/untagged per device) from `bridge_vlans`, with admin-editable per-VLAN labels in `vlan_labels`. Port-monitoring half is runtime-tunable via `port_monitor_enabled` / `port_monitor_filter` / `port_flap_threshold` / `port_flap_window_seconds` settings; emits `port_disabled` / `port_link_down` / `port_link_flap` event kinds.
  - Retention: 1h, deletes old traffic samples, stale neighbors, old wifi/client/loop history
- **`internal/poller/firmware.go`** — `UpgradeExecutor` runs async firmware upgrade jobs with per-device progress tracking (download → install → reboot → verify), broadcasting status via WS `upgrade.progress.<jobId>`
- **`internal/topology/builder.go`** — builds deduplicated bidirectional link graph from raw neighbor data. Resolution priority: MAC → IP → identity. Canonical link ordering `(min_id, max_id)` prevents duplicates.
- **`internal/ws/`** — topic-based WebSocket pub/sub hub. Topics: `device.health`, `topology.update`, `traffic.<id>.<iface>`, `firmware.update`, `upgrade.progress.<jobId>`, `wifi.event`, `network.health`, `network.health.event`
- **`internal/api/`** — chi router, REST endpoints under `/api/v1/`, WebSocket at `/api/v1/ws`

### Frontend (`frontend/`)

Next.js 16 (App Router) + React 19 + shadcn/ui (base-ui, not Radix) + Tailwind CSS v4.

- **Route group `(authenticated)/`** — protected layout with sidebar, redirects to `/login` if no token
- **Pages:** dashboard, topology (node card-grid view built from neighbor data; the `cytoscape` deps in package.json are currently unused), devices (data table + CRUD), device detail (`/devices/[id]` — tabs for interfaces/neighbors, CPU/mem gauges), traffic (Recharts area charts with live WS updates), firmware (bulk upgrade with real-time progress via WS), wifi (per-AP client list + roam timeline + live event feed), clients (ARP/DHCP/CAPsMAN snapshot enriched with `mac_lookup`), network-health (bridge/STP state + per-port state + recent loop/flap/port events), vlans (VLAN×device tagged/untagged matrix + per-VLAN port detail + admin-editable VLAN labels), users (admin), settings (admin: polling intervals, retention, dark mode, Kea Control Agent URL, DNS servers, port-monitoring filter/thresholds)
- **`src/lib/api.ts`** — typed API client for all REST endpoints
- **`src/lib/ws.ts`** — WebSocket client with auto-reconnect and topic subscription
- **`src/context/auth.tsx`** — auth state management (login, setup, refresh, logout)
- **`src/hooks/use-websocket.ts`** — React hook for subscribing to WS topics with shared connection

### Key patterns

- shadcn/ui in this project uses **base-ui** (not Radix). Use `render={<Component />}` instead of `asChild` for composition.
- Device credentials in `devices.password_enc` are encrypted at rest (AES-256-GCM, `internal/crypto`) when `MIKROTIK_NMS_ENCRYPTION_KEY` is set — encrypt/decrypt happens transparently in `queries` (CreateDevice/UpdateDevice/GetDevice/ListDevices), plaintext rows migrate on startup, and the column carries an `enc:v1:` prefix. Without a key it falls back to plaintext and is redacted from `/admin` backups/exports.
- Topology links are **derived** from raw neighbor data in the `neighbors` table, stored in `links` table. Rebuilt on each topology poll cycle.
- Traffic monitoring is on-demand: streaming starts when a WebSocket client subscribes to a `traffic.*` topic.
- WiFi join/leave/roam events have two sources: **wireless log lines** (authoritative — parsed from `/log/print`) and **registration-table snapshots** (safety net). Each row in `wifi_history` records its `source` (`log` / `snapshot` / `absence`) so the UI can show provenance.
- Bridge log parsing is best-effort: regex patterns in `routeros/bridge_logs.go` look for "loop detected" / "MAC flap" / "BPDU on edge". Patterns are deliberately loose; new RouterOS versions may emit phrasing that needs regex updates.
- App settings (`app_settings` table) are runtime-tunable via the **Settings** page; backend pollers that read from this table (wifi_interval, client_discovery_interval, kea_url, DNS servers, port_monitor_*, offline_threshold_seconds, info_interval, tcn_storm_threshold) pick up changes on the next cycle without a restart. Pollers driven by env vars (health, topology, firmware, network_health, retention) require a backend restart.
- Continuous deploy to the LXC has two complementary paths: a **HMAC webhook agent** (`backend/cmd/deploy-agent` + `deploy/webhook-agent/`) and a **self-hosted GitHub Actions runner** (`deploy/lxc/runner-install.sh` + `.github/workflows/deploy-lxc.yml`). Both call `install.sh --skip-deps`; the runner path is gated by a narrow sudoers grant to `/usr/local/sbin/mikrotik-nms-deploy` (a wrapper that validates the workspace before forwarding to the installer).
- First-run: `POST /api/v1/auth/setup` creates the initial admin user (only works when 0 users exist).
