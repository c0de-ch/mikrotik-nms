"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import dynamic from "next/dynamic";
import {
  Wifi, Router as RouterIcon, Network as NetworkIcon, Server, Cpu, MemoryStick, Activity,
  ArrowDownUp, Search, Maximize2, ZoomIn, ZoomOut, Crosshair, Users, Globe, Shield, Lock,
  Shuffle, ListTree,
} from "lucide-react";
import { useAuth } from "@/context/auth";
import {
  api, type TopologyData, type TopologyNode, type Device, type LinkTraffic,
  type BridgeVLAN, type NetworkClient, type DevicePort,
} from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { deviceStatusColor } from "@/lib/status";
import { fmtBps, BRAND, SYNTH, SYNTH_TYPES, portLoadColor } from "@/components/graph/graph-style";
import type { EdgeTraffic, CanvasApi, CanvasEdge } from "@/components/graph/topology-canvas";

const TopologyCanvas = dynamic(() => import("@/components/graph/topology-canvas"), {
  ssr: false,
  loading: () => <div className="flex h-full items-center justify-center text-sm text-muted-foreground">Loading map…</div>,
});

interface WifiClient {
  mac_address: string; ip_address: string; host_name: string; ap_name: string;
  ssid: string; band: string; channel: string; signal: string; tx_rate: string; rx_rate: string;
}

type Role = "all" | "router" | "switch" | "ap" | "other";
const PHYS_K = 3; // an interface hearing ≤3 neighbours is likely a real point-to-point port

function DeviceIcon({ type, className }: { type: string; className?: string }) {
  if (type === "router") return <RouterIcon className={className} />;
  if (type === "switch") return <NetworkIcon className={className} />;
  if (type === "ap") return <Wifi className={className} />;
  if (type === "internet") return <Globe className={className} />;
  if (type === "gateway") return <Shield className={className} />;
  if (type === "vpn") return <Lock className={className} />;
  return <Server className={className} />;
}

const SYNTH_DESCRIPTION: Record<string, string> = {
  internet: "Synthetic node — where the network's default routes lead. Edges into it are the discovered internet uplinks.",
  gateway: "External gateway (not a managed MikroTik) — devices whose default route points here egress through it.",
  vpn: "VPN tunnel interface running on the connected device.",
};

const isSynthEdge = (t: string) => t === "gateway" || t === "internet" || t === "vpn";

// ---- switch port grid (live per-port traffic, coloured by load) -------------
function PortGrid({ deviceId, dark }: { deviceId: string; dark: boolean }) {
  const { token } = useAuth();
  const [ports, setPorts] = useState<DevicePort[]>([]);
  useEffect(() => {
    if (!token) return;
    let alive = true;
    const load = () => api.devices.ports(token, deviceId).then((p) => { if (alive) setPorts(p); }).catch(() => {});
    load();
    const id = setInterval(load, 3000);
    return () => { alive = false; clearInterval(id); };
  }, [token, deviceId]);

  if (ports.length === 0) return <p className="text-xs text-muted-foreground">No physical ports.</p>;
  return (
    <div className="grid grid-cols-6 gap-1.5">
      {ports.map((p) => {
        const total = p.rx_bps + p.tx_bps;
        const down = p.disabled || !p.running;
        return (
          <div
            key={p.name}
            title={`${p.name}${p.comment ? ` (${p.comment})` : ""}\n${down ? (p.disabled ? "disabled" : "down") : `↓ ${fmtBps(p.rx_bps)}  ↑ ${fmtBps(p.tx_bps)}`}`}
            className="rounded-sm border text-[9px] font-mono px-1 py-1 text-center overflow-hidden"
            style={{
              background: down ? "transparent" : portLoadColor(total, dark),
              color: down ? "var(--muted-foreground)" : "#0b1220",
              opacity: down ? 0.5 : 1,
              borderStyle: p.disabled ? "dashed" : "solid",
            }}
          >
            {p.name.replace(/^ether/, "e").replace(/^sfp-sfpplus/, "sfp+").replace(/^qsfpplus/, "q")}
          </div>
        );
      })}
    </div>
  );
}

export default function MapPage() {
  const { token } = useAuth();
  const [topology, setTopology] = useState<TopologyData | null>(null);
  const [devices, setDevices] = useState<Device[]>([]);
  const [wifi, setWifi] = useState<WifiClient[]>([]);
  const [vlans, setVlans] = useState<BridgeVLAN[]>([]);
  const [clients, setClients] = useState<NetworkClient[]>([]);
  const [rawTraffic, setRawTraffic] = useState<Map<string, EdgeTraffic>>(new Map());
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [dark] = useState(() => typeof document !== "undefined" && document.documentElement.classList.contains("dark"));

  // filters
  const [search, setSearch] = useState("");
  const [role, setRole] = useState<Role>("all");
  const [vlanFilter, setVlanFilter] = useState<string>("");
  const [physicalOnly, setPhysicalOnly] = useState(true);
  const [showClients, setShowClients] = useState(false);
  const [layout, setLayout] = useState<"force" | "hierarchy">("force");

  const apiRef = useRef<CanvasApi | null>(null);
  const registerApi = useCallback((a: CanvasApi) => { apiRef.current = a; }, []);

  useEffect(() => {
    if (!token) return;
    api.topology.get(token).then(setTopology).catch(console.error);
    api.devices.list(token).then(setDevices).catch(console.error);
    api.wifi.current(token).then((c) => setWifi(c as WifiClient[])).catch(console.error);
    api.vlans.list(token).then(setVlans).catch(console.error);
    api.clients.cached(token).then((r) => setClients(r.clients)).catch(console.error);
    api.traffic.links(token).then((r) => setRawTraffic(new Map(r.links.map((l: LinkTraffic) => [l.id, { rx: l.rx_bps, tx: l.tx_bps }])))).catch(console.error);
  }, [token]);

  useWebSocket("topology.update", useCallback((data) => setTopology(data as TopologyData), []));
  useWebSocket("device.health", useCallback((data) => {
    const u = data as { device_id?: string; status?: Device["status"]; cpu_load?: number; uptime?: string };
    if (!u.device_id) return;
    setDevices((prev) => prev.map((d) => (d.id !== u.device_id ? d : { ...d, ...(u.status ? { status: u.status } : {}), ...(typeof u.cpu_load === "number" ? { cpu_load: u.cpu_load } : {}), ...(u.uptime ? { uptime: u.uptime } : {}) })));
  }, []));
  useWebSocket("topology.traffic", useCallback((data) => {
    const d = data as { links?: LinkTraffic[] };
    if (!d.links) return;
    setRawTraffic(new Map(d.links.map((l) => [l.id, { rx: l.rx_bps, tx: l.tx_bps }])));
  }, []));

  const nodes = useMemo<TopologyNode[]>(() => topology?.nodes.map((n) => n.data) ?? [], [topology]);
  const rawEdges = useMemo(() => topology?.edges.map((e) => e.data) ?? [], [topology]);
  const deviceById = useMemo(() => new Map(devices.map((d) => [d.id, d])), [devices]);
  const statusById = useMemo(() => new Map(devices.map((d) => [d.id, d.status])), [devices]);
  const typeById = useMemo(() => new Map(nodes.map((n) => [n.id, n.type])), [nodes]);
  const isInfra = useCallback((id: string) => { const t = typeById.get(id); return t === "router" || t === "switch"; }, [typeById]);

  // interface degree: how many distinct neighbours a device hears on each port.
  const ifaceDegree = useMemo(() => {
    const m = new Map<string, Map<string, Set<string>>>();
    const add = (dev: string, iface: string, nb: string) => {
      if (!iface) return;
      let mm = m.get(dev); if (!mm) { mm = new Map(); m.set(dev, mm); }
      let s = mm.get(iface); if (!s) { s = new Set(); mm.set(iface, s); }
      s.add(nb);
    };
    for (const e of rawEdges) { add(e.source, e.source_interface, e.target); add(e.target, e.target_interface, e.source); }
    return m;
  }, [rawEdges]);
  const degree = useCallback((dev: string, iface: string) => ifaceDegree.get(dev)?.get(iface)?.size ?? 99, [ifaceDegree]);

  // Collapse parallel edges to one per device-pair; keep the most-physical
  // constituent (lowest interface degree) as the representative for traffic.
  // Synthetic egress edges (gateway/internet/vpn) bypass the physical scoring.
  const { collapsedEdges, repLink } = useMemo(() => {
    const pairs = new Map<string, CanvasEdge & { score: number }>();
    const rep = new Map<string, string>();
    for (const e of rawEdges) {
      const [a, b] = e.source < e.target ? [e.source, e.target] : [e.target, e.source];
      const pid = `${a}__${b}`;
      const score = isSynthEdge(e.link_type)
        ? -1
        : Math.min(degree(e.source, e.source_interface), degree(e.target, e.target_interface));
      const ex = pairs.get(pid);
      if (!ex) {
        pairs.set(pid, { id: pid, source: a, target: b, link_type: e.link_type, status: e.status, score });
        rep.set(pid, e.id);
      } else {
        if (score < ex.score) { ex.score = score; ex.link_type = e.link_type; rep.set(pid, e.id); }
        if (e.status === "up") ex.status = "up";
      }
    }
    return { collapsedEdges: [...pairs.values()], repLink: rep };
  }, [rawEdges, degree]);

  const visibleEdges = useMemo<CanvasEdge[]>(() => {
    const l2 = collapsedEdges.filter((e) => !isSynthEdge(e.link_type));
    const synth = collapsedEdges.filter((e) => isSynthEdge(e.link_type));
    if (!physicalOnly) return [...l2, ...synth];

    const keptL2 = l2.filter((e) => e.score <= PHYS_K || (isInfra(e.source) && isInfra(e.target)));

    // Declutter gateway fan-in: every device default-routes to the same
    // gateway, so in physical view keep only the edge from the gateway to the
    // best-connected infra device — the L2 path the others reach it through.
    const deg = new Map<string, number>();
    for (const e of keptL2) {
      deg.set(e.source, (deg.get(e.source) ?? 0) + 1);
      deg.set(e.target, (deg.get(e.target) ?? 0) + 1);
    }
    const bestPerGw = new Map<string, { e: CanvasEdge; d: number }>();
    const keptSynth: CanvasEdge[] = [];
    for (const e of synth) {
      if (e.link_type !== "gateway") { keptSynth.push(e); continue; }
      const gwNode = e.source.startsWith("gw:") ? e.source : e.target;
      const dev = e.source.startsWith("gw:") ? e.target : e.source;
      const d = deg.get(dev) ?? 0;
      const cur = bestPerGw.get(gwNode);
      if (!cur || d > cur.d) bestPerGw.set(gwNode, { e, d });
    }
    for (const { e } of bestPerGw.values()) keptSynth.push(e);
    return [...keptL2, ...keptSynth];
  }, [collapsedEdges, physicalOnly, isInfra]);

  const pairTraffic = useMemo(() => {
    const m = new Map<string, EdgeTraffic>();
    for (const e of collapsedEdges) {
      const t = rawTraffic.get(repLink.get(e.id) ?? "");
      if (t) m.set(e.id, t);
    }
    return m;
  }, [collapsedEdges, repLink, rawTraffic]);

  // VLAN membership + client index for filters.
  const deviceVlans = useMemo(() => {
    const m = new Map<string, Set<string>>();
    for (const v of vlans) {
      const s = m.get(v.device_id) ?? new Set<string>();
      for (const id of (v.vlan_ids || "").split(",").map((x) => x.trim()).filter(Boolean)) s.add(id);
      m.set(v.device_id, s);
    }
    return m;
  }, [vlans]);
  const vlanOptions = useMemo(() => {
    const s = new Set<string>();
    for (const set of deviceVlans.values()) for (const id of set) s.add(id);
    return [...s].sort((a, b) => Number(a) - Number(b));
  }, [deviceVlans]);
  const clientIndex = useMemo(() => {
    const cnt = new Map<string, number>();
    const idx = new Map<string, string>();
    for (const c of clients) {
      if (!c.device_id) continue;
      cnt.set(c.device_id, (cnt.get(c.device_id) ?? 0) + 1);
      idx.set(c.device_id, (idx.get(c.device_id) ?? "") + ` ${c.ip_address} ${c.host_name} ${c.mac_address}`.toLowerCase());
    }
    return { cnt, idx };
  }, [clients]);

  // matchedDevices drives the zoom; matchIds additionally keeps the synthetic
  // egress nodes visible (never dimmed) for context.
  const { matchIds, matchedDevices } = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q && !vlanFilter && role === "all" && !showClients) return { matchIds: null, matchedDevices: [] as string[] };
    const out = new Set<string>();
    for (const n of nodes) {
      if (SYNTH_TYPES.has(n.type || "")) continue;
      if (role !== "all") {
        const t = n.type || "other";
        if (role === "other" ? ["router", "switch", "ap"].includes(t) : t !== role) continue;
      }
      if (vlanFilter && !(deviceVlans.get(n.id)?.has(vlanFilter))) continue;
      if (showClients && (clientIndex.cnt.get(n.id) ?? 0) === 0) continue;
      if (q) {
        const self = n.label.toLowerCase().includes(q) || n.address.toLowerCase().includes(q) || (n.model || "").toLowerCase().includes(q);
        const cli = (clientIndex.idx.get(n.id) ?? "").includes(q);
        if (!self && !cli) continue;
      }
      out.add(n.id);
    }
    const devices = [...out];
    for (const n of nodes) if (SYNTH_TYPES.has(n.type || "")) out.add(n.id);
    return { matchIds: out, matchedDevices: devices };
  }, [search, vlanFilter, role, showClients, nodes, deviceVlans, clientIndex]);

  // Zoom to the matched segment when the filter RESULT changes — keyed on the
  // stable id set, not array identity, so the 60s topology broadcasts don't
  // yank the user's pan/zoom while a filter is active.
  const matchedRef = useRef(matchedDevices);
  useEffect(() => {
    matchedRef.current = matchedDevices;
  }, [matchedDevices]);
  const matchKey = useMemo(() => matchedDevices.slice().sort().join(","), [matchedDevices]);
  useEffect(() => {
    if (matchKey) {
      const t = setTimeout(() => apiRef.current?.fitTo(matchedRef.current), 120);
      return () => clearTimeout(t);
    }
  }, [matchKey]);

  const summary = useMemo(() => {
    let total = 0, online = 0, offline = 0, rx = 0, tx = 0;
    for (const n of nodes) {
      if (SYNTH_TYPES.has(n.type || "")) continue;
      total++;
      const st = statusById.get(n.id) ?? n.status;
      if (st === "online") online++; else if (st === "offline") offline++;
    }
    for (const t of pairTraffic.values()) { rx += t.rx; tx += t.tx; }
    return { total, online, offline, thru: rx + tx, edges: visibleEdges.length, raw: rawEdges.length };
  }, [nodes, statusById, pairTraffic, visibleEdges, rawEdges]);

  const detail = useMemo(() => {
    if (!selectedId) return null;
    const node = nodes.find((n) => n.id === selectedId);
    if (!node) return null;
    const dev = deviceById.get(selectedId);
    const links = visibleEdges
      .filter((e) => e.source === selectedId || e.target === selectedId)
      .map((e) => {
        const peerId = e.source === selectedId ? e.target : e.source;
        const peer = nodes.find((n) => n.id === peerId);
        const t = pairTraffic.get(e.id);
        return { peerLabel: peer?.label ?? "?", peerStatus: peer?.status ?? "unknown", linkType: e.link_type, rx: t?.rx ?? 0, tx: t?.tx ?? 0 };
      })
      .sort((a, b) => (b.rx + b.tx) - (a.rx + a.tx));
    const label = node.label.toLowerCase();
    const wifiClients = node.type === "ap"
      ? wifi.filter((c) => { const ap = (c.ap_name || "").toLowerCase(); return ap && (ap === label || ap.includes(label) || label.includes(ap)); })
      : [];
    return { node, dev, links, wifiClients, clientCount: clientIndex.cnt.get(selectedId) ?? 0 };
  }, [selectedId, nodes, deviceById, visibleEdges, pairTraffic, wifi, clientIndex]);

  const memPct = detail?.dev && detail.dev.memory_used != null && detail.dev.memory_total
    ? Math.round((detail.dev.memory_used / detail.dev.memory_total) * 100) : null;

  const roles: Role[] = ["all", "router", "switch", "ap", "other"];

  return (
    <div className="flex flex-col h-[calc(100vh-6rem)] gap-3">
      {/* header + filters */}
      <div className="flex items-center justify-between gap-3 flex-wrap">
        <div>
          <h1 className="text-2xl font-bold">Network Map</h1>
          <p className="text-sm text-muted-foreground">
            {summary.total} devices · {summary.online} online{summary.offline ? ` · ${summary.offline} offline` : ""} · {summary.edges} links{physicalOnly ? ` (of ${summary.raw})` : ""}
          </p>
        </div>
        <Badge className="bg-primary/10 text-primary gap-1"><ArrowDownUp className="h-3.5 w-3.5" /> {fmtBps(summary.thru)}</Badge>
      </div>

      <div className="flex items-center gap-2 flex-wrap">
        <div className="relative w-56">
          <Search className="absolute left-2.5 top-2 h-4 w-4 text-muted-foreground" />
          <Input placeholder="Search name / IP / client…" value={search} onChange={(e) => setSearch(e.target.value)} className="pl-8 h-9" />
        </div>
        <div className="inline-flex rounded-md border overflow-hidden text-sm">
          {roles.map((r) => (
            <button key={r} onClick={() => setRole(r)} className={`px-2.5 h-9 capitalize ${role === r ? "bg-foreground text-background" : "hover:bg-muted"}`}>{r}</button>
          ))}
        </div>
        <select value={vlanFilter} onChange={(e) => setVlanFilter(e.target.value)} className="h-9 rounded-md border bg-background px-2 text-sm">
          <option value="">All VLANs</option>
          {vlanOptions.map((v) => <option key={v} value={v}>VLAN {v}</option>)}
        </select>
        <button onClick={() => setPhysicalOnly((v) => !v)} className={`px-2.5 h-9 rounded-md border text-sm ${physicalOnly ? "bg-foreground text-background" : "hover:bg-muted"}`} title="Collapse parallel edges + hide the MNDP flood mesh (heuristic)">
          Physical links
        </button>
        <button onClick={() => setShowClients((v) => !v)} className={`px-2.5 h-9 rounded-md border text-sm inline-flex items-center gap-1 ${showClients ? "bg-foreground text-background" : "hover:bg-muted"}`}>
          <Users className="h-3.5 w-3.5" /> With clients
        </button>
        <div className="inline-flex rounded-md border overflow-hidden text-sm" title="Layout">
          <button onClick={() => setLayout("force")} className={`px-2.5 h-9 inline-flex items-center gap-1 ${layout === "force" ? "bg-foreground text-background" : "hover:bg-muted"}`}>
            <Shuffle className="h-3.5 w-3.5" /> Auto
          </button>
          <button onClick={() => setLayout("hierarchy")} className={`px-2.5 h-9 inline-flex items-center gap-1 border-l ${layout === "hierarchy" ? "bg-foreground text-background" : "hover:bg-muted"}`}>
            <ListTree className="h-3.5 w-3.5" /> Hierarchy
          </button>
        </div>
      </div>

      <div className="relative flex-1 rounded-lg border bg-card overflow-hidden">
        {nodes.length === 0 ? (
          <div className="flex h-full items-center justify-center text-sm text-muted-foreground">No topology yet.</div>
        ) : (
          <TopologyCanvas
            nodes={nodes}
            edges={visibleEdges}
            statusById={statusById}
            trafficById={pairTraffic}
            matchIds={matchIds}
            layoutName={layout}
            onSelect={setSelectedId}
            registerApi={registerApi}
            dark={dark}
          />
        )}

        {/* zoom controls */}
        <div className="absolute top-3 right-3 flex flex-col gap-1">
          <Button variant="outline" size="icon" className="h-8 w-8 bg-background/90" title="Fit" onClick={() => apiRef.current?.fit()}><Maximize2 className="h-4 w-4" /></Button>
          <Button variant="outline" size="icon" className="h-8 w-8 bg-background/90" title="Zoom in" onClick={() => apiRef.current?.zoomBy(1.3)}><ZoomIn className="h-4 w-4" /></Button>
          <Button variant="outline" size="icon" className="h-8 w-8 bg-background/90" title="Zoom out" onClick={() => apiRef.current?.zoomBy(0.77)}><ZoomOut className="h-4 w-4" /></Button>
          {matchedDevices.length > 0 && (
            <Button variant="outline" size="icon" className="h-8 w-8 bg-background/90" title="Focus filtered segment" onClick={() => apiRef.current?.fitTo(matchedDevices)}><Crosshair className="h-4 w-4" /></Button>
          )}
        </div>

        {/* legend */}
        <div className="absolute bottom-3 left-3 rounded-md border bg-background/90 backdrop-blur px-3 py-2 text-[11px] space-y-1 pointer-events-none">
          <div className="flex items-center gap-3">
            <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full" style={{ background: BRAND.primary }} />online</span>
            <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full" style={{ background: BRAND.red }} />offline</span>
            <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full" style={{ background: SYNTH.internet }} />internet</span>
            <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full" style={{ background: SYNTH.gateway }} />gateway</span>
            <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full" style={{ background: SYNTH.vpn }} />vpn</span>
          </div>
          <div className="flex items-center gap-3 text-muted-foreground">
            <span>edge = live Mbps</span>
            <span className="inline-flex items-center gap-1"><span className="h-0.5 w-4" style={{ background: BRAND.primary }} />&lt;100</span>
            <span className="inline-flex items-center gap-1"><span className="h-0.5 w-4" style={{ background: BRAND.amber }} />100–500</span>
            <span className="inline-flex items-center gap-1"><span className="h-0.5 w-4" style={{ background: BRAND.red }} />&gt;500</span>
            <span>· dashed = routed/VPN path</span>
          </div>
        </div>
      </div>

      {/* detail panel */}
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
                {SYNTH_TYPES.has(detail.node.type) && (
                  <p className="text-xs text-muted-foreground mt-1">{SYNTH_DESCRIPTION[detail.node.type]}</p>
                )}
                {!SYNTH_TYPES.has(detail.node.type) && (
                  <div className="flex items-center gap-3 text-[11px] text-muted-foreground mt-1 flex-wrap">
                    <span className="inline-flex items-center gap-1"><span className={`h-2 w-2 rounded-full ${deviceStatusColor(detail.node.status)}`} />{detail.node.status}</span>
                    {detail.node.cpu_load != null && <span className="inline-flex items-center gap-1"><Cpu className="h-3 w-3" />{detail.node.cpu_load}%</span>}
                    {memPct != null && <span className="inline-flex items-center gap-1"><MemoryStick className="h-3 w-3" />{memPct}%</span>}
                    {detail.dev?.uptime && <span className="inline-flex items-center gap-1"><Activity className="h-3 w-3" />{detail.dev.uptime}</span>}
                    {detail.clientCount > 0 && <span className="inline-flex items-center gap-1"><Users className="h-3 w-3" />{detail.clientCount}</span>}
                  </div>
                )}
              </SheetHeader>

              <div className="px-4 pb-6 space-y-4">
                {(detail.node.type === "switch" || detail.node.type === "router") && (
                  <section>
                    <h3 className="text-xs font-semibold text-muted-foreground uppercase tracking-wide mb-2">Ports (live)</h3>
                    <PortGrid deviceId={detail.node.id} dark={dark} />
                  </section>
                )}

                <section>
                  <h3 className="text-xs font-semibold text-muted-foreground uppercase tracking-wide mb-2">Links ({detail.links.length})</h3>
                  {detail.links.length === 0 ? <p className="text-xs text-muted-foreground">No links in the current view.</p> : (
                    <div className="space-y-1.5">
                      {detail.links.map((l, i) => {
                        const total = l.rx + l.tx;
                        return (
                          <div key={i} className="rounded-md border px-2.5 py-2 text-xs">
                            <div className="flex items-center justify-between gap-2">
                              <span className="font-medium truncate flex items-center gap-1">
                                {l.linkType === "wireless" && <Wifi className="h-3 w-3" />}
                                <span className={`h-1.5 w-1.5 rounded-full ${deviceStatusColor(l.peerStatus)}`} />{l.peerLabel}
                              </span>
                              <span className="text-muted-foreground font-mono shrink-0">{fmtBps(total)}</span>
                            </div>
                            <div className="mt-1 h-1.5 rounded-full bg-muted overflow-hidden">
                              <div className="h-full rounded-full" style={{ width: `${Math.max(2, Math.min(100, (total / 1e9) * 100))}%`, background: total > 5e8 ? BRAND.red : total > 1e8 ? BRAND.amber : BRAND.primary }} />
                            </div>
                            <div className="mt-1 flex justify-between text-[11px] text-muted-foreground font-mono"><span>↓ {fmtBps(l.rx)}</span><span>↑ {fmtBps(l.tx)}</span></div>
                          </div>
                        );
                      })}
                    </div>
                  )}
                </section>

                {detail.node.type === "ap" && (
                  <section>
                    <h3 className="text-xs font-semibold text-muted-foreground uppercase tracking-wide mb-2">WiFi clients ({detail.wifiClients.length})</h3>
                    {detail.wifiClients.length === 0 ? <p className="text-xs text-muted-foreground">No associated clients.</p> : (
                      <div className="space-y-1">
                        {detail.wifiClients.map((c) => (
                          <div key={c.mac_address} className="rounded-md border px-2.5 py-1.5 text-xs">
                            <div className="flex items-center justify-between gap-2">
                              <span className="font-medium truncate">{c.host_name || c.ip_address || c.mac_address}</span>
                              <span className="text-muted-foreground shrink-0">{c.signal || ""}</span>
                            </div>
                            <div className="text-[11px] text-muted-foreground flex items-center gap-2 flex-wrap">
                              {c.ssid && <span className="font-mono">{c.ssid}</span>}{c.band && <span>{c.band}</span>}
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
