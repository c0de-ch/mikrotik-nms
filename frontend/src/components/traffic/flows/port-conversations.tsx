"use client";

import { useCallback, useState } from "react";
import Link from "next/link";
import { ArrowRight } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useAuth } from "@/context/auth";
import { api, type FlowDirection, type TrafficRange } from "@/lib/api";
import { usePolledFetch } from "../hooks";
import { RANGE_LABEL, fmtDateTime } from "../lib";
import { Notice } from "../notice";
import { Segmented } from "../segmented";
import { FlowRowsTable } from "./flow-top-tables";
import { coverageHint, coverageWarn, flowDirLabels, flowRefreshMs, protocolLabel, refreshesOnFlush } from "./flow-lib";
import { CoverageBadge, SourceBadge } from "./source-badge";
import { useFlushedRefresh } from "./use-flows";

const LIMIT = 10;

// PortConversations: the port detail's "Top conversations" card, from the
// flow observation point that covers this port (natively, or derived from a
// neighbouring exporter). Ports no point covers render nothing — no
// placeholder, so the Phase 1 port view is unchanged for them.
export function PortConversations({
  deviceId,
  iface,
  range,
  onOpenPort,
}: {
  deviceId: string;
  iface: string;
  range: TrafficRange;
  onOpenPort?: (deviceId: string, iface: string) => void;
}) {
  const { token } = useAuth();
  const [pick, setPick] = useState<number | null>(null);
  const [dir, setDir] = useState<FlowDirection>("download");
  const fetcher = useCallback(
    () => api.flows.port(token!, { device: deviceId, iface, range, limit: LIMIT, point: pick ?? undefined }),
    [token, deviceId, iface, range, pick],
  );
  const { data, reload } = usePolledFetch(
    token ? `fport|${deviceId}|${iface}|${range}|${pick ?? ""}` : null,
    fetcher,
    flowRefreshMs(range),
    { group: `fport|${deviceId}|${iface}|${range}` },
  );
  const point = data?.reason === "ok" ? data.point : null;
  useFlushedRefresh(!!point && refreshesOnFlush(range), point?.exporter_id ?? null, reload);

  // Nothing covers this port (or flow export is unavailable): render nothing.
  if (!data || data.reason !== "ok" || !point) return null;

  const labels = flowDirLabels(point);
  const side = dir === "download" ? data.download : data.upload;
  const cov = dir === "download" ? data.coverage.download : data.coverage.upload;
  const noData = data.coverage_from === null;
  const lateStart = !!data.coverage_from && !!data.from && Date.parse(data.coverage_from) > Date.parse(data.from);
  const derived = point.kind === "derived";
  const flowsHref = `/traffic?${new URLSearchParams({ view: "flows", fsrc: "flows", point: String(point.id), range })}`;

  return (
    <Card className="min-w-0">
      <CardHeader className="gap-2">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="min-w-0 space-y-1.5">
            <CardTitle className="text-base">Top conversations</CardTitle>
            <div className="flex flex-wrap items-center gap-1.5">
              <SourceBadge
                kind={derived ? "derived" : "measured"}
                protocol={point.protocol}
                exporter={point.exporter_name}
                sampling={point.sampling_rate}
                note={point.note}
              />
              {data.coverage.download !== null && <CoverageBadge value={data.coverage.download} label="↓" />}
              {data.coverage.upload !== null && <CoverageBadge value={data.coverage.upload} label="↑" />}
            </div>
            <p className="text-xs text-muted-foreground">
              {range === "live" ? "Last 5 minutes" : `Last ${RANGE_LABEL[range]}`} · view “{point.name}” ·{" "}
              {data.resolution === "1h" ? "hourly rollups" : "flows lag ≈ 2 min"}.
            </p>
          </div>
          <Button
            size="sm"
            variant="outline"
            render={<Link href={flowsHref} />}
          >
            Open in Flows <ArrowRight />
          </Button>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Segmented
            ariaLabel="Flow direction"
            value={dir}
            onChange={setDir}
            options={[
              { value: "download", label: `↓ ${labels.down}` },
              { value: "upload", label: `↑ ${labels.up}` },
            ]}
          />
          {data.alternatives.length > 0 && (
            <label className="flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground">
              also measured by
              <select
                aria-label="Observation point"
                className="h-7 min-w-0 max-w-[220px] rounded-md border bg-background px-1.5 text-xs text-foreground"
                value={point.id}
                onChange={(e) => setPick(Number(e.target.value))}
              >
                {[point, ...data.alternatives].map((p) => (
                  <option key={p.id} value={p.id} title={p.name}>
                    {p.exporter_name} · {protocolLabel(p.protocol)}
                    {p.kind === "derived" ? " · derived" : ""}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {noData && <Notice>No flow data for this range yet.</Notice>}
        {lateStart && <Notice>Flow data since {fmtDateTime(data.coverage_from!)}</Notice>}
        {derived && point.note && <Notice>{point.note}</Notice>}
        {cov !== null && coverageWarn(cov) && <Notice kind="warn">{coverageHint(cov)}</Notice>}
        {!noData && (
          <FlowRowsTable
            rows={side.rows}
            other={side.other}
            group="conv"
            dir={dir}
            wide
            onOpenPort={onOpenPort}
            empty="No conversations in this direction for the range."
          />
        )}
      </CardContent>
    </Card>
  );
}
