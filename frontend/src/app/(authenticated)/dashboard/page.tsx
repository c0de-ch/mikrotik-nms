"use client";

import { useEffect, useState, useCallback } from "react";
import Link from "next/link";
import { toast } from "sonner";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Server, Wifi, WifiOff, Cpu, Download, AlertTriangle, X } from "lucide-react";
import { useAuth } from "@/context/auth";
import { api, type Device, type LoopEvent } from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";
import { deviceStatusBadgeClass, deviceStatusColor, deviceStatusLabel } from "@/lib/status";

const ALERT_WINDOW_MS = 60 * 60 * 1000; // last 60 minutes

function isRecent(recordedAt: string): boolean {
  return Date.now() - new Date(recordedAt).getTime() <= ALERT_WINDOW_MS;
}

function eventTypeLabel(t: string): string {
  switch (t) {
    case "stp_disabled": return "STP disabled";
    case "tcn_storm": return "TCN storm";
    case "loop_detected": return "Loop detected";
    case "mac_flap": return "MAC flap";
    case "bpdu_on_edge": return "BPDU on edge";
    case "port_disabled": return "Port disabled";
    case "port_link_down": return "Link down";
    case "port_link_flap": return "Port flap";
    case "port_loop_protect": return "Loop protect tripped";
    default: return t;
  }
}

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

export default function DashboardPage() {
  const { token } = useAuth();
  const [devices, setDevices] = useState<Device[]>([]);
  // Recent loop/STP events (last 60 min) driving the alert banner.
  const [alertEvents, setAlertEvents] = useState<LoopEvent[]>([]);
  const [bannerDismissed, setBannerDismissed] = useState(false);

  useEffect(() => {
    if (!token) return;
    api.devices.list(token).then(setDevices).catch(console.error);
    api.networkHealth
      .events(token, 200)
      .then((events) => setAlertEvents(events.filter((e) => isRecent(e.recorded_at))))
      .catch(console.error);
  }, [token]);

  // Live loop/STP events. New events repopulate the banner (clearing any prior
  // dismissal) and a critical one also fires a toast.
  useWebSocket(
    "network.health.event",
    useCallback((data: unknown) => {
      const ev = data as LoopEvent;
      if (!ev || !ev.event_type) return;
      setAlertEvents((prev) => [ev, ...prev].filter((e) => isRecent(e.recorded_at)));
      setBannerDismissed(false);
      if (ev.severity === "critical") {
        toast.error(
          `${eventTypeLabel(ev.event_type)}${ev.bridge_name ? ` on ${ev.bridge_name}` : ""} — ${ev.message}`,
        );
      }
    }, []),
  );

  // Drop events that have aged out of the 60-minute window once a minute so the
  // banner counts stay accurate without a reload.
  useEffect(() => {
    const t = setInterval(() => {
      setAlertEvents((prev) => prev.filter((e) => isRecent(e.recorded_at)));
    }, 60_000);
    return () => clearInterval(t);
  }, []);

  const recentEvents = alertEvents.filter((e) => isRecent(e.recorded_at));
  const criticalCount = recentEvents.filter((e) => e.severity === "critical").length;
  const warnCount = recentEvents.filter((e) => e.severity === "warn").length;
  const showBanner = recentEvents.length > 0 && !bannerDismissed;

  useWebSocket("device.health", (data) => {
    const update = data as {
      device_id: string;
      status: string;
      cpu_load?: number;
      memory_pct?: number;
      last_seen?: string;
      error?: string;
    };
    setDevices((prev) =>
      prev.map((d) =>
        d.id === update.device_id
          ? {
              ...d,
              status: update.status as Device["status"],
              cpu_load: update.cpu_load ?? d.cpu_load,
              last_seen: update.last_seen ?? d.last_seen,
              last_error: update.status === "online" ? null : update.error ?? d.last_error,
            }
          : d,
      ),
    );
  });

  const total = devices.length;
  const online = devices.filter((d) => d.status === "online").length;
  const offline = devices.filter((d) => d.status === "offline").length;
  // Anything that isn't a confirmed online/offline is "not responding" (gray):
  // never-polled devices and those missing polls within the offline grace window.
  const notResponding = total - online - offline;
  const avgCpu = devices.filter((d) => d.cpu_load != null).reduce((sum, d) => sum + (d.cpu_load ?? 0), 0) /
    Math.max(devices.filter((d) => d.cpu_load != null).length, 1);

  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-bold">Dashboard</h1>

      {showBanner && (
        <div
          className={`flex items-start gap-3 rounded-lg border p-4 ${
            criticalCount > 0
              ? "border-red-500/50 bg-red-50 text-red-800 dark:bg-red-950/40 dark:text-red-200"
              : "border-amber-500/50 bg-amber-50 text-amber-800 dark:bg-amber-950/40 dark:text-amber-200"
          }`}
          role="alert"
        >
          <AlertTriangle className={`mt-0.5 h-5 w-5 shrink-0 ${criticalCount > 0 ? "text-red-600" : "text-amber-600"}`} />
          <div className="min-w-0 flex-1">
            <p className="text-sm font-semibold">
              {[
                criticalCount > 0 ? `${criticalCount} critical` : null,
                warnCount > 0 ? `${warnCount} warning${warnCount === 1 ? "" : "s"}` : null,
              ]
                .filter(Boolean)
                .join(", ")}{" "}
              in the last hour — loop / STP activity detected
            </p>
            <Link href="/network-health" className="text-sm font-medium underline underline-offset-2">
              View details
            </Link>
          </div>
          <Button
            variant="ghost"
            size="icon"
            className="h-7 w-7 shrink-0 text-current hover:bg-black/5 dark:hover:bg-white/10"
            onClick={() => setBannerDismissed(true)}
            aria-label="Dismiss alert"
          >
            <X className="h-4 w-4" />
          </Button>
        </div>
      )}

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        <Link href="/devices" className="block transition-transform hover:scale-[1.01]">
          <Card className="cursor-pointer hover:border-foreground/30">
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
              <CardTitle className="text-sm font-medium">Total Devices</CardTitle>
              <Server className="h-4 w-4 text-muted-foreground" />
            </CardHeader>
            <CardContent>
              <div className="text-2xl font-bold">{devices.length}</div>
            </CardContent>
          </Card>
        </Link>
        <Link href="/devices?status=online" className="block transition-transform hover:scale-[1.01]">
          <Card className="cursor-pointer hover:border-foreground/30">
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
              <CardTitle className="text-sm font-medium">Online</CardTitle>
              <Wifi className="h-4 w-4 text-green-500" />
            </CardHeader>
            <CardContent>
              <div className="text-2xl font-bold text-green-600">{online}</div>
            </CardContent>
          </Card>
        </Link>
        <Link href="/devices?status=offline" className="block transition-transform hover:scale-[1.01]">
          <Card className="cursor-pointer hover:border-foreground/30">
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
              <CardTitle className="text-sm font-medium">Offline</CardTitle>
              <WifiOff className="h-4 w-4 text-red-500" />
            </CardHeader>
            <CardContent>
              <div className="text-2xl font-bold text-red-600">{offline}</div>
              {notResponding > 0 && (
                <p className="mt-1 text-xs text-muted-foreground">+{notResponding} not responding</p>
              )}
            </CardContent>
          </Card>
        </Link>
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

      {total > 0 && (
        <Card>
          <CardContent className="space-y-2 pt-4">
            <div className="flex h-3 w-full overflow-hidden rounded-full bg-muted">
              {online > 0 && (
                <div
                  className={`${deviceStatusColor("online")} transition-all`}
                  style={{ width: `${(online / total) * 100}%` }}
                  title={`${online} online`}
                />
              )}
              {notResponding > 0 && (
                <div
                  className={`${deviceStatusColor("unknown")} transition-all`}
                  style={{ width: `${(notResponding / total) * 100}%` }}
                  title={`${notResponding} not responding`}
                />
              )}
              {offline > 0 && (
                <div
                  className={`${deviceStatusColor("offline")} transition-all`}
                  style={{ width: `${(offline / total) * 100}%` }}
                  title={`${offline} offline`}
                />
              )}
            </div>
            <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
              <span className="flex items-center gap-1.5">
                <span className={`h-2 w-2 rounded-full ${deviceStatusColor("online")}`} />
                {online} online
              </span>
              <span className="flex items-center gap-1.5">
                <span className={`h-2 w-2 rounded-full ${deviceStatusColor("unknown")}`} />
                {notResponding} not responding
              </span>
              <span className="flex items-center gap-1.5">
                <span className={`h-2 w-2 rounded-full ${deviceStatusColor("offline")}`} />
                {offline} offline
              </span>
            </div>
          </CardContent>
        </Card>
      )}

      <div>
        <h2 className="mb-4 text-lg font-semibold">Device Status</h2>
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {devices.map((device) => (
            <Link
              key={device.id}
              href={`/devices/${device.id}`}
              className="block transition-transform hover:scale-[1.01]"
            >
              <Card className="relative h-full cursor-pointer hover:border-foreground/30">
                <CardContent className="pt-4">
                  <div className="flex items-start justify-between gap-2">
                    <div className="min-w-0">
                      <p className="truncate font-medium">{device.identity || device.address}</p>
                      <p className="text-xs text-muted-foreground">{device.address}</p>
                    </div>
                    <Badge variant="outline" className={deviceStatusBadgeClass(device.status)}>
                      {deviceStatusLabel(device.status)}
                    </Badge>
                  </div>
                  {/* Cached details are always shown — they persist across
                      blips and only refresh on the info interval. */}
                  <div className="mt-3 space-y-1 text-xs text-muted-foreground">
                    <div className="grid grid-cols-2 gap-2">
                      <div>CPU: {device.cpu_load ?? "—"}%</div>
                      <div>
                        Mem:{" "}
                        {device.memory_used && device.memory_total
                          ? Math.round((device.memory_used / device.memory_total) * 100)
                          : "—"}
                        %
                      </div>
                    </div>
                    <div>v{device.ros_version || "—"} • {device.board || "—"}</div>
                    {device.status !== "online" && (
                      <div>Last seen: {device.last_seen ? timeAgo(device.last_seen) : "never"}</div>
                    )}
                    {device.status !== "online" && device.last_error && (
                      <div className="truncate text-red-600/80 dark:text-red-400/80" title={device.last_error}>
                        {device.last_error}
                      </div>
                    )}
                  </div>
                </CardContent>
              </Card>
            </Link>
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
