"use client";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { fmtBps } from "@/components/graph/graph-style";
import type { FlowDirection, FlowGroup, FlowOther, FlowTopResponse, FlowTopRow } from "@/lib/api";
import { RX_COLOR, TX_COLOR, fmtBytes } from "../lib";
import { Notice } from "../notice";
import { AppLabel, ConversationLabel, EndpointLabel } from "./endpoint-label";

export type TopGroup = Extract<FlowGroup, "src" | "dst" | "conv" | "app">;

export interface TopTableState {
  data: FlowTopResponse | null;
  error: string | null;
  loading: boolean;
}

// Table titles per direction: on download the source is the remote side, on
// upload the local side.
export function topTitle(group: TopGroup, dir: FlowDirection): string {
  if (group === "conv") return "Top conversations";
  if (group === "app") return "Top apps";
  const remote = (group === "src") === (dir === "download");
  return group === "src" ? `Top sources (${remote ? "remote" : "local"})` : `Top destinations (${remote ? "remote" : "local"})`;
}

// FlowRowsTable: ranked flow rows with bytes, average rate and a share bar,
// plus the "Other (below top-N)" remainder. Phone-friendly: the label and
// the numbers sit on one line, the share bar spans the row underneath.
export function FlowRowsTable({
  rows,
  other,
  group,
  dir,
  loading,
  empty,
  wide,
  onOpenPort,
  onNat,
}: {
  rows: FlowTopRow[];
  other: FlowOther | null;
  group: FlowGroup;
  dir: FlowDirection;
  loading?: boolean;
  empty?: string;
  // Full-width card: conversations on one line from the sm breakpoint.
  wide?: boolean;
  onOpenPort?: (deviceId: string, iface: string) => void;
  onNat?: (exporterName: string) => void;
}) {
  const color = dir === "download" ? RX_COLOR : TX_COLOR;
  if (rows.length === 0 && !(other && other.bytes > 0)) {
    return <p className="py-6 text-center text-xs text-muted-foreground">{empty ?? "No traffic in this direction for the range."}</p>;
  }
  return (
    <ol className={loading ? "opacity-60" : undefined}>
      {rows.map((r) => (
        <li key={r.key} className="border-b py-2 last:border-b-0">
          <div className="grid grid-cols-[1.5rem_minmax(0,1fr)_auto] items-start gap-x-2">
            <span className="pt-px text-right font-mono text-xs text-muted-foreground tabular-nums">{r.rank}</span>
            <div className="min-w-0 text-sm">
              <RowLabel row={r} group={group} wide={wide} onOpenPort={onOpenPort} onNat={onNat} />
            </div>
            <Values bytes={r.bytes} bps={r.avg_bps} />
          </div>
          <ShareBar share={r.share} color={color} />
        </li>
      ))}
      {other && other.bytes > 0 && (
        <li className="py-2">
          <div className="grid grid-cols-[1.5rem_minmax(0,1fr)_auto] items-start gap-x-2">
            <span />
            <span className="text-sm text-muted-foreground" title="Everything outside the rows above, plus flows the collector folded into “other” at ingest (per-minute row cap)">
              Other (below top {rows.length})
            </span>
            <Values bytes={other.bytes} bps={other.avg_bps} />
          </div>
          <ShareBar share={other.share} color="var(--muted-foreground)" />
        </li>
      )}
    </ol>
  );
}

function RowLabel({
  row,
  group,
  wide,
  onOpenPort,
  onNat,
}: {
  row: FlowTopRow;
  group: FlowGroup;
  wide?: boolean;
  onOpenPort?: (deviceId: string, iface: string) => void;
  onNat?: (exporterName: string) => void;
}) {
  switch (group) {
    case "src":
      return <EndpointLabel ep={row.src} onOpenPort={onOpenPort} onNat={onNat} />;
    case "dst":
      return <EndpointLabel ep={row.dst} onOpenPort={onOpenPort} onNat={onNat} />;
    case "app":
      return <AppLabel app={row.app} />;
    case "pair":
      return <ConversationLabel src={row.src} dst={row.dst} wide={wide} onOpenPort={onOpenPort} onNat={onNat} />;
    default:
      return <ConversationLabel src={row.src} dst={row.dst} app={row.app} wide={wide} onOpenPort={onOpenPort} onNat={onNat} />;
  }
}

function Values({ bytes, bps }: { bytes: number; bps: number }) {
  return (
    <div className="text-right font-mono text-xs tabular-nums">
      <div className="font-medium">{fmtBytes(bytes)}</div>
      <div className="text-muted-foreground" title="Average over the covered part of the range">
        {fmtBps(bps)}
      </div>
    </div>
  );
}

function ShareBar({ share, color }: { share: number; color: string }) {
  const pct = Math.max(0, Math.min(100, share * 100));
  return (
    <div
      className="ml-[2rem] mt-1 h-1.5 overflow-hidden rounded-full bg-muted"
      title={`${pct < 1 && pct > 0 ? "<1" : Math.round(pct)} % of the direction's bytes`}
    >
      {pct > 0 && <div className="h-full rounded-full" style={{ width: `max(${pct}%, 2px)`, background: color }} />}
    </div>
  );
}

const GROUPS: TopGroup[] = ["src", "dst", "conv", "app"];

// FlowTopTables: the four ranked lists of one point, direction and range
// (2×2 grid, one column on phones).
export function FlowTopTables({
  dir,
  tables,
  onOpenPort,
  onNat,
}: {
  dir: FlowDirection;
  tables: Record<TopGroup, TopTableState>;
  onOpenPort?: (deviceId: string, iface: string) => void;
  onNat?: (exporterName: string) => void;
}) {
  return (
    <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
      {GROUPS.map((g) => {
        const t = tables[g];
        return (
          <Card key={g} size="sm" className="min-w-0">
            <CardHeader>
              <CardTitle className="text-sm">{topTitle(g, dir)}</CardTitle>
            </CardHeader>
            <CardContent>
              {t.error && !t.data && <Notice kind="error">Could not load: {t.error}</Notice>}
              {!t.data && !t.error ? (
                <div className="h-40 animate-pulse rounded-md bg-muted" />
              ) : t.data ? (
                <FlowRowsTable
                  rows={t.data.rows}
                  other={t.data.other}
                  group={g}
                  dir={dir}
                  loading={t.loading}
                  onOpenPort={onOpenPort}
                  onNat={onNat}
                />
              ) : null}
            </CardContent>
          </Card>
        );
      })}
    </div>
  );
}
