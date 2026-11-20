"use client";

import { useState, useMemo, useEffect, useCallback, useRef } from "react";
import { Radar, Loader2, Wifi, ArrowUpDown, Monitor, RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
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
import { Input } from "@/components/ui/input";
import { useAuth } from "@/context/auth";
import { api, ApiError, type ClientRefreshStatus, type NetworkClient } from "@/lib/api";
import { useNow } from "@/hooks/use-now";
import { usePageVisible } from "@/hooks/use-websocket";
import { toast } from "sonner";

type SortKey = "mac_address" | "ip_address" | "host_name" | "device_name" | "interface" | "source" | "ssid" | "signal" | "ap" | "vendor" | "last_seen";

function timeAgo(dateStr?: string): string {
  if (!dateStr) return "—";
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h ${mins % 60}m ago`;
  return `${Math.floor(hrs / 24)}d ago`;
}

// "4:05" until a refresh is allowed again.
function countdown(ms: number): string {
  const total = Math.max(0, Math.ceil(ms / 1000));
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, "0")}`;
}

// How often the page re-reads the cache: while a refresh runs, and otherwise
// to follow the scheduled discovery.
const REFRESH_POLL_MS = 3_000;
const CACHE_POLL_MS = 60_000;

function formatRate(rate?: string): string | undefined {
  if (!rate) return undefined;
  const n = parseInt(rate);
  if (isNaN(n)) return rate;
  if (n >= 1e9) return `${(n / 1e9).toFixed(1)} Gbps`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(0)} Mbps`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(0)} Kbps`;
  return `${n} bps`;
}

function DetailRow({ label, value }: { label: string; value?: string | null }) {
  if (!value) return null;
  return (
    <div className="flex justify-between py-1.5">
      <span className="text-muted-foreground text-sm">{label}</span>
      <span className="text-sm font-medium">{value}</span>
    </div>
  );
}

export default function ClientsPage() {
  const { token } = useAuth();
  const now = useNow();
  const pageVisible = usePageVisible();
  const [clients, setClients] = useState<NetworkClient[]>([]);
  // Whether the cache has been read at least once (it may be empty).
  const [loaded, setLoaded] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [updatedAt, setUpdatedAt] = useState<string | null>(null);
  const [refresh, setRefresh] = useState<ClientRefreshStatus | null>(null);
  // The click is on its way to the server: bridges the moment before the
  // list reports the refresh as running.
  const [starting, setStarting] = useState(false);
  const [search, setSearch] = useState("");
  const [sortKey, setSortKey] = useState<SortKey>("ip_address");
  const [sortAsc, setSortAsc] = useState(true);
  const [tab, setTab] = useState("all");
  const [showInactive, setShowInactive] = useState(false);
  const [selected, setSelected] = useState<NetworkClient | null>(null);

  // The list is the cache the scheduled discovery keeps: reading it costs
  // the devices nothing, so the page simply re-reads it.
  const wasRunning = useRef(false);
  const load = useCallback(async () => {
    if (!token) return;
    try {
      const result = await api.clients.cached(token);
      setClients(result.clients ?? []);
      setUpdatedAt(result.updated_at ?? null);
      setRefresh(result.refresh ?? null);
      setLoaded(true);
      setLoadError(null);
      const running = !!result.refresh?.running;
      if (wasRunning.current && !running && result.refresh) {
        const r = result.refresh;
        toast.success(
          `Refreshed: ${r.clients} client(s) from ${r.devices_read} device(s)` +
            (r.timed_out ? " (some devices did not answer in time)" : ""),
        );
      }
      wasRunning.current = running;
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : "Failed to load clients");
    }
  }, [token]);

  const running = !!refresh?.running || starting;
  useEffect(() => {
    if (!token || !pageVisible) return;
    // Deferred a tick so the effect itself never sets state.
    const first = setTimeout(load, 0);
    const timer = setInterval(load, running ? REFRESH_POLL_MS : CACHE_POLL_MS);
    return () => {
      clearTimeout(first);
      clearInterval(timer);
    };
  }, [token, pageVisible, running, load]);

  const handleRefresh = async () => {
    if (!token || running) return;
    setStarting(true);
    try {
      const result = await api.clients.refresh(token);
      setRefresh(result.refresh);
      wasRunning.current = result.refresh.running;
    } catch (err) {
      // 409 (one is running) and 429 (too soon) are answers, not failures:
      // the list says which, and until when.
      if (err instanceof ApiError && (err.status === 409 || err.status === 429)) {
        toast.info(err.message);
      } else {
        toast.error(err instanceof Error ? err.message : "Refresh failed");
      }
      await load();
    } finally {
      setStarting(false);
    }
  };

  // The server's limit: one refresh per min_interval_seconds, fleet-wide.
  const nextAllowedMs = refresh?.next_allowed_at ? Date.parse(refresh.next_allowed_at) : 0;
  const waitMs = now > 0 && nextAllowedMs > now ? nextAllowedMs - now : 0;
  const limitMinutes = Math.round((refresh?.min_interval_seconds ?? 300) / 60);
  // Scheduled reads and refreshes re-stamp the clients they saw, so the
  // cache is as old as its newest entry. `now` re-renders the label.
  const updatedLabel = updatedAt && now > 0 ? timeAgo(updatedAt) : null;

  const toggleSort = (key: SortKey) => {
    if (sortKey === key) setSortAsc(!sortAsc);
    else { setSortKey(key); setSortAsc(true); }
  };

  // A client is "inactive" once its cache entry hasn't been refreshed within the
  // backend's inactivity window (e.g. a device that left, or a reservation on a
  // now-disabled DHCP server). Hidden by default; reveal them with the toggle.
  const inactiveCount = useMemo(() => clients.filter((c) => c.active === false).length, [clients]);
  const visible = showInactive ? clients : clients.filter((c) => c.active !== false);

  const wifiClients = visible.filter((c) => c.source === "wifi" || c.ssid);
  const allClients = tab === "wifi" ? wifiClients : visible;

  const filtered = useMemo(() => {
    const q = search.toLowerCase();
    const list = q
      ? allClients.filter((c) =>
          c.mac_address?.toLowerCase().includes(q) ||
          c.ip_address?.toLowerCase().includes(q) ||
          c.host_name?.toLowerCase().includes(q) ||
          c.device_name?.toLowerCase().includes(q) ||
          c.ssid?.toLowerCase().includes(q) ||
          c.ap?.toLowerCase().includes(q)
        )
      : allClients;

    return [...list].sort((a, b) => {
      const va = (String((a as unknown as Record<string, string>)[sortKey] ?? "")).toLowerCase();
      const vb = (String((b as unknown as Record<string, string>)[sortKey] ?? "")).toLowerCase();
      const cmp = va.localeCompare(vb);
      return sortAsc ? cmp : -cmp;
    });
  }, [allClients, search, sortKey, sortAsc]);

  const SortableHead = ({ label, field }: { label: string; field: SortKey }) => (
    <TableHead className="cursor-pointer select-none whitespace-nowrap" onClick={() => toggleSort(field)}>
      <span className="inline-flex items-center gap-1">
        {label}
        <ArrowUpDown className={`h-3 w-3 ${sortKey === field ? "text-foreground" : "text-muted-foreground/40"}`} />
      </span>
    </TableHead>
  );

  const signalBadge = (signal?: string) => {
    if (!signal) return null;
    const val = parseInt(signal);
    const color = val > -60 ? "text-green-600" : val > -75 ? "text-yellow-600" : "text-red-600";
    const label = val > -60 ? "Good" : val > -75 ? "Fair" : "Poor";
    return <span className={color}>{signal} ({label})</span>;
  };

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold">Network Clients</h1>
          <p className="text-xs text-muted-foreground">
            {loaded
              ? updatedLabel
                ? `From the discovery cache, updated ${updatedLabel}`
                : "From the discovery cache"
              : "Loading the discovery cache…"}
            {loadError && <span className="ml-2 text-destructive">{loadError}</span>}
          </p>
        </div>
        {refresh?.available && (
          <div className="flex flex-col items-end gap-1">
            <Button
              onClick={handleRefresh}
              disabled={running || waitMs > 0}
              title={`Reads the ARP, lease and registration tables of every device now. Limited to one refresh every ${limitMinutes} min.`}
            >
              {running ? (
                <><Loader2 className="mr-2 h-4 w-4 animate-spin" />Refreshing…</>
              ) : waitMs > 0 ? (
                <><RefreshCw className="mr-2 h-4 w-4" />Refresh in {countdown(waitMs)}</>
              ) : (
                <><RefreshCw className="mr-2 h-4 w-4" />Refresh</>
              )}
            </Button>
            {waitMs > 0 && !running && (
              <span className="text-xs text-muted-foreground">One refresh every {limitMinutes} min keeps the devices idle</span>
            )}
          </div>
        )}
      </div>

      {loaded && clients.length > 0 && (
        <>
          <div className="grid gap-4 md:grid-cols-4">
            <Card>
              <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Total Clients</CardTitle></CardHeader>
              <CardContent><div className="text-2xl font-bold">{clients.length}</div></CardContent>
            </Card>
            <Card>
              <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">WiFi Clients</CardTitle></CardHeader>
              <CardContent><div className="text-2xl font-bold">{wifiClients.length}</div></CardContent>
            </Card>
            <Card>
              <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">With Hostname</CardTitle></CardHeader>
              <CardContent><div className="text-2xl font-bold">{clients.filter((c) => c.host_name).length}</div></CardContent>
            </Card>
            <Card>
              <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Unique Devices</CardTitle></CardHeader>
              <CardContent><div className="text-2xl font-bold">{new Set(clients.map((c) => c.device_name)).size}</div></CardContent>
            </Card>
          </div>

          <div className="flex items-center gap-4">
            <Tabs value={tab} onValueChange={setTab}>
              <TabsList>
                <TabsTrigger value="all">
                  <Monitor className="mr-1.5 h-3.5 w-3.5" />All ({clients.length})
                </TabsTrigger>
                <TabsTrigger value="wifi">
                  <Wifi className="mr-1.5 h-3.5 w-3.5" />WiFi ({wifiClients.length})
                </TabsTrigger>
              </TabsList>
            </Tabs>
            <Input
              placeholder="Search MAC, IP, hostname, SSID, AP..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="max-w-sm"
            />
            <div className="ml-auto flex items-center gap-3">
              {inactiveCount > 0 && (
                <button
                  type="button"
                  onClick={() => setShowInactive((v) => !v)}
                  className="text-xs text-muted-foreground underline-offset-2 hover:underline"
                >
                  {showInactive ? "Hide" : "Show"} inactive ({inactiveCount})
                </button>
              )}
              <span className="text-xs text-muted-foreground">{filtered.length} result(s)</span>
            </div>
          </div>

          <Table>
            <TableHeader>
              <TableRow>
                <SortableHead label="IP Address" field="ip_address" />
                <SortableHead label="MAC Address" field="mac_address" />
                <SortableHead label="Hostname" field="host_name" />
                <SortableHead label="Vendor" field="vendor" />
                {tab === "wifi" && <SortableHead label="AP" field="ap" />}
                {tab === "wifi" && <SortableHead label="SSID" field="ssid" />}
                {tab === "wifi" && <SortableHead label="Signal" field="signal" />}
                <SortableHead label="Source" field="source" />
                <SortableHead label="Reported By" field="device_name" />
                <SortableHead label="Last seen" field="last_seen" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.map((c, i) => (
                <TableRow
                  key={`${c.mac_address}-${i}`}
                  className={`cursor-pointer hover:bg-muted/50${c.active === false ? " opacity-50" : ""}`}
                  onClick={() => setSelected(c)}
                >
                  <TableCell className="font-mono text-sm">{c.ip_address || "—"}</TableCell>
                  <TableCell className="font-mono text-xs whitespace-nowrap">{c.mac_address}</TableCell>
                  <TableCell>
                    {c.host_name || c.dns_name || <span className="text-muted-foreground">—</span>}
                    {c.active === false && (
                      <Badge variant="outline" className="ml-2 text-[10px] text-muted-foreground">
                        inactive
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-sm">
                    {c.vendor ? (
                      <span className="text-muted-foreground">{c.vendor}</span>
                    ) : c.randomized ? (
                      <Badge variant="outline" className="text-[10px] text-muted-foreground">Private MAC</Badge>
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  {tab === "wifi" && <TableCell className="text-sm">{c.ap || "—"}</TableCell>}
                  {tab === "wifi" && <TableCell className="text-sm">{c.ssid || "—"}</TableCell>}
                  {tab === "wifi" && (
                    <TableCell className="text-sm whitespace-nowrap">{signalBadge(c.signal) || "—"}</TableCell>
                  )}
                  <TableCell>
                    <Badge variant={c.source === "wifi" ? "default" : c.source === "dhcp" ? "secondary" : "outline"}>
                      {c.source}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-sm">{c.device_name}</TableCell>
                  <TableCell
                    className="whitespace-nowrap text-xs text-muted-foreground"
                    title={c.last_seen ? new Date(c.last_seen).toLocaleString() : ""}
                  >
                    {timeAgo(c.last_seen)}
                  </TableCell>
                </TableRow>
              ))}
              {filtered.length === 0 && (
                <TableRow>
                  <TableCell colSpan={tab === "wifi" ? 10 : 7} className="py-8 text-center text-muted-foreground">
                    {search ? "No clients match your search" : "No clients found"}
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </>
      )}

      {loaded && clients.length === 0 && (
        <Card>
          <CardContent className="py-16 text-center text-muted-foreground">
            <Radar className="mx-auto mb-3 h-10 w-10" />
            <p className="text-lg font-medium mb-1">No clients discovered yet</p>
            <p className="text-sm">
              The scheduled discovery fills this list within a few minutes of the first device coming online
              {refresh?.available ? " — or use Refresh to read the devices now" : ""}
            </p>
          </CardContent>
        </Card>
      )}
      {!loaded && !loadError && (
        <Card>
          <CardContent className="py-16 text-center text-muted-foreground">
            <Loader2 className="mx-auto mb-3 h-10 w-10 animate-spin" />
            <p className="text-lg font-medium">Loading clients…</p>
          </CardContent>
        </Card>
      )}

      {/* Client Detail Modal */}
      <Dialog open={!!selected} onOpenChange={(open) => !open && setSelected(null)}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{selected?.host_name || selected?.ip_address || selected?.mac_address}</DialogTitle>
          </DialogHeader>
          {selected && (
            <div className="space-y-4">
              <div>
                <h3 className="text-xs font-semibold uppercase text-muted-foreground mb-1">Network</h3>
                <div className="rounded-lg border p-3">
                  <DetailRow label="IP Address" value={selected.ip_address} />
                  <DetailRow label="MAC Address" value={selected.mac_address} />
                  <DetailRow label="Vendor" value={selected.vendor || (selected.randomized ? "Private / randomized MAC" : undefined)} />
                  <DetailRow label="Hostname" value={selected.host_name} />
                  <DetailRow label="DNS Name" value={selected.dns_name} />
                  <DetailRow label="Interface" value={selected.interface} />
                  <DetailRow label="Source" value={selected.source} />
                  <DetailRow label="Reported By" value={selected.device_name} />
                  <DetailRow label="Status" value={selected.active === false ? "Inactive" : "Active"} />
                  {selected.last_seen && (
                    <DetailRow label="Last seen" value={new Date(selected.last_seen).toLocaleString()} />
                  )}
                </div>
              </div>

              {(selected.ssid || selected.ap || selected.signal) && (
                <div>
                  <h3 className="text-xs font-semibold uppercase text-muted-foreground mb-1">Wireless</h3>
                  <div className="rounded-lg border p-3">
                    <DetailRow label="Access Point" value={selected.ap} />
                    <DetailRow label="Radio Interface" value={selected.interface} />
                    <DetailRow label="SSID" value={selected.ssid} />
                    <DetailRow label="Band" value={selected.band} />
                    <DetailRow label="Channel" value={selected.channel} />
                    <DetailRow label="Frequency" value={selected.frequency} />
                    <div className="flex justify-between py-1.5">
                      <span className="text-muted-foreground text-sm">Signal</span>
                      <span className="text-sm font-medium">{signalBadge(selected.signal) || "—"}</span>
                    </div>
                    <DetailRow label="TX Rate" value={formatRate(selected.tx_rate)} />
                    <DetailRow label="RX Rate" value={formatRate(selected.rx_rate)} />
                    <DetailRow label="Connected" value={selected.uptime} />
                    <DetailRow label="CAPsMAN Controller" value={selected.device_name} />
                  </div>
                </div>
              )}
            </div>
          )}
        </DialogContent>
      </Dialog>
    </div>
  );
}
