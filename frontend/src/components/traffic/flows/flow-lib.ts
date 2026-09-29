// Shared helpers for the flow-export views (Flows tab, port "Top
// conversations", Settings card). Pure functions only.

import type { FlowEndpointClass, FlowPoint, FlowProtocol, TrafficRange } from "@/lib/api";
import { BRAND, SYNTH } from "@/components/graph/graph-style";

export const PROTOCOL_LABEL: Record<Exclude<FlowProtocol, "">, string> = {
  ipfix: "IPFIX",
  netflow9: "NetFlow v9",
  netflow5: "NetFlow v5",
  sflow5: "sFlow",
};

export function protocolLabel(p: string | null | undefined): string {
  return (p && PROTOCOL_LABEL[p as Exclude<FlowProtocol, "">]) || "flow export";
}

// A point counts as "recent" when its interfaces reported flows within 1 h.
const FRESH_MS = 3_600_000;

export function isFresh(p: Pick<FlowPoint, "last_data">): boolean {
  return !!p.last_data && Date.now() - Date.parse(p.last_data) < FRESH_MS;
}

// Picker / default order: derived views first (they answer "what is on this
// port"), then internet-facing native points, then the rest; by name.
function pointRank(p: FlowPoint): number {
  if (p.kind === "derived") return 0;
  return p.facing === "up" ? 1 : 2;
}

export function orderPoints(points: FlowPoint[]): FlowPoint[] {
  return [...points].sort((a, b) => pointRank(a) - pointRank(b) || a.name.localeCompare(b.name, undefined, { numeric: true }));
}

// defaultPoint: the first enabled point with recent data in picker order,
// else the first enabled point (history may still be readable).
export function defaultPoint(points: FlowPoint[]): FlowPoint | null {
  const enabled = orderPoints(points.filter((p) => p.enabled));
  return enabled.find(isFresh) ?? enabled[0] ?? null;
}

export function hasFreshPoint(points: FlowPoint[]): boolean {
  return points.some((p) => p.enabled && isFresh(p));
}

// The Phase 1 counter that corresponds to the point's download direction
// (contract §6.6): tx when (facing=down) XOR (port_side=peer), else rx.
export function counterDirs(p: Pick<FlowPoint, "facing" | "port_side">): { down: "rx" | "tx"; up: "rx" | "tx" } {
  const tx = (p.facing === "down") !== (p.port_side === "peer");
  return tx ? { down: "tx", up: "rx" } : { down: "rx", up: "tx" };
}

// Tab / legend labels in the Phase 1 dirLabels format. Flow rows always have
// a direction (from the point's facing), even on peer / virtual ports where
// the port-counter labels fall back to raw in/out.
export function flowDirLabels(p: Pick<FlowPoint, "facing" | "port_side">): { down: string; up: string } {
  if (!p.port_side) return { down: "Download", up: "Upload" };
  const c = counterDirs(p);
  return { down: `Download (${c.down})`, up: `Upload (${c.up})` };
}

// Coverage = flow bytes / Phase 1 counter bytes (contract §6.6); healthy is
// about 0.85–1.0. Below LOW_COVERAGE flows miss traffic; above
// HIGH_COVERAGE the view over-counts or counter samples are missing (1.2,
// not 1.0: minute-bucket edges wobble on the short ranges).
export const LOW_COVERAGE = 0.7;
export const HIGH_COVERAGE = 1.2;

export function coverageWarn(ratio: number): boolean {
  return ratio < LOW_COVERAGE || ratio > HIGH_COVERAGE;
}

export function fmtPct(ratio: number): string {
  return `${Math.round(ratio * 100)} %`;
}

export function overCoverageHint(ratio: number): string {
  return `Flows exceed this port's counters by ${Math.round((ratio - 1) * 100)} % — counter samples are missing for part of the window (device polling gaps), a sampling override is too high, or the view includes traffic that does not cross this port.`;
}

export function coverageHint(ratio: number): string {
  if (ratio > HIGH_COVERAGE) return overCoverageHint(ratio);
  return `Flows explain ${Math.round(ratio * 100)} % of this port's bytes (L3 vs L2 overhead ≈ 3–6 %). Low values: hardware-offloaded or FastTrack traffic, UDP drops, template gap.`;
}

// Port of the first usable listen entry (":2055", "0.0.0.0:2055", "[::]:2055").
export function listenPort(listen: string[] | null | undefined): number | null {
  for (const l of listen ?? []) {
    const m = /:(\d{1,5})$/.exec(l.trim());
    if (!m) continue;
    const n = Number(m[1]);
    if (n >= 1 && n <= 65535) return n;
  }
  return null;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

// "HH:MM" local time.
export function fmtHM(iso: string): string {
  const d = new Date(iso);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// isRecent: iso is at most maxAgeMs old (false for null / unparsable).
export function isRecent(iso: string | null | undefined, maxAgeMs: number): boolean {
  if (!iso) return false;
  const t = Date.parse(iso);
  return Number.isFinite(t) && Date.now() - t < maxAgeMs;
}

// isToday: iso falls on the viewer's local calendar day.
export function isToday(iso: string): boolean {
  return new Date(iso).toDateString() === new Date().toDateString();
}

// "12 s ago" / "5 min ago" / "3 h ago" / "2 d ago"; "never" for null.
export function fmtAgo(iso: string | null | undefined): string {
  if (!iso) return "never";
  const s = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 1000));
  if (s < 60) return `${s} s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

export function fmtCount(n: number): string {
  if (!Number.isFinite(n)) return "0";
  if (Math.abs(n) >= 1e6) return `${(n / 1e6).toFixed(1)} M`;
  if (Math.abs(n) >= 1e4) return `${(n / 1e3).toFixed(0)} k`;
  if (Math.abs(n) >= 1e3) return `${(n / 1e3).toFixed(1)} k`;
  return n >= 10 || Number.isInteger(n) ? String(Math.round(n)) : n.toFixed(1);
}

export function fmtSkew(ms: number): string {
  if (!ms) return "0 s";
  const s = ms / 1000;
  const sign = s > 0 ? "+" : "−";
  const a = Math.abs(s);
  return `${sign}${a >= 10 ? a.toFixed(0) : a.toFixed(1)} s`;
}

// Refresh cadence of the measured views (contract §11.3). Short ranges also
// refetch on the "flows.flushed" WS event; the poll is their safety net.
export function flowRefreshMs(range: TrafficRange): number {
  if (range === "7d" || range === "30d") return 900_000;
  if (range === "6h" || range === "24h") return 300_000;
  return 120_000;
}

export function refreshesOnFlush(range: TrafficRange): boolean {
  return range === "live" || range === "15m" || range === "1h";
}

export const CLASS_META: Record<FlowEndpointClass, { label: string; color: string; hint: string }> = {
  internal: { label: "internal", color: BRAND.primary, hint: "Inside your network (address plan, internal prefixes, or a known ARP/DHCP host)" },
  external: { label: "external", color: SYNTH.internet, hint: "Outside your network" },
  self: { label: "self", color: BRAND.grey, hint: "An address of the exporting device itself" },
  vpn: { label: "VPN", color: SYNTH.vpn, hint: "Reached through a VPN tunnel" },
  multicast: { label: "multicast", color: BRAND.amber, hint: "Multicast or broadcast address" },
};

// copyText: clipboard API where available (secure contexts), else the
// execCommand fallback (plain-http LAN deployments).
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // fall through to the textarea fallback
  }
  try {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(ta);
    return ok;
  } catch {
    return false;
  }
}
