import { Badge } from "@/components/ui/badge";

export interface WifiEntry {
  id: number;
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
  event: string;
  controller_id: string;
  controller_name: string;
  source: string;
  reason: string;
  recorded_at: string;
}

export interface WifiEvent {
  mac: string;
  ap: string;
  prev_ap: string;
  event: string;
  signal: string;
  time: string;
}

export type MACLookupMap = Record<string, { mac_address: string; ip_address: string; host_name: string; dns_name: string; vendor?: string; randomized?: boolean; updated_at?: string }>;

// Detail panels that open from the stat cards at the top of the page.
export type StatView = "clients" | "aps" | "roams" | "events";

export function formatRate(rate?: string): string {
  if (!rate) return "—";
  const n = parseInt(rate);
  if (isNaN(n)) return rate;
  if (n >= 1e6) return `${(n / 1e6).toFixed(0)} Mbps`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(0)} Kbps`;
  return `${n} bps`;
}

export function signalColor(signal: string): string {
  const v = parseInt(signal);
  if (v > -60) return "text-green-600";
  if (v > -75) return "text-yellow-600";
  return "text-red-600";
}

export type SignalQuality = "good" | "fair" | "poor";

// signalQuality buckets a dBm reading with the same thresholds as signalColor.
export function signalQuality(signal: string): SignalQuality | null {
  const v = parseInt(signal);
  if (isNaN(v)) return null;
  if (v > -60) return "good";
  if (v > -75) return "fair";
  return "poor";
}

// bandLabel turns RouterOS band names ("2ghz-ax", "5ghz-ac") into "2.4 GHz" / "5 GHz" / "6 GHz".
export function bandLabel(band: string): string {
  const b = band.toLowerCase();
  if (b.startsWith("2")) return "2.4 GHz";
  if (b.startsWith("5")) return "5 GHz";
  if (b.startsWith("6")) return "6 GHz";
  return band || "—";
}

// isWireless reports whether a "current" entry looks like a real WiFi
// association (vs. a wired/unknown MAC that slipped into the table).
export function isWireless(e: WifiEntry): boolean {
  return !!(e.ssid || e.band || e.signal);
}

export function eventBadge(event: string) {
  switch (event) {
    case "join": return <Badge className="bg-green-100 text-green-700">join</Badge>;
    case "leave": return <Badge className="bg-red-100 text-red-700">leave</Badge>;
    case "roam": return <Badge className="bg-blue-100 text-blue-700">roam</Badge>;
    default: return <Badge variant="secondary">seen</Badge>;
  }
}

// SourceBadge shows where a wifi_history row came from. "log" = parsed
// from the controller's wireless log (authoritative). "snapshot" = caught
// by the registration-table poll. "absence" = inferred because the client
// disappeared from the registration table for several polls (safety net).
export function SourceBadge({ source }: { source: string }) {
  let label = source;
  let title = "";
  let cls = "bg-muted text-muted-foreground";
  switch (source) {
    case "log":
      label = "log";
      title = "Parsed from controller wireless log";
      cls = "bg-slate-100 text-slate-700";
      break;
    case "snapshot":
      label = "poll";
      title = "Inferred from registration-table polling";
      cls = "bg-amber-100 text-amber-700";
      break;
    case "absence":
      label = "absence";
      title = "Client missing from registration table for >5min (fallback)";
      cls = "bg-orange-100 text-orange-700";
      break;
    default:
      return null;
  }
  return <Badge title={title} className={`text-[10px] font-normal ${cls}`}>{label}</Badge>;
}

export function timeAgo(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h ${mins % 60}m ago`;
  return `${Math.floor(hrs / 24)}d ago`;
}

// formatDateTime renders an ISO date string as "dd.mm.yyyy HH:mm" in 24h
// format using the user's local timezone.
export function formatDateTime(dateStr: string): string {
  const d = new Date(dateStr);
  const dd = String(d.getDate()).padStart(2, "0");
  const mm = String(d.getMonth() + 1).padStart(2, "0");
  const yyyy = d.getFullYear();
  const hh = String(d.getHours()).padStart(2, "0");
  const min = String(d.getMinutes()).padStart(2, "0");
  return `${dd}.${mm}.${yyyy} ${hh}:${min}`;
}

export interface RoamRow {
  entry: WifiEntry;
  from: string; // previous AP, "" when it predates the loaded history window
}

// deriveRoams pairs every roam row with the AP the client was on before it.
// wifi_history roam rows only carry the destination AP, so the origin is the
// most recent earlier row for the same MAC. Returned newest first.
export function deriveRoams(history: WifiEntry[]): RoamRow[] {
  const asc = [...history].sort((a, b) => a.recorded_at.localeCompare(b.recorded_at) || a.id - b.id);
  const lastAP: Record<string, string> = {};
  const out: RoamRow[] = [];
  for (const e of asc) {
    if (e.event === "roam") out.push({ entry: e, from: lastAP[e.mac_address] || "" });
    if (e.ap_name) lastAP[e.mac_address] = e.ap_name;
  }
  return out.reverse();
}

// countBy tallies items by key and returns [key, count] pairs, largest first.
export function countBy<T>(items: T[], key: (t: T) => string): [string, number][] {
  const m = new Map<string, number>();
  for (const it of items) {
    const k = key(it);
    m.set(k, (m.get(k) || 0) + 1);
  }
  return [...m.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
}
