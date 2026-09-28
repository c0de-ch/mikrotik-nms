"use client";

import { useCallback, useMemo, useState } from "react";
import { Activity } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { useAuth } from "@/context/auth";
import { api, type PortRole, type PortSnapshot, type TopDir, type TopMetric } from "@/lib/api";
import type { TrafficParams, UpdateParams } from "./hooks";
import { usePolledFetch } from "./hooks";
import { METRIC_OPTIONS, RANGE_LABEL, RX_COLOR, TX_COLOR, collectingText, fmtDateTime, refreshMs, snapshotReady } from "./lib";
import { buildLiveRows } from "./live-rank";
import { EmptyState, Notice } from "./notice";
import { PortTable } from "./port-table";
import { DirKey } from "./rate-bar";
import { Segmented, Toggle } from "./segmented";

const PAGE = 20;
const MAX_LIMIT = 100;

const DIR_OPTIONS = [
  { value: "total" as TopDir, label: "Total", title: "rx + tx" },
  { value: "rx" as TopDir, label: "RX in", title: "Bits entering the device on the port" },
  { value: "tx" as TopDir, label: "TX out", title: "Bits leaving the device on the port" },
];

// TopTalkers: fleet-wide ranked ports for the selected range.
export function TopTalkers({
  params,
  update,
  snapshot,
  roles,
  nameOf,
  onOpenPort,
  onOpenDevice,
}: {
  params: TrafficParams;
  update: UpdateParams;
  snapshot: PortSnapshot | null;
  roles: Map<string, PortRole>;
  nameOf: (deviceId: string) => string;
  onOpenPort: (deviceId: string, iface: string) => void;
  onOpenDevice: (deviceId: string) => void;
}) {
  const { token } = useAuth();
  const [limit, setLimit] = useState(PAGE);
  const { range, dir, physical } = params;
  const live = range === "live";
  // Peak/volume are meaningless for a single instant: live ranks by "now".
  const metric: TopMetric = live ? "avg" : params.metric;
  // Live asks for more rows than it shows so re-ranked rows keep sparklines.
  const fetchLimit = live ? Math.min(200, limit * 2) : limit;

  const fetcher = useCallback(
    () => api.traffic.portsTop(token!, { range, metric, dir, limit: fetchLimit, physical }),
    [token, range, metric, dir, fetchLimit, physical],
  );
  const key = token ? `top|${range}|${metric}|${dir}|${fetchLimit}|${physical}` : null;
  // "Show more" only changes the limit: keep the current rows on screen while
  // the longer page loads instead of collapsing to a skeleton (scroll jump).
  const group = `top|${range}|${metric}|${dir}|${physical}`;
  const { data, error, loading } = usePolledFetch(key, fetcher, refreshMs(range), { group });

  const rows = useMemo(() => {
    if (!live) return data?.rows ?? [];
    if (!snapshotReady(snapshot)) return [];
    return buildLiveRows({
      ports: snapshot!.ports,
      serverRows: data?.rows ?? [],
      roles,
      nameOf,
      metric,
      dir,
      physical,
      includeIdle: false,
      limit,
    });
  }, [live, data, snapshot, roles, nameOf, metric, dir, physical, limit]);

  const tableLoading = live ? !snapshotReady(snapshot) : loading;
  const coverageLate =
    !live && data?.coverage_from && data.from && Date.parse(data.coverage_from) > Date.parse(data.from);
  const noHistory = !live && data && data.coverage_from === null;

  const description = live
    ? "Ranked by current rate, refreshed every collector cycle."
    : `Ranked by ${metric === "avg" ? "average rate" : metric === "max" ? "peak rate" : "volume"} over the last ${RANGE_LABEL[range]}${data?.resolution ? ` · ${data.resolution} resolution` : ""}.`;

  return (
    <Card>
      <CardHeader className="gap-3">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="min-w-0">
            <CardTitle className="text-base">Top talkers</CardTitle>
            <p className="text-xs text-muted-foreground">{description}</p>
          </div>
          <div className="flex flex-wrap items-center gap-3 text-[11px] text-muted-foreground">
            <DirKey color={RX_COLOR}>download / in</DirKey>
            <DirKey color={TX_COLOR}>upload / out</DirKey>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Segmented
            ariaLabel="Ranking metric"
            value={metric}
            onChange={(m) => update({ metric: m })}
            options={METRIC_OPTIONS(live)}
          />
          <Segmented ariaLabel="Direction" value={dir} onChange={(d) => update({ dir: d })} options={DIR_OPTIONS} />
          <Toggle
            pressed={physical}
            onChange={(v) => update({ physical: v })}
            title="Hide bridges, VLANs, tunnels and other logical interfaces (they double-count physical traffic)"
          >
            Physical ports only
          </Toggle>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {error && <Notice kind="error">Could not load top talkers: {error}</Notice>}
        {live && !snapshotReady(snapshot) && <Notice kind="loading">{collectingText(snapshot)}</Notice>}
        {noHistory && (
          <Notice>No history for this range yet — the collector writes 1-minute buckets; check back in a few minutes.</Notice>
        )}
        {coverageLate && <Notice>Data since {fmtDateTime(data!.coverage_from!)}</Notice>}

        {rows.length === 0 && !tableLoading && (noHistory || error) ? null : rows.length === 0 && !tableLoading ? (
          <EmptyState icon={Activity}>
            {live ? "No port is moving traffic right now." : "No port traffic in this range."}
          </EmptyState>
        ) : (
          <PortTable
            rows={rows}
            metric={metric}
            dir={dir}
            live={live}
            showDevice
            loading={tableLoading}
            onOpenPort={onOpenPort}
            onOpenDevice={onOpenDevice}
          />
        )}

        {rows.length >= limit && limit < MAX_LIMIT && (
          <div className="flex justify-center">
            <Button variant="outline" size="sm" onClick={() => setLimit((l) => Math.min(MAX_LIMIT, l + PAGE * 2))}>
              Show more
            </Button>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
