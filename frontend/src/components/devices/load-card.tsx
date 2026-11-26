"use client";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useNow } from "@/hooks/use-now";
import type { Device } from "@/lib/api";
import { fmtIn, isPollingPaused } from "@/lib/device-load";
import { cn } from "@/lib/utils";

// LoadCard: the device's own load (CPU, memory, uptime) beside the NMS
// polling card, with the hardware facts underneath. CPU, memory and uptime
// come from the minute sample (Tier 1: one cheap read per device and minute,
// taken once the NMS has left the device alone for 3 s), so the card says
// when the next one is due — and that the values stand still while polling
// is paused. Their history is the "CPU & NMS load" tab below.

function Gauge({ label, pct, detail }: { label: string; pct: number | null; detail?: string }) {
  return (
    <div>
      <div className="flex items-baseline justify-between gap-3 text-sm">
        <span className="text-muted-foreground">{label}</span>
        <span className="tabular-nums">
          {detail && <span className="mr-2 text-xs text-muted-foreground">{detail}</span>}
          <span className="font-semibold">{pct != null ? `${pct}%` : "—"}</span>
        </span>
      </div>
      <div
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={pct ?? undefined}
        aria-valuetext={pct != null ? `${pct}%` : "not read yet"}
        className="mt-1.5 h-2 overflow-hidden rounded-full bg-muted"
      >
        <div className="h-full rounded-full bg-primary transition-[width]" style={{ width: `${Math.min(pct ?? 0, 100)}%` }} />
      </div>
    </div>
  );
}

function mib(bytes: number): string {
  return `${Math.round(bytes / (1024 * 1024))} MiB`;
}

export function LoadCard({ device }: { device: Device }) {
  const now = useNow();
  const paused = isPollingPaused(device, now);
  const memPct =
    device.memory_used != null && device.memory_total ? Math.round((device.memory_used / device.memory_total) * 100) : null;
  const nextSample = device.polling?.next_runs.cpu ? Date.parse(device.polling.next_runs.cpu) : null;
  const cpu = [
    device.cpu_count ? `${device.cpu_count} core${device.cpu_count === 1 ? "" : "s"}` : "",
    device.cpu_mhz_max ? `${device.cpu_mhz_max} MHz` : "",
  ]
    .filter(Boolean)
    .join(", ");
  const facts: [string, string][] = [
    ["Model", device.board || "—"],
    ["RouterOS", device.ros_version || "—"],
    ["Architecture", device.architecture || "—"],
    ["CPU", cpu || "—"],
    ["Memory", device.memory_total ? mib(device.memory_total) : "—"],
    ["Platform", device.platform || "—"],
  ];

  return (
    <Card>
      <CardHeader>
        <CardTitle>Load</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="space-y-3">
          <Gauge label="CPU" pct={device.cpu_load} />
          <Gauge
            label="Memory"
            pct={memPct}
            detail={device.memory_used != null && device.memory_total ? `${mib(device.memory_used)} of ${mib(device.memory_total)}` : undefined}
          />
          <div className="flex items-baseline justify-between gap-3 text-sm">
            <span className="text-muted-foreground">Uptime</span>
            <span className="tabular-nums">{device.uptime || "—"}</span>
          </div>
          <p className={cn("text-xs", paused ? "text-load-hold" : "text-muted-foreground")}>
            {paused
              ? "Polling is paused: these are the values of the last sample."
              : nextSample !== null && now
                ? `Sampled once a minute, with the device left alone for 3 s first — next sample ${fmtIn(nextSample, now)}.`
                : device.status === "online"
                  ? "Sampled once a minute, with the device left alone for 3 s first."
                  : "Sampled once a minute while the device is online: these are the values of the last sample."}
          </p>
        </div>
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 border-t pt-3 text-sm">
          {facts.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted-foreground">{k}</dt>
              <dd className="min-w-0 break-words">{v}</dd>
            </div>
          ))}
        </dl>
      </CardContent>
    </Card>
  );
}
