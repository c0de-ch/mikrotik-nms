"use client";

import { useCallback, useEffect, useState } from "react";
import { useParams } from "next/navigation";
import { ExternalLink, ArrowLeft } from "lucide-react";
import Link from "next/link";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useAuth } from "@/context/auth";
import { api, type Device, type DeviceInterface, type Neighbor } from "@/lib/api";
import { deviceStatusBadgeClass, deviceStatusLabel } from "@/lib/status";
import { useWebSocket } from "@/hooks/use-websocket";
import { DeviceLoadBadges } from "@/components/devices/device-load";
import { FindingsCard } from "@/components/devices/findings-card";
import { LoadCard } from "@/components/devices/load-card";
import { PollingCard } from "@/components/devices/polling-card";

// The device is re-read this often (database only): the polling card's
// "next read in …" and a pause set from another tab stay current.
const DEVICE_REFRESH_MS = 30_000;

export default function DeviceDetailPage() {
  const { id } = useParams<{ id: string }>();
  const { token, user } = useAuth();
  const [device, setDevice] = useState<Device | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [interfaces, setInterfaces] = useState<DeviceInterface[]>([]);
  const [neighbors, setNeighbors] = useState<Neighbor[]>([]);

  useEffect(() => {
    if (!token || !id) return;
    let alive = true;
    const load = () =>
      api.devices
        .get(token, id)
        .then((d) => {
          if (!alive) return;
          setDevice(d);
          setError(null);
        })
        .catch((e) => {
          if (alive) setError(e instanceof Error ? e.message : String(e));
        });
    load();
    const t = setInterval(load, DEVICE_REFRESH_MS);
    api.devices.interfaces(token, id).then(setInterfaces).catch(console.error);
    api.devices.neighbors(token, id).then(setNeighbors).catch(console.error);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [token, id]);

  useWebSocket(
    "device.health",
    useCallback(
      (data: unknown) => {
        const update = data as { device_id: string; status: string; cpu_load?: number; memory_pct?: number; uptime?: string };
        if (update.device_id !== id) return;
        setDevice((prev) =>
          prev
            ? {
                ...prev,
                status: update.status as Device["status"],
                cpu_load: update.cpu_load ?? prev.cpu_load,
                uptime: update.uptime ?? prev.uptime,
              }
            : prev,
        );
      },
      [id],
    ),
  );

  if (!device) {
    return error ? (
      <div className="space-y-3">
        <p role="alert" className="text-sm text-red-700 dark:text-red-400">
          Could not load this device: {error}
        </p>
        <Button variant="outline" size="sm" render={<Link href="/devices" />}>
          <ArrowLeft />
          Back to devices
        </Button>
      </div>
    ) : (
      <div className="text-muted-foreground">Loading…</div>
    );
  }

  return (
    <div className="min-w-0 space-y-6 [contain:inline-size]">
      <div className="flex flex-wrap items-start gap-x-4 gap-y-3">
        <Button variant="ghost" size="icon" render={<Link href="/devices" />} aria-label="Back to devices">
          <ArrowLeft className="h-4 w-4" />
        </Button>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
            <h1 className="min-w-0 break-words text-2xl font-bold">{device.identity || device.address}</h1>
            <Badge variant="outline" className={deviceStatusBadgeClass(device.status)}>
              {deviceStatusLabel(device.status)}
            </Badge>
            <DeviceLoadBadges device={device} detail="full" />
          </div>
          <p className="mt-0.5 text-sm text-muted-foreground">
            {device.address}
            {device.board ? ` · ${device.board}` : ""}
          </p>
        </div>
        <Button
          variant="outline"
          aria-label="Open WebFig"
          render={<a href={`http://${device.address}`} target="_blank" rel="noopener" />}
        >
          <ExternalLink className="h-4 w-4 sm:mr-1" />
          <span className="hidden sm:inline">Open WebFig</span>
        </Button>
      </div>

      {/* The device's own load next to what the NMS asks of it: the two
          halves of "is the NMS why this switch is busy?". */}
      <div className="grid items-start gap-4 lg:grid-cols-5">
        <div className="lg:col-span-2">
          <LoadCard device={device} />
        </div>
        <div className="min-w-0 lg:col-span-3">
          {token && <PollingCard device={device} token={token} isAdmin={user?.role === "admin"} onChange={setDevice} />}
        </div>
      </div>

      {token && <FindingsCard deviceId={device.id} token={token} />}

      <Tabs defaultValue="interfaces">
        <TabsList>
          <TabsTrigger value="interfaces">Interfaces ({interfaces.length})</TabsTrigger>
          <TabsTrigger value="neighbors">Neighbors ({neighbors.length})</TabsTrigger>
        </TabsList>

        <TabsContent value="interfaces">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>MAC Address</TableHead>
                <TableHead>MTU</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Comment</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {interfaces.map((iface) => (
                <TableRow key={iface.id}>
                  <TableCell className="font-medium">{iface.name}</TableCell>
                  <TableCell>{iface.type}</TableCell>
                  <TableCell className="font-mono text-xs">{iface.mac_address || "—"}</TableCell>
                  <TableCell>{iface.mtu || "—"}</TableCell>
                  <TableCell>
                    {iface.disabled ? (
                      <Badge variant="secondary">disabled</Badge>
                    ) : iface.running ? (
                      <Badge variant="default">running</Badge>
                    ) : (
                      <Badge variant="destructive">down</Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">{iface.comment || "—"}</TableCell>
                </TableRow>
              ))}
              {interfaces.length === 0 && (
                <TableRow>
                  <TableCell colSpan={6} className="py-8 text-center text-muted-foreground">
                    No interfaces read yet
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </TabsContent>

        <TabsContent value="neighbors">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Local Interface</TableHead>
                <TableHead>Identity</TableHead>
                <TableHead>Address</TableHead>
                <TableHead>MAC</TableHead>
                <TableHead>Platform</TableHead>
                <TableHead>Version</TableHead>
                <TableHead>Discovered By</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {neighbors.map((n) => (
                <TableRow key={n.id}>
                  <TableCell className="font-medium">{n.local_interface}</TableCell>
                  <TableCell>{n.neighbor_identity || "—"}</TableCell>
                  <TableCell>{n.neighbor_address || "—"}</TableCell>
                  <TableCell className="font-mono text-xs">{n.neighbor_mac}</TableCell>
                  <TableCell>{n.neighbor_platform || "—"}</TableCell>
                  <TableCell>{n.neighbor_version || "—"}</TableCell>
                  <TableCell>
                    <Badge variant="secondary">{n.discovered_by || "—"}</Badge>
                  </TableCell>
                </TableRow>
              ))}
              {neighbors.length === 0 && (
                <TableRow>
                  <TableCell colSpan={7} className="py-8 text-center text-muted-foreground">
                    No neighbors discovered
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </TabsContent>
      </Tabs>
    </div>
  );
}
