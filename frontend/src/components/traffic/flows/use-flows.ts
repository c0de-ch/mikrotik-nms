"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useAuth } from "@/context/auth";
import { useWebSocket } from "@/hooks/use-websocket";
import { api, type FlowFlushedEvent, type FlowPoint, type FlowStatus } from "@/lib/api";
import { usePolledFetch } from "../hooks";

export const TOPIC_FLUSHED = "flows.flushed";
export const TOPIC_STATUS = "flows.status";

const STATUS_REFRESH_MS = 60_000;
const POINTS_REFRESH_MS = 60_000;
const FLUSH_THROTTLE_MS = 20_000;

export interface FlowStatusState {
  status: FlowStatus | null;
  error: string | null;
  loaded: boolean;
  reload: () => void;
}

// useFlowStatus: REST /flows/status (every 60 s, and on reload()) followed
// live by the "flows.status" WS topic (every 30 s while the collector runs).
// The WS payload has no suggested_exporters; they are kept from REST.
export function useFlowStatus(): FlowStatusState {
  const { token } = useAuth();
  const fetcher = useCallback(() => api.flows.status(token!), [token]);
  const { data, error, loading, reload } = usePolledFetch(token ? "flows-status" : null, fetcher, STATUS_REFRESH_MS);

  // A WS copy newer than the last REST answer. A fresh REST answer (e.g.
  // right after an admin write) replaces it — adjusting state during render,
  // per the React docs pattern.
  const [live, setLive] = useState<FlowStatus | null>(null);
  const [prevRest, setPrevRest] = useState(data);
  if (data !== prevRest) {
    setPrevRest(data);
    setLive(null);
  }
  useWebSocket(
    TOPIC_STATUS,
    useCallback((msg: unknown) => {
      const s = msg as FlowStatus;
      if (!s || !Array.isArray(s.exporters)) return;
      setLive(s);
    }, []),
  );

  const status = useMemo<FlowStatus | null>(() => {
    if (!live) return data;
    return { ...live, suggested_exporters: data?.suggested_exporters ?? [] };
  }, [live, data]);
  return { status, error: status ? null : error, loaded: !loading || !!error, reload };
}

export interface FlowPointsState {
  points: FlowPoint[] | null;
  error: string | null;
  loaded: boolean;
  reload: () => void;
}

export function useFlowPoints(): FlowPointsState {
  const { token } = useAuth();
  const fetcher = useCallback(() => api.flows.points(token!), [token]);
  const { data, error, loading, reload } = usePolledFetch(token ? "flows-points" : null, fetcher, POINTS_REFRESH_MS);
  return { points: data?.points ?? null, error: data ? null : error, loaded: !loading || !!error, reload };
}

// useFlushedRefresh calls refresh() when the collector flushes a minute of
// the given exporter (WS "flows.flushed"), at most once per 20 s (a trailing
// call catches the last flush of a burst). Inactive → not subscribed.
export function useFlushedRefresh(active: boolean, exporterId: number | null, refresh: () => void) {
  const refreshRef = useRef(refresh);
  useEffect(() => {
    refreshRef.current = refresh;
  }, [refresh]);
  const last = useRef(0);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current);
      timer.current = null;
    },
    [],
  );
  useWebSocket(
    active ? TOPIC_FLUSHED : "",
    useCallback(
      (msg: unknown) => {
        const ev = msg as FlowFlushedEvent;
        if (exporterId !== null && Array.isArray(ev?.exporter_ids) && !ev.exporter_ids.includes(exporterId)) return;
        const wait = last.current + FLUSH_THROTTLE_MS - Date.now();
        if (wait <= 0) {
          last.current = Date.now();
          refreshRef.current();
          return;
        }
        if (timer.current) return;
        timer.current = setTimeout(() => {
          timer.current = null;
          last.current = Date.now();
          refreshRef.current();
        }, wait);
      },
      [exporterId],
    ),
  );
}
