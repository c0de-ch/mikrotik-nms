"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import dynamic from "next/dynamic";
import { Wifi, Router as RouterIcon, Network as NetworkIcon, Server, Cpu, MemoryStick, Activity, ArrowDownUp } from "lucide-react";
import { useAuth } from "@/context/auth";
import { api, type TopologyData, type TopologyNode, type Device, type LinkTraffic } from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Badge } from "@/components/ui/badge";
import { deviceStatusColor } from "@/lib/status";
import { fmtBps, BRAND } from "@/components/graph/graph-style";
import type { EdgeTraffic } from "@/components/graph/topology-canvas";

const TopologyCanvas = dynamic(() => import("@/components/graph/topology-canvas"), {
  ssr: false,
  loading: () => <div className="flex h-full items-center justify-center text-sm text-muted-foreground">Loading map…</div>,
});

interface WifiClient {
  mac_address: string;
  ip_address: string;
  host_name: string;
  ap_name: string;
  ssid: string;
  band: string;
  channel: string;
  signal: string;
  tx_rate: string;
  rx_rate: string;
}

function DeviceIcon({ type, className }: { type: string; className?: string }) {
  if (type === "router") return <RouterIcon className={className} />;
  if (type === "switch") return <NetworkIcon className={className} />;
  if (type === "ap") return <Wifi className={className} />;
  return <Server className={className} />;
}

export default function MapPage() {
  const { token } = useAuth();
  const [topology, setTopology] = useState<TopologyData | null>(null);
  const [devices, setDevices] = useState<Device[]>([]);
  const [wifi, setWifi] = useState<WifiClient[]>([]);
  const [traffic, setTraffic] = useState<Map<string, EdgeTraffic>>(new Map());
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [dark] = useState(() => typeof document !== "undefined" && document.documentElement.classList.contains("dark"));

  useEffect(() => {
    if (!token) return;
    api.topology.get(token).then(setTopology).catch(console.error);
    api.devices.list(token).then(setDevices).catch(console.error);
    api.wifi.current(token).then((c) => setWifi(c as WifiClient[])).catch(console.error);
    api.traffic.links(token).then((r) => {
      setTraffic(new Map(r.links.map((l: LinkTraffic) => [l.id, { rx: l.rx_bps, tx: l.tx_bps }])));
    }).catch(console.error);
  }, [token]);

  // Live graph structure.
  useWebSocket("topology.update", useCallback((data) => setTopology(data as TopologyData), []));

  // Live node health.
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

  // Live per-link throughput.
  useWebSocket("topology.traffic", useCallback((data) => {
    const d = data as { links?: LinkTraffic[] };
    if (!d.links) return;
    setTraffic(new Map(d.links.map((l) => [l.id, { rx: l.rx_bps, tx: l.tx_bps }])));
  }, []));

  const nodes = useMemo<TopologyNode[]>(() => topology?.nodes.map((n) => n.data) ?? [], [topology]);
  const edges = useMemo(() => topology?.edges.map((e) => e.data) ?? [], [topology]);

  const deviceById = useMemo(() => {
    const m = new Map<string, Device>();
    for (const d of devices) m.set(d.id, d);
    return m;
  }, [devices]);

  // Node health is authoritative from the live device list.
  const statusById = useMemo(() => {
    const m = new Map<string, string>();
    for (const d of devices) m.set(d.id, d.status);
    return m;
  }, [devices]);

  const summary = useMemo(() => {
    let online = 0, offline = 0, rx = 0, tx = 0;
    for (const n of nodes) {
      const st = statusById.get(n.id) ?? n.status;
      if (st === "online") online++;
      else if (st === "offline") offline++;
    }
    for (const t of traffic.values()) { rx += t.rx; tx += t.tx; }
    return { total: nodes.length, online, offline, rx, tx };
  }, [nodes, statusById, traffic]);

  // Detail for the selected device.
  const detail = useMemo(() => {
    if (!selectedId) return null;
    const node = nodes.find((n) => n.id === selectedId);
    if (!node) return null;
    const dev = deviceById.get(selectedId);
    const links = edges
      .filter((e) => e.source === selectedId || e.target === selectedId)
      .map((e) => {
        const outgoing = e.source === selectedId;
        const peerId = outgoing ? e.target : e.source;
        const peer = nodes.find((n) => n.id === peerId);
        const t = traffic.get(e.id);
        return {
          localIface: outgoing ? e.source_interface : e.target_interface,
          peerLabel: peer?.label ?? "unknown",
          peerIface: outgoing ? e.target_interface : e.source_interface,
          peerStatus: peer?.status ?? "unknown",
          linkType: e.link_type,
          status: e.status,
          rx: t?.rx ?? 0,
          tx: t?.tx ?? 0,
        };
      })
      .sort((a, b) => (b.rx + b.tx) - (a.rx + a.tx));
    const label = node.label.toLowerCase();
    const clients = node.type === "ap"
      ? wifi.filter((c) => {
          const ap = (c.ap_name || "").toLowerCase();
          return ap && (ap === label || ap.includes(label) || label.includes(ap));
        })
      : [];
    return { node, dev, links, clients };
  }, [selectedId, nodes, edges, deviceById, traffic, wifi]);

  const memPct = detail?.dev && detail.dev.memory_used != null && detail.dev.memory_total
    ? Math.round((detail.dev.memory_used / detail.dev.memory_total) * 100) : null;

  return (
    <div className="flex flex-col h-[calc(100vh-6rem)] gap-3">
      <div className="flex items-center justify-between gap-3 flex-wrap">
        <div>
          <h1 className="text-2xl font-bold">Network Map</h1>
          <p className="text-sm text-muted-foreground">
            {summary.total} devices · {summary.online} online{summary.offline ? ` · ${summary.offline} offline` : ""} · live link traffic
          </p>
        </div>
        <div className="flex items-center gap-2 text-sm">
          <Badge className="bg-primary/10 text-primary gap-1">
            <ArrowDownUp className="h-3.5 w-3.5" /> {fmtBps(summary.rx + summary.tx)} total
          </Badge>
        </div>
      </div>

      <div className="relative flex-1 rounded-lg border bg-card overflow-hidden">
        {nodes.length === 0 ? (
          <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
            No topology yet — add devices and wait for discovery.
          </div>
        ) : (
          <TopologyCanvas
            nodes={nodes}
            edges={edges}
            statusById={statusById}
            trafficById={traffic}
            onSelect={setSelectedId}
            dark={dark}
          />
        )}
        {/* Legend */}
        <div className="absolute bottom-3 left-3 rounded-md border bg-background/90 backdrop-blur px-3 py-2 text-[11px] space-y-1 pointer-events-none">
          <div className="flex items-center gap-3">
            <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full" style={{ background: BRAND.primary }} />online</span>
            <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full" style={{ background: BRAND.red }} />offline</span>
            <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full" style={{ background: BRAND.grey }} />unknown</span>
          </div>
          <div className="flex items-center gap-3 text-muted-foreground">
            <span>edge width/colour = live Mbps</span>
            <span className="inline-flex items-center gap-1"><span className="h-0.5 w-4" style={{ background: BRAND.primary }} />&lt;100</span>
            <span className="inline-flex items-center gap-1"><span className="h-0.5 w-4" style={{ background: BRAND.amber }} />100–500</span>
            <span className="inline-flex items-center gap-1"><span className="h-0.5 w-4" style={{ background: BRAND.red }} />&gt;500</span>
          </div>
        </div>
        <p className="absolute top-3 right-3 text-[11px] text-muted-foreground pointer-events-none">tap a device for detail · drag to arrange · scroll to zoom</p>
      </div>

      {/* Detail panel */}
      <Sheet open={!!selectedId} onOpenChange={(o) => { if (!o) setSelectedId(null); }}>
        <SheetContent className="w-full sm:max-w-md overflow-y-auto">
          {detail && (
            <>
              <SheetHeader>
                <SheetTitle className="flex items-center gap-2">
                  <span className={`flex h-8 w-8 items-center justify-center rounded-lg text-white ${detail.node.status === "online" ? "bg-primary" : "bg-muted-foreground/50"}`}>
                    <DeviceIcon type={detail.node.type} className="h-4 w-4" />
                  </span>
                  {detail.node.label}
                </SheetTitle>
                <p className="text-xs text-muted-foreground">
                  {detail.node.address}{detail.node.model && ` · ${detail.node.model}`}{detail.node.ros_version && ` · v${detail.node.ros_version}`}
                </p>
                <div className="flex items-center gap-3 text-[11px] text-muted-foreground mt-1">
                  <span className="inline-flex items-center gap-1"><span className={`h-2 w-2 rounded-full ${deviceStatusColor(detail.node.status)}`} />{detail.node.status}</span>
                  {detail.node.cpu_load != null && <span className="inline-flex items-center gap-1"><Cpu className="h-3 w-3" />{detail.node.cpu_load}%</span>}
                  {memPct != null && <span className="inline-flex items-center gap-1"><MemoryStick className="h-3 w-3" />{memPct}%</span>}
                  {detail.dev?.uptime && <span className="inline-flex items-center gap-1"><Activity className="h-3 w-3" />{detail.dev.uptime}</span>}
                </div>
              </SheetHeader>

              <div className="px-4 pb-6 space-y-4">
                {/* Links / ports with live throughput */}
                <section>
                  <h3 className="text-xs font-semibold text-muted-foreground uppercase tracking-wide mb-2">
                    Links ({detail.links.length})
                  </h3>
                  {detail.links.length === 0 ? (
                    <p className="text-xs text-muted-foreground">No discovered inter-device links.</p>
                  ) : (
                    <div className="space-y-1.5">
                      {detail.links.map((l, i) => {
                        const total = l.rx + l.tx;
                        const barPct = Math.min(100, (total / 1e9) * 100); // relative to 1 Gbps
                        return (
                          <div key={`${l.localIface}-${i}`} className="rounded-md border px-2.5 py-2 text-xs">
                            <div className="flex items-center justify-between gap-2">
                              <span className="font-mono font-medium truncate">{l.localIface}</span>
                              <span className="text-muted-foreground flex items-center gap-1 shrink-0">
                                {l.linkType === "wireless" && <Wifi className="h-3 w-3" />}
                                <span className={`h-1.5 w-1.5 rounded-full ${deviceStatusColor(l.peerStatus)}`} />
                                {l.peerLabel}
                              </span>
                            </div>
                            <div className="mt-1 h-1.5 rounded-full bg-muted overflow-hidden">
                              <div className="h-full rounded-full" style={{ width: `${Math.max(2, barPct)}%`, background: total > 5e8 ? BRAND.red : total > 1e8 ? BRAND.amber : BRAND.primary }} />
                            </div>
                            <div className="mt-1 flex items-center justify-between text-[11px] text-muted-foreground font-mono">
                              <span>↓ {fmtBps(l.rx)}</span>
                              <span>↑ {fmtBps(l.tx)}</span>
                            </div>
                          </div>
                        );
                      })}
                    </div>
                  )}
                </section>

                {/* WiFi clients (APs only) */}
                {detail.node.type === "ap" && (
                  <section>
                    <h3 className="text-xs font-semibold text-muted-foreground uppercase tracking-wide mb-2">
                      WiFi clients ({detail.clients.length})
                    </h3>
                    {detail.clients.length === 0 ? (
                      <p className="text-xs text-muted-foreground">No associated clients right now.</p>
                    ) : (
                      <div className="space-y-1">
                        {detail.clients.map((c) => (
                          <div key={c.mac_address} className="rounded-md border px-2.5 py-1.5 text-xs">
                            <div className="flex items-center justify-between gap-2">
                              <span className="font-medium truncate">{c.host_name || c.ip_address || c.mac_address}</span>
                              <span className="text-muted-foreground shrink-0">{c.signal || ""}</span>
                            </div>
                            <div className="text-[11px] text-muted-foreground flex items-center gap-2 flex-wrap">
                              {c.ssid && <span className="font-mono">{c.ssid}</span>}
                              {c.band && <span>{c.band}</span>}
                              {(c.tx_rate || c.rx_rate) && <span className="font-mono">{c.tx_rate}/{c.rx_rate}</span>}
                            </div>
                          </div>
                        ))}
                      </div>
                    )}
                  </section>
                )}
              </div>
            </>
          )}
        </SheetContent>
      </Sheet>
    </div>
  );
}
