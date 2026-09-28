// Shared helpers for the /traffic drill-down (and the map's port heatmap).
// Colours and bps formatting come from graph-style so the map and traffic
// views read as one system.

import type { Device, PortRate, PortRole, PortRoleName, PortSnapshot, SeriesPoint, TopDir, TopMetric, TrafficRange } from "@/lib/api";
import { TRAFFIC, fmtBps } from "@/components/graph/graph-style";

export const RX_COLOR = TRAFFIC.rx;
export const TX_COLOR = TRAFFIC.tx;

// ---- ranges -----------------------------------------------------------------

export const RANGES: TrafficRange[] = ["live", "15m", "1h", "6h", "24h", "7d", "30d"];
export const RANGE_LABEL: Record<TrafficRange, string> = {
  live: "Live",
  "15m": "15m",
  "1h": "1h",
  "6h": "6h",
  "24h": "24h",
  "7d": "7d",
  "30d": "30d",
};

export function isRange(v: string | null): v is TrafficRange {
  return !!v && (RANGES as string[]).includes(v);
}

// Ranges long enough to warrant a Brush on the history chart.
export function isLongRange(r: TrafficRange): boolean {
  return r === "6h" || r === "24h" || r === "7d" || r === "30d";
}

// REST refresh cadence: aggregates over ≥24h barely move minute to minute
// and cost more to compute, so they refresh every 5 min instead of 1 min.
export function refreshMs(r: TrafficRange): number {
  return r === "24h" || r === "7d" || r === "30d" ? 300_000 : 60_000;
}

// ---- identity ---------------------------------------------------------------

export function portKey(deviceId: string, iface: string): string {
  return `${deviceId}/${iface}`;
}

export function deviceName(d: Pick<Device, "identity" | "address">): string {
  return d.identity || d.address;
}

// TS twin of the backend's IsPhysicalPort.
export function isPhysicalPort(name: string, type?: string): boolean {
  return type === "ether" || /^(ether|sfp|qsfp|combo)/i.test(name);
}

const RADIO_TYPES = new Set(["wifi", "wlan", "wireless", "cap", "w60g"]);

// Radio interfaces (wifi/wlan/CAPsMAN cap/60 GHz), by type or, when the type
// is unknown, by name.
export function isRadioPort(name: string, type?: string): boolean {
  return RADIO_TYPES.has((type || "").toLowerCase()) || /^(wlan|wifi)/i.test(name);
}

// Natural port ordering: ether2 before ether10.
export function comparePortNames(a: string, b: string): number {
  return a.localeCompare(b, undefined, { numeric: true, sensitivity: "base" });
}

// Compact heatmap label, same abbreviations the map has always used.
export function abbrevPort(name: string): string {
  return name.replace(/^ether/, "e").replace(/^sfp-sfpplus/, "sfp+").replace(/^qsfpplus/, "q");
}

// ---- roles & direction ------------------------------------------------------

const VPN_TYPES = new Set(["wg", "wireguard", "ovpn-out", "ovpn-in", "l2tp-out", "l2tp-in", "pptp-out", "pptp-in", "sstp-out", "sstp-in", "gre", "eoip", "ipip", "gre6", "eoipv6", "ipipv6"]);

// Role for a port the role graph hasn't classified yet (new interface, roles
// not loaded). Mirrors the server's type/name heuristic closely enough to
// keep virtual/VPN interfaces out of "physical" lists.
export function fallbackRole(name: string, type?: string): PortRoleName {
  const t = (type || "").toLowerCase();
  if (VPN_TYPES.has(t)) return "vpn";
  if (isPhysicalPort(name, t) || t === "bond" || isRadioPort(name, t)) return "idle";
  return "virtual";
}

export const PHYSICAL_ROLES = new Set<PortRoleName>(["wan", "uplink", "downlink", "peer", "access", "wireless", "idle"]);
const UPSTREAM = new Set<PortRoleName>(["wan", "uplink", "vpn"]);
const DOWNSTREAM = new Set<PortRoleName>(["downlink", "access", "wireless"]);

export interface DirSplit {
  down: number;
  up: number;
  // false for peer/virtual/idle ports: down/up then carry raw rx(in)/tx(out).
  directional: boolean;
}

// splitDirection maps a port's rx/tx onto download/upload using its role:
// upstream-facing ports download on rx, downstream-facing ones on tx.
export function splitDirection(role: PortRoleName | undefined, rx: number, tx: number): DirSplit {
  if (role && UPSTREAM.has(role)) return { down: rx, up: tx, directional: true };
  if (role && DOWNSTREAM.has(role)) return { down: tx, up: rx, directional: true };
  return { down: rx, up: tx, directional: false };
}

// splitDirection for one chart point; a gap (null rates: the collector did
// not cover that step) stays a gap on both series.
export function splitPoint(role: PortRoleName | undefined, p: SeriesPoint): { down: number | null; up: number | null } {
  if (p.rx_bps === null || p.tx_bps === null) return { down: null, up: null };
  const s = splitDirection(role, p.rx_bps, p.tx_bps);
  return { down: s.down, up: s.up };
}

// Labels for the two series given a split: "↓ Download (rx)" or raw "RX (in)".
export function dirLabels(role: PortRoleName | undefined): { down: string; up: string; downShort: string; upShort: string } {
  const s = splitDirection(role, 0, 0);
  if (!s.directional) return { down: "RX (in)", up: "TX (out)", downShort: "in", upShort: "out" };
  const downRaw = role && UPSTREAM.has(role) ? "rx" : "tx";
  const upRaw = downRaw === "rx" ? "tx" : "rx";
  return { down: `Download (${downRaw})`, up: `Upload (${upRaw})`, downShort: "↓", upShort: "↑" };
}

export const ROLE_META: Record<PortRoleName, { label: string; color: string; hint: string }> = {
  wan: { label: "WAN", color: "#8b5cf6", hint: "Internet-facing port (default route / gateway)" },
  uplink: { label: "uplink", color: "#0ea5e9", hint: "Faces the parent device, toward the internet" },
  downlink: { label: "downlink", color: "#5A9CB5", hint: "Feeds one or more downstream devices" },
  peer: {
    label: "peer link",
    color: "#a1887f",
    hint: "Link to another managed device outside the inferred tree (second edge router or redundant path) — no download/upload direction",
  },
  access: { label: "access", color: "#64748b", hint: "Edge port with attached clients" },
  wireless: { label: "wireless", color: "#06b6d4", hint: "Radio interface with associated clients" },
  vpn: { label: "VPN", color: "#c026d3", hint: "Tunnel interface" },
  virtual: { label: "virtual", color: "#94a3b8", hint: "Bridge, VLAN, loopback or other logical interface" },
  idle: { label: "idle", color: "#94a3b8", hint: "Physical port with no neighbour and no learned clients" },
};

// Role hint for one port. An idle port that still learned MACs (only managed
// devices, or a port the tree could not place) must not claim "no clients".
export function roleHint(role: PortRoleName, info?: Pick<PortRole, "mac_count" | "neighbor_name" | "master">): string {
  if (info?.master) return `Member of ${info.master} — carries part of the bond's traffic, in the bond's direction; clients are counted on the bond`;
  if (role === "peer" && info?.neighbor_name) return `Link to ${info.neighbor_name}, outside the inferred tree (second edge router or redundant path) — no download/upload direction`;
  if (role === "idle" && info && info.mac_count > 0) {
    return `Physical port the inferred topology could not place — ${info.mac_count} MAC${info.mac_count === 1 ? "" : "s"} learned, no neighbour or client attached`;
  }
  return ROLE_META[role]?.hint ?? ROLE_META.virtual.hint;
}

// Lookup helper shared by every view: server role → snapshot role → heuristic.
export function roleFor(
  roles: Map<string, PortRole>,
  deviceId: string,
  iface: string,
  type?: string,
  hint?: PortRoleName,
): PortRoleName {
  return roles.get(portKey(deviceId, iface))?.role ?? hint ?? fallbackRole(iface, type);
}

// ---- metric values ----------------------------------------------------------

export interface RateTotals {
  rx_avg: number;
  tx_avg: number;
  rx_max: number;
  tx_max: number;
  rx_bytes: number;
  tx_bytes: number;
}

// The ranking key the backend uses (contract §8.3), for client-side re-ranks.
export function metricValue(r: RateTotals, metric: TopMetric, dir: TopDir): number {
  if (metric === "max") {
    if (dir === "rx") return r.rx_max;
    if (dir === "tx") return r.tx_max;
    return Math.max(r.rx_max, r.tx_max);
  }
  if (metric === "bytes") {
    if (dir === "rx") return r.rx_bytes;
    if (dir === "tx") return r.tx_bytes;
    return r.rx_bytes + r.tx_bytes;
  }
  if (dir === "rx") return r.rx_avg;
  if (dir === "tx") return r.tx_avg;
  return r.rx_avg + r.tx_avg;
}

// The rx/tx pair a row's bar shows for the chosen metric.
export function metricPair(r: RateTotals, metric: TopMetric): { rx: number; tx: number } {
  if (metric === "max") return { rx: r.rx_max, tx: r.tx_max };
  if (metric === "bytes") return { rx: r.rx_bytes, tx: r.tx_bytes };
  return { rx: r.rx_avg, tx: r.tx_avg };
}

// Per-device live throughput from the snapshot: Σ(rx+tx)/2 over physical and
// radio ports (each bit crosses two ports of a switch — on an AP, ether1 and
// a radio). Drives the "busiest device" KPI and the Devices tab order.
// activePorts counts physical ports only, matching the heatmap.
export interface DeviceLoad {
  throughput: number;
  rx: number;
  tx: number;
  activePorts: number;
  ports: number;
}

export function deviceLoads(ports: PortRate[]): Map<string, DeviceLoad> {
  const m = new Map<string, DeviceLoad>();
  for (const p of ports) {
    let d = m.get(p.device_id);
    if (!d) {
      d = { throughput: 0, rx: 0, tx: 0, activePorts: 0, ports: 0 };
      m.set(p.device_id, d);
    }
    d.ports++;
    const physical = isPhysicalPort(p.iface, p.type);
    if (!physical && !isRadioPort(p.iface, p.type)) continue;
    d.rx += p.rx_bps;
    d.tx += p.tx_bps;
    d.throughput += (p.rx_bps + p.tx_bps) / 2;
    if (physical && p.rx_bps + p.tx_bps > 0) d.activePorts++;
  }
  return m;
}

// Metric toggle options; peak and volume are meaningless for one instant.
export const METRIC_OPTIONS = (live: boolean): { value: TopMetric; label: string; title: string; disabled?: boolean }[] => [
  { value: "avg", label: live ? "Now" : "Average", title: "Average rate over the range" },
  { value: "max", label: "Peak", title: "Highest poll-interval rate in the range", disabled: live },
  { value: "bytes", label: "Volume", title: "Bytes transferred in the range", disabled: live },
];

// ---- snapshot state ---------------------------------------------------------

export function collectingText(snapshot: PortSnapshot | null): string {
  const secs = 2 * (snapshot?.interval_seconds || 15);
  return `Collecting first samples — port rates appear after two polls (~${secs}s).`;
}

export function snapshotReady(snapshot: PortSnapshot | null): boolean {
  return !!snapshot && snapshot.ready && snapshot.ts !== null;
}

// ---- formatting -------------------------------------------------------------

// Compact bps for chart axes: "80 Mbps" rather than "80.0 Mbps", "1.5 Gbps"
// rather than "1.50 Gbps", so ticks stay short.
export function fmtBpsAxis(n: number): string {
  return fmtBps(n).replace(/\.(\d*?)0+ /, (_, d: string) => (d ? `.${d} ` : " "));
}

export function fmtBytes(n: number): string {
  if (!n || n < 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0;
  let v = n;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000;
    i++;
  }
  return `${v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

// "dd.mm.yyyy HH:mm" in local time (24h), same as the connectivity page.
export function fmtDateTime(iso: string): string {
  const d = new Date(iso);
  return `${pad(d.getDate())}.${pad(d.getMonth() + 1)}.${d.getFullYear()} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// Axis tick for a history chart: seconds for live, day for multi-day ranges.
export function fmtTick(ms: number, range: TrafficRange): string {
  const d = new Date(ms);
  if (range === "live") return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  if (range === "7d" || range === "30d") return `${pad(d.getDate())}.${pad(d.getMonth() + 1)} ${pad(d.getHours())}:00`;
  if (range === "24h") return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function fmtTooltipTime(ms: number, range: TrafficRange): string {
  const d = new Date(ms);
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}${range === "live" ? `:${pad(d.getSeconds())}` : ""}`;
  if (range === "24h" || range === "7d" || range === "30d") return `${pad(d.getDate())}.${pad(d.getMonth() + 1)} ${time}`;
  return time;
}

export function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
