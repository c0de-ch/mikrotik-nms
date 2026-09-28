"use client";

import { Globe, Network, Users, Wifi } from "lucide-react";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { fmtBps } from "@/components/graph/graph-style";
import type { PortRoleName, TopDir, TopMetric, TopPortRow } from "@/lib/api";
import { cn } from "@/lib/utils";
import { fmtBytes, metricPair, splitDirection } from "./lib";
import { RateBar } from "./rate-bar";
import { RoleBadge } from "./role-badge";
import { Sparkline } from "./sparkline";

function BehindIcon({ role }: { role: PortRoleName }) {
  const cls = "h-3 w-3 shrink-0";
  if (role === "wan") return <Globe className={cls} />;
  // uplink / peer: the server's summary already starts with "↑" / "↔".
  if (role === "downlink") return <Network className={cls} />;
  if (role === "wireless") return <Wifi className={cls} />;
  if (role === "access") return <Users className={cls} />;
  return null;
}

// PortTable is the ranked port list shared by Top talkers (fleet, with the
// device column) and the device view's port ranking. Rows open port detail;
// the device name opens the device view.
//
// dir: when the list is ranked by raw rx or tx, every row shows raw in/out
// (and emphasises the ranked side) instead of role-mapped ↓/↑ — otherwise
// the ranked value hops between the two columns and the order looks random.
export function PortTable({
  rows,
  metric,
  dir = "total",
  live,
  showDevice,
  loading,
  onOpenPort,
  onOpenDevice,
}: {
  rows: TopPortRow[];
  metric: TopMetric;
  dir?: TopDir;
  live: boolean;
  showDevice: boolean;
  loading?: boolean;
  onOpenPort: (deviceId: string, iface: string) => void;
  onOpenDevice?: (deviceId: string) => void;
}) {
  const raw = dir === "rx" || dir === "tx";
  const effMetric: TopMetric = live && metric === "bytes" ? "avg" : metric;
  const fmt = effMetric === "bytes" ? fmtBytes : fmtBps;
  let max = 0;
  for (const r of rows) {
    const p = metricPair(r, effMetric);
    max = Math.max(max, p.rx + p.tx);
  }

  if (loading && rows.length === 0) {
    return (
      <div className="space-y-2 py-2">
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} className="h-9 w-full" />
        ))}
      </div>
    );
  }

  const metricLabel = live ? "Now" : effMetric === "avg" ? "Average" : effMetric === "max" ? "Peak" : "Volume";

  return (
    <Table className={cn("min-w-[760px]", loading && "opacity-60")}>
      <TableHeader>
        <TableRow className="text-xs">
          <TableHead className="w-8 text-right">#</TableHead>
          <TableHead>{showDevice ? "Device · port" : "Port"}</TableHead>
          <TableHead className="w-56">{metricLabel}</TableHead>
          <TableHead>Behind</TableHead>
          {!live && effMetric !== "max" && <TableHead className="text-right">Peak</TableHead>}
          {!live && effMetric !== "bytes" && <TableHead className="text-right">Volume</TableHead>}
          <TableHead className="w-[104px]">{live ? "Last 30 min" : "Trend"}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((r) => {
          const pair = metricPair(r, effMetric);
          const s = splitDirection(raw ? undefined : r.role, pair.rx, pair.tx);
          const peak = Math.max(r.rx_max, r.tx_max);
          const open = () => onOpenPort(r.device_id, r.iface);
          return (
            <TableRow
              key={`${r.device_id}/${r.iface}`}
              tabIndex={0}
              aria-label={`Open ${showDevice ? `${r.device_name} ` : ""}${r.iface}`}
              onClick={open}
              onKeyDown={(e) => {
                // Only the row itself: Enter on the nested device button must
                // not also open the port (two history entries).
                if (e.target !== e.currentTarget) return;
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  open();
                }
              }}
              className={cn("cursor-pointer", !r.running && "text-muted-foreground")}
            >
              <TableCell className="text-right text-xs tabular-nums text-muted-foreground">{r.rank}</TableCell>
              <TableCell className="max-w-[260px]">
                <div className="flex min-w-0 flex-col gap-0.5">
                  <div className="flex min-w-0 items-center gap-1.5">
                    {showDevice && onOpenDevice && (
                      <>
                        <button
                          type="button"
                          className="min-w-0 truncate font-medium hover:underline"
                          title={`Open ${r.device_name}`}
                          onClick={(e) => {
                            e.stopPropagation();
                            onOpenDevice(r.device_id);
                          }}
                          onKeyDown={(e) => e.stopPropagation()}
                        >
                          {r.device_name}
                        </button>
                        <span className="text-muted-foreground">·</span>
                      </>
                    )}
                    {/* Only the device name truncates — the port must stay readable. */}
                    <span className={cn("font-mono text-xs", showDevice ? "shrink-0" : "truncate")} title={r.iface}>
                      {r.iface}
                    </span>
                  </div>
                  <div className="flex items-center gap-1.5">
                    <RoleBadge role={r.role} />
                    {!r.running && <span className="text-[11px]">down</span>}
                  </div>
                </div>
              </TableCell>
              <TableCell className="min-w-[180px]">
                <div className="flex flex-col gap-1">
                  <RateBar down={s.down} up={s.up} max={max} />
                  <div className="flex justify-between gap-2 font-mono text-[11px] tabular-nums text-muted-foreground">
                    <span className={cn(dir === "rx" && "font-semibold text-foreground")}>
                      {s.directional ? "↓" : "in"} {fmt(s.down)}
                    </span>
                    <span className={cn(dir === "tx" && "font-semibold text-foreground")}>
                      {s.directional ? "↑" : "out"} {fmt(s.up)}
                    </span>
                  </div>
                </div>
              </TableCell>
              <TableCell className="max-w-[220px]">
                {r.behind ? (
                  <span className="flex min-w-0 items-center gap-1 text-xs" title={r.behind}>
                    <BehindIcon role={r.role} />
                    <span className="truncate">{r.behind}</span>
                  </span>
                ) : (
                  <span className="text-xs text-muted-foreground">—</span>
                )}
              </TableCell>
              {!live && effMetric !== "max" && (
                <TableCell className="text-right font-mono text-xs tabular-nums">{fmtBps(peak)}</TableCell>
              )}
              {!live && effMetric !== "bytes" && (
                <TableCell className="text-right font-mono text-xs tabular-nums">{fmtBytes(r.rx_bytes + r.tx_bytes)}</TableCell>
              )}
              <TableCell>
                <Sparkline points={r.sparkline} role={raw ? undefined : r.role} />
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
