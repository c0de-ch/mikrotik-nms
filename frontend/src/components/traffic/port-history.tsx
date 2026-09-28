"use client";

import { useCallback, useMemo, useState } from "react";
import { Area, AreaChart, Brush, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { Activity } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { BRAND, fmtBps } from "@/components/graph/graph-style";
import { useAuth } from "@/context/auth";
import { useWebSocket } from "@/hooks/use-websocket";
import { api, type PortRoleName, type SeriesPoint, type SeriesStats, type TrafficRange } from "@/lib/api";
import { usePolledFetch } from "./hooks";
import {
  RANGE_LABEL,
  RX_COLOR,
  TX_COLOR,
  dirLabels,
  fmtBpsAxis,
  fmtBytes,
  fmtDateTime,
  fmtTick,
  fmtTooltipTime,
  isLongRange,
  refreshMs,
  splitDirection,
  splitPoint,
} from "./lib";
import { EmptyState, Notice } from "./notice";
import { DirKey } from "./rate-bar";

const LIVE_MAX_POINTS = 300;

function p95(vals: number[]): number {
  if (vals.length === 0) return 0;
  const s = [...vals].sort((a, b) => a - b);
  return s[Math.ceil(0.95 * s.length) - 1];
}

// Stats over 1 s live samples (the REST stats only cover the initial 5 min).
function liveStats(points: SeriesPoint[]): SeriesStats {
  const rx = points.flatMap((p) => (p.rx_bps === null ? [] : [p.rx_bps]));
  const tx = points.flatMap((p) => (p.tx_bps === null ? [] : [p.tx_bps]));
  const sum = (a: number[]) => a.reduce((s, v) => s + v, 0);
  const n = points.length || 1;
  return {
    rx_avg: Math.round(sum(rx) / n),
    tx_avg: Math.round(sum(tx) / n),
    rx_max: Math.max(0, ...rx),
    tx_max: Math.max(0, ...tx),
    rx_p95: p95(rx),
    tx_p95: p95(tx),
    rx_bytes: Math.round(sum(rx) / 8),
    tx_bytes: Math.round(sum(tx) / 8),
  };
}

// Round Y scale: a 1/2/5×10ⁿ step for at most ~4 intervals and a top that is
// a whole number of steps, so ticks read "10 / 20 / 30 / 40 Mbps" rather than
// fractions of the data maximum (and survive fmtBps' whole-Kbps rounding). An
// idle port still gets a 0–1 kbps axis.
function niceScale(max: number): { top: number; ticks: number[] } {
  const m = Math.max(max, 1000);
  const raw = m / 4;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 5, 10].map((f) => f * mag).find((v) => v >= raw) ?? 10 * mag;
  const n = Math.ceil(m / step - 1e-9);
  return { top: n * step, ticks: Array.from({ length: n + 1 }, (_, i) => i * step) };
}

// Single-line Y tick: Recharts' default tick wraps "36.4 Mbps" onto two lines
// when it is wider than the axis.
function YTick({ x, y, payload }: { x?: number; y?: number; payload?: { value: number } }) {
  return (
    <text x={x} y={y} dy={4} textAnchor="end" fontSize={11} fill="var(--muted-foreground)">
      {fmtBpsAxis(Number(payload?.value ?? 0))}
    </text>
  );
}

// PortHistoryCard: history chart + stats for one port. Ranges ≥ 15m read the
// 1-minute / hourly buckets; "live" seeds from the last 5 min of 1 s samples
// and then appends the port's own traffic.<id>.<iface> stream — the only
// place the traffic page still opens a per-port stream.
export function PortHistoryCard({
  deviceId,
  iface,
  range,
  role,
}: {
  deviceId: string;
  iface: string;
  range: TrafficRange;
  role: PortRoleName;
}) {
  const { token } = useAuth();
  const live = range === "live";
  const key = token ? `hist|${deviceId}|${iface}|${range}` : null;
  const fetcher = useCallback(() => api.traffic.portHistory(token!, deviceId, iface, range), [token, deviceId, iface, range]);
  const { data, error, loading } = usePolledFetch(key, fetcher, live ? null : refreshMs(range));

  // Live 1 s points, tagged with the fetch key they belong to.
  const [stream, setStream] = useState<{ key: string | null; points: SeriesPoint[] }>({ key: null, points: [] });
  // The card stays mounted across range changes and the live key is the same
  // every session, so start each Live session with an empty stream — else
  // points from an earlier session (maybe hours old) come back under "last
  // 5 minutes". Adjusting state during render, per the React docs pattern.
  const [prevLive, setPrevLive] = useState(live);
  if (live !== prevLive) {
    setPrevLive(live);
    if (live) setStream({ key: null, points: [] });
  }
  useWebSocket(
    live ? `traffic.${deviceId}.${iface}` : "",
    useCallback(
      (msg: unknown) => {
        const d = msg as { rx_bps?: number; tx_bps?: number; timestamp?: string };
        if (typeof d.rx_bps !== "number" || typeof d.tx_bps !== "number") return;
        const p: SeriesPoint = { ts: d.timestamp || new Date().toISOString(), rx_bps: d.rx_bps, tx_bps: d.tx_bps };
        setStream((prev) => ({
          key,
          points: [...(prev.key === key ? prev.points : []), p].slice(-LIVE_MAX_POINTS),
        }));
      },
      [key],
    ),
  );

  const points = useMemo<SeriesPoint[]>(() => {
    if (!live) return data?.points ?? [];
    const extra = stream.key === key ? stream.points : [];
    if (!data) return extra;
    // Append only stream points newer than the seed and inside its window
    // (data.from and the WS timestamps are both server clock).
    const base = data.points;
    const from = Date.parse(data.from);
    const lastTs = base.length ? Date.parse(base[base.length - 1].ts) : from;
    return [
      ...base,
      ...extra.filter((p) => {
        const t = Date.parse(p.ts);
        return t > lastTs && t >= from;
      }),
    ].slice(-LIVE_MAX_POINTS);
  }, [data, live, stream, key]);

  const chart = useMemo(() => points.map((p) => ({ t: Date.parse(p.ts), ...splitPoint(role, p) })), [points, role]);
  // Points with data (null = a step the collector did not cover: a gap).
  const plotted = useMemo(() => chart.filter((c) => c.down !== null).length, [chart]);
  const yScale = useMemo(() => {
    let max = 0;
    for (const c of chart) max = Math.max(max, c.down ?? 0, c.up ?? 0);
    return niceScale(max);
  }, [chart]);
  // A long range right after rollout can hold a single bucket: still draw it
  // (as a dot, placed in the requested window) rather than "No samples" next
  // to populated stats. Live waits for two samples.
  const single = !live && plotted === 1;
  const drawable = live ? plotted > 1 : plotted >= 1;
  const xDomain: [number | string, number | string] =
    single && data ? [Date.parse(data.from), Date.parse(data.to)] : ["dataMin", "dataMax"];
  const stats = live ? liveStats(points) : data?.stats;
  const labels = dirLabels(role);
  const noHistory = !live && data && data.coverage_from === null;
  const coverageLate = !live && data?.coverage_from && data.from && Date.parse(data.coverage_from) > Date.parse(data.from);

  return (
    <Card>
      <CardHeader className="gap-1">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div>
            <CardTitle className="text-base">History</CardTitle>
            <p className="text-xs text-muted-foreground">
              {live
                ? "Live 1-second samples (last 5 minutes)."
                : `Last ${RANGE_LABEL[range]}${data ? ` · ${data.step_seconds >= 3600 ? "hourly" : data.step_seconds >= 60 ? `${data.step_seconds / 60}-minute` : `${data.step_seconds}s`} points` : ""}.`}
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-3 text-[11px] text-muted-foreground">
            <DirKey color={RX_COLOR}>{labels.down}</DirKey>
            <DirKey color={TX_COLOR}>{labels.up}</DirKey>
          </div>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {error && <Notice kind="error">Could not load history: {error}</Notice>}
        {noHistory && (
          <Notice>No history for this range yet — the collector writes 1-minute buckets; check back in a few minutes.</Notice>
        )}
        {coverageLate && <Notice>Data since {fmtDateTime(data!.coverage_from!)}</Notice>}

        {drawable ? (
          <div className="h-[280px] w-full">
            <ResponsiveContainer width="100%" height="100%">
              <AreaChart data={chart} margin={{ top: 14, right: 8, bottom: 0, left: 0 }}>
                <CartesianGrid vertical={false} stroke="var(--border)" />
                <XAxis
                  dataKey="t"
                  type="number"
                  scale="time"
                  domain={xDomain}
                  tickFormatter={(v) => fmtTick(Number(v), range)}
                  tick={{ fontSize: 11, fill: "var(--muted-foreground)" }}
                  stroke="var(--border)"
                  minTickGap={40}
                />
                <YAxis
                  domain={[0, yScale.top]}
                  ticks={yScale.ticks}
                  allowDataOverflow
                  tick={<YTick />}
                  stroke="var(--border)"
                  width={76}
                />
                <Tooltip
                  labelFormatter={(v) => fmtTooltipTime(Number(v), range)}
                  formatter={(v, name) => [v === null || v === undefined ? "no data (collector down)" : fmtBps(Number(v)), name]}
                  contentStyle={{ background: "var(--popover)", border: "1px solid var(--border)", borderRadius: 8, fontSize: 12 }}
                  labelStyle={{ color: "var(--muted-foreground)" }}
                  itemStyle={{ color: "var(--popover-foreground)" }}
                />
                <Area type="monotone" dataKey="down" name={labels.down} stroke={RX_COLOR} strokeWidth={2} fill={RX_COLOR} fillOpacity={0.12} isAnimationActive={false} dot={single} connectNulls={false} />
                <Area type="monotone" dataKey="up" name={labels.up} stroke={TX_COLOR} strokeWidth={2} fill={TX_COLOR} fillOpacity={0.12} isAnimationActive={false} dot={single} connectNulls={false} />
                {isLongRange(range) && !single && (
                  <Brush dataKey="t" height={22} travellerWidth={8} stroke={BRAND.grey} fill="transparent" tickFormatter={(v) => fmtTick(Number(v), range)} />
                )}
              </AreaChart>
            </ResponsiveContainer>
          </div>
        ) : loading ? (
          <div className="h-[280px] animate-pulse rounded-lg bg-muted" />
        ) : (
          !noHistory && (
            <EmptyState icon={Activity}>
              {live
                ? "Waiting for live samples — the 1-second stream starts when this view opens."
                : "No samples for this port in the range."}
            </EmptyState>
          )
        )}

        {stats && points.length > 0 && !noHistory && <StatsGrid stats={stats} role={role} />}
      </CardContent>
    </Card>
  );
}

function StatsGrid({ stats, role }: { stats: SeriesStats; role: PortRoleName }) {
  const labels = dirLabels(role);
  const avg = splitDirection(role, stats.rx_avg, stats.tx_avg);
  const max = splitDirection(role, stats.rx_max, stats.tx_max);
  const pct = splitDirection(role, stats.rx_p95, stats.tx_p95);
  const vol = splitDirection(role, stats.rx_bytes, stats.tx_bytes);
  const items: { label: string; title: string; down: string; up: string }[] = [
    { label: "Average", title: "Bytes over the covered window", down: fmtBps(avg.down), up: fmtBps(avg.up) },
    { label: "Peak", title: "Highest poll-interval rate (can exceed any plotted point)", down: fmtBps(max.down), up: fmtBps(max.up) },
    { label: "95th percentile", title: "95% of intervals were at or below this rate", down: fmtBps(pct.down), up: fmtBps(pct.up) },
    { label: "Volume", title: "Bytes transferred", down: fmtBytes(vol.down), up: fmtBytes(vol.up) },
  ];
  return (
    <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
      {items.map((it) => (
        <div key={it.label} className="rounded-lg border px-3 py-2" title={it.title}>
          <div className="text-xs text-muted-foreground">{it.label}</div>
          <div className="mt-1 flex items-center gap-1.5 font-mono text-sm tabular-nums">
            <span className="h-2 w-2 rounded-sm" style={{ background: RX_COLOR }} aria-hidden />
            <span className="sr-only">{labels.down}</span>
            {it.down}
          </div>
          <div className="flex items-center gap-1.5 font-mono text-sm tabular-nums">
            <span className="h-2 w-2 rounded-sm" style={{ background: TX_COLOR }} aria-hidden />
            <span className="sr-only">{labels.up}</span>
            {it.up}
          </div>
        </div>
      ))}
    </div>
  );
}
