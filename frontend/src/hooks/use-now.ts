"use client";

import { useSyncExternalStore } from "react";

// One shared 1 s clock for "updated 12 s ago" / "next read in 42 s" hints,
// however many components read it; it only ticks while something is
// subscribed. Reading the time in the store rather than during render keeps
// renders pure.
let clockNow = 0;
let clockTimer: ReturnType<typeof setInterval> | null = null;
const clockListeners = new Set<() => void>();

function subscribeClock(onChange: () => void): () => void {
  clockListeners.add(onChange);
  if (!clockTimer) {
    clockNow = Date.now();
    clockTimer = setInterval(() => {
      clockNow = Date.now();
      for (const l of clockListeners) l();
    }, 1000);
  }
  return () => {
    clockListeners.delete(onChange);
    if (clockListeners.size === 0 && clockTimer) {
      clearInterval(clockTimer);
      clockTimer = null;
    }
  };
}

// useNow returns the current time in ms. With a coarser resolution (e.g.
// 60_000) the value only changes — and the component only re-renders — once
// per step, which is all a "2 h 10 min left" label needs. It is 0 on the
// first render (before the clock subscribed): callers fall back to the
// server's own flags then.
export function useNow(resolutionMs = 1000): number {
  return useSyncExternalStore(
    subscribeClock,
    () => (resolutionMs <= 1000 ? clockNow : Math.floor(clockNow / resolutionMs) * resolutionMs),
    () => 0,
  );
}
