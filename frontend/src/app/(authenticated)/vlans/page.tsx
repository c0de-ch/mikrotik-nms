"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { Network, Pencil } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useAuth } from "@/context/auth";
import { api, type BridgeVLAN, type VLANLabel, type Device } from "@/lib/api";
import { toast } from "sonner";

// Expand a vlan-ids field like "28,78" or "28" into individual numeric IDs.
function expandVLANIDs(vlanIDs: string): number[] {
  return vlanIDs
    .split(",")
    .map((s) => parseInt(s.trim(), 10))
    .filter((n) => !Number.isNaN(n));
}

function portList(s: string): string[] {
  return s
    .split(",")
    .map((p) => p.trim())
    .filter((p) => p !== "");
}

type Membership = "tagged" | "untagged" | "none";

export default function VLANsPage() {
  const { token, user } = useAuth();
  const isAdmin = user?.role === "admin";

  const [vlans, setVlans] = useState<BridgeVLAN[]>([]);
  const [labels, setLabels] = useState<VLANLabel[]>([]);
  const [devices, setDevices] = useState<Device[]>([]);
  const [selectedVLAN, setSelectedVLAN] = useState<number | null>(null);

  const [editing, setEditing] = useState<number | null>(null);
  const [editForm, setEditForm] = useState({ name: "", purpose: "", color: "" });

  const load = useCallback(() => {
    if (!token) return;
    Promise.all([
      api.vlans.list(token),
      api.vlans.labels(token),
      api.devices.list(token),
    ])
      .then(([v, l, d]) => {
        setVlans(v ?? []);
        setLabels(l ?? []);
        setDevices(d ?? []);
      })
      .catch(console.error);
  }, [token]);

  useEffect(() => {
    load();
    const t = setInterval(load, 30_000);
    return () => clearInterval(t);
  }, [load]);

  const labelByVLAN = useMemo(() => {
    const m = new Map<number, VLANLabel>();
    for (const l of labels) m.set(l.vlan_id, l);
    return m;
  }, [labels]);

  // Device columns: only devices that actually carry at least one VLAN row,
  // ordered by identity. Fall back to all known devices' identity for names.
  const deviceName = useCallback(
    (id: string) => {
      const d = devices.find((dev) => dev.id === id);
      if (d) return d.identity || d.address || id;
      const v = vlans.find((row) => row.device_id === id);
      return v?.device_name || id;
    },
    [devices, vlans],
  );

  const deviceColumns = useMemo(() => {
    const ids = new Set<string>();
    for (const v of vlans) ids.add(v.device_id);
    return Array.from(ids).sort((a, b) =>
      deviceName(a).localeCompare(deviceName(b)),
    );
  }, [vlans, deviceName]);

  // Distinct VLAN IDs across the fleet, sorted ascending.
  const vlanIDs = useMemo(() => {
    const ids = new Set<number>();
    for (const v of vlans) {
      for (const id of expandVLANIDs(v.vlan_ids)) ids.add(id);
    }
    return Array.from(ids).sort((a, b) => a - b);
  }, [vlans]);

  // matrix[vlanId][deviceId] => membership for the matrix cells.
  const matrix = useMemo(() => {
    const m = new Map<number, Map<string, Membership>>();
    for (const v of vlans) {
      const tagged = new Set(portList(v.tagged).concat(portList(v.current_tagged)));
      const untagged = new Set(portList(v.untagged).concat(portList(v.current_untagged)));
      let membership: Membership = "none";
      if (untagged.size > 0) membership = "untagged";
      else if (tagged.size > 0) membership = "tagged";
      else membership = "tagged"; // present on the bridge with no listed ports
      for (const id of expandVLANIDs(v.vlan_ids)) {
        if (!m.has(id)) m.set(id, new Map());
        const row = m.get(id)!;
        const prev = row.get(v.device_id);
        // Prefer untagged over tagged if a device has both for the same VLAN.
        if (prev === "untagged") continue;
        row.set(v.device_id, membership);
      }
    }
    return m;
  }, [vlans]);

  // Per-device tagged/untagged port detail for a selected VLAN.
  const selectedDetail = useMemo(() => {
    if (selectedVLAN === null) return [];
    const rows: {
      deviceId: string;
      bridge: string;
      tagged: string[];
      untagged: string[];
      comment: string;
    }[] = [];
    for (const v of vlans) {
      if (!expandVLANIDs(v.vlan_ids).includes(selectedVLAN)) continue;
      const tagged = Array.from(
        new Set(portList(v.tagged).concat(portList(v.current_tagged))),
      );
      const untagged = Array.from(
        new Set(portList(v.untagged).concat(portList(v.current_untagged))),
      );
      rows.push({
        deviceId: v.device_id,
        bridge: v.bridge_name,
        tagged,
        untagged,
        comment: v.comment,
      });
    }
    return rows.sort((a, b) =>
      deviceName(a.deviceId).localeCompare(deviceName(b.deviceId)),
    );
  }, [selectedVLAN, vlans, deviceName]);

  const openEdit = (vlanId: number) => {
    const l = labelByVLAN.get(vlanId);
    setEditForm({
      name: l?.name ?? "",
      purpose: l?.purpose ?? "",
      color: l?.color ?? "",
    });
    setEditing(vlanId);
  };

  const saveLabel = async () => {
    if (!token || editing === null) return;
    try {
      await api.vlans.updateLabel(token, {
        vlan_id: editing,
        name: editForm.name,
        purpose: editForm.purpose,
        color: editForm.color,
      });
      toast.success(`VLAN ${editing} label saved`);
      setEditing(null);
      load();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Failed to save label");
    }
  };

  function cellGlyph(m: Membership | undefined) {
    if (m === "tagged") return <span className="font-semibold text-blue-600">T</span>;
    if (m === "untagged") return <span className="font-semibold text-green-600">U</span>;
    return <span className="text-muted-foreground">–</span>;
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-2xl font-bold">VLANs</h1>
        <p className="text-sm text-muted-foreground">
          Bridge VLAN membership across the fleet. <span className="font-semibold text-blue-600">T</span> = tagged,{" "}
          <span className="font-semibold text-green-600">U</span> = untagged, – = not present.
        </p>
      </div>

      <div className="grid gap-4 md:grid-cols-3">
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">VLANs</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            <Network className="h-5 w-5 text-muted-foreground" />
            <div className="text-2xl font-bold">{vlanIDs.length}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Devices with VLANs</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            <div className="text-2xl font-bold">{deviceColumns.length}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Labelled</CardTitle></CardHeader>
          <CardContent className="flex items-center gap-2">
            <div className="text-2xl font-bold">{labels.filter((l) => l.name).length}</div>
          </CardContent>
        </Card>
      </div>

      {/* Matrix */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">VLAN Matrix</CardTitle>
          <p className="text-xs text-muted-foreground">
            Each cell shows whether the VLAN is tagged / untagged on the device. Click a VLAN ID to inspect its ports.
          </p>
        </CardHeader>
        <CardContent>
          {vlanIDs.length === 0 ? (
            <p className="py-12 text-center text-sm text-muted-foreground">
              No bridge VLANs discovered yet. Data appears after the first network-health poll cycle (60s),
              once devices with bridge VLAN filtering are polled.
            </p>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="sticky left-0 bg-background">VLAN</TableHead>
                    <TableHead>Label</TableHead>
                    {deviceColumns.map((id) => (
                      <TableHead key={id} className="text-center text-xs whitespace-nowrap">
                        {deviceName(id)}
                      </TableHead>
                    ))}
                    {isAdmin && <TableHead className="text-right">Edit</TableHead>}
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {vlanIDs.map((vid) => {
                    const label = labelByVLAN.get(vid);
                    const row = matrix.get(vid);
                    const selected = selectedVLAN === vid;
                    return (
                      <TableRow
                        key={vid}
                        className={selected ? "bg-muted/50" : "cursor-pointer hover:bg-muted/30"}
                        onClick={() => setSelectedVLAN(selected ? null : vid)}
                      >
                        <TableCell className="sticky left-0 bg-background font-mono font-semibold">
                          <Badge
                            className="font-mono"
                            style={label?.color ? { backgroundColor: label.color, color: "#fff" } : undefined}
                            variant={label?.color ? undefined : "secondary"}
                          >
                            {vid}
                          </Badge>
                        </TableCell>
                        <TableCell className="text-xs">
                          {label?.name ? (
                            <span>
                              <span className="font-medium">{label.name}</span>
                              {label.purpose && (
                                <span className="ml-1 text-muted-foreground">— {label.purpose}</span>
                              )}
                            </span>
                          ) : (
                            <span className="text-muted-foreground">—</span>
                          )}
                        </TableCell>
                        {deviceColumns.map((id) => (
                          <TableCell key={id} className="text-center font-mono text-sm">
                            {cellGlyph(row?.get(id))}
                          </TableCell>
                        ))}
                        {isAdmin && (
                          <TableCell className="text-right">
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              onClick={(e) => {
                                e.stopPropagation();
                                openEdit(vid);
                              }}
                            >
                              <Pencil className="h-3.5 w-3.5" />
                            </Button>
                          </TableCell>
                        )}
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>

      {/* Per-VLAN detail */}
      {selectedVLAN !== null && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">
              VLAN {selectedVLAN}
              {labelByVLAN.get(selectedVLAN)?.name && (
                <span className="ml-2 text-muted-foreground font-normal">
                  {labelByVLAN.get(selectedVLAN)?.name}
                </span>
              )}
            </CardTitle>
            <p className="text-xs text-muted-foreground">Tagged / untagged ports per device.</p>
          </CardHeader>
          <CardContent>
            {selectedDetail.length === 0 ? (
              <p className="py-6 text-center text-sm text-muted-foreground">No port detail for this VLAN.</p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Device</TableHead>
                    <TableHead>Bridge</TableHead>
                    <TableHead>Tagged ports</TableHead>
                    <TableHead>Untagged ports</TableHead>
                    <TableHead>Comment</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {selectedDetail.map((d, i) => (
                    <TableRow key={`${d.deviceId}-${d.bridge}-${i}`}>
                      <TableCell className="text-xs">{deviceName(d.deviceId)}</TableCell>
                      <TableCell className="font-mono text-xs">{d.bridge || "—"}</TableCell>
                      <TableCell className="font-mono text-xs text-blue-600">
                        {d.tagged.length > 0 ? d.tagged.join(", ") : "—"}
                      </TableCell>
                      <TableCell className="font-mono text-xs text-green-600">
                        {d.untagged.length > 0 ? d.untagged.join(", ") : "—"}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">{d.comment || "—"}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>
      )}

      {/* Label editor */}
      {isAdmin && (
        <Dialog open={editing !== null} onOpenChange={(open) => !open && setEditing(null)}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Edit VLAN {editing} label</DialogTitle>
            </DialogHeader>
            <form
              className="space-y-4"
              onSubmit={(e) => {
                e.preventDefault();
                saveLabel();
              }}
            >
              <div className="space-y-2">
                <Label>Name</Label>
                <Input
                  value={editForm.name}
                  onChange={(e) => setEditForm({ ...editForm, name: e.target.value })}
                  placeholder="e.g. Main LAN"
                />
              </div>
              <div className="space-y-2">
                <Label>Purpose</Label>
                <Input
                  value={editForm.purpose}
                  onChange={(e) => setEditForm({ ...editForm, purpose: e.target.value })}
                  placeholder="e.g. Trusted internal clients"
                />
              </div>
              <div className="space-y-2">
                <Label>Color</Label>
                <div className="flex items-center gap-2">
                  <Input
                    type="color"
                    className="h-9 w-14 p-1"
                    value={editForm.color || "#3b82f6"}
                    onChange={(e) => setEditForm({ ...editForm, color: e.target.value })}
                  />
                  <Input
                    value={editForm.color}
                    onChange={(e) => setEditForm({ ...editForm, color: e.target.value })}
                    placeholder="#3b82f6"
                  />
                </div>
              </div>
              <Button type="submit" className="w-full">Save</Button>
            </form>
          </DialogContent>
        </Dialog>
      )}
    </div>
  );
}
