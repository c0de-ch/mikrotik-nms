"use client";

import { useCallback, useEffect, useState } from "react";
import { api } from "@/lib/api";
import { useAuth } from "@/context/auth";
import { useWebSocket } from "@/hooks/use-websocket";

// useFiringAlertCount is the number of firing (not acknowledged) alerts,
// for the sidebar badge: read once, then again whenever the server says the
// alerts changed and every few minutes as a fallback.
export function useFiringAlertCount(): number {
  const { token } = useAuth();
  const [count, setCount] = useState(0);

  const load = useCallback(() => {
    if (!token) return;
    api.alerts
      .list(token)
      .then((r) => setCount(r.firing))
      .catch(() => {});
  }, [token]);

  useEffect(() => {
    if (!token) return;
    const first = setTimeout(load, 0);
    const timer = setInterval(load, 5 * 60_000);
    return () => {
      clearTimeout(first);
      clearInterval(timer);
    };
  }, [token, load]);

  useWebSocket("alerts.update", load);
  return count;
}
