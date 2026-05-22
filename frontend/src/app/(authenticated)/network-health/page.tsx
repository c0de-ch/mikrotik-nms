"use client";

import { useCallback, useEffect, useState } from "react";
import { AlertTriangle, ShieldAlert, ShieldCheck, GitBranch, Radio, Cable, ChevronDown, ChevronRight, Ban } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useAuth } from "@/context/auth";
import { api, type NetworkHealth, type LoopEvent, type BridgeWithPorts, type InterfaceState } from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";
import { foldEvents, foldOptions, type FoldBucket } from "@/lib/fold";

function formatDateTime(dateStr: string): string {
  const d = new Date(dateStr);
  const dd = String(d.getDate()).padStart(2, "0");
  const mm = String(d.getMonth() + 1).padStart(2, "0");
  const yyyy = d.getFullYear();
  const hh = String(d.getHours()).padStart(2, "0");
  const min = String(d.getMinutes()).padStart(2, "0");
  return `${dd}.${mm}.${yyyy} ${hh}:${min}`;
}

function timeAgo(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h ${mins % 60}m ago`;
  return `${Math.floor(hrs / 24)}d ago`;
}

function severityBadge(severity: string) {
  if (severity === "critical")
    return <Badge className="bg-red-100 text-red-700">critical</Badge>;
  return <Badge className="bg-amber-100 text-amber-700">warn</Badge>;
}

function eventTypeLabel(t: string): string {
  switch (t) {
    case "stp_disabled": return "STP disabled";
    case "tcn_storm": return "TCN storm";
    case "loop_detected": return "Loop detected";
    case "mac_flap": return "MAC flap";
    case "bpdu_on_edge": return "BPDU on edge";
    case "port_disabled": return "Port disabled";
    case "port_link_down": return "Link down";
    case "port_link_flap": return "Port flap";
    case "port_loop_protect": return "Loop protect tripped";
    default: return t;
  }
}

function roleColor(role: string): string {
  switch (role) {
    case "root":       return "bg-blue-100 text-blue-700";
    case "designated": return "bg-green-100 text-green-700";
    case "alternate":  return "bg-amber-100 text-amber-700";
    case "backup":     return "bg-amber-100 text-amber-700";
    case "disabled":   return "bg-muted text-muted-foreground";
    default:           return "bg-muted text-muted-foreground";
  }
}

function statusColor(status: string): string {
  switch (status) {
    case "forwarding": return "text-green-600";
    case "learning":   return "text-amber-600";
    case "discarding":
    case "blocking":   return "text-red-600";
    case "disabled":   return "text-muted-foreground";
    default:           return "";
  }
}

export default function NetworkHealthPage() {
  const { token } = useAuth();
  const [data, setData] = useState<NetworkHealth | null>(null);
  const [bucket, setBucket] = useState<FoldBucket>("off");
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});

  const load = useCallback(() => {
    if (!token) return;
    api.networkHealth.get(token).then(setData).catch(console.error);
  }, [token]);

  useEffect(() => {
    load();
    const t = setInterval(load, 30_000);
    return () => clearInterval(t);
  }, [load]);

  // Refresh on every new loop event so the page reflects state in near real-time.
  useWebSocket("network.health.event", useCallback(() => {
    load();
  }, [load]));

  // The summary topic fires on every poll cycle and just triggers a reload.
  useWebSocket("network.health", useCallback(() => {
    load();
  }, [load]));

  const bridges = data?.bridges ?? [];
  const events = data?.events ?? [];
  const portStates = data?.port_states ?? [];
  const stpOff = bridges.filter((b) => !b.stp_enabled && b.port_count > 1).length;
  const criticalCount = events.filter((e) => e.severity === "critical").length;
  const warnCount = events.filter((e) => e.severity === "warn").length;
  const downPorts = portStates.filter((p) => !p.running && !p.disabled).length;
  const flappingPorts = portStates.filter((p) => p.flap_count_window >= 3).length;
  const disabledPorts = portStates.filter((p) => p.disabled).length;
  const loopProtectPorts = portStates.filter((p) => {
    const s = (p.loop_protect_status || "").toLowerCase();
    return s !== "" && s !== "none";
  }).length;

  // Group ports by device for display.
  const portsByDevice = portStates.reduce<Record<string, InterfaceState[]>>((acc, p) => {
    const key = p.device_name || p.device_id;
    (acc[key] ??= []).push(p);
    return acc;
  }, {});

  // Group bridges by device for display.
  const grouped = bridges.reduce<Record<string, BridgeWithPorts[]>>((acc, b) => {
    const key = b.device_name || b.device_id;
    (acc[key] ??= []).push(b);
    return acc;
  }, {});

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-2xl font-bold">Network Health</h1>
        <p className="text-sm text-muted-foreground">
          Bridge / STP state and L2 loop signals across all monitored devices.
        </p>
      </div>

      <div className="grid gap-4 md:grid-cols-4 lg:grid-cols-9">
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Bridges</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            <GitBranch className="h-5 w-5 text-muted-foreground" />
            <div className="text-2xl font-bold">{bridges.length}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">STP Disabled</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            {stpOff > 0 ? <ShieldAlert className="h-5 w-5 text-amber-600" /> : <ShieldCheck className="h-5 w-5 text-green-600" />}
            <div className="text-2xl font-bold">{stpOff}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Ports tracked</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            <Cable className="h-5 w-5 text-muted-foreground" />
            <div className="text-2xl font-bold">{portStates.length}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Down</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            <Cable className={`h-5 w-5 ${downPorts > 0 ? "text-amber-600" : "text-muted-foreground"}`} />
            <div className="text-2xl font-bold">{downPorts}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Disabled</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            <Ban className={`h-5 w-5 ${disabledPorts > 0 ? "text-amber-600" : "text-muted-foreground"}`} />
            <div className="text-2xl font-bold">{disabledPorts}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Loop-protect</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            <ShieldAlert className={`h-5 w-5 ${loopProtectPorts > 0 ? "text-red-600" : "text-muted-foreground"}`} />
            <div className="text-2xl font-bold">{loopProtectPorts}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Flapping</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            <AlertTriangle className={`h-5 w-5 ${flappingPorts > 0 ? "text-red-600" : "text-muted-foreground"}`} />
            <div className="text-2xl font-bold">{flappingPorts}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Critical Events</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            <AlertTriangle className={`h-5 w-5 ${criticalCount > 0 ? "text-red-600" : "text-muted-foreground"}`} />
            <div className="text-2xl font-bold">{criticalCount}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Warnings</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            <Radio className={`h-5 w-5 ${warnCount > 0 ? "text-amber-600" : "text-muted-foreground"}`} />
            <div className="text-2xl font-bold">{warnCount}</div>
          </CardContent>
        </Card>
      </div>

      {/* Recent events */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between gap-3">
            <div>
              <CardTitle className="text-base">Recent Loop / Flap Events</CardTitle>
              <p className="text-xs text-muted-foreground">
                STP topology changes, loop detections from device logs, and MAC address flapping.
              </p>
            </div>
            <div className="flex items-center gap-2">
              <span className="text-xs text-muted-foreground">Group</span>
              <select
                className="h-8 rounded-md border bg-transparent px-2 text-sm"
                value={bucket}
                onChange={(e) => setBucket(e.target.value as FoldBucket)}
              >
                {foldOptions.map((o) => (
                  <option key={o.value} value={o.value}>{o.label}</option>
                ))}
              </select>
            </div>
          </div>
        </CardHeader>
        <CardContent>
          {events.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">
              No anomalies recorded. STP is doing its job — or you have only single-bridge devices.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Time</TableHead>
                  <TableHead>Severity</TableHead>
                  <TableHead>Type</TableHead>
                  <TableHead>Device</TableHead>
                  <TableHead>Bridge / Port</TableHead>
                  <TableHead>MAC</TableHead>
                  <TableHead>Detail</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {bucket === "off"
                  ? events.map((e: LoopEvent) => (
                      <TableRow key={e.id}>
                        <TableCell className="text-xs whitespace-nowrap" title={formatDateTime(e.recorded_at)}>{timeAgo(e.recorded_at)}</TableCell>
                        <TableCell>{severityBadge(e.severity)}</TableCell>
                        <TableCell className="text-xs">{eventTypeLabel(e.event_type)}</TableCell>
                        <TableCell className="text-xs">{e.device_name || e.device_id}</TableCell>
                        <TableCell className="text-xs font-mono">
                          {e.bridge_name && <span>{e.bridge_name}</span>}
                          {e.port_interface && <span className="ml-1 text-muted-foreground">/ {e.port_interface}</span>}
                        </TableCell>
                        <TableCell className="text-xs font-mono">{e.mac_address || "—"}</TableCell>
                        <TableCell className="text-xs text-muted-foreground max-w-[300px] truncate" title={e.message}>{e.message}</TableCell>
                      </TableRow>
                    ))
                  : foldEvents(events, bucket).flatMap((g) => {
                      const open = expanded[g.key] ?? false;
                      const critical = g.items.filter((i) => i.severity === "critical").length;
                      const warn = g.items.filter((i) => i.severity === "warn").length;
                      const header = (
                        <TableRow
                          key={`hdr-${g.key}`}
                          className="bg-muted/40 cursor-pointer hover:bg-muted"
                          onClick={() => setExpanded((s) => ({ ...s, [g.key]: !open }))}
                        >
                          <TableCell colSpan={7} className="text-xs">
                            <span className="inline-flex items-center gap-2">
                              {open ? <ChevronDown className="h-3.5 w-3.5" /> : <ChevronRight className="h-3.5 w-3.5" />}
                              <span className="font-medium">{g.key}</span>
                              <span className="text-muted-foreground">— {g.count} event{g.count === 1 ? "" : "s"}</span>
                              {critical > 0 && <Badge className="bg-red-100 text-red-700">{critical} critical</Badge>}
                              {warn > 0 && <Badge className="bg-amber-100 text-amber-700">{warn} warn</Badge>}
                            </span>
                          </TableCell>
                        </TableRow>
                      );
                      if (!open) return [header];
                      return [
                        header,
                        ...g.items.map((e) => (
                          <TableRow key={e.id}>
                            <TableCell className="text-xs whitespace-nowrap pl-8" title={formatDateTime(e.recorded_at)}>{timeAgo(e.recorded_at)}</TableCell>
                            <TableCell>{severityBadge(e.severity)}</TableCell>
                            <TableCell className="text-xs">{eventTypeLabel(e.event_type)}</TableCell>
                            <TableCell className="text-xs">{e.device_name || e.device_id}</TableCell>
                            <TableCell className="text-xs font-mono">
                              {e.bridge_name && <span>{e.bridge_name}</span>}
                              {e.port_interface && <span className="ml-1 text-muted-foreground">/ {e.port_interface}</span>}
                            </TableCell>
                            <TableCell className="text-xs font-mono">{e.mac_address || "—"}</TableCell>
                            <TableCell className="text-xs text-muted-foreground max-w-[300px] truncate" title={e.message}>{e.message}</TableCell>
                          </TableRow>
                        )),
                      ];
                    })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Per-device port state */}
      {Object.keys(portsByDevice).length > 0 && (
        <div className="space-y-4">
          <h2 className="text-lg font-semibold">Port State by Device</h2>
          {Object.entries(portsByDevice).map(([dev, ports]) => (
            <Card key={`ports-${dev}`}>
              <CardHeader className="pb-2">
                <CardTitle className="text-sm">{dev}</CardTitle>
              </CardHeader>
              <CardContent>
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Interface</TableHead>
                      <TableHead>Type</TableHead>
                      <TableHead>State</TableHead>
                      <TableHead>Reason / comment</TableHead>
                      <TableHead>Last link up</TableHead>
                      <TableHead>Last link down</TableHead>
                      <TableHead className="text-right">Recent flaps</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {ports.map((p) => {
                      const lpStatus = (p.loop_protect_status || "").toLowerCase();
                      const inLoop = lpStatus !== "" && lpStatus !== "none";
                      let stateBadge;
                      if (inLoop) stateBadge = <Badge className="bg-red-100 text-red-700">loop-protect</Badge>;
                      else if (p.disabled) stateBadge = <Badge variant="secondary">disabled</Badge>;
                      else if (!p.running) stateBadge = <Badge className="bg-red-100 text-red-700">link down</Badge>;
                      else stateBadge = <Badge className="bg-green-100 text-green-700">up</Badge>;

                      // Reason column: prefer loop-protect status > slave > comment.
                      const reasonParts: string[] = [];
                      if (inLoop) reasonParts.push(p.loop_protect_status);
                      if (p.slave) reasonParts.push("bond/bridge slave");
                      if (p.comment) reasonParts.push(p.comment);
                      const reason = reasonParts.join(" · ") || "—";

                      return (
                        <TableRow key={p.id}>
                          <TableCell className="font-mono text-xs">{p.interface_name}</TableCell>
                          <TableCell className="text-xs text-muted-foreground">{p.interface_type || "—"}</TableCell>
                          <TableCell>{stateBadge}</TableCell>
                          <TableCell className="text-xs text-muted-foreground max-w-[260px] truncate" title={reason}>{reason}</TableCell>
                          <TableCell className="text-xs text-muted-foreground">{p.last_link_up || "—"}</TableCell>
                          <TableCell className="text-xs text-muted-foreground">{p.last_link_down || "—"}</TableCell>
                          <TableCell className={`text-right text-xs ${p.flap_count_window >= 3 ? "text-red-600 font-semibold" : ""}`}>
                            {p.flap_count_window}
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      {/* Per-device bridges */}
      <div className="space-y-4">
        <h2 className="text-lg font-semibold">Bridges by Device</h2>
        {Object.keys(grouped).length === 0 && (
          <Card>
            <CardContent className="py-12 text-center text-sm text-muted-foreground">
              No bridges discovered yet. Data appears after the first network-health poll cycle (60s).
            </CardContent>
          </Card>
        )}
        {Object.entries(grouped).map(([dev, brs]) => (
          <Card key={dev}>
            <CardHeader className="pb-2">
              <CardTitle className="text-sm">{dev}</CardTitle>
            </CardHeader>
            <CardContent className="space-y-4">
              {brs.map((b) => (
                <div key={b.id} className="rounded-lg border p-3 space-y-2">
                  <div className="flex flex-wrap items-center gap-3 text-sm">
                    <span className="font-semibold">{b.bridge_name}</span>
                    <Badge variant={b.stp_enabled ? "default" : "secondary"} title={b.protocol}>
                      {b.stp_enabled ? b.protocol.toUpperCase() : "STP off"}
                    </Badge>
                    <span className="text-xs text-muted-foreground">{b.port_count} ports</span>
                    {b.topology_changes > 0 && (
                      <span className="text-xs text-muted-foreground" title="STP topology changes counter">
                        TCN: {b.topology_changes}
                        {b.last_topology_change && ` (last: ${b.last_topology_change})`}
                      </span>
                    )}
                    {b.root_bridge_id && (
                      <span className="text-xs font-mono text-muted-foreground" title="Root bridge ID">
                        root: {b.root_bridge_id}
                      </span>
                    )}
                  </div>
                  {b.ports?.length > 0 && (
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>Port</TableHead>
                          <TableHead>Role</TableHead>
                          <TableHead>Status</TableHead>
                          <TableHead>Edge</TableHead>
                          <TableHead>Path Cost</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {b.ports.map((p) => (
                          <TableRow key={p.id}>
                            <TableCell className="font-mono text-xs">{p.port_interface}</TableCell>
                            <TableCell>
                              {p.role
                                ? <Badge className={`text-[10px] font-normal ${roleColor(p.role)}`}>{p.role}</Badge>
                                : <span className="text-xs text-muted-foreground">—</span>}
                            </TableCell>
                            <TableCell className={`text-xs ${statusColor(p.status)}`}>{p.status || "—"}</TableCell>
                            <TableCell className="text-xs">{p.edge ? "yes" : "no"}</TableCell>
                            <TableCell className="text-xs">{p.path_cost || "—"}</TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  )}
                </div>
              ))}
            </CardContent>
          </Card>
        ))}
      </div>
    </div>
  );
}
