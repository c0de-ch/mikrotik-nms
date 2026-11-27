"use client";

import { useEffect, useMemo, useState } from "react";
import { Loader2, ScanSearch, Stethoscope } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useNow } from "@/hooks/use-now";
import { api, type Device, type DeviceFinding, type DeviceHygiene, type HygienePass, type HygienePassKind } from "@/lib/api";
import { fmtAgo, fmtIn, fmtStamp, isPollingPaused } from "@/lib/device-load";
import { cn } from "@/lib/utils";
import { FINDING_SEVERITY, HygieneFindingItem, passReason, passSummary } from "./hygiene";

// Findings: what the NMS found on the device.
//
// The hygiene audit comes first — settings that cost the device CPU or are
// unsafe, read from its configuration every 6 hours, each with an
// explanation and a suggested fix; admins can run the audit or the shorter
// diagnose pass now. Below it, what the pollers noticed in what they already
// read (today: the account lines of its log), grouped by kind, so each kind
// explains itself once.

const KINDS: Record<string, { label: string; hint: string }> = {
  parallel_api_poller: {
    label: "Another API client",
    hint: "Something other than this NMS logged in to the device's API — another monitoring system, a script or an app. Its reads add load this NMS does not budget for. If the address belongs to this NMS (behind NAT, or a second address), add it to NMS source addresses in Settings.",
  },
  tls_pin_mismatch: {
    label: "TLS certificate is not the pinned one",
    hint: "The device answered API-TLS with another certificate than the fingerprint pinned for it, so the NMS did not log in. Each row is a fingerprint that was presented. Compare it with the certificate on the device itself (/certificate print detail) before pinning it: if they differ, something else is answering at this address.",
  },
  login_failure: {
    label: "Failed login",
    hint: "A login to the device failed. Repeated failures from one address are usually a script with an old password, or someone guessing.",
  },
  flow_export_on_switch: {
    label: "Flow datagrams from a device that must not export",
    hint: "Flow datagrams arrive from this device's address although its role says it must not export. The finding is a warning until a read of the device's own traffic-flow settings confirms it; see the alert for the rollback steps.",
  },
};

function kindInfo(kind: string) {
  return KINDS[kind] ?? { label: kind.replaceAll("_", " "), hint: "" };
}

// Why a pass cannot be asked for right now ("" = it can).
function blockedReason(
  kind: HygienePassKind,
  device: Device,
  hygiene: DeviceHygiene,
  now: number,
): string {
  if (hygiene.pollers === "off") return "Polling is off on this instance: no device is read.";
  if (isPollingPaused(device, now)) return "Polling is paused for this device. Resume it to run a pass.";
  if (hygiene.running) return `${hygiene.running === "audit" ? "An audit" : "A diagnose pass"} is running on this device.`;
  if (device.status !== "online") return "The device is not reachable right now.";
  const at = (kind === "audit" ? hygiene.audit : hygiene.diagnose).available_at;
  const until = at ? Date.parse(at) : NaN;
  if (Number.isFinite(until) && (!now || until > now)) {
    return `One ${kind === "audit" ? "audit" : "diagnose pass"} per device and hour: available again ${now ? fmtIn(until, now) : "soon"}.`;
  }
  return "";
}

function PassLine({ label, pass, now, extra }: { label: string; pass: HygienePass | null; now: number; extra?: string }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0">
        {pass ? (
          <>
            <span className="tabular-nums">{passSummary(pass, now)}</span>
            <span className="text-muted-foreground"> · {passReason(pass)}</span>
          </>
        ) : (
          <span className="text-muted-foreground">not run yet</span>
        )}
        {extra && <span className="text-muted-foreground"> · {extra}</span>}
      </dd>
    </>
  );
}

interface Props {
  device: Device;
  token: string;
  isAdmin: boolean;
  hygiene: DeviceHygiene | null;
  hygieneError: string | null;
  // Called after a pass was started, so the hygiene view is read again.
  onPassStarted: () => void;
}

export function FindingsCard({ device, token, isAdmin, hygiene, hygieneError, onPassStarted }: Props) {
  const deviceId = device.id;
  const now = useNow(1000);
  const [state, setState] = useState<{ id: string; findings: DeviceFinding[]; error: string | null } | null>(null);
  const [starting, setStarting] = useState<HygienePassKind | null>(null);

  // The other findings are read once, and again when a pass has ended (a
  // pass can add one: an API session that is not the NMS's).
  const passMark = `${hygiene?.audit.last?.id ?? 0}:${hygiene?.diagnose.last?.id ?? 0}:${hygiene?.running ?? ""}`;
  useEffect(() => {
    let alive = true;
    api.devices
      .findings(token, deviceId)
      .then((findings) => {
        if (alive) setState({ id: deviceId, findings, error: null });
      })
      .catch((e) => {
        if (alive) setState({ id: deviceId, findings: [], error: e instanceof Error ? e.message : String(e) });
      });
    return () => {
      alive = false;
    };
  }, [token, deviceId, passMark]);

  const current = state?.id === deviceId ? state : null;
  // Kinds ordered by their most severe row, rows by last seen (the API
  // order). The audit's own findings are shown above, from the hygiene view.
  const groups = useMemo(() => {
    const byKind = new Map<string, DeviceFinding[]>();
    for (const f of current?.findings ?? []) {
      if (f.source === "audit") continue;
      const list = byKind.get(f.kind);
      if (list) list.push(f);
      else byKind.set(f.kind, [f]);
    }
    const worst = (rows: DeviceFinding[]) => Math.min(...rows.map((r) => FINDING_SEVERITY[r.severity]?.rank ?? 3));
    return [...byKind.entries()].sort((a, b) => worst(a[1]) - worst(b[1]));
  }, [current]);

  const start = async (kind: HygienePassKind) => {
    setStarting(kind);
    try {
      await (kind === "audit" ? api.devices.auditNow(token, deviceId) : api.devices.diagnoseNow(token, deviceId));
      toast.success(
        kind === "audit"
          ? "Audit started. It reads the device slowly — the result appears here when it is through."
          : "Diagnose pass started. The result appears here in a few minutes.",
      );
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Request failed");
    } finally {
      setStarting(null);
      onPassStarted();
    }
  };

  const audit = hygiene?.audit.last ?? null;
  const nextAudit = hygiene?.audit.next_run ? Date.parse(hygiene.audit.next_run) : NaN;
  const hygieneFindings = hygiene?.findings ?? [];
  const audited = !!audit && audit.status !== "running" && audit.status !== "aborted";

  return (
    <Card id="findings">
      <CardHeader>
        <CardTitle>Findings</CardTitle>
        <CardDescription>
          Settings on this device that cost it CPU or are unsafe, and what the pollers noticed in what they already read.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-6">
        <section aria-label="Hygiene audit">
          <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
            <div className="min-w-0">
              <h3 className="text-sm font-medium">Hygiene audit</h3>
              <p className="mt-0.5 max-w-prose text-xs text-muted-foreground">
                {"Every 6 hours the NMS reads this device's configuration — read-only, a few commands spaced well apart — " +
                  "and reports what is known to cost CPU or to be unsafe. While the CPU is high it stays away; the shorter " +
                  "diagnose pass looks for the cause then. Nothing is ever changed on the device."}
              </p>
            </div>
            {isAdmin && hygiene && (
              <div className="flex shrink-0 flex-wrap gap-2">
                {(
                  [
                    { kind: "audit", label: "Audit now", busyLabel: "Audit running", Icon: ScanSearch },
                    { kind: "diagnose", label: "Diagnose now", busyLabel: "Diagnose pass running", Icon: Stethoscope },
                  ] as const
                ).map(({ kind, label, busyLabel, Icon }) => {
                  const blocked = blockedReason(kind, device, hygiene, now);
                  const runningThis = hygiene.running === kind;
                  return (
                    <Button
                      key={kind}
                      size="sm"
                      variant="outline"
                      disabled={!!blocked || starting !== null}
                      title={
                        blocked ||
                        (kind === "audit"
                          ? "Read the device's configuration now (at most once an hour)"
                          : "Look for the usual device-side causes of a high CPU now (at most once an hour)")
                      }
                      onClick={() => start(kind)}
                    >
                      {runningThis || starting === kind ? <Loader2 className="animate-spin" /> : <Icon />}
                      {runningThis ? busyLabel : label}
                    </Button>
                  );
                })}
              </div>
            )}
          </div>

          {hygieneError && !hygiene ? (
            <p role="alert" className="mt-3 text-sm text-red-700 dark:text-red-400">
              Could not load the audit: {hygieneError}
            </p>
          ) : !hygiene ? (
            <div className="mt-3 space-y-2" aria-label="Loading the audit">
              <Skeleton className="h-4 w-1/2" />
              <Skeleton className="h-4 w-1/3" />
            </div>
          ) : (
            <>
              <dl className="mt-3 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
                <PassLine
                  label="Last audit"
                  pass={audit}
                  now={now}
                  extra={
                    hygiene.running === "audit"
                      ? undefined
                      : Number.isFinite(nextAudit) && now
                        ? `next ${fmtIn(nextAudit, now)}`
                        : hygiene.pollers === "off"
                          ? "none scheduled: polling is off"
                          : undefined
                  }
                />
                <PassLine label="Last diagnose pass" pass={hygiene.diagnose.last} now={now} />
                {hygiene.snapshot && (
                  <>
                    <dt className="text-muted-foreground">Configuration</dt>
                    <dd className="min-w-0 text-muted-foreground">
                      last seen to change{" "}
                      <span title={new Date(hygiene.snapshot.taken_at).toLocaleString()}>
                        {fmtStamp(Date.parse(hygiene.snapshot.taken_at), now)}
                      </span>
                    </dd>
                  </>
                )}
              </dl>

              {hygieneFindings.length > 0 ? (
                <ul className="mt-3 divide-y rounded-md border">
                  {hygieneFindings.map((f) => (
                    <li key={f.id}>
                      <HygieneFindingItem finding={f} now={now} />
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="mt-3 text-sm text-muted-foreground">
                  {audited
                    ? "The last audit found nothing on this device that costs CPU or is unsafe."
                    : hygiene.pollers === "off"
                      ? "Not audited: polling is off on this instance."
                      : "Not audited yet. The first audit runs a few minutes after the device is reachable and its CPU is calm."}
                </p>
              )}
            </>
          )}
        </section>

        <section aria-label="Noticed by the pollers" className={cn("border-t pt-5")}>
          <h3 className="text-sm font-medium">Noticed by the pollers</h3>
          {!current ? (
            <div className="mt-2 space-y-2" aria-label="Loading findings">
              <Skeleton className="h-4 w-1/3" />
              <Skeleton className="h-4 w-2/3" />
            </div>
          ) : current.error ? (
            <p role="alert" className="mt-2 text-sm text-red-700 dark:text-red-400">
              Could not load findings: {current.error}
            </p>
          ) : groups.length === 0 ? (
            <p className="mt-2 text-sm text-muted-foreground">
              {hygieneFindings.some((f) => f.kind === "parallel_api_poller")
                ? "Nothing more: the other API client the audit saw is listed with its findings above."
                : "Nothing. Nothing else has been seen logging in to this device's API."}
            </p>
          ) : (
            <div className="mt-3 space-y-5">
              {groups.map(([kind, rows]) => {
                const info = kindInfo(kind);
                return (
                  <section key={kind} aria-label={info.label}>
                    <h4 className="text-sm font-medium">{info.label}</h4>
                    {info.hint && <p className="mt-0.5 max-w-prose text-xs text-muted-foreground">{info.hint}</p>}
                    <ul className="mt-2 divide-y rounded-md border">
                      {rows.map((f) => {
                        const sev = FINDING_SEVERITY[f.severity] ?? FINDING_SEVERITY.info;
                        const first = Date.parse(f.first_seen);
                        const last = Date.parse(f.last_seen);
                        return (
                          <li key={f.id} className="grid gap-x-3 gap-y-1 px-3 py-2 sm:grid-cols-[4.5rem_1fr_auto] sm:items-baseline">
                            <span className={cn("inline-flex h-5 w-fit items-center rounded-full px-2 text-[11px] font-medium", sev.className)}>
                              {sev.label}
                            </span>
                            <span className="min-w-0 break-words text-sm">{f.detail || "—"}</span>
                            <span className="text-xs tabular-nums text-muted-foreground sm:text-right">
                              <span title={Number.isFinite(last) ? new Date(last).toLocaleString() : f.last_seen}>
                                {f.count}× · last {Number.isFinite(last) && now ? fmtAgo(Math.max(0, (now - last) / 1000)) : "—"}
                              </span>
                              <span className="block" title={Number.isFinite(first) ? new Date(first).toLocaleString() : f.first_seen}>
                                first {Number.isFinite(first) ? fmtStamp(first, now) : "—"}
                              </span>
                            </span>
                          </li>
                        );
                      })}
                    </ul>
                  </section>
                );
              })}
            </div>
          )}
        </section>
      </CardContent>
    </Card>
  );
}
