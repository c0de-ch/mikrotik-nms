"use client";

import { ArrowDown, ArrowUp, Flame, Server, type LucideIcon } from "lucide-react";
import { fmtBps } from "@/components/graph/graph-style";
import type { PortRole, PortSnapshot } from "@/lib/api";
import { cn } from "@/lib/utils";
import { deviceLoads, roleFor } from "./lib";

interface Tile {
  label: string;
  value: string;
  hint: React.ReactNode;
  icon: LucideIcon;
  onClick?: () => void;
  title?: string;
}

// KpiRow: live fleet headline numbers from the port snapshot + roles.
// WAN ↓/↑ sum wan-role ports (download = rx on a WAN port); busiest port
// excludes virtual/VPN interfaces; busiest device = Σ(rx+tx)/2 over its
// physical and radio ports.
export function KpiRow({
  snapshot,
  roles,
  anchored,
  nameOf,
  onOpenPort,
  onOpenDevice,
}: {
  snapshot: PortSnapshot | null;
  roles: Map<string, PortRole>;
  anchored: boolean | null;
  nameOf: (deviceId: string) => string;
  onOpenPort: (deviceId: string, iface: string) => void;
  onOpenDevice: (deviceId: string) => void;
}) {
  const ready = !!snapshot && snapshot.ready && snapshot.ts !== null;
  const ports = ready ? snapshot.ports : [];

  let wanDown = 0;
  let wanUp = 0;
  let wanPorts = 0;
  let busiest: { device_id: string; iface: string; total: number } | null = null;
  for (const p of ports) {
    const role = roleFor(roles, p.device_id, p.iface, p.type, p.role);
    if (role === "wan") {
      wanDown += p.rx_bps;
      wanUp += p.tx_bps;
      wanPorts++;
    }
    if (role === "virtual" || role === "vpn") continue;
    const total = p.rx_bps + p.tx_bps;
    if (!busiest || total > busiest.total) busiest = { device_id: p.device_id, iface: p.iface, total };
  }
  let busiestDev: { id: string; throughput: number } | null = null;
  for (const [id, load] of deviceLoads(ports)) {
    if (!busiestDev || load.throughput > busiestDev.throughput) busiestDev = { id, throughput: load.throughput };
  }

  const noWan = wanPorts === 0 || anchored === false;
  const wanHint = noWan ? "No WAN port detected" : `${wanPorts} WAN port${wanPorts === 1 ? "" : "s"} · live`;
  const dash = "—";

  const tiles: Tile[] = [
    { label: "WAN download", value: !ready || noWan ? dash : fmtBps(wanDown), hint: ready ? wanHint : "waiting for samples", icon: ArrowDown },
    { label: "WAN upload", value: !ready || noWan ? dash : fmtBps(wanUp), hint: ready ? wanHint : "waiting for samples", icon: ArrowUp },
    {
      label: "Busiest port",
      value: busiest && busiest.total > 0 ? fmtBps(busiest.total) : dash,
      hint: busiest && busiest.total > 0 ? (
        <>
          {nameOf(busiest.device_id)} · <span className="font-mono">{busiest.iface}</span>
        </>
      ) : (
        "no traffic yet"
      ),
      icon: Flame,
      title: "Highest rx+tx right now (physical and radio ports)",
      onClick: busiest && busiest.total > 0 ? () => onOpenPort(busiest!.device_id, busiest!.iface) : undefined,
    },
    {
      label: "Busiest device",
      value: busiestDev && busiestDev.throughput > 0 ? fmtBps(busiestDev.throughput) : dash,
      hint: busiestDev && busiestDev.throughput > 0 ? nameOf(busiestDev.id) : "no traffic yet",
      icon: Server,
      title: "Σ(rx+tx)/2 over the device's physical and radio ports, right now",
      onClick: busiestDev && busiestDev.throughput > 0 ? () => onOpenDevice(busiestDev!.id) : undefined,
    },
  ];

  return (
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
      {tiles.map((t) => (
        <KpiTile key={t.label} tile={t} />
      ))}
    </div>
  );
}

function KpiTile({ tile: t }: { tile: Tile }) {
  const Icon = t.icon;
  const body = (
    <>
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs font-medium text-muted-foreground sm:text-sm">{t.label}</span>
        <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
          <Icon className="h-3.5 w-3.5" />
        </span>
      </div>
      <div className="text-xl font-semibold tabular-nums sm:text-2xl">{t.value}</div>
      <div className="min-w-0 truncate text-xs text-muted-foreground">{t.hint}</div>
    </>
  );
  const cls = "flex min-w-0 flex-col gap-2 rounded-xl bg-card p-3 text-left text-card-foreground ring-1 ring-foreground/10 sm:p-4";
  if (!t.onClick) {
    return (
      <div className={cls} title={t.title}>
        {body}
      </div>
    );
  }
  return (
    <button
      type="button"
      onClick={t.onClick}
      title={t.title}
      className={cn(cls, "transition-all hover:-translate-y-0.5 hover:shadow-md hover:ring-primary/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary")}
    >
      {body}
    </button>
  );
}
