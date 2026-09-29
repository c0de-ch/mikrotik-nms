"use client";

import { useCallback, useMemo } from "react";
import { useAuth } from "@/context/auth";
import type { Device, FlowSource } from "@/lib/api";
import { FlowSankey } from "../flow-sankey";
import type { TrafficParams, UpdateParams } from "../hooks";
import { Notice } from "../notice";
import { Segmented } from "../segmented";
import { ExporterStatusCard } from "./exporter-status";
import { defaultPoint, hasFreshPoint } from "./flow-lib";
import { MeasuredFlows } from "./measured-flows";
import { PointPicker } from "./point-picker";
import { SetupHints } from "./setup-hints";
import { useFlowPoints, useFlowStatus } from "./use-flows";

// FlowsTab: the /traffic "Flows" view. Source = measured flow export (one
// observation point at a time) or the Phase 1 estimate from port counters,
// which is rendered unchanged. Unset (?fsrc absent) = measured when a view
// has data from the last hour and the collector is on, else estimated.
export function FlowsTab({
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
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  const st = useFlowStatus();
  const pts = useFlowPoints();
  const reloadStatus = st.reload;
  const reloadPoints = pts.reload;
  const onChanged = useCallback(() => {
    reloadStatus();
    reloadPoints();
  }, [reloadStatus, reloadPoints]);

  // Auto source: wait for both answers; a failed request (older backend,
  // flows unavailable) falls back to the estimate.
  let auto: FlowSource | null = null;
  if (st.loaded && pts.loaded) {
    auto = st.status?.enabled && pts.points && hasFreshPoint(pts.points) ? "flows" : "counters";
  }
  const src = params.fsrc ?? auto;
  const measured = src === "flows";

  const points = pts.points;
  const byId = useMemo(() => new Map((points ?? []).map((p) => [p.id, p])), [points]);
  const requested = params.point !== null ? byId.get(params.point) ?? null : null;
  const missing = params.point !== null && points !== null && !requested;
  const point = requested ?? (points ? defaultPoint(points) : null);
  const noPoints = points !== null && points.length === 0;
  // Tri-state: null while /flows/status is loading or failed — not "off".
  const collectorOn: boolean | null = st.status ? st.status.enabled : null;
  // Collector off and nothing ever configured or heard (every default
  // install): the exporter and setup cards have nothing to say under the
  // estimate — the Settings card and the Measured side explain how to enable it.
  const idleOff =
    st.status?.enabled === false &&
    (st.status.exporters?.length ?? 0) === 0 &&
    (st.status.unknown_senders?.length ?? 0) === 0;
  const flowCards = measured || !idleOff;

  // "NAT for <exporter>": jump to that exporter's inside-facing view, where
  // its hosts are visible one by one.
  const onNat = useCallback(
    (exporterName: string) => {
      const cands = (points ?? []).filter((p) => p.exporter_name === exporterName && p.enabled && p.facing === "down");
      const target = cands.find((p) => p.kind === "native") ?? cands[0];
      if (target) update({ point: target.id }, { push: true });
    },
    [points, update],
  );

  return (
    <div className="min-w-0 space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <Segmented
          ariaLabel="Flow data source"
          value={src ?? "counters"}
          onChange={(v) => update({ fsrc: v })}
          options={[
            {
              value: "flows",
              label: (
                <>
                  Measured<span className="hidden sm:inline"> (flow export)</span>
                </>
              ),
              title: "Per-conversation traffic exported by the devices (NetFlow / IPFIX)",
            },
            {
              value: "counters",
              label: (
                <>
                  Estimated<span className="hidden sm:inline"> (port counters)</span>
                </>
              ),
              title: "Traffic tree estimated from interface byte counters and the inferred topology",
            },
          ]}
        />
        {measured && points && points.length > 0 && (
          <>
            <PointPicker points={points} value={point?.id ?? null} onChange={(id) => update({ point: id }, { push: true })} />
            <Segmented
              ariaLabel="Flow direction"
              value={params.flow}
              onChange={(f) => update({ flow: f })}
              options={[
                { value: "download", label: "↓ Download" },
                { value: "upload", label: "↑ Upload" },
              ]}
            />
            <Segmented
              ariaLabel="Group the remote side by"
              value={params.remote}
              onChange={(r) => update({ remote: r })}
              options={[
                { value: "host", label: "Hosts", title: "Remote side of the graph by endpoint" },
                { value: "app", label: "Apps", title: "Remote side of the graph by service (protocol / port)" },
              ]}
            />
          </>
        )}
      </div>

      {src === null ? (
        <div className="h-[320px] animate-pulse rounded-lg bg-muted" />
      ) : !measured ? (
        <FlowSankey params={params} update={update} devices={devices} onOpenPort={onOpenPort} onOpenDevice={onOpenDevice} />
      ) : pts.error && !points ? (
        <Notice kind="error">Could not load flow views: {pts.error}</Notice>
      ) : !points ? (
        <div className="h-[320px] animate-pulse rounded-lg bg-muted" />
      ) : noPoints || !point ? (
        <SetupHints
          status={st.status}
          statusError={st.error}
          points={points}
          isAdmin={isAdmin}
          devices={devices}
          inline
          onChanged={onChanged}
        />
      ) : (
        <>
          {missing && <Notice kind="warn">View #{params.point} no longer exists — showing “{point.name}”.</Notice>}
          <MeasuredFlows
            key={point.id}
            point={point}
            range={params.range}
            dir={params.flow}
            remote={params.remote}
            collectorOn={collectorOn}
            onOpenPort={onOpenPort}
            onNat={onNat}
          />
        </>
      )}

      {flowCards && <ExporterStatusCard status={st.status} isAdmin={isAdmin} devices={devices} onChanged={onChanged} />}
      {flowCards && !(measured && (noPoints || (points && !point))) && (
        <SetupHints
          status={st.status}
          statusError={st.error}
          points={points}
          isAdmin={isAdmin}
          devices={devices}
          onChanged={onChanged}
        />
      )}
    </div>
  );
}
