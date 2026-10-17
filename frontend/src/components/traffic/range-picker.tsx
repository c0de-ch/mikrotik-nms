"use client";

import type { TrafficRange } from "@/lib/api";
import { RANGES, RANGE_LABEL } from "./lib";
import { Segmented } from "./segmented";

const TITLES: Record<TrafficRange, string> = {
  live: "Current rates, updated every collector cycle",
  "15m": "Last 15 minutes (1-minute buckets)",
  "1h": "Last hour (1-minute buckets)",
  "6h": "Last 6 hours (1-minute buckets)",
  "24h": "Last 24 hours (5-minute points)",
  "7d": "Last 7 days (hourly rollups)",
  "30d": "Last 30 days (hourly rollups)",
};

// What Live will show for the open port before anyone clicks it — the
// device's live-view caps: suffix is the sampling period ("15 s" on a weak
// device, the counter cadence when only snapshot data is allowed), title the
// sentence behind it.
export interface LiveHint {
  suffix?: string;
  title: string;
}

// RangePicker is the traffic page's shared time-range selector
// (live | 15m | 1h | 6h | 24h | 7d | 30d). The connectivity page keeps its own.
export function RangePicker({
  value,
  onChange,
  liveHint,
}: {
  value: TrafficRange;
  onChange: (r: TrafficRange) => void;
  liveHint?: LiveHint | null;
}) {
  return (
    <Segmented
      ariaLabel="Time range"
      value={value}
      onChange={onChange}
      options={RANGES.map((r) => ({
        value: r,
        title: r === "live" && liveHint ? liveHint.title : TITLES[r],
        label:
          r === "live" ? (
            <>
              <span
                aria-hidden
                className={`h-1.5 w-1.5 rounded-full ${value === "live" ? "bg-green-400 animate-pulse motion-reduce:animate-none" : "bg-green-500"}`}
              />
              {RANGE_LABEL[r]}
              {liveHint?.suffix && (
                <span className="text-[11px] tabular-nums opacity-70">
                  <span className="sr-only">, </span>
                  {liveHint.suffix}
                </span>
              )}
            </>
          ) : (
            RANGE_LABEL[r]
          ),
      }))}
    />
  );
}
