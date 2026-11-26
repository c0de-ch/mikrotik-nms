"use client";

import { useEffect, useRef, useState, useCallback } from "react";
import { RefreshCw, Download, Cpu, ArrowRightLeft } from "lucide-react";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
} from "@/components/ui/dropdown-menu";
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
import { api, type FirmwareStatus, type FirmwareRelease, type Device } from "@/lib/api";
import { DeviceCredentialsDialog, type CredentialsSubmit } from "@/components/firmware/device-credentials-dialog";
import { deviceStatusBadgeClass, deviceStatusLabel } from "@/lib/status";
import { useWebSocket } from "@/hooks/use-websocket";
import { ReauthCancelled, useFreshAuth } from "@/components/auth/fresh-auth";
import { toast } from "sonner";

// The device-changing action waiting for its RouterOS credentials.
type PendingAction =
  | { kind: "upgrade"; ids: string[] }
  | { kind: "channel"; ids: string[]; channel: string }
  | { kind: "routerboard"; ids: string[] };

function ago(iso: string): string {
  const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 90) return `${s} s ago`;
  if (s < 90 * 60) return `${Math.round(s / 60)} min ago`;
  if (s < 36 * 3600) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

interface UpgradeProgress {
  device_id: string;
  status: string;
  message: string;
}

// How often a running job's stored state is read (see syncJob).
const JOB_POLL_MS = 3_000;

export default function FirmwarePage() {
  const { token, user } = useAuth();
  // Upgrades, channel changes and RouterBOARD upgrades need a recent
  // password login; this asks for it when the server says so.
  const withFreshAuth = useFreshAuth();
  const [firmware, setFirmware] = useState<FirmwareStatus[]>([]);
  const [devices, setDevices] = useState<Device[]>([]);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [checking, setChecking] = useState(false);
  const [activeJobId, setActiveJobId] = useState<string | null>(null);
  // The job this page is following: what a late answer for an earlier job is
  // checked against.
  const activeJobRef = useRef<string | null>(null);
  const [upgradeProgress, setUpgradeProgress] = useState<Map<string, UpgradeProgress>>(new Map());
  const [releases, setReleases] = useState<FirmwareRelease[]>([]);
  const [pending, setPending] = useState<PendingAction | null>(null);

  const load = useCallback(() => {
    if (!token) return;
    api.firmware.list(token).then(setFirmware).catch(console.error);
    api.firmware.releases(token).then(setReleases).catch(console.error);
    api.devices.list(token).then(setDevices).catch(console.error);
  }, [token]);

  useEffect(() => { load(); }, [load]);

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
      const res = await api.firmware.check(token);
      if (res.status === "recent" && res.fetched_at) {
        toast.success(`Release feed checked ${ago(res.fetched_at)} — status recomputed`);
        load();
      } else {
        toast.success("Checking MikroTik's release feed…");
        setTimeout(load, 5000);
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed");
    } finally {
      setChecking(false);
    }
  };

  const runPending = async (action: PendingAction, values: CredentialsSubmit) => {
    const creds = { username: values.username, password: values.password };
    try {
      if (action.kind === "upgrade") {
        const result = await withFreshAuth((t) => api.firmware.upgrade(t, action.ids, values.reboot, creds));
        activeJobRef.current = result.job_id;
        setActiveJobId(result.job_id);
        setUpgradeProgress(new Map());
        toast.success(`Upgrade job started for ${action.ids.length} device(s)`);
        setSelected(new Set());
      } else if (action.kind === "channel") {
        const result = await withFreshAuth((t) => api.firmware.setChannel(t, action.ids, action.channel, creds));
        toast.success(`Channel set to "${action.channel}" on ${result.changed} device(s)`);
        if (result.errors?.length) toast.error(`Errors: ${result.errors.join(", ")}`, { duration: 8000 });
        load();
      } else {
        const result = await withFreshAuth((t) => api.firmware.upgradeRouterboard(t, action.ids, values.reboot, creds));
        toast.success(
          `RouterBOARD firmware upgrade triggered on ${result.upgraded} device(s)` +
            (values.reboot ? ". Devices will reboot." : " — applied on the next reboot."),
        );
        if (result.errors?.length) toast.error(`Errors: ${result.errors.join(", ")}`, { duration: 8000 });
        setTimeout(load, 5000);
      }
    } catch (err) {
      if (err instanceof ReauthCancelled) return;
      toast.error(err instanceof Error ? err.message : "Failed");
    }
  };

  const ask = (action: PendingAction) => {
    if (!token || action.ids.length === 0) return;
    setPending(action);
  };

  const submitCredentials = (values: CredentialsSubmit) => {
    const action = pending;
    setPending(null);
    if (action) void runPending(action, values);
  };

  const lastFetched = releases.reduce<string | null>(
    (acc, r) => (!acc || r.fetched_at > acc ? r.fetched_at : acc),
    null,
  );

  const dialogText: Record<PendingAction["kind"], { title: string; confirm: string; reboot?: string }> = {
    upgrade: { title: "Upgrade RouterOS", confirm: "Start upgrade", reboot: "Reboot to install (otherwise download only — installs on the next reboot)" },
    channel: { title: "Change update channel", confirm: "Change channel" },
    routerboard: { title: "Upgrade RouterBOARD firmware", confirm: "Upgrade", reboot: "Reboot when done" },
  };

  // syncJob reads the job's stored state and ends the job on this page once
  // the server says it is over. The WS frames below are the live view, but
  // they only reach a page that is already subscribed: a job whose login is
  // refused, or whose device is unreachable, is over before the subscription
  // exists, and frames are lost across a reconnect. So the stored state is
  // read when a job starts, every few seconds while it runs, and when the
  // job-finished frame arrives.
  const syncJob = useCallback(
    async (jobId: string) => {
      if (!token) return;
      let state;
      try {
        state = await api.firmware.job(token, jobId);
      } catch {
        return; // the next read tries again
      }
      if (activeJobRef.current !== jobId) return;
      setUpgradeProgress((prev) => {
        const next = new Map(prev);
        for (const d of state.devices) {
          if (d.status !== "pending") next.set(d.device_id, d);
        }
        return next;
      });
      if (state.job.status !== "completed" && state.job.status !== "failed") return;
      activeJobRef.current = null;
      setActiveJobId(null);
      const failed = state.devices.filter((d) => d.status === "failed").length;
      if (state.job.status === "failed") {
        toast.error("The upgrade job could not run", { duration: 10_000 });
      } else if (failed > 0) {
        toast.error(`Upgrade job finished: ${failed} of ${state.devices.length} device(s) failed`, { duration: 10_000 });
      } else {
        toast.success("Firmware upgrade job completed");
      }
      setTimeout(load, 2000);
    },
    [token, load],
  );

  useEffect(() => {
    if (!activeJobId) return;
    const first = setTimeout(() => void syncJob(activeJobId), 500);
    const timer = setInterval(() => void syncJob(activeJobId), JOB_POLL_MS);
    return () => {
      clearTimeout(first);
      clearInterval(timer);
    };
  }, [activeJobId, syncJob]);

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
    } else if (progress.status === "completed" && progress.job_id) {
      // The job is over: the stored state has every device's outcome.
      void syncJob(progress.job_id);
    }
  });

  const getDeviceUpgradeStatus = (deviceId: string) => {
    return upgradeProgress.get(deviceId);
  };

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <div>
          <h1 className="text-2xl font-bold">Firmware Management</h1>
          <p className="text-xs text-muted-foreground">
            Latest versions from MikroTik&apos;s release feed
            {lastFetched ? `, checked ${ago(lastFetched)}` : " — not fetched yet"}. Devices are not asked to check.
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
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
                  <DropdownMenuItem onClick={() => ask({ kind: "channel", ids: Array.from(selected), channel: "stable" })}>Stable</DropdownMenuItem>
                  <DropdownMenuItem onClick={() => ask({ kind: "channel", ids: Array.from(selected), channel: "long-term" })}>Long-term</DropdownMenuItem>
                  <DropdownMenuItem onClick={() => ask({ kind: "channel", ids: Array.from(selected), channel: "testing" })}>Testing</DropdownMenuItem>
                  <DropdownMenuItem onClick={() => ask({ kind: "channel", ids: Array.from(selected), channel: "development" })}>Development</DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>

              <Button variant="outline" onClick={() => ask({ kind: "routerboard", ids: Array.from(selected) })} disabled={selected.size === 0 || !!activeJobId}>
                <Cpu className="mr-2 h-4 w-4" />
                RouterBoard FW ({selected.size})
              </Button>

              <Button onClick={() => ask({ kind: "upgrade", ids: Array.from(selected) })} disabled={selected.size === 0 || !!activeJobId}>
                <Download className="mr-2 h-4 w-4" />
                {activeJobId ? "Upgrading..." : `Upgrade RouterOS (${selected.size})`}
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
            <TableHead>Last Read</TableHead>
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
                    <Badge variant="outline" className={deviceStatusBadgeClass(device.status)}>
                      {deviceStatusLabel(device.status)}
                    </Badge>
                  ) : fw?.update_available ? (
                    <Badge variant="default">Update Available</Badge>
                  ) : fw && !fw.latest_version ? (
                    // Nothing to compare with: a RouterOS 6 device (the feed
                    // is v7's), or a channel whose fetch has not succeeded.
                    <Badge
                      variant="outline"
                      title="MikroTik's release feed has no version to compare this device with (RouterOS 6, or the channel has not been fetched yet)"
                    >
                      Not compared
                    </Badge>
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

      <DeviceCredentialsDialog
        open={!!pending}
        title={pending ? dialogText[pending.kind].title : ""}
        description={
          pending
            ? `${pending.ids.length} device(s)${pending.kind === "channel" ? ` → ${pending.channel}` : ""}. ` +
              "Enter a RouterOS user allowed to change the devices."
            : ""
        }
        confirmLabel={pending ? dialogText[pending.kind].confirm : ""}
        rebootLabel={pending ? dialogText[pending.kind].reboot : undefined}
        onCancel={() => setPending(null)}
        onSubmit={submitCredentials}
      />
    </div>
  );
}
