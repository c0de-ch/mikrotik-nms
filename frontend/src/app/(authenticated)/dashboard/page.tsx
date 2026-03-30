"use client";

import { useEffect, useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Server, Wifi, WifiOff, Cpu, Download } from "lucide-react";
import { useAuth } from "@/context/auth";
import { api, type Device } from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";

export default function DashboardPage() {
  const { token } = useAuth();
  const [devices, setDevices] = useState<Device[]>([]);

  useEffect(() => {
    if (!token) return;
    api.devices.list(token).then(setDevices).catch(console.error);
  }, [token]);

  useWebSocket("device.health", (data) => {
    const update = data as { device_id: string; status: string; cpu_load?: number; memory_pct?: number };
    setDevices((prev) =>
      prev.map((d) =>
        d.id === update.device_id
          ? { ...d, status: update.status as Device["status"], cpu_load: update.cpu_load ?? d.cpu_load }
          : d,
      ),
    );
  });

  const online = devices.filter((d) => d.status === "online").length;
  const offline = devices.filter((d) => d.status === "offline").length;
  const avgCpu = devices.filter((d) => d.cpu_load != null).reduce((sum, d) => sum + (d.cpu_load ?? 0), 0) /
    Math.max(devices.filter((d) => d.cpu_load != null).length, 1);

  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-bold">Dashboard</h1>

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Total Devices</CardTitle>
            <Server className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{devices.length}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Online</CardTitle>
            <Wifi className="h-4 w-4 text-green-500" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold text-green-600">{online}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Offline</CardTitle>
            <WifiOff className="h-4 w-4 text-red-500" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold text-red-600">{offline}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Avg CPU Load</CardTitle>
            <Cpu className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{avgCpu.toFixed(0)}%</div>
          </CardContent>
        </Card>
      </div>

      <div>
        <h2 className="mb-4 text-lg font-semibold">Device Status</h2>
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {devices.map((device) => (
            <Card key={device.id} className="relative">
              <CardContent className="pt-4">
                <div className="flex items-start justify-between">
                  <div className="min-w-0">
                    <p className="truncate font-medium">{device.identity || device.address}</p>
                    <p className="text-xs text-muted-foreground">{device.address}</p>
                  </div>
                  <Badge variant={device.status === "online" ? "default" : "destructive"}>
                    {device.status}
                  </Badge>
                </div>
                {device.status === "online" && (
                  <div className="mt-3 grid grid-cols-2 gap-2 text-xs text-muted-foreground">
                    <div>CPU: {device.cpu_load ?? "—"}%</div>
                    <div>
                      Mem:{" "}
                      {device.memory_used && device.memory_total
                        ? Math.round((device.memory_used / device.memory_total) * 100)
                        : "—"}
                      %
                    </div>
                    <div className="col-span-2">v{device.ros_version || "—"} • {device.board || "—"}</div>
                  </div>
                )}
              </CardContent>
            </Card>
          ))}
          {devices.length === 0 && (
            <Card className="col-span-full">
              <CardContent className="py-8 text-center text-muted-foreground">
                <Download className="mx-auto mb-2 h-8 w-8" />
                <p>No devices yet. Add a device to get started.</p>
              </CardContent>
            </Card>
          )}
        </div>
      </div>
    </div>
  );
}
