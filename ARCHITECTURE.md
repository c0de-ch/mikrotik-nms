# Architecture

MikroTik NMS is a web-based, **read-only** network management system for MikroTik
RouterOS devices. A Go backend polls each device over the RouterOS API and serves
topology, traffic, firmware, WiFi, client and L2-health data to a Next.js frontend
over REST plus a WebSocket pub/sub channel. Configuration changes are never pushed
to devices — operators jump to a device's native config tool via a WinBox deep link
(`winbox://`). Access is gated by JWT auth with two roles: **admin** (full CRUD on
devices, users, firmware, settings) and **viewer** (read-only).

## High-level diagram

```
                    RouterOS API (8728 / TLS 8729)
   ┌───────────┐    MNDP discovery (UDP 5678)
   │  MikroTik │◄──────────────┐
   │  devices  │               │
   └───────────┘               │
                               ▼
   ┌─────────────────────────────────────────────────────────────┐
   │  Go backend  (cmd/mikrotik-nms)                               │
   │                                                               │
   │   routeros.Pool ──► poller.Manager ──┬─► SQLite (WAL)         │
   │   (client cache)    health/info/topo │   one writer only      │
   │                     traffic/wifi/...  │                        │
   │                                       └─► ws.Hub (topic pub)   │
   │   internal/api (chi) ── REST /api/v1 ───────► WebSocket /ws    │
   └──────┬──────────────────────────────────────────┬────────────┘
          │ REST + JWT                                │ WS (?token=)
          ▼                                           ▼
   ┌─────────────────────────────────────────────────────────────┐
   │  Next.js frontend  (App Router, shadcn/base-ui)              │
   │   lib/api.ts (REST)   lib/ws.ts (WS)   hooks/use-websocket    │
   └─────────────────────────────────────────────────────────────┘

   Optional external integrations (backend → out):
     Kea Control Agent · OPNsense Kea-lease API   (client/IP enrichment)
     NetBox CSV/JSON export                        (generated locally, no API call)
```

## Backend components

All backend code lives under `backend/`. Key packages:

| Package | Responsibility |
|---|---|
| `cmd/mikrotik-nms` | Entrypoint; wires config → DB → `ws.Hub` → `routeros.Pool` → `poller.Manager` → chi HTTP server, with graceful shutdown |
| `cmd/deploy-agent` | Standalone HMAC-SHA256 GitHub-webhook listener that runs a configured shell deploy command |
| `internal/config` | Loads `MIKROTIK_NMS_*` env vars; requires `JWT_SECRET` |
| `internal/database` | Opens SQLite (WAL mode) and runs embedded goose migrations `001`–`013` |
| `internal/database/queries` | Raw-SQL query functions, one file per domain (devices, interfaces, neighbors, links, traffic, firmware, upgrades, users, dns, wifi, mac_lookup, network_health, vlans, settings, interface_state) |
| `internal/auth` | JWT access/refresh issuance, bcrypt passwords, `RequireAuth` / `RequireRole` chi middleware |
| `internal/routeros` | RouterOS API connection `Pool` + call wrappers (resources, interfaces, bridge, logs, clients, firmware, traffic, ping, MNDP discovery, subnet scan) |
| `internal/kea` | Kea DHCP Control Agent client (`lease4-get-all`) for MAC→IP/host enrichment |
| `internal/opnsense` | OPNsense Kea-lease REST client (v4 + v6 search) |
| `internal/resolver` | Concurrent reverse-DNS resolver, 5-minute cache, custom servers + system fallback |
| `internal/topology` | Builds a deduplicated bidirectional link graph from neighbor data (MAC→IP→identity resolution) and the Cytoscape graph types |
| `internal/poller` | Background polling manager and per-concern loops (health, info, topology, traffic, wifi, client discovery, network health, port monitor, firmware, retention) |
| `internal/ws` | Topic-based WebSocket pub/sub hub and per-connection client |
| `internal/api` | chi router and REST/WS handlers under `/api/v1` |

> **Security note:** `devices.password_enc` is encrypted at rest with
> AES-256-GCM when `MIKROTIK_NMS_ENCRYPTION_KEY` is set (existing rows are
> migrated on startup). Without a key it falls back to plaintext and is then
> stripped from `/admin/backup` and `/admin/export/devices`. User passwords
> (`users.password_hash`) use bcrypt.

## Frontend components

The frontend is Next.js 16 (App Router) + React 19 under `frontend/src/`.

- **Route group `(authenticated)/`** — protected layout (`layout.tsx`) with the
  sidebar; redirects to `/login` when no token is present. Pages: `dashboard`,
  `topology` (node card-grid view), `devices` + `devices/[id]`, `traffic` (Recharts),
  `firmware`, `wifi`, `clients`, `network-health`, `vlans`, `export`, `users`
  (admin), `settings` (admin). `/login` and the first-run setup live outside the group.
- **`src/lib/`**
  - `api.ts` — typed REST client; injects the bearer token, retries once through a
    single-flight `/auth/refresh` on 401, and dispatches `auth:refreshed` / `auth:expired` events.
  - `ws.ts` — `NmsWebSocket` class: auto-reconnect with backoff, topic re-subscribe
    on reopen, and the same single-flight token refresh; connects to `/api/v1/ws?token=…`.
  - `status.ts` — single source of truth for the green `online` / red `offline` /
    gray "not responding" status triad (labels, badge classes, dot colors).
  - `fold.ts` — time-bucket grouping helpers (`off`/`hour`/`day`/`week`/`month`) for event tables.
  - `utils.ts` — small UI helpers (`cn`, etc.).
- **`src/hooks/`** — `use-websocket.ts` (subscribe to a topic over a single shared,
  ref-counted WS connection), `use-mobile.ts`, `use-version-watchdog.ts`.
- **`src/context/auth.tsx`** — auth state (login, setup, refresh, logout).
- **shadcn/ui uses base-ui** (not Radix) in this project — components compose via the
  `render={<Component />}` prop, not `asChild`.

## Polling model

`poller.Manager.Start` launches one goroutine per loop. Interval source is either an
env var (`MIKROTIK_NMS_*`, restart to change) or an `app_settings` row (runtime-tunable).

| Loop | Interval & source | RouterOS calls | DB tables written | WS topic |
|---|---|---|---|---|
| health (liveness) | `HEALTH_INTERVAL` env / 30s | `routeros.Ping` (plain TCP dial, 2 tries) | `devices` (status/last_seen/last_error) | `device.health` |
| info (heavy refresh) | `info_interval` setting / 60m | `/system/resource`, `/system/identity`, `/interface/print` | `devices` (cpu/mem/uptime/board/version/arch), `interfaces` | `device.health` |
| topology | `TOPOLOGY_INTERVAL` env / 60s | `/ip/neighbor/print` | `neighbors`, `links` | `topology.update` |
| firmware | `FIRMWARE_INTERVAL` env / 6h | `/system/package/update`, `/system/routerboard/print` | `firmware_status` | `firmware.update` |
| traffic | on-demand; reconcile 2s, per-iface 1s | `/interface/monitor-traffic` | `traffic_samples` | `traffic.<id>.<iface>` |
| wifi | `wifi_interval` setting / 30s | `/log/print` (wireless) + CAPsMAN/WiFi reg tables | `wifi_history`, `mac_lookup` | `wifi.event` |
| client discovery | `client_discovery_interval` setting / 15m | `/ip/arp`, `/ip/dhcp-server/lease`, CAPsMAN (+ Kea, + OPNsense) | `mac_lookup`, `client_history` | — |
| network health | `NETWORK_HEALTH_INTERVAL` env / 60s | bridge + bridge/port + bridge/vlan print/monitor, ethernet, bridge logs | `bridge_status`, `bridge_port_status`, `bridge_vlans`, `interface_state`, `loop_events` | `network.health`, `network.health.event` |
| retention | `RETENTION_INTERVAL` env / 1h | — | prunes `traffic_samples`, stale `neighbors` (24h), old `wifi_history`/`client_history`/`loop_events` (`RETENTION_DAYS`/7) | — |

> The wifi and client-discovery loops re-read `wifi_interval` /
> `client_discovery_interval` (seconds) from `app_settings` each cycle, falling
> back to 30s / 15m, so Settings changes apply without a restart. Other
> runtime-tunable knobs include `offline_threshold_seconds`, `info_interval`,
> `tcn_storm_threshold`, and the `port_monitor_*` settings.

## WebSocket topics

Payloads are JSON `{ "topic", "timestamp", "data" }`. All `data` values are objects
except `topology.update`, which carries the typed graph. `timestamp` is the RFC 3339
publish time.

| Topic | Payload | Publisher | Subscriber |
|---|---|---|---|
| `device.health` | device status/info snapshot | health + info loops | dashboard, devices, device detail |
| `topology.update` | `*topology.Graph` (nodes/edges, Cytoscape element format) | topology loop | topology page |
| `traffic.<deviceID>.<iface>` | rx/tx sample | TrafficManager (on demand) | traffic page |
| `firmware.update` | firmware status summary | firmware loop | firmware page |
| `upgrade.progress.<jobId>` | per-device upgrade stage | `UpgradeExecutor` | firmware page (during upgrade) |
| `wifi.event` | join/leave/roam event | WifiTracker | wifi page |
| `network.health` | bridge/port cycle summary | NetworkHealthPoller | network-health, dashboard banner |
| `network.health.event` | a single `loop_event` | NetworkHealthPoller | network-health page |

**Subscribe protocol:** the browser opens `GET /api/v1/ws?token=<access_token>`
(JWT passed as a query param because browsers can't set WS headers). The client then
sends `{"action":"subscribe","topic":"<topic>"}` per topic, and
`{"action":"unsubscribe",...}` to drop one. The frontend funnels all of this through a
single shared, ref-counted connection (`use-websocket.ts`) and re-subscribes
automatically after a reconnect.

## Data flow

### (a) Liveness / health status

```
healthLoop (30s)
  └─ routeros.Ping(dev)  ── plain TCP dial to API port, bypasses the pool mutex
       ├─ success → devices.last_seen = now, status = "online"
       └─ failure → status stays "online"/"unknown" until the device has missed
                    pings for > offline_threshold_seconds (default 120s),
                    then status = "offline"  (avoids flapping)
  └─ Hub.Publish("device.health", snapshot)
       └─ browser (subscribed) updates the status dot via lib/status.ts
```

The lightweight ping never blocks on the heavier pollers (it does not take a pool
connection), and the info loop never downgrades status — ownership of `status` is the
liveness loop's alone. A freshly added device with no board/version is enriched once,
immediately, on its first successful ping rather than waiting a full `info_interval`.

### (b) On-demand traffic streaming

```
traffic page mounts
  └─ useWebSocket("traffic.<id>.<iface>") → WS {action:"subscribe", topic}
       └─ Hub records the subscription
TrafficManager reconcile tick (every 2s)
  └─ Hub.TopicSubscriberCount(topic) > 0 ?
       ├─ yes & not running → start 1s /interface/monitor-traffic stream
       │      └─ each sample: write traffic_samples + Hub.Publish(topic, sample)
       │           └─ Recharts area chart appends the point live
       └─ no  & running    → stop the stream (saves device + DB load)
page unmounts → WS unsubscribe → next reconcile tears the stream down
```

Traffic is never polled fleet-wide; a device interface is only sampled while at least
one client is watching it.

## Database

Single SQLite file in **WAL mode**. SQLite permits only one writer, so the backend
runs as a **single replica** (the Kubernetes `Deployment` is pinned to `replicas: 1`).
Tables grouped by domain:

| Domain | Tables | Purpose (one line each) |
|---|---|---|
| Identity / auth | `users` | admin/viewer accounts; bcrypt `password_hash` |
| Inventory | `devices`, `interfaces` | device records (creds, cpu/mem/version) + their interfaces |
| Topology | `neighbors`, `links` | raw MNDP/discovery neighbors and the derived deduped link graph |
| Time-series | `traffic_samples` | per-interface rx/tx samples for the traffic charts |
| Firmware | `firmware_status`, `upgrade_jobs`, `upgrade_job_devices` | current vs available versions + async upgrade jobs and per-device progress |
| WiFi / clients | `wifi_history`, `mac_lookup`, `client_history` | join/leave/roam log, MAC→IP/host cache, ARP/DHCP snapshots |
| Network health / VLANs | `bridge_status`, `bridge_port_status`, `bridge_vlans`, `interface_state`, `loop_events`, `vlan_labels` | STP/bridge state, port runtime state, VLAN matrix, loop/flap events (+ack), admin VLAN labels |
| DNS / settings | `dns_servers`, `app_settings` | reverse-DNS resolvers and the runtime key/value config store |

## Key design decisions & tradeoffs

- **Derived topology.** Links are computed from raw neighbor rows each topology cycle
  (MAC → IP → identity resolution with canonical `(min_id, max_id)` ordering) rather
  than stored by hand. Self-healing as the network changes, at the cost of a rebuild
  per poll and dependence on neighbor data quality.
- **On-demand traffic streaming.** Per-interface 1s polling starts only when a client
  subscribes and stops when the last one leaves. Keeps device/DB load near zero when
  no one is watching; trades a ~2s reconcile latency before the first sample arrives.
- **Liveness vs. info split.** A cheap 30s TCP ping owns `status` while the expensive
  full `/system/resource` + interface pass runs only every ~60m. Status stays fresh
  and non-flapping without hammering devices for data that rarely changes; the UI
  shows cached details between info refreshes.
- **Runtime settings vs. env-var pollers.** `app_settings` knobs
  (`offline_threshold_seconds`, `info_interval`, `tcn_storm_threshold`,
  `port_monitor_*`, Kea/OPNsense/DNS) take effect on the next cycle with no restart;
  the core loop intervals (health, topology, firmware, network-health, retention) are
  env-driven and require a backend restart. Simpler operations for the hot knobs,
  explicit redeploys for the structural ones.
- **Raw SQL, no ORM.** Hand-written queries in `internal/database/queries` keep the
  binary small and the SQL legible against SQLite's feature set; the price is more
  boilerplate and no compile-time query checking.
- **Device credentials at rest.** Device passwords are encrypted with AES-256-GCM
  when `MIKROTIK_NMS_ENCRYPTION_KEY` is configured (and migrated on startup);
  without a key they fall back to plaintext and are redacted from backups/exports.

## See also

- [REST + WebSocket API reference](./docs/API.md)
- [Deployment guide (Docker, Kubernetes, LXC, CHR)](./docs/DEPLOYMENT.md)
- [Development setup and workflow](./docs/DEVELOPMENT.md)
