# Improvement Backlog

A prioritized, actionable punch-list for MikroTik NMS, produced from a full-codebase
review (security, performance, correctness, testing, code quality, UX, deployment).

Each item has a concrete location and a fix direction. Priorities:

- **P0** — security/correctness gaps to close **before exposing the app beyond a trusted network**.
- **P1** — correctness, reliability, and medium-severity security worth doing soon.
- **P2** — quality, testing, performance polish, UX/a11y, and hygiene.

Legend: ✅ = verified against source during this review · 📋 = reported by review, spot-check before acting.

> **Context:** MikroTik NMS is *read-only* monitoring designed for a trusted management
> network. None of the P0 items are remotely catastrophic on a segmented LAN, but they
> matter the moment the UI/API is reachable by an untrusted client or hosted multi-tenant.

---

## Status (addressed in the `fix/review-findings` PR)

**Fixed:** #1 (AES-256-GCM device password encryption + startup migration), #2 (backup/export
redaction), #3 (configurable CORS allow-list), #4 (WS origin allow-list + token redacted from
logs), #5 (per-IP auth rate limiting), #7 (settings secrets hidden from non-admins), #8
(per-command RouterOS timeout), #9 (WAL checkpoint + daily VACUUM + retention index), #10
(wifi/client-discovery intervals now honored), #11 (opt-in `MIKROTIK_NMS_ROS_TLS_VERIFY`), #12
(atomic first-run setup), #13 (mac_lookup retention pruning), #14 (CI workflow), #15 (firmware
reuses the shared pool), #18 (topology merges health deltas instead of refetching), plus the
`ws.Message.timestamp`, security-headers, JWT-length-warning, and frontend error-boundary items.

**Partial:** #16 (unstructured errors/logging unchanged; this PR adds the CI gate and keeps
the existing panic recovery — structured logging / `/metrics` deferred), #17 (added tests for
the new and highest-value paths; full handler/WS coverage still pending), #20 (`.golangci.yml`
added; `go-routeros` bump and dedupe deferred), #22 (frontend image now non-root + healthcheck;
backend image stays root pending a `/data` volume-ownership migration).

**Deferred (with reason):** #6 — moving tokens out of `localStorage` to cookie-only/in-memory
would break the project's split-origin deployments (docker-compose serves frontend:3000 and
backend:8080 as different origins, and `SameSite=Strict` blocks the refresh cookie cross-site).
Mitigated here via security headers (`frame-ancestors`/`X-Frame-Options`) and the short-lived
access token; a proper fix needs same-origin hosting or a WS-ticket/subprotocol auth change.
Also deferred: removing the unused `cytoscape*` deps (would desync `package-lock.json` without a
network `npm install`), eager-Recharts dynamic import, and the broad a11y aria-label pass.

---

## Summary

| # | Priority | Area | Issue | Verified |
|---|---|---|---|---|
| 1 | P0 | Security | Device RouterOS passwords stored in plaintext (`ENCRYPTION_KEY` unused) | ✅ |
| 2 | P0 | Security | `/admin/backup` & `/admin/export/devices` dump those passwords in cleartext | ✅ |
| 3 | P0 | Security | CORS reflects any origin **with credentials** | ✅ |
| 4 | P0 | Security | WebSocket accepts any origin + takes JWT in `?token=` (log leakage, CSWSH) | ✅ |
| 5 | P0 | Security | No rate limiting / lockout on `/auth/login` & `/auth/refresh` | ✅ |
| 6 | P0 | Security | Frontend keeps **both** tokens in `localStorage` (XSS-readable) | ✅ |
| 7 | P1 | Security | `GET /settings` leaks `opnsense_api_secret` / `kea_url` to any viewer | ✅ |
| 8 | P1 | Reliability | No per-device timeout on RouterOS calls — one hung device stalls a whole poller | ✅ |
| 9 | P1 | Storage | Retention only `DELETE`s; no `VACUUM` — the DB file grows forever | ✅ |
| 10 | P1 | Correctness | `wifi_interval` / `client_discovery_interval` settings are ignored (hardcoded) | ✅ |
| 11 | P1 | Security | RouterOS API-TLS hardcodes `InsecureSkipVerify: true` | ✅ |
| 12 | P1 | Security | `/auth/setup` "0 users" gate is not race-safe | ✅ |
| 13 | P1 | Performance | Several list endpoints return whole tables unbounded | ✅ |
| 14 | P1 | CI | No PR pipeline runs tests / vet / lint / build | ✅ |
| 15 | P1 | Reliability | Firmware upgrades spin up a throwaway connection pool per job | 📋 |
| 16 | P1 | Observability | Swallowed errors everywhere; unstructured logging; no `/metrics` | 📋 |
| 17 | P2 | Testing | No tests for HTTP handlers, WS hub, topology dedup, firmware state machine | ✅ |
| 18 | P2 | Frontend | Topology re-fetches the full device list on every health tick | ✅ |
| 19 | P2 | Hygiene | Unused Cytoscape deps; README/CLAUDE.md describe a graph that isn't rendered | ✅ |
| 20 | P2 | Quality | No `golangci-lint`; stale `go-routeros` dep; duplicated collectors; large files | 📋 |
| 21 | P2 | UX/a11y | Color-only status, icon buttons without labels, no error boundaries | 📋 |
| 22 | P2 | Deployment | Root containers, mutable `:latest`, no frontend probe, weak default secrets | 📋 |

---

## P0 — Close before untrusted exposure

### 1. Device RouterOS passwords are stored in plaintext ✅
`MIKROTIK_NMS_ENCRYPTION_KEY` is loaded into config but **never used** — its only two
references are the struct field and the `os.Getenv` read
(`internal/config/config.go:13,37`). Passwords are written verbatim
(`internal/api/devices.go:116`, and `:170-171` carries a literal `// TODO: encrypt`),
stored in `devices.password_enc`, and read back unencrypted straight into the RouterOS
pool (`internal/poller/manager.go:277`, `internal/poller/firmware.go:84`).

**Fix:** add an `internal/crypto` helper (AES-256-GCM or `nacl/secretbox`) keyed from a
32-byte key derived from `cfg.EncryptionKey`. Encrypt at the write boundary in
`api/devices.go` (create + update), decrypt in a thin accessor before handing the
password to the pool. Validate the key at startup and fail closed when devices exist but
no key is set. Migrate existing plaintext rows on upgrade, then delete the TODO.

### 2. Backups/exports leak the plaintext passwords ✅
`GET /admin/backup` and `GET /admin/export/devices` build rows from
`SELECT * FROM <table>` (`internal/api/admin_backup.go:190`, `:212-219`), which bypasses
the `json:"-"` tag that protects the normal `GET /devices` path. The downloaded JSON
contains every device's cleartext RouterOS password (and bcrypt user hashes). Admin-gated,
so this is an authenticated-admin / data-at-rest disclosure, not an open leak.

**Fix:** maintain a per-table sensitive-column set (`devices.password_enc`,
`users.password_hash`) and strip those keys from each exported row (keep them on
import/restore), or select explicit columns instead of `*`. Solving #1 also de-fangs this.

### 3. CORS reflects any origin with credentials ✅
`internal/api/router.go:35-41` configures `AllowOriginFunc: func(...) bool { return true }`
**and** `AllowCredentials: true`. go-chi/cors then reflects the caller's `Origin` back in
`Access-Control-Allow-Origin`, so any site is trusted with credentials. Partially blunted
by the refresh cookie being `SameSite=Strict` (`internal/api/auth.go:56-58`), but it still
broadly weakens same-origin protection.

**Fix:** replace the always-true function with an explicit allow-list from config
(e.g. `MIKROTIK_NMS_ALLOWED_ORIGINS`). If the frontend is served same-origin, set
`AllowCredentials: false` or drop the CORS handler entirely.

### 4. WebSocket: any origin + JWT in the query string ✅
The upgrade uses `OriginPatterns: []string{"*"}` (`internal/api/ws.go:11-14`), disabling
the library's same-origin (CSWSH) protection, and the token is read from `?token=`
(`internal/auth/middleware.go:69-77`; the frontend sends it at
`frontend/src/lib/ws.ts:79`). Because `middleware.Logger` is enabled globally
(`router.go:33`), the full URL — token included — lands in access logs, and also in proxy
logs and browser history.

**Fix:** (a) validate `Origin` against an allow-list before `websocket.Accept`; (b) stop
putting the JWT in the URL — carry it in the `Sec-WebSocket-Protocol` header, an httpOnly
cookie, or a short-lived single-use ticket. If the query param must stay temporarily,
redact `token` in the request logger and any proxy logs.

### 5. No rate limiting on auth endpoints ✅
`/auth/login`, `/auth/refresh`, `/auth/setup` are registered with no throttling and the
handlers do no attempt-tracking/lockout (`internal/api/router.go:30-48`,
`internal/api/auth.go:35-44`). bcrypt slows each guess but is not brute-force protection.

**Fix:** wrap the public auth routes in a limiter — e.g. `github.com/go-chi/httprate`
`LimitByIP` returning 429 (`middleware.RealIP` is already set) — and add per-account
failed-attempt backoff/lockout. Keep the existing `CountUsers` guard on setup.

### 6. Frontend stores both tokens in `localStorage` ✅
Access **and** refresh tokens are persisted to `localStorage`
(`frontend/src/context/auth.tsx:20-23`, `frontend/src/lib/api.ts:26-27`,
`frontend/src/lib/ws.ts:37-38`). The backend already sets the refresh token as an httpOnly
cookie, but the handlers *also* return it in the JSON body (`api/auth.go:62/112/161`) and
the client persists it — defeating the cookie.

**Fix:** drop `refresh_token` from the response body; have `/auth/refresh` read it solely
from the cookie (send `credentials:"include"`). Hold the access token in memory (module
var / React state) rather than `localStorage` so an XSS payload can't exfiltrate a
long-lived credential.

---

## P1 — Correctness, reliability & medium-severity security

### 7. `GET /settings` discloses integration secrets to viewers ✅
`GET /settings` is registered **outside** the admin group (`internal/api/router.go:138`;
only `PUT /settings` is admin at `:140`) and `handleGetSettings` returns every
`app_settings` row unredacted (`internal/api/settings.go:9-16`) — including
`opnsense_api_secret`, `opnsense_api_key`, and `kea_url` (which may embed credentials). Any
authenticated **viewer** can read them.

**Fix:** redact secret-valued keys (`opnsense_api_secret`, `opnsense_api_key`, and any
credential-bearing URL) for non-admins, or gate `GET /settings` behind `RequireRole("admin")`
and return only the display-safe subset to viewers.

### 8. No per-device timeout on RouterOS calls ✅
`RunCommand` has no deadline (`internal/routeros/client.go:112-118`). Pollers iterate
devices **serially** in one goroutine, so a single hung API call blocks the entire loop
indefinitely while holding that device's client mutex.

**Fix:** thread a `context.WithTimeout` into `RunCommand`/`EnsureConnection` and the
per-device poll step; on timeout, skip the device and continue the loop. Consider a small
worker pool so one slow device can't serialize the whole fleet.

### 9. Retention never reclaims space (no VACUUM) ✅
The retention loop only issues `DELETE`s (`internal/poller/manager.go:537-568`); freed
pages are never returned and the WAL bloats, so the DB file grows monotonically even with
retention working.

**Fix:** run `PRAGMA incremental_vacuum`/periodic `VACUUM` after the retention sweep (or set
`PRAGMA auto_vacuum=INCREMENTAL` at init), and tune `wal_autocheckpoint`. Optional:
`CREATE INDEX idx_traffic_collected_at ON traffic_samples(collected_at)` so the hourly
purge ranges instead of full-scanning the one genuinely large table (all interactive
queries are already well-indexed).

### 10. `wifi_interval` / `client_discovery_interval` settings do nothing ✅
The WiFi tracker and client-discovery poller are constructed with a **hardcoded** 30s / 15m
and never read those `app_settings` keys (`internal/poller/manager.go:50,53`), yet the
Settings page exposes them and CLAUDE.md/README describe them as runtime-tunable.

**Fix:** read `wifi_interval` / `client_discovery_interval` on each cycle like the other
runtime knobs — **or** remove the knobs from the Settings allow-list and correct the docs.
(DEPLOYMENT.md already documents this as a known gap.)

### 11. RouterOS API-TLS skips certificate verification ✅
`tls.Config{InsecureSkipVerify: true}` is hardcoded with no opt-out in both the pool dial
(`internal/routeros/client.go:64-67`) and the add-device test
(`internal/api/devices.go:90`). Medium because TLS is opt-in (default port 8728, plaintext)
— but when enabled it provides no authenticity. The OPNsense client already does this
correctly via a `VerifyTLS` toggle (`internal/opnsense/client.go:55`).

**Fix:** add a per-device `verify_tls` flag (mirroring OPNsense) and/or pin the device cert
fingerprint via `VerifyPeerCertificate`; default to verifying once a CA/fingerprint exists.

### 12. `/auth/setup` first-admin gate is not race-safe ✅
`handleSetup` does count → guard → insert as three separate statements with no transaction
or mutex (`internal/api/auth.go:67-103`), and WAL permits concurrent access. Two concurrent
requests with different usernames can both pass `count==0` and both create admins. Only
exploitable on a fresh 0-user instance.

**Fix:** make it atomic — `INSERT ... SELECT ... WHERE (SELECT COUNT(*) FROM users)=0` and
treat 0 rows-affected as "already set up", or wrap count+insert in a `BEGIN IMMEDIATE`
transaction.

### 13. Unbounded list endpoints ✅
Several read endpoints return entire tables with no pagination:
`GET /network-health` returns all `bridge_status` + `bridge_port_status`
(`internal/api/network_health.go:32-92`); `GET /mac-lookup` (`internal/api/wifi.go:51`) and
`GET /clients/cached` (`internal/api/clients.go:322`) return the whole `mac_lookup` table,
which grows with every client ever seen. Helpers like `deviceNameMap` /
`enrichWifiEntries` also reload `ListDevices` + the full `mac_lookup` per request.

**Fix:** add `limit`/`offset` (or time-window) params and server-side caps; cache the
device-name map across requests rather than rebuilding it each call.

### 14. No CI pipeline on PRs ✅
The only workflows are `docker.yml` (build/push images) and `deploy-lxc.yml` (deploy).
Nothing runs `go test` / `go vet` / `npm run lint` / build on a PR, so a change can merge
with failing tests or lint.

**Fix:** add a `ci.yml` that runs `make test` (ideally `go test -race ./...`), `make lint`,
and `make build` on pull requests; make it a required check.

### 15. Firmware upgrades create a throwaway pool per job 📋
`handleUpgradeFirmware` builds a brand-new `routeros.NewPool()` per job
(`internal/poller/firmware.go:79`), separate from the shared pool — so upgrades re-login
instead of reusing live connections and don't share the per-device mutex with the pollers.

**Fix:** inject and reuse the shared `*routeros.Pool` for upgrades.

### 16. Swallowed errors, unstructured logging, no metrics 📋
Nearly all `queries.*` writes discard errors with `_ =`, and several per-source poller
closures `recover()` without logging (`internal/poller/wifi.go:130`,
`internal/poller/client_discovery.go:88`). Logging is stdlib `log` (unstructured); there is
no `/metrics`. (Panic recovery and `/api/v1/health` do exist.)

**Fix:** log write failures at least at debug/warn; move to `slog` with structured fields;
expose a `/metrics` endpoint (poll durations, error counts, device up/down) for Prometheus.

---

## P2 — Quality, testing, performance polish, UX & hygiene

### 17. Thin test coverage on the load-bearing paths ✅
Good coverage exists for pure functions (log/bridge regex, subnet scan, port-monitor state,
JWT/bcrypt, config, Kea/OPNsense HTTP). But there are **no tests** for: any
`internal/api` HTTP handler, the auth middleware (`RequireAuth`/`RequireRole`), the WS hub
(`internal/ws`), the topology graph builder's dedup/resolution
(`internal/topology/builder_test.go` only tests string helpers, not `Build()`/`resolveNeighbor`),
the firmware upgrade state machine, the RouterOS client, or most pollers.

**Fix:** add `httptest`-based handler tests (auth, devices CRUD, settings allow-list,
backup redaction), a `-race` test for the WS hub, and table-driven tests for topology
canonical `(min_id,max_id)` dedup and MAC→IP→identity priority.

### 18. Topology page full-reloads on every health tick ✅
The topology page re-fetches the entire device list on every `topology.update` **and** every
`device.health` WS message (`frontend/src/app/(authenticated)/topology/page.tsx:108-110`);
at 200 devices pinging every 30s that's constant full reloads + `useMemo` rebuilds.

**Fix:** merge `device.health` deltas into existing state instead of refetching; only refetch
the graph on `topology.update`.

### 19. Dead Cytoscape deps & stale "graph" docs ✅
`cytoscape`, `react-cytoscapejs`, `cytoscape-cola` (+ `@types`) are in `package.json` but
**never imported** anywhere in `src/` — the topology page renders a **Card grid**
(`frontend/src/app/(authenticated)/topology/page.tsx:308`). README ("powered by
Cytoscape.js") and CLAUDE.md describe a graph that isn't drawn. (ARCHITECTURE.md has been
corrected to say "node card-grid view".)

**Fix:** either wire up the Cytoscape graph view (the backend already returns Cytoscape
element format) or drop the unused dependencies; update README/CLAUDE.md to match reality.

### 20. Lint, deps & duplication 📋
- No `golangci-lint` — `make lint` is `go vet` + `eslint` only. Add a `.golangci.yml`
  (errcheck would surface the swallowed errors in #16). Consider `noUncheckedIndexedAccess`
  in `frontend/tsconfig.json`.
- `go-routeros/routeros` is pinned to a **2021 pseudo-version** — review/update the core
  RouterOS API dependency.
- Duplicated ARP/DHCP/CAPsMAN collection + AP-name extraction between
  `internal/api/clients.go` and `internal/poller/client_discovery.go`; the device-name map
  is rebuilt in `network_health.go`, `wifi.go`, and `vlans.go`. Extract shared helpers.
- Large frontend files to split: `settings/page.tsx` (~938 lines), `devices/page.tsx`
  (~641), `network-health/page.tsx` (~604).

### 21. Frontend UX & accessibility 📋
- Device status is conveyed by **color alone** (`frontend/src/lib/status.ts`) — add text /
  `aria-label` / `role="status"` so it's distinguishable without color.
- Icon-only buttons (e.g. WinBox/external-link, edit/delete on `devices/page.tsx`) lack
  `aria-label`s.
- No React **error boundaries** around the authenticated routes — a render crash blanks the
  page. Add `error.tsx` boundaries.
- Performance polish: `recharts` is eagerly imported in the traffic page (consider
  `next/dynamic`); a multi-port device view mounts one live chart per running port
  (`traffic/page.tsx:374`) re-rendering every second — cap or virtualize. Big live tables
  (clients/wifi/network-health) sort/filter in JS each render — memoize.
- `ws.Message.timestamp` is never set on publish (`internal/ws/hub.go`), so clients can't
  rely on it — set it, or drop the field.

### 22. Deployment hardening 📋
- **Run containers as non-root:** neither Dockerfile (`deploy/docker/Dockerfile.backend`,
  `Dockerfile.frontend`) sets a `USER` — both run as root.
- **Pin images by digest in prod:** K8s manifests use `:latest` with `imagePullPolicy:
  Always` (mutable). Consider signing images (cosign) + SBOMs.
- **Frontend has no health probe** in Compose or K8s — add one so a hung Node process is
  detected.
- **Weak default secrets in sample files:** `docker-compose.yml` defaults
  `JWT_SECRET=change-me-in-production`; `deploy/k8s/secret.yaml` ships a placeholder. The
  binary requires a non-empty secret but does **not** enforce a minimum length
  (`internal/config/config.go:51`). Add a length check and keep real secrets out of VCS.
- **Self-hosted runner:** ensure "Require approval for first-time contributors" is enabled
  so fork PRs can't auto-run on the LXC runner.

---

## Notably already-good (verified — not problems)

- The fast-growing time-series tables (`traffic_samples`, `wifi_history`, `client_history`,
  `loop_events`) **are** properly indexed on their `(device_id, …, timestamp)` lookup
  columns; interactive history queries are not full scans.
- The SQLite DB files are correctly **gitignored and never committed** — no data leak, no
  repo bloat.
- The WS hub uses buffered-drop backpressure (256-deep `send` chan) so a slow client can't
  block the publisher, and broadcasts marshal once under `RLock`.
- The refresh cookie is `httpOnly` + `SameSite=Strict`; bcrypt is used for user passwords;
  systemd units apply a strong hardening matrix; deploy paths use constant-time HMAC and a
  narrowly-scoped sudoers grant.
