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
| IDs | Devices, users, DNS servers, and upgrade jobs use UUID strings. Loop events, flow exporters and flow points use integer IDs. |
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
| `409 Conflict` | Duplicate resource (username, device address, flow exporter address, flow view for a port) or setup already done. |
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
| GET | `/traffic/summary` | any | One rx/tx bps figure per device in the port-stats snapshot (`[{device_id, rx_bps, tx_bps}]`): its first bridge, else `ether1`, else its first interface. No device calls. |
| GET | `/traffic/links` | any | One-shot per-link throughput for the network map's first paint (`{links: [...]}`); the continuous feed is WS `topology.traffic`. |
| GET | `/traffic/ports/latest` | any | Fleet-wide port snapshot (`{ts, interval_seconds, ready, ports}`), each port enriched with `device_name` and `role`. `ts` is `null` before the collector's first cycle. |
| GET | `/traffic/ports/top` | any | Top talkers. Query `range`, `metric` (`avg`\|`max`\|`bytes`, default `avg`), `dir` (`total`\|`rx`\|`tx`, default `total`), `limit` (1–200, default 20), `physical` (default on: drops only `virtual` and `vpn`, so every physical port that carried traffic — `peer` links and bond members included — is listed; `0`/`false` also lists virtual and VPN interfaces), `device` (rank every known interface of that device, idle ones as zeros), `sparkline` (default on; `0` skips). |
| GET | `/traffic/ports/roles` | any | Inferred device tree and every port's role: `{anchored, devices: [{device_id, name, parent_id, parent_iface, uplink_iface, depth, anchor}], roles: [{device_id, device_name, iface, role, neighbor_*, client_count, mac_count, gateway_ip, upstream_node, master, behind}]}`. `master` (optional) names the bond a member port belongs to. |
| GET | `/traffic/ports/history` | any | One port's chart + stats. Query `device`, `iface` (required), `range`. Returns `{device_id, iface, range, resolution, step_seconds, from, to, coverage_from, points: [{ts, rx_bps, tx_bps}], stats: {rx_avg, tx_avg, rx_max, tx_max, rx_p95, tx_p95, rx_bytes, tx_bytes}}`. A point's `rx_bps`/`tx_bps` are `null` for a step the collector did not cover (see *Coverage* below). `404` for an unknown device; an unknown interface returns zeros. |
| GET | `/traffic/ports/behind` | any | What sits behind a port. Query `device`, `iface` (required). Returns `{role, neighbor, uplink, client_count, mac_count, clients: [{mac, ip, host_name, vendor, vid, wireless, ap, ssid, signal, attached_device_id, attached_iface, attached_device_name, last_seen}], vlans: [{vid, name, tagged}]}`. `neighbor` is set for `uplink`, `downlink` and `peer`; `uplink` for `wan`, `vpn` and uplinks to a synthetic node (Internet / gateway / VPN). Clients are the non-managed MACs learned on that port (at most 500, attached-here first). |
| GET | `/traffic/path` | any | Internet → device/port path with the live rate of each segment. Query `device` (required), `iface` (optional; adds the sink hop: the child device or the clients). `{device_id, iface, role, anchored, hops: [{kind, id, label, device_id, in_iface, out_iface, client_count, sink, down_bps, up_bps, measured}]}`. A segment into a device that shares its parent port with other children is measured on the device's own uplink. `404` for an unknown device. |
| GET | `/traffic/sankey` | any | **Estimated** source → sink flow tree from port counters + topology (not flow export). Query `source` (`counters`, the default, or `flows` for the **measured** Sankey of one flow observation point — see [Flows](#flows); anything else is `400 invalid source`), `range`, `dir` (`download`\|`upload`, default `download`), `device` (optional subtree root). `{source: "counters", estimated, direction, range, from, to, nodes: [{id, name, type, device_id, iface, client_count}], links: [{source, target, value, source_iface}]}`; integer node indices, values in bits/s. A tree, plus one `local:<device_id>` source node (type `other`, name `Local / east-west`) feeding each non-root device whose outflow exceeds its inflow by ≥ 1 kbps (east-west traffic such as a NAS), so every node conserves flow; such a device has two incoming links. Leaves are `access`/`wireless`/`idle` ports only — never `peer` links or bond members. |
| GET | `/traffic/{deviceId}/{iface}` | any | Historical 1 s traffic samples (recorded while someone streams the interface). Query `from`/`to` (RFC3339, default last 1h), `limit` (1–10000, default 1000). |

Port-specific endpoints take `device` + `iface` as **query parameters** (URL
encoded): RouterOS interface names can contain spaces and slashes. All the
`/traffic/ports/*`, `/traffic/path` and `/traffic/sankey` routes are static, so
they never collide with `/traffic/{deviceId}/{iface}`.

**Direction.** On any port `rx` is bits entering the device through it and `tx`
bits leaving it. Download is `rx` on upstream-facing roles (`wan`, `uplink`,
`vpn`) and `tx` on downstream-facing ones (`downlink`, `access`, `wireless`);
`peer`, `virtual` and `idle` have no download/upload reading (raw in/out).
Roles: `wan uplink downlink peer access wireless vpn virtual idle`.

- `peer` — a physical port on an up link to another managed device that is not
  an edge of the inferred tree: a cable between two anchored routers (each with
  its own egress), or a redundant / ring / parallel link the tree did not
  choose. `neighbor_device_id` / `neighbor_name` / `neighbor_iface` name the
  far end, `behind` is `↔ <neighbor> <iface>`; no clients, never a path sink or
  Sankey leaf.
- A **bond member** takes its bond's role and neighbour, with `master` set to
  the bond and no clients of its own (the bridge FDB learns on the bond);
  `behind` is `member of <bond> · <bond's behind>`. A member of a WAN bond is an
  `uplink` toward the same upstream.
- The **physical carrier** of a WAN that runs over a VLAN or a PPPoE client
  (e.g. `pppoe-out1` → `vlan-wan` → `ether1`) is an `uplink` with the WAN's
  `upstream_node` — not a second `wan` port, since the logical interface
  already measures the internet edge.
- Each device's `uplink` faces its own egress: the managed device its default
  route points at, or the device whose FDB found that gateway; only then the
  first anchor it hears, then the port hearing the most devices.

Bond membership and VLAN/PPPoE carriers are read from the devices
(`/interface/bonding`, `/interface/vlan`, `/interface/pppoe-client`) on each
`port_hosts_interval` pass and kept in `interface_relations`. The port list of
a device the collector currently polls is its live `/interface/print`;
`interfaces`-table rows it lacks are ignored (and pruned on the next info
refresh).

**Ranges** (`range`, default `1h`; anything else is `400 invalid range`).
Windows are end-anchored at the newest complete bucket, and points/stats start
at `coverage_from` (the first stored bucket in the window, fleet-wide; on the
hourly table the first covered minute of that hour while 1-minute data for it
remains) — points before coverage are omitted, `coverage_from: null` means no
data yet.

**Coverage.** A native bucket (minute or hour) without a single row fleet-wide
means the collector was not running (restart, deploy, outage): MNDP/ARP chatter
makes every running port non-idle each minute. Such steps are gaps — points
with `rx_bps`/`tx_bps` `null` — and are left out of every average and of the
p95, while an idle port in a covered step is a real `0`. Every average (history
stats, top talkers, Sankey) is `bytes*8 / covered seconds`, and a step covered
only in part (coverage starting mid-step) is divided by its covered seconds.

| `range` | Window | Source | History step (points) | Top sparkline step |
|---|---|---|---|---|
| `live` | 5 min (history) / now | `traffic_samples` (history, 1 s) / collector snapshot (top, sankey) | 1 s, raw | last 30 min of 1m @ 60 s |
| `15m` | 15 min | `port_stats_1m` | 60 s (15) | 60 s |
| `1h` | 1 h | `port_stats_1m` | 60 s (60) | 120 s |
| `6h` | 6 h | `port_stats_1m` | 60 s (360) | 720 s |
| `24h` | 24 h | `port_stats_1m` | 300 s (288) | 2880 s |
| `7d` | 7 d | `port_stats_1h` | 3600 s (168) | 21600 s |
| `30d` | 30 d | `port_stats_1h` | 3600 s (720) | 86400 s |

For `range=live`, top talkers rank the current snapshot rates (`metric=bytes`
is treated as `avg` and echoed as such) and `from`/`to` are `null`. `*_max` are
true poll-interval peaks, so they can exceed any plotted point. The collector
spreads each poll interval's bytes over the minutes it overlaps, so 1-minute
points stay flat for a steady rate at any `port_stats_interval`.

Live per-interface streaming (1 s) and the fleet-wide port snapshot are
delivered over WebSocket — see [§4](#4-websocket-api).

### Flows

Measured traffic from **flow export** (NetFlow v5/v9, IPFIX, sFlow) received by
the backend's UDP collector (`MIKROTIK_NMS_FLOW_LISTEN`, off by default; see
[FLOW-EXPORT.md](FLOW-EXPORT.md)). Every read is open to any logged-in user
(like `/clients`); configuration writes are **admin**. Every admin write makes
the collector re-read its configuration at once.

**Observation points.** Rows are stored per exporter and **never summed across
exporters** (one packet can be seen by several). Every view therefore picks one
*point*: one exporter plus a set `S` of its ifIndexes, with `facing` `up` (the
interfaces face the Internet) or `down` (they face hosts). Direction of a stored
row `(in_if, out_if)`: `up` → download ⇔ `in_if ∈ S`, upload ⇔ `out_if ∈ S`;
`down` → download ⇔ `out_if ∈ S`, upload ⇔ `in_if ∈ S` (a row may count in both).
Download: local = `dst`, remote = `src`; upload the reverse. Addresses are the
NAT-normalised inside view. `native` points are created by the collector (one
per exporter interface that carried traffic); `derived` points are added by an
admin, usually from `/flows/points/suggestions`, and may name a managed port
(`device_id` + `iface`, `port_side` `same` = the exporter's own port, `peer` =
the far end of the wire) so the view appears on that port and is compared with
its port counters. Per-row filters: `local_internal` keeps only rows whose local
endpoint is `internal`/`vpn`; `exclude_attach` drops rows whose local endpoint's
MAC is learned (fresh FDB) on one of the listed ports.

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/flows/status` | any | Collector status (below) plus `suggested_exporters`. |
| GET | `/flows/setup` | any | Copy-paste exporter setup. Query `device` (optional; adds its address as `src-address`). `404 device not found`. |
| GET | `/flows/exporters/{id}/interfaces` | any | `{exporter_id, interfaces: [{if_index, name, type, vlan_id, parent, role, source, hint, first_seen, last_seen}]}`. `source` = `device` (RouterOS `.id` sync), `learned` (seen in flows; `hint` = top ingress source prefixes, e.g. `192.168.78.0/24,192.168.79.0/24`) or `manual`. `404 exporter not found`. |
| POST | `/flows/exporters` | admin | Add an exporter. `201` ExporterStatus. |
| PUT | `/flows/exporters/{id}` | admin | Replace an exporter's configuration. `200` ExporterStatus; `404 exporter not found`. |
| DELETE | `/flows/exporters/{id}` | admin | Delete an exporter (its interfaces, points and templates cascade; its flow history is purged by the next retention sweep). `204`; `404`. |
| PUT | `/flows/exporters/{id}/interfaces/{ifIndex}` | admin | Name an interface and set its role. Body `{name, role}`, `role` ∈ `""`, `lan`, `wan`, `vpn`, `other` (`400 invalid role`); `ifIndex` 1–4294967295 (`400 invalid ifIndex`). Empty name and role hand the row back to the device sync. `200` = the interface object. |
| GET | `/flows/points` | any | `{points: [FlowPointView]}` — every point, disabled ones included, ordered by exporter name, derived before native, name. |
| GET | `/flows/points/suggestions` | any | Derived views worth adding: `{suggestions: [{rule, reason, exists, point}]}` (below). |
| POST | `/flows/points` | admin | Add a point. `201` FlowPointView. |
| PUT | `/flows/points/{id}` | admin | Replace a point (it stops being auto-maintained: `auto` becomes `false`). `200` FlowPointView; `404 point not found`. |
| DELETE | `/flows/points/{id}` | admin | `204`; `404 point not found`. |
| GET | `/flows/top` | any | Top talkers of one point (below). |
| GET | `/flows/port` | any | "Top conversations" of one managed port (below). |
| GET | `/flows/coverage` | any | Flow vs port-counter series of one point (below). |
| GET | `/traffic/sankey?source=flows` | any | Measured Sankey of one point (below). |

**Ranges and windows.** `range` ∈ `live`, `15m`, `1h` (default), `6h`, `24h`,
`7d`, `30d` (else `400 invalid range`). `live` is the last **5 minutes** (flow
records lag the traffic by the exporters' active timeout, ≈ 1–2 min);
`live`–`24h` read the per-minute table (`resolution: "1m"`), `7d`/`30d` the
hourly rollup (`"1h"`). The window ends at the collector's newest flushed minute
(1m) or rolled-up hour (1h) — without a collector, at the newest stored bucket —
and `from = to − window`. A bucket counts as *covered* when the point's exporter
sent datagrams in it; a silent exporter is a gap, not a zero. `coverage_from` is
the first covered bucket in the window (`null` = no flow data yet), `seconds`
the covered seconds, and every average is `avg_bps = round(bytes*8/seconds)`.
Byte and packet figures are sampling-scaled estimates; `sampling_rate` is the
highest 1-in-N in the window (1 = unsampled).

**Status** (`GET /flows/status`; also the `flows.status` WS payload, without
`suggested_exporters`):

```jsonc
{ "enabled": true, "listen": [":2055"], "listen_errors": [],   // "<addr>: <bind error>"
  "started_at": "…Z" | null, "flushed_through": "…Z" | null, "rolled_through": "…Z" | null,
  "rcvbuf_bytes": 4194304,
  "global": { "datagrams": 0, "unknown_datagrams": 0, "rejected": 0, "rate_limited": 0, "queue_dropped": 0,
              "kernel_dropped": 0, "writer_dropped": 0, "late_records": 0, "overflow_records": 0,
              "self_export_dropped": 0, "dup_dropped": 0, "panics": 0 },   // since process start
  "exporters": [ ExporterStatus ],                                        // every exporter, by name
  "unknown_senders": [ { "address": "192.168.78.81", "first_seen": "…Z", "last_seen": "…Z", "datagrams": 12,
                         "protocol": "netflow9", "device_id": null, "device_name": "" } ],   // newest first, ≤ 16
  "suggested_exporters": [ { "address": "192.168.78.81", "name": "firewall003", "kind": "opnsense",
                             "reason": "sending NetFlow v9 (unknown sender)" } ] }
```

`ExporterStatus`:

```jsonc
{ "id": 2, "name": "firewall003", "address": "192.168.78.81", "kind": "opnsense",   // routeros | opnsense | other
  "device_id": null, "enabled": true, "auto": false,          // auto = auto-accepted managed device
  "sampling_override": 0, "nat_addresses": ["192.168.28.81"],
  "state": "ok",                                              // ok (datagram < 3 min ago) | stale | never | disabled
  "last_seen": "…Z" | null, "protocol": "netflow9",           // netflow5 | netflow9 | ipfix | sflow5 | ""
  "sampling_rate": 1, "templates": 2,
  "template_state": "ok",                                     // ok | waiting (data, no template yet) | persisted | n/a
  "per_minute": { "datagrams": 0, "flows": 0, "bytes": 0 },   // 5-min average (0 while the collector is off)
  "counters": { "datagrams": 0, "flows": 0, "bytes": 0, "decode_errors": 0, "template_misses": 0, "replayed": 0,
                "pending_dropped": 0, "seq_lost": 0, "rate_limited": 0, "dup_dropped": 0,
                "self_export_dropped": 0, "nat_dst_records": 0,
                "reboots": 0, "iface_overflow": 0, "oversized": 0 },   // since process start
  "time_source": { "sysuptime": 0, "absolute": 0, "uptime_est": 0, "export": 0 },
  "clock_skew_ms": 0, "last_error": "", "last_error_at": null, "interfaces_seen": 3 }
```

`datagrams`, `flows`, `bytes`, `decode_errors`, `template_misses` and `seq_lost`
are cumulative since the exporter was added (persisted about once a minute); the
other counters count since the backend started. With the collector off every DB
exporter is still listed, from its persisted counters. `suggested_exporters`
lists unknown senders (kind `routeros` for a managed device's address, `opnsense`
for NetFlow v9 from the host of an `opnsense*_url` setting, else `other`) and the
IP-literal hosts of `opnsense*_url` settings (`reason: "configured as OPNsense
(opnsense_url)"`), minus addresses that already are exporters.

**Setup** (`GET /flows/setup`):

```jsonc
{ "enabled": true, "port": 2055,                               // the collector's port; 2055 while it is off
  "advertise_address": "192.168.79.216", "advertise_source": "setting",   // setting | auto | unknown
  "routeros": { "device_id": "c7a3…" | null,
    "apply": ["/ip traffic-flow target add dst-address=192.168.79.216 port=2055 src-address=192.168.78.202 version=ipfix v9-template-timeout=1m",
              "/ip traffic-flow set enabled=yes interfaces=all active-flow-timeout=1m inactive-flow-timeout=15s"],
    "rollback": ["/ip traffic-flow set enabled=no active-flow-timeout=30m",
                 "/ip traffic-flow target remove [find dst-address=192.168.79.216 port=2055]"] },
  "opnsense": { "destination": "192.168.79.216:2055", "version": "v9", "active_timeout": 60, "inactive_timeout": 15,
    "steps": ["Reporting → NetFlow → Capture (enable advanced mode)", "Listening interfaces: LAN",
              "WAN interfaces: leave empty", "Version: v9", "Destinations: add 192.168.79.216:2055",
              "Active Timeout 60, Inactive Timeout 15", "Apply"] } }
```

`advertise_address` is the `flow_advertise_address` setting (`setting`), else
the first `MIKROTIK_NMS_FLOW_LISTEN` entry bound to a specific, non-loopback
address (`auto`: the collector only listens there), else the local address the
backend would use toward the device (or the first exporter / device address)
(`auto`; a connected UDP socket, nothing is sent),
else empty (`unknown`, and the strings contain `<NMS-IP>`). `src-address=` is
included only with `device`.

**Exporter body** (POST / PUT):

```json
{ "name": "firewall003", "address": "192.168.78.81", "kind": "opnsense", "device_id": null,
  "enabled": true, "sampling_override": 0, "nat_addresses": ["192.168.28.81", "192.168.111.81"] }
```

`name` 1–64 characters (`400 name is required`); `address` an IP literal, the
exporter's UDP source (`400 invalid address`; stored unmapped); `kind` ∈
`routeros`, `opnsense`, `other` (`400 invalid kind`); `device_id` an existing
device or `null` (`400 unknown device`); `sampling_override` 0–65535, the 1-in-N
applied when the exporter announces no sampling (`400 invalid
sampling_override`); `nat_addresses`, the addresses this exporter's NATed
traffic appears as at other exporters (`400 invalid nat address`). A duplicate
address is `409 exporter address already exists`. POST defaults `enabled` to
`true` and `sampling_override` to 0; PUT requires both (`400 enabled is
required` / `400 sampling_override is required`) and keeps the stored
`device_id` / `nat_addresses` when they are omitted (`null` clears them).
A managed RouterOS device sending from its `devices.address` is accepted
automatically (`auto: true`) while `flow_auto_accept_devices` is on.

**FlowPointView** (the `point` of every read below):

```jsonc
{ "id": 7, "name": "switch001 · ether1", "kind": "derived",            // native | derived
  "exporter_id": 2, "exporter_name": "firewall003", "exporter_kind": "opnsense",
  "exporter_state": "ok", "protocol": "netflow9",
  "if_indexes": [1], "if_names": ["LAN"],                                // name, or "if#N" when unnamed
  "facing": "down", "port_side": "peer",                                 // "" when no managed port
  "device_id": "0bf4…" | null, "device_name": "switch001" | "", "iface": "ether1" | "",
  "local_internal": true, "exclude_attach": [ { "device_id": "c7a3…", "iface": "eth3" } ],
  "note": "Derived from firewall003 NetFlow …", "auto": false, "enabled": true,
  "sampling_rate": 1,                                                    // the exporter's last effective 1-in-N
  "coverage_capable": true,                                              // has a managed port
  "last_data": "2026-09-28T16:41:00Z" | null }                           // newest minute with bytes on its interfaces
```

**Point body** (POST / PUT; also the `point` of a suggestion):

```json
{ "exporter_id": 2, "name": "switch001 · ether1", "kind": "derived", "if_indexes": [1], "facing": "down",
  "port_side": "peer", "device_id": "0bf4…", "iface": "ether1", "local_internal": true,
  "exclude_attach": [], "note": "…", "enabled": true }
```

`exporter_id` must exist (`400 unknown exporter`); `name` 1–128 characters;
`kind` ∈ `native`, `derived` (`400 invalid kind`); `if_indexes` at least one,
none 0 (`400 if_indexes must be non-empty`), at most 256 distinct (`400 too many
if_indexes (max 256)`); `facing` ∈ `up`, `down` (`400 invalid
facing`); `port_side` ∈ `""`, `same`, `peer` (`400 invalid port_side`) — a
non-empty side needs an existing `device_id` (`400 unknown device`) and an
`iface` (`400 iface is required`); with `port_side: ""` the device and iface are
cleared. `exclude_attach` entries need both fields and an existing device
(`400 invalid exclude_attach` / `400 unknown device in exclude_attach`); `note`
at most 1000 characters. Omitted `local_internal` defaults to `true` for an
`opnsense` exporter (else `false`) on POST and is kept on PUT; omitted `enabled`
defaults to `true` / is kept. A second derived view of the same exporter and port,
or a second native point over the same interfaces, is `409 a view for this port
already exists`.

**Suggestions** (`GET /flows/points/suggestions`):

- `gateway-host` — a managed switch port whose gateway host is a flow exporter
  without a device record (e.g. OPNsense on switch001 `ether1`): the exporter's
  LAN interface (`role: lan`, else the learned interface whose `hint` contains
  the exporter's address) as a derived `down`/`peer` view of that port with
  `local_internal: true`. Without a LAN interface yet the suggestion has
  `point: null` and `reason: "waiting for first flows from <exporter>"`. The note
  mentions any other MACs learned on the port.
- `trunk` — a RouterOS exporter's physical up link to another managed device
  (Phase 1 role `peer`, `uplink`, or a downlink to one device): the VLAN
  interfaces (and the bridge, where it is an untagged VLAN member) routing the
  VLANs the port carries, as two derived `down` views — the far end (`peer`) and
  the router's own port (`same`) — excluding hosts on the router's other ports of
  those VLANs (`exclude_attach`). L2-switched traffic the router does not route
  is invisible, which the note and the coverage figure say.

`exists` is `true` when a derived point of that exporter already names the port.

**Endpoint** and **App** objects:

```jsonc
{ "ip": "192.168.28.33", "class": "internal",       // internal | external | self | multicast | vpn
  "name": "net28-client01", "mac": "BC:24:11:B3:6B:38" | "", "vendor": "",
  "device_id": null,                                // set for a managed device's address
  "attached": { "device_id": "…", "device_name": "switch001", "iface": "ether12" } | null,
  "nat_of": "firewall003" | null }                  // the exporter whose nat_addresses contain this IP
{ "proto": 6, "proto_name": "tcp", "port": 443, "label": "HTTPS" }   // label "" when unknown; 443/udp = "QUIC"
```

Class, first match wins: multicast/broadcast (incl. the broadcast address of an
internal prefix) → `multicast`; the point's exporter's own address → `self`;
inside `flow_external_prefixes` → `external`; inside `flow_internal_prefixes` →
`internal`; the most specific prefix of the address plan (derived from the managed
devices' interface addresses every 15 min) → its class (`transit` shows as
`external`); an IP known to the client cache (`mac_lookup`) → `internal`; else
`external`. RFC 1918 is not a blanket rule. Names: a managed device's identity,
else an exporter's name, else the client cache (host name, DNS name, vendor);
external addresses get a reverse-DNS name only with `resolve=1` **and** the
`flow_resolve_ptr` setting (1.5 s budget per request). `attached` is the port the
MAC is attached at (Phase 1 attachment).

**Top** (`GET /flows/top`). Query `point` (required: `400 point is required`,
`400 invalid point`, `404 point not found`), `range`, `dir` (`download` default \|
`upload`; `400 invalid dir`), `group` (`src`, `dst`, `pair` default, `conv`,
`app`; `400 invalid group`), `limit` (1–200, default 20), `resolve` (`1`).

```jsonc
{ "point": FlowPointView, "range": "1h", "dir": "download", "group": "pair", "resolution": "1m",
  "from": "…Z", "to": "…Z", "coverage_from": "…Z" | null, "seconds": 3540,
  "total_bytes": 123456789, "total_bps": 278953, "sampling_rate": 1, "overflow": 0,
  "rows": [ { "rank": 1, "key": "192.168.28.33|1.1.1.1",
              "src": Endpoint | null, "dst": Endpoint | null, "app": App | null,
              "bytes": 123456, "packets": 100, "flows": 3, "avg_bps": 16460, "share": 0.42 } ],
  "other": { "bytes": 0, "packets": 0, "flows": 0, "avg_bps": 0, "share": 0.0 } }
```

`key` per group: `src` → `<src ip>`, `dst` → `<dst ip>`, `pair` → `<src>|<dst>`,
`conv` → `<src>|<dst>|<proto>/<port>`, `app` → `<proto>/<port>`; `src` rows carry
`src`, `dst` rows `dst`, `pair` both, `conv` both plus `app`, `app` rows `app`
(the rest `null`). `port` is the flow's service port (the well-known one of the
two, else the lower). Rows are ranked by bytes (ties: key); the remainder and the
conversations the collector folded below its per-minute top-N go to `other`.
`share = bytes / total_bytes`. `overflow` counts records that exceeded the
collector's per-minute memory cap (their bytes are in `other`).

**Port** (`GET /flows/port`) backs the port-detail "Top conversations". Query
`device`, `iface` (required: `400 device and iface are required`, `404 device not
found`), `range`, `limit` (1–50, default 10), `point` (optional).

```jsonc
{ "reason": "ok",                                     // ok | no_point | collector_disabled
  "point": FlowPointView | null, "alternatives": [FlowPointView],
  "range": "1h", "resolution": "1m", "from": "…Z" | null, "to": "…Z" | null, "coverage_from": "…Z" | null,
  "seconds": 3540,
  "coverage": { "download": 0.97 | null, "upload": 0.93 | null,
                "flow_down_bytes": 0, "flow_up_bytes": 0, "counter_down_bytes": 0, "counter_up_bytes": 0 },
  "download": { "rows": [FlowRow], "total_bytes": 0, "other": FlowOther },   // group conv
  "upload":   { "rows": [FlowRow], "total_bytes": 0, "other": FlowOther } }
```

The point is the enabled point whose managed port is `(device, iface)`: native
first, then derived, then the lowest id; the others are `alternatives`, and
`point=<id>` picks one of them (`400 point does not cover this port`). Without a
matching point the reply is `200` with `reason` `collector_disabled` (collector
off) or `no_point`, `point: null` and empty lists; a matching point always
answers `ok`, so history stays readable while the collector is off.

**Coverage.** A point with a managed port is compared with that port's counters:
the counter carrying the point's download is `tx` when `(facing = down) XOR
(port_side = peer)`, else `rx` (RouterOS `eth1-wan` up/same → rx; switch001
`ether1` down/peer → rx; RouterOS `net28` down/same → tx). Only buckets covered by
both the exporter and the port-counter collector count; `coverage =
flow_bytes / counter_bytes` per direction, `null` when the counter bytes are 0 or
there is no managed port. Flows count L3 bytes and counters L2, so ≈ 0.85–1.0 is
healthy; low values point at hardware-offloaded / FastTrack traffic, UDP drops
or a template gap.

**Coverage series** (`GET /flows/coverage`). Query `point` (required), `range`.
Steps: `live`–`6h` 60 s, `24h` 300 s, `7d`/`30d` 3600 s; points start at
`coverage_from`.

```jsonc
{ "point": FlowPointView, "range": "1h", "step_seconds": 60, "from": "…Z", "to": "…Z", "coverage_from": "…Z" | null,
  "points": [ { "ts": "…Z", "flow_down_bps": 123 | null, "flow_up_bps": 45 | null,
                "counter_down_bps": 130 | null, "counter_up_bps": 50 | null } ],
  "ratio": { "download": 0.95 | null, "upload": 0.9 | null } }
```

Flow values are `null` for steps the exporter did not cover; counter values are
`null` for steps the port-counter collector did not cover and for points without
a managed port.

**Measured Sankey** (`GET /traffic/sankey?source=flows`). Query `point`
(required), `range`, `dir`, `remote` (`host` default \| `app`; `400 invalid
remote`), `resolve`.

```jsonc
{ "source": "flows", "estimated": false, "direction": "download", "range": "1h", "from": "…Z", "to": "…Z",
  "point": FlowPointView, "sampling_rate": 1, "coverage": 0.96 | null, "total_bps": 123,
  "nodes": [ { "id": "remote:1.1.1.1", "name": "one.one.one.one", "type": "remote", "ip": "1.1.1.1", "class": "external" },
             { "id": "app:6/443", "name": "HTTPS (tcp/443)", "type": "app" },
             { "id": "point:7", "name": "switch001 · ether1", "type": "point" },
             { "id": "host:192.168.28.33", "name": "net28-client01", "type": "host", "ip": "192.168.28.33", "class": "internal" },
             { "id": "port:<dev>:<iface>", "name": "switch001 · ether12", "type": "port", "device_id": "…", "iface": "ether12" },
             { "id": "other:remote", "name": "Other remote (37)", "type": "other" },
             { "id": "other:local", "name": "Other hosts (12)", "type": "other" } ],
  "links": [ { "source": 0, "target": 2, "value": 1000 } ] }
```

Download is layered `remote|app → point → host → port`, upload `port → host →
point → remote|app` (the same nodes, links reversed), so the graph is acyclic.
`remote=host` shows the top 12 remote endpoints, `remote=app` the top 12 apps
(`other:remote` is then named `Other apps (N)`); hosts are the top 15 local
endpoints; `other:*` also carry the conversations the collector folded at
ingest. Each shown host links to its attachment port (top 15 ports). `value` =
`max(1, round(bytes*8/seconds))` in bits/s; zero links are omitted and an empty
window returns `nodes: []`, `links: []`. `coverage` is the chosen direction's
coverage (above).

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
| POST | `/admin/purge-history` | admin | Wipe history tables. Body `{wifi, clients, network_health, traffic, flows, older_than_days}`. Returns `{deleted: {<table>: rows}}`. |
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
`port_stats_interval`, `port_hosts_interval`, `port_stats_1m_days`,
`port_stats_1h_days`, `port_hosts_stale_days`,
`flow_top_n`, `flow_top_n_hourly`, `flow_max_rows_per_minute`, `flow_1m_days`,
`flow_1h_days`, `flow_rate_limit_pps`, `flow_auto_accept_devices`,
`flow_internal_prefixes`, `flow_external_prefixes`, `flow_resolve_ptr`,
`flow_advertise_address`,
`opnsense_url`, `opnsense_api_key`, `opnsense_api_secret`,
`opnsense_verify_tls`.

`older_than_days = 0` (or omitted) on purge means *delete everything* from the
selected tables; the purgeable tables are `wifi_history`, `client_history`,
`loop_events`, and — for `traffic` — `traffic_samples` plus the per-port
traffic analytics history `port_stats_1m` and `port_stats_1h` (deleted in
bounded chunks, so a large purge never locks out other writers) and — for
`flows` — the flow history `flow_1m` and `flow_1h` (counts include their
covered-bucket meta rows; exporter and point configuration is kept). Current-state
tables (including `port_hosts`) are never touched. The backup / export tables
include the flow configuration (`flow_exporters`, `flow_exporter_ifaces`,
`flow_points`) but never the flow history.

Traffic-analytics settings: `port_stats_interval` (s, default 15, 5..300),
`port_hosts_interval` (s, default 300, 60..3600), `port_stats_1m_days` (default
2, min 1 max 90 — the UI reads at most 24 h of 1-minute data), `port_stats_1h_days`
(default 365, 7..1825), `port_hosts_stale_days` (default 7, 1..365). Values are
clamped when read.

Flow-collector settings (re-read within 30 s, no restart): `flow_top_n`
(conversations kept per interface pair and minute, default 50, 10..500),
`flow_top_n_hourly` (default 100, 20..2000), `flow_max_rows_per_minute` (per
exporter, default 1500, 200..10000), `flow_1m_days` (default 3, 1..14),
`flow_1h_days` (default 90, 7..730), `flow_rate_limit_pps` (datagrams/s per
exporter, default 2000, 100..50000), `flow_auto_accept_devices` (default
`true`), `flow_internal_prefixes` / `flow_external_prefixes` (CIDR or IP lists
overriding the endpoint classification), `flow_resolve_ptr` (default `false`;
allows reverse-DNS lookups of external IPs with `resolve=1`),
`flow_advertise_address` (the collector address shown in `/flows/setup`). The
listen address itself is the env var `MIKROTIK_NMS_FLOW_LISTEN` (restart).

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
| `traffic.ports` | object | Fleet-wide port snapshot `{ts, interval_seconds, ready, ports: [{device_id, iface, type, comment?, running, disabled, rx_bps, tx_bps, rx_pps, tx_pps}]}`, once per `port_stats_interval` (default 15 s) while anyone is subscribed. Raw: no `role`/`device_name` (join `/traffic/ports/roles`). |
| `topology.traffic` | object | Per-link throughput for the network map, every 5 s while anyone is subscribed. |
| `firmware.update` | object | Firmware status changed (poll cycle or triggered check). |
| `upgrade.progress.<jobId>` | object | Per-device upgrade progress for one job. |
| `wifi.event` | object | A WiFi join/leave/roam event. |
| `network.health` | object | Network-health poll-cycle summary. |
| `network.health.event` | object | A single new loop/flap/port event. |
| `flows.flushed` | object | `{exporter_ids, bucket, flushed_through}` after each per-minute flow flush (≈ 2–3 min after the minute; refetch `/flows/*` views on it). Only published while anyone is subscribed. |
| `flows.status` | object | The `/flows/status` object without `suggested_exporters`, every 30 s while anyone is subscribed. |

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
