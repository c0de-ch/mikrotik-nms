"use client";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { fmtBps } from "@/components/graph/graph-style";
import type { Device, PortRate, PortRole, PortSnapshot, TrafficRange } from "@/lib/api";
import { BehindPort } from "./behind-port";
import { RX_COLOR, TX_COLOR, collectingText, dirLabels, portKey, roleFor, roleHint, snapshotReady, splitDirection } from "./lib";
import { Notice } from "./notice";
import { PathStrip } from "./path-strip";
import { PortHistoryCard } from "./port-history";
import { RoleBadge } from "./role-badge";

// PortDetail: one port — live rate, the Internet → port path, history with
// stats for the range, and what is behind it.
export function PortDetail({
  deviceId,
  iface,
  device,
  range,
  snapshot,
  byKey,
  deviceHasLivePorts,
  roles,
  nameOf,
  onOpenPort,
  onOpenDevice,
}: {
  deviceId: string;
  iface: string;
  device: Device | undefined;
  range: TrafficRange;
  snapshot: PortSnapshot | null;
  byKey: Map<string, PortRate>;
  deviceHasLivePorts: boolean;
  roles: Map<string, PortRole>;
  nameOf: (deviceId: string) => string;
  onOpenPort: (deviceId: string, iface: string) => void;
  onOpenDevice: (deviceId: string) => void;
}) {
  const rate = byKey.get(portKey(deviceId, iface));
  const role = roleFor(roles, deviceId, iface, rate?.type, rate?.role);
  const roleInfo = roles.get(portKey(deviceId, iface));
  const ready = snapshotReady(snapshot);
  const offline = ready && !deviceHasLivePorts;
  const labels = dirLabels(role);
  const now = rate && ready ? splitDirection(role, rate.rx_bps, rate.tx_bps) : null;
  const pps = rate && ready ? splitDirection(role, rate.rx_pps, rate.tx_pps) : null;

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader className="gap-3">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0 space-y-1">
              <CardTitle className="flex flex-wrap items-center gap-2 text-base">
                <span className="font-mono">{iface}</span>
                <RoleBadge role={role} />
                {rate?.disabled ? (
                  <span className="text-xs font-normal text-muted-foreground">disabled</span>
                ) : rate && !rate.running ? (
                  <span className="text-xs font-normal text-muted-foreground">link down</span>
                ) : null}
              </CardTitle>
              <p className="text-xs text-muted-foreground">
                <button type="button" className="font-medium text-foreground hover:underline" onClick={() => onOpenDevice(deviceId)}>
                  {nameOf(deviceId)}
                </button>
                {device?.address ? ` · ${device.address}` : ""}
                {rate?.type ? ` · ${rate.type}` : ""}
                {rate?.comment ? ` · ${rate.comment}` : ""}
              </p>
              <p className="text-xs text-muted-foreground">{roleHint(role, roleInfo?.role === role ? roleInfo : undefined)}.</p>
            </div>
            {now && pps && (
              <div className="grid grid-cols-[auto_auto] gap-x-3 text-right text-sm" aria-label="Current rate">
                <span className="flex items-center justify-end gap-1.5 text-xs text-muted-foreground">
                  <span className="h-2 w-2 rounded-sm" style={{ background: RX_COLOR }} />
                  {labels.down}
                </span>
                <span className="font-mono font-semibold tabular-nums">
                  {fmtBps(now.down)} <span className="text-xs font-normal text-muted-foreground">{pps.down} pps</span>
                </span>
                <span className="flex items-center justify-end gap-1.5 text-xs text-muted-foreground">
                  <span className="h-2 w-2 rounded-sm" style={{ background: TX_COLOR }} />
                  {labels.up}
                </span>
                <span className="font-mono font-semibold tabular-nums">
                  {fmtBps(now.up)} <span className="text-xs font-normal text-muted-foreground">{pps.up} pps</span>
                </span>
              </div>
            )}
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          {!ready && <Notice kind="loading">{collectingText(snapshot)}</Notice>}
          {offline && <Notice kind="warn">Device offline — live counters unavailable. History below still works.</Notice>}
          {ready && !offline && !rate && (
            <Notice>This interface is not in the live snapshot (removed, or not reported by the device).</Notice>
          )}
          <div>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">Path</h3>
            <PathStrip deviceId={deviceId} iface={iface} byKey={byKey} roles={roles} live={ready} onOpenPort={onOpenPort} />
          </div>
        </CardContent>
      </Card>

      <PortHistoryCard deviceId={deviceId} iface={iface} range={range} role={role} />

      <BehindPort deviceId={deviceId} iface={iface} roleInfo={roleInfo} onOpenDevice={onOpenDevice} onOpenPort={onOpenPort} />
    </div>
  );
}
