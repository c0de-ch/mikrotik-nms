"use client";

import { useCallback, useMemo, useState } from "react";
import { Area, AreaChart, Brush, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { Activity } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { BRAND, fmtBps } from "@/components/graph/graph-style";
import { ClassMeter, PauseGlyph } from "@/components/devices/device-load";
import { useAuth } from "@/context/auth";
import { useWebSocket } from "@/hooks/use-websocket";
import {
  api,
  type Device,
  type LiveStreamStatus,
  type PortRate,
  type PortRoleName,
  type PortSnapshot,
  type SeriesPoint,
  type SeriesStats,
  type TrafficRange,
} from "@/lib/api";
import {
  CLASSES,
  budgetClass,
  budgeted,
  counterIntervalSeconds,
  fmtInterval,
  isPollingPaused,
  liveRefusalText,
} from "@/lib/device-load";
import type { ControlType } from "@/lib/ws";
import { useLiveLimits, useNow, usePolledFetch } from "./hooks";
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

// Stats over the live points (the REST stats only cover the seed window).
// Each point stands for the time until the next one — 1 s for the full
// stream, 15 s on a weak device's stream, the device's counter cadence for
// snapshot data — capped at three periods so a gap does not count as
// traffic.
function liveStats(points: SeriesPoint[], periodSeconds: number): SeriesStats {
  const rx: number[] = [];
  const tx: number[] = [];
  let rxBits = 0;
  let txBits = 0;
  let secs = 0;
  for (let i = 0; i < points.length; i++) {
    const p = points[i];
    if (p.rx_bps === null || p.tx_bps === null) continue;
    const next = points[i + 1];
    const dt = next ? Math.min((Date.parse(next.ts) - Date.parse(p.ts)) / 1000, 3 * periodSeconds) : periodSeconds;
    const w = dt > 0 ? dt : periodSeconds;
    rx.push(p.rx_bps);
    tx.push(p.tx_bps);
    rxBits += p.rx_bps * w;
    txBits += p.tx_bps * w;
    secs += w;
  }
  return {
    rx_avg: secs ? Math.round(rxBits / secs) : 0,
    tx_avg: secs ? Math.round(txBits / secs) : 0,
    rx_max: Math.max(0, ...rx),
    tx_max: Math.max(0, ...tx),
    rx_p95: p95(rx),
    tx_p95: p95(tx),
    rx_bytes: Math.round(rxBits / 8),
    tx_bytes: Math.round(txBits / 8),
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

// How the live range is fed: waiting for the device's limits, the port's own
// traffic.<id>.<iface> stream, or — when the device may not stream for this
// user (weak device, pause, another session holding its live view) — the
// port-counter snapshot the page already follows.
type LiveMode = "off" | "pending" | "stream" | "snapshot";

// PortHistoryCard: history chart + stats for one port. Ranges ≥ 15m read the
// 1-minute / hourly buckets; "live" seeds from the last 5 min of stream
// samples and then appends the port's own traffic.<id>.<iface> stream — the
// only place the traffic page opens a per-port stream, and the one the
// device's class caps: a weak device streams every 15 s and to admins only,
// so everyone else sees the snapshot instead of a spinner — one point per
// reading of the device's counters (every 60 s on a weak device), and none
// while its polling is paused (the snapshot keeps its last rates then, which
// would draw a flat line that looks measured).
export function PortHistoryCard({
  deviceId,
  device,
  iface,
  range,
  role,
  snapshot,
  rate,
}: {
  deviceId: string;
  // The managed device, for its class budget and pause (undefined while the
  // device list loads; the live limits stand in then).
  device: Device | undefined;
  iface: string;
  range: TrafficRange;
  role: PortRoleName;
  // The fleet port-counter snapshot and this port's rate in it: the live
  // range's fallback when the device may not stream.
  snapshot: PortSnapshot | null;
  rate: PortRate | undefined;
}) {
  const { token } = useAuth();
  const now = useNow(60_000);
  const live = range === "live";
  const key = token ? `hist|${deviceId}|${iface}|${range}` : null;
  const fetcher = useCallback(() => api.traffic.portHistory(token!, deviceId, iface, range), [token, deviceId, iface, range]);
  const { data, error, loading } = usePolledFetch(key, fetcher, live ? null : refreshMs(range));

  // Live stream points, tagged with the fetch key they belong to.
  const [stream, setStream] = useState<{ key: string | null; points: SeriesPoint[] }>({ key: null, points: [] });
  // Snapshot points, one per counter cycle, while the device may not stream.
  const [snapSeries, setSnapSeries] = useState<{ key: string | null; lastTs: string | null; points: SeriesPoint[] }>({
    key: null,
    lastTs: null,
    points: [],
  });
  // The server's answer to the subscription (subscribe.ack / .error).
  const streamTopic = `traffic.${deviceId}.${iface}`;
  const [answer, setAnswer] = useState<{ topic: string; type: ControlType; status: LiveStreamStatus } | null>(null);
  // Counts Live sessions, so each re-asks for the device's limits.
  const [session, setSession] = useState(0);
  // The card stays mounted across range changes and the live key is the same
  // every session, so start each Live session afresh — else points from an
  // earlier session (maybe hours old) come back under "last 5 minutes", and a
  // refusal from then (another session held the live view) would stick.
  // Adjusting state during render, per the React docs pattern.
  const [prevLive, setPrevLive] = useState(live);
  if (live !== prevLive) {
    setPrevLive(live);
    if (live) {
      setStream({ key: null, points: [] });
      setSnapSeries({ key: null, lastTs: null, points: [] });
      setAnswer(null);
      setSession((n) => n + 1);
    }
  }

  // Ask before subscribing, so a viewer of a weak device never opens a
  // stream the server would refuse; a failed check subscribes anyway (the
  // server vets every subscription).
  const limits = useLiveLimits(live ? deviceId : null, session);
  const ack = answer?.topic === streamTopic && answer.type === "subscribe.ack" ? answer.status : null;
  const serverRefusal = answer?.topic === streamTopic && answer.type === "subscribe.error" ? answer.status : null;
  const caps = ack ?? limits.data;
  // The device's own counter cadence — what a snapshot point is worth: the
  // class the caps follow (limits.class: the effective class, C1 on the
  // light profile), floored by the fleet counter interval.
  const budget = limits.data ? budgeted(limits.data.class) : device ? budgetClass(device) : null;
  const floor = snapshot?.interval_seconds || limits.data?.snapshot_interval_seconds;
  const counterSeconds = budget ? counterIntervalSeconds(budget, floor) : floor || 15;
  const refusedByLimits = !!limits.data && !limits.data.allowed;
  const refusalCode = !live ? undefined : refusedByLimits ? limits.data!.reason : serverRefusal?.code;
  const refusal =
    live && (refusedByLimits || serverRefusal)
      ? liveRefusalText(refusalCode, {
          counterSeconds,
          maxIfaces: caps?.max_ifaces,
          serverMessage: (refusedByLimits ? limits.data!.message : serverRefusal?.error) || undefined,
        })
      : null;
  const mode: LiveMode = !live ? "off" : limits.loading ? "pending" : refusal ? "snapshot" : "stream";
  // Nothing is read while the device's polling is paused or the instance
  // polls nothing: the snapshot rates are a last reading, so no new points
  // (and no promise of one).
  const frozenCode =
    refusalCode === "pollers_off"
      ? "pollers_off"
      : refusalCode === "polling_paused" || (!!device && isPollingPaused(device, now))
        ? "polling_paused"
        : null;
  const frozen = frozenCode !== null;
  const period =
    mode === "snapshot" ? counterSeconds : ack?.period_seconds || Math.max(1, limits.data?.min_period_seconds ?? 1);

  useWebSocket(
    mode === "stream" ? streamTopic : "",
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
    useCallback(
      (type: ControlType, status: unknown) => setAnswer({ topic: streamTopic, type, status: status as LiveStreamStatus }),
      [streamTopic],
    ),
  );

  // Snapshot mode: a new reading of this port's counters adds a point (the
  // page follows traffic.ports already, so this costs nothing extra). The
  // fleet snapshot moves on every cycle while a weak device is read every
  // 60 s, so a cycle only counts as a reading when the rate changed or the
  // device's own interval has passed since the last point; a paused device
  // adds none.
  if (
    mode === "snapshot" &&
    !frozen &&
    snapshot?.ready &&
    snapshot.ts &&
    rate &&
    (snapSeries.key !== key || snapSeries.lastTs !== snapshot.ts)
  ) {
    const ts = snapshot.ts;
    const prevPoints = snapSeries.key === key ? snapSeries.points : [];
    const last = prevPoints[prevPoints.length - 1];
    const reading =
      !last ||
      last.rx_bps !== rate.rx_bps ||
      last.tx_bps !== rate.tx_bps ||
      Date.parse(ts) - Date.parse(last.ts) >= 0.9 * counterSeconds * 1000;
    const p: SeriesPoint = { ts, rx_bps: rate.rx_bps, tx_bps: rate.tx_bps };
    setSnapSeries({
      key,
      lastTs: ts,
      points: reading ? [...prevPoints, p].slice(-LIVE_MAX_POINTS) : prevPoints,
    });
  }

  const points = useMemo<SeriesPoint[]>(() => {
    if (!live) return data?.points ?? [];
    const extra =
      mode === "snapshot" ? (snapSeries.key === key ? snapSeries.points : []) : stream.key === key ? stream.points : [];
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
  }, [data, live, mode, stream, snapSeries, key]);

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
  const stats = live ? liveStats(points, period) : data?.stats;
  const labels = dirLabels(role);
  const noHistory = !live && data && data.coverage_from === null;
  const coverageLate = !live && data?.coverage_from && data.from && Date.parse(data.coverage_from) > Date.parse(data.from);
  const liveText =
    mode === "snapshot"
      ? frozen
        ? `Snapshot data — no new readings while polling is ${frozenCode === "pollers_off" ? "off" : "paused"}.`
        : `Snapshot data, one reading every ${fmtInterval(counterSeconds)}.`
      : period > 1
        ? `Live samples every ${fmtInterval(period)}.`
        : "Live 1-second samples (last 5 minutes).";
  const capsClass = caps ? budgeted(caps.class) : null;

  return (
    <Card>
      <CardHeader className="gap-1">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div>
            <CardTitle className="text-base">History</CardTitle>
            <p className="text-xs text-muted-foreground">
              {live
                ? liveText
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
        {mode === "snapshot" && refusal && (
          <Notice
            kind={frozen ? "hold" : "info"}
            icon={
              frozen ? (
                <PauseGlyph />
              ) : capsClass && (refusalCode === "admin_only" || refusalCode === "iface_limit" || refusalCode === "lease_limit") ? (
                <ClassMeter cls={capsClass} className="text-[13px]" />
              ) : undefined
            }
          >
            {/* A pause outranks whatever else refused the stream: no
                snapshot readings arrive either. */}
            {frozenCode ? liveRefusalText(frozenCode, { counterSeconds }) : refusal}
          </Notice>
        )}
        {mode === "stream" && period > 1 && capsClass && (
          <Notice icon={<ClassMeter cls={capsClass} className="text-[13px]" />}>
            Live samples every {fmtInterval(period)} on this {CLASSES[capsClass].word} device, at most{" "}
            {caps!.max_ifaces} ports at once, to spare its CPU.
          </Notice>
        )}

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
              {mode === "snapshot"
                ? frozenCode === "pollers_off"
                  ? "Polling is off on this instance — no new readings."
                  : frozen
                    ? "Polling is paused for this device — no new readings until it resumes."
                    : `Waiting for the next reading — one every ${fmtInterval(counterSeconds)}.`
                : mode === "pending"
                  ? "Checking whether this device can stream…"
                  : live && period > 1
                    ? `Waiting for live samples — one every ${period} s.`
                    : live
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
