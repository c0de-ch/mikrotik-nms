"use client";

import { useEffect, useState, useCallback } from "react";
import { RefreshCw, Download, Cpu, ArrowRightLeft, Power, AlertTriangle } from "lucide-react";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
} from "@/components/ui/dropdown-menu";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog";
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
  const [rebootDialogOpen, setRebootDialogOpen] = useState(false);
  const [rebooting, setRebooting] = useState(false);

  const load = useCallback(() => {
    if (!token) return;
    api.firmware.list(token).then(setFirmware).catch(console.error);
    api.devices.list(token).then(setDevices).catch(console.error);
  }, [token]);

  useEffect(() => { load(); }, [load]);

  const deviceMap = new Map(devices.map((d) => [d.id, d]));
  const firmwareMap = new Map(firmware.map((f) => [f.device_id, f]));
  const isAdmin = user?.role === "admin";
  // Show all online devices, merging firmware data
  const allDeviceRows = devices.filter((d) => d.status === "online" || firmwareMap.has(d.id));

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

  const handleSetChannel = async (channel: string) => {
    if (!token || selected.size === 0) return;
    try {
      const result = await api.firmware.setChannel(token, Array.from(selected), channel);
      toast.success(`Channel set to "${channel}" on ${result.changed} device(s)`);
      if (result.errors?.length) toast.error(`Errors: ${result.errors.join(", ")}`, { duration: 8000 });
      setTimeout(load, 2000);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed");
    }
  };

  const handleReboot = async () => {
    if (!token || selected.size === 0) return;
    setRebooting(true);
    try {
      const result = await api.firmware.reboot(token, Array.from(selected));
      toast.success(`Reboot triggered on ${result.rebooted} device(s) — they will be unreachable for ~30s`);
      if (result.errors?.length) toast.error(`Errors: ${result.errors.join(", ")}`, { duration: 8000 });
      setSelected(new Set());
      setRebootDialogOpen(false);
      // give devices a chance to come back before the next refresh
      setTimeout(load, 60_000);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed");
    } finally {
      setRebooting(false);
    }
  };

  const handleUpgradeRouterboard = async () => {
    if (!token || selected.size === 0) return;
    try {
      const result = await api.firmware.upgradeRouterboard(token, Array.from(selected), true);
      toast.success(`RouterBoard firmware upgrade triggered on ${result.upgraded} device(s). Devices will reboot.`);
      if (result.errors?.length) toast.error(`Errors: ${result.errors.join(", ")}`, { duration: 8000 });
      setTimeout(load, 5000);
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

              <DropdownMenu>
                <DropdownMenuTrigger render={<Button variant="outline" disabled={selected.size === 0} />}>
                  <ArrowRightLeft className="mr-2 h-4 w-4" />
                  Channel ({selected.size})
                </DropdownMenuTrigger>
                <DropdownMenuContent>
                  <DropdownMenuItem onClick={() => handleSetChannel("stable")}>Stable</DropdownMenuItem>
                  <DropdownMenuItem onClick={() => handleSetChannel("long-term")}>Long-term</DropdownMenuItem>
                  <DropdownMenuItem onClick={() => handleSetChannel("testing")}>Testing</DropdownMenuItem>
                  <DropdownMenuItem onClick={() => handleSetChannel("development")}>Development</DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>

              <Button variant="outline" onClick={handleUpgradeRouterboard} disabled={selected.size === 0 || !!activeJobId}>
                <Cpu className="mr-2 h-4 w-4" />
                RouterBoard FW ({selected.size})
              </Button>

              <Button
                variant="outline"
                onClick={() => setRebootDialogOpen(true)}
                disabled={selected.size === 0 || !!activeJobId}
                className="text-destructive hover:text-destructive"
              >
                <Power className="mr-2 h-4 w-4" />
                Reboot ({selected.size})
              </Button>

              <Button onClick={handleUpgrade} disabled={selected.size === 0 || !!activeJobId}>
                <Download className="mr-2 h-4 w-4" />
                {activeJobId ? "Upgrading..." : `Upgrade RouterOS (${selected.size})`}
              </Button>
            </>
          )}
        </div>
      </div>

      <Dialog open={rebootDialogOpen} onOpenChange={setRebootDialogOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <AlertTriangle className="h-5 w-5 text-destructive" />
              Reboot {selected.size} device{selected.size === 1 ? "" : "s"}?
            </DialogTitle>
          </DialogHeader>
          <p className="text-sm text-muted-foreground">
            The selected devices will reboot immediately and be unreachable for roughly 30
            seconds. Any clients connected through them will lose connectivity during the reboot.
          </p>
          <div className="max-h-48 overflow-y-auto rounded-md border bg-muted/30 p-2">
            <ul className="space-y-1 text-sm">
              {Array.from(selected).map((id) => {
                const d = deviceMap.get(id);
                return (
                  <li key={id} className="font-mono text-xs">
                    {d?.identity || d?.address || id}
                    {d?.address && d?.identity && (
                      <span className="ml-2 text-muted-foreground">{d.address}</span>
                    )}
                  </li>
                );
              })}
            </ul>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRebootDialogOpen(false)} disabled={rebooting}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={handleReboot} disabled={rebooting}>
              <Power className="mr-2 h-4 w-4" />
              {rebooting ? "Rebooting..." : "Reboot now"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

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
          {allDeviceRows.map((device) => {
            const fw = firmwareMap.get(device.id);
            const progress = getDeviceUpgradeStatus(device.id);
            return (
              <TableRow key={device.id}>
                {isAdmin && (
                  <TableCell>
                    <input
                      type="checkbox"
                      checked={selected.has(device.id)}
                      onChange={() => toggleSelect(device.id)}
                      disabled={!!activeJobId}
                    />
                  </TableCell>
                )}
                <TableCell className="font-medium">
                  {device.identity || device.address}
                  <span className="ml-2 text-xs text-muted-foreground">{device.board}</span>
                </TableCell>
                <TableCell>{fw?.channel || "—"}</TableCell>
                <TableCell>{fw?.installed_version || device.ros_version || "—"}</TableCell>
                <TableCell>{fw?.latest_version || "—"}</TableCell>
                <TableCell>
                  {progress ? (
                    <Badge variant={progress.status === "failed" ? "destructive" : progress.status === "completed" ? "secondary" : "default"}>
                      {progress.status}
                    </Badge>
                  ) : device.status !== "online" ? (
                    <Badge variant="secondary">offline</Badge>
                  ) : fw?.update_available ? (
                    <Badge variant="default">Update Available</Badge>
                  ) : fw ? (
                    <Badge variant="secondary">Up to date</Badge>
                  ) : (
                    <Badge variant="secondary">Checking...</Badge>
                  )}
                  {progress?.message && (
                    <p className="mt-1 text-xs text-muted-foreground">{progress.message}</p>
                  )}
                </TableCell>
                <TableCell className="text-xs">
                  {fw?.routerboard_current || "—"}
                  {fw?.routerboard_upgrade && fw.routerboard_upgrade !== fw.routerboard_current && (
                    <span className="text-yellow-600"> → {fw.routerboard_upgrade}</span>
                  )}
                </TableCell>
                <TableCell className="text-xs text-muted-foreground">
                  {fw?.last_checked ? new Date(fw.last_checked).toLocaleString() : "—"}
                </TableCell>
              </TableRow>
            );
          })}
          {allDeviceRows.length === 0 && (
            <TableRow>
              <TableCell colSpan={isAdmin ? 8 : 7} className="py-8 text-center text-muted-foreground">
                No devices — add devices first
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
    </div>
  );
}
