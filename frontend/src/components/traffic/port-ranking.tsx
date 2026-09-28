"use client";

import { useCallback, useMemo, useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { fmtBps } from "@/components/graph/graph-style";
import { useAuth } from "@/context/auth";
import { api, type PortRate, type PortRole, type PortSnapshot, type TopMetric, type TopPortRow } from "@/lib/api";
import type { TrafficParams, UpdateParams } from "./hooks";
import { usePolledFetch } from "./hooks";
import { METRIC_OPTIONS, PHYSICAL_ROLES, RANGE_LABEL, fmtDateTime, refreshMs, snapshotReady } from "./lib";
import { buildLiveRows } from "./live-rank";
import { Notice } from "./notice";
import { PortTable } from "./port-table";
import { Segmented } from "./segmented";

function rerank(rows: TopPortRow[]): TopPortRow[] {
  return rows.map((r, i) => ({ ...r, rank: i + 1 }));
}

// PortRanking: every interface of one device, ranked for the selected range.
// Physical/radio ports lead; bridges, VLANs, tunnels and other logical
// interfaces are collapsed underneath (they re-count physical traffic).
export function PortRanking({
  deviceId,
  params,
  update,
  snapshot,
  devicePorts,
  roles,
  nameOf,
  onOpenPort,
}: {
  deviceId: string;
  params: TrafficParams;
  update: UpdateParams;
  snapshot: PortSnapshot | null;
  devicePorts: PortRate[];
  roles: Map<string, PortRole>;
  nameOf: (deviceId: string) => string;
  onOpenPort: (deviceId: string, iface: string) => void;
}) {
  const { token } = useAuth();
  const { range } = params;
  const live = range === "live";
  const metric: TopMetric = live ? "avg" : params.metric;

  const fetcher = useCallback(
    () => api.traffic.portsTop(token!, { range, metric, dir: "total", limit: 200, physical: false, device: deviceId }),
    [token, range, metric, deviceId],
  );
  const key = token ? `devtop|${deviceId}|${range}|${metric}` : null;
  const { data, error, loading } = usePolledFetch(key, fetcher, refreshMs(range));

  const { phys, down, virt } = useMemo(() => {
    let rows: TopPortRow[];
    if (live) {
      rows = buildLiveRows({
        ports: devicePorts,
        serverRows: data?.rows ?? [],
        roles,
        nameOf,
        metric,
        dir: "total",
        physical: false,
        includeIdle: true,
        limit: 500,
      });
    } else {
      rows = data?.rows ?? [];
    }
    // A blank-named interface row (legacy interfaces-table junk) can't be
    // opened — iface="" is "no port selected" — so it only adds a dead row.
    rows = rows.filter((r) => r.iface.trim() !== "");
    const physical = rows.filter((r) => PHYSICAL_ROLES.has(r.role));
    // Link-down ports that moved nothing in the range only bury the useful
    // rows; they stay available, collapsed, so the list is still complete.
    const idleDown = (r: TopPortRow) => !r.running && r.rx_avg + r.tx_avg + r.rx_bytes + r.tx_bytes === 0;
    return {
      phys: rerank(physical.filter((r) => !idleDown(r))),
      down: rerank(physical.filter(idleDown)),
      virt: rerank(rows.filter((r) => !PHYSICAL_ROLES.has(r.role))),
    };
  }, [live, devicePorts, data, roles, nameOf, metric]);

  const virtTotal = virt.reduce((s, r) => s + r.rx_avg + r.tx_avg, 0);
  const noHistory = !live && data && data.coverage_from === null;
  const coverageLate =
    !live && data?.coverage_from && data.from && Date.parse(data.coverage_from) > Date.parse(data.from);
  const tableLoading = live ? !snapshotReady(snapshot) && !data : loading;

  return (
    <Card>
      <CardHeader className="gap-3">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div>
            <CardTitle className="text-base">Ports ranked</CardTitle>
            <p className="text-xs text-muted-foreground">
              {live ? "By current rate." : `By ${metric === "avg" ? "average rate" : metric === "max" ? "peak rate" : "volume"} over the last ${RANGE_LABEL[range]}.`}
            </p>
          </div>
          <Segmented ariaLabel="Ranking metric" value={metric} onChange={(m) => update({ metric: m })} options={METRIC_OPTIONS(live)} />
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {error && <Notice kind="error">Could not load the port ranking: {error}</Notice>}
        {noHistory && (
          <Notice>No history for this range yet — the collector writes 1-minute buckets; check back in a few minutes.</Notice>
        )}
        {coverageLate && <Notice>Data since {fmtDateTime(data!.coverage_from!)}</Notice>}

        {phys.length === 0 && !tableLoading && (noHistory || error) ? null : phys.length === 0 && !tableLoading ? (
          <p className="py-4 text-center text-sm text-muted-foreground">
            {down.length > 0 ? "No active ports — every physical port is down." : "No physical or radio ports known for this device yet."}
          </p>
        ) : (
          <PortTable rows={phys} metric={metric} live={live} showDevice={false} loading={tableLoading} onOpenPort={onOpenPort} />
        )}

        {down.length > 0 && (
          <CollapsedRows title="Ports down" hint={`${down.length} · no link, no traffic`} rows={down} metric={metric} live={live} onOpenPort={onOpenPort} />
        )}
        {virt.length > 0 && (
          <CollapsedRows
            title="Virtual interfaces"
            hint={`${virt.length} · bridges, VLANs, tunnels`}
            total={metric === "avg" ? virtTotal : undefined}
            rows={virt}
            metric={metric}
            live={live}
            onOpenPort={onOpenPort}
          />
        )}
      </CardContent>
    </Card>
  );
}

function CollapsedRows({
  title,
  hint,
  total,
  rows,
  metric,
  live,
  onOpenPort,
}: {
  title: string;
  hint: string;
  total?: number;
  rows: TopPortRow[];
  metric: TopMetric;
  live: boolean;
  onOpenPort: (deviceId: string, iface: string) => void;
}) {
  const [open, setOpen] = useState(false);
  return (
    <div className="rounded-lg border">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        className="flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-sm hover:bg-muted/50"
      >
        <span className="flex items-center gap-2">
          {open ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
          {title}
          <span className="text-xs text-muted-foreground">({hint})</span>
        </span>
        {total !== undefined && <span className="font-mono text-xs text-muted-foreground">{fmtBps(total)}</span>}
      </button>
      {open && (
        <div className="border-t px-2 pb-2">
          <PortTable rows={rows} metric={metric} live={live} showDevice={false} onOpenPort={onOpenPort} />
        </div>
      )}
    </div>
  );
}
