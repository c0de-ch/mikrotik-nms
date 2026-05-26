"use client";

import { Suspense, useEffect, useState, useMemo } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { Plus, ExternalLink, Trash2, Radar, Loader2, Check, ArrowUpDown, AlertTriangle, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
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
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { useAuth } from "@/context/auth";
import { api, type Device, type DiscoveredDevice, type DeepDiscoveredDevice } from "@/lib/api";
import { deviceStatusBadgeClass, deviceStatusLabel } from "@/lib/status";
import { toast } from "sonner";

// timeAgo formats an absolute timestamp as "5m ago" / "2h 14m ago" / "3d ago".
function timeAgo(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h ${mins % 60}m ago`;
  return `${Math.floor(hrs / 24)}d ago`;
}

type StatusFilter = "online" | "offline" | "unknown";
function isStatusFilter(v: string | null): v is StatusFilter {
  return v === "online" || v === "offline" || v === "unknown";
}

export default function DevicesPage() {
  return (
    <Suspense fallback={null}>
      <DevicesPageInner />
    </Suspense>
  );
}

function DevicesPageInner() {
  const { token, user } = useAuth();
  const searchParams = useSearchParams();
  const statusParam = searchParams.get("status");
  const statusFilter: StatusFilter | null = isStatusFilter(statusParam) ? statusParam : null;
  const [devices, setDevices] = useState<Device[]>([]);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [discoveryOpen, setDiscoveryOpen] = useState(false);
  const [scanning, setScanning] = useState(false);
  const [discovered, setDiscovered] = useState<DiscoveredDevice[]>([]);
  const [selectedMACs, setSelectedMACs] = useState<Set<string>>(new Set());
  const [addingMAC, setAddingMAC] = useState<Set<string>>(new Set());
  const [discoveryCredentials, setDiscoveryCredentials] = useState({ username: "admin", password: "", api_port: "8728" });
  const [addingSelected, setAddingSelected] = useState(false);
  const [discoveryError, setDiscoveryError] = useState<string | null>(null);
  const [deepCidr, setDeepCidr] = useState("");
  const [deepScanning, setDeepScanning] = useState(false);
  const [deepResults, setDeepResults] = useState<DeepDiscoveredDevice[]>([]);
  const [deepError, setDeepError] = useState<string | null>(null);
  const [form, setForm] = useState({ address: "", identity: "", username: "admin", password: "", api_port: "8728" });
  const router = useRouter();

  const loadDevices = () => {
    if (!token) return;
    api.devices.list(token).then(setDevices).catch(console.error);
  };

  useEffect(() => { loadDevices(); }, [token]);

  const existingAddresses = new Set(devices.map((d) => d.address));

  const handleAdd = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!token) return;
    try {
      await api.devices.create(token, {
        address: form.address,
        identity: form.identity,
        username: form.username,
        password: form.password,
        api_port: parseInt(form.api_port) || 8728,
      });
      toast.success("Device added");
      setDialogOpen(false);
      setForm({ address: "", identity: "", username: "admin", password: "", api_port: "8728" });
      loadDevices();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to add device");
    }
  };

  const handleDelete = async (id: string) => {
    if (!token) return;
    try {
      await api.devices.delete(token, id);
      toast.success("Device removed");
      loadDevices();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to delete device");
    }
  };

  const handleScan = async () => {
    if (!token) return;
    setScanning(true);
    setDiscovered([]);
    try {
      const results = await api.discovery.scan(token, 10);
      setDiscovered(results);
      if (results.length === 0) {
        toast.info("No MikroTik devices found on the network");
      } else {
        toast.success(`Found ${results.length} device(s)`);
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Discovery failed");
    } finally {
      setScanning(false);
    }
  };

  const handleAddDiscovered = async (dev: DiscoveredDevice) => {
    if (!token) return;
    if (!discoveryCredentials.password) {
      setDiscoveryError("Password is required. Enter the RouterOS API password before adding devices.");
      return;
    }
    setDiscoveryError(null);
    setAddingMAC((prev) => new Set(prev).add(dev.mac_address));
    try {
      await api.devices.create(token, {
        address: dev.ip_address,
        identity: dev.identity,
        username: discoveryCredentials.username,
        password: discoveryCredentials.password,
        api_port: parseInt(discoveryCredentials.api_port) || 8728,
      });
      toast.success(`Added ${dev.identity || dev.ip_address}`);
      loadDevices();
    } catch (err) {
      setDiscoveryError(`Failed to add ${dev.identity || dev.ip_address}: ${err instanceof Error ? err.message : "connection failed"}`);
    } finally {
      setAddingMAC((prev) => {
        const next = new Set(prev);
        next.delete(dev.mac_address);
        return next;
      });
    }
  };

  const addableDevices = discovered.filter((d) => d.ip_address && !existingAddresses.has(d.ip_address));

  type SortKey = "identity" | "ip_address" | "mac_address" | "board" | "version" | "uptime";
  const [sortKey, setSortKey] = useState<SortKey>("identity");
  const [sortAsc, setSortAsc] = useState(true);

  const toggleSort = (key: SortKey) => {
    if (sortKey === key) setSortAsc(!sortAsc);
    else { setSortKey(key); setSortAsc(true); }
  };

  const sortedDiscovered = useMemo(() => {
    return [...discovered].sort((a, b) => {
      const va = (a[sortKey] || "").toLowerCase();
      const vb = (b[sortKey] || "").toLowerCase();
      const cmp = va.localeCompare(vb);
      return sortAsc ? cmp : -cmp;
    });
  }, [discovered, sortKey, sortAsc]);

  const toggleSelectMAC = (mac: string) => {
    setSelectedMACs((prev) => {
      const next = new Set(prev);
      if (next.has(mac)) next.delete(mac); else next.add(mac);
      return next;
    });
  };

  const toggleSelectAll = () => {
    if (selectedMACs.size === addableDevices.length) {
      setSelectedMACs(new Set());
    } else {
      setSelectedMACs(new Set(addableDevices.map((d) => d.mac_address)));
    }
  };

  const handleAddSelected = async () => {
    if (!token || selectedMACs.size === 0) return;
    const toAdd = discovered.filter((d) => selectedMACs.has(d.mac_address) && d.ip_address && !existingAddresses.has(d.ip_address));
    if (toAdd.length === 0) return;
    if (!discoveryCredentials.password) {
      setDiscoveryError("Password is required. Enter the RouterOS API password before adding devices.");
      return;
    }
    setDiscoveryError(null);
    setAddingSelected(true);
    let added = 0;
    const failed: string[] = [];
    for (const dev of toAdd) {
      try {
        await api.devices.create(token, {
          address: dev.ip_address,
          identity: dev.identity,
          username: discoveryCredentials.username,
          password: discoveryCredentials.password,
          api_port: parseInt(discoveryCredentials.api_port) || 8728,
        });
        added++;
      } catch (err) {
        failed.push(`${dev.identity || dev.ip_address}: ${err instanceof Error ? err.message : "failed"}`);
      }
    }
    if (added > 0) toast.success(`Added ${added} device(s)`);
    if (failed.length > 0) setDiscoveryError(`Failed to add ${failed.length} device(s):\n${failed.join("\n")}`);
    loadDevices();
    setSelectedMACs(new Set());
    setAddingSelected(false);
  };

  // Derive a sensible default CIDR from the first managed device's address
  // (propose its /24). The user can edit it before scanning.
  const defaultCidr = useMemo(() => {
    const ipv4 = devices.map((d) => d.address).find((a) => /^\d+\.\d+\.\d+\.\d+$/.test(a));
    if (!ipv4) return "";
    const parts = ipv4.split(".");
    return `${parts[0]}.${parts[1]}.${parts[2]}.0/24`;
  }, [devices]);

  // Prefill the CIDR input once a default is available (don't clobber edits).
  useEffect(() => {
    if (defaultCidr && !deepCidr) setDeepCidr(defaultCidr);
  }, [defaultCidr, deepCidr]);

  const handleDeepScan = async () => {
    if (!token) return;
    setDeepScanning(true);
    setDeepError(null);
    setDeepResults([]);
    try {
      const results = await api.discovery.deep(token, deepCidr.trim() || undefined);
      setDeepResults(results);
      if (results.length === 0) {
        toast.info("No unmanaged devices found");
      } else {
        toast.success(`Found ${results.length} device(s)`);
      }
    } catch (err) {
      const msg = err instanceof Error ? err.message : "Deep scan failed";
      setDeepError(msg);
      toast.error(msg);
    } finally {
      setDeepScanning(false);
    }
  };

  // Open the Add Device form prefilled from a deep-scan result so the user can
  // enter credentials and enroll it.
  const handleEnrollDeep = (dev: DeepDiscoveredDevice) => {
    setForm({ address: dev.address, identity: dev.identity || "", username: "admin", password: "", api_port: "8728" });
    setDiscoveryOpen(false);
    setDialogOpen(true);
  };

  const addableDeepResults = useMemo(() => {
    const managed = new Set(devices.map((d) => d.address));
    return deepResults.filter((d) => !managed.has(d.address));
  }, [deepResults, devices]);

  const isAdmin = user?.role === "admin";

  const visibleDevices = useMemo(
    () => (statusFilter ? devices.filter((d) => d.status === statusFilter) : devices),
    [devices, statusFilter],
  );

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">Devices</h1>
        {isAdmin && (
          <div className="flex gap-2">
            <Dialog open={discoveryOpen} onOpenChange={setDiscoveryOpen}>
              <DialogTrigger render={<Button variant="outline" />}>
                <Radar className="mr-2 h-4 w-4" />Discover
              </DialogTrigger>
              <DialogContent className="sm:max-w-[calc(100vw-4rem)] max-h-[85vh] overflow-y-auto">
                <DialogHeader>
                  <DialogTitle>Network Discovery</DialogTitle>
                </DialogHeader>

                <Tabs defaultValue="mndp">
                  <TabsList>
                    <TabsTrigger value="mndp">MNDP scan</TabsTrigger>
                    <TabsTrigger value="deep">Deep scan</TabsTrigger>
                  </TabsList>

                  <TabsContent value="mndp" className="space-y-4 pt-2">
                <p className="text-sm text-muted-foreground">
                  Scan the local network for MikroTik devices using MNDP (MikroTik Neighbor Discovery Protocol). Only finds devices in the NMS host&apos;s broadcast domain.
                </p>

                <div className="grid grid-cols-4 gap-3 items-end">
                  <div className="space-y-1">
                    <Label className="text-xs">Username (for all)</Label>
                    <Input value={discoveryCredentials.username} onChange={(e) => setDiscoveryCredentials({ ...discoveryCredentials, username: e.target.value })} placeholder="admin" />
                  </div>
                  <div className="space-y-1">
                    <Label className="text-xs">Password (for all) *</Label>
                    <Input type="password" value={discoveryCredentials.password} onChange={(e) => { setDiscoveryCredentials({ ...discoveryCredentials, password: e.target.value }); setDiscoveryError(null); }} placeholder="required" required />
                  </div>
                  <div className="space-y-1">
                    <Label className="text-xs">API Port</Label>
                    <Input value={discoveryCredentials.api_port} onChange={(e) => setDiscoveryCredentials({ ...discoveryCredentials, api_port: e.target.value })} />
                  </div>
                  <Button onClick={handleScan} disabled={scanning}>
                    {scanning ? (
                      <><Loader2 className="mr-2 h-4 w-4 animate-spin" />Scanning...</>
                    ) : (
                      <><Radar className="mr-2 h-4 w-4" />Scan Network</>
                    )}
                  </Button>
                </div>

                {discoveryError && (
                  <div className="flex items-start gap-3 rounded-lg border-2 border-destructive bg-destructive/10 p-4">
                    <AlertTriangle className="h-5 w-5 shrink-0 text-destructive mt-0.5" />
                    <div className="flex-1">
                      <p className="font-semibold text-destructive">Error</p>
                      <p className="text-sm text-destructive whitespace-pre-wrap">{discoveryError}</p>
                    </div>
                    <button onClick={() => setDiscoveryError(null)} className="shrink-0 text-destructive hover:text-destructive/70">
                      <X className="h-4 w-4" />
                    </button>
                  </div>
                )}

                {discovered.length > 0 && (
                  <>
                    <div className="flex items-center justify-between">
                      <span className="text-sm text-muted-foreground">{discovered.length} device(s) found</span>
                      <Button onClick={handleAddSelected} disabled={addingSelected || selectedMACs.size === 0}>
                        {addingSelected ? (
                          <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                        ) : (
                          <Plus className="mr-2 h-4 w-4" />
                        )}
                        Add Selected ({selectedMACs.size})
                      </Button>
                    </div>
                    <Table className="table-fixed w-full">
                      <TableHeader>
                        <TableRow>
                          <TableHead className="w-8">
                            <input
                              type="checkbox"
                              checked={addableDevices.length > 0 && selectedMACs.size === addableDevices.length}
                              onChange={toggleSelectAll}
                              className="accent-primary"
                            />
                          </TableHead>
                          {([["identity","Identity"],["ip_address","IP"],["mac_address","MAC"],["board","Board"],["version","Ver"],["uptime","Uptime"]] as [SortKey, string][]).map(([key, label]) => (
                            <TableHead key={key} className="cursor-pointer select-none truncate" onClick={() => toggleSort(key)}>
                              <span className="inline-flex items-center gap-1">{label}<ArrowUpDown className={`h-3 w-3 shrink-0 ${sortKey === key ? "text-foreground" : "text-muted-foreground/40"}`} /></span>
                            </TableHead>
                          ))}
                          <TableHead className="w-16" />
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {sortedDiscovered.map((dev) => {
                          const alreadyAdded = existingAddresses.has(dev.ip_address);
                          const canAdd = !!dev.ip_address && !alreadyAdded;
                          return (
                            <TableRow key={dev.mac_address}>
                              <TableCell className="w-8">
                                <input
                                  type="checkbox"
                                  checked={selectedMACs.has(dev.mac_address)}
                                  onChange={() => toggleSelectMAC(dev.mac_address)}
                                  disabled={!canAdd}
                                  className="accent-primary"
                                />
                              </TableCell>
                              <TableCell className="font-medium truncate" title={dev.identity}>{dev.identity || "—"}</TableCell>
                              <TableCell className="truncate font-mono text-xs" title={dev.ip_address}>{dev.ip_address || "—"}</TableCell>
                              <TableCell className="font-mono text-xs truncate" title={dev.mac_address}>{dev.mac_address}</TableCell>
                              <TableCell className="text-xs truncate" title={dev.board}>{dev.board || "—"}</TableCell>
                              <TableCell className="text-xs truncate">{dev.version || "—"}</TableCell>
                              <TableCell className="text-xs truncate">{dev.uptime || "—"}</TableCell>
                              <TableCell>
                                {alreadyAdded ? (
                                  <Badge variant="secondary"><Check className="mr-1 h-3 w-3" />Added</Badge>
                                ) : (
                                  <Button
                                    size="sm"
                                    variant="ghost"
                                    onClick={() => handleAddDiscovered(dev)}
                                    disabled={addingMAC.has(dev.mac_address) || !dev.ip_address}
                                  >
                                    {addingMAC.has(dev.mac_address) ? (
                                      <Loader2 className="h-3 w-3 animate-spin" />
                                    ) : (
                                      <Plus className="h-3 w-3" />
                                    )}
                                  </Button>
                                )}
                              </TableCell>
                            </TableRow>
                          );
                        })}
                      </TableBody>
                    </Table>
                  </>
                )}
                  </TabsContent>

                  <TabsContent value="deep" className="space-y-4 pt-2">
                    <p className="text-sm text-muted-foreground">
                      Find devices that aren&apos;t directly adjacent: unmanaged neighbors seen by managed devices, plus an optional subnet port-scan (ports 8728 / 8729 / 8291). Subnets larger than /22 are rejected.
                    </p>

                    <div className="flex items-end gap-3">
                      <div className="flex-1 space-y-1">
                        <Label className="text-xs">Subnet CIDR (optional)</Label>
                        <Input
                          value={deepCidr}
                          onChange={(e) => setDeepCidr(e.target.value)}
                          placeholder="192.168.1.0/24"
                        />
                      </div>
                      <Button onClick={handleDeepScan} disabled={deepScanning}>
                        {deepScanning ? (
                          <><Loader2 className="mr-2 h-4 w-4 animate-spin" />Scanning...</>
                        ) : (
                          <><Radar className="mr-2 h-4 w-4" />Deep scan</>
                        )}
                      </Button>
                    </div>

                    {deepError && (
                      <div className="flex items-start gap-3 rounded-lg border-2 border-destructive bg-destructive/10 p-4">
                        <AlertTriangle className="h-5 w-5 shrink-0 text-destructive mt-0.5" />
                        <div className="flex-1">
                          <p className="font-semibold text-destructive">Error</p>
                          <p className="text-sm text-destructive whitespace-pre-wrap">{deepError}</p>
                        </div>
                        <button onClick={() => setDeepError(null)} className="shrink-0 text-destructive hover:text-destructive/70">
                          <X className="h-4 w-4" />
                        </button>
                      </div>
                    )}

                    {deepResults.length > 0 && (
                      <>
                        <span className="text-sm text-muted-foreground">
                          {deepResults.length} device(s) found · {addableDeepResults.length} not yet managed
                        </span>
                        <Table className="table-fixed w-full">
                          <TableHeader>
                            <TableRow>
                              <TableHead className="truncate">Address</TableHead>
                              <TableHead className="truncate">Identity</TableHead>
                              <TableHead className="truncate">Board / Version</TableHead>
                              <TableHead className="truncate">Source</TableHead>
                              <TableHead className="truncate">Open ports</TableHead>
                              <TableHead className="truncate">Seen from</TableHead>
                              <TableHead className="w-16" />
                            </TableRow>
                          </TableHeader>
                          <TableBody>
                            {deepResults.map((dev) => {
                              const alreadyAdded = existingAddresses.has(dev.address);
                              const sourceVariant =
                                dev.source === "both" ? "default" : dev.source === "neighbor" ? "secondary" : "outline";
                              return (
                                <TableRow key={dev.address}>
                                  <TableCell className="font-mono text-xs truncate" title={dev.address}>{dev.address}</TableCell>
                                  <TableCell className="font-medium truncate" title={dev.identity}>{dev.identity || "—"}</TableCell>
                                  <TableCell className="text-xs truncate" title={`${dev.board} ${dev.version}`}>
                                    {dev.board || dev.version ? `${dev.board || "—"}${dev.version ? ` / ${dev.version}` : ""}` : "—"}
                                  </TableCell>
                                  <TableCell>
                                    <Badge variant={sourceVariant}>{dev.source}</Badge>
                                  </TableCell>
                                  <TableCell className="font-mono text-xs truncate">{dev.open_ports?.length ? dev.open_ports.join(", ") : "—"}</TableCell>
                                  <TableCell className="text-xs truncate" title={dev.seen_from}>{dev.seen_from || "—"}</TableCell>
                                  <TableCell>
                                    {alreadyAdded ? (
                                      <Badge variant="secondary"><Check className="mr-1 h-3 w-3" />Added</Badge>
                                    ) : (
                                      <Button size="sm" variant="ghost" onClick={() => handleEnrollDeep(dev)} title="Add device">
                                        <Plus className="h-3 w-3" />
                                      </Button>
                                    )}
                                  </TableCell>
                                </TableRow>
                              );
                            })}
                          </TableBody>
                        </Table>
                      </>
                    )}
                  </TabsContent>
                </Tabs>
              </DialogContent>
            </Dialog>

            <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
              <DialogTrigger render={<Button />}>
                <Plus className="mr-2 h-4 w-4" />Add Device
              </DialogTrigger>
              <DialogContent>
                <DialogHeader>
                  <DialogTitle>Add Device</DialogTitle>
                </DialogHeader>
                <form onSubmit={handleAdd} className="space-y-4">
                  <div className="space-y-2">
                    <Label>IP Address</Label>
                    <Input value={form.address} onChange={(e) => setForm({ ...form, address: e.target.value })} required placeholder="192.168.1.1" />
                  </div>
                  <div className="space-y-2">
                    <Label>Identity (optional)</Label>
                    <Input value={form.identity} onChange={(e) => setForm({ ...form, identity: e.target.value })} placeholder="core-router" />
                  </div>
                  <div className="grid grid-cols-2 gap-4">
                    <div className="space-y-2">
                      <Label>Username</Label>
                      <Input value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} />
                    </div>
                    <div className="space-y-2">
                      <Label>Password</Label>
                      <Input type="password" value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} />
                    </div>
                  </div>
                  <div className="space-y-2">
                    <Label>API Port</Label>
                    <Input value={form.api_port} onChange={(e) => setForm({ ...form, api_port: e.target.value })} />
                  </div>
                  <Button type="submit" className="w-full">Add Device</Button>
                </form>
              </DialogContent>
            </Dialog>
          </div>
        )}
      </div>

      {statusFilter && (
        <div className="flex items-center gap-2">
          <span className="text-sm text-muted-foreground">Filter:</span>
          <Badge variant="secondary" className="gap-1.5">
            status: {deviceStatusLabel(statusFilter)}
            <button
              onClick={() => router.push("/devices")}
              className="ml-1 rounded-sm hover:bg-foreground/10"
              title="Clear filter"
            >
              <X className="h-3 w-3" />
            </button>
          </Badge>
          <span className="text-sm text-muted-foreground">
            {visibleDevices.length} of {devices.length}
          </span>
        </div>
      )}

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Status</TableHead>
            <TableHead>Identity</TableHead>
            <TableHead>Address</TableHead>
            <TableHead>Model</TableHead>
            <TableHead>Version</TableHead>
            <TableHead>CPU</TableHead>
            <TableHead>Uptime</TableHead>
            <TableHead>Last seen</TableHead>
            <TableHead className="w-[100px]">Actions</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {visibleDevices.map((device) => (
            <TableRow key={device.id} className="cursor-pointer" onClick={() => router.push(`/devices/${device.id}`)}>
              <TableCell>
                <Badge variant="outline" className={deviceStatusBadgeClass(device.status)}>
                  {deviceStatusLabel(device.status)}
                </Badge>
              </TableCell>
              <TableCell className="font-medium">{device.identity || "—"}</TableCell>
              <TableCell>{device.address}</TableCell>
              <TableCell className="text-sm">{device.board || "—"}</TableCell>
              <TableCell className="text-sm">{device.ros_version || "—"}</TableCell>
              <TableCell className="text-sm">{device.cpu_load != null ? `${device.cpu_load}%` : "—"}</TableCell>
              <TableCell className="text-sm">{device.uptime || "—"}</TableCell>
              <TableCell
                className="text-sm text-muted-foreground"
                title={device.last_seen ? new Date(device.last_seen).toLocaleString() : ""}
              >
                {device.last_seen ? timeAgo(device.last_seen) : "—"}
              </TableCell>
              <TableCell>
                <div className="flex gap-1" onClick={(e) => e.stopPropagation()}>
                  <Button variant="ghost" size="icon" render={<a href={`http://${device.address}`} target="_blank" rel="noopener" title="Open WebFig" />}>
                    <ExternalLink className="h-4 w-4" />
                  </Button>
                  {isAdmin && (
                    <Button variant="ghost" size="icon" onClick={() => handleDelete(device.id)} title="Remove">
                      <Trash2 className="h-4 w-4 text-destructive" />
                    </Button>
                  )}
                </div>
              </TableCell>
            </TableRow>
          ))}
          {visibleDevices.length === 0 && (
            <TableRow>
              <TableCell colSpan={9} className="text-center py-8 text-muted-foreground">
                {devices.length === 0
                  ? "No devices configured — use Discover to scan the network"
                  : `No devices with status "${statusFilter ? deviceStatusLabel(statusFilter) : ""}"`}
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
    </div>
  );
}
