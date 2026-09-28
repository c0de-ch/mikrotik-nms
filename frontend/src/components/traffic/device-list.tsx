"use client";

import { useMemo } from "react";
import { ChevronRight, Server } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { fmtBps } from "@/components/graph/graph-style";
import type { Device, PortSnapshot } from "@/lib/api";
import { deviceStatusColor, deviceStatusLabel } from "@/lib/status";
import { deviceLoads, deviceName, snapshotReady } from "./lib";
import { EmptyState, Notice } from "./notice";
import { RateBar } from "./rate-bar";

// DeviceList: the Devices tab without a selection — every device as a card,
// ordered by live throughput (Σ(rx+tx)/2 over physical and radio ports, the
// same number as the "Busiest device" KPI). Devices without live counters
// sort last.
export function DeviceList({
  devices,
  loaded,
  error,
  snapshot,
  onOpenDevice,
}: {
  devices: Device[];
  loaded: boolean;
  // Device-list fetch error; the page shows it (with a retry) above.
  error: string | null;
  snapshot: PortSnapshot | null;
  onOpenDevice: (deviceId: string) => void;
}) {
  const loads = useMemo(() => deviceLoads(snapshot?.ports ?? []), [snapshot]);
  const sorted = useMemo(() => {
    return [...devices].sort((a, b) => {
      const la = loads.get(a.id);
      const lb = loads.get(b.id);
      if (!!la !== !!lb) return la ? -1 : 1;
      const d = (lb?.throughput ?? 0) - (la?.throughput ?? 0);
      return d || deviceName(a).localeCompare(deviceName(b));
    });
  }, [devices, loads]);
  const max = useMemo(() => {
    let m = 0;
    for (const l of loads.values()) m = Math.max(m, l.rx + l.tx);
    return m;
  }, [loads]);
  const ready = snapshotReady(snapshot);

  if (!loaded) {
    return (
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} className="h-28 w-full rounded-xl" />
        ))}
      </div>
    );
  }
  if (devices.length === 0) {
    // A failed fetch is not "no devices": the page-level notice explains it.
    if (error) return <Notice kind="warn">The device list could not be loaded, so there is nothing to show yet.</Notice>;
    return (
      <Card>
        <CardContent>
          <EmptyState icon={Server}>No devices yet — add some on the Devices page.</EmptyState>
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {sorted.map((d) => {
        const load = loads.get(d.id);
        return (
          <button
            key={d.id}
            type="button"
            onClick={() => onOpenDevice(d.id)}
            className="group flex min-w-0 flex-col gap-3 rounded-xl bg-card p-4 text-left text-card-foreground ring-1 ring-foreground/10 transition-all hover:-translate-y-0.5 hover:shadow-md hover:ring-primary/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
          >
            <div className="flex items-start justify-between gap-2">
              <div className="min-w-0">
                <p className="flex items-center gap-1.5 truncate font-medium">
                  <span className={`h-2 w-2 shrink-0 rounded-full ${deviceStatusColor(d.status)}`} title={deviceStatusLabel(d.status)} />
                  <span className="truncate">{deviceName(d)}</span>
                </p>
                <p className="truncate text-xs text-muted-foreground">
                  {d.address}
                  {d.board ? ` · ${d.board}` : ""}
                </p>
              </div>
              <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
            </div>
            {load ? (
              <div className="space-y-1.5">
                <div className="flex items-baseline justify-between gap-2">
                  <span className="text-lg font-semibold tabular-nums">{fmtBps(load.throughput)}</span>
                  <span className="text-xs text-muted-foreground">
                    {load.activePorts} active port{load.activePorts === 1 ? "" : "s"}
                  </span>
                </div>
                <RateBar down={load.rx} up={load.tx} max={max} />
                <div className="flex justify-between font-mono text-[11px] tabular-nums text-muted-foreground">
                  <span>in {fmtBps(load.rx)}</span>
                  <span>out {fmtBps(load.tx)}</span>
                </div>
              </div>
            ) : (
              <p className="text-xs text-muted-foreground">
                {!ready ? "Waiting for the first samples…" : d.status === "online" ? "No live counters yet" : "Device offline — live counters unavailable"}
              </p>
            )}
          </button>
        );
      })}
    </div>
  );
}
