"use client";

import { useEffect } from "react";
import { api } from "@/lib/api";

// useVersionWatchdog polls /api/v1/health for the backend's instance_id and
// forces a full-page reload when the value changes. instance_id is a random
// 64-bit hex generated each time the backend process starts, so any restart
// (deploy, manual systemctl restart) produces a new value.
//
// Why: after a deploy the Next.js chunk hashes change. A tab that already
// loaded the old HTML keeps trying to import dead chunks and never
// recovers — empty page, blank screen, "the page is dead". The Caddyfile
// is also configured to send `Cache-Control: no-store` on the HTML so a
// reload always fetches fresh, but the tab has to *initiate* that reload.
// This hook is the thing that initiates it.
//
// Triggers a check on three signals:
//   • interval: every 5 min while the tab is visible
//   • visibilitychange: when the tab regains focus after being hidden
//   • window focus: when the user switches back to this tab
//
// First successful response stores the baseline instance_id. Subsequent
// responses with a different value → reload. Network errors are silently
// ignored; if the backend is genuinely down, browser cache control will
// surface that on the next user interaction anyway.
export function useVersionWatchdog() {
  useEffect(() => {
    if (typeof window === "undefined") return;
    let baseline: string | null = null;

    const check = async () => {
      try {
        const res = await api.health();
        // The Health type only exposes status, but the backend now also
        // returns instance_id. Read it dynamically so we don't have to
        // touch the API client surface for this hook.
        const id = (res as unknown as { instance_id?: string }).instance_id;
        if (!id) return;
        if (baseline == null) {
          baseline = id;
          return;
        }
        if (id !== baseline) {
          // The HTML is no-store so this fetches the fresh entrypoint and
          // therefore the fresh chunk hashes. No special argument needed
          // on modern browsers.
          window.location.reload();
        }
      } catch {
        // Backend unreachable — leave baseline alone, retry on next tick.
      }
    };

    check(); // capture baseline immediately

    const interval = setInterval(check, 5 * 60 * 1000);
    const onVisibility = () => { if (document.visibilityState === "visible") check(); };
    document.addEventListener("visibilitychange", onVisibility);
    window.addEventListener("focus", check);

    return () => {
      clearInterval(interval);
      document.removeEventListener("visibilitychange", onVisibility);
      window.removeEventListener("focus", check);
    };
  }, []);
}
