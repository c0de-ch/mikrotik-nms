"use client";

import { PauseGlyph } from "@/components/devices/device-load";
import type { SnapshotMeta } from "@/lib/api";
import { fmtInterval } from "@/lib/device-load";
import { cn } from "@/lib/utils";
import { useNow } from "./hooks";
import { ageNow, fmtAge } from "./lib";

// SnapshotAge says how old snapshot-derived rates are ("Updated 12 s ago"),
// so a view built on port counters never passes for a live reading. It turns
// amber ("hold") once the data is older than three counter reads — the
// collector has stopped refreshing it (device not answering, or polling
// off) — and says so outright while the device's polling is paused, when the
// rates keep their last reading on purpose.
//
// intervalSeconds is the device's own counter cadence (lib/device-load
// counterIntervalSeconds: a weak device is read every 60 s, not at the
// fleet's 15 s); without it the fleet interval in meta stands. receivedAt is
// when the response carrying meta arrived (Date.now() in the fetch/WS
// handler).
export function SnapshotAge({
  meta,
  receivedAt,
  intervalSeconds,
  paused,
  className,
}: {
  meta: Pick<SnapshotMeta, "age_seconds" | "interval_seconds" | "ready"> | null;
  receivedAt: number;
  intervalSeconds?: number;
  paused?: boolean;
  className?: string;
}) {
  const now = useNow();
  if (!meta || meta.age_seconds === null) {
    return <span className={cn("text-xs text-muted-foreground", className)}>No port counters yet</span>;
  }
  if (!meta.ready) {
    // A cycle has run but rates need two readings: the zeros are not data.
    return <span className={cn("text-xs text-muted-foreground", className)}>Collecting first samples…</span>;
  }
  const age = ageNow(meta.age_seconds, receivedAt, now);
  const interval = intervalSeconds || meta.interval_seconds || 15;
  if (paused) {
    return (
      <span
        className={cn("inline-flex items-center gap-1.5 text-xs tabular-nums text-load-hold", className)}
        title="NMS polling is paused for this device: these rates are its last reading and do not update until polling resumes."
      >
        <PauseGlyph />
        Paused, last read {fmtAge(age)}
      </span>
    );
  }
  const stale = age > 3 * interval;
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-xs tabular-nums",
        stale ? "text-load-hold" : "text-muted-foreground",
        className,
      )}
      title={
        stale
          ? `No new counter reading for over three reads (this device is read every ${fmtInterval(interval)}) — it is not answering, or polling is off.`
          : `Rates from port counters read every ${fmtInterval(interval)}; showing them sends the device no commands.`
      }
    >
      <span aria-hidden className={cn("h-1.5 w-1.5 rounded-full", stale ? "bg-load-hold" : "bg-green-500")} />
      Updated {fmtAge(age)}
    </span>
  );
}
