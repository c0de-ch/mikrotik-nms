"use client";

import { useCallback, useEffect, useState } from "react";
import { PauseCircle, PlayCircle } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { api, type Device, type DeviceAuditEntry, type DeviceClass, type PollProfile } from "@/lib/api";

// The NMS polling card: how hard the NMS may push this device. The class
// scale is the one visual element — three steps, the effective one lit — and
// everything else is plain controls and plain words, because the person
// reading it is deciding whether a switch at 100 % CPU is the NMS's fault.

const CLASS_STEPS: { id: DeviceClass; name: string; hint: string }[] = [
  { id: "C1", name: "Weak", hint: "MIPS or ≤ 128 MiB: one counter read a minute, STP every 5 min" },
  { id: "C2", name: "Small", hint: "Single-core ARM: counters every 30 s, STP every 2 min" },
  { id: "C3", name: "Capable", hint: "arm64 / x86 with 2+ cores: counters every 15 s" },
];

const CLASS_RANK: Record<DeviceClass, number> = { C0: 1, C1: 1, C2: 2, C3: 3 };

function classWord(c: DeviceClass | ""): string {
  switch (c) {
    case "C1":
      return "weak";
    case "C2":
      return "small";
    case "C3":
      return "capable";
    default:
      return "unprobed";
  }
}

function describeHardware(d: Device): string {
  const parts: string[] = [];
  if (d.cpu_count) parts.push(`${d.cpu_count} core${d.cpu_count === 1 ? "" : "s"}`);
  if (d.cpu_mhz_max) parts.push(`${d.cpu_mhz_max} MHz`);
  if (d.architecture) parts.push(d.architecture);
  if (d.memory_total) parts.push(`${Math.round(d.memory_total / (1024 * 1024))} MiB`);
  return parts.join(" · ");
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

function formatWhen(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

function remaining(iso: string): string {
  const ms = new Date(iso).getTime() - Date.now();
  if (!Number.isFinite(ms) || ms <= 0) return "";
  const h = Math.floor(ms / 3_600_000);
  const m = Math.round((ms % 3_600_000) / 60_000);
  return h > 0 ? `${h} h ${m} min` : `${m} min`;
}

// "in 42 s" / "in 4 min" / "now" for a scheduled read, as of page load.
function inWords(iso: string): string {
  const s = Math.round((new Date(iso).getTime() - Date.now()) / 1000);
  if (!Number.isFinite(s) || s <= 0) return "now";
  return s < 90 ? `in ${s} s` : `in ${Math.round(s / 60)} min`;
}

interface Props {
  device: Device;
  token: string;
  isAdmin: boolean;
  onChange: (device: Device) => void;
}

export function PollingCard({ device, token, isAdmin, onChange }: Props) {
  const [busy, setBusy] = useState(false);
  const [audit, setAudit] = useState<DeviceAuditEntry[]>([]);

  const loadAudit = useCallback(() => {
    api.devices
      .audit(token, device.id, 8)
      .then(setAudit)
      .catch(() => setAudit([]));
  }, [token, device.id]);

  useEffect(() => {
    loadAudit();
  }, [loadAudit]);

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

  const probed = device.device_class || "C0";
  const effective = device.effective_class;
  const paused = device.polling_paused && device.polling_paused_until;
  const hardware = describeHardware(device);
  const probedRank = CLASS_RANK[probed as DeviceClass] ?? 1;

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-sm font-medium">NMS polling</CardTitle>
        <CardDescription>
          {probed === "C0" ? (
            <>Not probed yet — polled as a weak device until the first resource read.</>
          ) : (
            <>
              Probed as {classWord(probed)}
              {hardware ? ` (${hardware})` : ""}
              {device.class_override ? `, overridden to ${classWord(device.class_override)}` : ""}
              {device.poll_profile === "light" ? ", on the light profile" : ""}.
            </>
          )}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* Class scale: the effective class is lit; the probed one, when
            an override moved away from it, keeps a thin outline. */}
        <div className="flex items-stretch gap-1" role="list" aria-label="CPU class">
          {CLASS_STEPS.map((step) => {
            const lit = step.id === effective;
            const wasProbed = step.id === probed && !lit;
            return (
              <div
                key={step.id}
                role="listitem"
                title={step.hint}
                aria-current={lit ? "true" : undefined}
                className={[
                  "flex-1 rounded-md border px-2.5 py-1.5 text-xs leading-tight transition-colors",
                  lit
                    ? "border-primary bg-primary text-primary-foreground"
                    : wasProbed
                      ? "border-foreground/40 border-dashed text-muted-foreground"
                      : "border-border text-muted-foreground",
                ].join(" ")}
              >
                <div className="font-medium">
                  {step.id} <span className={lit ? "opacity-90" : "opacity-70"}>{step.name}</span>
                </div>
                <div className={`mt-0.5 hidden text-[11px] sm:block ${lit ? "opacity-80" : "opacity-60"}`}>
                  {step.id === probed && !device.class_override ? "probed" : wasProbed ? "probed, overridden" : lit ? "in use" : ""}
                </div>
              </div>
            );
          })}
        </div>

        {paused ? (
          <p className="rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-sm text-amber-700 dark:text-amber-300">
            Paused until {formatWhen(device.polling_paused_until!)}
            {remaining(device.polling_paused_until!) ? ` (${remaining(device.polling_paused_until!)} left)` : ""}. Only the
            liveness check runs; charts show this time as a gap.
          </p>
        ) : device.polling?.next_runs.counters ? (
          <p className="text-xs text-muted-foreground">Next counters read {inWords(device.polling.next_runs.counters)}.</p>
        ) : null}

        <div className="grid gap-3 sm:grid-cols-2">
          <label className="flex flex-col gap-1 text-sm">
            <span className="text-muted-foreground">Profile</span>
            <select
              className="flex h-8 rounded-md border bg-transparent px-2 text-sm disabled:opacity-60"
              value={device.poll_profile}
              disabled={!isAdmin || busy}
              onChange={(e) =>
                run("Profile saved", () =>
                  api.devices.updatePolling(token, device.id, { profile: e.target.value as PollProfile }),
                )
              }
            >
              <option value="auto">Auto — cadences of its class</option>
              <option value="light">Light — the weak-device cadences</option>
            </select>
          </label>
          <label className="flex flex-col gap-1 text-sm">
            <span className="text-muted-foreground">Class override</span>
            <select
              className="flex h-8 rounded-md border bg-transparent px-2 text-sm disabled:opacity-60"
              value={device.class_override || ""}
              disabled={!isAdmin || busy}
              onChange={(e) =>
                run("Class override saved", () =>
                  api.devices.updatePolling(token, device.id, {
                    class_override: e.target.value ? (e.target.value as DeviceClass) : null,
                  }),
                )
              }
            >
              <option value="">None — use the probed class</option>
              {CLASS_STEPS.map((step) => (
                <option key={step.id} value={step.id} disabled={CLASS_RANK[step.id] > probedRank}>
                  {step.id} {step.name}
                  {CLASS_RANK[step.id] > probedRank ? " — more capable than probed" : ""}
                </option>
              ))}
            </select>
          </label>
        </div>

        {isAdmin ? (
          <div className="flex flex-wrap items-center gap-2">
            {paused ? (
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                onClick={() => run("Polling resumed", () => api.devices.resumePolling(token, device.id))}
              >
                <PlayCircle className="h-4 w-4" />
                Resume polling
              </Button>
            ) : (
              <>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy}
                  onClick={() => run("Polling paused for 1 hour", () => api.devices.pausePolling(token, device.id, 1))}
                >
                  <PauseCircle className="h-4 w-4" />
                  Pause 1 hour
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy}
                  onClick={() => run("Polling paused for 24 hours", () => api.devices.pausePolling(token, device.id, 24))}
                >
                  <PauseCircle className="h-4 w-4" />
                  Pause 24 hours
                </Button>
              </>
            )}
            <span className="text-xs text-muted-foreground">
              A pause keeps the liveness check and stops every other read.
            </span>
          </div>
        ) : (
          <p className="text-xs text-muted-foreground">Only an admin can change polling.</p>
        )}

        {audit.length > 0 ? (
          <ol className="space-y-1 border-t pt-3 text-xs">
            {audit.map((e) => (
              <li key={e.id} className="flex flex-wrap gap-x-2 text-muted-foreground">
                <time dateTime={e.at} className="tabular-nums">
                  {formatWhen(e.at)}
                </time>
                <span>
                  <span className="text-foreground">{e.username || "system"}</span> {auditVerb(e.action)}
                  {e.detail ? ` — ${e.detail}` : ""}
                </span>
              </li>
            ))}
          </ol>
        ) : null}
      </CardContent>
    </Card>
  );
}
