"use client";

import { useCallback, useMemo, useState } from "react";
import { Check, ChevronDown, ChevronRight, Copy, Plus, Split } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useAuth } from "@/context/auth";
import { api, type Device, type FlowPoint, type FlowPointSuggestion, type FlowStatus } from "@/lib/api";
import { cn } from "@/lib/utils";
import { usePolledFetch } from "../hooks";
import { deviceName, errorMessage } from "../lib";
import { Notice } from "../notice";
import { copyText, listenPort } from "./flow-lib";

const SUGGESTIONS_REFRESH_MS = 300_000;

function CodeBlock({ title, lines }: { title: string; lines: string[] }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    const ok = await copyText(lines.join("\n"));
    if (ok) {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } else toast.error("Copy failed — select the text instead");
  };
  return (
    <div className="min-w-0 space-y-1">
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs font-medium">{title}</span>
        <Button size="xs" variant="ghost" onClick={copy} aria-label={`Copy ${title}`}>
          {copied ? <Check /> : <Copy />}
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      <pre className="overflow-x-auto rounded-md border bg-muted/40 px-3 py-2 font-mono text-[11px] leading-relaxed whitespace-pre">
        {lines.join("\n")}
      </pre>
    </div>
  );
}

// SetupInstructions: RouterOS commands (apply + rollback) and the OPNsense
// steps from /flows/setup, for the NMS address and port the backend reports.
function SetupInstructions({ devices, defaultDevice }: { devices: Device[]; defaultDevice: string }) {
  const { token } = useAuth();
  const [device, setDevice] = useState(defaultDevice);
  const fetcher = useCallback(() => api.flows.setup(token!, device || undefined), [token, device]);
  const { data, error } = usePolledFetch(token ? `fsetup|${device}` : null, fetcher, null, { group: "fsetup" });
  const sorted = useMemo(() => [...devices].sort((a, b) => deviceName(a).localeCompare(deviceName(b))), [devices]);

  if (error && !data) return <Notice kind="error">Could not load setup instructions: {error}</Notice>;
  if (!data) return <div className="h-24 animate-pulse rounded-md bg-muted" />;
  return (
    <div className="grid min-w-0 gap-4 lg:grid-cols-2">
      <div className="min-w-0 space-y-2">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h4 className="text-sm font-medium">RouterOS (IPFIX)</h4>
          <select
            aria-label="RouterOS device"
            title="Pins src-address to the device's management address, so the NMS recognises it as that managed device"
            className="h-7 max-w-[220px] rounded-md border bg-background px-2 text-xs"
            value={device}
            onChange={(e) => setDevice(e.target.value)}
          >
            <option value="">Any device (no src-address)</option>
            {sorted.map((d) => (
              <option key={d.id} value={d.id}>
                {deviceName(d)}
              </option>
            ))}
          </select>
        </div>
        <CodeBlock title="Apply" lines={data.routeros.apply} />
        <CodeBlock title="Rollback" lines={data.routeros.rollback} />
        <p className="text-[11px] text-muted-foreground">
          Traffic Flow sees what the router&apos;s CPU forwards or terminates; hardware-offloaded bridging and FastTrack
          bypass it.
        </p>
      </div>
      <div className="min-w-0 space-y-2">
        <h4 className="text-sm font-medium">OPNsense (NetFlow {data.opnsense.version})</h4>
        <ol className="ml-4 list-decimal space-y-0.5 text-xs">
          {data.opnsense.steps.map((s, i) => (
            <li key={i}>{s}</li>
          ))}
        </ol>
        <p className="text-[11px] text-muted-foreground">
          Destination <span className="font-mono">{data.opnsense.destination}</span> · active timeout {data.opnsense.active_timeout} s ·
          inactive timeout {data.opnsense.inactive_timeout} s. Then add the firewall under “Flow exporters” (it shows up as an
          unknown sender).
        </p>
      </div>
      {data.advertise_source === "unknown" && (
        <Notice kind="warn" className="lg:col-span-2">
          The NMS could not work out its own address — replace &lt;NMS-IP&gt;, or set “Flow advertise address” in Settings.
        </Notice>
      )}
    </div>
  );
}

// Suggested derived views (admin): ports of devices that cannot export flows,
// seen through a neighbouring exporter.
function Suggestions({ pointsVersion, onChanged }: { pointsVersion: string; onChanged: () => void }) {
  const { token } = useAuth();
  const fetcher = useCallback(() => api.flows.suggestions(token!), [token]);
  const { data, error, reload } = usePolledFetch(token ? `fsug|${pointsVersion}` : null, fetcher, SUGGESTIONS_REFRESH_MS, {
    group: "fsug",
  });
  const [busy, setBusy] = useState(false);
  const all = data?.suggestions ?? [];
  // Views already added need no action; they are only counted.
  const list = all.filter((s) => !s.exists);
  const added = all.length - list.length;
  const addable = list.filter((s) => s.point);

  const add = async (items: FlowPointSuggestion[]) => {
    if (!token || items.length === 0) return;
    setBusy(true);
    let ok = 0;
    const errors: string[] = [];
    for (const s of items) {
      try {
        await api.flows.createPoint(token, s.point!);
        ok++;
      } catch (e) {
        errors.push(`${s.point!.name}: ${errorMessage(e)}`);
      }
    }
    setBusy(false);
    if (ok) toast.success(`Added ${ok} view${ok === 1 ? "" : "s"}`);
    for (const e of errors) toast.error(e);
    reload();
    onChanged();
  };

  if (error && !data) return <Notice kind="error">Could not load suggested views: {error}</Notice>;
  if (list.length === 0) {
    return added > 0 ? (
      <p className="flex items-center gap-1 text-xs text-muted-foreground">
        <Check className="h-3.5 w-3.5" /> All {added} suggested port view{added === 1 ? " is" : "s are"} added.
      </p>
    ) : null;
  }
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h4 className="flex items-center gap-1.5 text-sm font-medium">
            <Split className="h-3.5 w-3.5 text-muted-foreground" />
            Add suggested views
          </h4>
          <p className="text-xs text-muted-foreground">
            Ports of devices that cannot export flows, derived from a neighbouring exporter. Each view is labelled
            “derived” and says what it can and cannot see.
          </p>
        </div>
        {added > 0 && <span className="text-xs text-muted-foreground">{added} already added</span>}
        {addable.length > 1 && (
          <Button size="sm" variant="outline" disabled={busy} onClick={() => add(addable)}>
            <Plus /> Add all ({addable.length})
          </Button>
        )}
      </div>
      <ul className="divide-y rounded-md border">
        {list.map((s, i) => (
          <li key={`${s.rule}|${s.point?.name ?? s.reason}|${i}`} className="flex flex-wrap items-start justify-between gap-2 px-3 py-2 text-xs">
            <div className="min-w-0 flex-1 space-y-0.5">
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="rounded-full border px-1.5 text-[10px] text-muted-foreground">
                  {s.rule === "gateway-host" ? "gateway host" : "router trunk"}
                </span>
                <span className="font-medium">{s.point?.name ?? "Not ready yet"}</span>
              </div>
              <p className="text-muted-foreground">{s.reason}</p>
              {s.point?.note && <p className="text-[11px] text-muted-foreground/80">{s.point.note}</p>}
            </div>
            {s.point ? (
              <Button size="xs" variant="outline" disabled={busy} onClick={() => add([s])}>
                <Plus /> Add
              </Button>
            ) : null}
          </li>
        ))}
      </ul>
    </div>
  );
}

// SetupHints: what is missing before measured flows show up — collector off,
// no exporters, an exporter that never sent or has no template yet, no
// views yet — with the device commands, and (admin) the suggested derived
// views. inline = rendered in place of the measured view (no points yet).
export function SetupHints({
  status,
  statusError,
  points,
  isAdmin,
  devices,
  inline,
  onChanged,
}: {
  status: FlowStatus | null;
  statusError: string | null;
  points: FlowPoint[] | null;
  isAdmin: boolean;
  devices: Device[];
  inline?: boolean;
  onChanged: () => void;
}) {
  const exporters = status?.exporters ?? [];
  const never = exporters.filter((e) => e.enabled && e.state === "never");
  const waiting = exporters.filter((e) => e.enabled && e.template_state === "waiting");
  const port = listenPort(status?.listen) ?? 2055;
  const setupNeeded = !!status?.enabled && (exporters.length === 0 || never.length > 0);
  const [showSetup, setShowSetup] = useState<boolean | null>(null);
  const setupOpen = showSetup ?? setupNeeded;
  const defaultDevice = never.find((e) => e.kind === "routeros" && e.device_id)?.device_id ?? "";
  // Re-key the suggestions whenever the point list changes (an add elsewhere).
  const pointsVersion = (points ?? []).map((p) => p.id).join(",");
  // "No datagrams yet" names the address the devices should send to, which
  // only /flows/setup knows.
  const { token } = useAuth();
  const setupFetcher = useCallback(() => api.flows.setup(token!), [token]);
  const { data: setup } = usePolledFetch(token && never.length > 0 ? "fsetup-msg" : null, setupFetcher, null);

  if (!status) {
    if (!statusError) return null;
    return <Notice kind="error">Could not load the flow collector status: {statusError}</Notice>;
  }

  const off = !status.enabled;
  const noPoints = points !== null && points.length === 0;
  const messages: { kind: "info" | "warn" | "error"; text: React.ReactNode }[] = [];
  // Viewers can't act on the off state; outside the inline (Measured) card
  // it is admin-only (the Measured view says "collector is off" itself).
  if (off && (inline || isAdmin)) {
    messages.push({
      kind: inline ? "warn" : "info",
      text: (
        <>
          Flow collector is off. Set <code className="font-mono">MIKROTIK_NMS_FLOW_LISTEN=:2055</code> in the backend environment
          (LXC: <span className="font-mono">/etc/mikrotik-nms/env</span>) and restart the backend.
        </>
      ),
    });
  }
  for (const e of status.listen_errors ?? []) messages.push({ kind: "error", text: <>Cannot listen for flows: {e}</> });
  if (!off && exporters.length === 0) messages.push({ kind: "info", text: <>Waiting for flow exporters on udp/{port}.</> });
  for (const e of never) {
    messages.push({
      kind: "warn",
      text: (
        <>
          No datagrams yet from {e.name}. Check the device config and that udp/{setup?.port ?? port} reaches{" "}
          {setup?.advertise_address || "the NMS"}.
        </>
      ),
    });
  }
  for (const e of waiting) {
    messages.push({
      kind: "warn",
      text: <>Receiving data from {e.name} but no template yet — RouterOS resends within 1 min, OPNsense within 10 min.</>,
    });
  }
  if (!off && exporters.length > 0 && noPoints) {
    messages.push({ kind: "info", text: <>First flows arrive after the active timeout (≈1 min).</> });
  }

  const hasSuggestions = isAdmin && !off;
  // Nothing to fix and nothing a viewer could act on.
  if (!inline && messages.length === 0 && !isAdmin) return null;

  return (
    <Card className={cn("min-w-0", !inline && messages.length === 0 && "gap-2")}>
      <CardHeader className="gap-1">
        <CardTitle className="text-base">{inline ? "Measured flows are not set up yet" : "Flow export setup"}</CardTitle>
        {inline && (
          <p className="text-xs text-muted-foreground">
            Measured flows come from devices that export NetFlow / IPFIX to the NMS. Until then, “Estimated (port counters)”
            shows the traffic tree from interface counters.
          </p>
        )}
      </CardHeader>
      <CardContent className="space-y-3">
        {messages.map((m, i) => (
          <Notice key={i} kind={m.kind}>
            {m.text}
          </Notice>
        ))}
        {hasSuggestions && <Suggestions pointsVersion={pointsVersion} onChanged={onChanged} />}
        {!off && (
          <div className="space-y-3">
            <button
              type="button"
              aria-expanded={setupOpen}
              onClick={() => setShowSetup(!setupOpen)}
              className="flex items-center gap-1 text-left text-xs font-medium text-muted-foreground hover:text-foreground"
            >
              {setupOpen ? <ChevronDown className="h-3.5 w-3.5" /> : <ChevronRight className="h-3.5 w-3.5" />}
              Exporter setup instructions (RouterOS / OPNsense)
            </button>
            {setupOpen && <SetupInstructions devices={devices} defaultDevice={defaultDevice} />}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
