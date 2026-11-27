"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { AlertTriangle, Ban, ChevronDown, ChevronRight, ListTree, Pencil, Plus, Power, PowerOff, Radio, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useAuth } from "@/context/auth";
import {
  api,
  ApiError,
  type Device,
  type FlowBlockedSender,
  type FlowExporterBody,
  type FlowExporterKind,
  type FlowExporterState,
  type FlowExporterStatus,
  type FlowIface,
  type FlowIfaceRole,
  type FlowStatus,
  type FlowSuggestedExporter,
  type FlowUnknownSender,
} from "@/lib/api";
import { cn } from "@/lib/utils";
import { deviceName, errorMessage } from "../lib";
import { Notice } from "../notice";
import { fmtAgo, fmtCount, fmtSkew, protocolLabel } from "./flow-lib";
import { roleLabel } from "@/lib/device-role";
import { StopExportDialog } from "./rollback";

const KIND_LABEL: Record<FlowExporterKind, string> = {
  routeros: "RouterOS",
  opnsense: "OPNsense",
  other: "other",
};

const STATE_META: Record<FlowExporterState, { label: string; dot: string; hint: string }> = {
  ok: { label: "OK", dot: "bg-green-500", hint: "Datagrams received within the last 3 minutes" },
  stale: { label: "stale", dot: "bg-amber-500", hint: "No datagrams for 3 minutes or more" },
  never: { label: "no data yet", dot: "bg-gray-400", hint: "Accepted, but nothing received from this address yet" },
  // "disabled" on the wire. It only stops the NMS from reading the
  // datagrams: the device keeps exporting (and paying for it).
  disabled: {
    label: "ignored",
    dot: "bg-gray-400",
    hint: "Ignored: the NMS drops this address's datagrams — the device keeps exporting",
  },
};

export function StateDot({ state }: { state: FlowExporterState }) {
  const m = STATE_META[state] ?? STATE_META.never;
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap" title={m.hint}>
      <span className={cn("h-2 w-2 rounded-full", m.dot)} />
      <span className={cn(state === "disabled" && "line-through", state !== "ok" && "text-muted-foreground")}>{m.label}</span>
    </span>
  );
}

// ---- add / edit form ---------------------------------------------------------

interface ExporterForm {
  name: string;
  address: string;
  kind: FlowExporterKind;
  device_id: string;
  nat: string;
  sampling: string;
  enabled: boolean;
  // Why the address is blocked ("" = it is not): adding it then needs the
  // admin to say so explicitly.
  blocked: string;
}

const EMPTY_FORM: ExporterForm = {
  name: "",
  address: "",
  kind: "other",
  device_id: "",
  nat: "",
  sampling: "0",
  enabled: true,
  blocked: "",
};

function formFromExporter(e: FlowExporterStatus): ExporterForm {
  return {
    name: e.name,
    address: e.address,
    kind: e.kind,
    device_id: e.device_id ?? "",
    nat: e.nat_addresses.join(", "),
    sampling: String(e.sampling_override || 0),
    enabled: e.enabled,
    blocked: "",
  };
}

// A blocked sender, to be added again on the admin's say-so.
function formFromBlocked(b: FlowBlockedSender): ExporterForm {
  return {
    ...EMPTY_FORM,
    name: b.name || b.device_name || b.address,
    address: b.address,
    kind: b.kind,
    device_id: b.device_id,
    blocked: b.reason || "the sender was blocked",
  };
}

function formFromSender(u: FlowUnknownSender, sug?: FlowSuggestedExporter): ExporterForm {
  const kind: FlowExporterKind = sug?.kind ?? (u.device_id ? "routeros" : "other");
  return { ...EMPTY_FORM, name: sug?.name || u.device_name || u.address, address: u.address, kind, device_id: u.device_id ?? "" };
}

function formFromSuggestion(s: FlowSuggestedExporter): ExporterForm {
  return { ...EMPTY_FORM, name: s.name || s.address, address: s.address, kind: s.kind };
}

function bodyFromExporter(e: FlowExporterStatus, patch: Partial<FlowExporterBody> = {}): FlowExporterBody {
  return {
    name: e.name,
    address: e.address,
    kind: e.kind,
    device_id: e.device_id,
    enabled: e.enabled,
    sampling_override: e.sampling_override,
    nat_addresses: e.nat_addresses,
    ...patch,
  };
}

function ExporterDialog({
  open,
  onOpenChange,
  initial,
  editId,
  devices,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  initial: ExporterForm;
  editId: number | null;
  devices: Device[];
  onSaved: () => void;
}) {
  const { token } = useAuth();
  const [form, setForm] = useState<ExporterForm>(initial);
  const [err, setErr] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  // A blocked address is only added when the admin ticks "add it anyway".
  const [override, setOverride] = useState(false);
  const [prevInitial, setPrevInitial] = useState(initial);
  if (initial !== prevInitial) {
    setPrevInitial(initial);
    setForm(initial);
    setErr(null);
    setOverride(false);
  }
  const needsOverride = editId === null && form.blocked !== "";
  const sortedDevices = useMemo(() => [...devices].sort((a, b) => deviceName(a).localeCompare(deviceName(b))), [devices]);
  const set = (patch: Partial<ExporterForm>) => setForm((f) => ({ ...f, ...patch }));

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!token) return;
    const name = form.name.trim();
    const address = form.address.trim();
    const sampling = Number(form.sampling || "0");
    if (!name) return setErr("Name is required.");
    if (!address) return setErr("Address is required.");
    if (!Number.isInteger(sampling) || sampling < 0 || sampling > 65535) return setErr("Sampling override must be 0–65535.");
    const body: FlowExporterBody = {
      name,
      address,
      kind: form.kind,
      device_id: form.device_id || null,
      enabled: form.enabled,
      sampling_override: sampling,
      nat_addresses: form.nat
        .split(/[\s,]+/)
        .map((s) => s.trim())
        .filter(Boolean),
    };
    setSaving(true);
    setErr(null);
    try {
      if (editId === null) await api.flows.createExporter(token, needsOverride && override ? { ...body, unblock: true } : body);
      else await api.flows.updateExporter(token, editId, body);
      toast.success(editId === null ? `Exporter ${name} added` : `Exporter ${name} saved`);
      onOpenChange(false);
      onSaved();
    } catch (e2) {
      if (editId === null && e2 instanceof ApiError && e2.code === "sender_blocked") {
        // The server says the address is blocked: show why and ask.
        set({ blocked: e2.message.replace(/^sender \S+ is blocked: /, "") });
        setOverride(false);
      } else {
        setErr(errorMessage(e2));
      }
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{editId === null ? "Add flow exporter" : "Edit flow exporter"}</DialogTitle>
          <DialogDescription>
            Datagrams are accepted only from exporter addresses. A RouterOS exporter linked to its managed device gets
            interface names and port views automatically. Flow export belongs on routers: a switch or an access point
            gains nothing from it and pays for it in CPU.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="space-y-3">
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <Label htmlFor="fx-name">Name</Label>
              <Input id="fx-name" value={form.name} maxLength={64} onChange={(e) => set({ name: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="fx-address">Sender address</Label>
              <Input
                id="fx-address"
                className="font-mono"
                placeholder="192.0.2.10"
                value={form.address}
                onChange={(e) => {
                  // "Blocked" and the tick to add it anyway were about the
                  // address that was typed before: a new address is checked
                  // by the server again.
                  set({ address: e.target.value, blocked: "" });
                  setOverride(false);
                }}
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="fx-kind">Kind</Label>
              <select
                id="fx-kind"
                aria-describedby="fx-kind-hint"
                className="h-8 w-full rounded-lg border bg-transparent px-2 text-sm"
                value={form.kind}
                onChange={(e) => set({ kind: e.target.value as FlowExporterKind })}
              >
                <option value="routeros">RouterOS (IPFIX / NetFlow)</option>
                <option value="opnsense">OPNsense (NetFlow v9)</option>
                <option value="other">Other</option>
              </select>
              <p id="fx-kind-hint" className="text-xs text-muted-foreground">
                OPNsense: drops ng_netflow duplicate records, ignores v9 sequence gaps and keeps only internal hosts on the
                local side of its views. Pick it for any OPNsense firewall.
              </p>
            </div>
            <div className="space-y-1">
              <Label htmlFor="fx-device">Managed device (optional)</Label>
              <select
                id="fx-device"
                className="h-8 w-full rounded-lg border bg-transparent px-2 text-sm"
                value={form.device_id}
                onChange={(e) => {
                  const d = devices.find((x) => x.id === e.target.value);
                  set({
                    device_id: e.target.value,
                    ...(d && !form.name.trim() ? { name: deviceName(d) } : {}),
                    ...(d && !form.address.trim() ? { address: d.address } : {}),
                  });
                }}
              >
                <option value="">None</option>
                {sortedDevices.map((d) => (
                  <option key={d.id} value={d.id}>
                    {deviceName(d)}
                  </option>
                ))}
              </select>
            </div>
          </div>
          <div className="space-y-1">
            <Label htmlFor="fx-nat">NAT addresses</Label>
            <Input
              id="fx-nat"
              className="font-mono"
              placeholder="optional, e.g. 192.0.2.20, 198.51.100.7"
              value={form.nat}
              onChange={(e) => set({ nat: e.target.value })}
            />
            <p className="text-xs text-muted-foreground">
              Addresses this exporter NATs its hosts to (its other legs). Another exporter&apos;s rows with these addresses
              are labelled “NAT for {form.name.trim() || "this exporter"}”.
            </p>
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <Label htmlFor="fx-sampling">Sampling override</Label>
              <Input
                id="fx-sampling"
                type="number"
                min={0}
                max={65535}
                value={form.sampling}
                onChange={(e) => set({ sampling: e.target.value })}
              />
              <p className="text-xs text-muted-foreground">0 = use the rate the exporter announces (none = unsampled).</p>
            </div>
            <label
              className="flex items-center gap-2 self-start pt-6 text-sm"
              title="Off: the NMS ignores its datagrams. The device keeps exporting."
            >
              <input type="checkbox" checked={form.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
              Accept its datagrams
            </label>
          </div>
          {needsOverride && (
            <Notice kind="warn">
              <p>This sender is blocked: {form.blocked}.</p>
              <label className="mt-1.5 flex items-center gap-2 font-medium">
                <input type="checkbox" checked={override} onChange={(e) => setOverride(e.target.checked)} />
                Add it anyway (its datagrams are accepted and decoded from then on)
              </label>
            </Notice>
          )}
          {err && <Notice kind="error">{err}</Notice>}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={saving || (needsOverride && !override)}>
              {saving ? "Saving…" : editId === null ? (needsOverride ? "Add anyway" : "Add exporter") : "Save"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- interfaces dialog ------------------------------------------------------

const ROLE_OPTIONS: { value: FlowIfaceRole; label: string }[] = [
  { value: "", label: "—" },
  { value: "lan", label: "LAN" },
  { value: "wan", label: "WAN" },
  { value: "vpn", label: "VPN" },
  { value: "other", label: "other" },
];

function IfaceRow({ exporterId, iface, isAdmin, onSaved }: { exporterId: number; iface: FlowIface; isAdmin: boolean; onSaved: (f: FlowIface) => void }) {
  const { token } = useAuth();
  const [name, setName] = useState(iface.name);
  const [role, setRole] = useState<FlowIfaceRole>(iface.role);
  const [saving, setSaving] = useState(false);
  const dirty = name !== iface.name || role !== iface.role;
  const save = async () => {
    if (!token) return;
    setSaving(true);
    try {
      const f = await api.flows.updateIface(token, exporterId, iface.if_index, { name: name.trim(), role });
      toast.success(`if#${iface.if_index} saved`);
      onSaved(f);
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setSaving(false);
    }
  };
  const typeText = [iface.type, iface.vlan_id ? `vlan ${iface.vlan_id}` : "", iface.parent ? `on ${iface.parent}` : ""].filter(Boolean).join(" · ");
  return (
    <TableRow>
      <TableCell className="font-mono text-xs">{iface.if_index}</TableCell>
      <TableCell className="min-w-[8rem]">
        {isAdmin ? (
          <Input
            aria-label={`Name of if#${iface.if_index}`}
            className="h-7 font-mono text-xs"
            value={name}
            placeholder={`if#${iface.if_index}`}
            onChange={(e) => setName(e.target.value)}
          />
        ) : (
          <span className="font-mono text-xs">{iface.name || `if#${iface.if_index}`}</span>
        )}
      </TableCell>
      <TableCell>
        {isAdmin ? (
          <select
            aria-label={`Role of if#${iface.if_index}`}
            className="h-7 rounded-md border bg-transparent px-1.5 text-xs"
            value={role}
            onChange={(e) => setRole(e.target.value as FlowIfaceRole)}
          >
            {ROLE_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        ) : (
          <span className="text-xs">{ROLE_OPTIONS.find((o) => o.value === iface.role)?.label ?? "—"}</span>
        )}
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">{typeText || "—"}</TableCell>
      <TableCell className="max-w-[14rem] truncate font-mono text-[11px] text-muted-foreground" title={iface.hint}>
        {iface.hint || "—"}
      </TableCell>
      <TableCell className="text-xs text-muted-foreground" title={iface.source}>
        {fmtAgo(iface.last_seen)}
      </TableCell>
      {isAdmin && (
        <TableCell>
          <Button size="xs" variant="outline" disabled={!dirty || saving} onClick={save}>
            {saving ? "…" : "Save"}
          </Button>
        </TableCell>
      )}
    </TableRow>
  );
}

function InterfacesDialog({
  exporter,
  isAdmin,
  onClose,
  onSaved,
}: {
  exporter: FlowExporterStatus | null;
  isAdmin: boolean;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { token } = useAuth();
  const [state, setState] = useState<{ id: number | null; ifaces: FlowIface[] | null; error: string | null }>({ id: null, ifaces: null, error: null });
  const id = exporter?.id ?? null;
  useEffect(() => {
    if (!token || id === null) return;
    let alive = true;
    api.flows
      .ifaces(token, id)
      .then((r) => alive && setState({ id, ifaces: r.interfaces, error: null }))
      .catch((e) => alive && setState({ id, ifaces: null, error: errorMessage(e) }));
    return () => {
      alive = false;
    };
  }, [token, id]);
  const current = state.id === id ? state : { id, ifaces: null, error: null };
  const replace = useCallback(
    (f: FlowIface) => {
      setState((s) => ({ ...s, ifaces: s.ifaces?.map((x) => (x.if_index === f.if_index ? f : x)) ?? null }));
      onSaved();
    },
    [onSaved],
  );

  return (
    <Dialog open={exporter !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Interfaces of {exporter?.name}</DialogTitle>
          <DialogDescription>
            ifIndexes as the exporter reports them.{" "}
            {exporter?.device_id
              ? "Names come from the managed device (a name set here overrides it)."
              : "This exporter is not a managed device, so its interfaces are learned from traffic — name them and set the role (LAN / WAN) so its views get the right direction."}{" "}
            Hint = address prefixes seen entering the interface.
          </DialogDescription>
        </DialogHeader>
        {current.error && <Notice kind="error">Could not load interfaces: {current.error}</Notice>}
        {!current.ifaces && !current.error ? (
          <div className="h-32 animate-pulse rounded-md bg-muted" />
        ) : current.ifaces && current.ifaces.length === 0 ? (
          <Notice>No interfaces seen yet — they appear with the first flows.</Notice>
        ) : current.ifaces ? (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ifIndex</TableHead>
                <TableHead>Name</TableHead>
                <TableHead>Role</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Hint</TableHead>
                <TableHead>Last seen</TableHead>
                {isAdmin && <TableHead />}
              </TableRow>
            </TableHeader>
            <TableBody>
              {current.ifaces.map((f) => (
                <IfaceRow key={`${f.if_index}|${f.name}|${f.role}`} exporterId={exporter!.id} iface={f} isAdmin={isAdmin} onSaved={replace} />
              ))}
            </TableBody>
          </Table>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

// ---- status card ------------------------------------------------------------

function errorsOf(e: FlowExporterStatus): number {
  return (e.counters?.decode_errors ?? 0) + (e.counters?.template_misses ?? 0);
}

function countersTitle(e: FlowExporterStatus): string {
  return Object.entries(e.counters ?? {})
    .map(([k, v]) => `${k.replace(/_/g, " ")}: ${v}`)
    .join("\n");
}

// ExporterStatusCard: every configured exporter with its health, plus (for
// admins) unknown senders and suggested exporters with one-click "Add", the
// blocked senders with their reason, and accept / ignore / edit / interfaces
// / delete actions. Collapsed while every exporter is OK and nothing needs
// an admin's attention.
//
// Nothing here stops a device from exporting: "ignore" and "delete" only
// decide what the NMS does with the datagrams. Stopping the export is done
// on the device, with the rollback steps (StopExportDialog).
export function ExporterStatusCard({
  status,
  isAdmin,
  devices,
  onChanged,
}: {
  status: FlowStatus | null;
  isAdmin: boolean;
  devices: Device[];
  onChanged: () => void;
}) {
  const { token } = useAuth();
  const exporters = useMemo(() => status?.exporters ?? [], [status]);
  const unknown = status?.unknown_senders ?? [];
  const suggested = status?.suggested_exporters ?? [];
  const blocked = status?.blocked_senders ?? [];
  const blockedAddrs = new Set(blocked.map((b) => b.address));
  // A blocked sender is shown as blocked, not as an unknown one to add.
  const unknownOpen = unknown.filter((u) => !blockedAddrs.has(u.address));
  const unknownAddrs = new Set(unknown.map((u) => u.address));
  const suggestedOnly = suggested.filter((s) => !unknownAddrs.has(s.address) && !blockedAddrs.has(s.address));
  const allOk = exporters.length > 0 && exporters.every((e) => e.state === "ok");
  // A device that must not export and does — blocked on its first datagram,
  // or accepted earlier — is worth opening the card for.
  const exportingAnyway = blocked.some((b) => b.source === "auto" && b.last_seen) || exporters.some((e) => !!e.export_warning);
  const attention = isAdmin && (unknownOpen.length > 0 || suggestedOnly.length > 0 || exportingAnyway);
  // Collector off: nothing is received, so nothing needs attention yet —
  // start collapsed.
  const off = status?.enabled === false;
  const [openOverride, setOpenOverride] = useState<boolean | null>(null);
  const open = openOverride ?? (!off && (!allOk || attention));

  const [dialog, setDialog] = useState<{ form: ExporterForm; editId: number | null } | null>(null);
  const [ifaceOf, setIfaceOf] = useState<FlowExporterStatus | null>(null);
  const [deleting, setDeleting] = useState<FlowExporterStatus | null>(null);
  const [busy, setBusy] = useState<number | null>(null);
  const [unblocking, setUnblocking] = useState<string | null>(null);
  // The device whose rollback steps are shown.
  const [stopping, setStopping] = useState<{ id: string; name: string } | null>(null);
  const byDevice = useMemo(() => new Map(devices.map((d) => [d.id, d])), [devices]);

  if (!status) return null;
  if (exporters.length === 0 && !isAdmin) return null;

  const toggle = async (e: FlowExporterStatus) => {
    if (!token) return;
    setBusy(e.id);
    try {
      await api.flows.updateExporter(token, e.id, bodyFromExporter(e, { enabled: !e.enabled }));
      toast.success(e.enabled ? `Datagrams from ${e.name} are ignored now — the device keeps exporting` : `Datagrams from ${e.name} are accepted again`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };
  const remove = async () => {
    if (!token || !deleting) return;
    setBusy(deleting.id);
    try {
      await api.flows.deleteExporter(token, deleting.id);
      toast.success(`Exporter ${deleting.name} deleted — ${deleting.address} is blocked now`);
      setDeleting(null);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };
  const unblock = async (b: FlowBlockedSender) => {
    if (!token) return;
    setUnblocking(b.address);
    try {
      await api.flows.unblockSender(token, b.address);
      toast.success(`${b.address} is no longer blocked`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setUnblocking(null);
    }
  };
  const stopFor = (deviceId: string | null | undefined, fallbackName: string) => {
    if (!deviceId) return;
    const dev = byDevice.get(deviceId);
    setStopping({ id: deviceId, name: dev ? deviceName(dev) : fallbackName });
  };

  const okCount = exporters.filter((e) => e.state === "ok").length;
  const summary =
    exporters.length === 0
      ? off
        ? "Collector off"
        : "No exporters yet"
      : `${exporters.length} exporter${exporters.length === 1 ? "" : "s"} · ${
          off ? "collector off" : okCount === exporters.length ? "all OK" : `${okCount} OK`
        }`;

  return (
    <Card className="min-w-0">
      <CardHeader className="gap-1">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <button
            type="button"
            onClick={() => setOpenOverride(!open)}
            aria-expanded={open}
            className="flex min-w-0 items-center gap-1.5 text-left"
          >
            {open ? <ChevronDown className="h-4 w-4 shrink-0" /> : <ChevronRight className="h-4 w-4 shrink-0" />}
            <CardTitle className="flex items-center gap-2 text-base">
              <Radio className="h-4 w-4 text-muted-foreground" />
              Flow exporters
            </CardTitle>
            <span className="truncate text-xs text-muted-foreground">· {summary}</span>
          </button>
          {isAdmin && (
            <Button size="sm" variant="outline" onClick={() => setDialog({ form: { ...EMPTY_FORM }, editId: null })}>
              <Plus /> Add exporter
            </Button>
          )}
        </div>
      </CardHeader>
      {open && (
        <CardContent className="space-y-3">
          {exporters.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Exporter</TableHead>
                  <TableHead title="State and time of the last datagram">State</TableHead>
                  <TableHead>Address</TableHead>
                  <TableHead>Protocol</TableHead>
                  <TableHead className="text-right" title="Flow records per minute (5-minute average)">
                    Flows/min
                  </TableHead>
                  <TableHead title="Live templates · ok / waiting (data without a template) / persisted (loaded from the DB) / n/a">Templates</TableHead>
                  <TableHead>Sampling</TableHead>
                  <TableHead className="text-right" title="Decode errors + data sets without a template">
                    Errors
                  </TableHead>
                  <TableHead title="Exporter clock minus NMS clock">Clock skew</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {exporters.map((e) => {
                  const dev = e.device_id ? byDevice.get(e.device_id) : undefined;
                  const errs = errorsOf(e);
                  return (
                    <TableRow key={e.id}>
                      <TableCell className="max-w-[16rem]">
                        <div className={cn("truncate font-medium", !e.enabled && "text-muted-foreground line-through")}>{e.name}</div>
                        <div className="truncate text-[11px] text-muted-foreground">
                          {KIND_LABEL[e.kind] ?? e.kind}
                          {dev ? ` · ${deviceName(dev)}` : ""}
                          {e.auto ? " · auto-accepted" : ""}
                          {e.nat_addresses.length ? ` · NAT ${e.nat_addresses.join(", ")}` : ""}
                        </div>
                        {e.last_error && (
                          <div className="truncate text-[11px] text-red-600 dark:text-red-400" title={e.last_error}>
                            {e.last_error}
                            {e.last_error_at ? ` (${fmtAgo(e.last_error_at)})` : ""}
                          </div>
                        )}
                        {e.export_warning && (
                          <div className="mt-0.5 flex items-start gap-1 whitespace-normal text-[11px] text-amber-800 dark:text-amber-300">
                            <AlertTriangle className="mt-px h-3 w-3 shrink-0" />
                            <span>
                              {roleLabel(e.device_role) || "This device"} — should not export flows: {e.export_warning}.
                              {e.device_id && (
                                <>
                                  {" "}
                                  <button
                                    type="button"
                                    className="underline underline-offset-2"
                                    onClick={() => stopFor(e.device_id, e.name)}
                                  >
                                    How to stop it
                                  </button>
                                </>
                              )}
                            </span>
                          </div>
                        )}
                      </TableCell>
                      <TableCell className="text-xs">
                        <StateDot state={e.state} />
                        <div className="whitespace-nowrap text-[11px] text-muted-foreground">{fmtAgo(e.last_seen)}</div>
                      </TableCell>
                      <TableCell className="font-mono text-xs">{e.address}</TableCell>
                      <TableCell className="text-xs">{e.protocol ? protocolLabel(e.protocol) : "—"}</TableCell>
                      <TableCell className="text-right font-mono text-xs tabular-nums">{fmtCount(e.per_minute?.flows ?? 0)}</TableCell>
                      <TableCell className="text-xs">
                        <span className={cn(e.template_state === "waiting" && "text-amber-700 dark:text-amber-300")}>
                          {e.template_state === "n/a" ? "n/a" : `${e.templates} · ${e.template_state}`}
                        </span>
                      </TableCell>
                      <TableCell className="text-xs" title={e.sampling_override ? `Override 1:${e.sampling_override}` : "As announced by the exporter"}>
                        {e.sampling_rate > 1 ? `1:${e.sampling_rate}` : "none"}
                        {e.sampling_override ? " (override)" : ""}
                      </TableCell>
                      <TableCell
                        className={cn("text-right font-mono text-xs tabular-nums", errs > 0 && "text-amber-700 dark:text-amber-300")}
                        title={countersTitle(e)}
                      >
                        {fmtCount(errs)}
                      </TableCell>
                      <TableCell className="font-mono text-xs tabular-nums">{fmtSkew(e.clock_skew_ms)}</TableCell>
                      <TableCell>
                        <div className="flex items-center justify-end gap-0.5">
                          <Button size="icon-sm" variant="ghost" title="Interfaces" aria-label={`Interfaces of ${e.name}`} onClick={() => setIfaceOf(e)}>
                            <ListTree />
                          </Button>
                          {isAdmin && (
                            <>
                              <Button
                                size="icon-sm"
                                variant="ghost"
                                title={e.enabled ? "Ignore datagrams (device keeps exporting)" : "Accept datagrams again"}
                                aria-label={`${e.enabled ? "Ignore datagrams from" : "Accept datagrams from"} ${e.name}`}
                                disabled={busy === e.id}
                                onClick={() => toggle(e)}
                              >
                                {e.enabled ? <PowerOff /> : <Power />}
                              </Button>
                              <Button
                                size="icon-sm"
                                variant="ghost"
                                title="Edit"
                                aria-label={`Edit ${e.name}`}
                                onClick={() => setDialog({ form: formFromExporter(e), editId: e.id })}
                              >
                                <Pencil />
                              </Button>
                              <Button
                                size="icon-sm"
                                variant="ghost"
                                title="Delete"
                                aria-label={`Delete ${e.name}`}
                                disabled={busy === e.id}
                                onClick={() => setDeleting(e)}
                              >
                                <Trash2 />
                              </Button>
                            </>
                          )}
                        </div>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
          {exporters.length === 0 && isAdmin && (
            <p className="text-xs text-muted-foreground">
              {off ? (
                <>
                  No exporters configured. Flow collector is off — nothing is received until{" "}
                  <code className="font-mono">MIKROTIK_NMS_FLOW_LISTEN</code> is set.
                </>
              ) : status.auto_accept_classes === "router" ? (
                <>
                  No exporters configured. A managed router (a strong device with an internet or VPN egress of its own) is
                  accepted automatically when it sends from its management address; add anything else (a firewall, a
                  probe) here. Switches and access points are never accepted.
                </>
              ) : (
                <>
                  No exporters configured. Nothing is accepted automatically: senders show up below as unknown until you
                  add them. Flow export belongs on routers and firewalls — switches and access points are blocked.
                </>
              )}
            </p>
          )}

          {isAdmin && unknownOpen.length > 0 && (
            <div className="space-y-2">
              <Notice kind="warn">
                {unknownOpen.length} unknown sender{unknownOpen.length === 1 ? "" : "s"} — add them as exporters to accept their
                flows.
              </Notice>
              <ul className="divide-y rounded-md border">
                {unknownOpen.map((u) => {
                  const sug = suggested.find((s) => s.address === u.address);
                  // The suggested name falls back to the address itself; don't repeat it.
                  const label = [sug?.name, u.device_name].find((n) => n && n !== u.address);
                  return (
                    <li key={u.address} className="flex flex-wrap items-center justify-between gap-2 px-3 py-2 text-xs">
                      <div className="min-w-0">
                        <span className="font-mono font-medium">{u.address}</span>
                        {label && <span className="ml-1.5">{label}</span>}
                        <div className="text-muted-foreground">
                          {u.protocol ? protocolLabel(u.protocol) : "unrecognised protocol"} · {fmtCount(u.datagrams)} datagrams · last{" "}
                          {fmtAgo(u.last_seen)}
                          {sug?.reason ? ` · ${sug.reason}` : ""}
                        </div>
                        {u.device_role && (
                          <div className="text-muted-foreground" title={u.device_role_reason}>
                            Managed device: {roleLabel(u.device_role)}
                            {u.device_role === "unknown" && u.device_role_reason ? ` — ${u.device_role_reason}` : ""}
                          </div>
                        )}
                      </div>
                      <Button size="xs" variant="outline" onClick={() => setDialog({ form: formFromSender(u, sug), editId: null })}>
                        <Plus /> Add
                      </Button>
                    </li>
                  );
                })}
              </ul>
            </div>
          )}

          {isAdmin && suggestedOnly.length > 0 && (
            <div className="space-y-2">
              <p className="text-xs font-medium text-muted-foreground">Suggested exporters</p>
              <ul className="divide-y rounded-md border">
                {suggestedOnly.map((s) => (
                  <li key={s.address} className="flex flex-wrap items-center justify-between gap-2 px-3 py-2 text-xs">
                    <div className="min-w-0">
                      {s.name && s.name !== s.address && <span className="font-medium">{s.name} </span>}
                      <span className={cn("font-mono", s.name && s.name !== s.address ? "text-muted-foreground" : "font-medium")}>
                        {s.address}
                      </span>
                      <div className="text-muted-foreground">
                        {KIND_LABEL[s.kind] ?? s.kind} · {s.reason}
                      </div>
                    </div>
                    <Button size="xs" variant="outline" onClick={() => setDialog({ form: formFromSuggestion(s), editId: null })}>
                      <Plus /> Add
                    </Button>
                  </li>
                ))}
              </ul>
            </div>
          )}

          {blocked.length > 0 && (
            <div className="space-y-2">
              <p className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
                <Ban className="h-3.5 w-3.5" />
                Blocked senders — their datagrams are dropped before decoding
              </p>
              <ul className="divide-y rounded-md border">
                {blocked.map((b) => {
                  const label = [b.name, b.device_name].find((n) => n && n !== b.address);
                  const auto = b.source === "auto";
                  // A device that still must not export is blocked again by
                  // its next datagram: "Unblock" alone would do nothing.
                  const stillNever = auto && b.still_never;
                  return (
                    <li key={b.address} className="flex flex-wrap items-start justify-between gap-2 px-3 py-2 text-xs">
                      <div className="min-w-0 flex-1">
                        <span className="font-mono font-medium">{b.address}</span>
                        {label && <span className="ml-1.5">{label}</span>}
                        <div className={cn(auto && b.last_seen ? "text-amber-800 dark:text-amber-300" : "text-muted-foreground")}>
                          blocked: {b.reason || "no reason recorded"}
                        </div>
                        <div className="text-muted-foreground">
                          {b.datagrams > 0
                            ? `${fmtCount(b.datagrams)} datagrams dropped · last ${fmtAgo(b.last_seen)}`
                            : "nothing received since it was blocked"}
                          {auto && b.last_seen ? " · the device is exporting: stop it on the device" : ""}
                        </div>
                      </div>
                      <div className="flex flex-wrap items-center gap-1">
                        {b.device_id && (
                          <Button size="xs" variant="outline" onClick={() => stopFor(b.device_id, b.name || b.address)}>
                            How to stop the export
                          </Button>
                        )}
                        {isAdmin && !stillNever && (
                          <Button
                            size="xs"
                            variant="ghost"
                            title="Remove the block: the sender is listed as unknown again"
                            disabled={unblocking === b.address}
                            onClick={() => unblock(b)}
                          >
                            Unblock
                          </Button>
                        )}
                        {isAdmin && (
                          <Button size="xs" variant="ghost" onClick={() => setDialog({ form: formFromBlocked(b), editId: null })}>
                            Add anyway…
                          </Button>
                        )}
                      </div>
                    </li>
                  );
                })}
              </ul>
            </div>
          )}
        </CardContent>
      )}

      {isAdmin && (
        <ExporterDialog
          open={dialog !== null}
          onOpenChange={(o) => !o && setDialog(null)}
          initial={dialog?.form ?? EMPTY_FORM}
          editId={dialog?.editId ?? null}
          devices={devices}
          onSaved={onChanged}
        />
      )}
      <InterfacesDialog exporter={ifaceOf} isAdmin={isAdmin} onClose={() => setIfaceOf(null)} onSaved={onChanged} />
      <StopExportDialog deviceId={stopping?.id ?? null} deviceName={stopping?.name ?? ""} onClose={() => setStopping(null)} />
      <Dialog open={deleting !== null} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete exporter {deleting?.name}?</DialogTitle>
            <DialogDescription>
              Its configuration, interface names and views are removed now; its stored flow history is purged by the next
              retention sweep. The address {deleting?.address} is then blocked: its datagrams are dropped before decoding, and
              it is not accepted again until you unblock it or add it again. Deleting does not stop the device from
              exporting — that is done on the device, with the rollback steps.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={remove} disabled={busy !== null}>
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}
