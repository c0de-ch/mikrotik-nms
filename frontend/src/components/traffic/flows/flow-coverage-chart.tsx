"use client";

import { useCallback, useMemo, useState } from "react";
import { CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { ChevronDown, ChevronRight } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { fmtBps } from "@/components/graph/graph-style";
import { useAuth } from "@/context/auth";
import { api, type FlowPoint, type TrafficRange } from "@/lib/api";
import { usePolledFetch } from "../hooks";
import { RX_COLOR, TX_COLOR, fmtBpsAxis, fmtTick, fmtTooltipTime } from "../lib";
import { EmptyState, Notice } from "../notice";
import { DirKey } from "../rate-bar";
import { CoverageBadge } from "./source-badge";
import { flowDirLabels, flowRefreshMs } from "./flow-lib";

// FlowCoverageChart (collapsible): the point's flow bytes against the Phase 1
// counters of its managed port, per step. Flows count L3 bytes, counters L2
// frames, so flows run a few percent low even when nothing is missing; a
// large gap means traffic the exporter does not see. Only for points with a
// managed port (coverage_capable). Fetches only while open.
export function FlowCoverageChart({
  point,
  range,
  refreshKey,
}: {
  point: FlowPoint;
  range: TrafficRange;
  refreshKey?: number;
}) {
  const { token } = useAuth();
  const [open, setOpen] = useState(false);
  const key = token && open ? `fcov|${point.id}|${range}|${refreshKey ?? 0}` : null;
  const fetcher = useCallback(() => api.flows.coverage(token!, { point: point.id, range }), [token, point.id, range]);
  const { data, error, loading } = usePolledFetch(key, fetcher, flowRefreshMs(range), { group: `fcov|${point.id}|${range}` });
  const labels = flowDirLabels(point);

  const chart = useMemo(
    () =>
      (data?.points ?? []).map((p) => ({
        t: Date.parse(p.ts),
        flowDown: p.flow_down_bps,
        flowUp: p.flow_up_bps,
        ctrDown: p.counter_down_bps,
        ctrUp: p.counter_up_bps,
      })),
    [data],
  );
  const plotted = chart.some((c) => c.flowDown !== null || c.ctrDown !== null);

  return (
    <Card className="min-w-0">
      <CardHeader className="gap-1">
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          aria-expanded={open}
          className="flex w-full items-center justify-between gap-2 text-left"
        >
          <CardTitle className="flex items-center gap-1.5 text-base">
            {open ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
            Coverage: flows vs port counters
          </CardTitle>
          {data && (
            <span className="flex flex-wrap items-center justify-end gap-1.5">
              {data.ratio.download !== null && <CoverageBadge value={data.ratio.download} label="↓" />}
              {data.ratio.upload !== null && <CoverageBadge value={data.ratio.upload} label="↑" />}
            </span>
          )}
        </button>
        {open && (
          <p className="text-xs text-muted-foreground">
            Solid: flow export ({point.exporter_name}). Dashed: port counters of {point.device_name || "the port"}
            {point.iface ? ` · ${point.iface}` : ""}. Flows count IP bytes, counters count Ethernet frames — expect flows a few
            percent lower.
          </p>
        )}
      </CardHeader>
      {open && (
        <CardContent className="space-y-3">
          {error && <Notice kind="error">Could not load coverage: {error}</Notice>}
          {!data && !error ? (
            <div className="h-[220px] animate-pulse rounded-lg bg-muted" />
          ) : data && !plotted ? (
            <EmptyState>No flow or counter data for this range yet.</EmptyState>
          ) : data ? (
            <>
              <div className="flex flex-wrap items-center gap-3 text-[11px] text-muted-foreground">
                <DirKey color={RX_COLOR}>{labels.down}</DirKey>
                <DirKey color={TX_COLOR}>{labels.up}</DirKey>
                <span>— flows · - - counters</span>
              </div>
              <div className={`h-[220px] w-full ${loading ? "opacity-60" : ""}`}>
                <ResponsiveContainer width="100%" height="100%">
                  <LineChart data={chart} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
                    <CartesianGrid vertical={false} stroke="var(--border)" />
                    <XAxis
                      dataKey="t"
                      type="number"
                      scale="time"
                      domain={["dataMin", "dataMax"]}
                      tickFormatter={(v) => fmtTick(Number(v), range)}
                      tick={{ fontSize: 11, fill: "var(--muted-foreground)" }}
                      stroke="var(--border)"
                      minTickGap={40}
                    />
                    <YAxis
                      tickFormatter={(v) => fmtBpsAxis(Number(v))}
                      tick={{ fontSize: 11, fill: "var(--muted-foreground)" }}
                      stroke="var(--border)"
                      width={76}
                    />
                    <Tooltip
                      labelFormatter={(v) => fmtTooltipTime(Number(v), range)}
                      formatter={(v, name) => [v === null || v === undefined ? "no data" : fmtBps(Number(v)), name]}
                      contentStyle={{ background: "var(--popover)", border: "1px solid var(--border)", borderRadius: 8, fontSize: 12 }}
                      labelStyle={{ color: "var(--muted-foreground)" }}
                      itemStyle={{ color: "var(--popover-foreground)" }}
                    />
                    <Line type="monotone" dataKey="flowDown" name={`${labels.down} · flows`} stroke={RX_COLOR} strokeWidth={2} dot={false} connectNulls={false} isAnimationActive={false} />
                    <Line type="monotone" dataKey="flowUp" name={`${labels.up} · flows`} stroke={TX_COLOR} strokeWidth={2} dot={false} connectNulls={false} isAnimationActive={false} />
                    <Line type="monotone" dataKey="ctrDown" name={`${labels.down} · counters`} stroke={RX_COLOR} strokeWidth={1.5} strokeDasharray="4 3" dot={false} connectNulls={false} isAnimationActive={false} />
                    <Line type="monotone" dataKey="ctrUp" name={`${labels.up} · counters`} stroke={TX_COLOR} strokeWidth={1.5} strokeDasharray="4 3" dot={false} connectNulls={false} isAnimationActive={false} />
                  </LineChart>
                </ResponsiveContainer>
              </div>
            </>
          ) : null}
        </CardContent>
      )}
    </Card>
  );
}
