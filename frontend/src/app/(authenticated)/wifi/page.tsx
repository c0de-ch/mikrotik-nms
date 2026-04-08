"use client";

import { useEffect, useState, useCallback } from "react";
import { Wifi, ArrowRight, Clock, Signal, Radio, Search, ChevronDown, ChevronRight } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { useAuth } from "@/context/auth";
import { api } from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";
import { toast } from "sonner";

interface WifiEntry {
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

interface WifiEvent {
  mac: string;
  ap: string;
  prev_ap: string;
  event: string;
  signal: string;
  time: string;
}

function formatRate(rate?: string): string {
  if (!rate) return "—";
  const n = parseInt(rate);
  if (isNaN(n)) return rate;
  if (n >= 1e6) return `${(n / 1e6).toFixed(0)} Mbps`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(0)} Kbps`;
  return `${n} bps`;
}

function signalColor(signal: string): string {
  const v = parseInt(signal);
  if (v > -60) return "text-green-600";
  if (v > -75) return "text-yellow-600";
  return "text-red-600";
}

function eventBadge(event: string) {
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
function SourceBadge({ source }: { source: string }) {
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

function timeAgo(dateStr: string): string {
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
function formatDateTime(dateStr: string): string {
  const d = new Date(dateStr);
  const dd = String(d.getDate()).padStart(2, "0");
  const mm = String(d.getMonth() + 1).padStart(2, "0");
  const yyyy = d.getFullYear();
  const hh = String(d.getHours()).padStart(2, "0");
  const min = String(d.getMinutes()).padStart(2, "0");
  return `${dd}.${mm}.${yyyy} ${hh}:${min}`;
}

type MACLookupMap = Record<string, { mac_address: string; ip_address: string; host_name: string; dns_name: string }>;

function UnknownSection({ count, groups, renderGroup }: { count: number; groups: string[]; renderGroup: (g: string) => React.ReactNode }) {
  const [expanded, setExpanded] = useState(false);
  return (
    <div>
      <button
        onClick={() => setExpanded(!expanded)}
        className="flex w-full items-center gap-2 rounded-lg border border-dashed p-3 text-sm text-muted-foreground hover:bg-muted/30 transition-colors"
      >
        {expanded ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
        <span>Non-wireless / Unknown ({count} entries in {groups.length} groups)</span>
      </button>
      {expanded && (
        <div className="mt-2 space-y-4 opacity-70">
          {groups.map(renderGroup)}
        </div>
      )}
    </div>
  );
}

export default function WifiPage() {
  const { token } = useAuth();
  const [tab, setTab] = useState("live");
  const [groupBy, setGroupBy] = useState<"ap" | "ssid">("ssid");
  const [current, setCurrent] = useState<WifiEntry[]>([]);
  const [history, setHistory] = useState<WifiEntry[]>([]);
  const [liveEvents, setLiveEvents] = useState<WifiEvent[]>([]);
  const [selectedMAC, setSelectedMAC] = useState<string | null>(null);
  const [clientHistory, setClientHistory] = useState<WifiEntry[]>([]);
  const [search, setSearch] = useState("");
  const [macLookups, setMacLookups] = useState<MACLookupMap>({});

  const loadCurrent = useCallback(() => {
    if (!token) return;
    api.wifi.current(token).then((d) => setCurrent(d as WifiEntry[])).catch(console.error);
  }, [token]);

  const loadHistory = useCallback(() => {
    if (!token) return;
    api.wifi.history(token, { limit: 500 }).then((d) => setHistory(d as WifiEntry[])).catch(console.error);
  }, [token]);

  const loadMACLookups = useCallback(() => {
    if (!token) return;
    api.wifi.macLookup(token).then((d) => setMacLookups(d as MACLookupMap)).catch(console.error);
  }, [token]);

  useEffect(() => {
    loadCurrent();
    loadHistory();
    loadMACLookups();
    const interval = setInterval(() => { loadCurrent(); loadHistory(); }, 30000);
    return () => clearInterval(interval);
  }, [loadCurrent, loadHistory, loadMACLookups]);

  // Resolve a MAC to a friendly name
  const resolveName = (mac: string, entryHostname?: string) => {
    if (entryHostname) return entryHostname;
    const lookup = macLookups[mac];
    if (lookup?.host_name) return lookup.host_name;
    if (lookup?.dns_name) return lookup.dns_name;
    return "";
  };

  // Live wifi events via WebSocket
  useWebSocket("wifi.event", useCallback((data: unknown) => {
    const evt = data as WifiEvent;
    setLiveEvents((prev) => [evt, ...prev].slice(0, 100));
    if (evt.event === "roam") {
      toast.info(`${evt.mac} roamed: ${evt.prev_ap} → ${evt.ap}`);
    }
  }, []));

  const openClientHistory = async (mac: string) => {
    setSelectedMAC(mac);
    setClientHistory([]); // Clear stale data immediately
    if (!token) return;
    const entries = await api.wifi.history(token, { mac, limit: 200 }) as WifiEntry[];
    setClientHistory(entries);
  };

  // Group current clients by AP or SSID
  const groups = current.reduce<Record<string, WifiEntry[]>>((acc, c) => {
    const key = groupBy === "ssid" ? (c.ssid || "Unknown SSID") : (c.ap_name || "Unknown AP");
    if (!acc[key]) acc[key] = [];
    acc[key].push(c);
    return acc;
  }, {});
  const sortedGroups = Object.keys(groups).sort();
  const uniqueAPs = new Set(current.map((c) => c.ap_name)).size;
  const uniqueSSIDs = new Set(current.map((c) => c.ssid)).size;

  const filteredHistory = search
    ? history.filter((e) =>
        e.mac_address.toLowerCase().includes(search.toLowerCase()) ||
        e.host_name?.toLowerCase().includes(search.toLowerCase()) ||
        e.ap_name?.toLowerCase().includes(search.toLowerCase())
      )
    : history;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">WiFi Tracking</h1>
          <p className="text-sm text-muted-foreground">
            {current.length} clients · {uniqueAPs} APs · {uniqueSSIDs} networks · updates every 30s
          </p>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-4">
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Connected</CardTitle></CardHeader>
          <CardContent><div className="text-2xl font-bold">{current.length}</div></CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Access Points</CardTitle></CardHeader>
          <CardContent><div className="text-2xl font-bold">{uniqueAPs}</div></CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Roaming Events</CardTitle></CardHeader>
          <CardContent><div className="text-2xl font-bold">{history.filter((e) => e.event === "roam").length}</div></CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Live Events</CardTitle></CardHeader>
          <CardContent><div className="text-2xl font-bold">{liveEvents.length}</div></CardContent>
        </Card>
      </div>

      <div className="flex items-center justify-between">
        <Tabs value={tab} onValueChange={setTab}>
          <TabsList>
            <TabsTrigger value="live"><Wifi className="mr-1.5 h-3.5 w-3.5" />Current ({current.length})</TabsTrigger>
            <TabsTrigger value="timeline"><Clock className="mr-1.5 h-3.5 w-3.5" />Timeline</TabsTrigger>
            <TabsTrigger value="events"><Radio className="mr-1.5 h-3.5 w-3.5" />Live Events ({liveEvents.length})</TabsTrigger>
          </TabsList>
        </Tabs>
        {tab === "live" && (
          <div className="flex items-center gap-2 text-sm">
            <span className="text-muted-foreground">Group by:</span>
            <Button size="sm" variant={groupBy === "ap" ? "default" : "outline"} onClick={() => setGroupBy("ap")}>AP</Button>
            <Button size="sm" variant={groupBy === "ssid" ? "default" : "outline"} onClick={() => setGroupBy("ssid")}>Network (SSID)</Button>
          </div>
        )}
      </div>

      {/* Current view: clients grouped */}
      {tab === "live" && (() => {
        const isUnknown = (key: string) => {
          if (groupBy === "ssid") return !key || key === "Unknown SSID" || key === "";
          // For AP grouping, check if all clients in that AP have no SSID/band (likely not wireless)
          const clients = groups[key];
          return clients?.every((c) => !c.ssid && !c.band && !c.signal);
        };
        const knownGroups = sortedGroups.filter((g) => !isUnknown(g));
        const unknownGroups = sortedGroups.filter((g) => isUnknown(g));
        const unknownCount = unknownGroups.reduce((sum, g) => sum + groups[g].length, 0);

        const renderGroup = (group: string) => (
          <Card key={group}>
            <CardHeader className="pb-2">
              <div className="flex items-center gap-2">
                <Wifi className="h-4 w-4 text-primary" />
                <CardTitle className="text-sm">{group}</CardTitle>
                <Badge variant="secondary">{groups[group].length} clients</Badge>
              </div>
            </CardHeader>
            <CardContent className="pt-0">
              <div className="space-y-1">
                {groups[group].map((c) => {
                  const name = resolveName(c.mac_address, c.host_name);
                  return (
                    <button
                      key={c.mac_address}
                      onClick={() => openClientHistory(c.mac_address)}
                      className="flex w-full items-center gap-3 rounded-md px-3 py-2 text-xs hover:bg-muted/50 transition-colors text-left"
                    >
                      <div className="flex-1 min-w-0">
                        <span className="font-medium">{name || c.mac_address}</span>
                        {name && <span className="text-muted-foreground ml-2 font-mono">{c.mac_address}</span>}
                      </div>
                      {groupBy === "ssid" && <span className="text-muted-foreground shrink-0">{c.ap_name}</span>}
                      {groupBy === "ap" && <span className="text-muted-foreground shrink-0">{c.ssid}</span>}
                      <span className="text-muted-foreground shrink-0">{c.band}</span>
                      {c.signal && <span className={`font-mono shrink-0 ${signalColor(c.signal)}`}>{c.signal}</span>}
                      <span className="text-muted-foreground shrink-0">{formatRate(c.tx_rate)}</span>
                    </button>
                  );
                })}
              </div>
            </CardContent>
          </Card>
        );

        return (
          <div className="space-y-4">
            {knownGroups.map(renderGroup)}
            {knownGroups.length === 0 && unknownGroups.length === 0 && (
              <Card>
                <CardContent className="py-12 text-center text-muted-foreground">
                  <Wifi className="mx-auto mb-3 h-8 w-8" />
                  <p>No WiFi clients tracked yet. Data appears after the first polling cycle (30s).</p>
                </CardContent>
              </Card>
            )}
            {unknownGroups.length > 0 && (
              <UnknownSection count={unknownCount} groups={unknownGroups} renderGroup={renderGroup} />
            )}
          </div>
        );
      })()}

      {/* Timeline view */}
      {tab === "timeline" && (
        <div className="space-y-3">
          <div className="relative w-64">
            <Search className="absolute left-2.5 top-2 h-4 w-4 text-muted-foreground" />
            <Input placeholder="Filter by MAC, name, AP..." value={search} onChange={(e) => setSearch(e.target.value)} className="pl-8" />
          </div>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Time</TableHead>
                <TableHead>Event</TableHead>
                <TableHead>Client</TableHead>
                <TableHead>AP</TableHead>
                <TableHead>SSID / Band</TableHead>
                <TableHead>Signal</TableHead>
                <TableHead>Source</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {filteredHistory.slice(0, 200).map((e) => (
                <TableRow key={e.id} className="cursor-pointer hover:bg-muted/50" onClick={() => openClientHistory(e.mac_address)}>
                  <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{timeAgo(e.recorded_at)}</TableCell>
                  <TableCell>{eventBadge(e.event)}</TableCell>
                  <TableCell>
                    <span className="font-medium text-sm">{resolveName(e.mac_address, e.host_name) || e.mac_address}</span>
                    {resolveName(e.mac_address, e.host_name) && <span className="text-xs text-muted-foreground ml-1 font-mono">{e.mac_address}</span>}
                  </TableCell>
                  <TableCell className="text-sm">{e.ap_name || "—"}</TableCell>
                  <TableCell className="text-xs">{e.ssid} {e.band && `· ${e.band}`}</TableCell>
                  <TableCell className={`text-xs font-mono ${e.signal ? signalColor(e.signal) : ""}`}>{e.signal || "—"}</TableCell>
                  <TableCell className="text-xs text-muted-foreground">{e.controller_name || "—"}</TableCell>
                </TableRow>
              ))}
              {filteredHistory.length === 0 && (
                <TableRow>
                  <TableCell colSpan={7} className="py-8 text-center text-muted-foreground">No history yet</TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
      )}

      {/* Live events feed */}
      {tab === "events" && (
        <div className="space-y-2">
          {liveEvents.length === 0 && (
            <Card>
              <CardContent className="py-12 text-center text-muted-foreground">
                <Radio className="mx-auto mb-3 h-8 w-8 animate-pulse" />
                <p>Listening for WiFi events...</p>
                <p className="text-xs mt-1">Roaming, join, and leave events appear here in real-time</p>
              </CardContent>
            </Card>
          )}
          {liveEvents.map((evt, i) => (
            <div key={i} className="flex items-center gap-3 rounded-lg border p-3 text-sm">
              <div className="shrink-0">{eventBadge(evt.event)}</div>
              <div className="flex-1 min-w-0">
                <span className="font-mono font-medium">{evt.mac}</span>
                {evt.event === "roam" && (
                  <span className="text-muted-foreground ml-2">
                    {evt.prev_ap} <ArrowRight className="inline h-3 w-3" /> {evt.ap}
                  </span>
                )}
                {evt.event === "join" && <span className="text-muted-foreground ml-2">→ {evt.ap}</span>}
                {evt.event === "leave" && <span className="text-muted-foreground ml-2">← {evt.ap}</span>}
              </div>
              {evt.signal && <span className={`font-mono text-xs ${signalColor(evt.signal)}`}>{evt.signal}</span>}
              <span className="text-xs text-muted-foreground shrink-0">{timeAgo(evt.time)}</span>
            </div>
          ))}
        </div>
      )}

      {/* Client history modal */}
      <Dialog open={!!selectedMAC} onOpenChange={(open) => !open && setSelectedMAC(null)}>
        <DialogContent className="sm:max-w-2xl max-h-[80vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Client History: {(selectedMAC && resolveName(selectedMAC)) || selectedMAC}</DialogTitle>
          </DialogHeader>
          {clientHistory.length === 0 && selectedMAC && (
            <div className="py-8 text-center text-muted-foreground">Loading history...</div>
          )}
          {clientHistory.length > 0 && (() => {
            const latest = clientHistory[0];
            const lookup = macLookups[selectedMAC || ""];
            const hostname = latest.host_name || lookup?.host_name || lookup?.dns_name || "";
            const ip = latest.ip_address || lookup?.ip_address || "";
            // Find most recent entry with SSID/band (may not be the latest if it's a "leave")
            const withSSID = clientHistory.find((e) => e.ssid);
            const withBand = clientHistory.find((e) => e.band);
            const withSignal = clientHistory.find((e) => e.signal);
            return (
            <div className="space-y-3">
              <div className="rounded-lg border p-3 text-sm space-y-1">
                {hostname && <div className="flex justify-between"><span className="text-muted-foreground">Hostname</span><span className="font-medium">{hostname}</span></div>}
                {ip && <div className="flex justify-between"><span className="text-muted-foreground">IP Address</span><span className="font-mono">{ip}</span></div>}
                <div className="flex justify-between"><span className="text-muted-foreground">MAC Address</span><span className="font-mono">{selectedMAC}</span></div>
                <div className="flex justify-between"><span className="text-muted-foreground">Current AP</span><span className="font-medium">{latest.ap_name || "—"}</span></div>
                {(withSSID?.ssid) && <div className="flex justify-between"><span className="text-muted-foreground">SSID / Network</span><span>{withSSID.ssid}</span></div>}
                {(withBand?.band) && <div className="flex justify-between"><span className="text-muted-foreground">Band</span><span>{withBand.band}</span></div>}
                {(latest.channel) && <div className="flex justify-between"><span className="text-muted-foreground">Channel</span><span>{latest.channel}</span></div>}
                {(withSignal?.signal) && <div className="flex justify-between"><span className="text-muted-foreground">Signal</span><span className={signalColor(withSignal.signal)}>{withSignal.signal}</span></div>}
                {(latest.tx_rate) && <div className="flex justify-between"><span className="text-muted-foreground">TX / RX Rate</span><span>{formatRate(latest.tx_rate)} / {formatRate(latest.rx_rate)}</span></div>}
                {latest.controller_name && <div className="flex justify-between"><span className="text-muted-foreground">Source Controller</span><span>{latest.controller_name}</span></div>}
              </div>

              {/* Roaming timeline */}
              <h3 className="font-medium text-sm">Movement Timeline</h3>
              <div className="space-y-0">
                {clientHistory.filter((e) => e.event !== "seen").map((e) => {
                  const detail = [
                    formatDateTime(e.recorded_at),
                    e.band,
                    formatRate(e.tx_rate),
                  ].filter(Boolean).join(" · ");
                  const trailing: string[] = [];
                  if (e.controller_name) trailing.push(`via ${e.controller_name}`);
                  if (e.event === "leave" && e.reason) trailing.push(e.reason);
                  return (
                  <div key={e.id} className="flex items-center gap-3 border-l-2 border-muted pl-4 py-2 ml-2 relative">
                    <div className="absolute -left-1.5 h-3 w-3 rounded-full bg-background border-2 border-primary" />
                    <div className="flex-1">
                      <div className="flex items-center gap-2">
                        {eventBadge(e.event)}
                        <span className="font-medium text-sm">{e.ap_name}</span>
                        {e.signal && <span className={`text-xs font-mono ${signalColor(e.signal)}`}>{e.signal}</span>}
                        {e.source && <SourceBadge source={e.source} />}
                      </div>
                      <p className="text-xs text-muted-foreground">{detail}{trailing.length > 0 ? ` · ${trailing.join(" · ")}` : ""}</p>
                    </div>
                  </div>
                  );
                })}
              </div>
              {clientHistory.filter((e) => e.event !== "seen").length === 0 && (
                <p className="text-sm text-muted-foreground">No roaming events recorded yet — only polling data.</p>
              )}
            </div>
          ); })()}
        </DialogContent>
      </Dialog>
    </div>
  );
}
