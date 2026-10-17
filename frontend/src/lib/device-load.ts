// Device load — the one vocabulary every view uses for three questions: how
// hard may the NMS poll this device (its class budget), is it polling it
// right now (pause), and how fresh is what the page shows (counter cadence).
// Pure: no React, no I/O. The class tables are TS twins of the backend's
// internal/devclass (cadence.go, live.go) — keep them in step.

import type { Device, DeviceClass } from "./api";

// The classes a device can be budgeted as: an unprobed device (C0, "") is
// polled as C1 until its first /system/resource read.
export type BudgetClass = "C1" | "C2" | "C3";

export interface ClassInfo {
  id: BudgetClass;
  rank: 1 | 2 | 3;
  // Plain word, as the backend's Class.Label() says it.
  word: string;
  name: string;
  hardware: string;
  // Tier 0 (interface counters) cadence in seconds.
  counterSeconds: number;
  // Tier 2a (bridge ports / STP) cadence.
  stp: string;
}

export const CLASS_ORDER: BudgetClass[] = ["C1", "C2", "C3"];

export const CLASSES: Record<BudgetClass, ClassInfo> = {
  C1: {
    id: "C1",
    rank: 1,
    word: "weak",
    name: "Weak",
    hardware: "MIPS, or 128 MiB of memory or less",
    counterSeconds: 60,
    stp: "5 min",
  },
  C2: {
    id: "C2",
    rank: 2,
    word: "small",
    name: "Small",
    hardware: "32-bit ARM, or up to two cores at 1 GHz",
    counterSeconds: 30,
    stp: "2 min",
  },
  C3: {
    id: "C3",
    rank: 3,
    word: "capable",
    name: "Capable",
    hardware: "arm64 or x86 with two or more cores",
    counterSeconds: 15,
    stp: "2 min",
  },
};

// budgeted maps any class value onto the one whose cadences apply: C0, ""
// and anything unknown count as C1 (devclass.Class.Budgeted).
export function budgeted(c: DeviceClass | "" | null | undefined): BudgetClass {
  return c === "C2" || c === "C3" ? c : "C1";
}

// classWord: "weak" / "small" / "capable", "unprobed" for C0 or "".
export function classWord(c: DeviceClass | "" | null | undefined): string {
  return c === "C1" || c === "C2" || c === "C3" ? CLASSES[c].word : "unprobed";
}

type LoadFields = Pick<Device, "effective_class" | "poll_profile" | "device_class" | "class_override">;

// budgetClass is what the pollers budget the device as: its effective class,
// or C1 on the light profile (the class the live caps follow, too).
export function budgetClass(d: Pick<Device, "effective_class" | "poll_profile">): BudgetClass {
  return d.poll_profile === "light" ? "C1" : budgeted(d.effective_class);
}

// Why the budget is not simply the probed class, if it is not.
export type BudgetReason = "unprobed" | "light" | "override" | null;

export function budgetReason(d: LoadFields): BudgetReason {
  if (!d.device_class || d.device_class === "C0") return "unprobed";
  if (d.poll_profile === "light" && budgeted(d.effective_class) !== "C1") return "light";
  if (d.class_override && budgeted(d.class_override) !== budgeted(d.device_class)) return "override";
  return null;
}

export const REASON_SHORT: Record<Exclude<BudgetReason, null>, string> = {
  unprobed: "unprobed",
  light: "light",
  override: "override",
};

// fmtInterval: "15 s", "60 s", "5 min", "1 h" — a cadence as the UI says it.
export function fmtInterval(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  if (s < 120) return `${s} s`;
  if (s < 7200) return `${Math.round(s / 60)} min`;
  return `${Math.round(s / 3600)} h`;
}

// One sentence for a badge's title: what the budget is and why.
export function describeBudget(d: LoadFields): string {
  const b = CLASSES[budgetClass(d)];
  const counters = `Interface counters at most every ${fmtInterval(b.counterSeconds)}.`;
  switch (budgetReason(d)) {
    case "unprobed":
      return `Not probed yet — polled as a weak device (C1) until the first resource read. ${counters}`;
    case "light":
      return `Light profile — polled like a weak device (C1); probed as ${budgeted(d.device_class)} ${classWord(d.device_class)}. ${counters}`;
    case "override":
      return `Overridden to ${b.id} ${b.word}; probed as ${budgeted(d.device_class)} ${classWord(d.device_class)}. ${counters}`;
    default:
      return `${b.id} ${b.word} — ${b.hardware}. ${counters}`;
  }
}

// counterIntervalSeconds is how often this device's interface counters are
// read: the class cadence, floored by the fleet-wide port_stats_interval
// (the interval_seconds / snapshot_interval_seconds the API reports). An
// admin can slow every device down, never push one past its class.
export function counterIntervalSeconds(cls: BudgetClass, floorSeconds?: number | null): number {
  const floor = floorSeconds && floorSeconds > 0 ? floorSeconds : 0;
  return Math.max(CLASSES[cls].counterSeconds, floor);
}

// ---- pause ------------------------------------------------------------------

type PauseFields = Pick<Device, "polling_paused" | "polling_paused_until">;

export function pausedUntilMs(d: PauseFields): number | null {
  if (!d.polling_paused_until) return null;
  const t = Date.parse(d.polling_paused_until);
  return Number.isFinite(t) ? t : null;
}

// isPollingPaused: an admin pause in force at now. polling_paused_until
// stays set after a pause runs out, so the time decides; before the shared
// clock runs (now = 0) the server's own flag stands in.
export function isPollingPaused(d: PauseFields, now: number): boolean {
  const until = pausedUntilMs(d);
  if (until === null) return false;
  if (!now) return !!d.polling_paused;
  return until > now;
}

// "2 h 10 min", "45 min", "under a minute".
export function fmtRemaining(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 60_000) return "under a minute";
  const total = Math.round(ms / 60_000);
  if (total < 60) return `${total} min`;
  const h = Math.floor(total / 60);
  const m = total % 60;
  return m ? `${h} h ${m} min` : `${h} h`;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

// "14:30" today, "Tue 14:30" within the week, "17.10.2026 14:30" beyond —
// 24-hour local time like the rest of the app's timestamps.
export function fmtUntil(atMs: number, nowMs: number): string {
  const at = new Date(atMs);
  const time = `${pad(at.getHours())}:${pad(at.getMinutes())}`;
  const ref = nowMs || atMs;
  if (at.toDateString() === new Date(ref).toDateString()) return time;
  if (atMs > ref && atMs - ref < 6 * 86_400_000) {
    return `${at.toLocaleDateString(undefined, { weekday: "short" })} ${time}`;
  }
  return `${pad(at.getDate())}.${pad(at.getMonth() + 1)}.${at.getFullYear()} ${time}`;
}

// "14:02" today, "17.10. 14:02" this year, "17.10.2025 14:02" before — for
// past events (audit entries, findings).
export function fmtStamp(atMs: number, nowMs: number): string {
  const at = new Date(atMs);
  const time = `${pad(at.getHours())}:${pad(at.getMinutes())}`;
  const ref = new Date(nowMs || atMs);
  if (at.toDateString() === ref.toDateString()) return time;
  if (at.getFullYear() === ref.getFullYear()) return `${pad(at.getDate())}.${pad(at.getMonth() + 1)}. ${time}`;
  return `${pad(at.getDate())}.${pad(at.getMonth() + 1)}.${at.getFullYear()} ${time}`;
}

// "in 42 s", "in 4 min", "in 1 h 5 min", "due now" for a scheduled read.
export function fmtIn(atMs: number, nowMs: number): string {
  const s = Math.round((atMs - nowMs) / 1000);
  if (!Number.isFinite(s) || s <= 0) return "due now";
  if (s < 90) return `in ${s} s`;
  if (s < 5400) return `in ${Math.round(s / 60)} min`;
  return `in ${fmtRemaining(s * 1000)}`;
}

// "just now", "12 s ago", "4 min ago", "3 h ago".
export function fmtAgo(seconds: number): string {
  if (seconds < 2) return "just now";
  if (seconds < 120) return `${Math.round(seconds)} s ago`;
  if (seconds < 7200) return `${Math.round(seconds / 60)} min ago`;
  if (seconds < 172_800) return `${Math.round(seconds / 3600)} h ago`;
  return `${Math.round(seconds / 86_400)} d ago`;
}

// ---- scheduled reads --------------------------------------------------------

// Tier names of device.polling.next_runs (devclass tierNames), in words.
export const TIER_LABELS: Record<string, string> = {
  counters: "Interface counters",
  cpu: "CPU and memory",
  logs: "Log",
  bridge_ports: "Bridge ports and STP",
  bridge_vlan: "Bridge VLANs",
  bridge_hosts: "Bridge host table",
  relations: "Bonds, VLANs and PPPoE",
  neighbors: "Neighbors",
  routes: "Default route",
  ethernet: "Ethernet details",
  arp: "ARP table",
  dhcp_leases: "DHCP leases",
  wifi_registrations: "Wi-Fi registrations",
  inventory: "Inventory, CPU and memory",
};

export function tierLabel(tier: string): string {
  return TIER_LABELS[tier] ?? tier.replaceAll("_", " ");
}

// ---- live view --------------------------------------------------------------

// What the port detail says instead of a refused per-port stream, by the
// refusal code of GET /devices/{id}/live-limits (reason) or subscribe.error
// (code). It names this device's own counter cadence; codes it does not
// know keep the server's message.
export function liveRefusalText(
  code: string | undefined,
  opts: { counterSeconds: number; maxIfaces?: number; serverMessage?: string },
): string {
  const snapshot = `showing snapshot data, one reading every ${fmtInterval(opts.counterSeconds)}`;
  switch (code) {
    case "admin_only":
      return `Live view is limited on weak devices — ${snapshot}.`;
    case "iface_limit":
      return `Live view is limited to ${opts.maxIfaces ?? "a few"} ports at a time on this device — ${snapshot}.`;
    case "lease_limit":
      return `Another session is using this device's live view — ${snapshot}.`;
    case "polling_paused":
      return "NMS polling is paused for this device — showing its last reading.";
    case "pollers_off":
      return "Polling is off on this instance — showing stored data only.";
  }
  return opts.serverMessage || "Live view is not available for this device.";
}
