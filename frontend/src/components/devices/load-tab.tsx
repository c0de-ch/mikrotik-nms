"use client";

import { useCallback, useMemo, useState } from "react";
import {
  Area,
  CartesianGrid,
  ComposedChart,
  Line,
  ReferenceArea,
  ReferenceLine,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { Cpu } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { PauseGlyph } from "@/components/devices/device-load";
import { SeverityPill, passEndedAt, passReason, passSummary } from "@/components/devices/hygiene";
import { usePolledFetch } from "@/components/traffic/hooks";
import { fmtDateTime, fmtTick, fmtTooltipTime } from "@/components/traffic/lib";
import { EmptyState, Notice } from "@/components/traffic/notice";
import { Segmented } from "@/components/traffic/segmented";
import { useNow } from "@/hooks/use-now";
import {
  api,
  type Device,
  type DeviceHygiene,
  type DeviceLoadPoint,
  type DeviceLoadRange,
  type DeviceLoadResponse,
} from "@/lib/api";
import { isPollingPaused } from "@/lib/device-load";
import { cn } from "@/lib/utils";

// "CPU & NMS load": is the NMS why this device is busy? Three things on one
// time axis — the device's CPU sampled with the NMS keeping quiet, the CPU
// right after one of the NMS's counter reads, and what the NMS sent — so the
// answer is a look, not a calculation. The panels share the x-axis and the
// hover instead of sharing a plot: percent, commands, milliseconds and bytes
// have no common scale, and a second y-axis would invent one.

// The device's own load wears the app's chart blue; everything the NMS did
// wears the load orange. Reboots and gaps are neutral.
const CPU_COLOR = "var(--chart-1)";
const MEM_COLOR = "var(--chart-5)";
const NMS_COLOR = "var(--load-nms)";

const RANGES: DeviceLoadRange[] = ["1h", "6h", "24h", "7d", "30d"];
const RANGE_TITLE: Record<DeviceLoadRange, string> = {
  "1h": "Last hour (1-minute samples)",
  "6h": "Last 6 hours (1-minute samples)",
  "24h": "Last 24 hours (1-minute samples)",
  "7d": "Last 7 days (hourly)",
  "30d": "Last 30 days (hourly)",
};

// Why a bucket has no coverage, in words.
const GAP_TEXT: Record<string, string> = {
  paused: "NMS polling paused",
  unreachable: "Device unreachable",
  read_failed: "Counter read failed",
  no_session: "No API session to the device",
  no_baseline: "First read after a gap (nothing to compare with yet)",
  not_polled: "Not polled",
  not_covered: "Not covered",
  no_data: "No telemetry (the NMS was not recording)",
};

function gapText(gap: string): string {
  return GAP_TEXT[gap] ?? gap.replaceAll("_", " ");
}

interface Row {
  t: number;
  p: DeviceLoadPoint;
  cpu: number | null;
  cpuMax: number | null;
  post: number | null;
  memMiB: number | null;
  // Per minute, whatever the bucket length.
  cmds: number | null;
  wall: number | null;
  // Always 0: an invisible series over every bucket, so the hover (and with
  // it the tooltip) also works where nothing was measured.
  anchor: number;
}

const MIB = 1024 * 1024;

function fmtPct(v: number | null | undefined): string {
  if (v === null || v === undefined) return "—";
  return `${Number.isInteger(v) ? v : v.toFixed(1)} %`;
}

function fmtMs(ms: number): string {
  if (ms >= 10_000) return `${(ms / 1000).toFixed(0)} s`;
  if (ms >= 1000) return `${(ms / 1000).toFixed(1)} s`;
  return `${Math.round(ms)} ms`;
}

function fmtCount(v: number): string {
  return v >= 100 || Number.isInteger(v) ? String(Math.round(v)) : v.toFixed(1);
}

// Round axis top: 1/2/5 × 10ⁿ steps, at least `floor`.
function niceTop(max: number, floor: number): { top: number; ticks: number[] } {
  const m = Math.max(max, floor);
  const raw = m / 2;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 5, 10].map((f) => f * mag).find((v) => v >= raw) ?? 10 * mag;
  const n = Math.max(1, Math.ceil(m / step - 1e-9));
  return { top: n * step, ticks: Array.from({ length: n + 1 }, (_, i) => i * step) };
}

// Axis ticks on round local times: every 10 min, hour, 3 h, day or 5 days.
const TICK_STEP_MS: Record<DeviceLoadRange, number> = {
  "1h": 10 * 60_000,
  "6h": 3_600_000,
  "24h": 3 * 3_600_000,
  "7d": 86_400_000,
  "30d": 5 * 86_400_000,
};

function timeTicks(from: number, to: number, range: DeviceLoadRange): number[] {
  const step = TICK_STEP_MS[range];
  const tz = new Date(from).getTimezoneOffset() * 60_000;
  const ticks: number[] = [];
  for (let t = Math.ceil((from - tz) / step) * step + tz; t <= to; t += step) ticks.push(t);
  return ticks;
}

function fmtAxisTime(ms: number, range: DeviceLoadRange): string {
  if (range === "7d" || range === "30d") {
    const d = new Date(ms);
    return `${String(d.getDate()).padStart(2, "0")}.${String(d.getMonth() + 1).padStart(2, "0")}.`;
  }
  return fmtTick(ms, range);
}

// The 90th percentile of the values: the basis of a scale top that the
// outliers (commands that ran into their 30 s timeout while a device was
// unreachable) do not flatten everything else against.
function p90(values: number[]): number {
  if (values.length === 0) return 0;
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.min(sorted.length - 1, Math.ceil(0.9 * sorted.length) - 1)];
}

const AXIS_TICK = { fontSize: 11, fill: "var(--muted-foreground)" };
const Y_WIDTH = 52;

// Single-line Y tick: Recharts' default tick wraps "500 ms" onto two lines
// when it is wider than the axis.
function YTick({ x, y, payload, format }: { x?: number; y?: number; payload?: { value: number }; format: (v: number) => string }) {
  return (
    <text x={x} y={y} dy={4} textAnchor="end" fontSize={11} fill="var(--muted-foreground)">
      {format(Number(payload?.value ?? 0))}
    </text>
  );
}
const fmtPctTick = (v: number) => `${v} %`;
const fmtPlainTick = (v: number) => String(v);
const MARGIN = { top: 8, right: 10, bottom: 0, left: 0 };

// One stat of the summary row.
function Stat({
  label,
  value,
  sub,
  swatch,
  title,
}: {
  label: string;
  value: string;
  sub?: React.ReactNode;
  swatch?: string;
  title?: string;
}) {
  return (
    <div className="min-w-0 rounded-lg border px-3 py-2" title={title}>
      <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
        {swatch && <span aria-hidden className="h-2 w-2 shrink-0 rounded-full" style={{ background: swatch }} />}
        <span className="truncate">{label}</span>
      </div>
      <div className="mt-0.5 text-lg font-semibold tabular-nums">{value}</div>
      {sub && <div className="text-xs text-muted-foreground">{sub}</div>}
    </div>
  );
}

// Legend entry: a mark sample and its name (text in the text colour — the
// mark carries the identity).
function Key({ children, sample }: { children: React.ReactNode; sample: React.ReactNode }) {
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
      <span aria-hidden className="inline-flex h-3 w-4 items-center justify-center">
        {sample}
      </span>
      {children}
    </span>
  );
}

function PanelTitle({ children, hint }: { children: React.ReactNode; hint?: string }) {
  return (
    <div className="mb-1 flex flex-wrap items-baseline gap-x-2 text-xs">
      <span className="font-medium">{children}</span>
      {hint && <span className="text-muted-foreground">{hint}</span>}
    </div>
  );
}

// The post-poll marker: a dot with a surface ring, so it stays readable on
// top of the CPU line.
function PostDot(props: { cx?: number; cy?: number; value?: number | null }) {
  const { cx, cy, value } = props;
  if (cx === undefined || cy === undefined || value === null || value === undefined) return null;
  return <circle cx={cx} cy={cy} r={4.5} fill={NMS_COLOR} stroke="var(--card)" strokeWidth={2} />;
}

function TooltipLine({ swatch, label, value }: { swatch?: string; label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-4">
      <span className="flex items-center gap-1.5 text-muted-foreground">
        <span aria-hidden className="h-2 w-2 shrink-0 rounded-full" style={{ background: swatch ?? "transparent" }} />
        {label}
      </span>
      <span className="font-medium tabular-nums">{value}</span>
    </div>
  );
}

// Everything known about the hovered bucket, in one tooltip (shown at the CPU
// panel; the other panels only move their cursor).
function LoadTooltip({ row, range, hourly, memoryTotal }: { row: Row; range: DeviceLoadRange; hourly: boolean; memoryTotal: number | null }) {
  const p = row.p;
  const nms: string[] = [];
  if (p.nms_cmds !== null) nms.push(`${p.nms_cmds} command${p.nms_cmds === 1 ? "" : "s"}`);
  if (p.nms_wall_ms !== null) nms.push(`${fmtMs(p.nms_wall_ms)} wall time`);
  if (p.nms_rows !== null) nms.push(`${p.nms_rows} rows`);
  const trouble: string[] = [];
  if (p.nms_timeouts) trouble.push(`${p.nms_timeouts} timeout${p.nms_timeouts === 1 ? "" : "s"}`);
  if (p.nms_reconnects) trouble.push(`${p.nms_reconnects} reconnect${p.nms_reconnects === 1 ? "" : "s"}`);
  const memPct = p.free_memory !== null && memoryTotal ? Math.round((p.free_memory / memoryTotal) * 100) : null;
  return (
    <div className="min-w-56 space-y-1 rounded-lg border bg-popover px-3 py-2 text-xs text-popover-foreground shadow-md">
      <div className="text-muted-foreground">
        {fmtTooltipTime(row.t, range)}
        {hourly ? " (1 hour)" : ""}
      </div>
      {p.gap && <div className="font-medium">{gapText(p.gap)}</div>}
      {p.rebooted && <div className="font-medium">Device rebooted</div>}
      {p.cpu_avg !== null && (
        <TooltipLine
          swatch={CPU_COLOR}
          label="CPU, quiet window"
          value={`${fmtPct(p.cpu_avg)}${p.cpu_max !== null && p.cpu_max > p.cpu_avg ? ` (max ${fmtPct(p.cpu_max)})` : ""}`}
        />
      )}
      {p.cpu_p95 !== null && <TooltipLine label="CPU, 95th percentile" value={fmtPct(p.cpu_p95)} />}
      {p.cpu_post !== null && <TooltipLine swatch={NMS_COLOR} label="CPU after a counter read" value={fmtPct(p.cpu_post)} />}
      {p.free_memory !== null && (
        <TooltipLine
          swatch={MEM_COLOR}
          label="Free memory"
          value={`${Math.round(p.free_memory / MIB)} MiB${memPct !== null ? ` (${memPct} %)` : ""}`}
        />
      )}
      {nms.length > 0 && (
        <div className="border-t pt-1">
          <div className="flex items-center gap-1.5 text-muted-foreground">
            <span aria-hidden className="h-2 w-2 shrink-0 rounded-full" style={{ background: NMS_COLOR }} />
            NMS sent
          </div>
          <div className="pl-3.5 font-medium tabular-nums">{nms.join(" · ")}</div>
          {trouble.length > 0 && <div className="pl-3.5 font-medium tabular-nums">{trouble.join(" · ")}</div>}
        </div>
      )}
      {p.gap === null && p.covered_s !== null && !hourly && p.covered_s < 60 && (
        <div className="text-muted-foreground">Counters covered {p.covered_s} s of this minute</div>
      )}
    </div>
  );
}

// What a counter read does to the CPU, in one sentence.
function postVerdict(post: number | null, quiet: number | null, unit: string): React.ReactNode {
  if (post === null || quiet === null) return "No sample after a counter read in this range yet (one every 10 minutes).";
  const delta = Math.round(post - quiet);
  const base = `quiet window in the same ${unit}s: ${fmtPct(quiet)}`;
  if (delta >= 5) return `+${delta} points over the ${base}`;
  if (delta <= -5) return `${delta} points against the ${base}`;
  return `within 5 points of the ${base}`;
}

// What on the device itself costs CPU: the other half of "is it us?". The
// chart above shows what the NMS sent; this lists what the hygiene audit and
// the diagnose pass found configured on the device — graphing, a mirror to
// the CPU, flow export. The explanations and fixes are in the Findings
// section; here the findings stand next to the CPU line they explain.
function DeviceSideCauses({ hygiene, now }: { hygiene: DeviceHygiene | null; now: number }) {
  if (!hygiene) return null;
  const diagnose = hygiene.diagnose.last;
  const diagnosed = diagnose && passEndedAt(diagnose) !== null && diagnose.status !== "aborted" ? diagnose : null;
  const audit = hygiene.audit.last;
  const audited = !!audit && audit.status !== "running" && audit.status !== "aborted";
  return (
    <section aria-label="Device-side causes" className="border-t pt-4">
      <PanelTitle hint="what the hygiene audit and the diagnose pass found on the device">Device-side causes</PanelTitle>
      <div className="mt-2 space-y-3">
        {hygiene.running === "diagnose" && (
          <Notice kind="loading">A diagnose pass is reading the device, one command at a time. Its result appears here.</Notice>
        )}
        {diagnosed && (
          <p className="text-xs text-muted-foreground">
            <span className="font-medium text-foreground">Diagnose pass, {passSummary(diagnosed, now)}</span>
            {` (${passReason(diagnosed)}): `}
            {diagnosed.found.length === 0
              ? "none of the usual device-side causes — no graphing, traffic-flow, running sniffer, mirror or switch rule to the CPU, unsuitable bond mode, SNMP or other API client."
              : `found ${diagnosed.found.map((f) => f.title || f.kind.replaceAll("_", " ")).join("; ")}.`}
          </p>
        )}
        {hygiene.findings.length > 0 ? (
          <>
            <ul className="divide-y rounded-md border">
              {hygiene.findings.map((f) => (
                <li key={f.id} className="grid gap-x-3 gap-y-0.5 px-3 py-2 sm:grid-cols-[4.75rem_1fr] sm:items-baseline">
                  <SeverityPill severity={f.severity} />
                  <span className="min-w-0">
                    <span className="block text-sm">{f.title || f.kind.replaceAll("_", " ")}</span>
                    <span className="block break-words text-xs text-muted-foreground">{f.detail}</span>
                  </span>
                </li>
              ))}
            </ul>
            <p className="text-xs text-muted-foreground">
              Why each matters and how to fix it:{" "}
              <a href="#findings" className="underline underline-offset-2 hover:text-foreground">
                Findings
              </a>
              .
            </p>
          </>
        ) : (
          <p className="text-xs text-muted-foreground">
            {audited
              ? "The hygiene audit has found nothing configured on this device that costs CPU or is unsafe."
              : hygiene.pollers === "off"
                ? "Not audited: polling is off on this instance."
                : "Not audited yet: the first audit runs once the device is reachable and its CPU is calm."}
          </p>
        )}
      </div>
    </section>
  );
}

export function DeviceLoadTab({ device, token, hygiene }: { device: Device; token: string; hygiene: DeviceHygiene | null }) {
  const [range, setRange] = useState<DeviceLoadRange>("6h");
  const now = useNow(60_000);
  const paused = isPollingPaused(device, now);
  const hourly = range === "7d" || range === "30d";

  const fetcher = useCallback(() => api.devices.load(token, device.id, range), [token, device.id, range]);
  const { data, error, loading } = usePolledFetch<DeviceLoadResponse>(
    `load|${device.id}|${range}`,
    fetcher,
    hourly ? 300_000 : 60_000,
  );

  const view = useMemo(() => {
    if (!data) return null;
    const perMinute = Math.max(1, data.step_seconds / 60);
    const stepMs = data.step_seconds * 1000;
    const rows: Row[] = data.points.map((p) => ({
      t: Date.parse(p.ts),
      p,
      cpu: p.cpu_avg,
      cpuMax: p.cpu_max,
      post: p.cpu_post,
      memMiB: p.free_memory !== null ? p.free_memory / MIB : null,
      cmds: p.nms_cmds !== null ? p.nms_cmds / perMinute : null,
      wall: p.nms_wall_ms !== null ? p.nms_wall_ms / perMinute : null,
      anchor: 0,
    }));
    // Runs of buckets without coverage, and the reboot markers.
    const gaps: { x1: number; x2: number }[] = [];
    const reboots: number[] = [];
    let cmdsMax = 0;
    let wallMax = 0;
    let memMax = 0;
    for (const r of rows) {
      if (r.p.gap !== null) {
        const last = gaps[gaps.length - 1];
        if (last && last.x2 === r.t) last.x2 = r.t + stepMs;
        else gaps.push({ x1: r.t, x2: r.t + stepMs });
      }
      if (r.p.rebooted) reboots.push(r.t);
      cmdsMax = Math.max(cmdsMax, r.cmds ?? 0);
      wallMax = Math.max(wallMax, r.wall ?? 0);
      memMax = Math.max(memMax, r.memMiB ?? 0);
    }
    // Memory against the device's total when it is known: how close to
    // empty is what matters.
    const totalMiB = data.memory_total ? Math.round(data.memory_total / MIB) : 0;
    const mem = totalMiB > 0 ? { top: totalMiB, ticks: [0, Math.round(totalMiB / 2), totalMiB] } : niceTop(memMax, 16);
    // Wall time is scaled to the bulk of the minutes; a timeout spike runs
    // off the top instead of flattening them.
    const wall = niceTop(p90(rows.flatMap((r) => (r.wall !== null ? [r.wall] : []))) * 2, 200);
    const domain: [number, number] = [Date.parse(data.from), Date.parse(data.to)];
    return {
      rows,
      byTime: new Map(rows.map((r) => [r.t, r])),
      gaps,
      reboots,
      domain,
      ticks: timeTicks(domain[0], domain[1], data.range),
      cmds: niceTop(cmdsMax, 4),
      wall,
      wallClipped: wallMax > wall.top,
      mem,
      hasCPU: rows.some((r) => r.cpu !== null),
      hasNMS: rows.some((r) => r.cmds !== null),
      hasMem: rows.some((r) => r.memMiB !== null),
    };
  }, [data]);

  // The hovered bucket is found by its time, not through a series' payload:
  // in a gap no series has a value, and the gap is exactly what the tooltip
  // must explain.
  const byTime = view?.byTime;
  const renderTooltip = useCallback(
    (props: { active?: boolean; label?: unknown; payload?: ReadonlyArray<{ payload?: unknown }> }) => {
      const row = props.active ? (byTime?.get(Number(props.label)) ?? (props.payload?.[0]?.payload as Row | undefined)) : undefined;
      if (!row) return null;
      return <LoadTooltip row={row} range={range} hourly={hourly} memoryTotal={data?.memory_total ?? null} />;
    },
    [byTime, range, hourly, data?.memory_total],
  );

  const s = data?.summary;
  const noTelemetry = !!data && data.coverage_from === null;
  const lateStart = !!data?.coverage_from && Date.parse(data.coverage_from) > Date.parse(data.from);
  const unit = hourly ? "hour" : "minute";

  // Shared by every panel: the same x-domain, grid, gap shading and reboot
  // markers, so the eye can run straight down from a CPU bump to what the NMS
  // was doing.
  const xAxis = (showTicks: boolean) => (
    <XAxis
      dataKey="t"
      type="number"
      scale="time"
      domain={view?.domain ?? ["dataMin", "dataMax"]}
      ticks={view?.ticks}
      tickFormatter={(v) => fmtAxisTime(Number(v), range)}
      tick={showTicks ? AXIS_TICK : false}
      tickLine={showTicks}
      height={showTicks ? 24 : 6}
      stroke="var(--border)"
      minTickGap={48}
    />
  );
  const backdrop = (labelReboots: boolean) => (
    <>
      {view?.gaps.map((g) => (
        <ReferenceArea key={g.x1} x1={g.x1} x2={g.x2} fill="var(--muted)" fillOpacity={0.9} stroke="none" ifOverflow="hidden" />
      ))}
      {view?.reboots.map((t) => (
        <ReferenceLine
          key={t}
          x={t}
          stroke="var(--foreground)"
          strokeOpacity={0.55}
          strokeDasharray="3 3"
          label={labelReboots ? { value: "reboot", position: "insideTopRight", fontSize: 10, fill: "var(--muted-foreground)" } : undefined}
        />
      ))}
    </>
  );
  const cursorOnly = <Tooltip content={() => null} cursor={{ stroke: "var(--muted-foreground)", strokeOpacity: 0.5 }} />;

  return (
    <Card>
      <CardHeader className="gap-1">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="min-w-0">
            <CardTitle className="text-base">{"CPU & NMS load"}</CardTitle>
            <p className="text-xs text-muted-foreground">
              Is the NMS why this device is busy? The CPU is sampled once a minute after the NMS has left the device alone
              for 3 s; every 10 minutes it is sampled again 3 s after a counter read. Below: what the NMS sent.
            </p>
          </div>
          <Segmented
            ariaLabel="Time range"
            value={range}
            onChange={setRange}
            options={RANGES.map((r) => ({ value: r, label: r, title: RANGE_TITLE[r] }))}
          />
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        {error && <Notice kind="error">Could not load the device load: {error}</Notice>}
        {paused && (
          <Notice kind="hold" icon={<PauseGlyph />}>
            NMS polling is paused for this device: nothing is sampled until it resumes, and the paused time shows as a gap.
          </Notice>
        )}
        {noTelemetry && (
          <Notice>
            No telemetry in this range yet. The NMS records one row per device and minute while it polls; the first points
            appear a few minutes after it starts.
          </Notice>
        )}
        {lateStart && !noTelemetry && <Notice>Telemetry since {fmtDateTime(data!.coverage_from!)}</Notice>}

        {s && !noTelemetry && (
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 xl:grid-cols-4">
            <Stat
              label="CPU, quiet window"
              swatch={CPU_COLOR}
              value={fmtPct(s.cpu_avg)}
              sub={s.cpu_max !== null ? `average · highest sample ${fmtPct(s.cpu_max)}` : "no sample in this range"}
              title="Average of the samples taken with the device left alone by the NMS for at least 3 s"
            />
            <Stat
              label="CPU 3 s after a counter read"
              swatch={NMS_COLOR}
              value={fmtPct(s.cpu_post_avg)}
              sub={postVerdict(s.cpu_post_avg, s.cpu_quiet_at_post, unit)}
              title="The NMS's own effect: the CPU right after it read the interface counters, against the quiet-window CPU of the same minutes"
            />
            <Stat
              label="NMS sent"
              swatch={NMS_COLOR}
              value={s.nms_cmds_per_min !== null ? `${fmtCount(s.nms_cmds_per_min)} commands/min` : "—"}
              sub={
                s.nms_wall_ms_per_min !== null
                  ? `${fmtMs(s.nms_wall_ms_per_min)} wall time per minute · ${s.nms_timeouts} timeout${s.nms_timeouts === 1 ? "" : "s"} · ${s.nms_reconnects} reconnect${s.nms_reconnects === 1 ? "" : "s"}`
                  : undefined
              }
              title="Every API command this NMS sent the device, and how long it waited for the answers"
            />
            <Stat
              label="Counter coverage"
              value={fmtPct(s.covered_pct)}
              sub={`of the range · ${s.reboots} reboot${s.reboots === 1 ? "" : "s"} seen`}
              title="Share of the range in which the device's interface counters were read; the rest shows as gaps"
            />
          </div>
        )}

        {view && !noTelemetry ? (
          <div className="space-y-3">
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
              <Key sample={<span className="h-0.5 w-4 rounded" style={{ background: CPU_COLOR }} />}>CPU, quiet window (average)</Key>
              <Key sample={<span className="h-px w-4 rounded opacity-60" style={{ background: CPU_COLOR }} />}>
                highest sample of the {unit}
              </Key>
              <Key sample={<span className="h-2.5 w-2.5 rounded-full" style={{ background: NMS_COLOR }} />}>
                CPU 3 s after a counter read
              </Key>
              <Key sample={<span className="h-2.5 w-4 rounded-sm opacity-40" style={{ background: NMS_COLOR }} />}>sent by the NMS</Key>
              <Key sample={<span className="h-3 w-4 rounded-sm border bg-muted" />}>no coverage (paused, unreachable, not recorded)</Key>
              <Key sample={<span className="h-3 w-px border-l border-dashed border-foreground/60" />}>reboot</Key>
            </div>

            <div>
              <PanelTitle hint="percent">CPU load</PanelTitle>
              <div
                className="h-[220px] w-full"
                role="img"
                aria-label={`CPU load over the last ${range}: quiet-window average ${fmtPct(s?.cpu_avg)}, after a counter read ${fmtPct(s?.cpu_post_avg)}`}
              >
                <ResponsiveContainer width="100%" height="100%">
                  <ComposedChart data={view.rows} syncId="device-load" syncMethod="value" margin={MARGIN}>
                    <CartesianGrid vertical={false} stroke="var(--border)" />
                    {xAxis(false)}
                    <YAxis domain={[0, 100]} ticks={[0, 25, 50, 75, 100]} tick={<YTick format={fmtPctTick} />} stroke="var(--border)" width={Y_WIDTH} />
                    {backdrop(true)}
                    <Tooltip content={renderTooltip} cursor={{ stroke: "var(--muted-foreground)", strokeOpacity: 0.5 }} />
                    <Line dataKey="anchor" stroke="none" strokeWidth={0} dot={false} activeDot={false} legendType="none" isAnimationActive={false} />
                    <Line
                      dataKey="cpuMax"
                      name="highest sample"
                      type="monotone"
                      stroke={CPU_COLOR}
                      strokeOpacity={0.45}
                      strokeWidth={1}
                      dot={false}
                      activeDot={false}
                      connectNulls={false}
                      isAnimationActive={false}
                    />
                    <Line
                      dataKey="cpu"
                      name="quiet window"
                      type="monotone"
                      stroke={CPU_COLOR}
                      strokeWidth={2}
                      dot={false}
                      activeDot={{ r: 4, stroke: "var(--card)", strokeWidth: 2 }}
                      connectNulls={false}
                      isAnimationActive={false}
                    />
                    <Line
                      dataKey="post"
                      name="after a counter read"
                      stroke="none"
                      strokeWidth={0}
                      dot={(d: unknown) => {
                        const { cx, cy, value, index } = d as { cx?: number; cy?: number; value?: number | null; index?: number };
                        return <PostDot key={index} cx={cx} cy={cy} value={value} />;
                      }}
                      activeDot={false}
                      connectNulls={false}
                      isAnimationActive={false}
                    />
                  </ComposedChart>
                </ResponsiveContainer>
              </div>
              {!view.hasCPU && (
                <p className="mt-1 text-xs text-muted-foreground">No CPU sample in this range: the device was not reachable, or its polling was paused.</p>
              )}
            </div>

            <div>
              <PanelTitle hint="per minute">NMS commands</PanelTitle>
              <div className="h-[92px] w-full" role="img" aria-label={`API commands the NMS sent per minute, ${s?.nms_cmds_per_min ?? 0} on average`}>
                <ResponsiveContainer width="100%" height="100%">
                  <ComposedChart data={view.rows} syncId="device-load" syncMethod="value" margin={MARGIN}>
                    <CartesianGrid vertical={false} stroke="var(--border)" />
                    {xAxis(false)}
                    <YAxis domain={[0, view.cmds.top]} ticks={view.cmds.ticks} tick={<YTick format={fmtPlainTick} />} stroke="var(--border)" width={Y_WIDTH} />
                    {backdrop(false)}
                    {cursorOnly}
                    <Area
                      dataKey="cmds"
                      name="commands"
                      type="stepAfter"
                      stroke={NMS_COLOR}
                      strokeWidth={1.5}
                      fill={NMS_COLOR}
                      fillOpacity={0.25}
                      dot={false}
                      activeDot={false}
                      connectNulls={false}
                      isAnimationActive={false}
                    />
                  </ComposedChart>
                </ResponsiveContainer>
              </div>
            </div>

            <div>
              <PanelTitle
                hint={`per minute, waiting for the device's answers${view.wallClipped ? ` — spikes above ${fmtMs(view.wall.top)} run off the scale` : ""}`}
              >
                NMS wall time
              </PanelTitle>
              <div className="h-[92px] w-full" role="img" aria-label="Time the NMS spent waiting for the device's answers, per minute">
                <ResponsiveContainer width="100%" height="100%">
                  <ComposedChart data={view.rows} syncId="device-load" syncMethod="value" margin={MARGIN}>
                    <CartesianGrid vertical={false} stroke="var(--border)" />
                    {xAxis(false)}
                    <YAxis
                      domain={[0, view.wall.top]}
                      ticks={view.wall.ticks}
                      tick={<YTick format={fmtMs} />}
                      stroke="var(--border)"
                      width={Y_WIDTH}
                      allowDataOverflow
                    />
                    {backdrop(false)}
                    {cursorOnly}
                    <Area
                      dataKey="wall"
                      name="wall time"
                      type="stepAfter"
                      stroke={NMS_COLOR}
                      strokeWidth={1.5}
                      fill={NMS_COLOR}
                      fillOpacity={0.25}
                      dot={false}
                      activeDot={false}
                      connectNulls={false}
                      isAnimationActive={false}
                    />
                  </ComposedChart>
                </ResponsiveContainer>
              </div>
              {hourly && !view.rows.some((r) => r.wall !== null) && view.hasNMS && (
                <p className="mt-1 text-xs text-muted-foreground">Wall time is kept for 14 days; older hours show commands only.</p>
              )}
            </div>

            <div>
              <PanelTitle hint={data?.memory_total ? `MiB of ${Math.round(data.memory_total / MIB)}` : "MiB"}>Free memory</PanelTitle>
              <div className="h-[116px] w-full" role="img" aria-label="Free memory of the device over time">
                <ResponsiveContainer width="100%" height="100%">
                  <ComposedChart data={view.rows} syncId="device-load" syncMethod="value" margin={MARGIN}>
                    <CartesianGrid vertical={false} stroke="var(--border)" />
                    {xAxis(true)}
                    <YAxis domain={[0, view.mem.top]} ticks={view.mem.ticks} tick={<YTick format={fmtPlainTick} />} stroke="var(--border)" width={Y_WIDTH} />
                    {backdrop(false)}
                    {cursorOnly}
                    <Line
                      dataKey="memMiB"
                      name="free memory"
                      type="monotone"
                      stroke={MEM_COLOR}
                      strokeWidth={2}
                      dot={false}
                      activeDot={false}
                      connectNulls={false}
                      isAnimationActive={false}
                    />
                  </ComposedChart>
                </ResponsiveContainer>
              </div>
            </div>
          </div>
        ) : loading && !data ? (
          <div className={cn("h-[420px] animate-pulse rounded-lg bg-muted")} />
        ) : (
          noTelemetry && <EmptyState icon={Cpu}>Nothing recorded for this device in the last {range}.</EmptyState>
        )}

        <DeviceSideCauses hygiene={hygiene} now={now} />
      </CardContent>
    </Card>
  );
}
