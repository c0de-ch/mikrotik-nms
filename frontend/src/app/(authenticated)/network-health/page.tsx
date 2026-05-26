"use client";

import { useCallback, useEffect, useState, type ReactNode } from "react";
import { AlertTriangle, ShieldAlert, ShieldCheck, GitBranch, Radio, Cable, ChevronDown, ChevronRight, Ban, X } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
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

type FilterKey =
  | "none"
  | "bridges"
  | "stp_disabled"
  | "ports"
  | "down"
  | "disabled"
  | "loop_protect"
  | "flapping"
  | "critical"
  | "warn";

const filterLabels: Record<Exclude<FilterKey, "none">, string> = {
  bridges: "Bridges",
  stp_disabled: "STP Disabled",
  ports: "Ports tracked",
  down: "Down ports",
  disabled: "Disabled ports",
  loop_protect: "Loop-protect ports",
  flapping: "Flapping ports",
  critical: "Critical events",
  warn: "Warnings",
};

// Shared port predicates — kept in one place so the stat counts and the
// filtered table stay in sync.
function isDownPort(p: InterfaceState): boolean {
  return !p.running && !p.disabled;
}
function isFlappingPort(p: InterfaceState): boolean {
  return p.flap_count_window >= 3;
}
function isDisabledPort(p: InterfaceState): boolean {
  return p.disabled;
}
function isLoopProtectPort(p: InterfaceState): boolean {
  const s = (p.loop_protect_status || "").toLowerCase();
  return s !== "" && s !== "none";
}

function StatCard({
  title,
  value,
  icon,
  active,
  onClick,
}: {
  title: string;
  value: number;
  icon: ReactNode;
  active: boolean;
  onClick: () => void;
}) {
  return (
    <button type="button" onClick={onClick} className="text-left" aria-pressed={active}>
      <Card
        className={`cursor-pointer transition-colors hover:border-foreground/30 ${
          active ? "border-primary ring-2 ring-primary/40" : ""
        }`}
      >
        <CardHeader className="pb-2">
          <CardTitle className="text-sm font-medium">{title}</CardTitle>
        </CardHeader>
        <CardContent className="flex items-center gap-2">
          {icon}
          <div className="text-2xl font-bold">{value}</div>
        </CardContent>
      </Card>
    </button>
  );
}

export default function NetworkHealthPage() {
  const { token } = useAuth();
  const [data, setData] = useState<NetworkHealth | null>(null);
  const [bucket, setBucket] = useState<FoldBucket>("off");
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [filter, setFilter] = useState<FilterKey>("none");
  const [showAcked, setShowAcked] = useState(false);

  const load = useCallback(() => {
    if (!token) return;
    api.networkHealth.get(token).then(setData).catch(console.error);
  }, [token]);

  const toggleFilter = useCallback((key: Exclude<FilterKey, "none">) => {
    setFilter((cur) => (cur === key ? "none" : key));
  }, []);

  const ackEvent = useCallback(
    (id: number) => {
      if (!token) return;
      api.networkHealth.ackEvent(token, id).then(load).catch(console.error);
    },
    [token, load],
  );

  const ackAll = useCallback(() => {
    if (!token) return;
    api.networkHealth.ackAll(token).then(load).catch(console.error);
  }, [token, load]);

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

  // Active-alert counts ignore acknowledged events.
  const activeEvents = events.filter((e) => !e.acknowledged);
  const stpOff = bridges.filter((b) => !b.stp_enabled && b.port_count > 1).length;
  const criticalCount = activeEvents.filter((e) => e.severity === "critical").length;
  const warnCount = activeEvents.filter((e) => e.severity === "warn").length;
  const downPorts = portStates.filter(isDownPort).length;
  const flappingPorts = portStates.filter(isFlappingPort).length;
  const disabledPorts = portStates.filter(isDisabledPort).length;
  const loopProtectPorts = portStates.filter(isLoopProtectPort).length;

  // Which top-level section a filter targets.
  const showBridgesSection = filter === "none" || filter === "bridges" || filter === "stp_disabled";
  const showPortsSection =
    filter === "none" ||
    filter === "ports" ||
    filter === "down" ||
    filter === "disabled" ||
    filter === "loop_protect" ||
    filter === "flapping";
  const showEventsSection = filter === "none" || filter === "critical" || filter === "warn";

  // Apply the active filter's predicates to the raw lists.
  const filteredBridges =
    filter === "stp_disabled"
      ? bridges.filter((b) => !b.stp_enabled && b.port_count > 1)
      : bridges;

  const filteredPorts = portStates.filter((p) => {
    switch (filter) {
      case "down": return isDownPort(p);
      case "disabled": return isDisabledPort(p);
      case "loop_protect": return isLoopProtectPort(p);
      case "flapping": return isFlappingPort(p);
      default: return true; // "none" | "ports"
    }
  });

  // Events table: severity filter + acknowledged visibility toggle.
  const tableEvents = events.filter((e) => {
    if (!showAcked && e.acknowledged) return false;
    if (filter === "critical") return e.severity === "critical";
    if (filter === "warn") return e.severity === "warn";
    return true;
  });
  const hasUnackedEvents = events.some((e) => !e.acknowledged);

  // Group ports by device for display.
  const portsByDevice = filteredPorts.reduce<Record<string, InterfaceState[]>>((acc, p) => {
    const key = p.device_name || p.device_id;
    (acc[key] ??= []).push(p);
    return acc;
  }, {});

  // Group bridges by device for display.
  const grouped = filteredBridges.reduce<Record<string, BridgeWithPorts[]>>((acc, b) => {
    const key = b.device_name || b.device_id;
    (acc[key] ??= []).push(b);
    return acc;
  }, {});

  const renderEventRow = (e: LoopEvent, nested = false) => (
    <TableRow key={e.id} className={e.acknowledged ? "opacity-50" : ""}>
      <TableCell className={`text-xs whitespace-nowrap${nested ? " pl-8" : ""}`} title={formatDateTime(e.recorded_at)}>{timeAgo(e.recorded_at)}</TableCell>
      <TableCell>{severityBadge(e.severity)}</TableCell>
      <TableCell className="text-xs">{eventTypeLabel(e.event_type)}</TableCell>
      <TableCell className="text-xs">{e.device_name || e.device_id}</TableCell>
      <TableCell className="text-xs font-mono">
        {e.bridge_name && <span>{e.bridge_name}</span>}
        {e.port_interface && <span className="ml-1 text-muted-foreground">/ {e.port_interface}</span>}
      </TableCell>
      <TableCell className="text-xs font-mono">{e.mac_address || "—"}</TableCell>
      <TableCell className="text-xs text-muted-foreground max-w-[300px] truncate" title={e.message}>{e.message}</TableCell>
      <TableCell className="text-right">
        {e.acknowledged ? (
          <Badge variant="secondary">acked</Badge>
        ) : (
          <Button variant="outline" size="xs" onClick={() => ackEvent(e.id)}>
            Ack
          </Button>
        )}
      </TableCell>
    </TableRow>
  );

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-2xl font-bold">Network Health</h1>
        <p className="text-sm text-muted-foreground">
          Bridge / STP state and L2 loop signals across all monitored devices.
        </p>
      </div>

      <div className="grid gap-4 md:grid-cols-4 lg:grid-cols-9">
        <StatCard
          title="Bridges"
          value={bridges.length}
          icon={<GitBranch className="h-5 w-5 text-muted-foreground" />}
          active={filter === "bridges"}
          onClick={() => toggleFilter("bridges")}
        />
        <StatCard
          title="STP Disabled"
          value={stpOff}
          icon={stpOff > 0 ? <ShieldAlert className="h-5 w-5 text-amber-600" /> : <ShieldCheck className="h-5 w-5 text-green-600" />}
          active={filter === "stp_disabled"}
          onClick={() => toggleFilter("stp_disabled")}
        />
        <StatCard
          title="Ports tracked"
          value={portStates.length}
          icon={<Cable className="h-5 w-5 text-muted-foreground" />}
          active={filter === "ports"}
          onClick={() => toggleFilter("ports")}
        />
        <StatCard
          title="Down"
          value={downPorts}
          icon={<Cable className={`h-5 w-5 ${downPorts > 0 ? "text-amber-600" : "text-muted-foreground"}`} />}
          active={filter === "down"}
          onClick={() => toggleFilter("down")}
        />
        <StatCard
          title="Disabled"
          value={disabledPorts}
          icon={<Ban className={`h-5 w-5 ${disabledPorts > 0 ? "text-amber-600" : "text-muted-foreground"}`} />}
          active={filter === "disabled"}
          onClick={() => toggleFilter("disabled")}
        />
        <StatCard
          title="Loop-protect"
          value={loopProtectPorts}
          icon={<ShieldAlert className={`h-5 w-5 ${loopProtectPorts > 0 ? "text-red-600" : "text-muted-foreground"}`} />}
          active={filter === "loop_protect"}
          onClick={() => toggleFilter("loop_protect")}
        />
        <StatCard
          title="Flapping"
          value={flappingPorts}
          icon={<AlertTriangle className={`h-5 w-5 ${flappingPorts > 0 ? "text-red-600" : "text-muted-foreground"}`} />}
          active={filter === "flapping"}
          onClick={() => toggleFilter("flapping")}
        />
        <StatCard
          title="Critical Events"
          value={criticalCount}
          icon={<AlertTriangle className={`h-5 w-5 ${criticalCount > 0 ? "text-red-600" : "text-muted-foreground"}`} />}
          active={filter === "critical"}
          onClick={() => toggleFilter("critical")}
        />
        <StatCard
          title="Warnings"
          value={warnCount}
          icon={<Radio className={`h-5 w-5 ${warnCount > 0 ? "text-amber-600" : "text-muted-foreground"}`} />}
          active={filter === "warn"}
          onClick={() => toggleFilter("warn")}
        />
      </div>

      {filter !== "none" && (
        <div className="flex items-center gap-2">
          <Badge variant="secondary" className="gap-1">
            Filtered: {filterLabels[filter]}
          </Badge>
          <Button variant="ghost" size="sm" onClick={() => setFilter("none")}>
            <X className="h-3.5 w-3.5" /> Clear
          </Button>
        </div>
      )}

      {/* Recent events */}
      {showEventsSection && (
      <Card>
        <CardHeader>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <CardTitle className="text-base">Recent Loop / Flap Events</CardTitle>
              <p className="text-xs text-muted-foreground">
                STP topology changes, loop detections from device logs, and MAC address flapping.
              </p>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
                <input
                  type="checkbox"
                  checked={showAcked}
                  onChange={(e) => setShowAcked(e.target.checked)}
                />
                Show acknowledged
              </label>
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
              <Button variant="outline" size="sm" onClick={ackAll} disabled={!hasUnackedEvents}>
                Ack all
              </Button>
            </div>
          </div>
        </CardHeader>
        <CardContent>
          {tableEvents.length === 0 ? (
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
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {bucket === "off"
                  ? tableEvents.map((e: LoopEvent) => renderEventRow(e))
                  : foldEvents(tableEvents, bucket).flatMap((g) => {
                      const open = expanded[g.key] ?? false;
                      const critical = g.items.filter((i) => i.severity === "critical").length;
                      const warn = g.items.filter((i) => i.severity === "warn").length;
                      const header = (
                        <TableRow
                          key={`hdr-${g.key}`}
                          className="bg-muted/40 cursor-pointer hover:bg-muted"
                          onClick={() => setExpanded((s) => ({ ...s, [g.key]: !open }))}
                        >
                          <TableCell colSpan={8} className="text-xs">
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
                      return [header, ...g.items.map((e) => renderEventRow(e, true))];
                    })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
      )}

      {/* Per-device port state */}
      {showPortsSection && Object.keys(portsByDevice).length > 0 && (
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
      {showBridgesSection && (
      <div className="space-y-4">
        <h2 className="text-lg font-semibold">Bridges by Device</h2>
        {Object.keys(grouped).length === 0 && (
          <Card>
            <CardContent className="py-12 text-center text-sm text-muted-foreground">
              {filter === "stp_disabled"
                ? "No bridges with STP disabled — all multi-port bridges have spanning tree enabled."
                : "No bridges discovered yet. Data appears after the first network-health poll cycle (60s)."}
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
      )}
    </div>
  );
}
