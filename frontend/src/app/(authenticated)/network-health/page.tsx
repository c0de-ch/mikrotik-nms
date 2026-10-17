"use client";

import { Fragment, useCallback, useEffect, useState, type ReactNode } from "react";
import { AlertTriangle, ShieldAlert, ShieldCheck, GitBranch, Radio, Cable, ChevronDown, ChevronRight, Ban, X, Lightbulb } from "lucide-react";
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
import {
  api,
  type BridgePortStatus,
  type BridgeWithPorts,
  type Device,
  type InterfaceState,
  type LoopEvent,
  type NetworkHealth,
  type StpSource,
} from "@/lib/api";
import { DeviceLoadBadges } from "@/components/devices/device-load";
import { useWebSocket } from "@/hooks/use-websocket";
import { foldEvents, foldOptions, type FoldBucket } from "@/lib/fold";
import { explainEvent, explainPortState, type HealthHint } from "@/lib/health-hints";

// Expanded "what is this & how do I fix it" panel under a clicked row.
// extra: event-specific evidence (the ports behind a TCN storm). Table cells
// do not wrap (whitespace-nowrap), so the panel wraps on its own and stays
// within the visible width even when the table beside it scrolls.
function HintPanel({ hint, extra }: { hint: HealthHint; extra?: ReactNode }) {
  return (
    <div className="max-w-[min(48rem,calc(100vw-4.5rem))] space-y-2 whitespace-normal py-1 text-xs">
      <p className="flex items-start gap-1.5">
        <Lightbulb className="h-3.5 w-3.5 mt-0.5 shrink-0 text-amber-500" />
        <span>{hint.meaning}</span>
      </p>
      {extra && <div className="pl-5">{extra}</div>}
      <div className="grid gap-3 md:grid-cols-2 pl-5">
        <div>
          <p className="mb-1 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">Likely causes</p>
          <ul className="list-disc space-y-0.5 pl-4 text-muted-foreground">
            {hint.causes.map((c, i) => <li key={i}>{c}</li>)}
          </ul>
        </div>
        <div>
          <p className="mb-1 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">How to fix</p>
          <ul className="list-disc space-y-0.5 pl-4 text-muted-foreground">
            {hint.fixes.map((f, i) => <li key={i}>{f}</li>)}
          </ul>
        </div>
      </div>
    </div>
  );
}

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
    return <Badge className="bg-red-100 text-red-700 dark:bg-red-500/15 dark:text-red-400">critical</Badge>;
  return <Badge className="bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300">warn</Badge>;
}

// How a bridge's port STP state was read (bridge_status.stp_source): a quiet
// provenance hint, since the cheaper the read the more often it can run.
const STP_SOURCE: Record<StpSource, { label: string; title: string }> = {
  print: {
    label: "STP via port print",
    title: "RouterOS 7.24+ reports STP state in the bridge port print — the cheapest read: one command for every port.",
  },
  "monitor-batched": {
    label: "STP via batched monitor",
    title: "Read with one bridge-port monitor over all active ports (RouterOS before 7.24).",
  },
  "monitor-per-port": {
    label: "STP via per-port monitor",
    title: "Read with one monitor per active port — the costliest fallback, used when this RouterOS accepts no batched monitor.",
  },
  none: {
    label: "No STP state",
    title: "The device reported no STP runtime state: no active ports, or none of the read forms worked.",
  },
};

// A port's STP state: the status the device reported, else what the runtime
// flags say. "" when nothing is known (an older read, or STP off).
function stpState(p: BridgePortStatus): string {
  if (p.inactive || p.port_state === "inactive" || p.status === "inactive") return "inactive";
  if (p.status) return p.status;
  if (p.forwarding) return "forwarding";
  if (p.learning) return "learning";
  if (p.forwarding === false && p.learning === false) return "discarding";
  return "";
}

// Natural port order: ether2 before ether10.
function byPortName(a: string, b: string): number {
  return a.localeCompare(b, undefined, { numeric: true, sensitivity: "base" });
}

// Rows grouped by device id, in the API's order, with the device's name.
function groupByDevice<T extends { device_id: string; device_name: string }>(
  rows: T[],
): { id: string; name: string; rows: T[] }[] {
  const out = new Map<string, { id: string; name: string; rows: T[] }>();
  for (const r of rows) {
    let g = out.get(r.device_id);
    if (!g) {
      g = { id: r.device_id, name: r.device_name || r.device_id, rows: [] };
      out.set(r.device_id, g);
    }
    g.rows.push(r);
  }
  return [...out.values()];
}

// The ports of a bridge with the most topology changes, most first.
function topChangedPorts(b: BridgeWithPorts | undefined, n: number): BridgePortStatus[] {
  return (b?.ports ?? [])
    .filter((p) => (p.topology_changes ?? 0) > 0)
    .sort((x, y) => (y.topology_changes ?? 0) - (x.topology_changes ?? 0))
    .slice(0, n);
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
    case "ip_address_changed": return "IP address changed";
    default: return t;
  }
}

function roleColor(role: string): string {
  switch (role) {
    case "root":       return "bg-blue-100 text-blue-700 dark:bg-blue-500/15 dark:text-blue-300";
    case "designated": return "bg-green-100 text-green-700 dark:bg-green-500/15 dark:text-green-400";
    case "alternate":  return "bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300";
    case "backup":     return "bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300";
    case "disabled":   return "bg-muted text-muted-foreground";
    default:           return "bg-muted text-muted-foreground";
  }
}

function statusColor(status: string): string {
  switch (status) {
    case "forwarding": return "text-green-600 dark:text-green-400";
    case "learning":   return "text-amber-600 dark:text-amber-300";
    case "discarding":
    case "blocking":   return "text-red-600 dark:text-red-400";
    case "disabled":
    case "inactive":   return "text-muted-foreground";
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
  // Only "on" means loop-protect is actually enabled on the port. RouterOS
  // reports "off" (or empty) when it isn't in use — those are NOT alerts.
  // An actual loop-protect *trip* surfaces as a port_loop_protect event.
  const s = (p.loop_protect_status || "").toLowerCase();
  return s === "on";
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
  const [hintOpen, setHintOpen] = useState<Record<number, boolean>>({});
  const [portHintOpen, setPortHintOpen] = useState<Record<string, boolean>>({});
  // The device list, for the class and pause marks next to each device name
  // (a paused device's rows hold their last reading).
  const [devices, setDevices] = useState<Map<string, Device>>(new Map());

  const load = useCallback(() => {
    if (!token) return;
    api.networkHealth.get(token).then(setData).catch(console.error);
  }, [token]);

  useEffect(() => {
    if (!token) return;
    let alive = true;
    const loadDevices = () =>
      api.devices
        .list(token)
        .then((list) => {
          if (alive) setDevices(new Map(list.map((d) => [d.id, d])));
        })
        .catch(console.error);
    loadDevices();
    const t = setInterval(loadDevices, 60_000);
    return () => {
      alive = false;
      clearInterval(t);
    };
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

  // Group ports and bridges by device (id: two devices may share a name),
  // in the order the API lists them.
  const portsByDevice = groupByDevice(filteredPorts).map((g) => ({
    ...g,
    rows: [...g.rows].sort((a, b) => byPortName(a.interface_name, b.interface_name)),
  }));
  const grouped = groupByDevice(filteredBridges);
  const bridgeOf = (deviceId: string, bridge: string) =>
    bridges.find((b) => b.device_id === deviceId && b.bridge_name === bridge);

  // The device's name with its class and pause marks, as every device card
  // title on the page shows it.
  const deviceTitle = (id: string, name: string) => {
    const dev = devices.get(id);
    return (
      <CardTitle className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm">
        <span>{name}</span>
        {dev && <DeviceLoadBadges device={dev} />}
      </CardTitle>
    );
  };

  // A TCN storm's evidence: the bridge's ports whose topology-change
  // counters are highest — usually the flapping link.
  const tcnEvidence = (e: LoopEvent) => {
    if (e.event_type !== "tcn_storm") return null;
    const top = topChangedPorts(bridgeOf(e.device_id, e.bridge_name), 3);
    if (top.length === 0) return null;
    return (
      <p className="text-muted-foreground">
        Most topology changes on {e.bridge_name || "this bridge"}:{" "}
        {top.map((p, i) => (
          <span key={p.id}>
            {i > 0 && ", "}
            <span className="font-mono text-foreground">{p.port_interface}</span>{" "}
            <span className="tabular-nums">{p.topology_changes}</span>
          </span>
        ))}
        . Start with the first port in Bridges below.
      </p>
    );
  };

  const renderEventRow = (e: LoopEvent, nested = false) => {
    const open = hintOpen[e.id] ?? false;
    return (
      <Fragment key={e.id}>
        <TableRow
          className={`cursor-pointer ${e.acknowledged ? "opacity-50" : ""}`}
          onClick={() => setHintOpen((s) => ({ ...s, [e.id]: !open }))}
          title="Click for cause & fix"
        >
          <TableCell className={`text-xs whitespace-nowrap${nested ? " pl-8" : ""}`} title={formatDateTime(e.recorded_at)}>
            <span className="inline-flex items-center gap-1">
              {open ? <ChevronDown className="h-3 w-3 text-muted-foreground" /> : <ChevronRight className="h-3 w-3 text-muted-foreground" />}
              {timeAgo(e.recorded_at)}
            </span>
          </TableCell>
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
              <Button variant="outline" size="xs" onClick={(ev) => { ev.stopPropagation(); ackEvent(e.id); }}>
                Ack
              </Button>
            )}
          </TableCell>
        </TableRow>
        {open && (
          <TableRow className="bg-muted/30 hover:bg-muted/30">
            <TableCell colSpan={8} className={nested ? "pl-8" : ""}>
              <HintPanel hint={explainEvent(e)} extra={tcnEvidence(e)} />
            </TableCell>
          </TableRow>
        )}
      </Fragment>
    );
  };

  return (
    <div className="min-w-0 space-y-4 [contain:inline-size]">
      <div>
        <h1 className="text-2xl font-bold">Network Health</h1>
        <p className="text-sm text-muted-foreground">
          Bridge / STP state and L2 loop signals across all monitored devices.
        </p>
      </div>

      <div className="grid grid-cols-2 gap-3 sm:gap-4 md:grid-cols-4 lg:grid-cols-9">
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
          icon={<ShieldAlert className={`h-5 w-5 ${loopProtectPorts > 0 ? "text-blue-600" : "text-muted-foreground"}`} />}
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
                Click a row for the likely cause and how to fix it.
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
      {showPortsSection && portsByDevice.length > 0 && (
        <div className="space-y-4">
          <h2 className="text-lg font-semibold">Port State by Device</h2>
          {portsByDevice.map(({ id: devId, name: dev, rows: ports }) => (
            <Card key={`ports-${devId}`}>
              <CardHeader className="pb-2">{deviceTitle(devId, dev)}</CardHeader>
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
                      <TableHead className="text-right" title="Link-downs within the flap window (Settings → Port Monitoring); 3 or more is a flapping port">
                        Recent link-downs
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {ports.map((p) => {
                      const lpOn = (p.loop_protect_status || "").toLowerCase() === "on";
                      let stateBadge;
                      if (p.disabled) stateBadge = <Badge variant="secondary">disabled</Badge>;
                      else if (!p.running) stateBadge = <Badge className="bg-red-100 text-red-700 dark:bg-red-500/15 dark:text-red-400">link down</Badge>;
                      else if (lpOn) stateBadge = <Badge className="bg-blue-100 text-blue-700 dark:bg-blue-500/15 dark:text-blue-300">loop-protect on</Badge>;
                      else stateBadge = <Badge className="bg-green-100 text-green-700 dark:bg-green-500/15 dark:text-green-400">up</Badge>;

                      // Reason column: loop-protect (enabled) > slave > comment.
                      const reasonParts: string[] = [];
                      if (lpOn) reasonParts.push("loop-protect enabled");
                      if (p.slave) reasonParts.push("bond/bridge slave");
                      if (p.comment) reasonParts.push(p.comment);
                      const reason = reasonParts.join(" · ") || "—";

                      const hint = explainPortState(p);
                      const pOpen = portHintOpen[p.id] ?? false;

                      return (
                        <Fragment key={p.id}>
                          <TableRow
                            className={hint ? "cursor-pointer" : ""}
                            onClick={hint ? () => setPortHintOpen((s) => ({ ...s, [p.id]: !pOpen })) : undefined}
                            title={hint ? "Click for cause & fix" : undefined}
                          >
                            <TableCell className="font-mono text-xs">
                              <span className="inline-flex items-center gap-1">
                                {hint && (pOpen ? <ChevronDown className="h-3 w-3 text-muted-foreground" /> : <ChevronRight className="h-3 w-3 text-muted-foreground" />)}
                                {p.interface_name}
                              </span>
                            </TableCell>
                            <TableCell className="text-xs text-muted-foreground">{p.interface_type || "—"}</TableCell>
                            <TableCell>{stateBadge}</TableCell>
                            <TableCell className="text-xs text-muted-foreground max-w-[260px] truncate" title={reason}>{reason}</TableCell>
                            <TableCell className="text-xs text-muted-foreground">{p.last_link_up || "—"}</TableCell>
                            <TableCell className="text-xs text-muted-foreground">{p.last_link_down || "—"}</TableCell>
                            <TableCell className={`text-right text-xs ${p.flap_count_window >= 3 ? "text-red-600 font-semibold" : ""}`}>
                              {p.flap_count_window}
                            </TableCell>
                          </TableRow>
                          {hint && pOpen && (
                            <TableRow className="bg-muted/30 hover:bg-muted/30">
                              <TableCell colSpan={7}>
                                <HintPanel hint={hint} />
                              </TableCell>
                            </TableRow>
                          )}
                        </Fragment>
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
        {grouped.length === 0 && (
          <Card>
            <CardContent className="py-12 text-center text-sm text-muted-foreground">
              {filter === "stp_disabled"
                ? "No bridges with STP disabled — all multi-port bridges have spanning tree enabled."
                : "No bridges discovered yet. They appear after each device's first bridge read — every 2 min on capable and small devices, every 5 min on weak ones."}
            </CardContent>
          </Card>
        )}
        {grouped.map(({ id: devId, name: dev, rows: brs }) => (
          <Card key={devId}>
            <CardHeader className="pb-2">{deviceTitle(devId, dev)}</CardHeader>
            <CardContent className="space-y-4">
              {brs.map((b) => {
                // "No STP state" says nothing new next to "STP off".
                const source =
                  b.stp_source && !(b.stp_source === "none" && !b.stp_enabled) ? STP_SOURCE[b.stp_source as StpSource] : undefined;
                const maxChanges = Math.max(0, ...(b.ports ?? []).map((p) => p.topology_changes ?? 0));
                const hasChanges = (b.ports ?? []).some((p) => p.topology_changes != null);
                return (
                  <div key={b.id} className="rounded-lg border p-3 space-y-2">
                    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm">
                      <span className="font-semibold">{b.bridge_name}</span>
                      <Badge variant={b.stp_enabled ? "default" : "secondary"} title={b.protocol}>
                        {b.stp_enabled ? b.protocol.toUpperCase() : "STP off"}
                      </Badge>
                      <span className="text-xs text-muted-foreground">{b.port_count} ports</span>
                      {b.topology_changes > 0 && (
                        <span className="text-xs text-muted-foreground" title="Topology changes, summed over the bridge's ports">
                          {b.topology_changes} topology changes
                          {b.last_topology_change && ` (last ${b.last_topology_change})`}
                        </span>
                      )}
                      {b.root_bridge_id && (
                        <span className="text-xs font-mono text-muted-foreground" title="Root bridge ID">
                          root: {b.root_bridge_id}
                        </span>
                      )}
                      {source && (
                        <span className="text-[11px] text-muted-foreground sm:ml-auto" title={source.title}>
                          {source.label}
                        </span>
                      )}
                    </div>
                    {b.ports?.length > 0 && (
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead>Port</TableHead>
                            <TableHead>Role</TableHead>
                            <TableHead>State</TableHead>
                            <TableHead>Edge</TableHead>
                            <TableHead>Path Cost</TableHead>
                            {hasChanges && (
                              <TableHead className="text-right" title="The port's topology-change counter: a fast-growing one points at a flapping link">
                                Topology changes
                              </TableHead>
                            )}
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {[...b.ports].sort((x, y) => byPortName(x.port_interface, y.port_interface)).map((p) => {
                            const state = stpState(p);
                            const changes = p.topology_changes;
                            return (
                              <TableRow key={p.id}>
                                <TableCell className="font-mono text-xs">
                                  <span className="inline-flex items-center gap-1.5">
                                    {p.port_interface}
                                    {p.hw_offload && (
                                      <span
                                        className="rounded border px-1 font-sans text-[10px] leading-4 text-muted-foreground"
                                        title={`Hardware-offloaded${p.hw_offload_group ? ` (${p.hw_offload_group})` : ""}: the switch chip forwards this port's traffic`}
                                      >
                                        HW
                                      </span>
                                    )}
                                  </span>
                                </TableCell>
                                <TableCell>
                                  {p.role
                                    ? <Badge className={`text-[10px] font-normal ${roleColor(p.role)}`}>{p.role}</Badge>
                                    : <span className="text-xs text-muted-foreground">—</span>}
                                </TableCell>
                                <TableCell className={`text-xs ${statusColor(state)}`}>{state || "—"}</TableCell>
                                <TableCell className="text-xs" title={p.edge_config ? `Configured edge=${p.edge_config}` : undefined}>
                                  {p.edge ? "yes" : "no"}
                                  {p.edge_config && p.edge_config !== (p.edge ? "yes" : "no") && (
                                    <span className="text-muted-foreground"> ({p.edge_config})</span>
                                  )}
                                </TableCell>
                                <TableCell className="text-xs">{p.path_cost || "—"}</TableCell>
                                {hasChanges && (
                                  <TableCell
                                    className={`text-right text-xs tabular-nums ${changes && changes === maxChanges ? "font-semibold text-foreground" : "text-muted-foreground"}`}
                                  >
                                    {changes ?? "—"}
                                  </TableCell>
                                )}
                              </TableRow>
                            );
                          })}
                        </TableBody>
                      </Table>
                    )}
                  </div>
                );
              })}
            </CardContent>
          </Card>
        ))}
      </div>
      )}
    </div>
  );
}
