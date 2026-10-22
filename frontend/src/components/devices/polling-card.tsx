"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { ChevronDown, ChevronRight, PlayCircle } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { useNow } from "@/hooks/use-now";
import {
  api,
  type Device,
  type DeviceAuditEntry,
  type DeviceClass,
  type LiveLimits,
  type PollProfile,
} from "@/lib/api";
import {
  CLASSES,
  CLASS_ORDER,
  budgetClass,
  budgetReason,
  budgeted,
  classWord,
  counterIntervalSeconds,
  fmtIn,
  fmtInterval,
  fmtRemaining,
  fmtStamp,
  fmtUntil,
  isPollingPaused,
  pausedUntilMs,
  tierLabel,
} from "@/lib/device-load";
import { cn } from "@/lib/utils";
import { PauseGlyph } from "./device-load";

// The NMS polling card: how hard the NMS may poll this device, whether it is
// polling it now, and what it reads next. The class scale is the device-load
// meter at full size — the same three bars every list shows, labelled — and
// everything else is plain controls and plain words, because the person
// reading it is deciding whether a switch at 100 % CPU is the NMS's fault.

function describeHardware(d: Device): string {
  const parts: string[] = [];
  if (d.cpu_count) parts.push(`${d.cpu_count} core${d.cpu_count === 1 ? "" : "s"}`);
  if (d.cpu_mhz_max) parts.push(`${d.cpu_mhz_max} MHz`);
  if (d.architecture) parts.push(d.architecture);
  if (d.memory_total) parts.push(`${Math.round(d.memory_total / (1024 * 1024))} MiB`);
  return parts.join(", ");
}

function auditVerb(action: string): string {
  switch (action) {
    case "polling_paused":
      return "paused polling";
    case "polling_resumed":
      return "resumed polling";
    case "poll_profile_changed":
      return "changed the profile";
    case "class_override_changed":
      return "changed the class override";
    case "device_class_probed":
      return "probed the class";
    default:
      return action.replaceAll("_", " ");
  }
}

// ClassScale: the three classes as one segmented bar, filled up to the class
// the device is polled as. The probed class keeps a dashed outline when the
// light profile or an override moved the device off it; an unprobed
// device's C1 is outlined — assumed, not known.
function ClassScale({ device }: { device: Device }) {
  const budget = budgetClass(device);
  const reason = budgetReason(device);
  const probed = reason === "unprobed" ? null : budgeted(device.device_class);
  const override = device.class_override ? budgeted(device.class_override) : null;
  const rank = CLASSES[budget].rank;
  return (
    <div role="group" aria-label={`CPU class: polled as ${budget} ${CLASSES[budget].word}`} className="grid grid-cols-3 gap-1.5">
      {CLASS_ORDER.map((c) => {
        const info = CLASSES[c];
        const inUse = c === budget;
        const isProbed = !inUse && probed === c;
        const isOverride = !inUse && !isProbed && override === c;
        const filled = info.rank <= rank;
        return (
          <div key={c} className="min-w-0" aria-current={inUse ? "true" : undefined}>
            <div
              aria-hidden
              className={cn(
                "h-2.5 rounded-[3px]",
                !filled ? "bg-load-track" : reason === "unprobed" ? "border border-load-meter" : "bg-load-meter",
                isProbed && "outline-1 outline-offset-2 outline-dashed outline-foreground/45",
              )}
            />
            <div className={cn("mt-2 truncate text-xs", inUse ? "font-medium text-foreground" : "text-muted-foreground")}>
              {c} {info.name}
            </div>
            <div className="text-[11px] text-muted-foreground">
              {inUse ? (reason === "unprobed" ? "Assumed" : "In use") : isProbed ? "Probed" : isOverride ? "Override" : " "}
            </div>
          </div>
        );
      })}
    </div>
  );
}

// The sentence under the scale: what the probe found and what moved the
// device off it.
function classSentence(d: Device): string {
  const reason = budgetReason(d);
  if (reason === "unprobed") return "Not probed yet — polled as a weak device until the first resource read.";
  const hardware = describeHardware(d);
  const probed = budgeted(d.device_class);
  let s = `Probed as ${CLASSES[probed].word}${hardware ? ` (${hardware})` : ""}`;
  if (d.class_override && budgeted(d.class_override) !== probed) s += `, overridden to ${classWord(d.class_override)}`;
  if (d.poll_profile === "light") s += ", on the light profile";
  return `${s}.`;
}

function liveSentence(l: LiveLimits): string {
  const who = l.admin_only ? "admins only, " : "";
  const period = l.min_period_seconds > 1 ? `one sample per ${fmtInterval(l.min_period_seconds)}` : "1-second samples";
  return `${who}${period}, up to ${l.max_ifaces} port${l.max_ifaces === 1 ? "" : "s"} at once`;
}

// Audit details carry RFC 3339 times ("1 h, until 2026-10-17T14:30:00Z"):
// shown in local time like every other timestamp on the page.
function localizeDetail(detail: string, now: number): string {
  return detail.replace(/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})/g, (iso) => {
    const t = Date.parse(iso);
    return Number.isFinite(t) ? fmtStamp(t, now) : iso;
  });
}

interface Props {
  device: Device;
  token: string;
  isAdmin: boolean;
  onChange: (device: Device) => void;
}

const VISIBLE_READS = 4;

export function PollingCard({ device, token, isAdmin, onChange }: Props) {
  const now = useNow();
  const [busy, setBusy] = useState(false);
  const [audit, setAudit] = useState<DeviceAuditEntry[]>([]);
  const [limits, setLimits] = useState<LiveLimits | null>(null);
  const [allReads, setAllReads] = useState(false);

  const budget = budgetClass(device);
  const paused = isPollingPaused(device, now);
  const until = pausedUntilMs(device);

  const loadAudit = useCallback(() => {
    api.devices
      .audit(token, device.id, 8)
      .then(setAudit)
      .catch(() => setAudit([]));
  }, [token, device.id]);

  useEffect(() => {
    loadAudit();
  }, [loadAudit]);

  // The live-view caps follow the budget and the pause; ask again when
  // either changes. A failed fetch only hides the live-view row.
  useEffect(() => {
    let alive = true;
    api.devices
      .liveLimits(token, device.id)
      .then((l) => {
        if (alive) setLimits(l);
      })
      .catch(() => {
        if (alive) setLimits(null);
      });
    return () => {
      alive = false;
    };
  }, [token, device.id, budget, paused]);

  const run = async (label: string, fn: () => Promise<Device>) => {
    setBusy(true);
    try {
      const updated = await fn();
      onChange(updated);
      toast.success(label);
      loadAudit();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Request failed");
    } finally {
      setBusy(false);
    }
  };

  const reads = useMemo(
    () =>
      Object.entries(device.polling?.next_runs ?? {})
        .map(([tier, at]) => ({ tier, at: Date.parse(at) }))
        .filter((r) => Number.isFinite(r.at))
        .sort((a, b) => a.at - b.at),
    [device.polling],
  );
  const shownReads = allReads ? reads : reads.slice(0, VISIBLE_READS);
  const counterSeconds = counterIntervalSeconds(budget, limits?.snapshot_interval_seconds);
  const nextCounters = device.polling?.next_runs.counters ? Date.parse(device.polling.next_runs.counters) : null;
  const probedRank = probedRankOf(device);
  // MIKROTIK_NMS_POLLERS=off: nothing is read from any device (the banner
  // says so for the whole instance); the live limits are how the card knows.
  const instanceOff = limits?.reason === "pollers_off";

  return (
    <Card>
      <CardHeader>
        <CardTitle>NMS polling</CardTitle>
        <CardDescription>How hard this NMS may poll the device, and what it reads next.</CardDescription>
        {isAdmin && (
          <CardAction>
            {paused ? (
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                onClick={() => run("Polling resumed", () => api.devices.resumePolling(token, device.id))}
              >
                <PlayCircle />
                Resume polling
              </Button>
            ) : (
              <DropdownMenu>
                <DropdownMenuTrigger render={<Button size="sm" variant="outline" disabled={busy} />}>
                  <PauseGlyph className="h-3.5 w-3.5" />
                  Pause polling
                  <ChevronDown className="opacity-60" />
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end" className="w-auto">
                  <DropdownMenuItem onClick={() => run("Polling paused for 1 hour", () => api.devices.pausePolling(token, device.id, 1))}>
                    For 1 hour
                  </DropdownMenuItem>
                  <DropdownMenuItem onClick={() => run("Polling paused for 24 hours", () => api.devices.pausePolling(token, device.id, 24))}>
                    For 24 hours
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            )}
          </CardAction>
        )}
      </CardHeader>

      <CardContent className="space-y-4">
        <div className="space-y-2.5">
          <ClassScale device={device} />
          <p className="text-sm text-muted-foreground">{classSentence(device)}</p>
        </div>

        {paused && until !== null ? (
          <div role="status" className="flex items-start gap-2 rounded-md border border-load-hold/30 bg-load-hold/10 px-3 py-2 text-sm text-load-hold">
            <PauseGlyph className="mt-0.5 h-3.5 w-3.5" />
            <p>
              <span className="font-medium">Paused until {fmtUntil(until, now)}</span>
              {now && fmtRemaining(until - now) ? ` (${fmtRemaining(until - now)} left)` : ""}. Only the liveness check
              runs: live rates keep their last reading and charts show the pause as a gap.
            </p>
          </div>
        ) : (
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
            <dt className="text-muted-foreground">Interface counters</dt>
            <dd className="tabular-nums">
              every {fmtInterval(counterSeconds)}
              {instanceOff ? (
                <span className="text-muted-foreground">, once polling is on</span>
              ) : nextCounters !== null && now ? (
                <span className="text-muted-foreground">, next read {fmtIn(nextCounters, now)}</span>
              ) : null}
            </dd>
            {limits && (
              <>
                <dt className="text-muted-foreground">Live view</dt>
                <dd>{instanceOff ? <span className="text-muted-foreground">off while polling is off</span> : liveSentence(limits)}</dd>
              </>
            )}
            {reads.length === 0 && (
              <>
                <dt className="text-muted-foreground">Schedule</dt>
                <dd className="text-muted-foreground">
                  {instanceOff
                    ? "Polling is off on this instance — no device is read."
                    : device.status === "online"
                      ? "Nothing scheduled yet — the loops plan each read on their next tick."
                      : "No reads while the device is unreachable; the liveness check keeps trying."}
                </dd>
              </>
            )}
          </dl>
        )}

        {!paused && reads.length > 0 && (
          <div>
            <h3 className="text-xs font-medium text-muted-foreground">Next reads</h3>
            <ul className="mt-1.5 grid gap-x-6 gap-y-1 text-sm sm:grid-cols-2">
              {shownReads.map((r) => (
                <li key={r.tier} className="flex min-w-0 items-baseline justify-between gap-3">
                  <span className="truncate">{tierLabel(r.tier)}</span>
                  <span className="shrink-0 text-xs tabular-nums text-muted-foreground">{now ? fmtIn(r.at, now) : "…"}</span>
                </li>
              ))}
            </ul>
            {reads.length > VISIBLE_READS && (
              <button
                type="button"
                aria-expanded={allReads}
                onClick={() => setAllReads((v) => !v)}
                className="mt-1.5 inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
              >
                {allReads ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
                {allReads ? "Show fewer" : `Show all ${reads.length} scheduled reads`}
              </button>
            )}
          </div>
        )}

        {isAdmin ? (
          <div className="grid gap-3 sm:grid-cols-2">
            <label className="flex flex-col gap-1 text-sm">
              <span className="text-muted-foreground">Profile</span>
              <select
                className="flex h-8 rounded-md border bg-transparent px-2 text-sm disabled:opacity-60"
                value={device.poll_profile}
                disabled={busy}
                onChange={(e) =>
                  run("Profile saved", () =>
                    api.devices.updatePolling(token, device.id, { profile: e.target.value as PollProfile }),
                  )
                }
              >
                <option value="auto">Auto — its class cadences</option>
                <option value="light">Light — weak-device cadences</option>
              </select>
            </label>
            <label className="flex flex-col gap-1 text-sm">
              <span className="text-muted-foreground">Class override</span>
              <select
                className="flex h-8 rounded-md border bg-transparent px-2 text-sm disabled:opacity-60"
                value={device.class_override || ""}
                disabled={busy}
                onChange={(e) =>
                  run("Class override saved", () =>
                    api.devices.updatePolling(token, device.id, {
                      class_override: e.target.value ? (e.target.value as DeviceClass) : null,
                    }),
                  )
                }
              >
                <option value="">None — use the probed class</option>
                {CLASS_ORDER.map((c) => (
                  <option key={c} value={c} disabled={CLASSES[c].rank > probedRank}>
                    {c} {CLASSES[c].name}
                    {CLASSES[c].rank > probedRank ? " — more capable than probed" : ""}
                  </option>
                ))}
              </select>
            </label>
            <p className="text-xs text-muted-foreground sm:col-span-2">
              An override can only make polling gentler. A pause keeps the liveness check and stops every other read.
            </p>
          </div>
        ) : (
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
            <dt className="text-muted-foreground">Profile</dt>
            <dd>{device.poll_profile === "light" ? "Light — weak-device cadences" : "Auto — its class cadences"}</dd>
            <dt className="text-muted-foreground">Class override</dt>
            <dd>{device.class_override ? `${device.class_override} ${classWord(device.class_override)}` : "None"}</dd>
            <dd className="col-span-2 text-xs text-muted-foreground">Only an admin can change polling.</dd>
          </dl>
        )}

        {audit.length > 0 && (
          <div className="border-t pt-3">
            <h3 className="text-xs font-medium text-muted-foreground">Recent changes</h3>
            <ol className="mt-1.5 space-y-1 text-xs">
              {audit.map((e) => {
                const at = Date.parse(e.at);
                return (
                  <li key={e.id} className="flex gap-3 text-muted-foreground">
                    <time
                      dateTime={e.at}
                      title={Number.isFinite(at) ? new Date(at).toLocaleString() : e.at}
                      className="w-[5.5rem] shrink-0 tabular-nums"
                    >
                      {Number.isFinite(at) ? fmtStamp(at, now) : e.at}
                    </time>
                    <span className="min-w-0">
                      <span className="text-foreground">{e.username || "system"}</span> {auditVerb(e.action)}
                      {e.detail ? `: ${localizeDetail(e.detail, now)}` : ""}
                    </span>
                  </li>
                );
              })}
            </ol>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// The override select offers only classes no more capable than the probed
// one (an unprobed device counts as C1).
function probedRankOf(d: Device): number {
  return CLASSES[budgeted(d.device_class)].rank;
}
