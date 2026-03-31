"use client";

import { useState, useMemo, useEffect, useRef } from "react";
import { Radar, Loader2, Wifi, ArrowUpDown, Monitor, X } from "lucide-react";
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
import { Separator } from "@/components/ui/separator";
import { Input } from "@/components/ui/input";
import { useAuth } from "@/context/auth";
import { api, type NetworkClient } from "@/lib/api";
import { toast } from "sonner";

type SortKey = "mac_address" | "ip_address" | "host_name" | "device_name" | "interface" | "source" | "ssid" | "signal" | "ap";

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
  const [clients, setClients] = useState<NetworkClient[]>([]);
  const [scanning, setScanning] = useState(false);
  const [scanned, setScanned] = useState(false);
  const [search, setSearch] = useState("");
  const [sortKey, setSortKey] = useState<SortKey>("ip_address");
  const [sortAsc, setSortAsc] = useState(true);
  const [tab, setTab] = useState("all");
  const [selected, setSelected] = useState<NetworkClient | null>(null);
  const [scanTime, setScanTime] = useState<string | null>(null);
  const [scanTimeout, setScanTimeout] = useState(30);
  const [scanLimit, setScanLimit] = useState(0);
  const [scanTotal, setScanTotal] = useState(0);
  const [wasLimited, setWasLimited] = useState(false);
  const [wasTimedOut, setWasTimedOut] = useState(false);

  const handleScan = async () => {
    if (!token) return;
    setScanning(true);
    setWasLimited(false);
    setWasTimedOut(false);
    try {
      const result = await api.clients.scan(token, {
        limit: scanLimit > 0 ? scanLimit : undefined,
        timeout: scanTimeout,
      });
      setClients(result.clients);
      setScanTotal(result.total);
      setWasLimited(result.limited);
      setWasTimedOut(result.timed_out);
      setScanned(true);
      setScanTime(new Date().toLocaleTimeString());
      let msg = `Found ${result.total} client(s)`;
      if (result.limited) msg += ` (showing ${result.clients.length})`;
      if (result.timed_out) msg += " (scan timed out)";
      toast.success(msg);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Scan failed");
    } finally {
      setScanning(false);
    }
  };

  // Load cached clients on mount, then auto-scan for fresh data
  const didInit = useRef(false);
  useEffect(() => {
    if (!token || didInit.current) return;
    didInit.current = true;

    // Load cached data immediately (instant, no scan delay)
    api.clients.cached(token).then((result) => {
      if (result.clients?.length > 0) {
        setClients(result.clients);
        setScanTotal(result.total);
        setScanned(true);
      }
    }).catch(() => {});

    // Auto-trigger a fresh scan in the background
    setScanning(true);
    api.clients.scan(token, { timeout: scanTimeout }).then((result) => {
      setClients(result.clients);
      setScanTotal(result.total);
      setWasLimited(result.limited);
      setWasTimedOut(result.timed_out);
      setScanned(true);
      setScanTime(new Date().toLocaleTimeString());
    }).catch(() => {}).finally(() => setScanning(false));
  }, [token, scanTimeout]);

  const toggleSort = (key: SortKey) => {
    if (sortKey === key) setSortAsc(!sortAsc);
    else { setSortKey(key); setSortAsc(true); }
  };

  const wifiClients = clients.filter((c) => c.source === "wifi" || c.ssid);
  const allClients = tab === "wifi" ? wifiClients : clients;

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
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Network Clients</h1>
          {scanTime && (
            <p className="text-xs text-muted-foreground">
              Last scan: {scanTime} — {scanTotal} total
              {wasLimited && ` (limited to ${clients.length})`}
              {wasTimedOut && " (timed out)"}
            </p>
          )}
        </div>
        <div className="flex items-end gap-3">
          <div className="space-y-1">
            <label className="text-xs text-muted-foreground">Timeout (s)</label>
            <select
              className="flex h-8 w-20 rounded-md border bg-transparent px-2 text-sm"
              value={scanTimeout}
              onChange={(e) => setScanTimeout(Number(e.target.value))}
            >
              <option value={10}>10s</option>
              <option value={30}>30s</option>
              <option value={60}>60s</option>
              <option value={120}>120s</option>
            </select>
          </div>
          <div className="space-y-1">
            <label className="text-xs text-muted-foreground">Max clients</label>
            <select
              className="flex h-8 w-24 rounded-md border bg-transparent px-2 text-sm"
              value={scanLimit}
              onChange={(e) => setScanLimit(Number(e.target.value))}
            >
              <option value={0}>All</option>
              <option value={50}>50</option>
              <option value={100}>100</option>
              <option value={250}>250</option>
              <option value={500}>500</option>
              <option value={1000}>1000</option>
            </select>
          </div>
          <Button onClick={handleScan} disabled={scanning}>
            {scanning ? (
              <><Loader2 className="mr-2 h-4 w-4 animate-spin" />Scanning ({scanTimeout}s)...</>
            ) : (
              <><Radar className="mr-2 h-4 w-4" />Scan Network</>
            )}
          </Button>
        </div>
      </div>

      {scanned && (
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
            <span className="text-xs text-muted-foreground ml-auto">{filtered.length} result(s)</span>
          </div>

          <Table>
            <TableHeader>
              <TableRow>
                <SortableHead label="IP Address" field="ip_address" />
                <SortableHead label="MAC Address" field="mac_address" />
                <SortableHead label="Hostname" field="host_name" />
                {tab === "wifi" && <SortableHead label="AP" field="ap" />}
                {tab === "wifi" && <SortableHead label="SSID" field="ssid" />}
                {tab === "wifi" && <SortableHead label="Signal" field="signal" />}
                <SortableHead label="Source" field="source" />
                <SortableHead label="Reported By" field="device_name" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.map((c, i) => (
                <TableRow
                  key={`${c.mac_address}-${i}`}
                  className="cursor-pointer hover:bg-muted/50"
                  onClick={() => setSelected(c)}
                >
                  <TableCell className="font-mono text-sm">{c.ip_address || "—"}</TableCell>
                  <TableCell className="font-mono text-xs whitespace-nowrap">{c.mac_address}</TableCell>
                  <TableCell>{c.host_name || <span className="text-muted-foreground">—</span>}</TableCell>
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
                </TableRow>
              ))}
              {filtered.length === 0 && (
                <TableRow>
                  <TableCell colSpan={tab === "wifi" ? 8 : 6} className="py-8 text-center text-muted-foreground">
                    {search ? "No clients match your search" : "No clients found"}
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </>
      )}

      {!scanned && !scanning && (
        <Card>
          <CardContent className="py-16 text-center text-muted-foreground">
            <Radar className="mx-auto mb-3 h-10 w-10" />
            <p className="text-lg font-medium mb-1">No clients discovered yet</p>
            <p className="text-sm">Scanning will start automatically, or click Scan Network above</p>
          </CardContent>
        </Card>
      )}
      {!scanned && scanning && (
        <Card>
          <CardContent className="py-16 text-center text-muted-foreground">
            <Loader2 className="mx-auto mb-3 h-10 w-10 animate-spin" />
            <p className="text-lg font-medium mb-1">Scanning network...</p>
            <p className="text-sm">Querying ARP tables, DHCP leases, and WiFi registrations from all managed devices</p>
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
                  <DetailRow label="Hostname" value={selected.host_name} />
                  <DetailRow label="DNS Name" value={selected.dns_name} />
                  <DetailRow label="Interface" value={selected.interface} />
                  <DetailRow label="Source" value={selected.source} />
                  <DetailRow label="Reported By" value={selected.device_name} />
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
