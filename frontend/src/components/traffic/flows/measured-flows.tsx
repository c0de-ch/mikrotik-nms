"use client";

import { useCallback, useState } from "react";
import { useAuth } from "@/context/auth";
import { fmtBps } from "@/components/graph/graph-style";
import { api, type FlowDirection, type FlowPoint, type FlowRemoteGrouping, type TrafficRange } from "@/lib/api";
import { usePolledFetch } from "../hooks";
import { RANGE_LABEL, fmtDateTime } from "../lib";
import { Notice } from "../notice";
import { FlowCoverageChart } from "./flow-coverage-chart";
import { FlowTopTables, type TopGroup, type TopTableState } from "./flow-top-tables";
import {
  coverageHint,
  coverageWarn,
  fmtAgo,
  fmtCount,
  fmtHM,
  flowRefreshMs,
  isRecent,
  isToday,
  refreshesOnFlush,
} from "./flow-lib";
import { MeasuredSankey } from "./measured-sankey";
import { SourceBadge } from "./source-badge";
import { useFlushedRefresh } from "./use-flows";

const TOP_LIMIT = 10;

// The window end counts as current when it trails "now" by no more than the
// normal lag: ≈2 min for minute flushes, up to ≈62 min for hourly rollups
// (run at xx+1:02). Older ends mean the collector stopped recording.
const CURRENT_MS: Record<"1m" | "1h", number> = { "1m": 5 * 60_000, "1h": 65 * 60_000 };

function useTop(
  token: string | null,
  point: number,
  range: TrafficRange,
  dir: FlowDirection,
  group: TopGroup,
): TopTableState & { reload: () => void } {
  const fetcher = useCallback(
    // resolve=1: the server adds PTR names only when flow_resolve_ptr is on.
    () => api.flows.top(token!, { point, range, dir, group, limit: TOP_LIMIT, resolve: true }),
    [token, point, range, dir, group],
  );
  const { data, error, loading, reload } = usePolledFetch(
    token ? `ftop|${point}|${range}|${dir}|${group}` : null,
    fetcher,
    flowRefreshMs(range),
  );
  return { data, error, loading, reload };
}

// MeasuredFlows: the measured side of the Flows tab for one observation
// point — summary line and notices, the flow graph, the four top lists and
// (for points with a managed port) the coverage chart.
export function MeasuredFlows({
  point,
  range,
  dir,
  remote,
  collectorOn,
  onOpenPort,
  onNat,
}: {
  point: FlowPoint;
  range: TrafficRange;
  dir: FlowDirection;
  remote: FlowRemoteGrouping;
  // null while /flows/status is loading or failed: neither "on" nor "off".
  collectorOn: boolean | null;
  onOpenPort: (deviceId: string, iface: string) => void;
  onNat?: (exporterName: string) => void;
}) {
  const { token } = useAuth();
  const sankeyFetcher = useCallback(
    () => api.traffic.sankey(token!, { range, dir, source: "flows", point: point.id, remote }),
    [token, range, dir, point.id, remote],
  );
  const sankey = usePolledFetch(token ? `fsk|${point.id}|${range}|${dir}|${remote}` : null, sankeyFetcher, flowRefreshMs(range));

  const src = useTop(token, point.id, range, dir, "src");
  const dst = useTop(token, point.id, range, dir, "dst");
  const conv = useTop(token, point.id, range, dir, "conv");
  const app = useTop(token, point.id, range, dir, "app");

  // New minutes flushed for this exporter: refetch in place (same keys, so
  // the current rows stay on screen) and bump the coverage chart.
  const [nonce, setNonce] = useState(0);
  const reloadSankey = sankey.reload;
  const reloadSrc = src.reload;
  const reloadDst = dst.reload;
  const reloadConv = conv.reload;
  const reloadApp = app.reload;
  const refresh = useCallback(() => {
    reloadSankey();
    reloadSrc();
    reloadDst();
    reloadConv();
    reloadApp();
    setNonce((n) => n + 1);
  }, [reloadSankey, reloadSrc, reloadDst, reloadConv, reloadApp]);
  // Subscribing while the status is unknown is harmless (no events arrive
  // when the collector is off).
  useFlushedRefresh(collectorOn !== false && refreshesOnFlush(range), point.exporter_id, refresh);

  const tables: Record<TopGroup, TopTableState> = { src, dst, conv, app };

  // Window facts are the same in every /flows/top answer of this point/range.
  const meta = conv.data ?? src.data ?? dst.data ?? app.data;
  const sampling = sankey.data?.sampling_rate ?? meta?.sampling_rate ?? point.sampling_rate;
  const coverage = sankey.data?.coverage ?? null;
  const to = meta?.to ?? sankey.data?.to ?? null;
  const noData = !!meta && meta.coverage_from === null;
  const lateStart = !!meta?.coverage_from && Date.parse(meta.coverage_from) > Date.parse(meta.from);
  const derived = point.kind === "derived";
  const hourly = meta ? meta.resolution === "1h" : range === "7d" || range === "30d";
  // A window anchored at old stored data (collector off or stopped): say so
  // and show the date, instead of a "Last 24h … data through 14:06" that
  // reads as today.
  const current = !!to && collectorOn === true && isRecent(to, CURRENT_MS[hourly ? "1h" : "1m"]);
  const pastEnd = !!to && !current;
  const rangeText = range === "live" ? "Last 5 minutes" : `Last ${RANGE_LABEL[range]}`;
  const pastText = `${range === "live" ? "5-minute" : RANGE_LABEL[range]} window ending at the last stored data`;

  return (
    <div className="space-y-4">
      <div className="space-y-2">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 text-xs text-muted-foreground">
          <SourceBadge
            kind={derived ? "derived" : "measured"}
            protocol={point.protocol}
            exporter={point.exporter_name}
            sampling={sampling}
            coverage={point.coverage_capable ? coverage : null}
            coverageLabel={dir === "download" ? "↓" : "↑"}
            note={point.note}
          />
          <span>
            {pastEnd ? pastText : rangeText}
            {meta ? ` · ${meta.resolution === "1h" ? "hourly" : "1-minute"} buckets` : ""}
            {meta && meta.seconds > 0 ? ` · ${fmtBps(meta.total_bps)} average` : ""}
          </span>
          {to && (
            <span>
              data through {current || isToday(to) ? fmtHM(to) : fmtDateTime(to)}
              {current ? ` (${hourly ? "hourly rollups" : "flows lag ≈ 2 min"})` : ""}
            </span>
          )}
        </div>
        {collectorOn === false && (
          <Notice kind="warn">Flow collector is off — showing stored history only; nothing new is recorded.</Notice>
        )}
        {!point.enabled && <Notice>This view is disabled — it is hidden from the picker; stored history stays readable.</Notice>}
        {collectorOn === true && point.exporter_state === "stale" && (
          <Notice kind="warn">
            {point.exporter_name} has not sent flows for a while (last data {fmtAgo(point.last_data)}) — recent minutes may be
            missing.
          </Notice>
        )}
        {point.exporter_state === "disabled" && (
          <Notice kind="warn">Exporter {point.exporter_name} is disabled — its datagrams are dropped; only older data is shown.</Notice>
        )}
        {noData && <Notice>No flow data for this range yet.</Notice>}
        {lateStart && <Notice>Flow data since {fmtDateTime(meta!.coverage_from!)}</Notice>}
        {derived && point.note && <Notice>{point.note}</Notice>}
        {point.coverage_capable && coverage !== null && coverageWarn(coverage) && (
          <Notice kind="warn">{coverageHint(coverage)}</Notice>
        )}
        {!!meta && meta.overflow > 0 && (
          <Notice>
            Busy minutes exceeded the collector&apos;s per-minute key cap: {fmtCount(meta.overflow)} flow records are counted
            under “Other” (totals stay exact).
          </Notice>
        )}
      </div>

      {/* Nothing recorded in the window: the notice above says so; empty
          graph and tables would only repeat it. */}
      {!noData && (
        <>
          <MeasuredSankey
            data={sankey.data}
            error={sankey.error}
            loading={sankey.loading}
            dir={dir}
            remote={remote}
            onOpenPort={onOpenPort}
          />
          <FlowTopTables dir={dir} tables={tables} onOpenPort={onOpenPort} onNat={onNat} />
        </>
      )}

      {point.coverage_capable && <FlowCoverageChart point={point} range={range} refreshKey={nonce} />}
    </div>
  );
}
