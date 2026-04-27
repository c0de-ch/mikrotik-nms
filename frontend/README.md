# MikroTik NMS — Frontend

Next.js 15 (App Router) + React + Tailwind CSS v4 + shadcn/ui (base-ui flavor) UI for the MikroTik NMS backend.

For project overview, install paths (Docker / Kubernetes / native LXC), and configuration, see the [top-level README](../README.md).

## Local development

```bash
npm ci
NEXT_PUBLIC_API_URL=http://localhost:8080 \
NEXT_PUBLIC_WS_URL=ws://localhost:8080 \
  npm run dev
```

Dev server runs on http://localhost:3000.

## Build

```bash
npm run build
```

Outputs a Next.js standalone bundle to `.next/standalone/`. The K8s and LXC deploys both use this layout.

## Layout

- `src/app/(authenticated)/` — protected route group; redirects to `/login` without a token.
- `src/app/login/`, `src/app/setup/` — public auth flows.
- `src/lib/api.ts` — typed REST client used by every page.
- `src/lib/ws.ts` — WebSocket client (auto-reconnect, topic subscription).
- `src/hooks/use-websocket.ts` — React hook for sharing one WS connection across components.
- `src/context/auth.tsx` — auth context (login, refresh, logout, current user).
- `src/components/ui/` — shadcn/ui components. **This project uses base-ui, not Radix** — use `render={<Component />}` for composition rather than `asChild`.

## WebSocket topics

The frontend subscribes to topics published by the backend hub:

| Topic | Used by |
|---|---|
| `device.health` | dashboard, device list / detail |
| `topology.update` | topology page |
| `traffic.<deviceId>.<iface>` | traffic page (on-demand) |
| `firmware.update` | firmware page |
| `upgrade.progress.<jobId>` | firmware upgrade progress |
| `wifi.event` | wifi page (live join / leave / roam) |
| `network.health`, `network.health.event` | network-health page |

## Build-time env vars

`NEXT_PUBLIC_API_URL` and `NEXT_PUBLIC_WS_URL` are inlined into the JS bundle by Next.js at build time. Changing them in production requires rebuilding the frontend image (Compose / K8s) or re-running the LXC installer.
