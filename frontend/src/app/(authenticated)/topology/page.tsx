"use client";

import { useEffect, useState, useMemo, useCallback } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Copy,
  Search,
  Server,
  Wifi,
  Network as NetworkIcon,
  Router as RouterIcon,
  Cpu,
  MemoryStick,
  Activity,
  LayoutGrid,
  Rows,
  ChevronDown,
  ChevronRight,
} from "lucide-react";
import { toast } from "sonner";
import { useAuth } from "@/context/auth";
import { api, type TopologyData, type TopologyNode, type Device } from "@/lib/api";
import { deviceStatusColor } from "@/lib/status";
import { SYNTH_TYPES } from "@/components/graph/graph-style";
import { useWebSocket } from "@/hooks/use-websocket";

interface PortConnection {
  localInterface: string;
  remoteDevice: string;
  remoteInterface: string;
  remoteStatus: string;
  linkType: string;
  linkStatus: string;
}

function copyText(text: string) {
  if (navigator.clipboard?.writeText) {
    navigator.clipboard.writeText(text);
    return;
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  document.execCommand("copy");
  document.body.removeChild(ta);
}

function DeviceIcon({ type }: { type: string }) {
  if (type === "router") return <RouterIcon className="h-5 w-5" />;
  if (type === "switch") return <NetworkIcon className="h-5 w-5" />;
  if (type === "ap") return <Wifi className="h-5 w-5" />;
  return <Server className="h-5 w-5" />;
}

const statusDotColor = deviceStatusColor;

// Heat-aware text color for a load percentage (0..100).
function loadColor(n: number | null | undefined) {
  if (n == null) return "text-muted-foreground";
  if (n >= 85) return "text-red-600";
  if (n >= 60) return "text-amber-600";
  return "text-muted-foreground";
}

// Compress a RouterOS uptime like "2d1h33m15s" to "2d 1h".
function shortUptime(u: string | null | undefined): string {
  if (!u) return "";
  // grab the first two units (dhms ordered) for readability
  const m = u.match(/(\d+w)?(\d+d)?(\d+h)?(\d+m)?(\d+s)?/);
  if (!m) return u;
  const parts = m.slice(1).filter(Boolean);
  return parts.slice(0, 2).join(" ");
}

type ViewMode = "compact" | "detailed";
type TypeFilter = "all" | "router" | "switch" | "ap" | "other";

const PORTS_VISIBLE_DEFAULT = 8;

export default function TopologyPage() {
  const { token } = useAuth();
  const [topology, setTopology] = useState<TopologyData | null>(null);
  const [devices, setDevices] = useState<Device[]>([]);
  const [search, setSearch] = useState("");
  const [typeFilter, setTypeFilter] = useState<TypeFilter>("all");
  const [view, setView] = useState<ViewMode>("compact");
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});

  const loadAll = useCallback(() => {
    if (!token) return;
    api.topology.get(token).then(setTopology).catch(console.error);
    api.devices.list(token).then(setDevices).catch(console.error);
  }, [token]);

  useEffect(() => {
    loadAll();
  }, [loadAll]);

  useWebSocket("topology.update", (data) => {
    setTopology(data as TopologyData);
  });
  // Merge live health deltas into device state instead of refetching the whole
  // device list on every tick (at scale that was many full reloads per minute).
  useWebSocket("device.health", useCallback((data) => {
    const u = data as { device_id?: string; status?: Device["status"]; cpu_load?: number; uptime?: string; last_seen?: string };
    if (!u.device_id) return;
    setDevices((prev) => prev.map((d) => {
      if (d.id !== u.device_id) return d;
      const next = { ...d };
      if (u.status) next.status = u.status;
      if (typeof u.cpu_load === "number") next.cpu_load = u.cpu_load;
      if (u.uptime) next.uptime = u.uptime;
      if (u.last_seen) next.last_seen = u.last_seen;
      return next;
    }));
  }, []));

  // Build per-device port connections + lookup maps.
  const { deviceConnections, deviceByID } = useMemo(() => {
    const dById = new Map<string, Device>();
    for (const d of devices) dById.set(d.id, d);

    if (!topology) return { deviceConnections: new Map<string, PortConnection[]>(), deviceByID: dById };

    const nMap = new Map<string, TopologyNode>();
    for (const n of topology.nodes) nMap.set(n.data.id, n.data);

    const conns = new Map<string, PortConnection[]>();
    for (const n of topology.nodes) conns.set(n.data.id, []);

    for (const e of topology.edges) {
      const src = nMap.get(e.data.source);
      const tgt = nMap.get(e.data.target);
      if (!src || !tgt) continue;
      // Synthetic egress elements (internet/gateway/vpn) belong to the Map
      // page; this card grid shows managed devices and their L2 links only.
      if (SYNTH_TYPES.has(src.type || "") || SYNTH_TYPES.has(tgt.type || "")) continue;

      conns.get(e.data.source)?.push({
        localInterface: e.data.source_interface,
        remoteDevice: tgt.label,
        remoteInterface: e.data.target_interface,
        remoteStatus: tgt.status,
        linkType: e.data.link_type,
        linkStatus: e.data.status,
      });
      conns.get(e.data.target)?.push({
        localInterface: e.data.target_interface,
        remoteDevice: src.label,
        remoteInterface: e.data.source_interface,
        remoteStatus: src.status,
        linkType: e.data.link_type,
        linkStatus: e.data.status,
      });
    }
    for (const [, ports] of conns) {
      ports.sort((a, b) => a.localInterface.localeCompare(b.localInterface, undefined, { numeric: true }));
    }
    return { deviceConnections: conns, deviceByID: dById };
  }, [topology, devices]);

  const nodes = useMemo(
    () => topology?.nodes.map((n) => n.data).filter((n) => !SYNTH_TYPES.has(n.type || "")) ?? [],
    [topology],
  );

  // Summary stats — computed from all nodes, ignoring filters.
  const summary = useMemo(() => {
    const l2Edges = (topology?.edges ?? []).filter(
      (e) => e.data.link_type === "ethernet" || e.data.link_type === "wireless",
    );
    const s = {
      total: nodes.length,
      online: 0,
      offline: 0,
      byType: { router: 0, switch: 0, ap: 0, other: 0 },
      connections: l2Edges.length,
      wireless: 0,
    };
    for (const n of nodes) {
      if (n.status === "online") s.online++;
      else if (n.status === "offline") s.offline++;
      const t = (n.type || "other") as keyof typeof s.byType;
      if (t in s.byType) s.byType[t]++;
      else s.byType.other++;
    }
    for (const e of topology?.edges ?? []) {
      if (e.data.link_type === "wireless") s.wireless++;
    }
    return s;
  }, [nodes, topology]);

  // Filter + sort devices.
  const filteredNodes = useMemo(() => {
    const q = search.toLowerCase();
    return nodes
      .filter((n) => {
        if (typeFilter !== "all") {
          const t = (n.type || "other") as TypeFilter;
          if (typeFilter === "other") {
            if (n.type === "router" || n.type === "switch" || n.type === "ap") return false;
          } else if (t !== typeFilter) return false;
        }
        if (!q) return true;
        return (
          n.label.toLowerCase().includes(q) ||
          n.address.toLowerCase().includes(q) ||
          (n.model || "").toLowerCase().includes(q)
        );
      })
      .sort((a, b) => {
        if (a.status !== b.status) return a.status === "online" ? -1 : 1;
        return a.label.localeCompare(b.label);
      });
  }, [nodes, search, typeFilter]);

  if (!topology) {
    return (
      <div className="flex h-[50vh] items-center justify-center text-muted-foreground">
        Loading topology...
      </div>
    );
  }

  if (topology.nodes.length === 0) {
    return (
      <div className="flex h-[50vh] items-center justify-center text-muted-foreground">
        No topology data — add devices and wait for discovery
      </div>
    );
  }

  const filterPills: { value: TypeFilter; label: string; n: number }[] = [
    { value: "all", label: "All", n: summary.total },
    { value: "router", label: "Routers", n: summary.byType.router },
    { value: "switch", label: "Switches", n: summary.byType.switch },
    { value: "ap", label: "APs", n: summary.byType.ap },
    { value: "other", label: "Other", n: summary.byType.other },
  ];

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-3 flex-wrap">
        <div>
          <h1 className="text-2xl font-bold">Network Topology</h1>
          <p className="text-sm text-muted-foreground">
            {summary.total} devices · {summary.connections} connections{summary.wireless ? ` · ${summary.wireless} wireless` : ""}
          </p>
        </div>
        <div className="flex items-center gap-2 flex-wrap">
          <div className="relative w-56">
            <Search className="absolute left-2.5 top-2 h-4 w-4 text-muted-foreground" />
            <Input
              placeholder="Search devices..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="pl-8"
            />
          </div>
          <div className="inline-flex rounded-md border overflow-hidden">
            <button
              className={`px-2 h-9 inline-flex items-center gap-1 text-sm ${view === "compact" ? "bg-muted" : "hover:bg-muted/50"}`}
              onClick={() => setView("compact")}
              title="Compact view"
            >
              <LayoutGrid className="h-4 w-4" />
              <span className="hidden sm:inline">Compact</span>
            </button>
            <button
              className={`px-2 h-9 inline-flex items-center gap-1 text-sm border-l ${view === "detailed" ? "bg-muted" : "hover:bg-muted/50"}`}
              onClick={() => setView("detailed")}
              title="Detailed view"
            >
              <Rows className="h-4 w-4" />
              <span className="hidden sm:inline">Detailed</span>
            </button>
          </div>
        </div>
      </div>

      {/* Summary chips */}
      <div className="flex flex-wrap items-center gap-2">
        <Badge className="bg-green-100 text-green-700">
          <span className="mr-1 h-1.5 w-1.5 rounded-full bg-green-500 inline-block" />
          {summary.online} online
        </Badge>
        {summary.offline > 0 && (
          <Badge className="bg-red-100 text-red-700">
            <span className="mr-1 h-1.5 w-1.5 rounded-full bg-red-500 inline-block" />
            {summary.offline} offline
          </Badge>
        )}
        <span className="text-muted-foreground text-xs ml-2">filter:</span>
        {filterPills.map((p) => (
          <button
            key={p.value}
            onClick={() => setTypeFilter(p.value)}
            className={`px-2.5 h-7 rounded-full border text-xs inline-flex items-center gap-1.5 transition-colors ${
              typeFilter === p.value ? "bg-foreground text-background border-foreground" : "hover:bg-muted"
            }`}
          >
            {p.label}
            <span className="opacity-70">{p.n}</span>
          </button>
        ))}
      </div>

      <div className={view === "compact" ? "grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4" : "grid gap-4 md:grid-cols-2 xl:grid-cols-3"}>
        {filteredNodes.map((node) => {
          const ports = deviceConnections.get(node.id) || [];
          const wired = ports.filter((p) => p.linkType !== "wireless");
          const wireless = ports.filter((p) => p.linkType === "wireless");
          const dev = deviceByID.get(node.id);
          const memPct = dev && dev.memory_used != null && dev.memory_total && dev.memory_total > 0
            ? Math.round((dev.memory_used / dev.memory_total) * 100)
            : null;
          const isExpanded = expanded[node.id] ?? false;
          const visiblePorts = view === "detailed" && !isExpanded && ports.length > PORTS_VISIBLE_DEFAULT
            ? ports.slice(0, PORTS_VISIBLE_DEFAULT)
            : ports;

          return (
            <Card key={node.id} className="overflow-hidden">
              <CardHeader className="pb-2">
                <div className="flex items-center gap-3">
                  <div className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-lg text-white ${node.status === "online" ? "bg-primary" : "bg-muted-foreground/50"}`}>
                    <DeviceIcon type={node.type} />
                  </div>
                  <div className="flex-1 min-w-0">
                    <CardTitle className="text-sm truncate" title={node.label}>{node.label}</CardTitle>
                    <p className="text-xs text-muted-foreground truncate" title={`${node.address} · ${node.model}`}>
                      {node.address}{node.model && ` · ${node.model}`}
                    </p>
                  </div>
                  <div className="flex items-center gap-1 shrink-0">
                    <div className={`h-2.5 w-2.5 rounded-full ${statusDotColor(node.status)}`} title={node.status} />
                    <Button variant="ghost" size="icon" className="h-7 w-7" title="Copy IP" onClick={(e) => { e.stopPropagation(); copyText(node.address); toast.success(`Copied ${node.address}`); }}>
                      <Copy className="h-3.5 w-3.5" />
                    </Button>
                  </div>
                </div>
                {/* Health line */}
                <div className="flex items-center gap-3 text-[11px] text-muted-foreground mt-1 pl-12 flex-wrap">
                  {node.cpu_load != null && (
                    <span className={`inline-flex items-center gap-1 ${loadColor(node.cpu_load)}`} title="CPU load">
                      <Cpu className="h-3 w-3" />{node.cpu_load}%
                    </span>
                  )}
                  {memPct != null && (
                    <span className={`inline-flex items-center gap-1 ${loadColor(memPct)}`} title="Memory used">
                      <MemoryStick className="h-3 w-3" />{memPct}%
                    </span>
                  )}
                  {dev?.uptime && (
                    <span className="inline-flex items-center gap-1" title={`uptime: ${dev.uptime}`}>
                      <Activity className="h-3 w-3" />{shortUptime(dev.uptime)}
                    </span>
                  )}
                  {node.ros_version && <span title="RouterOS version">v{node.ros_version}</span>}
                </div>
              </CardHeader>

              <CardContent className="pt-0">
                {view === "compact" ? (
                  ports.length > 0 ? (
                    <div className="space-y-1">
                      <div className="flex items-center justify-between text-xs">
                        <span className="text-muted-foreground">
                          {ports.length} neighbor{ports.length === 1 ? "" : "s"}
                          {wireless.length > 0 && (
                            <span className="ml-1.5 inline-flex items-center gap-0.5 text-muted-foreground">
                              <Wifi className="h-3 w-3" />{wireless.length}
                            </span>
                          )}
                          {wired.length > 0 && wireless.length > 0 && (
                            <span className="ml-1.5 text-muted-foreground">· wired {wired.length}</span>
                          )}
                        </span>
                      </div>
                      {/* Show up to 3 top connections */}
                      {ports.slice(0, 3).map((port, i) => (
                        <div key={`${port.localInterface}-${i}`} className="flex items-center gap-1.5 text-[11px]">
                          <span className="font-mono truncate w-16 shrink-0" title={port.localInterface}>{port.localInterface}</span>
                          <span className="text-muted-foreground">→</span>
                          <div className={`h-1.5 w-1.5 rounded-full shrink-0 ${statusDotColor(port.remoteStatus)}`} />
                          <span className="truncate" title={`${port.remoteDevice}:${port.remoteInterface}`}>{port.remoteDevice}</span>
                          {port.linkType === "wireless" && <Wifi className="h-2.5 w-2.5 text-muted-foreground shrink-0" />}
                        </div>
                      ))}
                      {ports.length > 3 && (
                        <button
                          onClick={() => setView("detailed")}
                          className="text-[11px] text-muted-foreground hover:text-foreground inline-flex items-center gap-0.5"
                        >
                          +{ports.length - 3} more
                        </button>
                      )}
                    </div>
                  ) : (
                    <p className="text-xs text-muted-foreground">No discovered connections</p>
                  )
                ) : ports.length > 0 ? (
                  <div className="space-y-0.5">
                    {visiblePorts.map((port, i) => (
                      <div key={`${port.localInterface}-${i}`} className="flex items-center gap-2 rounded px-2 py-1.5 text-xs hover:bg-muted/50">
                        <span className="font-mono font-medium w-20 shrink-0 truncate" title={port.localInterface}>{port.localInterface}</span>
                        <span className="text-muted-foreground">→</span>
                        <div className="flex-1 min-w-0 flex items-center gap-1.5">
                          <div className={`h-1.5 w-1.5 rounded-full shrink-0 ${statusDotColor(port.remoteStatus)}`} />
                          <span className="truncate font-medium">{port.remoteDevice}</span>
                          <span className="text-muted-foreground font-mono shrink-0">:{port.remoteInterface}</span>
                        </div>
                        {port.linkType === "wireless" && <Wifi className="h-3 w-3 text-muted-foreground shrink-0" />}
                      </div>
                    ))}
                    {ports.length > PORTS_VISIBLE_DEFAULT && (
                      <button
                        onClick={() => setExpanded((s) => ({ ...s, [node.id]: !isExpanded }))}
                        className="mt-1 text-xs text-muted-foreground hover:text-foreground inline-flex items-center gap-1 px-2"
                      >
                        {isExpanded
                          ? (<><ChevronDown className="h-3 w-3" />Show fewer</>)
                          : (<><ChevronRight className="h-3 w-3" />Show {ports.length - PORTS_VISIBLE_DEFAULT} more</>)}
                      </button>
                    )}
                  </div>
                ) : (
                  <p className="text-xs text-muted-foreground py-2">No discovered connections</p>
                )}
              </CardContent>
            </Card>
          );
        })}
      </div>

      {filteredNodes.length === 0 && (
        <Card>
          <CardContent className="py-12 text-center text-sm text-muted-foreground">
            No devices match the current filter.
          </CardContent>
        </Card>
      )}
    </div>
  );
}
