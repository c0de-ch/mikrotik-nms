"use client";

import type { SnapshotMeta } from "@/lib/api";
import { cn } from "@/lib/utils";
import { useNow } from "./hooks";
import { ageNow, fmtAge } from "./lib";

// SnapshotAge says how old snapshot-derived rates are ("updated 12 s ago"),
// so a view built on port counters never passes for a live reading. It turns
// amber once the data is older than three counter cycles — the collector has
// stopped refreshing it (device offline, or polling off). receivedAt is when
// the response carrying meta arrived (Date.now() in the fetch/WS handler).
export function SnapshotAge({
  meta,
  receivedAt,
  className,
}: {
  meta: Pick<SnapshotMeta, "age_seconds" | "interval_seconds" | "ready"> | null;
  receivedAt: number;
  className?: string;
}) {
  const now = useNow();
  if (!meta || meta.age_seconds === null) {
    return <span className={cn("text-xs text-muted-foreground", className)}>No port counters yet</span>;
  }
  const age = ageNow(meta.age_seconds, receivedAt, now);
  const interval = meta.interval_seconds || 15;
  const stale = age > 3 * interval;
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-xs tabular-nums",
        stale ? "text-amber-700 dark:text-amber-400" : "text-muted-foreground",
        className,
      )}
      title={`Rates come from port counters read every ${interval} s; showing them sends the devices no commands.`}
    >
      <span
        aria-hidden
        className={cn("h-1.5 w-1.5 rounded-full", stale ? "bg-amber-500" : "bg-emerald-500/80")}
      />
      Updated {fmtAge(age)}
    </span>
  );
}
