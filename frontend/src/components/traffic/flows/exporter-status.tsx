"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { ChevronDown, ChevronRight, ListTree, Pencil, Plus, Power, PowerOff, Radio, Trash2 } from "lucide-react";
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
  type Device,
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

const KIND_LABEL: Record<FlowExporterKind, string> = {
  routeros: "RouterOS",
  opnsense: "OPNsense",
  other: "other",
};

const STATE_META: Record<FlowExporterState, { label: string; dot: string; hint: string }> = {
  ok: { label: "OK", dot: "bg-green-500", hint: "Datagrams received within the last 3 minutes" },
  stale: { label: "stale", dot: "bg-amber-500", hint: "No datagrams for 3 minutes or more" },
  never: { label: "no data yet", dot: "bg-gray-400", hint: "Enabled, but nothing received from this address yet" },
  disabled: { label: "disabled", dot: "bg-gray-400", hint: "Disabled: datagrams from this address are dropped" },
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
}

const EMPTY_FORM: ExporterForm = { name: "", address: "", kind: "other", device_id: "", nat: "", sampling: "0", enabled: true };

function formFromExporter(e: FlowExporterStatus): ExporterForm {
  return {
    name: e.name,
    address: e.address,
    kind: e.kind,
    device_id: e.device_id ?? "",
    nat: e.nat_addresses.join(", "),
    sampling: String(e.sampling_override || 0),
    enabled: e.enabled,
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
  const [prevInitial, setPrevInitial] = useState(initial);
  if (initial !== prevInitial) {
    setPrevInitial(initial);
    setForm(initial);
    setErr(null);
  }
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
      if (editId === null) await api.flows.createExporter(token, body);
      else await api.flows.updateExporter(token, editId, body);
      toast.success(editId === null ? `Exporter ${name} added` : `Exporter ${name} saved`);
      onOpenChange(false);
      onSaved();
    } catch (e2) {
      setErr(errorMessage(e2));
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
            interface names and port views automatically.
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
                onChange={(e) => set({ address: e.target.value })}
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
            <label className="flex items-center gap-2 self-start pt-6 text-sm">
              <input type="checkbox" checked={form.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
              Enabled (accept its datagrams)
            </label>
          </div>
          {err && <Notice kind="error">{err}</Notice>}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={saving}>
              {saving ? "Saving…" : editId === null ? "Add exporter" : "Save"}
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
// admins) unknown senders and suggested exporters with one-click "Add", and
// enable / edit / interfaces / delete actions. Collapsed while every
// exporter is OK and nothing needs an admin's attention.
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
  const unknownAddrs = new Set(unknown.map((u) => u.address));
  const suggestedOnly = suggested.filter((s) => !unknownAddrs.has(s.address));
  const allOk = exporters.length > 0 && exporters.every((e) => e.state === "ok");
  const attention = isAdmin && (unknown.length > 0 || suggestedOnly.length > 0);
  // Collector off: nothing is received, so nothing needs attention yet —
  // start collapsed.
  const off = status?.enabled === false;
  const [openOverride, setOpenOverride] = useState<boolean | null>(null);
  const open = openOverride ?? (!off && (!allOk || attention));

  const [dialog, setDialog] = useState<{ form: ExporterForm; editId: number | null } | null>(null);
  const [ifaceOf, setIfaceOf] = useState<FlowExporterStatus | null>(null);
  const [deleting, setDeleting] = useState<FlowExporterStatus | null>(null);
  const [busy, setBusy] = useState<number | null>(null);
  const byDevice = useMemo(() => new Map(devices.map((d) => [d.id, d])), [devices]);

  if (!status) return null;
  if (exporters.length === 0 && !isAdmin) return null;

  const toggle = async (e: FlowExporterStatus) => {
    if (!token) return;
    setBusy(e.id);
    try {
      await api.flows.updateExporter(token, e.id, bodyFromExporter(e, { enabled: !e.enabled }));
      toast.success(`${e.name} ${e.enabled ? "disabled" : "enabled"}`);
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
      toast.success(`Exporter ${deleting.name} deleted`);
      setDeleting(null);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(null);
    }
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
                                title={e.enabled ? "Disable" : "Enable"}
                                aria-label={`${e.enabled ? "Disable" : "Enable"} ${e.name}`}
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
              ) : (
                <>
                  No exporters configured. RouterOS devices that are managed here are accepted automatically when they send
                  from their management address; add anything else (OPNsense, probes) here.
                </>
              )}
            </p>
          )}

          {isAdmin && unknown.length > 0 && (
            <div className="space-y-2">
              <Notice kind="warn">
                {unknown.length} unknown sender{unknown.length === 1 ? "" : "s"} — add them as exporters to accept their flows.
              </Notice>
              <ul className="divide-y rounded-md border">
                {unknown.map((u) => {
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
      <Dialog open={deleting !== null} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete exporter {deleting?.name}?</DialogTitle>
            <DialogDescription>
              Its configuration, interface names and views are removed now; its stored flow history is purged by the next
              retention sweep. Datagrams from {deleting?.address} are then treated as an unknown sender.
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
