"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ResponsiveContainer, Sankey, Tooltip, type TooltipContentProps } from "recharts";
import { ArrowRight, GitFork, RotateCcw, Server } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { BRAND, SYNTH, fmtBps } from "@/components/graph/graph-style";
import { useAuth } from "@/context/auth";
import { api, type Device, type TrafficSankey, type TrafficSankeyNode } from "@/lib/api";
import type { TrafficParams, UpdateParams } from "./hooks";
import { usePolledFetch } from "./hooks";
import { RANGE_LABEL, deviceName } from "./lib";
import { EmptyState, Notice } from "./notice";
import { Segmented } from "./segmented";

const LIVE_REFRESH_MS = 15_000;
const RANGE_REFRESH_MS = 300_000;
// Recharts spaces columns (contentWidth - NODE_WIDTH) / maxDepth apart; the
// chart's min width keeps that ≥ COL_WIDTH so a label truncated to
// LABEL_CHARS ends before the next column's nodes.
const COL_WIDTH = 220;
const NODE_WIDTH = 10;
const NODE_PADDING = 12;
const CHAR_PX = 6.4; // average glyph width of the 11px label font
const LABEL_CHARS = Math.floor((COL_WIDTH - NODE_WIDTH - 22) / CHAR_PX);
const ROW_HEIGHT = 30;
const MARGIN = { top: 10, bottom: 10, left: 4 };

const NODE_COLOR: Record<TrafficSankeyNode["type"], string> = {
  internet: SYNTH.internet,
  gateway: SYNTH.gateway,
  vpn: SYNTH.vpn,
  device: BRAND.primary,
  port: BRAND.grey,
  other: BRAND.grey,
};

function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, Math.max(1, n - 1))}…` : s;
}

// "name · value" on one line, the name shortened so the whole label fits in
// LABEL_CHARS (the space before the next column).
function oneLineLabel(name: string, value: string): string {
  return `${truncate(name, Math.max(8, LABEL_CHARS - value.length - 3))} · ${value}`;
}

function isLocal(n: Pick<TrafficSankeyNode, "id">): boolean {
  return n.id.startsWith("local:");
}

interface Shape {
  depth: number; // deepest column index
  leaves: number;
  maxCol: number; // most nodes in any one column
  lastColChars: number; // longest label in the last column
}

// Column layout the way Recharts computes it — depth = longest path from any
// source (the "local:" east-west sources make this a DAG, not a forest) — so
// the chart can be sized to give every column and row enough room.
function shape(data: TrafficSankey): Shape {
  const n = data.nodes.length;
  const out: number[][] = Array.from({ length: n }, () => []);
  const indeg = new Array<number>(n).fill(0);
  const inSum = new Array<number>(n).fill(0);
  const outSum = new Array<number>(n).fill(0);
  for (const l of data.links) {
    if (l.source < 0 || l.source >= n || l.target < 0 || l.target >= n) continue;
    out[l.source].push(l.target);
    indeg[l.target]++;
    inSum[l.target] += l.value;
    outSum[l.source] += l.value;
  }
  // Kahn's order; a node in a cycle (the server guarantees none) keeps depth 0.
  const depth = new Array<number>(n).fill(0);
  const queue: number[] = [];
  indeg.forEach((d, i) => {
    if (d === 0) queue.push(i);
  });
  for (let head = 0; head < queue.length; head++) {
    const u = queue[head];
    for (const v of out[u]) {
      depth[v] = Math.max(depth[v], depth[u] + 1);
      if (--indeg[v] === 0) queue.push(v);
    }
  }
  const maxDepth = n ? Math.max(...depth) : 0;
  const perDepth = new Map<number, number>();
  let leaves = 0;
  let lastColChars = 0;
  for (let i = 0; i < n; i++) {
    perDepth.set(depth[i], (perDepth.get(depth[i]) ?? 0) + 1);
    if (out[i].length === 0) leaves++;
    if (depth[i] === maxDepth) {
      const label = oneLineLabel(data.nodes[i].name, fmtBps(Math.max(inSum[i], outSum[i])));
      lastColChars = Math.max(lastColChars, label.length);
    }
  }
  return { depth: maxDepth, leaves, maxCol: Math.max(0, ...perDepth.values()), lastColChars };
}

interface NodeShapeProps {
  x: number;
  y: number;
  width: number;
  height: number;
  payload: TrafficSankeyNode & { value: number; targetNodes: number[] };
}

function SankeyNodeShape({ x, y, width, height, payload }: NodeShapeProps) {
  const color = NODE_COLOR[payload.type] ?? BRAND.grey;
  const value = fmtBps(payload.value);
  const clickable = payload.type === "device" || payload.type === "port";
  const tx = x + width + 6;
  const cy = y + height / 2;
  const twoLines = height >= 26;
  return (
    <g style={{ cursor: clickable ? "pointer" : "default" }}>
      <rect x={x} y={y} width={width} height={Math.max(height, 1)} rx={2} fill={color} />
      <text
        x={tx}
        y={twoLines ? cy - 3 : cy}
        dominantBaseline={twoLines ? "auto" : "middle"}
        fontSize={11}
        fontWeight={500}
        fill="var(--foreground)"
        stroke="var(--card)"
        strokeWidth={3}
        paintOrder="stroke"
      >
        {twoLines ? truncate(payload.name, LABEL_CHARS) : oneLineLabel(payload.name, value)}
      </text>
      {twoLines && (
        <text
          x={tx}
          y={cy + 11}
          fontSize={10}
          fill="var(--muted-foreground)"
          stroke="var(--card)"
          strokeWidth={3}
          paintOrder="stroke"
        >
          {value}
          {payload.client_count ? ` · ${payload.client_count} client${payload.client_count === 1 ? "" : "s"}` : ""}
        </text>
      )}
    </g>
  );
}

interface LinkShapeProps {
  sourceX: number;
  targetX: number;
  sourceY: number;
  targetY: number;
  sourceControlX: number;
  targetControlX: number;
  linkWidth: number;
  payload: { source: TrafficSankeyNode };
}

// Links are filled ribbons between the top and bottom edge curves. A stroked
// centre line of width linkWidth bulges far outside its end slots when the
// link is wide and steep (it swept over the Internet node); a ribbon never
// leaves the vertical span of its two ends.
function SankeyLinkShape({ sourceX, targetX, sourceY, targetY, sourceControlX, targetControlX, linkWidth, payload }: LinkShapeProps) {
  const color = NODE_COLOR[payload.source?.type] ?? BRAND.grey;
  const h = Math.max(1, linkWidth) / 2;
  const d =
    `M${sourceX},${sourceY - h} C${sourceControlX},${sourceY - h} ${targetControlX},${targetY - h} ${targetX},${targetY - h}` +
    ` L${targetX},${targetY + h} C${targetControlX},${targetY + h} ${sourceControlX},${sourceY + h} ${sourceX},${sourceY + h} Z`;
  return (
    <path
      d={d}
      fill={color}
      fillOpacity={payload.source && isLocal(payload.source) ? 0.12 : 0.22}
      stroke="none"
      className="transition-[fill-opacity] hover:[fill-opacity:0.5]"
    />
  );
}

function SankeyTip({ active, payload }: TooltipContentProps) {
  if (!active || !payload?.length) return null;
  const item = payload[0];
  const p = item.payload as { source?: TrafficSankeyNode; target?: TrafficSankeyNode; source_iface?: string } | undefined;
  const isLink = !!p?.source && !!p?.target;
  return (
    <div className="rounded-md border bg-popover px-3 py-2 text-xs text-popover-foreground shadow-md">
      {isLink ? (
        <>
          <div className="font-medium">
            {p!.source!.name} → {p!.target!.name}
          </div>
          {p!.source_iface && <div className="font-mono text-muted-foreground">via {p!.source_iface}</div>}
        </>
      ) : (
        <div className="font-medium">{String(item.name ?? "")}</div>
      )}
      <div className="font-mono tabular-nums">{fmtBps(Number(item.value ?? 0))}</div>
    </div>
  );
}

// FlowSankey: the Flows tab — an ESTIMATED source→sink tree built by the
// server from port counters and the inferred topology (no flow export). One
// direction at a time; clicking a device re-roots the tree at it, clicking a
// port leaf opens its detail.
export function FlowSankey({
  params,
  update,
  devices,
  onOpenPort,
  onOpenDevice,
}: {
  params: TrafficParams;
  update: UpdateParams;
  devices: Device[];
  onOpenPort: (deviceId: string, iface: string) => void;
  onOpenDevice: (deviceId: string) => void;
}) {
  const { token } = useAuth();
  const { range, flow, device } = params;
  const live = range === "live";
  const fetcher = useCallback(
    () => api.traffic.sankey(token!, { range, dir: flow, device: device ?? undefined }),
    [token, range, flow, device],
  );
  const key = token ? `sankey|${range}|${flow}|${device ?? ""}` : null;
  const { data, error, loading } = usePolledFetch(key, fetcher, live ? LIVE_REFRESH_MS : RANGE_REFRESH_MS);

  const sortedDevices = useMemo(() => [...devices].sort((a, b) => deviceName(a).localeCompare(deviceName(b))), [devices]);
  const dims = useMemo(() => {
    if (!data || data.nodes.length === 0) return null;
    const { depth, leaves, maxCol, lastColChars } = shape(data);
    const right = Math.max(48, Math.ceil(lastColChars * CHAR_PX) + 16);
    // No fixed height cap: Recharts' yRatio goes negative (garbled layout)
    // once a column's padding exceeds the height, so the chart grows with its
    // fullest column instead and the page scrolls.
    const rows = Math.max(leaves, maxCol);
    return {
      right,
      minWidth: Math.max(640, MARGIN.left + depth * COL_WIDTH + NODE_WIDTH + right),
      height: Math.max(320, rows * ROW_HEIGHT + MARGIN.top + MARGIN.bottom + 20),
    };
  }, [data]);
  const hasLocal = useMemo(() => !!data?.nodes.some(isLocal), [data]);

  // Deep trees are wider than the card and scroll sideways; say so, since
  // nothing else hints that the last columns are off-screen.
  const scrollRef = useRef<HTMLDivElement>(null);
  const [overflows, setOverflows] = useState(false);
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    // The observer fires once on observe(), so no synchronous check is needed.
    const ro = new ResizeObserver(() => setOverflows(el.scrollWidth > el.clientWidth + 1));
    ro.observe(el);
    if (el.firstElementChild) ro.observe(el.firstElementChild);
    return () => ro.disconnect();
  }, [dims]);
  // Every east-west source is named "Local / east-west" by the server and
  // they all sit in the first column, far from the device each one feeds —
  // name them after that device ("Local → switch001") so they can be told apart.
  const chartData = useMemo(() => {
    if (!data) return null;
    const feeds = new Map<number, number>();
    for (const l of data.links) feeds.set(l.source, l.target);
    return {
      nodes: data.nodes.map((n, i) => {
        const target = isLocal(n) ? data.nodes[feeds.get(i) ?? -1] : undefined;
        return target ? { ...n, name: `Local → ${target.name}` } : { ...n };
      }),
      links: data.links.map((l) => ({ ...l })),
    };
  }, [data]);

  const renderNode = useCallback((props: unknown) => <SankeyNodeShape {...(props as NodeShapeProps)} />, []);
  const renderLink = useCallback((props: unknown) => <SankeyLinkShape {...(props as LinkShapeProps)} />, []);
  const onClick = useCallback(
    (item: unknown, type: string) => {
      if (type !== "node") return;
      const n = (item as { payload?: TrafficSankeyNode }).payload;
      if (!n) return;
      if (n.type === "device" && n.device_id) update({ device: n.device_id }, { push: true });
      else if (n.type === "port" && n.device_id && n.iface) onOpenPort(n.device_id, n.iface);
    },
    [update, onOpenPort],
  );

  const rootName = device ? (() => {
    const d = devices.find((x) => x.id === device);
    return d ? deviceName(d) : device.slice(0, 8);
  })() : null;

  return (
    <Card>
      <CardHeader className="gap-3">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="min-w-0">
            <CardTitle className="flex flex-wrap items-center gap-2 text-base">
              Flows
              <Badge variant="outline" className="gap-1 border-amber-500/40 font-normal text-amber-800 dark:text-amber-300" title="Derived from interface byte counters and the inferred L2 tree — not from flow export. Shared segments and hairpinned traffic can make it approximate.">
                <GitFork className="h-3 w-3" />
                Estimated from port counters + topology
              </Badge>
            </CardTitle>
            <p className="text-xs text-muted-foreground">
              {flow === "download" ? "Download: internet → devices → edge ports" : "Upload: edge ports → devices → internet (drawn from the internet side)"}
              {" · "}
              {live ? "current rates" : `average over the last ${RANGE_LABEL[range]}`}
              {rootName ? ` · rooted at ${rootName}` : ""}. Click a device to root the tree there, a port to open it.
            </p>
            {hasLocal && (
              <p className="text-xs text-muted-foreground">
                {flow === "download"
                  ? "Includes local (east-west) traffic: what a device sends toward its ports beyond what it receives from its parent — e.g. from a NAS or server — enters as a “Local → <device>” source."
                  : "Includes local (east-west) traffic: what a device's ports send beyond what it forwards to its parent enters as a “Local → <device>” source."}
              </p>
            )}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Segmented
            ariaLabel="Flow direction"
            value={flow}
            onChange={(f) => update({ flow: f })}
            options={[
              { value: "download", label: "↓ Download" },
              { value: "upload", label: "↑ Upload" },
            ]}
          />
          <select
            aria-label="Root device"
            className="h-8 max-w-[240px] rounded-md border bg-background px-2 text-sm"
            value={device ?? ""}
            onChange={(e) => update({ device: e.target.value || null })}
          >
            <option value="">Whole network</option>
            {sortedDevices.map((d) => (
              <option key={d.id} value={d.id}>
                {deviceName(d)}
              </option>
            ))}
          </select>
          {device && (
            <>
              <Button variant="outline" size="sm" onClick={() => update({ device: null }, { push: true })}>
                <RotateCcw /> Whole network
              </Button>
              <Button variant="ghost" size="sm" onClick={() => onOpenDevice(device)}>
                <Server /> Open device
              </Button>
            </>
          )}
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {error && <Notice kind="error">Could not load flows: {error}</Notice>}
        {loading && !data ? (
          <div className="h-[320px] animate-pulse rounded-lg bg-muted" />
        ) : data && data.nodes.length === 0 ? (
          <EmptyState icon={GitFork}>No flows above 1 kbps (or topology not discovered yet).</EmptyState>
        ) : chartData && dims ? (
          <>
            {overflows && (
              <p className="flex items-center justify-end gap-1 text-[11px] text-muted-foreground">
                Deeper levels continue to the right — scroll sideways <ArrowRight className="h-3 w-3" />
              </p>
            )}
            <div ref={scrollRef} className="overflow-x-auto">
              <div style={{ minWidth: dims.minWidth, height: dims.height }} className={loading ? "opacity-60" : undefined}>
                <ResponsiveContainer width="100%" height="100%">
                  <Sankey
                    data={chartData}
                    node={renderNode}
                    link={renderLink}
                    nodeWidth={NODE_WIDTH}
                    nodePadding={NODE_PADDING}
                    linkCurvature={0.5}
                    iterations={48}
                    align="left"
                    margin={{ ...MARGIN, right: dims.right }}
                    onClick={onClick}
                  >
                    <Tooltip content={SankeyTip} />
                  </Sankey>
                </ResponsiveContainer>
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
              {(
                [
                  ["internet", "Internet"],
                  ["gateway", "Gateway"],
                  ["vpn", "VPN"],
                  ["device", "Device"],
                  ["port", hasLocal ? "Edge port / local / other" : "Edge port / other"],
                ] as [TrafficSankeyNode["type"], string][]
              ).map(([t, l]) => (
                <span key={t} className="inline-flex items-center gap-1.5">
                  <span className="h-2.5 w-2.5 rounded-sm" style={{ background: NODE_COLOR[t] }} />
                  {l}
                </span>
              ))}
              <span>· edge width = bits/s</span>
            </div>
          </>
        ) : null}
      </CardContent>
    </Card>
  );
}
