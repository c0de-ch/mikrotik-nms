"use client";

import { useEffect, useState, useMemo } from "react";
import { useRouter } from "next/navigation";
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
import { useAuth } from "@/context/auth";
import { api, type Device, type DiscoveredDevice } from "@/lib/api";
import { toast } from "sonner";

export default function DevicesPage() {
  const { token, user } = useAuth();
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

  const isAdmin = user?.role === "admin";

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
              <DialogContent className="sm:max-w-6xl max-h-[85vh] overflow-y-auto">
                <DialogHeader>
                  <DialogTitle>Network Discovery</DialogTitle>
                </DialogHeader>
                <p className="text-sm text-muted-foreground">
                  Scan the local network for MikroTik devices using MNDP (MikroTik Neighbor Discovery Protocol).
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
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead className="w-10">
                            <input
                              type="checkbox"
                              checked={addableDevices.length > 0 && selectedMACs.size === addableDevices.length}
                              onChange={toggleSelectAll}
                              className="accent-primary"
                            />
                          </TableHead>
                          {([["identity","Identity"],["ip_address","IP Address"],["mac_address","MAC Address"],["board","Board"],["version","Version"],["uptime","Uptime"]] as [SortKey, string][]).map(([key, label]) => (
                            <TableHead key={key} className="whitespace-nowrap cursor-pointer select-none" onClick={() => toggleSort(key)}>
                              <span className="inline-flex items-center gap-1">{label}<ArrowUpDown className={`h-3 w-3 ${sortKey === key ? "text-foreground" : "text-muted-foreground/40"}`} /></span>
                            </TableHead>
                          ))}
                          <TableHead className="w-[80px]" />
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {sortedDiscovered.map((dev) => {
                          const alreadyAdded = existingAddresses.has(dev.ip_address);
                          const canAdd = !!dev.ip_address && !alreadyAdded;
                          return (
                            <TableRow key={dev.mac_address}>
                              <TableCell>
                                <input
                                  type="checkbox"
                                  checked={selectedMACs.has(dev.mac_address)}
                                  onChange={() => toggleSelectMAC(dev.mac_address)}
                                  disabled={!canAdd}
                                  className="accent-primary"
                                />
                              </TableCell>
                              <TableCell className="font-medium whitespace-nowrap">{dev.identity || "—"}</TableCell>
                              <TableCell className="whitespace-nowrap">{dev.ip_address || "—"}</TableCell>
                              <TableCell className="font-mono text-xs whitespace-nowrap">{dev.mac_address}</TableCell>
                              <TableCell className="text-sm whitespace-nowrap">{dev.board || "—"}</TableCell>
                              <TableCell className="text-sm whitespace-nowrap">{dev.version || "—"}</TableCell>
                              <TableCell className="text-sm whitespace-nowrap">{dev.uptime || "—"}</TableCell>
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
            <TableHead className="w-[100px]">Actions</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {devices.map((device) => (
            <TableRow key={device.id} className="cursor-pointer" onClick={() => router.push(`/devices/${device.id}`)}>
              <TableCell>
                <Badge variant={device.status === "online" ? "default" : device.status === "offline" ? "destructive" : "secondary"}>
                  {device.status}
                </Badge>
              </TableCell>
              <TableCell className="font-medium">{device.identity || "—"}</TableCell>
              <TableCell>{device.address}</TableCell>
              <TableCell className="text-sm">{device.board || "—"}</TableCell>
              <TableCell className="text-sm">{device.ros_version || "—"}</TableCell>
              <TableCell className="text-sm">{device.cpu_load != null ? `${device.cpu_load}%` : "—"}</TableCell>
              <TableCell className="text-sm">{device.uptime || "—"}</TableCell>
              <TableCell>
                <div className="flex gap-1" onClick={(e) => e.stopPropagation()}>
                  <Button variant="ghost" size="icon" render={<a href={`winbox://${device.address}`} title="Open in WinBox" />}>
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
          {devices.length === 0 && (
            <TableRow>
              <TableCell colSpan={8} className="text-center py-8 text-muted-foreground">
                No devices configured — use Discover to scan the network
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
    </div>
  );
}
