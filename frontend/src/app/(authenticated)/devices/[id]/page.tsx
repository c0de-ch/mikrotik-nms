"use client";

import { useEffect, useState } from "react";
import { useParams } from "next/navigation";
import { ExternalLink, ArrowLeft } from "lucide-react";
import Link from "next/link";
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
import { useAuth } from "@/context/auth";
import { api, type Device, type DeviceInterface, type Neighbor } from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";

export default function DeviceDetailPage() {
  const { id } = useParams<{ id: string }>();
  const { token } = useAuth();
  const [device, setDevice] = useState<Device | null>(null);
  const [interfaces, setInterfaces] = useState<DeviceInterface[]>([]);
  const [neighbors, setNeighbors] = useState<Neighbor[]>([]);

  useEffect(() => {
    if (!token || !id) return;
    api.devices.get(token, id).then(setDevice).catch(console.error);
    api.devices.interfaces(token, id).then(setInterfaces).catch(console.error);
    api.devices.neighbors(token, id).then(setNeighbors).catch(console.error);
  }, [token, id]);

  useWebSocket("device.health", (data) => {
    const update = data as { device_id: string; status: string; cpu_load?: number; memory_pct?: number; uptime?: string };
    if (update.device_id === id && device) {
      setDevice((prev) =>
        prev ? { ...prev, status: update.status as Device["status"], cpu_load: update.cpu_load ?? prev.cpu_load } : prev,
      );
    }
  });

  if (!device) {
    return <div className="text-muted-foreground">Loading...</div>;
  }

  const memPct = device.memory_used && device.memory_total
    ? Math.round((device.memory_used / device.memory_total) * 100)
    : null;

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-4">
        <Button variant="ghost" size="icon" render={<Link href="/devices" />}>
          <ArrowLeft className="h-4 w-4" />
        </Button>
        <div className="flex-1">
          <h1 className="text-2xl font-bold">{device.identity || device.address}</h1>
          <p className="text-sm text-muted-foreground">{device.address}</p>
        </div>
        <Badge variant={device.status === "online" ? "default" : "destructive"} className="text-sm">
          {device.status}
        </Badge>
        <Button variant="outline" render={<a href={`winbox://${device.address}`} />}>
          <ExternalLink className="mr-2 h-4 w-4" />
          Open in WinBox
        </Button>
      </div>

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Model</CardTitle></CardHeader>
          <CardContent><div className="text-lg font-semibold">{device.board || "—"}</div></CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">RouterOS</CardTitle></CardHeader>
          <CardContent><div className="text-lg font-semibold">{device.ros_version || "—"}</div></CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">CPU Load</CardTitle></CardHeader>
          <CardContent>
            <div className="text-lg font-semibold">{device.cpu_load != null ? `${device.cpu_load}%` : "—"}</div>
            {device.cpu_load != null && (
              <div className="mt-1 h-2 rounded-full bg-muted">
                <div
                  className="h-full rounded-full bg-primary transition-all"
                  style={{ width: `${Math.min(device.cpu_load, 100)}%` }}
                />
              </div>
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Memory</CardTitle></CardHeader>
          <CardContent>
            <div className="text-lg font-semibold">{memPct != null ? `${memPct}%` : "—"}</div>
            {memPct != null && (
              <div className="mt-1 h-2 rounded-full bg-muted">
                <div
                  className="h-full rounded-full bg-primary transition-all"
                  style={{ width: `${Math.min(memPct, 100)}%` }}
                />
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      <div className="grid gap-4 md:grid-cols-3">
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Uptime</CardTitle></CardHeader>
          <CardContent><div className="text-sm">{device.uptime || "—"}</div></CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Architecture</CardTitle></CardHeader>
          <CardContent><div className="text-sm">{device.architecture || "—"}</div></CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2"><CardTitle className="text-sm font-medium">Platform</CardTitle></CardHeader>
          <CardContent><div className="text-sm">{device.platform || "—"}</div></CardContent>
        </Card>
      </div>

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
