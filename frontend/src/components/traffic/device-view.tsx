"use client";

import { useMemo } from "react";
import { Server } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { fmtBps } from "@/components/graph/graph-style";
import type { Device, PortRate, PortRole, PortSnapshot } from "@/lib/api";
import { deviceStatusBadgeClass, deviceStatusLabel } from "@/lib/status";
import type { TrafficParams, UpdateParams } from "./hooks";
import { collectingText, comparePortNames, deviceLoads, isPhysicalPort, portKey, roleFor, snapshotReady } from "./lib";
import { EmptyState, Notice } from "./notice";
import { HeatmapLegend, PortHeatmap, type HeatmapPort } from "./port-heatmap";
import { PortRanking } from "./port-ranking";

// DeviceView: one device's live port heatmap (from the traffic.ports
// snapshot — no per-port streams) plus its ranked ports for the range.
export function DeviceView({
  deviceId,
  device,
  devicesLoaded,
  devicesError,
  knownToServer,
  params,
  update,
  snapshot,
  devicePorts,
  roles,
  nameOf,
  dark,
  onOpenPort,
}: {
  deviceId: string;
  device: Device | undefined;
  devicesLoaded: boolean;
  devicesError: string | null;
  // The role graph knows this id (a device added after the list loaded, or
  // one the list failed to fetch).
  knownToServer: boolean;
  params: TrafficParams;
  update: UpdateParams;
  snapshot: PortSnapshot | null;
  devicePorts: PortRate[];
  roles: Map<string, PortRole>;
  nameOf: (deviceId: string) => string;
  dark: boolean;
  onOpenPort: (deviceId: string, iface: string) => void;
}) {
  const heat = useMemo<HeatmapPort[]>(
    () =>
      devicePorts
        .filter((p) => isPhysicalPort(p.iface, p.type))
        .sort((a, b) => comparePortNames(a.iface, b.iface))
        .map((p) => {
          const r = roles.get(portKey(p.device_id, p.iface));
          return {
            name: p.iface,
            type: p.type,
            running: p.running,
            disabled: p.disabled,
            comment: p.comment,
            rx_bps: p.rx_bps,
            tx_bps: p.tx_bps,
            role: roleFor(roles, p.device_id, p.iface, p.type, p.role),
            behind: r?.behind,
          };
        }),
    [devicePorts, roles],
  );
  const load = useMemo(() => deviceLoads(devicePorts).get(deviceId), [devicePorts, deviceId]);
  const ready = snapshotReady(snapshot);

  // Only claim the device is gone when the list loaded fine and neither the
  // live snapshot nor the role graph has heard of it either.
  if (devicesLoaded && !devicesError && !device && devicePorts.length === 0 && !knownToServer) {
    return (
      <Card>
        <CardContent>
          <EmptyState icon={Server}>This device no longer exists.</EmptyState>
        </CardContent>
      </Card>
    );
  }

  const offline = ready && devicePorts.length === 0;

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader className="gap-3">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
              <CardTitle className="flex flex-wrap items-center gap-2 text-base">
                <span className="truncate">{nameOf(deviceId)}</span>
                {device && (
                  <Badge variant="outline" className={deviceStatusBadgeClass(device.status)}>
                    {deviceStatusLabel(device.status)}
                  </Badge>
                )}
              </CardTitle>
              {device && (
                <p className="text-xs text-muted-foreground">
                  {device.address}
                  {device.board ? ` · ${device.board}` : ""}
                  {device.ros_version ? ` · RouterOS ${device.ros_version}` : ""}
                </p>
              )}
            </div>
            {load && ready && (
              <div className="text-right">
                <div className="text-lg font-semibold tabular-nums">{fmtBps(load.throughput)}</div>
                <div className="text-xs text-muted-foreground">
                  switched now · {load.activePorts}/{heat.length} ports active
                </div>
              </div>
            )}
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          {!ready && <Notice kind="loading">{collectingText(snapshot)}</Notice>}
          {offline && <Notice kind="warn">Device offline — live counters unavailable. History below still works.</Notice>}
          {heat.length > 0 && (
            <>
              <PortHeatmap ports={heat} dark={dark} onSelect={(iface) => onOpenPort(deviceId, iface)} />
              <HeatmapLegend dark={dark} />
            </>
          )}
          {ready && !offline && heat.length === 0 && (
            <p className="text-xs text-muted-foreground">No physical ports reported — see the ranking below for radio and virtual interfaces.</p>
          )}
        </CardContent>
      </Card>

      <PortRanking
        deviceId={deviceId}
        params={params}
        update={update}
        snapshot={snapshot}
        devicePorts={devicePorts}
        roles={roles}
        nameOf={nameOf}
        onOpenPort={onOpenPort}
      />
    </div>
  );
}
