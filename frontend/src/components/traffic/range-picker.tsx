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

// RangePicker is the traffic page's shared time-range selector
// (live | 15m | 1h | 6h | 24h | 7d | 30d). The connectivity page keeps its own.
export function RangePicker({ value, onChange }: { value: TrafficRange; onChange: (r: TrafficRange) => void }) {
  return (
    <Segmented
      ariaLabel="Time range"
      value={value}
      onChange={onChange}
      options={RANGES.map((r) => ({
        value: r,
        title: TITLES[r],
        label:
          r === "live" ? (
            <>
              <span className={`h-1.5 w-1.5 rounded-full ${value === "live" ? "bg-green-400 animate-pulse" : "bg-green-500"}`} />
              {RANGE_LABEL[r]}
            </>
          ) : (
            RANGE_LABEL[r]
          ),
      }))}
    />
  );
}
