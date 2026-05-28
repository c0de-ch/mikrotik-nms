# Development Guide

This is the contributor / developer guide for **MikroTik NMS** — a web-based
read-only monitoring tool for MikroTik RouterOS devices (Go backend + Next.js
frontend, REST + WebSocket). For deployment and feature docs see the
[README](../README.md) and [`CLAUDE.md`](../CLAUDE.md).

## 1. Prerequisites & first-time setup

| Tool | Version | Notes |
|------|---------|-------|
| Go | 1.25+ | matches `go 1.25.0` in [`backend/go.mod`](../backend/go.mod) |
| Node.js | 22+ | for the Next.js 16 / React 19 frontend |
| npm | bundled with Node | `npm ci` for reproducible installs |
| make | any | thin wrapper over the `go`/`npm` commands |

```bash
git clone https://github.com/c0de-ch/mikrotik-nms.git
cd mikrotik-nms

# Backend deps are resolved on build via the Go module cache (no extra step).
# Frontend deps:
cd frontend && npm ci && cd ..
```

The backend reads configuration from `MIKROTIK_NMS_*` environment variables
(see the [README env table](../README.md#environment-variables)). The only
**required** one is `MIKROTIK_NMS_JWT_SECRET`. SQLite is embedded
(`modernc.org/sqlite`), so there is no external database to install.

## 2. Repo layout

```
mikrotik-c0de/
├── Makefile                       # build / dev / test / lint targets
├── scripts/dev.sh                 # run backend + frontend concurrently
├── backend/
│   ├── go.mod                     # Go 1.25, chi, goose, modernc sqlite
│   └── cmd/
│   │   ├── mikrotik-nms/main.go   # entrypoint: config → DB → ws hub → poller → http
│   │   └── deploy-agent/          # HMAC webhook deploy daemon
│   └── internal/
│       ├── config/                # env-based config (MIKROTIK_NMS_* prefix)
│       ├── database/
│       │   ├── db.go              # Open(): pragmas + goose migrations
│       │   ├── migrations/        # 001_init.sql … 013_loop_event_ack.sql (embedded)
│       │   └── queries/           # raw-SQL query funcs, one .go file per domain
│       ├── auth/                  # JWT, bcrypt, RequireAuth / RequireRole middleware
│       ├── routeros/              # RouterOS API client pool + log parsing
│       ├── kea/ opnsense/         # external lease/IPAM enrichment clients
│       ├── resolver/              # MAC/IP → hostname resolution
│       ├── poller/                # background polling goroutines (manager.go)
│       ├── topology/              # neighbor → dedup link-graph builder
│       ├── ws/                    # topic-based WebSocket pub/sub hub
│       └── api/                   # chi router + REST handlers (router.go wires them)
└── frontend/
    ├── package.json               # next dev / build / start / lint scripts
    └── src/
        ├── app/
        │   ├── login/             # public login / first-run setup
        │   └── (authenticated)/   # protected route group (sidebar layout)
        │       ├── layout.tsx     # redirects to /login when no token
        │       └── dashboard/ devices/ topology/ traffic/ firmware/ wifi/
        │           clients/ network-health/ vlans/ users/ settings/ export/
        ├── components/            # ui/ (shadcn base-ui) + feature components
        ├── context/auth.tsx       # auth state (login/setup/refresh/logout)
        ├── hooks/use-websocket.ts # shared-connection WS subscription hook
        └── lib/
            ├── api.ts             # typed REST client
            ├── ws.ts              # auto-reconnecting WS client
            └── status.ts          # device-status presentation helpers
```

## 3. Running locally

Each command needs the secret/DB env vars; the Make targets bake in dev values.

```bash
# Backend only (binds :8080). MIKROTIK_NMS_JWT_SECRET is required.
make dev-backend
# == cd backend && MIKROTIK_NMS_JWT_SECRET=dev-secret \
#      MIKROTIK_NMS_DB_PATH=mikrotik-nms.db go run ./cmd/mikrotik-nms/

# Frontend only (binds :3000), pointed at the local backend.
make dev-frontend
# == cd frontend && NEXT_PUBLIC_API_URL=http://localhost:8080 \
#      NEXT_PUBLIC_WS_URL=ws://localhost:8080 npm run dev

# Both at once (foreground, kills both on Ctrl-C):
./scripts/dev.sh     # or: make dev
```

Open <http://localhost:3000>. On the **first run with zero users**, the login
page shows the setup flow, which calls `POST /api/v1/auth/setup` to create the
initial **admin** account. That endpoint only works while no users exist; after
that, use the normal login. New users are created from the **Users** page
(admin only).

> `NEXT_PUBLIC_*` values are inlined into the bundle at build time — changing
> them requires a frontend rebuild.

## 4. Build / test / lint

| Target | Runs |
|--------|------|
| `make build` | backend binary + frontend `.next` |
| `make build-backend` | `CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/mikrotik-nms ./cmd/mikrotik-nms/` |
| `make build-frontend` | `npm run build` (Next.js production build) |
| `make test` | `cd backend && go test ./...` |
| `make test-verbose` | `go test -v ./...` |
| `make lint` | `go vet ./...` **and** `npm run lint` (eslint) |
| `make docker-build` / `docker-up` / `docker-down` | Docker Compose lifecycle |
| `make clean` | removes `bin/`, the dev DB, and `frontend/.next` |

Run a **single Go test package** (per [`CLAUDE.md`](../CLAUDE.md)):

```bash
cd backend && go test -v ./internal/topology/ -run TestBuildLinks
```

Frontend lint / build:

```bash
cd frontend && npm run lint
cd frontend && npm run build
```

## 5. Backend how-tos

### Add a DB migration

Migrations are goose SQL files in
[`backend/internal/database/migrations/`](../backend/internal/database/migrations),
embedded via `//go:embed migrations/*.sql` and run with `goose.Up` on every
`database.Open`. Numbering is **zero-padded sequential** (`NNN_description.sql`).
The next file after `013_loop_event_ack.sql` is `014_<name>.sql`.

```sql
-- +goose Up
CREATE TABLE my_table (
    id   TEXT PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
DROP TABLE IF EXISTS my_table;
```

`PRAGMA foreign_keys=ON` is set, so declare FKs accordingly. SQLite's `DROP
COLUMN` is unavailable on older engines, so additive `ALTER TABLE ... ADD
COLUMN` migrations typically use a table-rebuild in their `-- +goose Down`
(see `007_mac_lookup_wifi_fields.sql` for the pattern).

### Add a query function

Query functions live in
[`backend/internal/database/queries/`](../backend/internal/database/queries),
one file per domain (`devices.go`, `dns.go`, …). There is **no ORM** — raw SQL
against `*sql.DB`. Define the row struct (with `json` tags) and CRUD functions
in the matching file:

```go
func ListDNSServers(db *sql.DB) ([]DNSServer, error) {
    rows, err := db.Query(`SELECT id, name, address, port, enabled, created_at FROM dns_servers ORDER BY created_at`)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var servers []DNSServer
    for rows.Next() {
        var s DNSServer
        if err := rows.Scan(&s.ID, &s.Name, &s.Address, &s.Port, &s.Enabled, &s.CreatedAt); err != nil {
            return nil, err
        }
        servers = append(servers, s)
    }
    return servers, rows.Err()
}
```

Always `defer rows.Close()` and return `rows.Err()`. For "not found" on a
delete/update, return `sql.ErrNoRows` so the handler can map it to a 404.

### Add a REST endpoint

Handlers are methods on `*Server` in
[`backend/internal/api/`](../backend/internal/api), one file per domain. Use the
helpers in `helpers.go` — `decodeJSON`, `writeJSON`, `writeError` — and
`r.PathValue("id")` for path params:

```go
func (s *Server) handleCreateDNSServer(w http.ResponseWriter, r *http.Request) {
    var req struct {
        Name    string `json:"name"`
        Address string `json:"address"`
    }
    if err := decodeJSON(r, &req); err != nil {
        writeError(w, http.StatusBadRequest, "invalid request body")
        return
    }
    if req.Address == "" {
        writeError(w, http.StatusBadRequest, "address is required")
        return
    }
    srv := &queries.DNSServer{ID: uuid.NewString(), Name: req.Name, Address: req.Address}
    if err := queries.CreateDNSServer(s.db, srv); err != nil {
        writeError(w, http.StatusInternalServerError, "failed to create DNS server")
        return
    }
    writeJSON(w, http.StatusCreated, srv)
}
```

Wire it in [`router.go`](../backend/internal/api/router.go) under the
`/api/v1` route tree. Authenticated routes go inside the
`r.Use(auth.RequireAuth(cfg.JWTSecret))` group; mutating endpoints go inside a
nested `r.Group` with `r.Use(auth.RequireRole("admin"))`:

```go
r.Get("/dns", s.handleListDNSServers)          // any authenticated user
r.Group(func(r chi.Router) {
    r.Use(auth.RequireRole("admin"))
    r.Post("/dns", s.handleCreateDNSServer)    // admin only
    r.Put("/dns/{id}", s.handleUpdateDNSServer)
})
```

### Add a background poller

Pollers live in [`backend/internal/poller/`](../backend/internal/poller) and are
launched from `Manager.Start()` in
[`manager.go`](../backend/internal/poller/manager.go). Each is a goroutine that
loops on a `time.Ticker` and exits on `ctx.Done()`:

```go
func (m *Manager) myLoop(ctx context.Context) {
    ticker := time.NewTicker(m.cfg.MyInterval)
    defer ticker.Stop()
    m.doMyWork(ctx)            // run once immediately on start
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            m.doMyWork(ctx)
        }
    }
}
```

Add `go m.myLoop(ctx)` to `Start()`. Interval source matters: env-driven
intervals (read from `cfg`) need a backend restart; `app_settings`-driven
intervals are picked up on the next cycle (see the per-poller notes in
[`CLAUDE.md`](../CLAUDE.md)). Wrap per-device work in a `recover()` guard like
`safePollDevice` so one bad device can't crash the loop.

### Add a WebSocket topic

The hub is [`backend/internal/ws/`](../backend/internal/ws). Topics are plain
strings; there is no registry — publishers and subscribers just agree on the
name. Publish from a poller via the injected `*ws.Hub`:

```go
m.hub.Publish("network.health", map[string]interface{}{
    "bridges":    totalBridges,
    "new_events": totalEvents,
    "polled_at":  cycleStart.Format(time.RFC3339),
})
```

Existing topics: `device.health`, `topology.update`, `traffic.<id>.<iface>`,
`firmware.update`, `upgrade.progress.<jobId>`, `wifi.event`, `network.health`,
`network.health.event`. Clients subscribe by sending
`{"action":"subscribe","topic":"<name>"}`; messages are delivered as
`{"topic":"<name>","data":...}`.

## 6. Frontend how-tos

### Add an authenticated page

Create `frontend/src/app/(authenticated)/<name>/page.tsx`. The route group's
[`layout.tsx`](../frontend/src/app/(authenticated)/layout.tsx) already handles
auth (redirects to `/login` when there is no token) and renders the sidebar, so
the page just reads `token` from `useAuth()` and fetches its data. Add the
sidebar link in
[`components/layout/app-sidebar.tsx`](../frontend/src/components/layout/app-sidebar.tsx).
Start the file with `"use client";`.

### The shadcn / base-ui `render={}` gotcha

This project's shadcn components are built on **base-ui, not Radix**. There is
**no `asChild`** — use the `render={}` prop to compose a trigger with another
component:

```tsx
// Correct (base-ui):
<DialogTrigger render={<Button variant="outline" />}>Add device</DialogTrigger>
<Button render={<a href="http://..." target="_blank" />}>Open WebFig</Button>

// Wrong — asChild does nothing here:
<DialogTrigger asChild><Button>…</Button></DialogTrigger>
```

### Calling the API

Use the typed client in [`lib/api.ts`](../frontend/src/lib/api.ts). It prefixes
`/api/v1`, injects the `Authorization` header, and **auto-refreshes** the access
token on a 401 (deduplicating concurrent refreshes). Most methods take `token`
as the first argument:

```tsx
const { token } = useAuth();
const devices = await api.devices.list(token);
await api.devices.create(token, { name, address /* … */ });
```

Add new endpoints as methods on the exported `api` object, with request/response
`interface`s defined alongside.

### Subscribing to WebSocket topics

Use the [`useWebSocket`](../frontend/src/hooks/use-websocket.ts) hook. It shares
a single `NmsWebSocket` connection across all subscribers (ref-counted) and
auto-reconnects with backoff and token refresh:

```tsx
useWebSocket("device.health", (data) => {
  const update = data as { device_id: string; status: string; cpu_load?: number };
  setDevices((prev) => prev.map((d) => (d.id === update.device_id ? { ...d, ...update } : d)));
});
```

Pass a stable handler (wrap in `useCallback` if it closes over changing state)
to avoid re-subscribing every render.

### Device-status helpers

Always render device status through [`lib/status.ts`](../frontend/src/lib/status.ts)
so the green / red / gray triad stays consistent. Any status that is not
`online` or `offline` (including `unknown`) is shown as **"not responding"**
(gray):

```tsx
import { deviceStatusLabel, deviceStatusBadgeClass, deviceStatusColor } from "@/lib/status";

<Badge variant="outline" className={deviceStatusBadgeClass(d.status)}>
  {deviceStatusLabel(d.status)}
</Badge>
<span className={`h-2 w-2 rounded-full ${deviceStatusColor(d.status)}`} />
```

## 7. Testing patterns

Go tests sit next to the code they cover (`*_test.go` in the same package) —
e.g. `internal/topology/builder_test.go`, `internal/auth/jwt_test.go`,
`internal/poller/port_monitor_test.go`, `internal/database/db_test.go`. The
preferred style is **table-driven** with `t.Run` subtests:

```go
func TestInferDeviceType(t *testing.T) {
    tests := []struct {
        board string
        want  string
    }{
        {"CCR2004-1G-12S+2XS", "router"},
        {"CSS610-8G-2S+IN", "switch"},
        {"cAP ac", "ap"},
        {"", "unknown"},
    }
    for _, tt := range tests {
        t.Run(tt.board, func(t *testing.T) {
            if got := inferDeviceType(tt.board); got != tt.want {
                t.Errorf("inferDeviceType(%q) = %q, want %q", tt.board, got, tt.want)
            }
        })
    }
}
```

Tests that need a database use a temp file via `t.TempDir()` and the real
`database.Open` so migrations run (see `db_test.go`). There is currently no Go
test for the API/handlers layer, and the frontend has no test runner — `npm run
lint` is the frontend gate.

## 8. Git workflow

- Work on a **feature branch**, never commit directly to `main`.
- Open a **pull request to `main`**, and **wait for explicit merge approval** —
  do not self-merge.
- Commit / PR titles follow the observed conventional-ish style: a short
  scope-prefixed subject, e.g.

  ```
  Network health: stop flagging loop-protect "off" ports as errors (#32)
  Discovery: deep scan for non-adjacent devices (subnet sweep + unmanaged neighbors) (#31)
  Firmware: stop incomplete checks from blanking known versions + make Check All real (#29)
  ```

  i.e. `<Area>: <imperative summary> (#PR)`.

## 9. Coding conventions

**Backend (Go)**

- **Raw SQL, no ORM.** Keep all SQL in `internal/database/queries/`; handlers
  call query functions, never write SQL inline.
- **Wrap errors with context** at boundaries using `fmt.Errorf("doing X: %w",
  err)` (see `database.Open`); return bare `err` from thin query helpers.
- Handlers map errors to status codes via `writeError`; never leak internal
  error strings — return a short human message and log details server-side.
- Long-running goroutines take a `context.Context` and exit on `ctx.Done()`;
  guard per-item work with `recover()` where a panic must not kill the loop.

**Frontend (TypeScript)**

- `strict` TypeScript — no implicit `any`. Cast WS payloads to an explicit
  shape (`data as { … }`) since `useWebSocket` delivers `unknown`.
- Interactive pages/components are client components (`"use client";`).
- Reach the backend only through `lib/api.ts` and `lib/ws.ts`; do not hit
  `fetch` / `WebSocket` directly from components.
- Use the shadcn/base-ui primitives in `components/ui/` and the `render={}`
  composition prop (never `asChild`).
