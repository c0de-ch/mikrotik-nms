"use client";

// Shared pieces of the /traffic Sankey charts: the estimated Phase 1 tree
// (flow-sankey.tsx) and the measured flow-export graph
// (flows/measured-sankey.tsx) — node and link shapes, label truncation,
// column sizing and the sideways-scroll container.

import { useEffect, useRef, useState } from "react";
import type { TooltipContentProps } from "recharts";
import { ArrowRight } from "lucide-react";
import { BRAND, fmtBps } from "@/components/graph/graph-style";
import type { TrafficSankey, TrafficSankeyNode } from "@/lib/api";

// Recharts spaces columns (contentWidth - NODE_WIDTH) / maxDepth apart; the
// chart's min width keeps that ≥ COL_WIDTH so a label truncated to
// LABEL_CHARS ends before the next column's nodes.
export const COL_WIDTH = 220;
export const NODE_WIDTH = 10;
export const NODE_PADDING = 12;
export const CHAR_PX = 6.4; // average glyph width of the 11px label font
export const LABEL_CHARS = Math.floor((COL_WIDTH - NODE_WIDTH - 22) / CHAR_PX);
export const ROW_HEIGHT = 30;
export const MARGIN = { top: 10, bottom: 10, left: 4 };

export type NodeColors = Partial<Record<TrafficSankeyNode["type"], string>>;

export function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, Math.max(1, n - 1))}…` : s;
}

// "name · value" on one line, the name shortened so the whole label fits in
// LABEL_CHARS (the space before the next column).
export function oneLineLabel(name: string, value: string): string {
  return `${truncate(name, Math.max(8, LABEL_CHARS - value.length - 3))} · ${value}`;
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
function shape(data: Pick<TrafficSankey, "nodes" | "links">): Shape {
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

export interface SankeyDims {
  right: number;
  minWidth: number;
  height: number;
}

// sankeyDims sizes the chart from its column layout; null for an empty graph.
export function sankeyDims(data: Pick<TrafficSankey, "nodes" | "links"> | null | undefined): SankeyDims | null {
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
}

export interface NodeShapeProps {
  x: number;
  y: number;
  width: number;
  height: number;
  payload: TrafficSankeyNode & { value: number; targetNodes: number[] };
}

export function SankeyNodeShape({
  x,
  y,
  width,
  height,
  payload,
  colors,
  clickable,
}: NodeShapeProps & { colors: NodeColors; clickable: boolean }) {
  const color = colors[payload.type] ?? BRAND.grey;
  const value = fmtBps(payload.value);
  const tx = x + width + 6;
  const cy = y + height / 2;
  const twoLines = height >= 26;
  // Measured graph: the address under a resolved name (Phase 1 nodes have none).
  const ip = payload.ip && payload.ip !== payload.name ? ` · ${payload.ip}` : "";
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
          {ip ? truncate(`${value}${ip}`, LABEL_CHARS + 4) : value}
          {payload.client_count ? ` · ${payload.client_count} client${payload.client_count === 1 ? "" : "s"}` : ""}
        </text>
      )}
    </g>
  );
}

export interface LinkShapeProps {
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
// leaves the vertical span of its two ends. faint(source) links are drawn
// lighter (Phase 1: the synthetic east-west "local:" sources).
export function SankeyLinkShape({
  sourceX,
  targetX,
  sourceY,
  targetY,
  sourceControlX,
  targetControlX,
  linkWidth,
  payload,
  colors,
  faint,
}: LinkShapeProps & { colors: NodeColors; faint?: (n: TrafficSankeyNode) => boolean }) {
  const color = colors[payload.source?.type] ?? BRAND.grey;
  const h = Math.max(1, linkWidth) / 2;
  const d =
    `M${sourceX},${sourceY - h} C${sourceControlX},${sourceY - h} ${targetControlX},${targetY - h} ${targetX},${targetY - h}` +
    ` L${targetX},${targetY + h} C${targetControlX},${targetY + h} ${sourceControlX},${sourceY + h} ${sourceX},${sourceY + h} Z`;
  return (
    <path
      d={d}
      fill={color}
      fillOpacity={payload.source && faint?.(payload.source) ? 0.12 : 0.22}
      stroke="none"
      className="transition-[fill-opacity] hover:[fill-opacity:0.5]"
    />
  );
}

// reversed: the chart draws the links against the traffic direction (the
// measured upload graph is laid out from the remote side), so a link reads
// target → source.
export function SankeyTip({ active, payload, reversed }: TooltipContentProps & { reversed?: boolean }) {
  if (!active || !payload?.length) return null;
  const item = payload[0];
  const p = item.payload as { source?: TrafficSankeyNode; target?: TrafficSankeyNode; source_iface?: string } | undefined;
  const isLink = !!p?.source && !!p?.target;
  const ends = isLink ? (reversed ? { from: p!.target!, to: p!.source! } : { from: p!.source!, to: p!.target! }) : null;
  // Node hover: the payload is the node itself.
  const node = !isLink ? (p as Partial<TrafficSankeyNode> | undefined) : undefined;
  return (
    <div className="rounded-md border bg-popover px-3 py-2 text-xs text-popover-foreground shadow-md">
      {ends ? (
        <>
          <div className="font-medium">
            {ends.from.name} → {ends.to.name}
          </div>
          {p!.source_iface && <div className="font-mono text-muted-foreground">via {p!.source_iface}</div>}
        </>
      ) : (
        <>
          <div className="font-medium">{String(item.name ?? "")}</div>
          {node?.ip && node.ip !== node.name && <div className="font-mono text-muted-foreground">{node.ip}</div>}
        </>
      )}
      <div className="font-mono tabular-nums">{fmtBps(Number(item.value ?? 0))}</div>
    </div>
  );
}

// SankeyScroll: the chart's sideways-scroll container. Deep graphs are wider
// than the card and scroll inside it; say so, since nothing else hints that
// the last columns are off-screen.
export function SankeyScroll({
  dims,
  loading,
  children,
}: {
  dims: SankeyDims;
  loading?: boolean;
  children: React.ReactNode;
}) {
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
  return (
    <>
      {overflows && (
        <p className="flex items-center justify-end gap-1 text-[11px] text-muted-foreground">
          Deeper levels continue to the right — scroll sideways <ArrowRight className="h-3 w-3" />
        </p>
      )}
      <div ref={scrollRef} className="overflow-x-auto">
        <div style={{ minWidth: dims.minWidth, height: dims.height }} className={loading ? "opacity-60" : undefined}>
          {children}
        </div>
      </div>
    </>
  );
}

// Colour swatch + label for a Sankey legend.
export function SankeyLegend({ items, suffix }: { items: [string, string][]; suffix?: React.ReactNode }) {
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
      {items.map(([color, label]) => (
        <span key={label} className="inline-flex items-center gap-1.5">
          <span className="h-2.5 w-2.5 rounded-sm" style={{ background: color }} />
          {label}
        </span>
      ))}
      {suffix}
    </div>
  );
}
