"use client";

import { Area, AreaChart } from "recharts";
import type { PortRoleName, SeriesPoint } from "@/lib/api";
import { RX_COLOR, TX_COLOR, splitPoint } from "./lib";

// Sparkline: tiny axis-less download/upload area chart for ranked rows.
export function Sparkline({
  points,
  role,
  width = 96,
  height = 28,
}: {
  points: SeriesPoint[];
  role?: PortRoleName;
  width?: number;
  height?: number;
}) {
  if (points.length < 2) {
    return <div style={{ width, height }} className="flex items-center text-[10px] text-muted-foreground">—</div>;
  }
  // Null points (collector not running) stay gaps rather than dips to 0.
  const data = points.map((p) => splitPoint(role, p));
  return (
    <AreaChart width={width} height={height} data={data} margin={{ top: 2, right: 0, bottom: 0, left: 0 }} aria-hidden>
      <Area type="monotone" dataKey="down" stroke={RX_COLOR} strokeWidth={1.25} fill={RX_COLOR} fillOpacity={0.15} isAnimationActive={false} dot={false} />
      <Area type="monotone" dataKey="up" stroke={TX_COLOR} strokeWidth={1.25} fill={TX_COLOR} fillOpacity={0.15} isAnimationActive={false} dot={false} />
    </AreaChart>
  );
}
