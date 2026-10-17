"use client";

import { useEffect, useState } from "react";
import { PauseGlyph } from "@/components/devices/device-load";
import { api } from "@/lib/api";

// PollingOffBanner tells everyone on this instance that nothing is being
// polled (MIKROTIK_NMS_POLLERS=off): a dev or staging copy of the database
// shows its history, but no device is contacted and nothing updates. It has
// no close button on purpose — the state belongs to the instance, not to the
// tab, and hiding it is how someone ends up debugging "stale" data for an
// hour. It stays quiet (muted band): informational, not an alarm. Its one
// mark is the device-load pause glyph in the "hold" colour — the instance-
// wide version of a paused device, so the two read as the same state.
export function PollingOffBanner() {
  const [off, setOff] = useState(false);

  useEffect(() => {
    let cancelled = false;
    api
      .health()
      .then((h) => {
        if (!cancelled) setOff(h.pollers === "off");
      })
      .catch(() => {
        // Unreachable backend: nothing to say about polling.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!off) return null;

  return (
    <div
      role="status"
      className="flex items-start gap-2 border-b border-border bg-muted/60 px-4 py-2 text-sm leading-6 text-muted-foreground sm:px-6"
    >
      <PauseGlyph className="mt-1.5 text-load-hold" />
      <p className="min-w-0">
        <span className="font-medium text-foreground">Polling is off on this instance.</span>{" "}
        Devices are not contacted and nothing updates — this is stored history. Start the backend
        with{" "}
        <code className="rounded bg-background px-1.5 py-0.5 font-mono text-xs text-foreground [overflow-wrap:anywhere]">
          MIKROTIK_NMS_POLLERS=local
        </code>{" "}
        to poll.
      </p>
    </div>
  );
}
