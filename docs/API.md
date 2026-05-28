# MikroTik NMS — API Reference

Complete reference for the MikroTik NMS REST and WebSocket APIs. The backend is a
Go (chi) service; every route below is defined in
[`backend/internal/api/router.go`](../backend/internal/api/router.go).

For project setup and environment variables see the [README](../README.md).

## 1. Base URL & Conventions

All endpoints live under the `/api/v1` prefix. In a default deployment the
backend listens on `:8080`, so the base URL is:

```
http://<host>:8080/api/v1
```

There is no further version negotiation — `v1` is the only namespace.

| Convention | Detail |
|---|---|
| Content type | Request and response bodies are JSON (`Content-Type: application/json`). |
| Auth header | `Authorization: Bearer <access_token>` on every protected route. |
| Timestamps | RFC 3339 / ISO-8601 in strings (e.g. traffic `from`/`to` query params). |
| IDs | Devices, users, DNS servers, and upgrade jobs use UUID strings. Loop events use integer IDs. |
| CORS | All origins allowed; methods `GET, POST, PUT, DELETE, OPTIONS`; credentials allowed. |

### Error shape

Errors are returned as a single-field JSON object (see
[`helpers.go`](../backend/internal/api/helpers.go)):

```json
{ "error": "device not found" }
```

The HTTP status code carries the semantics. The auth middleware emits its own
fixed bodies, e.g. `{"error":"missing authorization token"}` (401) and
`{"error":"forbidden"}` (403).

| Status | Meaning |
|---|---|
| `200 OK` | Success. |
| `201 Created` | Resource created (setup, user, device, DNS server). |
| `202 Accepted` | Async work started (firmware check, upgrade). |
| `400 Bad Request` | Malformed body or invalid parameters. |
| `401 Unauthorized` | Missing/invalid token or bad credentials. |
| `403 Forbidden` | Authenticated but lacking the required role. |
| `404 Not Found` | Resource does not exist. |
| `409 Conflict` | Duplicate resource (username, device address) or setup already done. |
| `500 Internal Server Error` | Unexpected backend / database failure. |

## 2. Authentication

Authentication is JWT-based with an access/refresh token pair, signed with
HS256 using `MIKROTIK_NMS_JWT_SECRET`
([`auth/jwt.go`](../backend/internal/auth/jwt.go)).

| Token | Lifetime | Claims |
|---|---|---|
| Access | 15 minutes | `uid`, `usr`, `role`, plus standard `exp`/`iat`/`sub` |
| Refresh | 7 days | minimal — `exp`/`iat`/`sub` (subject = user ID) |

On `login`/`setup`/`refresh` the server returns:

```json
{
  "access_token": "<jwt>",
  "refresh_token": "<jwt>",
  "expires_at": 1764300000
}
```

`expires_at` is the access token's Unix expiry. The refresh token is **also**
set as an `httpOnly` cookie named `refresh_token` (path `/api/v1/auth`,
`SameSite=Strict`).

### Refresh flow

The access token expires after 15 minutes. To obtain a fresh pair, call
`POST /auth/refresh` with the refresh token supplied **either** as the
`refresh_token` cookie **or** in the JSON body as `{"refresh_token": "..."}`.
The cookie is checked first. A valid refresh token returns a new pair and
re-sets the cookie.

### First-run setup

`POST /auth/setup` creates the initial **admin** user, and only works while
zero users exist (otherwise `409 Conflict`). It returns a token pair so the new
admin is logged in immediately.

### Roles

There are exactly two roles: `admin` and `viewer`. Role matching is **exact** —
there is no admin-implies-viewer hierarchy in the middleware
([`auth/middleware.go`](../backend/internal/auth/middleware.go)). Viewer-safe
routes simply omit the role gate, so an admin passes them too. Admin-only routes
require the role string to equal `admin`.

The `Role` column below uses: **public** (no token), **any** (any authenticated
user), **admin** (admin role required).

## 3. Endpoint Reference

### Auth & Session

| Method | Path | Role | Description |
|---|---|---|---|
| POST | `/auth/login` | public | Body `{username, password}` → token pair. `401` on bad credentials. |
| POST | `/auth/setup` | public | First-admin bootstrap. Body `{username, password}`. `409` if users exist. |
| POST | `/auth/refresh` | public | Refresh token (cookie or body) → new token pair. |
| POST | `/auth/logout` | any | Clears the `refresh_token` cookie. Returns `{"status":"ok"}`. |
| GET | `/auth/me` | any | Current user `{id, username, role}`. |

### Users (admin)

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/users` | admin | List all users. |
| POST | `/users` | admin | Create user. Body `{username, password, role}`; `role` ∈ `admin`/`viewer`. `409` if username exists. |
| DELETE | `/users/{id}` | admin | Delete a user. `400` if deleting yourself. |

### Devices

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/devices` | any | List all managed devices. |
| GET | `/devices/{id}` | any | Get one device. `404` if absent. |
| GET | `/devices/{id}/interfaces` | any | Cached interface list for the device. |
| GET | `/devices/{id}/neighbors` | any | Cached `/ip/neighbor` neighbors for the device. |
| POST | `/devices` | admin | Add a device. Tests the RouterOS connection first; `400` if unreachable. |
| PUT | `/devices/{id}` | admin | Update device fields (only non-empty fields override). |
| DELETE | `/devices/{id}` | admin | Remove a device. |

Create/update body fields:

```json
{
  "address": "10.0.0.1",
  "identity": "core-router",
  "username": "admin",
  "password": "secret",
  "use_tls": false,
  "api_port": 8728,
  "tags": "[]",
  "notes": ""
}
```

`address` is required on create. Omitted `username`/`api_port`/`password` fall
back to the `MIKROTIK_NMS_DEFAULT_ROS_*` config values. On create the backend
dials the device, and if `identity` is blank it reads `/system/identity`.

### Discovery

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/discovery` | any | MNDP broadcast scan (UDP 5678). Query `duration` (1–30s, default 10). |
| GET | `/discovery/deep` | admin | Merge unmanaged neighbors with an optional subnet port-scan. Query `cidr` (e.g. `10.0.0.0/24`) probes ports 8728/8729/8291. |

Deep-scan results carry a `source` of `neighbor`, `port-scan`, or `both`.

### Topology

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/topology` | any | Rebuilds and returns the Cytoscape graph (`nodes` + `edges`) from neighbor data. |

### Traffic

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/traffic/summary` | any | One-shot rx/tx bps snapshot for each online device (`[{device_id, rx_bps, tx_bps}]`). |
| GET | `/traffic/{deviceId}/{iface}` | any | Historical traffic samples. Query `from`/`to` (RFC3339, default last 1h), `limit` (1–10000, default 1000). |

Live per-interface streaming is delivered over WebSocket — see
[§4](#4-websocket-api).

### Firmware

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/firmware` | any | Latest firmware status per device. |
| GET | `/firmware/upgrade/{jobId}` | any | Upgrade job state plus per-device progress (`{job, devices}`). |
| POST | `/firmware/check` | admin | Trigger an async update check across online devices. `202`. |
| POST | `/firmware/upgrade` | admin | Start an upgrade job. Body `{device_ids: [...], reboot: bool}`. Returns `202` with `job_id`. |
| POST | `/firmware/channel` | admin | Set release channel. Body `{device_ids, channel}`; `channel` ∈ `stable`/`long-term`/`testing`/`development`. |
| POST | `/firmware/routerboard` | admin | Upgrade RouterBOOT firmware. Body `{device_ids, reboot}`. |

Channel/routerboard responses summarize per-device outcomes, e.g.
`{"changed": 2, "errors": ["<id>: not connected"]}`.

### WiFi & MAC Lookup

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/wifi/current` | any | Each tracked client's current AP, enriched with device + MAC-lookup data. |
| GET | `/wifi/history` | any | Join/leave/roam history. Query `mac`, `ap`, or neither (recent); `limit` (1–5000, default 200). |
| GET | `/mac-lookup` | any | Full MAC → IP/host/AP lookup map. |

### Clients

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/clients` | any | Live ARP/DHCP/CAPsMAN scan across online devices, deduped by MAC and DNS-enriched. Query `limit` (>0, unlimited if absent), `timeout` (5–120s, default 30). |
| GET | `/clients/cached` | any | Last persisted client snapshot from `mac_lookup` (fast, no device contact). |
| GET | `/debug/wifi` | any | Raw WiFi/CAPsMAN registration-table rows for one device. **Requires** query `device_id`. |

The `/clients` response is `{clients: [...], total, limited, timed_out}`.

### Network Health (bridge / STP / loop detection)

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/network-health` | any | Bridges (with ports), recent loop events, and per-interface port states. |
| GET | `/network-health/events` | any | Loop/flap/port events. Query `limit` (1–5000, default 200). |
| POST | `/network-health/events/{id}/ack` | any | Acknowledge one event (integer `id`). |
| POST | `/network-health/events/ack-all` | any | Acknowledge all events. Returns `{"acknowledged": <count>}`. |

Event kinds include `stp_disabled`, `tcn_storm`, `loop_detected`, `mac_flap`,
`bpdu_on_edge`, `port_disabled`, `port_link_down`, and `port_link_flap`.

### VLANs

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/vlans` | any | Bridge VLAN table (tagged/untagged per device), with `device_name`. |
| GET | `/vlan-labels` | any | User-defined VLAN labels. |
| PUT | `/vlan-labels` | admin | Upsert a label. Body `{vlan_id, name, purpose, color}`. |

### DNS

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/dns` | any | List configured reverse-DNS resolvers. |
| POST | `/dns/resolve` | any | Reverse-resolve IPs. Body `{ips: [...]}` → `{ip: hostname}` map. |
| POST | `/dns` | admin | Add a resolver. Body `{name, address, port}` (port defaults to 53). |
| PUT | `/dns/{id}` | admin | Update a resolver. Body `{name, address, port, enabled?}`. |
| DELETE | `/dns/{id}` | admin | Delete a resolver. |

### NetBox Export

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/netbox/export` | any | All NetBox import data as one JSON object (manufacturers, device types/roles, devices, interfaces, IPs, cables). |
| GET | `/netbox/export/{type}` | any | A single CSV file (`Content-Type: text/csv`). |

Valid `{type}` values: `manufacturers`, `device_types`, `device_roles`,
`devices`, `interfaces`, `ip_addresses`, `cables`. Any other value → `400`.

### Settings & Admin

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/settings` | any | All `app_settings` key/value pairs. |
| PUT | `/settings` | admin | Update settings. Body is a flat `{key: value}` map; unknown keys are silently ignored. |
| POST | `/admin/purge-history` | admin | Wipe history tables. Body `{wifi, clients, network_health, traffic, older_than_days}`. |
| GET | `/admin/export/{table}` | admin | Download one table as a JSON file (allowlisted tables only). |
| POST | `/admin/import/{table}` | admin | Import rows into one table (`INSERT OR IGNORE`). Body is a JSON array of row objects. |
| GET | `/admin/backup` | admin | Download a full multi-table JSON backup bundle. |
| POST | `/admin/restore` | admin | Restore from a backup bundle (version-checked). |

Settings keys accepted by `PUT /settings`: `health_interval`,
`topology_interval`, `firmware_interval`, `wifi_interval`,
`client_discovery_interval`, `network_health_interval`,
`offline_threshold_seconds`, `info_interval`, `retention_days`, `dark_mode`,
`kea_url`, `port_monitor_enabled`, `port_monitor_filter`,
`port_flap_threshold`, `port_flap_window_seconds`, `tcn_storm_threshold`,
`opnsense_url`, `opnsense_api_key`, `opnsense_api_secret`,
`opnsense_verify_tls`.

`older_than_days = 0` (or omitted) on purge means *delete everything* from the
selected tables; the four purgeable tables are `wifi_history`,
`client_history`, `loop_events`, and `traffic_samples`. Current-state tables
are never touched.

### Health

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/health` | public | Liveness probe. Returns `{"status":"ok","instance_id":"<hex>"}`. |

`instance_id` is regenerated on each backend start, so clients can detect a
redeploy by comparing it across polls.

## 4. WebSocket API

Connect to:

```
ws://<host>:8080/api/v1/ws?token=<access_token>
```

The WebSocket upgrade is gated by `RequireAuth`, but browsers cannot set an
`Authorization` header on a WebSocket. The middleware therefore falls back to a
`token` **query parameter** ([`auth/middleware.go`](../backend/internal/auth/middleware.go)).
An expired/invalid token causes the upgrade to be rejected; the client should
refresh and reconnect (see [`frontend/src/lib/ws.ts`](../frontend/src/lib/ws.ts)).

### Subscribe / unsubscribe protocol

After connecting, the client sends JSON control frames. The server tracks
subscriptions per topic and only forwards messages for topics you subscribed to
([`ws/client.go`](../backend/internal/ws/client.go)):

```json
{ "action": "subscribe",   "topic": "device.health" }
{ "action": "unsubscribe", "topic": "device.health" }
```

`action` must be `subscribe` or `unsubscribe`; an empty `topic` is ignored. The
server pings every 30s and limits inbound frames to 4096 bytes.

### Server message envelope

Every broadcast is a `Message` ([`ws/hub.go`](../backend/internal/ws/hub.go)):

```json
{
  "topic": "device.health",
  "timestamp": "2026-05-28T12:00:00Z",
  "data": { }
}
```

`timestamp` is the RFC 3339 publish time. Route on `topic` and read the payload
from `data`; match the incoming `msg.topic` against the topic you subscribed to.

### Topic catalog

| Topic pattern | Payload | Description |
|---|---|---|
| `device.health` | object | Device liveness/info updates from the health and info pollers. |
| `topology.update` | graph object | Full topology graph (`nodes` + `edges`); the one non-`map` payload. |
| `traffic.<deviceID>.<iface>` | object | 1s rx/tx samples; streaming starts on subscribe, stops on last unsubscribe. |
| `firmware.update` | object | Firmware status changed (poll cycle or triggered check). |
| `upgrade.progress.<jobId>` | object | Per-device upgrade progress for one job. |
| `wifi.event` | object | A WiFi join/leave/roam event. |
| `network.health` | object | Network-health poll-cycle summary. |
| `network.health.event` | object | A single new loop/flap/port event. |

The `<deviceID>`, `<iface>`, and `<jobId>` placeholders are substituted with the
concrete IDs you want to follow, e.g. `traffic.<uuid>.ether1` or
`upgrade.progress.<jobId>`.

## 5. Examples

### curl: login then call a protected endpoint

```bash
BASE=http://localhost:8080/api/v1

# 1. Log in and capture the access token (jq required)
TOKEN=$(curl -s -X POST "$BASE/auth/login" \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"changeme"}' \
  | jq -r .access_token)

# 2. Use the bearer token to list devices
curl -s "$BASE/devices" -H "Authorization: Bearer $TOKEN" | jq

# 3. Start a firmware upgrade (admin only)
curl -s -X POST "$BASE/firmware/upgrade" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"device_ids":["<device-uuid>"],"reboot":true}'
```

First-run bootstrap (no users yet) uses `setup` instead of `login`:

```bash
curl -s -X POST "$BASE/auth/setup" \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"a-strong-password"}'
```

### JavaScript: subscribe to a WebSocket topic

```javascript
const token = localStorage.getItem("access_token");
const ws = new WebSocket(`ws://localhost:8080/api/v1/ws?token=${token}`);

ws.onopen = () => {
  // Follow live network-health events
  ws.send(JSON.stringify({ action: "subscribe", topic: "network.health.event" }));

  // Stream live traffic for one interface
  ws.send(JSON.stringify({ action: "subscribe", topic: "traffic.<device-uuid>.ether1" }));
};

ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);   // { topic, timestamp, data }
  if (msg.topic === "network.health.event") {
    console.log("loop/flap event:", msg.data);
  }
};

// Later: stop streaming
ws.send(JSON.stringify({ action: "unsubscribe", topic: "traffic.<device-uuid>.ether1" }));
```

The production frontend wraps this in a reconnecting client with automatic token
refresh — see [`frontend/src/lib/ws.ts`](../frontend/src/lib/ws.ts) and the
[`useWebSocket`](../frontend/src/hooks/use-websocket.ts) hook.
