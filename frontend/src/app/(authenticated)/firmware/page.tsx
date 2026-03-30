"use client";

import { useEffect, useState, useCallback } from "react";
import { RefreshCw, Download } from "lucide-react";
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
import { useAuth } from "@/context/auth";
import { api, type FirmwareStatus, type Device } from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";
import { toast } from "sonner";

interface UpgradeProgress {
  device_id: string;
  status: string;
  message: string;
}

export default function FirmwarePage() {
  const { token, user } = useAuth();
  const [firmware, setFirmware] = useState<FirmwareStatus[]>([]);
  const [devices, setDevices] = useState<Device[]>([]);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [checking, setChecking] = useState(false);
  const [activeJobId, setActiveJobId] = useState<string | null>(null);
  const [upgradeProgress, setUpgradeProgress] = useState<Map<string, UpgradeProgress>>(new Map());

  const load = useCallback(() => {
    if (!token) return;
    api.firmware.list(token).then(setFirmware).catch(console.error);
    api.devices.list(token).then(setDevices).catch(console.error);
  }, [token]);

  useEffect(() => { load(); }, [load]);

  const deviceMap = new Map(devices.map((d) => [d.id, d]));
  const isAdmin = user?.role === "admin";

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const handleCheckAll = async () => {
    if (!token) return;
    setChecking(true);
    try {
      await api.firmware.check(token);
      toast.success("Firmware check triggered");
      setTimeout(load, 5000);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed");
    } finally {
      setChecking(false);
    }
  };

  const handleUpgrade = async () => {
    if (!token || selected.size === 0) return;
    try {
      const result = await api.firmware.upgrade(token, Array.from(selected), true) as { job_id: string };
      setActiveJobId(result.job_id);
      setUpgradeProgress(new Map());
      toast.success(`Upgrade job started for ${selected.size} device(s)`);
      setSelected(new Set());
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed");
    }
  };

  // Subscribe to upgrade progress
  const wsTopic = activeJobId ? `upgrade.progress.${activeJobId}` : "";
  useWebSocket(wsTopic, (data) => {
    const progress = data as UpgradeProgress & { status: string; job_id: string };
    if (progress.device_id) {
      setUpgradeProgress((prev) => {
        const next = new Map(prev);
        next.set(progress.device_id, progress);
        return next;
      });
    }
    if (progress.status === "completed" && !progress.device_id) {
      // Job completed
      toast.success("Firmware upgrade job completed");
      setActiveJobId(null);
      setTimeout(load, 2000);
    }
  });

  const getDeviceUpgradeStatus = (deviceId: string) => {
    return upgradeProgress.get(deviceId);
  };

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">Firmware Management</h1>
        <div className="flex gap-2">
          {isAdmin && (
            <>
              <Button variant="outline" onClick={handleCheckAll} disabled={checking}>
                <RefreshCw className={`mr-2 h-4 w-4 ${checking ? "animate-spin" : ""}`} />
                Check All
              </Button>
              <Button onClick={handleUpgrade} disabled={selected.size === 0 || !!activeJobId}>
                <Download className="mr-2 h-4 w-4" />
                {activeJobId ? "Upgrading..." : `Upgrade Selected (${selected.size})`}
              </Button>
            </>
          )}
        </div>
      </div>

      <Table>
        <TableHeader>
          <TableRow>
            {isAdmin && <TableHead className="w-10" />}
            <TableHead>Device</TableHead>
            <TableHead>Channel</TableHead>
            <TableHead>Installed</TableHead>
            <TableHead>Latest</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>RouterBoard FW</TableHead>
            <TableHead>Last Checked</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {firmware.map((fw) => {
            const device = deviceMap.get(fw.device_id);
            const progress = getDeviceUpgradeStatus(fw.device_id);
            return (
              <TableRow key={fw.id}>
                {isAdmin && (
                  <TableCell>
                    <input
                      type="checkbox"
                      checked={selected.has(fw.device_id)}
                      onChange={() => toggleSelect(fw.device_id)}
                      disabled={!fw.update_available || !!activeJobId}
                    />
                  </TableCell>
                )}
                <TableCell className="font-medium">{device?.identity || device?.address || fw.device_id}</TableCell>
                <TableCell>{fw.channel}</TableCell>
                <TableCell>{fw.installed_version}</TableCell>
                <TableCell>{fw.latest_version || "—"}</TableCell>
                <TableCell>
                  {progress ? (
                    <Badge variant={progress.status === "failed" ? "destructive" : progress.status === "completed" ? "secondary" : "default"}>
                      {progress.status}
                    </Badge>
                  ) : fw.update_available ? (
                    <Badge variant="default">Update Available</Badge>
                  ) : (
                    <Badge variant="secondary">Up to date</Badge>
                  )}
                  {progress?.message && (
                    <p className="mt-1 text-xs text-muted-foreground">{progress.message}</p>
                  )}
                </TableCell>
                <TableCell className="text-xs">
                  {fw.routerboard_current || "—"}
                  {fw.routerboard_upgrade && fw.routerboard_upgrade !== fw.routerboard_current && (
                    <span className="text-yellow-600"> → {fw.routerboard_upgrade}</span>
                  )}
                </TableCell>
                <TableCell className="text-xs text-muted-foreground">
                  {fw.last_checked ? new Date(fw.last_checked).toLocaleString() : "—"}
                </TableCell>
              </TableRow>
            );
          })}
          {firmware.length === 0 && (
            <TableRow>
              <TableCell colSpan={isAdmin ? 8 : 7} className="py-8 text-center text-muted-foreground">
                No firmware data — add devices and wait for the firmware check
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
    </div>
  );
}
