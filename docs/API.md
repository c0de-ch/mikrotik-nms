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
| `403 Forbidden` | Authenticated but lacking the required role — or a device action whose target lies outside `MIKROTIK_NMS_ALLOWED_TARGET_CIDRS`. |
| `404 Not Found` | Resource does not exist. |
| `409 Conflict` | Duplicate resource (username, device address, flow exporter address, flow view for a port) or setup already done — or an on-demand action on a device whose polling is paused (`{"error": "polling paused until <RFC 3339 time>"}`, see [Devices](#devices)). |
| `500 Internal Server Error` | Unexpected backend / database failure. |
| `503 Service Unavailable` | A device action on an instance whose polling is off (`MIKROTIK_NMS_POLLERS=off`). |

## 2. Authentication

Authentication is JWT-based with an access/refresh token pair, HS256-signed
with keys derived from `MIKROTIK_NMS_JWT_SECRET` — one key per token type, so
an access token never verifies as a refresh token or the other way round
([`auth/jwt.go`](../backend/internal/auth/jwt.go)).

| Token | Lifetime | Claims |
|---|---|---|
| Access | 15 minutes | `typ: "access"`, `aud: ["mikrotik-nms"]`, `uid`, `usr`, `role`, `tv`, `auth_time`, plus `exp`/`iat`/`sub` |
| Refresh | 7 days | `typ: "refresh"`, `aud: ["mikrotik-nms"]`, `uid`, `tv`, `auth_time`, plus `exp`/`iat`/`sub` (subject = user ID) |

`tv` is the user's token version when the token was issued, and `auth_time`
(Unix seconds) is when the user last typed the password. A token is only
accepted with the right `typ` and `aud`, and while its `tv` still equals the
user's current token version (see Revocation).

On `login`/`setup`/`refresh` the server returns:

```json
{
  "access_token": "<jwt>",
  "refresh_token": "<jwt>",
  "expires_at": 1764300000,
  "auth_time": 1764299100
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
re-sets the cookie. A refresh is not a login: the new pair keeps the
session's `auth_time`.

### Revocation

Every authenticated request is checked against the user's current token
version and role in the database, so a session ends at once — not when its
access token expires — when the user **logs out** (`POST /auth/logout` bumps
the version: every session of that user, on every device, is ended), resets
the password, or is deleted. The role applied is the user's current one, not
the one in the token. (An open WebSocket stays open until it reconnects.)

### Fresh login for device-changing actions

`POST /firmware/upgrade`, `/firmware/channel`, `/firmware/routerboard` and
`/devices/address-proposals/{id}/approve` need a password login at most
**5 minutes** old. A session kept alive by
refreshes gets:

```json
{ "error": "this action needs a fresh login: enter your password again", "code": "reauth_required", "max_age_seconds": 300 }
```

with status `403` (never `401`, which clients answer with a silent refresh).
Call `POST /auth/login` again and repeat the request with the new access
token; the web UI does this with a password prompt.

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
| POST | `/auth/logout` | any | Ends every session of the user (bumps the token version, see Revocation) and clears the `refresh_token` cookie. Returns `{"status":"ok"}`. |
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
| GET | `/devices` | any | List all managed devices. Each carries `role` (`router`, `weak-router`, `switch`, `ap-cpe`, `unknown`), `role_reason` (one sentence an operator can read) and `flow_export_eligible` — see [Device role](#device-role). Database only. |
| GET | `/devices/{id}` | any | Get one device (with `role` / `role_reason` / `flow_export_eligible`, and `polling`). `404` if absent. |
| GET | `/devices/{id}/interfaces` | any | Cached interface list for the device. |
| GET | `/devices/{id}/neighbors` | any | Cached `/ip/neighbor` neighbors for the device. |
| GET | `/devices/{id}/ports` | any | The device's faceplate ports (ethernet, SFP, QSFP, combo) with their rates from the port-stats snapshot — **no device call** (the map's port grid). `{device_id, source: "snapshot", sampled_at, age_seconds, interval_seconds, ready, ports: [{name, type, running, disabled, comment, rx_bps, tx_bps}]}`. Inventory order, then any port the snapshot has but the hourly inventory does not yet; running/disabled come from the snapshot when it has the port. `sampled_at` is when **this device's** counters were last read (a device whose reads fail keeps its last rates for up to three intervals, and shows their real age); it and `age_seconds` are `null` (and rates 0) while the snapshot has none of the device's interfaces. `404` for an unknown device. |
| GET | `/devices/{id}/live-limits` | any | What a per-port live stream (WS `traffic.<id>.<iface>`) of the device may do for the caller: `{device_id, class, profile, min_period_seconds, max_ifaces, leases, admin_only, snapshot_interval_seconds, allowed, reason?, message?}`. `class` is the one the caps follow (`C1` on the `light` profile). `allowed` covers role, an admin pause and the polling mode (`reason`: `admin_only`, `polling_paused`, `pollers_off`); the interface and viewer ceilings depend on other viewers and are decided on subscribe. Database only. See [Live streams](#live-streams). |
| GET | `/devices/{id}/load` | any | The device's CPU and memory telemetry next to what the NMS itself sent it. Query `range` (`1h` default, `6h`, `24h`: 1-minute points; `7d`, `30d`: hourly; anything else `400`). **Database only — no device call.** See [Device load](#device-load). `404` for an unknown device. |
| GET | `/devices/{id}/audit` | any | Operator actions on the device (pauses, profile and class changes, address-move decisions, firmware requests — `firmware_upgrade_requested` / `firmware_channel_requested` / `firmware_routerboard_requested`, never with the RouterOS login typed for them), newest first. Query `limit` (default 50, max 500). `[{id, at, user_id, username, action, device_id, detail}]`. |
| GET | `/devices/{id}/findings` | any | What the NMS found on the device, most recently seen first: `[{id, device_id, kind, severity, detail, first_seen, last_seen, count, title?, explanation?, fix?, source?}]`. `severity` ∈ `info`/`warn`/`critical`. The hygiene audit's findings carry `source: "audit"`, a `title`, a plain `explanation` and `fix` — a list of steps `{text, commands?, read_only?, window?}` (`window`: the step interrupts traffic; `read_only`: the commands only print). Query `limit` (default 100, max 1000). |
| GET | `/devices/{id}/hygiene` | any | The hygiene audit's view of the device. **Database only.** See [Hygiene audit](#hygiene-audit). |
| POST | `/devices/{id}/hygiene/audit` | admin | "Audit now": start an audit of the device. `202` with the pass. See [Hygiene audit](#hygiene-audit) for the refusals. Audited (`audit_requested`). |
| POST | `/devices/{id}/hygiene/diagnose` | admin | "Diagnose now": start a diagnose pass. Same answers. Audited (`diagnose_requested`). |
| POST | `/devices` | admin | Add a device. `username` and `password` are **required** (there are no default credentials); with `use_tls` so is `pin_cert_sha256`, the fingerprint of the device's api-ssl certificate (`400` without it, before anything is dialled). Tests the RouterOS connection first; `400` if unreachable — or, over TLS, if the device presents another certificate than the pinned one (no login is sent). `500` when the password cannot be stored encrypted — it is never stored in plaintext instead. |
| PUT | `/devices/{id}` | admin | Update device fields (only non-empty fields override; `use_tls` and `pin_cert_sha256` change only when the body carries them, and an empty `pin_cert_sha256` clears the pin). The stored password changes only when the body carries a new `password`. `400` for a malformed fingerprint (nothing is changed). A change of address, port, TLS flag, pin, username or password closes the device's pooled API session and forgets its reconnect backoff, so the next poll connects with the new values. |
| DELETE | `/devices/{id}` | admin | Remove a device (and its pooled API session). |
| GET | `/devices/address-proposals` | any | IP auto-follow proposals (see below), newest sighting first. Pending only by default; `?status=all` or one status. `[{id, device_id, device_identity, old_address, new_address, mac, first_seen, last_seen, status, decided_by?, decided_at?, result?}]`. Database only. |
| POST | `/devices/address-proposals/{id}/approve` | admin + fresh login | Log in at `new_address` with the device's **stored** credential (a separate one-off session; for a device on API-TLS only after the certificate presented there matched its pin), check board, RouterOS version and the matched MAC, and re-point the device when all hold. `200` with the proposal — `status` `approved`, or `failed` with the reason in `result` (the address is untouched). `409` when the proposal is no longer pending, the device's address changed since (the proposal becomes `superseded`) or its polling is paused; `503`/`403` from the polling fence (the proposal stays pending). Audited (`address_move_approved` / `address_move_refused`). |
| POST | `/devices/address-proposals/{id}/dismiss` | admin | Close a pending proposal without contacting anything. Audited (`address_move_dismissed`). The same move (device, old → new address) is not proposed again for 7 days; after a failed approval, for 24 h. A sighting at another address opens a new proposal. |
| POST | `/devices/{id}/polling/pause` | admin | Pause every poller but liveness for `{"hours": 1…168}` (the UI offers 1 h and 24 h). Returns the device. |
| POST | `/devices/{id}/polling/resume` | admin | Clear a pause. Returns the device. |
| PUT | `/devices/{id}/polling` | admin | `{"profile": "auto"\|"light", "class_override": "C1"\|"C2"\|"C3"\|null}`; omitted keys are kept, `null` clears the override. `400` when the override is more capable than the probed class (overrides may only make polling gentler). Returns the device. |

Every device carries its CPU class (`internal/devclass`): `device_class` is
what the last `/system/resource` probe derived (`""` until probed, then
`C1` weak — MIPS or ≤ 128 MiB, `C2` small ARM, `C3` capable arm64/x86_64),
`class_override` an admin's gentler-only override, and `effective_class` the
class the pollers budget the device as (an unprobed device counts as `C1`).
`poll_profile` is `auto` (the class cadences) or `light` (the `C1` cadences
whatever the class); `polling_paused_until` / `polling_paused` show an
active pause. Pauses and profile/override changes are written to the audit
log with the acting user. `PUT /devices/{id}/polling` validates the whole
body before writing anything: a `400` leaves every field as it was.

#### Hygiene audit

Every 6 hours per device (tier `audit` in `polling.next_runs`) the NMS reads
the device's configuration — read-only prints that each carry a `.proplist`
(or `=count-only=`), one at a time on the pooled session, spaced by class
(every 20 s on `C1`, 5 s on `C2`, 2 s on `C3`) — and reports the settings
known to cost CPU or to be unsafe as findings (`device_findings`,
`source: "audit"`). A scheduled audit does not run while the device's last
quiet-window CPU sample is at or above 80 %, while it is paused, offline or
without an API session, or while polling is off; it is tried again later.
The *diagnose pass* is the shorter list of reads (about a dozen, every 30 s
on `C1`) that covers the usual device-side causes of a high CPU; it runs
once per alert when `cpu_high`, `flow_export_cpu_high` or a confirmed
`flow_export_on_switch` opens, also while the CPU is high, and at most once
per device per hour. No pass reads a secret (SNMP community names,
passwords, keys, scheduler scripts are never asked for) and none changes
anything on a device.

`GET /devices/{id}/hygiene`:

```json
{
  "pollers": "local",
  "running": "",
  "audit": {
    "last": {"id": 41, "device_id": "…", "kind": "audit", "reason": "scheduled",
             "started_at": "…", "finished_at": "…", "status": "complete",
             "reads": 19, "failed_reads": 0,
             "found": [{"kind": "graphing_enabled", "severity": "critical",
                        "title": "Graphing is enabled on a weak device",
                        "detail": "interface rule interface=all store-on-disk=yes"}]},
    "next_run": "2026-01-10T18:08:00Z",
    "available_at": null
  },
  "diagnose": {"last": null, "available_at": null},
  "snapshot": {"taken_at": "…", "hash": "…", "ros_version": "7.20.4"},
  "findings": [ /* the device's audit findings, most severe first, with explanation and fix */ ]
}
```

`running` is the kind of pass in progress (`""`, `audit`, `diagnose`). `last`
is the newest pass that judged something — `status` `complete`, or `partial`
when a read failed (what was read was judged, no snapshot was stored) — else
the newest pass at all (`running`, `aborted`); `reason` is `scheduled`,
`admin` (with `requested_by`) or the alert rule that asked. `next_run` is the
next scheduled audit (absent while none is scheduled). `available_at` is when
an admin may next ask for that kind of pass (`null` = now). `snapshot` says
when the stored configuration snapshot last changed; the snapshot itself is
not served by the API.

`POST /devices/{id}/hygiene/audit` and `/diagnose` (admin) answer `202` with
the pass (`status: "running"`); the result shows up in the view above when it
is through — an audit of a `C1` device takes about six minutes. Unlike the
scheduled audit they run whatever the device's CPU says. Refusals: `404`
unknown device; `409` `polling paused until <time>`, a pass is already
running on the device, or the device is not reachable over the API; `429`
with `Retry-After` within an hour of the device's last pass of that kind
(whoever started it); `503` while polling is off on this instance.

A finding stays until a pass reads the device again and no longer finds the
setting; a pass that could not read a menu (`not enough permissions`, a
failed read) leaves the findings behind that menu alone and reports the
refusal as its own finding (`audit_unreadable`). Finding kinds:
`graphing_enabled`, `traffic_flow_enabled`, `traffic_flow_foreign_target`,
`snmp_open`, `snmp_write_access`, `snmp_on_weak_device`,
`bridge_ip_firewall`, `bridge_cpu_forwarding`, `bond_not_offloaded`,
`bridge_port_not_offloaded`, `bridge_horizon_set`, `multiple_hw_bridges`,
`switch_mirror_to_cpu`, `switch_rules_to_cpu`, `flow_control_enabled`,
`loop_protect_on_trunk`, `switch_filter_rules`, `debug_logging`,
`fast_scheduler`, `sniffer_running`, `cpu_services_on_switch`,
`nms_account_privileged`, `device_mode_permissive`, `audit_unreadable`. A
critical one raises the `audit_critical` alert.

#### Device role

`role` says what the stored facts show the device doing in the network, and
`role_reason` which facts those are. It is derived per request from rows the
pollers already keep — board name and architecture, the probed CPU class, the
device's own fresh egress (`device_uplinks`), the interface list and the
bridge port table — and never costs a device a command. The first matching
rule wins:

| Role | When |
|---|---|
| `switch` | Switch hardware (a `CRS…`, `CSS…`, netPower / netFiber / FiberBox board), whatever its routes. |
| `ap-cpe` | An access-point or CPE board — or a board of unknown family with wireless interfaces — without an internet egress of its own, whose default route points at another router (a private or managed next hop). |
| `router` | A device with an internet or VPN egress of its own (a default route to a public next hop or out an interface, or a running tunnel) and a probed class of `C3`. |
| `weak-router` | The same egress on a `C2` or `C1` device, or on one that has not been probed yet. |
| `unknown` | Everything else: router hardware without an egress of its own (it may be working as a switch), an access-point board the NMS knows no default route on, a board it does not know, a device not read yet. |

The role is deliberately conservative: a default route via a private gateway
or a managed device is not an egress of its own (every switch has one for its
management address), so a device is called a router only when the facts
clearly show an L3 boundary. `flow_export_eligible` is `true` for a `router`,
and for a `weak-router` budgeted as `C2`; a device budgeted as `C1` (C1
hardware, the `light` profile, an admin's `C1` override, not probed yet) is
never eligible. The role gates the flow-export setup hints, auto-accept and
the `flow_export_on_switch` alert (see [Flows](#flows)).

While a pause is in force only the liveness ping reaches the device, and
the on-demand actions that would contact it answer `409` with
`{"error": "polling paused until <RFC 3339 time>"}` before any device I/O:
the speed-test, ping and traceroute run-nows of a target it probes from,
`GET /devices/{id}/addresses`, `POST /firmware/upgrade` / `channel` /
`routerboard` (refused when any listed device is paused; the message names
it, and no job is created), and `GET /debug/wifi`. A firmware job already
running when the pause starts is not interrupted. A granted live stream
stops at its next sample.

#### Device load

`GET /devices/{id}/load` answers "is the NMS why this device is busy?" from
`device_minutes` (one row per device and minute the NMS was polling) and its
hourly rollup `device_hours`:

```json
{
  "device_id": "…", "range": "6h", "resolution": "1m", "step_seconds": 60,
  "from": "…Z", "to": "…Z", "coverage_from": "…Z" | null, "memory_total": 67108864 | null,
  "points": [
    { "ts": "…Z", "gap": null, "covered_s": 60, "paused": false, "rebooted": false,
      "cpu_avg": 12.0, "cpu_max": 14, "cpu_p95": null, "cpu_post": 41, "free_memory": 20971520,
      "nms_cmds": 6, "nms_wall_ms": 310, "nms_rows": 240, "nms_timeouts": 0, "nms_reconnects": 0 }
  ],
  "summary": { "covered_pct": 99.4, "cpu_avg": 11.8, "cpu_max": 63,
               "cpu_post_avg": 38.5, "cpu_quiet_at_post": 12.1,
               "nms_cmds_per_min": 5.9, "nms_wall_ms_per_min": 300.2,
               "nms_timeouts": 0, "nms_reconnects": 1, "reboots": 0 }
}
```

- `cpu_avg` / `cpu_max` are the **quiet-window** samples of the bucket: one
  `/system/resource/print` (`cpu-load`, `free-memory`, `uptime`) per device
  and minute, taken once the device has been left alone for 3 s (after at
  most 10 s of waiting). `cpu_post` is the sample taken 3 s **after an
  interface-counter read**, every 10 minutes: next to the quiet-window value
  it shows what the NMS's own read does to the CPU. `free_memory` is the
  lowest reading of the bucket (bytes).
- `nms_*` is what the NMS sent the device in the bucket: commands, their
  wall time (including the wait for the API session), reply rows, commands
  that timed out, and API sessions that had to be re-opened.
- **Gaps are explicit.** `gap` is `null` when the device's interface counters
  covered at least part of the bucket (`covered_s` > 0). Otherwise it names
  the reason and everything that was not measured is `null`, never `0`:
  `paused` (admin pause), `unreachable` (the liveness check never saw the
  device up), `read_failed` (the counter read failed or timed out),
  `no_session` (no API session), `no_baseline` (the first read after a start,
  a pause or an outage — nothing to compare with yet), `not_polled`, and
  `no_data` — there is no row at all: the NMS was not running, or the
  telemetry did not exist yet. An uncovered bucket with a row still carries
  the `nms_*` figures (timeouts while a device is unreachable are real).
  Hourly points use `not_covered` for an hour with a row but no covered
  second.
- `rebooted` marks a bucket in which the device's uptime went backwards.
- Hourly points (`7d`, `30d`) add `cpu_p95` (of the hour's minute averages);
  `rebooted`, `cpu_post` (the hour's average), `nms_wall_ms`, `nms_rows`,
  `nms_timeouts` and `nms_reconnects` come from the minute rows and are
  `null` for hours older than their 14-day retention.
- `summary.cpu_post_avg` against `summary.cpu_quiet_at_post` is the
  comparison in one pair of numbers: both run over the buckets that have a
  post-poll sample **and** a quiet-window sample (`null` when there is no
  such bucket). The `*_per_min` figures are per recorded minute.

The series ends at the newest final minute (hour), like the port charts: a
device-minute is written together with the port rows of that minute, one to
three minutes after it ends.

**IP auto-follow proposals.** With the `auto_follow_ip` setting on, the
topology loop matches neighbor-discovery (MNDP/CDP) sightings to managed
devices by MAC; a device seen at an address other than the one it is polled
at gets a `pending` proposal (one per device and new address; repeat
sightings bump `last_seen`, and a newer sighting at another address
supersedes the older proposal). The NMS never logs in at a proposed address
on its own: discovery packets are unauthenticated, so sending the stored
credential there needs an admin's approval. Pending proposals become
`superseded` when the device's address changes and `expired` after 24 h
without a sighting. A new proposal is also recorded as an
`ip_address_proposed` network-health event.

Create/update body fields:

```json
{
  "address": "10.0.0.1",
  "identity": "core-router",
  "username": "admin",
  "password": "secret",
  "use_tls": false,
  "pin_cert_sha256": "",
  "api_port": 8728,
  "tags": "[]",
  "notes": ""
}
```

`address`, `username` and `password` are required on create; an omitted
`api_port` falls back to `MIKROTIK_NMS_DEFAULT_ROS_PORT`. On create the
backend dials the device, and if `identity` is blank it reads
`/system/identity`.

**TLS pins.** `pin_cert_sha256` is the SHA-256 fingerprint of the certificate
the device's api-ssl service presents — what RouterOS prints as
`fingerprint` in `/certificate print detail`. It is accepted in upper or
lower case, with or without colons between the bytes, stored as 64
lower-case hex digits, and returned with every device (a fingerprint is not
a secret). With `use_tls` the NMS logs in only when the certificate
presented in the TLS handshake has that fingerprint:

- a TLS device **without a pin** is not contacted over the API at all — the
  pollers skip it, and on-demand actions that would reach it say why
  (`TLS pin missing: …`): `409` from `GET /devices/{id}/addresses`, the
  speed-test and traceroute run-nows and the approval of an address
  proposal; the firmware actions list it as that device's error;
- a device that presents **another certificate** is refused before any
  credential is sent; the refusal is recorded as a `tls_pin_mismatch`
  finding of the device (`GET /devices/{id}/findings`, `detail` = the
  fingerprint presented) and raises the alert of the same name.

Plain-API devices (`use_tls: false`) ignore the pin.

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
| GET | `/traffic/links` | any | Per-link throughput for the network map's first paint, derived from the port-stats snapshot — **no device call**; the continuous feed is WS `topology.traffic`. `{source: "snapshot", sampled_at, age_seconds, interval_seconds, ready, links: [{id, source, target, rx_bps, tx_bps, device_id, iface}]}`: `id` is the `/topology` edge id (an up link, an egress edge `up:<dev>:<iface>:<gw>`, or a gateway's FDB edge `gwhost:<ip>`), rates are in `source`'s frame (rx enters `source`), and `device_id`/`iface` name the end whose counters were read — the end read more recently by over half a counter interval (a device whose reads fail keeps stale rates for up to three intervals), on a tie the online one, then the one on the shorter counter cadence, then the A end. A bridge interface is never read for a link (it counts the device's whole bridged traffic). `age_seconds` is the data's age when the response was built. |
| GET | `/traffic/ports/latest` | any | Fleet-wide port snapshot (`{ts, interval_seconds, ready, ports}`), each port enriched with `device_name` and `role`. `ts` is `null` before the collector's first cycle. |
| GET | `/traffic/ports/top` | any | Top talkers. Query `range`, `metric` (`avg`\|`max`\|`bytes`, default `avg`), `dir` (`total`\|`rx`\|`tx`, default `total`), `limit` (1–200, default 20), `physical` (default on: drops only `virtual` and `vpn`, so every physical port that carried traffic — `peer` links and bond members included — is listed; `0`/`false` also lists virtual and VPN interfaces), `device` (rank every known interface of that device, idle ones as zeros), `sparkline` (default on; `0` skips). |
| GET | `/traffic/ports/roles` | any | Inferred device tree and every port's role: `{anchored, devices: [{device_id, name, parent_id, parent_iface, uplink_iface, depth, anchor}], roles: [{device_id, device_name, iface, role, neighbor_*, client_count, mac_count, gateway_ip, upstream_node, master, behind}]}`. `master` (optional) names the bond a member port belongs to. |
| GET | `/traffic/ports/history` | any | One port's chart + stats. Query `device`, `iface` (required), `range`. Returns `{device_id, iface, range, resolution, step_seconds, from, to, coverage_from, points: [{ts, rx_bps, tx_bps}], stats: {rx_avg, tx_avg, rx_max, tx_max, rx_p95, tx_p95, rx_bytes, tx_bytes}}`. A point's `rx_bps`/`tx_bps` are `null` for a step the collector did not cover (see *Coverage* below). `404` for an unknown device; an unknown interface returns zeros. |
| GET | `/traffic/ports/behind` | any | What sits behind a port. Query `device`, `iface` (required). Returns `{role, neighbor, uplink, client_count, mac_count, clients: [{mac, ip, host_name, vendor, vid, wireless, ap, ssid, signal, attached_device_id, attached_iface, attached_device_name, last_seen}], vlans: [{vid, name, tagged}]}`. `neighbor` is set for `uplink`, `downlink` and `peer`; `uplink` for `wan`, `vpn` and uplinks to a synthetic node (Internet / gateway / VPN). Clients are the non-managed MACs learned on that port (at most 500, attached-here first). |
| GET | `/traffic/path` | any | Internet → device/port path with the live rate of each segment. Query `device` (required), `iface` (optional; adds the sink hop: the child device or the clients). `{device_id, iface, role, anchored, hops: [{kind, id, label, device_id, in_iface, out_iface, client_count, sink, down_bps, up_bps, measured}]}`. A segment into a device that shares its parent port with other children is measured on the device's own uplink. `404` for an unknown device. |
| GET | `/traffic/sankey` | any | **Estimated** source → sink flow tree from port counters + topology (not flow export). Query `source` (`counters`, the default, or `flows` for the **measured** Sankey of one flow observation point — see [Flows](#flows); anything else is `400 invalid source`), `range`, `dir` (`download`\|`upload`, default `download`), `device` (optional subtree root). `{source: "counters", estimated, direction, range, from, to, nodes: [{id, name, type, device_id, iface, client_count}], links: [{source, target, value, source_iface}]}`; integer node indices, values in bits/s. A tree, plus one `local:<device_id>` source node (type `other`, name `Local / east-west`) feeding each non-root device whose outflow exceeds its inflow by ≥ 1 kbps (east-west traffic such as a NAS), so every node conserves flow; such a device has two incoming links. Leaves are `access`/`wireless`/`idle` ports only — never `peer` links or bond members. |
| GET | `/traffic/{deviceId}/{iface}` | any | Historical live-stream samples (recorded while someone streams the interface: every second, or at the class's longer period). Query `from`/`to` (RFC3339, default last 1h), `limit` (1–10000, default 1000). |

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
data yet. For one port (`/traffic/ports/history`) whose device has telemetry
(`device_minutes`) from before the window, `coverage_from` is the window
start: every bucket is then either covered or an explicit gap point.

**Coverage.** A port bucket (minute or hour) is a **gap unless its own device
was covered in it**: the device has a `device_minutes` row with `polled_s` > 0
for the minute (`device_hours.covered_s` > 0 for the hour) — its interface
counters were read on both sides of at least part of the bucket. A device
that was paused, unreachable or not read shows a gap on every port, whatever
the rest of the fleet did; only inside a covered bucket does a port without a
row read `0` (idle). Gaps are points with `rx_bps`/`tx_bps` `null` and are
left out of every average and of the p95. Every average (history stats, top
talkers, Sankey) is `bytes*8 / the device's covered seconds`, and a bucket
covered only in part is divided by its covered seconds.

Buckets older than a device's first `device_minutes` (`device_hours`) row —
history from before this telemetry existed, or beyond its 14-day (730-day)
retention — are judged the earlier, fleet-wide way: a bucket without a single
row for any port means the collector was not running (a gap); any row
anywhere means it was, and a port without one was idle (`0`).

Stored port rows also carry the per-bucket deltas of the drop, error and
link-down counters (`rx_drop`, `tx_drop`, `rx_error`, `tx_error`,
`tx_queue_drop`, `link_downs`; `NULL` where the interface does not report the
counter) and `covered_s`; a port minute is stored when any byte or any of
those deltas is non-zero.

| `range` | Window | Source | History step (points) | Top sparkline step |
|---|---|---|---|---|
| `live` | 5 min (history) / now | `traffic_samples` (history: the stream's samples, 1 s or the class period) / collector snapshot (top, sankey) | stream period, raw | last 30 min of 1m @ 60 s |
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

Live per-interface streaming (1 s on capable devices, capped by class — see
[Live streams](#live-streams)) and the fleet-wide port snapshot are
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
| GET | `/flows/status` | any | Collector status (below) with each sender's device role, the block list and `suggested_exporters`. Database + collector memory only. |
| GET | `/flows/setup` | any | Exporter setup hints, gated by the device's role and class (below). Query `device` (optional; without it no command that enables export is shown). `404 device not found`. Database only — never a device call. |
| GET | `/flows/exporters/{id}/interfaces` | any | `{exporter_id, interfaces: [{if_index, name, type, vlan_id, parent, role, source, hint, first_seen, last_seen}]}`. `source` = `device` (RouterOS `.id` sync), `learned` (seen in flows; `hint` = top ingress source prefixes, e.g. `192.168.78.0/24,192.168.79.0/24`) or `manual`. `404 exporter not found`. |
| POST | `/flows/exporters` | admin | Add an exporter. `201` ExporterStatus. `409` with `code: "sender_blocked"` and the reason when the address is blocked — a deleted exporter's tombstone, or the address of a managed device that must not export — unless the body carries `"unblock": true` (audited as `flow_exporter_added_blocked`). |
| PUT | `/flows/exporters/{id}` | admin | Replace an exporter's configuration. `200` ExporterStatus; `404 exporter not found`; `409 sender_blocked` when the new address is blocked. `enabled: false` only makes the NMS ignore the datagrams — the device keeps exporting. |
| DELETE | `/flows/exporters/{id}` | admin | Delete an exporter (its interfaces, points and templates cascade; its flow history is purged by the next retention sweep) and **block its address**: the datagrams are dropped before decode, the sender is listed under `blocked_senders` with the reason and is never accepted automatically again. Deleting does not stop the device from exporting. `204`; `404`. Audited (`flow_exporter_deleted`). |
| DELETE | `/flows/blocked?address=` | admin | Remove a block: the sender is an unknown sender again. (A managed device that must not export is blocked again by its next datagram; to accept it anyway, POST it with `unblock`.) `204`; `400 invalid address`; `404 sender is not blocked`. Audited (`flow_sender_unblocked`). |
| PUT | `/flows/exporters/{id}/interfaces/{ifIndex}` | admin | Name an interface and set its role. Body `{name, role}`, `role` ∈ `""`, `lan`, `wan`, `vpn`, `other` (`400 invalid role`); `ifIndex` 1–4294967295 (`400 invalid ifIndex`). Empty name and role hand the row back to the device sync. `200` = the interface object. |
| GET | `/flows/points` | any | `{points: [FlowPointView]}` — every point, disabled ones included, ordered by exporter name, derived before native, name. |
| GET | `/flows/points/suggestions` | any | Derived views worth adding: `{suggestions: [{rule, reason, exists, point}]}` (below). The router-trunk rule is only offered for exporters whose device has a router role. |
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
  "global": { "datagrams": 0, "unknown_datagrams": 0, "rejected": 0, "blocked": 0, "rate_limited": 0,
              "queue_dropped": 0, "kernel_dropped": 0, "writer_dropped": 0, "late_records": 0, "overflow_records": 0,
              "self_export_dropped": 0, "dup_dropped": 0, "panics": 0 },   // since process start
  "exporters": [ ExporterStatus ],                                        // every exporter, by name
  "unknown_senders": [ { "address": "198.51.100.9", "first_seen": "…Z", "last_seen": "…Z", "datagrams": 12,
                         "protocol": "netflow9", "device_id": null, "device_name": "",
                         "device_role": "", "device_role_reason": "" } ],   // newest first, ≤ 16
  "blocked_senders": [ { "address": "192.0.2.10", "name": "access-switch", "kind": "routeros",
                         "reason": "CRS326-24G-2S+ is a switch (CRS/CSS hardware): …",
                         "source": "auto",                                  // auto | deleted (a deleted exporter)
                         "device_id": "0bf4…", "device_name": "access-switch", "device_role": "switch",
                         "device_role_reason": "…", "created_by": "",       // "" = the collector
                         "created_at": "…Z", "last_seen": "…Z" | null, "datagrams": 42 } ],   // dropped since blocked
  "auto_accept_classes": "",                                              // "" (off) | "router"
  "suggested_exporters": [ { "address": "198.51.100.9", "name": "firewall", "kind": "opnsense",
                             "reason": "sending NetFlow v9 (unknown sender)" } ] }
```

`ExporterStatus`:

```jsonc
{ "id": 2, "name": "firewall", "address": "198.51.100.9", "kind": "opnsense",   // routeros | opnsense | other
  "device_id": null, "enabled": true, "auto": false,          // auto = accepted automatically
  "device_role": "", "device_role_reason": "",                // the managed device at the address, "" = none
  "export_warning": "",                                       // set when that device must not export (it keeps working)
  "sampling_override": 0, "nat_addresses": ["198.51.100.10"],
  "state": "ok",                                              // ok (datagram < 3 min ago) | stale | never | disabled (= ignored)
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
(opnsense_url)"`), minus addresses that already are exporters, are blocked, or
belong to a managed device that must not export.

**Guardrails.** Flow export costs the exporting device CPU for every packet it
accounts, and only a device that routes has anything to show for it. The
collector and the API therefore judge every managed device by its
[role](#device-role):

- **Accepting.** Nothing is accepted automatically unless the setting
  `flow_auto_accept_classes` is `router`; then the first datagram of a managed
  `router` budgeted as `C3` with a fresh internet or VPN egress creates its
  exporter (`auto: true`). Everything else is an unknown sender until an admin
  adds it.
- **Blocking.** The first flow datagram from the address of a managed
  `switch`, `ap-cpe` or C1 device blocks that address (`source: "auto"`), and
  deleting an exporter blocks its address too (`source: "deleted"`). A blocked
  sender's datagrams are dropped before decode and counted
  (`global.blocked`, `blocked_senders[].datagrams`).
- **Exporters accepted earlier keep working**, whatever their device's role;
  one on a device that must not export carries `export_warning`.
- **Alert.** Flow datagrams from a device that must not export raise
  `flow_export_on_switch` — a warning marked unconfirmed (`confirmed: false`),
  because a UDP source address can be forged. The collector then reads the
  device's `/ip/traffic-flow` settings and targets once (two read-only prints,
  at most once per hour per device, never on a paused device or with polling
  off); only when that read says the device exports does the alert become
  critical, with the rollback steps in `facts.rollback`.

**Setup** (`GET /flows/setup?device=<id>`):

```jsonc
{ "enabled": true, "port": 2055,                               // the collector's port; 2055 while it is off
  "advertise_address": "198.51.100.5", "advertise_source": "setting",   // setting | unknown
  "class": "C3", "role": "router",                             // the device; all "" without ?device=
  "role_reason": "RB5009UG+S+ routes its own internet egress (ether1) and has a capable CPU (class C3)",
  "eligible": true,                                            // routeros.apply is shown
  "warnings": [],                                              // why not, or what to watch
  "advice": ["Do not enable Tools → Graphing on switches; the NMS keeps per-port history (1 min for 2 d, 1 h for 365 d)."],
  "routeros": { "device_id": "c7a3…" | null,
    "check": ["/ip traffic-flow print", "/ip traffic-flow target print"],          // read-only, always present
    "apply": ["/ip traffic-flow target add dst-address=198.51.100.5 port=2055 src-address=192.0.2.1 version=ipfix v9-template-timeout=1m",
              "/ip traffic-flow set enabled=yes interfaces=ether1,bridge,vlan20 active-flow-timeout=1m inactive-flow-timeout=15s"],
    "interfaces_note": "interfaces= lists the routed interfaces the NMS knows on …",
    "rollback": { "fixed": false,                              // true: the device's RouterOS has the 7.25 fix
      "note": "RouterOS 7.23.1 (stable) is older than 7.25: …",
      "steps": [ { "text": "Decide first. Run the profiler over SSH. …", "commands": ["/tool profile duration=30"] },
                 { "text": "Disable the target that points at this collector — only that one.",
                   "commands": ["/ip traffic-flow target disable [find dst-address=198.51.100.5 port=2055]"] },
                 { "text": "Only if enabled=no is still needed: download the update first, …", "optional": true,
                   "warning": "Before RouterOS 7.25, disabling traffic-flow on a busy system can crash the kernel …",
                   "commands": ["/system package update download", "/ip traffic-flow set enabled=no"] },
                 { "text": "Remove the target.",
                   "commands": ["/ip traffic-flow target remove [find dst-address=198.51.100.5 port=2055]"] },
                 { "text": "Verify: …", "commands": ["/ip traffic-flow target print", "/tool profile duration=30"] } ] } },
  "opnsense": { "destination": "198.51.100.5:2055", "version": "v9", "active_timeout": 60, "inactive_timeout": 15,
    "steps": ["Reporting → NetFlow → Capture (enable advanced mode)", "Listening interfaces: LAN",
              "WAN interfaces: leave empty", "Version: v9", "Destinations: add 198.51.100.5:2055",
              "Active Timeout 60, Inactive Timeout 15", "Apply"] } }
```

- **Who gets which commands.** `routeros.apply` is non-empty only for an
  eligible device: a `router` (no warning), or a `weak-router` budgeted as `C2`
  (with a warning). A `switch`, an `ap-cpe`, a device of `unknown` role, any
  device budgeted as `C1` and a request without `device` get `apply: []` — the
  read-only `check` and the `rollback` only — and `warnings` says why.
- **`interfaces=`** is never `all`: it lists the device's routed interfaces as
  far as the NMS knows them (its egress interfaces, then every interface the
  address plan found an address on), or the placeholder
  `<routed-interfaces>` for the operator to fill in.
- **The collector address** is the `flow_advertise_address` setting
  (`advertise_source: "setting"`) and nothing else. It is never detected from
  the process — not from the listen address, not from a route. While it is
  unset, or set to the address of a managed device, `advertise_address` is
  `""` (`"unknown"`), the commands carry `<NMS-IP>` and `warnings` says so.
  `PUT /settings` refuses an address that belongs to a managed device.
- **The rollback** always has the same five steps in the same order: profile
  first (the decision point), disable the one target, switch traffic-flow off
  *only if still needed*, remove the target, verify. Before RouterOS 7.25 —
  read from the device's stored version; an unknown version and a 7.25 beta or
  release candidate count as older — the third step carries a warning (disabling traffic-flow on a busy system
  can crash the kernel) and downloads the update first; from 7.25 it has
  neither. Every `[find …]` names the one target. When the device was seen
  exporting, the target is the one the device itself reported.

**Exporter body** (POST / PUT):

```json
{ "name": "firewall", "address": "198.51.100.9", "kind": "opnsense", "device_id": null,
  "enabled": true, "sampling_override": 0, "nat_addresses": ["198.51.100.10", "203.0.113.9"] }
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
POST also takes `unblock` (default `false`): without it, an address on the
block list or belonging to a managed device that must not export answers
`409 {"error": "sender <address> is blocked: <reason>", "code":
"sender_blocked"}`; with it the block is removed and the exporter is created
in one transaction.

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
first, then derived, then a native point whose exporter sits on a managed
device that is not a router (such a device sees only what its CPU handles, so
an admin's derived view of the port outranks it), then the lowest id; the
others are `alternatives`, and
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

### Alerts

Rules are evaluated every 30 seconds on an instance that polls
(`MIKROTIK_NMS_POLLERS=local`), from the database only. There is at most one
open alert per rule and key. Rules: `device_unreachable`,
`parallel_api_poller`, `cpu_high`, `mem_low` (key: the device id) and
`port_egress_drops` (key `<device id>|<interface>`). The three telemetry
rules judge a window of the device's newest final minutes and treat a minute
that was not measured as unknown, not as healthy: an open alert stays open
while its device is paused, unreachable or not being recorded, and none
opens. An alert whose severity rises is notified again at once. Runbooks:
[`RUNBOOKS.md`](RUNBOOKS.md).

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/alerts` | any | Open alerts plus the ones resolved in the last 7 days: `{alerts: [...], firing, acked, silences: [...], rules: [...], evaluating}`. An alert is `{id, rule, key, device_id?, device_name?, severity, state, first_at, last_at, last_notified_at?, resolved_at?, acked_by?, acked_at?, confirmed, summary, facts?, runbook_url, silenced}`; `severity` ∈ `info`/`warning`/`critical`, `state` ∈ `firing`/`acked`/`resolved`. `evaluating` is false on an instance that polls nothing. |
| POST | `/alerts/{id}/ack` | admin | Acknowledge a firing alert: it stays open and resolves by itself, but is no longer re-notified. `409` when it is not open. |
| POST | `/alerts/silences` | admin | Body `{scope: "device"\|"rule"\|"site", target, hours: 1…720, reason?}` (`target` is a device id or a rule name; none for `site`). A silence stops notifications, never the alert; when it ends, whatever still fires is announced. `201` with the silence. |
| DELETE | `/alerts/silences/{id}` | admin | End a silence now. `204`. |
| GET | `/alerts/channels` | admin | `[{id, name, kind, target_hint, min_severity, enabled}]`. The target (an e-mail address, or a webhook URL — a bearer secret) is stored encrypted and **never returned**; `target_hint` is enough to recognise it. |
| POST | `/alerts/channels` | admin | Body `{name?, kind: "smtp"\|"webhook", target, min_severity?, enabled?}`. `400` for an invalid address or URL; `500` when the target cannot be stored encrypted. |
| PUT | `/alerts/channels/{id}` | admin | Body `{name?, target?, min_severity?, enabled?}`; an omitted `target` keeps the stored one. |
| DELETE | `/alerts/channels/{id}` | admin | `204`. |
| POST | `/alerts/channels/{id}/test` | admin | Send a test notification to that channel, enabled or not: `{ok, message}` (a delivery failure is `ok: false`, not an HTTP error). |

**Rules.** `device_unreachable` (critical): the liveness check reports the
device `offline`, i.e. past `offline_threshold_seconds`. `parallel_api_poller`
(warning): the login watch confirmed a live API session from an address that
is not this NMS. `tls_pin_mismatch` (critical): a device on API-TLS presented
another certificate than the one pinned for it and the NMS withheld its
login; it resolves when the device is pinned to the presented certificate or
taken off TLS, otherwise 2 h 15 min after the last refused handshake. `flow_export_on_switch`: flow datagrams arrive from the
address of a managed device that must not export (a switch, an access point or
CPE, a C1 device) — a **warning** with `confirmed: false` while only the UDP
source address says so, **critical** with `confirmed: true` once the
collector's read of the device's own traffic-flow settings confirmed the
export; `facts.rollback` then carries the rollback steps as text lines (and
`facts.check` the read-only commands while unconfirmed).
`flow_export_cpu_high` (warning): in the 30 minutes after a flow exporter of
a managed device was accepted, its quiet-window CPU reached 80 % in three
minutes or rose 30 points above its 24-hour baseline; it carries
`facts.rollback`, stands for the rest of the 30 minutes and then while the
newest five minutes are still elevated. `audit_critical` (critical): the
device has a critical hygiene finding ([Hygiene audit](#hygiene-audit));
`facts.findings` and `facts.fix` are text lines; it resolves when a pass no
longer finds the setting, never by time.

**Likely cause.** `cpu_high`, `flow_export_cpu_high` and a confirmed
`flow_export_on_switch` start a diagnose pass on the device. What the
device's newest pass (at most two hours old) found is attached as
`facts.likely_cause` (text lines, most severe first — or one line saying
that none of the usual causes was found) with `facts.diagnosed_at`.

`confirmed: false` marks an alert raised on evidence that can be forged or
mistaken; such an alert is never critical.

**Notifications.** A firing alert goes to every enabled channel whose
`min_severity` it reaches: when it starts, every 4 hours while nobody
acknowledged it, and when it resolves (only if its start was delivered). An
alert whose severity rises fires again — an acknowledgement of the lower
severity is dropped — and is notified at once. A firing alert whose likely
cause arrives (`facts.diagnosed_at` changes — the diagnose pass takes a few
minutes) is notified once more, with it; an acknowledged one is not. A
firing alert's mail carries its `facts.likely_cause`, `facts.findings`,
`facts.fix` and `facts.rollback` lines. An
alert that could not be delivered anywhere is tried again at the next
evaluation. E-mail uses the SMTP relay from Settings (no public base URL
needed). A webhook receives a JSON `POST`:

```json
{
  "title": "[NMS] CRITICAL: sw01 (192.0.2.1) is not reachable",
  "message": "sw01 (192.0.2.1) is not reachable",
  "text": "[NMS] CRITICAL: sw01 (192.0.2.1) is not reachable",
  "priority": 8,
  "alert": { "id": 7, "event": "firing", "rule": "device_unreachable", "key": "…", "device_id": "…", "severity": "critical", "state": "firing", "summary": "…", "first_at": "…", "runbook_url": "…", "url": "…", "facts": { } }
}
```

`title`/`message`/`priority` are the fields Gotify reads, `text` the one
Slack-compatible endpoints read; `event` is `firing`, `resolved` or `test`.

### Firmware

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/firmware` | any | Firmware status per device: channel and installed version (from the device's hourly inventory read), the newest version of that channel and `update_available` (from MikroTik's release feed), RouterBOARD firmware. `last_checked` = the device read. |
| GET | `/firmware/releases` | any | The release feed as last fetched: `[{channel, version, build_time, fetched_at}]`. |
| GET | `/firmware/upgrade/{jobId}` | any | Upgrade job state plus per-device progress (`{job, devices}`): what the WS `upgrade.progress.<jobId>` frames said so far. `job.status` ends as `completed` (each device row has its own `status`/`message`) or `failed`. The firmware page reads it while a job runs, because a job can end before a client has subscribed. |
| POST | `/firmware/check` | admin | "Check All": fetch MikroTik's release feed (`NEWESTa7.<channel>`) and recompute every device — **no device is contacted**. At most one fetch per hour: within the hour `200 {"status":"recent","fetched_at","next_fetch"}` (recomputed only), otherwise `202 {"status":"fetching"}`. `503` while polling is off. |
| POST | `/firmware/upgrade` | admin, fresh login | Start an upgrade job. Body `{device_ids: [...], reboot: bool, username, password}`. Returns `202` with `job_id`. `reboot: true` installs (the device reboots) and, once the device is back on the new version, stores its firmware facts read on the job's own session; `false` only downloads — RouterOS installs it on the next reboot. |
| POST | `/firmware/channel` | admin, fresh login | Set the update channel. Body `{device_ids, channel, username, password}`; `channel` ∈ `stable`/`long-term`/`testing`/`development`. |
| POST | `/firmware/routerboard` | admin, fresh login | Upgrade RouterBOOT firmware. Body `{device_ids, reboot, username, password}`. |

`username` + `password` are a RouterOS login typed for that one action
(`400` without them). The server uses them only on dedicated one-shot
sessions to the job's devices — never the pooled poll session, never the
stored poll credential — and never stores, logs or returns them; it drops
them when the action or job ends. The first device that refuses the login
stops the action: the remaining devices are skipped (`"skipped: login
refused by <device> …"`) rather than sent the same wrong password.
`403 reauth_required` when the NMS password login is older than 5 minutes.

Channel/routerboard responses summarize per-device outcomes, e.g.
`{"changed": 2, "errors": ["<device>: login refused by <device> — check the username and password typed for this job"]}`.

### WiFi & MAC Lookup

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/wifi/current` | any | Each tracked client's current AP, enriched with device + MAC-lookup data. |
| GET | `/wifi/history` | any | Join/leave/roam history. Query `mac`, `ap`, or neither (recent); `limit` (1–5000, default 200). |
| GET | `/mac-lookup` | any | Full MAC → IP/host/AP lookup map. |

### Clients

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/clients` | any | The client list from the cache (`mac_lookup`) the scheduled pollers keep: deduped by MAC, DNS-enriched. **No device is contacted.** (This endpoint used to scan the whole fleet on every call; its `limit` and `timeout` parameters are ignored now.) |
| GET | `/clients/cached` | any | The same response as `/clients`. |
| POST | `/clients/refresh` | any | Start an on-demand refresh of the cache: the ARP, DHCP-lease and wifi-registration reads the pollers make anyway, pulled forward once. Rate-limited **fleet-wide to one per 5 minutes**, one at a time. Answers at once: `202 {refresh}`; `409 {error, refresh}` while one is running; `429 {error, refresh}` with a `Retry-After` header inside the interval; `503` with `MIKROTIK_NMS_POLLERS=off`. Poll the list until `refresh.running` is false. |
| GET | `/debug/wifi` | any | Raw WiFi/CAPsMAN registration-table rows for one device. **Requires** query `device_id`. |

The list response is `{clients: [...], total, active, cached: true, updated_at,
age_seconds, refresh, limited: false, timed_out: false}`. `updated_at` /
`age_seconds` are the newest cache entry's (absent for an empty cache);
`limited` and `timed_out` are kept for browser tabs still running a frontend
from before the scan was removed. `refresh` is `{available, running,
started_at?, finished_at?, next_allowed_at?, min_interval_seconds, clients,
devices_read, devices_skipped, timed_out?}`: `available` is false on an
instance that polls nothing, `next_allowed_at` is set while a new refresh
would be refused, and the counters describe the last finished refresh.

A client is `active` until it has gone unseen for `client_inactive_after_seconds`
(app setting); without the setting, for two reads of the fleet's slowest ARP
reader plus 10 minutes, never under 30 minutes — weak devices are read every
30 minutes, so their clients stay active for 70.

A refresh reads a device only as its polling plan allows — nothing on a
paused or offline device, DHCP leases only where the class plan has them
(C3), registration tables only from the menus the capability probe found (a
switch is never asked for one) — plus the external lease sources (Kea,
OPNsense).

### Network Health (bridge / STP / loop detection)

| Method | Path | Role | Description |
|---|---|---|---|
| GET | `/network-health` | any | Bridges (with ports), recent loop events, per-interface port states, and `findings`: every device's hygiene findings ([Hygiene audit](#hygiene-audit)), most severe first, each with `device_name`. |
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
| PUT | `/settings` | admin | Update settings. Body is a flat `{key: value}` map; unknown keys are silently ignored. `400` — and nothing is written — when `flow_advertise_address` is not an IP address or is the address of a managed device, or `flow_auto_accept_classes` is neither empty nor `router`. |
| POST | `/admin/purge-history` | admin | Wipe history tables. Body `{wifi, clients, network_health, traffic, flows, older_than_days}`. Returns `{deleted: {<table>: rows}}`. `traffic` clears `traffic_samples`, `port_stats_1m` / `port_stats_1h` and — because they say which of those buckets were covered — the device telemetry `device_minutes` / `device_hours` (the CPU history of the same period goes with it). |
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
`flow_1h_days`, `flow_rate_limit_pps`, `flow_auto_accept_classes`,
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
`flow_points`) and the block list (`flow_sender_policy`) but never the flow
history.

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
exporter, default 2000, 100..50000), `flow_auto_accept_classes` (default `""`
= nothing is accepted automatically; `router` = a managed router-role C3
device with a fresh internet or VPN egress is accepted on its first datagram),
`flow_internal_prefixes` / `flow_external_prefixes` (CIDR or IP lists
overriding the endpoint classification), `flow_resolve_ptr` (default `false`;
allows reverse-DNS lookups of external IPs with `resolve=1`),
`flow_advertise_address` (the collector address in `/flows/setup` — the only
source of it; empty, or an IP address that is not a managed device's). The
listen address itself is the env var `MIKROTIK_NMS_FLOW_LISTEN` (restart).
`flow_auto_accept_devices`, the accept-every-managed-device switch of earlier
releases, is no longer read or accepted; migration 031 sets it to `false`.

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
`token` **query parameter** ([`auth/middleware.go`](../backend/internal/auth/middleware.go))
— on WebSocket upgrade requests only; every other request must send the
`Authorization` header.
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
server pings every 30s and limits inbound frames to 4096 bytes. A client's
subscribes and unsubscribes are applied in the order it sent them.

**Limits.** Only the topics in the [topic catalog](#topic-catalog) can be subscribed
to, and one connection holds at most **64** subscriptions at a time
([`ws/topics.go`](../backend/internal/ws/topics.go)). A subscribe outside
either limit is not applied and is answered with a `subscribe.error` control
frame whose `data` is `{code, error}` — `code` `unknown_topic`, or
`too_many_subscriptions` (with `max`). Subscribing again to a topic the
connection already holds does not use another slot; unsubscribing frees one.

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

**Control frames.** A topic whose server-side owner vets subscribers (today
only the per-port live stream) answers each subscribe with a frame that has a
`type` — data frames never carry one. (The hub itself sends a
`subscribe.error` for a subscribe it refuses, see Limits above.)

```json
{ "topic": "traffic.<uuid>.ether1", "type": "subscribe.ack",   "timestamp": "…", "data": { "period_seconds": 15, "class": "C1", … } }
{ "topic": "traffic.<uuid>.ether1", "type": "subscribe.error", "timestamp": "…", "data": { "code": "admin_only", "error": "Live view is limited on weak devices — showing 15 s snapshot data", "fallback": "traffic.ports", … } }
```

Every subscribe is answered, a repeated one included. After a
`subscribe.error` the server has dropped that subscribe (only that one: a
subscribe sent after it is answered on its own). A later `subscribe.ack` on
the same topic (new period after a class change) replaces the earlier one.

### Topic catalog

| Topic pattern | Payload | Description |
|---|---|---|
| `device.health` | object | Device updates `{device_id, status, …}`: from the liveness check every health interval (`status`, `last_seen`, `error`), and from the CPU sample once a minute per device that is online, not paused and has an API session (`status: "online"`, `cpu_load`, `memory_pct`, `uptime`). |
| `topology.update` | graph object | Full topology graph (`nodes` + `edges`); the one non-`map` payload. |
| `traffic.<deviceID>.<iface>` | object | Live `/interface/monitor-traffic` samples of one interface `{device_id, interface_name, rx_bps, tx_bps, rx_pps, tx_pps, period_seconds, timestamp}`, within the device class's caps (see below). Vetted: every subscribe gets a `subscribe.ack` or `subscribe.error`. Sampling starts once granted and stops on the last unsubscribe. |
| `traffic.ports` | object | Fleet-wide port snapshot `{ts, interval_seconds, ready, ports: [{device_id, iface, type, comment?, running, disabled, rx_bps, tx_bps, rx_pps, tx_pps}]}` while anyone is subscribed, after each counter cycle that changed the rates (a reading landed or rates expired; devices are read on their own cadences, so several times per `port_stats_interval`, but never from a cycle that read nothing). Raw: no `role`/`device_name` (join `/traffic/ports/roles`). |
| `topology.traffic` | object | Per-link throughput for the network map — the `GET /traffic/links` payload — once per port-counter cycle while anyone is subscribed. Derived from the snapshot: no device call. |
| `firmware.update` | object | A device's update became available (inventory read or release-feed recompute). |
| `upgrade.progress.<jobId>` | object | Per-device upgrade progress for one job. |
| `wifi.event` | object | A WiFi join/leave/roam event. |
| `network.health` | object | Network-health poll-cycle summary. |
| `network.health.event` | object | A single new loop/flap/port event. |
| `flows.flushed` | object | `{exporter_ids, bucket, flushed_through}` after each per-minute flow flush (≈ 2–3 min after the minute; refetch `/flows/*` views on it). Only published while anyone is subscribed. |
| `flows.status` | object | The `/flows/status` object without `suggested_exporters`, every 30 s while anyone is subscribed. |
| `alerts.update` | object | `{at}` whenever an alert opened, resolved, was acknowledged or silenced, or a notification went out: refetch `GET /alerts`. |

The `<deviceID>`, `<iface>`, and `<jobId>` placeholders are substituted with the
concrete IDs you want to follow, e.g. `traffic.<uuid>.ether1` or
`upgrade.progress.<jobId>`.

### Live streams

The per-port stream (`traffic.<deviceID>.<iface>`) is the one UI view that
reads a device on demand, so its cost is capped by the device's CPU class
(`internal/devclass` `LiveCaps`; the class is the effective one, `C1` on the
`light` profile and for an unprobed device). A lease is one connection
streaming from the device; all of a device's granted interfaces are read
together in one `monitor-traffic` call per period.

| Class | Period | Interfaces per device | Leases | Who |
|---|---|---|---|---|
| `C1` (and unprobed / `light`) | ≥ 15 s | 4 | 1 | admins only |
| `C2` | ≥ 5 s | 8 | 2 | any user |
| `C3` | ≥ 1 s | 16 | 4 | any user |

A refused subscribe gets a `subscribe.error` whose `code` is `admin_only`,
`iface_limit`, `lease_limit`, `polling_paused`, `unknown_interface` or
`unknown_device`, with `error` (what to show) and `fallback: "traffic.ports"`
— the port-counter snapshot, which still carries the port's rate. Granted
subscriptions keep their slot; a class override, pause or profile change is
applied within 30 s (re-vetting may then refuse or re-ack). `GET
/devices/{id}/live-limits` answers the role/pause question before
subscribing.

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
  -d '{"device_ids":["<device-uuid>"],"reboot":true,"username":"<routeros-user>","password":"<routeros-password>"}'
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
