"use client";

import { useState, useMemo } from "react";
import { Radar, Loader2, Wifi, ArrowUpDown, Monitor } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Input } from "@/components/ui/input";
import { useAuth } from "@/context/auth";
import { api, type NetworkClient } from "@/lib/api";
import { toast } from "sonner";

type SortKey = "mac_address" | "ip_address" | "host_name" | "device_name" | "interface" | "source" | "ssid" | "signal" | "ap";

export default function ClientsPage() {
  const { token } = useAuth();
  const [clients, setClients] = useState<NetworkClient[]>([]);
  const [scanning, setScanning] = useState(false);
  const [scanned, setScanned] = useState(false);
  const [search, setSearch] = useState("");
  const [sortKey, setSortKey] = useState<SortKey>("ip_address");
  const [sortAsc, setSortAsc] = useState(true);
  const [tab, setTab] = useState("all");

  const handleScan = async () => {
    if (!token) return;
    setScanning(true);
    try {
      const results = await api.clients.scan(token);
      setClients(results);
      setScanned(true);
      toast.success(`Found ${results.length} client(s) on the network`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Scan failed");
    } finally {
      setScanning(false);
    }
  };

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

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">Network Clients</h1>
        <Button onClick={handleScan} disabled={scanning}>
          {scanning ? (
            <><Loader2 className="mr-2 h-4 w-4 animate-spin" />Scanning all devices...</>
          ) : (
            <><Radar className="mr-2 h-4 w-4" />Scan Network</>
          )}
        </Button>
      </div>

      {scanned && (
        <>
          <div className="grid gap-4 md:grid-cols-3">
            <Card>
              <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Total Clients</CardTitle></CardHeader>
              <CardContent><div className="text-2xl font-bold">{clients.length}</div></CardContent>
            </Card>
            <Card>
              <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">WiFi Clients</CardTitle></CardHeader>
              <CardContent>
                <div className="text-2xl font-bold">{wifiClients.length}</div>
              </CardContent>
            </Card>
            <Card>
              <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">With Hostname</CardTitle></CardHeader>
              <CardContent>
                <div className="text-2xl font-bold">{clients.filter((c) => c.host_name).length}</div>
              </CardContent>
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
              placeholder="Search MAC, IP, hostname, SSID..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="max-w-sm"
            />
          </div>

          {tab === "all" ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <SortableHead label="IP Address" field="ip_address" />
                  <SortableHead label="MAC Address" field="mac_address" />
                  <SortableHead label="Hostname" field="host_name" />
                  <SortableHead label="Interface" field="interface" />
                  <SortableHead label="Source" field="source" />
                  <SortableHead label="Reported By" field="device_name" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {filtered.map((c, i) => (
                  <TableRow key={`${c.mac_address}-${i}`}>
                    <TableCell className="font-mono text-sm">{c.ip_address || "—"}</TableCell>
                    <TableCell className="font-mono text-xs whitespace-nowrap">{c.mac_address}</TableCell>
                    <TableCell>{c.host_name || <span className="text-muted-foreground">—</span>}</TableCell>
                    <TableCell className="text-sm">{c.interface || "—"}</TableCell>
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
                    <TableCell colSpan={6} className="py-8 text-center text-muted-foreground">
                      {search ? "No clients match your search" : "No clients found"}
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <SortableHead label="IP Address" field="ip_address" />
                  <SortableHead label="MAC Address" field="mac_address" />
                  <SortableHead label="Hostname" field="host_name" />
                  <SortableHead label="AP" field="ap" />
                  <SortableHead label="SSID" field="ssid" />
                  <TableHead className="whitespace-nowrap">Band / Channel</TableHead>
                  <SortableHead label="Signal" field="signal" />
                  <TableHead className="whitespace-nowrap">TX / RX Rate</TableHead>
                  <TableHead>Uptime</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filtered.map((c, i) => (
                  <TableRow key={`${c.mac_address}-${i}`}>
                    <TableCell className="font-mono text-sm">{c.ip_address || "—"}</TableCell>
                    <TableCell className="font-mono text-xs whitespace-nowrap">{c.mac_address}</TableCell>
                    <TableCell>{c.host_name || <span className="text-muted-foreground">—</span>}</TableCell>
                    <TableCell className="text-sm">{c.ap || "—"}</TableCell>
                    <TableCell className="text-sm">{c.ssid || "—"}</TableCell>
                    <TableCell className="text-sm whitespace-nowrap">
                      {c.band || "—"}{c.channel ? ` / ${c.channel}` : ""}{c.frequency ? ` (${c.frequency})` : ""}
                    </TableCell>
                    <TableCell className="text-sm whitespace-nowrap">
                      {c.signal ? (
                        <span className={parseInt(c.signal) > -60 ? "text-green-600" : parseInt(c.signal) > -75 ? "text-yellow-600" : "text-red-600"}>
                          {c.signal}
                        </span>
                      ) : "—"}
                    </TableCell>
                    <TableCell className="text-xs whitespace-nowrap">
                      {c.tx_rate || "—"} / {c.rx_rate || "—"}
                    </TableCell>
                    <TableCell className="text-sm">{c.uptime || "—"}</TableCell>
                  </TableRow>
                ))}
                {filtered.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={9} className="py-8 text-center text-muted-foreground">
                      {search ? "No WiFi clients match your search" : "No WiFi clients found"}
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          )}
        </>
      )}

      {!scanned && (
        <Card>
          <CardContent className="py-16 text-center text-muted-foreground">
            <Radar className="mx-auto mb-3 h-10 w-10" />
            <p className="text-lg font-medium mb-1">Scan your network</p>
            <p className="text-sm">Queries ARP tables, DHCP leases, and CAPsMAN/WiFi registrations from all managed devices</p>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
