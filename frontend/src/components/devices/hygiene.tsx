"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { CalendarClock, Eye } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useNow } from "@/hooks/use-now";
import {
  api,
  type DeviceFinding,
  type DeviceHygiene,
  type FindingFix,
  type FindingSeverity,
  type HygieneFinding,
  type HygienePass,
} from "@/lib/api";
import { fmtAgo } from "@/lib/device-load";
import { cn } from "@/lib/utils";

// Shared pieces of the hygiene audit's UI: a finding with its explanation
// and suggested fix, as the device page and the Network Health page show it.
//
// A fix is advice — the NMS never applies it — and it is rendered by the
// rules of the network-health hints (lib/health-hints.ts): commands stand
// apart from the text, a step that interrupts traffic carries the
// maintenance-window mark, and a command that only prints says so.

export const FINDING_SEVERITY: Record<FindingSeverity, { label: string; className: string; rank: number }> = {
  critical: { label: "Critical", className: "bg-red-500/15 text-red-700 dark:text-red-400", rank: 0 },
  warn: { label: "Warning", className: "bg-amber-500/15 text-amber-800 dark:text-amber-300", rank: 1 },
  info: { label: "Info", className: "bg-muted text-muted-foreground", rank: 2 },
};

export function SeverityPill({ severity }: { severity: string }) {
  const sev = FINDING_SEVERITY[severity as FindingSeverity] ?? FINDING_SEVERITY.info;
  return (
    <span className={cn("inline-flex h-5 w-fit shrink-0 items-center rounded-full px-2 text-[11px] font-medium", sev.className)}>
      {sev.label}
    </span>
  );
}

// What an alert rule that asked for a diagnose pass is called.
const PASS_REASON: Record<string, string> = {
  scheduled: "scheduled",
  admin: "asked for by an admin",
  cpu_high: "started by the High CPU alert",
  flow_export_cpu_high: "started by the flow-export CPU watch",
  flow_export_on_switch: "started by the flow-export alert",
};

export function passReason(pass: HygienePass): string {
  if (pass.reason === "admin" && pass.requested_by) return `asked for by ${pass.requested_by}`;
  return PASS_REASON[pass.reason] ?? pass.reason.replaceAll("_", " ");
}

// When a pass ended (ms), or null while it runs.
export function passEndedAt(pass: HygienePass): number | null {
  const t = Date.parse(pass.finished_at);
  return Number.isFinite(t) && t > 0 ? t : null;
}

// "2 h ago — complete, 19 reads" / "running since 12:04".
export function passSummary(pass: HygienePass, now: number): string {
  const ended = passEndedAt(pass);
  if (pass.status === "running" || ended === null) return "running";
  const when = now ? fmtAgo(Math.max(0, (now - ended) / 1000)) : new Date(ended).toLocaleTimeString();
  const reads = `${pass.reads} read${pass.reads === 1 ? "" : "s"}`;
  switch (pass.status) {
    case "complete":
      return `${when} — ${reads}`;
    case "partial":
      return `${when} — incomplete: ${pass.failed_reads} of ${reads} failed`;
    default:
      return `${when} — stopped${pass.detail ? `: ${pass.detail}` : ""}`;
  }
}

// The steps of a suggested fix.
export function FixSteps({ fix }: { fix: FindingFix[] }) {
  if (fix.length === 0) return null;
  return (
    <ol className="space-y-2">
      {fix.map((step, i) => (
        <li key={i} className="text-xs leading-relaxed">
          {step.window && (
            <span
              className="mr-1.5 inline-flex items-center gap-1 rounded-sm bg-amber-100 px-1 py-px align-middle text-[10px] font-medium text-amber-800 dark:bg-amber-500/15 dark:text-amber-300"
              title="This step interrupts traffic: plan it, and tell the people who depend on this device"
            >
              <CalendarClock className="h-3 w-3" />
              maintenance window
            </span>
          )}
          <span>{step.text}</span>
          {(step.commands ?? []).length > 0 && (
            <div className="mt-1 space-y-1">
              {step.read_only && (
                <span
                  className="inline-flex items-center gap-1 text-[10px] font-medium uppercase tracking-wide text-muted-foreground"
                  title="This command only prints; it changes nothing on the device"
                >
                  <Eye className="h-3 w-3" />
                  read-only check
                </span>
              )}
              {(step.commands ?? []).map((c) => (
                <code
                  key={c}
                  className="block w-fit max-w-full whitespace-pre-wrap break-words rounded-sm bg-muted px-1.5 py-0.5 font-mono text-[11px] text-foreground"
                >
                  {c}
                </code>
              ))}
            </div>
          )}
        </li>
      ))}
    </ol>
  );
}

// A finding's explanation and fix.
export function FindingBody({ finding }: { finding: DeviceFinding }) {
  const fix = finding.fix ?? [];
  return (
    <div className="max-w-prose space-y-2.5 whitespace-normal">
      {finding.explanation && <p className="text-xs leading-relaxed text-muted-foreground">{finding.explanation}</p>}
      {fix.length > 0 && (
        <div>
          <p className="mb-1 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">Suggested fix</p>
          <FixSteps fix={fix} />
          <p className="mt-2 text-[11px] text-muted-foreground">
            The NMS only reads: run the commands yourself, on the device, after checking them.
          </p>
        </div>
      )}
    </div>
  );
}

// One hygiene finding: what was found, and — folded away — why it matters
// and what to do. Critical findings start open.
export function HygieneFindingItem({
  finding,
  now,
  deviceName,
  deviceHref,
}: {
  finding: DeviceFinding;
  now: number;
  // On a fleet-wide list: the device the finding is about, and its page.
  deviceName?: string;
  deviceHref?: string;
}) {
  const last = Date.parse(finding.last_seen);
  const first = Date.parse(finding.first_seen);
  const hasBody = !!finding.explanation || (finding.fix ?? []).length > 0;
  const head = (
    <div className="grid min-w-0 flex-1 gap-x-3 gap-y-1 sm:grid-cols-[4.75rem_1fr_auto] sm:items-baseline">
      <SeverityPill severity={finding.severity} />
      <span className="min-w-0">
        <span className="block text-sm font-medium">
          {deviceName ? (
            <span className="font-normal text-muted-foreground">
              {deviceHref ? (
                <Link href={deviceHref} className="underline-offset-2 hover:text-foreground hover:underline">
                  {deviceName}
                </Link>
              ) : (
                deviceName
              )}
              {" · "}
            </span>
          ) : null}
          {finding.title || finding.kind.replaceAll("_", " ")}
        </span>
        <span className="block break-words text-xs text-muted-foreground">{finding.detail}</span>
      </span>
      <span
        className="text-xs tabular-nums text-muted-foreground sm:text-right"
        title={Number.isFinite(first) ? `First found ${new Date(first).toLocaleString()}` : undefined}
      >
        checked {Number.isFinite(last) && now ? fmtAgo(Math.max(0, (now - last) / 1000)) : "—"}
      </span>
    </div>
  );
  if (!hasBody) return <div className="px-3 py-2">{head}</div>;
  return (
    <details className="group px-3 py-2" open={finding.severity === "critical"}>
      <summary className="flex cursor-pointer list-none items-start gap-2 [&::-webkit-details-marker]:hidden">
        <span aria-hidden className="mt-1 text-[10px] text-muted-foreground transition-transform group-open:rotate-90">
          ▶
        </span>
        {head}
      </summary>
      <div className="mt-2 pl-[1.1rem] sm:pl-[calc(1.1rem+4.75rem+0.75rem)]">
        <FindingBody finding={finding} />
      </div>
    </details>
  );
}

// FleetHygieneCard: every device's hygiene findings on the Network Health
// page, most severe first. Informational findings (a default the audit
// mentions, like device-mode) are folded away: there is one on almost every
// switch and access point, and they must not bury a warning.
export function FleetHygieneCard({ findings }: { findings: HygieneFinding[] }) {
  const now = useNow(60_000);
  const [showInfo, setShowInfo] = useState(false);
  const serious = findings.filter((f) => f.severity !== "info");
  const info = findings.filter((f) => f.severity === "info");
  const critical = serious.filter((f) => f.severity === "critical").length;
  const row = (f: HygieneFinding) => (
    <li key={f.id}>
      <HygieneFindingItem finding={f} now={now} deviceName={f.device_name || "device"} deviceHref={`/devices/${f.device_id}`} />
    </li>
  );
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Configuration hygiene</CardTitle>
        <p className="text-xs text-muted-foreground">
          {"Settings that cost a device CPU or are unsafe, from the read-only audit of each device's configuration " +
            "(every 6 hours) and the diagnose pass. Open a row for why it matters and the suggested fix; nothing is " +
            "ever changed on a device."}
        </p>
      </CardHeader>
      <CardContent className="space-y-3">
        {findings.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No findings. Devices are audited a few minutes after they become reachable, then every 6 hours.
          </p>
        ) : (
          <>
            <p className="text-xs text-muted-foreground">
              {critical > 0 ? `${critical} critical, ` : ""}
              {serious.length - critical} warning{serious.length - critical === 1 ? "" : "s"}, {info.length} informational
            </p>
            {serious.length > 0 && <ul className="divide-y rounded-md border">{serious.map(row)}</ul>}
            {info.length > 0 && (
              <div>
                <button
                  type="button"
                  aria-expanded={showInfo}
                  onClick={() => setShowInfo((v) => !v)}
                  className="text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
                >
                  {showInfo ? "Hide" : "Show"} the {info.length} informational finding{info.length === 1 ? "" : "s"}
                </button>
                {showInfo && <ul className="mt-2 divide-y rounded-md border">{info.map(row)}</ul>}
              </div>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}

// useDeviceHygiene reads GET /devices/{id}/hygiene (database only) every
// 30 s, and every 5 s while a pass runs on the device, so its result shows
// up when it ends. reload asks again at once (after "Audit now").
export function useDeviceHygiene(token: string | null, deviceId: string) {
  const [state, setState] = useState<{ id: string; data: DeviceHygiene | null; error: string | null }>({
    id: deviceId,
    data: null,
    error: null,
  });
  const [nonce, setNonce] = useState(0);
  const reload = useCallback(() => setNonce((n) => n + 1), []);
  const current = state.id === deviceId ? state : null;
  const running = current?.data?.running ?? "";

  useEffect(() => {
    if (!token) return;
    let alive = true;
    const load = () =>
      api.devices
        .hygiene(token, deviceId)
        .then((data) => {
          if (alive) setState({ id: deviceId, data, error: null });
        })
        .catch((e) => {
          if (!alive) return;
          const error = e instanceof Error ? e.message : String(e);
          setState((prev) => ({ id: deviceId, data: prev.id === deviceId ? prev.data : null, error }));
        });
    load();
    const timer = setInterval(load, running ? 5_000 : 30_000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [token, deviceId, nonce, running]);

  return { hygiene: current?.data ?? null, error: current?.error ?? null, reload };
}
