"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { AreaChart, Area, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer } from "recharts";
import { Globe, Loader2, Monitor, Pencil, Play, Plus, Radar, Trash2 } from "lucide-react";
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
import {
  api,
  type Device,
  type LoopEvent,
  type NetworkClient,
  type PingSample,
  type PingTarget,
  type WifiHistoryEntry,
} from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";
import { toast } from "sonner";

// ----- status derivation (per wire contract) --------------------------------

type TargetStatus = "ok" | "degraded" | "down" | "nodata";

// error != "" means the probe could not run at all -> "no data" (gray).
// 100% loss -> down (red). Any loss or avg RTT above 150 ms -> degraded.
function statusOf(s: PingSample | null | undefined): TargetStatus {
  if (!s) return "nodata";
  if (s.error) return "nodata";
  if (s.loss_pct >= 100) return "down";
  if (s.loss_pct > 0 || (s.rtt_avg_ms !== null && s.rtt_avg_ms > 150)) return "degraded";
  return "ok";
}

function StatusBadge({ sample }: { sample: PingSample | null }) {
  switch (statusOf(sample)) {
    case "ok":
      return <Badge className="bg-green-100 text-green-700">ok</Badge>;
    case "degraded":
      return <Badge className="bg-amber-100 text-amber-700">degraded</Badge>;
    case "down":
      return <Badge className="bg-red-100 text-red-700">down</Badge>;
    default:
      return (
        <Badge className="bg-muted text-muted-foreground" title={sample?.error || undefined}>
          no data
        </Badge>
      );
  }
}

// ----- small formatting helpers (duplicated per page, repo pattern) ---------

function formatMs(v: number | null | undefined): string {
  if (v === null || v === undefined) return "—";
  return `${v.toFixed(1)} ms`;
}

function lossColor(pct: number): string {
  if (pct >= 100) return "text-red-600";
  if (pct > 0) return "text-amber-600";
  return "text-green-600";
}

// formatLoss rounds loss_pct for display — backend fallback paths can produce
// repeating decimals like 33.33333333333333.
function formatLoss(pct: number): string {
  const rounded = Math.round(pct * 10) / 10;
  return `${Number.isInteger(rounded) ? rounded.toFixed(0) : rounded.toFixed(1)}%`;
}

function timeAgo(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h ${mins % 60}m ago`;
  return `${Math.floor(hrs / 24)}d ago`;
}

// formatDateTime renders an ISO date string as "dd.mm.yyyy HH:mm" in 24h
// format using the user's local timezone.
function formatDateTime(dateStr: string): string {
  const d = new Date(dateStr);
  const dd = String(d.getDate()).padStart(2, "0");
  const mm = String(d.getMonth() + 1).padStart(2, "0");
  const yyyy = d.getFullYear();
  const hh = String(d.getHours()).padStart(2, "0");
  const min = String(d.getMinutes()).padStart(2, "0");
  return `${dd}.${mm}.${yyyy} ${hh}:${min}`;
}

// ----- detail-dialog ranges --------------------------------------------------

type RangeKey = "1h" | "6h" | "24h" | "7d";
const rangeKeys: RangeKey[] = ["1h", "6h", "24h", "7d"];
const rangeMs: Record<RangeKey, number> = {
  "1h": 3_600_000,
  "6h": 6 * 3_600_000,
  "24h": 24 * 3_600_000,
  "7d": 7 * 24 * 3_600_000,
};

// Axis labels: include the day for the longer ranges, plain time otherwise.
function timeLabel(dateStr: string, range: RangeKey): string {
  const d = new Date(dateStr);
  if (range === "24h" || range === "7d") {
    const dd = String(d.getDate()).padStart(2, "0");
    const mm = String(d.getMonth() + 1).padStart(2, "0");
    const hh = String(d.getHours()).padStart(2, "0");
    const min = String(d.getMinutes()).padStart(2, "0");
    return `${dd}.${mm} ${hh}:${min}`;
  }
  return d.toLocaleTimeString();
}

// One row of the correlated event list (wifi + network-health merged).
interface MergedEvent {
  key: string;
  source: "wifi" | "network";
  event: string;
  severity: string;
  text: string;
  recorded_at: string;
}

function eventBadge(ev: MergedEvent) {
  if (ev.source === "wifi") {
    switch (ev.event) {
      case "join":
        return <Badge className="bg-green-100 text-green-700">join</Badge>;
      case "leave":
        return <Badge className="bg-red-100 text-red-700">leave</Badge>;
      case "roam":
        return <Badge className="bg-blue-100 text-blue-700">roam</Badge>;
      default:
        return <Badge variant="secondary">{ev.event}</Badge>;
    }
  }
  const cls = ev.severity === "critical" ? "bg-red-100 text-red-700" : "bg-amber-100 text-amber-700";
  return <Badge className={cls}>{ev.event}</Badge>;
}

const quickAddTargets = [
  { label: "Cloudflare", address: "1.1.1.1" },
  { label: "Google", address: "8.8.8.8" },
];

export default function ConnectivityPage() {
  const { token, user } = useAuth();
  const isAdmin = user?.role === "admin";

  const [targets, setTargets] = useState<PingTarget[]>([]);
  const [devices, setDevices] = useState<Device[]>([]);
  const [clients, setClients] = useState<NetworkClient[]>([]);
  const [running, setRunning] = useState<Set<string>>(new Set());

  // Add / edit / delete dialog state
  const [addInternetOpen, setAddInternetOpen] = useState(false);
  const [internetForm, setInternetForm] = useState({ label: "", address: "", device_id: "" });
  const [watchOpen, setWatchOpen] = useState(false);
  const [watchForm, setWatchForm] = useState({ mac: "", freeMac: "", label: "", device_id: "" });
  const [editTarget, setEditTarget] = useState<PingTarget | null>(null);
  const [editForm, setEditForm] = useState({ label: "", address: "", device_id: "", enabled: true });
  const [confirmDelete, setConfirmDelete] = useState<PingTarget | null>(null);

  // Detail dialog state
  const [detailTarget, setDetailTarget] = useState<PingTarget | null>(null);
  const [range, setRange] = useState<RangeKey>("6h");
  const [detailLoading, setDetailLoading] = useState(false);
  // Monotonic id of the newest detail fetch; stale responses bail out.
  const detailSeqRef = useRef(0);
  // Chronological (oldest-first) so live samples append at the end.
  const [detailPings, setDetailPings] = useState<PingSample[]>([]);
  const [detailSignals, setDetailSignals] = useState<{ recorded_at: string; signal_dbm: number | null }[]>([]);
  const [detailWifiEvents, setDetailWifiEvents] = useState<WifiHistoryEntry[]>([]);
  const [detailNetEvents, setDetailNetEvents] = useState<LoopEvent[]>([]);

  const load = useCallback(() => {
    if (!token) return;
    Promise.all([api.connectivity.targets(token), api.devices.list(token)])
      .then(([t, d]) => {
        setTargets(t ?? []);
        setDevices(d ?? []);
      })
      .catch(console.error);
  }, [token]);

  useEffect(() => {
    load();
    const t = setInterval(load, 30_000);
    return () => clearInterval(t);
  }, [load]);

  // Client list for the watch picker — cached snapshot, loaded once.
  useEffect(() => {
    if (!token) return;
    api.clients
      .cached(token)
      .then((res) => setClients(res.clients ?? []))
      .catch(console.error);
  }, [token]);

  // Live samples: update the matching target's last_sample (and cached IP for
  // client targets) and, if the detail dialog shows that target, append a point.
  useWebSocket(
    "connectivity.sample",
    useCallback(
      (data: unknown) => {
        const s = data as PingSample;
        if (!s?.target_id) return;
        setTargets((prev) =>
          prev.map((t) =>
            t.id === s.target_id ? { ...t, last_sample: s, address: s.address || t.address } : t,
          ),
        );
        if (detailTarget && detailTarget.id === s.target_id) {
          // Cap matches the fetch limit: a smaller cap would truncate the
          // fetched history (e.g. a 7d range) on the first live append. The
          // array resets on every openDetail/range change, so growth while the
          // dialog is open is one sample per poll cycle.
          setDetailPings((prev) => [...prev, s].slice(-10000));
        }
      },
      [detailTarget],
    ),
  );

  // ----- detail data ---------------------------------------------------------

  const loadDetail = useCallback(async () => {
    if (!token || !detailTarget) return;
    // Guard against out-of-order responses: switching 7d -> 1h quickly can
    // resolve the light 1h query first, then the stale 7d response would
    // overwrite it. Only the newest request may apply its results.
    const seq = ++detailSeqRef.current;
    setDetailLoading(true);
    const from = new Date(Date.now() - rangeMs[range]).toISOString();
    try {
      if (detailTarget.kind === "client") {
        const tl = await api.connectivity.clientTimeline(token, detailTarget.mac_address, from);
        if (seq !== detailSeqRef.current) return;
        setDetailPings([...(tl.pings ?? [])].reverse());
        setDetailSignals([...(tl.signals ?? [])].reverse());
        setDetailWifiEvents(tl.wifi_events ?? []);
        setDetailNetEvents(tl.network_events ?? []);
      } else {
        const samples = await api.connectivity.samples(token, detailTarget.id, { from, limit: 10000 });
        if (seq !== detailSeqRef.current) return;
        setDetailPings([...(samples ?? [])].reverse());
        setDetailSignals([]);
        setDetailWifiEvents([]);
        setDetailNetEvents([]);
      }
    } catch (e) {
      if (seq === detailSeqRef.current) {
        toast.error(e instanceof Error ? e.message : "Failed to load samples");
      }
    } finally {
      if (seq === detailSeqRef.current) {
        setDetailLoading(false);
      }
    }
  }, [token, detailTarget, range]);

  useEffect(() => {
    loadDetail();
  }, [loadDetail]);

  const openDetail = (t: PingTarget) => {
    setDetailPings([]);
    setDetailSignals([]);
    setDetailWifiEvents([]);
    setDetailNetEvents([]);
    setRange("6h");
    setDetailTarget(t);
  };

  // Failed probes (error != "") chart as loss 100 / rtt null so outages are
  // visible in the loss chart instead of silently missing.
  const chartData = useMemo(
    () =>
      detailPings.map((s) => ({
        time: timeLabel(s.recorded_at, range),
        rtt: s.error ? null : s.rtt_avg_ms,
        loss: s.error ? 100 : s.loss_pct,
      })),
    [detailPings, range],
  );

  const signalData = useMemo(
    () =>
      detailSignals.map((s) => ({
        time: timeLabel(s.recorded_at, range),
        signal: s.signal_dbm,
      })),
    [detailSignals, range],
  );

  const mergedEvents = useMemo<MergedEvent[]>(() => {
    const out: MergedEvent[] = [];
    for (const e of detailWifiEvents) {
      if (e.event === "seen") continue;
      let text: string;
      switch (e.event) {
        case "roam":
          text = `roam → ${e.ap_name || "?"}`;
          break;
        case "join":
          text = `joined ${e.ap_name || "?"}`;
          break;
        case "leave":
          text = `left ${e.ap_name || "?"}${e.reason ? ` (${e.reason})` : ""}`;
          break;
        default:
          text = e.ap_name || e.event;
      }
      if (e.signal) text += ` (signal ${e.signal})`;
      out.push({ key: `w-${e.id}`, source: "wifi", event: e.event, severity: "", text, recorded_at: e.recorded_at });
    }
    for (const e of detailNetEvents) {
      const where = [e.bridge_name, e.port_interface].filter(Boolean).join("/");
      const text = `${where ? `on ${where}` : e.device_name || e.device_id}${e.message ? `: ${e.message}` : ""}`;
      out.push({
        key: `n-${e.id}`,
        source: "network",
        event: e.event_type,
        severity: e.severity,
        text,
        recorded_at: e.recorded_at,
      });
    }
    out.sort((a, b) => new Date(b.recorded_at).getTime() - new Date(a.recorded_at).getTime());
    return out.slice(0, 100);
  }, [detailWifiEvents, detailNetEvents]);

  const latestPing = detailPings[detailPings.length - 1];

  // ----- mutations -----------------------------------------------------------

  const runNow = async (t: PingTarget) => {
    if (!token) return;
    setRunning((prev) => new Set(prev).add(t.id));
    try {
      const s = await api.connectivity.runTarget(token, t.id);
      setTargets((prev) =>
        prev.map((x) => (x.id === t.id ? { ...x, last_sample: s, address: s.address || x.address } : x)),
      );
      if (s.error) {
        toast.error(`Probe failed: ${s.error}`);
      } else {
        toast.success(`${s.address}: ${formatMs(s.rtt_avg_ms)} avg, ${formatLoss(s.loss_pct)} loss`);
      }
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Failed to run probe");
    } finally {
      setRunning((prev) => {
        const next = new Set(prev);
        next.delete(t.id);
        return next;
      });
    }
  };

  const openAddInternet = (prefill?: { label?: string; address?: string }) => {
    setInternetForm({ label: prefill?.label ?? "", address: prefill?.address ?? "", device_id: "" });
    setAddInternetOpen(true);
  };

  const submitAddInternet = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!token) return;
    if (!internetForm.address.trim()) {
      toast.error("Address is required");
      return;
    }
    if (!internetForm.device_id) {
      toast.error("Select the device that runs the probe");
      return;
    }
    try {
      await api.connectivity.createTarget(token, {
        kind: "internet",
        address: internetForm.address.trim(),
        label: internetForm.label.trim() || undefined,
        device_id: internetForm.device_id,
      });
      toast.success("Target added");
      setAddInternetOpen(false);
      load();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to add target");
    }
  };

  const openWatch = () => {
    setWatchForm({ mac: "", freeMac: "", label: "", device_id: "" });
    setWatchOpen(true);
  };

  const submitWatch = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!token) return;
    const mac = (watchForm.freeMac.trim() || watchForm.mac).toUpperCase();
    if (!mac) {
      toast.error("Select a client or enter a MAC address");
      return;
    }
    if (!/^([0-9A-F]{2}[:-]){5}[0-9A-F]{2}$/.test(mac)) {
      toast.error("Invalid MAC address (expected AA:BB:CC:DD:EE:FF)");
      return;
    }
    try {
      await api.connectivity.createTarget(token, {
        kind: "client",
        mac_address: mac,
        label: watchForm.label.trim() || undefined,
        device_id: watchForm.device_id || undefined,
      });
      toast.success("Client watch added");
      setWatchOpen(false);
      load();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to watch client");
    }
  };

  const openEdit = (t: PingTarget) => {
    setEditForm({ label: t.label, address: t.address, device_id: t.device_id, enabled: t.enabled });
    setEditTarget(t);
  };

  const submitEdit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!token || !editTarget) return;
    if (editTarget.kind === "internet") {
      if (!editForm.address.trim()) {
        toast.error("Address is required");
        return;
      }
      if (!editForm.device_id) {
        toast.error("Select the device that runs the probe");
        return;
      }
    }
    try {
      const data: { label: string; device_id: string; enabled: boolean; address?: string } = {
        label: editForm.label.trim(),
        device_id: editForm.device_id,
        enabled: editForm.enabled,
      };
      // For client targets the address is auto-resolved from mac_lookup;
      // never overwrite the cached value from the edit form.
      if (editTarget.kind === "internet") data.address = editForm.address.trim();
      await api.connectivity.updateTarget(token, editTarget.id, data);
      toast.success("Target updated");
      setEditTarget(null);
      load();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to update target");
    }
  };

  const doDelete = async () => {
    if (!token || !confirmDelete) return;
    try {
      await api.connectivity.deleteTarget(token, confirmDelete.id);
      toast.success("Target deleted");
      setConfirmDelete(null);
      if (detailTarget?.id === confirmDelete.id) setDetailTarget(null);
      load();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to delete target");
    }
  };

  // ----- derived lists -------------------------------------------------------

  const internetTargets = useMemo(
    () =>
      targets
        .filter((t) => t.kind === "internet")
        .sort((a, b) => (a.label || a.address).localeCompare(b.label || b.address)),
    [targets],
  );

  const clientTargets = useMemo(
    () =>
      targets
        .filter((t) => t.kind === "client")
        .sort((a, b) =>
          (a.label || a.host_name || a.mac_address).localeCompare(b.label || b.host_name || b.mac_address),
        ),
    [targets],
  );

  const okCount = (list: PingTarget[]) => list.filter((t) => t.enabled && statusOf(t.last_sample) === "ok").length;
  const downNow = targets.filter((t) => t.enabled && statusOf(t.last_sample) === "down").length;
  const degradedNow = targets.filter((t) => t.enabled && statusOf(t.last_sample) === "degraded").length;

  const sortedClients = useMemo(
    () =>
      [...clients].sort((a, b) =>
        (a.host_name || a.dns_name || a.mac_address).localeCompare(b.host_name || b.dns_name || b.mac_address),
      ),
    [clients],
  );

  const clientDisplayName = (t: PingTarget) => t.label || t.host_name || t.mac_address;

  const detailTitle = detailTarget
    ? detailTarget.kind === "client"
      ? clientDisplayName(detailTarget)
      : detailTarget.label || detailTarget.address
    : "";

  // Shared cells: Probing device / Last RTT / Loss / Status / Updated.
  const probingDeviceCell = (t: PingTarget) => (
    <TableCell className="text-xs">{t.device_name || (t.kind === "client" ? "auto" : "—")}</TableCell>
  );
  const rttCell = (t: PingTarget) => (
    <TableCell className="text-xs font-mono">{formatMs(t.last_sample?.rtt_avg_ms ?? null)}</TableCell>
  );
  const lossCell = (t: PingTarget) => (
    <TableCell className={`text-xs font-mono ${t.last_sample && !t.last_sample.error ? lossColor(t.last_sample.loss_pct) : "text-muted-foreground"}`}>
      {t.last_sample && !t.last_sample.error ? formatLoss(t.last_sample.loss_pct) : "—"}
    </TableCell>
  );
  const statusCell = (t: PingTarget) => (
    <TableCell>
      <span className="inline-flex items-center gap-1.5">
        <StatusBadge sample={t.last_sample} />
        {!t.enabled && <Badge variant="secondary">disabled</Badge>}
      </span>
    </TableCell>
  );
  const updatedCell = (t: PingTarget) => (
    <TableCell
      className="text-xs text-muted-foreground whitespace-nowrap"
      title={t.last_sample ? formatDateTime(t.last_sample.recorded_at) : undefined}
    >
      {t.last_sample ? timeAgo(t.last_sample.recorded_at) : "—"}
    </TableCell>
  );
  const actionsCell = (t: PingTarget) =>
    isAdmin ? (
      <TableCell className="text-right" onClick={(e) => e.stopPropagation()}>
        <div className="flex justify-end gap-1">
          <Button
            variant="ghost"
            size="icon-sm"
            title="Run probe now"
            disabled={running.has(t.id)}
            onClick={() => runNow(t)}
          >
            {running.has(t.id) ? (
              <Loader2 className="h-3.5 w-3.5 animate-spin" />
            ) : (
              <Play className="h-3.5 w-3.5" />
            )}
          </Button>
          <Button variant="ghost" size="icon-sm" title="Edit" onClick={() => openEdit(t)}>
            <Pencil className="h-3.5 w-3.5" />
          </Button>
          <Button variant="ghost" size="icon-sm" title="Delete" onClick={() => setConfirmDelete(t)}>
            <Trash2 className="h-3.5 w-3.5 text-destructive" />
          </Button>
        </div>
      </TableCell>
    ) : null;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Connectivity</h1>
          <p className="text-sm text-muted-foreground">
            Internet-path health and per-client reachability, probed from your RouterOS devices.
          </p>
        </div>
        {isAdmin && (
          <div className="flex gap-2">
            <Button onClick={() => openAddInternet()}>
              <Plus className="mr-1.5 h-4 w-4" />
              Add target
            </Button>
            <Button variant="outline" onClick={openWatch}>
              <Monitor className="mr-1.5 h-4 w-4" />
              Watch client
            </Button>
          </div>
        )}
      </div>

      {/* Stat cards */}
      <div className="grid gap-4 md:grid-cols-4">
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium">Internet targets</CardTitle>
          </CardHeader>
          <CardContent className="flex items-center gap-2">
            <Globe className="h-5 w-5 text-muted-foreground" />
            <div className="text-2xl font-bold">{internetTargets.length}</div>
            <span className="text-xs text-muted-foreground">{okCount(internetTargets)} ok</span>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium">Watched clients</CardTitle>
          </CardHeader>
          <CardContent className="flex items-center gap-2">
            <Monitor className="h-5 w-5 text-muted-foreground" />
            <div className="text-2xl font-bold">{clientTargets.length}</div>
            <span className="text-xs text-muted-foreground">{okCount(clientTargets)} ok</span>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium">Down now</CardTitle>
          </CardHeader>
          <CardContent className="flex items-center gap-2">
            <Radar className={`h-5 w-5 ${downNow > 0 ? "text-red-600" : "text-muted-foreground"}`} />
            <div className={`text-2xl font-bold ${downNow > 0 ? "text-red-600" : ""}`}>{downNow}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium">Degraded now</CardTitle>
          </CardHeader>
          <CardContent className="flex items-center gap-2">
            <Radar className={`h-5 w-5 ${degradedNow > 0 ? "text-amber-600" : "text-muted-foreground"}`} />
            <div className={`text-2xl font-bold ${degradedNow > 0 ? "text-amber-600" : ""}`}>{degradedNow}</div>
          </CardContent>
        </Card>
      </div>

      {/* Internet targets */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Internet targets</CardTitle>
          <p className="text-xs text-muted-foreground">
            Fixed IPs / hostnames pinged from a chosen RouterOS device. Click a row for charts.
          </p>
        </CardHeader>
        <CardContent>
          {internetTargets.length === 0 ? (
            <div className="py-8 text-center text-sm text-muted-foreground space-y-3">
              <p>
                No internet targets yet. Add one to track latency and loss toward the outside world.
                Samples appear within one polling cycle (default 30s).
              </p>
              {isAdmin && (
                <div className="flex justify-center gap-2">
                  {quickAddTargets.map((q) => (
                    <Button
                      key={q.address}
                      variant="outline"
                      size="sm"
                      onClick={() => openAddInternet({ label: q.label, address: q.address })}
                    >
                      <Plus className="mr-1 h-3.5 w-3.5" />
                      Add {q.address} ({q.label})
                    </Button>
                  ))}
                </div>
              )}
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Label</TableHead>
                  <TableHead>Address</TableHead>
                  <TableHead>Probing device</TableHead>
                  <TableHead>Last RTT</TableHead>
                  <TableHead>Loss</TableHead>
                  <TableHead>Jitter</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Updated</TableHead>
                  {isAdmin && <TableHead className="text-right">Actions</TableHead>}
                </TableRow>
              </TableHeader>
              <TableBody>
                {internetTargets.map((t) => (
                  <TableRow
                    key={t.id}
                    className={`cursor-pointer hover:bg-muted/50 ${t.enabled ? "" : "opacity-60"}`}
                    onClick={() => openDetail(t)}
                  >
                    <TableCell className="text-sm font-medium">{t.label || "—"}</TableCell>
                    <TableCell className="font-mono text-xs">{t.address}</TableCell>
                    {probingDeviceCell(t)}
                    {rttCell(t)}
                    {lossCell(t)}
                    <TableCell className="text-xs font-mono">{formatMs(t.last_sample?.jitter_ms ?? null)}</TableCell>
                    {statusCell(t)}
                    {updatedCell(t)}
                    {actionsCell(t)}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Watched clients */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Watched clients</CardTitle>
          <p className="text-xs text-muted-foreground">
            Clients followed by MAC — the current IP is resolved each cycle and pinged from the nearest
            online device. Click a row for latency, signal and event history.
          </p>
        </CardHeader>
        <CardContent>
          {clientTargets.length === 0 ? (
            <p className="py-8 text-center text-sm text-muted-foreground">
              No watched clients yet. Watch a client to root-cause dropoffs with correlated latency,
              loss, signal and event timelines. Samples appear within one polling cycle (default 30s).
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Client</TableHead>
                  <TableHead>Current IP</TableHead>
                  <TableHead>Probing device</TableHead>
                  <TableHead>Last RTT</TableHead>
                  <TableHead>Loss</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Updated</TableHead>
                  {isAdmin && <TableHead className="text-right">Actions</TableHead>}
                </TableRow>
              </TableHeader>
              <TableBody>
                {clientTargets.map((t) => (
                  <TableRow
                    key={t.id}
                    className={`cursor-pointer hover:bg-muted/50 ${t.enabled ? "" : "opacity-60"}`}
                    onClick={() => openDetail(t)}
                  >
                    <TableCell>
                      <span className="text-sm font-medium">{clientDisplayName(t)}</span>
                      <span className="ml-2 font-mono text-xs text-muted-foreground">{t.mac_address}</span>
                    </TableCell>
                    <TableCell className="font-mono text-xs">{t.address || "—"}</TableCell>
                    {probingDeviceCell(t)}
                    {rttCell(t)}
                    {lossCell(t)}
                    {statusCell(t)}
                    {updatedCell(t)}
                    {actionsCell(t)}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Add internet target */}
      {isAdmin && (
        <Dialog open={addInternetOpen} onOpenChange={setAddInternetOpen}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Add internet target</DialogTitle>
            </DialogHeader>
            <form className="space-y-4" onSubmit={submitAddInternet}>
              <div className="space-y-2">
                <Label>Label (optional)</Label>
                <Input
                  value={internetForm.label}
                  onChange={(e) => setInternetForm({ ...internetForm, label: e.target.value })}
                  placeholder="e.g. Cloudflare DNS"
                />
              </div>
              <div className="space-y-2">
                <Label>Address</Label>
                <Input
                  value={internetForm.address}
                  onChange={(e) => setInternetForm({ ...internetForm, address: e.target.value })}
                  placeholder="1.1.1.1 or example.com"
                  required
                />
              </div>
              <div className="space-y-2">
                <Label>Probing device</Label>
                <select
                  className="flex h-8 w-full rounded-md border bg-transparent px-2 text-sm"
                  value={internetForm.device_id}
                  onChange={(e) => setInternetForm({ ...internetForm, device_id: e.target.value })}
                  required
                >
                  <option value="">Select device…</option>
                  {devices.map((d) => (
                    <option key={d.id} value={d.id}>
                      {d.identity || d.address}
                    </option>
                  ))}
                </select>
                <p className="text-xs text-muted-foreground">The RouterOS device that runs /ping.</p>
              </div>
              <Button type="submit" className="w-full">
                Add target
              </Button>
            </form>
          </DialogContent>
        </Dialog>
      )}

      {/* Watch client */}
      {isAdmin && (
        <Dialog open={watchOpen} onOpenChange={setWatchOpen}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Watch client</DialogTitle>
            </DialogHeader>
            <form className="space-y-4" onSubmit={submitWatch}>
              <div className="space-y-2">
                <Label>Known client</Label>
                <select
                  className="flex h-8 w-full rounded-md border bg-transparent px-2 text-sm"
                  value={watchForm.mac}
                  onChange={(e) => setWatchForm({ ...watchForm, mac: e.target.value })}
                >
                  <option value="">Select a client…</option>
                  {sortedClients.map((c) => (
                    <option key={c.mac_address} value={c.mac_address}>
                      {`${c.host_name || c.dns_name || c.mac_address} (${c.mac_address}${c.ip_address ? `, ${c.ip_address}` : ""})`}
                    </option>
                  ))}
                </select>
              </div>
              <div className="space-y-2">
                <Label>…or MAC address</Label>
                <Input
                  value={watchForm.freeMac}
                  onChange={(e) => setWatchForm({ ...watchForm, freeMac: e.target.value })}
                  placeholder="AA:BB:CC:DD:EE:FF"
                  className="font-mono"
                />
                <p className="text-xs text-muted-foreground">Takes precedence over the picker when filled.</p>
              </div>
              <div className="space-y-2">
                <Label>Label (optional)</Label>
                <Input
                  value={watchForm.label}
                  onChange={(e) => setWatchForm({ ...watchForm, label: e.target.value })}
                  placeholder="Defaults to the client hostname"
                />
              </div>
              <div className="space-y-2">
                <Label>Probing device (optional)</Label>
                <select
                  className="flex h-8 w-full rounded-md border bg-transparent px-2 text-sm"
                  value={watchForm.device_id}
                  onChange={(e) => setWatchForm({ ...watchForm, device_id: e.target.value })}
                >
                  <option value="">(auto — follow client)</option>
                  {devices.map((d) => (
                    <option key={d.id} value={d.id}>
                      {d.identity || d.address}
                    </option>
                  ))}
                </select>
              </div>
              <Button type="submit" className="w-full">
                Watch client
              </Button>
            </form>
          </DialogContent>
        </Dialog>
      )}

      {/* Edit target */}
      {isAdmin && (
        <Dialog open={!!editTarget} onOpenChange={(open) => !open && setEditTarget(null)}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>
                Edit {editTarget?.kind === "client" ? "client watch" : "internet target"}
              </DialogTitle>
            </DialogHeader>
            <form className="space-y-4" onSubmit={submitEdit}>
              <div className="space-y-2">
                <Label>Label</Label>
                <Input
                  value={editForm.label}
                  onChange={(e) => setEditForm({ ...editForm, label: e.target.value })}
                />
              </div>
              {editTarget?.kind === "internet" && (
                <div className="space-y-2">
                  <Label>Address</Label>
                  <Input
                    value={editForm.address}
                    onChange={(e) => setEditForm({ ...editForm, address: e.target.value })}
                    required
                  />
                </div>
              )}
              <div className="space-y-2">
                <Label>Probing device{editTarget?.kind === "client" ? " (optional)" : ""}</Label>
                <select
                  className="flex h-8 w-full rounded-md border bg-transparent px-2 text-sm"
                  value={editForm.device_id}
                  onChange={(e) => setEditForm({ ...editForm, device_id: e.target.value })}
                >
                  <option value="">
                    {editTarget?.kind === "client" ? "(auto — follow client)" : "Select device…"}
                  </option>
                  {devices.map((d) => (
                    <option key={d.id} value={d.id}>
                      {d.identity || d.address}
                    </option>
                  ))}
                </select>
              </div>
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={editForm.enabled}
                  onChange={(e) => setEditForm({ ...editForm, enabled: e.target.checked })}
                />
                Enabled (probed every cycle)
              </label>
              <Button type="submit" className="w-full">
                Save changes
              </Button>
            </form>
          </DialogContent>
        </Dialog>
      )}

      {/* Delete confirm */}
      {isAdmin && (
        <Dialog open={!!confirmDelete} onOpenChange={(open) => !open && setConfirmDelete(null)}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Delete target?</DialogTitle>
            </DialogHeader>
            <p className="text-sm">
              Removes{" "}
              <span className="font-medium">
                {confirmDelete
                  ? confirmDelete.kind === "client"
                    ? clientDisplayName(confirmDelete)
                    : confirmDelete.label || confirmDelete.address
                  : ""}
              </span>{" "}
              and all of its recorded samples. This cannot be undone.
            </p>
            <div className="mt-4 flex justify-end gap-2">
              <Button variant="outline" onClick={() => setConfirmDelete(null)}>
                Cancel
              </Button>
              <Button variant="destructive" onClick={doDelete}>
                Delete
              </Button>
            </div>
          </DialogContent>
        </Dialog>
      )}

      {/* Detail dialog */}
      <Dialog open={!!detailTarget} onOpenChange={(open) => !open && setDetailTarget(null)}>
        <DialogContent className="sm:max-w-3xl max-h-[85vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{detailTitle}</DialogTitle>
          </DialogHeader>
          {detailTarget && (
            <div className="space-y-4">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <p className="text-xs text-muted-foreground font-mono">
                  {detailTarget.kind === "client"
                    ? `${detailTarget.mac_address}${detailTarget.address ? ` · ${detailTarget.address}` : ""}`
                    : detailTarget.address}
                  {detailTarget.device_name && ` · via ${detailTarget.device_name}`}
                </p>
                <div className="flex gap-1">
                  {rangeKeys.map((r) => (
                    <Button
                      key={r}
                      size="sm"
                      variant={range === r ? "default" : "outline"}
                      onClick={() => setRange(r)}
                    >
                      {r}
                    </Button>
                  ))}
                </div>
              </div>

              {latestPing?.error && (
                <p className="text-xs text-muted-foreground">
                  Latest probe error: <span className="font-mono">{latestPing.error}</span>
                </p>
              )}

              {chartData.length === 0 ? (
                <p className="py-8 text-center text-sm text-muted-foreground">
                  {detailLoading
                    ? "Loading samples…"
                    : "No samples in this range yet. Samples appear within one polling cycle (default 30s)."}
                </p>
              ) : (
                <>
                  <div>
                    <p className="mb-1 text-xs font-medium text-muted-foreground">Round-trip time</p>
                    <ResponsiveContainer width="100%" height={240}>
                      <AreaChart data={chartData}>
                        <CartesianGrid strokeDasharray="3 3" />
                        <XAxis dataKey="time" tick={{ fontSize: 11 }} interval="preserveStartEnd" />
                        <YAxis tick={{ fontSize: 11 }} tickFormatter={(v) => `${v} ms`} />
                        <Tooltip formatter={(value) => formatMs(Number(value))} />
                        <Area
                          type="monotone"
                          dataKey="rtt"
                          stroke="#5A9CB5"
                          fill="#5A9CB5"
                          fillOpacity={0.2}
                          name="RTT avg"
                          isAnimationActive={false}
                        />
                      </AreaChart>
                    </ResponsiveContainer>
                  </div>
                  <div>
                    <p className="mb-1 text-xs font-medium text-muted-foreground">Packet loss</p>
                    <ResponsiveContainer width="100%" height={140}>
                      <AreaChart data={chartData}>
                        <CartesianGrid strokeDasharray="3 3" />
                        <XAxis dataKey="time" tick={{ fontSize: 11 }} interval="preserveStartEnd" />
                        <YAxis domain={[0, 100]} tick={{ fontSize: 11 }} tickFormatter={(v) => `${v}%`} />
                        <Tooltip formatter={(value) => formatLoss(Number(value))} />
                        <Area
                          type="monotone"
                          dataKey="loss"
                          stroke="#E8590C"
                          fill="#E8590C"
                          fillOpacity={0.2}
                          name="Loss"
                          isAnimationActive={false}
                        />
                      </AreaChart>
                    </ResponsiveContainer>
                  </div>
                </>
              )}

              {detailTarget.kind === "client" && signalData.length > 0 && (
                <div>
                  <p className="mb-1 text-xs font-medium text-muted-foreground">WiFi signal</p>
                  <ResponsiveContainer width="100%" height={140}>
                    <AreaChart data={signalData}>
                      <CartesianGrid strokeDasharray="3 3" />
                      <XAxis dataKey="time" tick={{ fontSize: 11 }} interval="preserveStartEnd" />
                      <YAxis domain={["auto", "auto"]} tick={{ fontSize: 11 }} tickFormatter={(v) => `${v} dBm`} />
                      <Tooltip formatter={(value) => `${value} dBm`} />
                      <Area
                        type="monotone"
                        dataKey="signal"
                        stroke="#FAAC68"
                        fill="#FAAC68"
                        fillOpacity={0.25}
                        name="Signal"
                        isAnimationActive={false}
                      />
                    </AreaChart>
                  </ResponsiveContainer>
                </div>
              )}

              {detailTarget.kind === "client" && (
                <div className="space-y-2">
                  <h3 className="text-sm font-medium">Events</h3>
                  {mergedEvents.length === 0 ? (
                    <p className="text-sm text-muted-foreground">
                      No correlated WiFi or network events in this range.
                    </p>
                  ) : (
                    <div className="space-y-1">
                      {mergedEvents.map((ev) => (
                        <div key={ev.key} className="flex items-center gap-3 rounded-md border p-2 text-xs">
                          <span
                            className="w-44 shrink-0 text-muted-foreground"
                            title={formatDateTime(ev.recorded_at)}
                          >
                            {timeAgo(ev.recorded_at)} · {formatDateTime(ev.recorded_at)}
                          </span>
                          {eventBadge(ev)}
                          <span className="min-w-0 truncate" title={ev.text}>
                            {ev.text}
                          </span>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              )}
            </div>
          )}
        </DialogContent>
      </Dialog>
    </div>
  );
}
