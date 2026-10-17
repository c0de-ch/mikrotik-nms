"use client";

import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { useSearchParams } from "next/navigation";
import { useAuth } from "@/context/auth";
import { useWebSocket } from "@/hooks/use-websocket";
import {
  api,
  type Device,
  type DevicePortsResponse,
  type FlowDirection,
  type FlowRemoteGrouping,
  type FlowSource,
  type PortRate,
  type PortRole,
  type PortRolesResponse,
  type PortSnapshot,
  type TopDir,
  type TopMetric,
  type TrafficRange,
} from "@/lib/api";
import { errorMessage, isRange, portKey } from "./lib";

// ---- URL state --------------------------------------------------------------

export type TrafficView = "top" | "devices" | "flows";

export interface TrafficParams {
  view: TrafficView;
  device: string | null;
  iface: string | null;
  range: TrafficRange;
  metric: TopMetric;
  dir: TopDir;
  flow: FlowDirection;
  physical: boolean;
  // Flows tab: null = auto (measured when a flow point has recent data).
  fsrc: FlowSource | null;
  // Flows tab (measured): the observation point id; null = default choice.
  point: number | null;
  // Measured Sankey: group the remote side by host or by app.
  remote: FlowRemoteGrouping;
}

export type TrafficParamsPatch = Partial<TrafficParams>;
export type UpdateParams = (patch: TrafficParamsPatch, opts?: { push?: boolean }) => void;

const VIEWS: TrafficView[] = ["top", "devices", "flows"];
const METRICS: TopMetric[] = ["avg", "max", "bytes"];
const DIRS: TopDir[] = ["total", "rx", "tx"];
const FLOWS: FlowDirection[] = ["download", "upload"];
const FSRCS: FlowSource[] = ["flows", "counters"];
const REMOTES: FlowRemoteGrouping[] = ["host", "app"];

function pickId(v: string | null): number | null {
  if (!v || !/^\d+$/.test(v)) return null;
  const n = Number(v);
  return n > 0 && Number.isSafeInteger(n) ? n : null;
}

function pick<T extends string>(v: string | null, allowed: T[], fallback: T): T {
  return v && (allowed as string[]).includes(v) ? (v as T) : fallback;
}

// useTrafficParams reads the drill-down state from the query string and
// writes it back with history.pushState/replaceState, which Next's router
// picks up (useSearchParams re-renders) without a round-trip. Drill-down
// steps push so Back walks up the hierarchy; toggles replace.
export function useTrafficParams(): [TrafficParams, UpdateParams] {
  const sp = useSearchParams();
  const params = useMemo<TrafficParams>(() => {
    const range = sp.get("range");
    return {
      view: pick(sp.get("view"), VIEWS, "top"),
      device: sp.get("device") || null,
      iface: sp.get("iface") || null,
      range: isRange(range) ? range : "1h",
      metric: pick(sp.get("metric"), METRICS, "avg"),
      dir: pick(sp.get("dir"), DIRS, "total"),
      flow: pick(sp.get("flow"), FLOWS, "download"),
      physical: sp.get("physical") !== "0",
      fsrc: (FSRCS as string[]).includes(sp.get("fsrc") ?? "") ? (sp.get("fsrc") as FlowSource) : null,
      point: pickId(sp.get("point")),
      remote: pick(sp.get("remote"), REMOTES, "host"),
    };
  }, [sp]);

  const update = useCallback<UpdateParams>((patch, opts) => {
    const url = new URL(window.location.href);
    const q = url.searchParams;
    const set = (key: string, value: string | null | undefined, dflt?: string) => {
      if (value === undefined) return;
      if (value === null || value === "" || value === dflt) q.delete(key);
      else q.set(key, value);
    };
    set("view", patch.view, "top");
    set("device", patch.device);
    set("iface", patch.iface);
    set("range", patch.range, "1h");
    set("metric", patch.metric, "avg");
    set("dir", patch.dir, "total");
    set("flow", patch.flow, "download");
    if (patch.physical !== undefined) set("physical", patch.physical ? null : "0");
    set("fsrc", patch.fsrc);
    if (patch.point !== undefined) set("point", patch.point === null ? null : String(patch.point));
    set("remote", patch.remote, "host");
    if (url.href === window.location.href) return;
    if (opts?.push) window.history.pushState(null, "", url);
    else window.history.replaceState(null, "", url);
  }, []);

  return [params, update];
}

// ---- polled fetch -----------------------------------------------------------

interface FetchState<T> {
  key: string | null;
  group: string | null;
  data: T | null;
  error: string | null;
}

// usePolledFetch runs fetcher whenever key changes (and every intervalMs
// while mounted). Results are tagged with the key they were fetched for, so
// a stale response never shows under new parameters and "loading" is derived
// rather than set inside the effect. A null key disables fetching.
//
// opts.group: keys in the same group are "the same list, differently sized"
// (e.g. only the row limit changed). While the new key loads, the previous
// group member's data keeps showing (with loading=true) instead of dropping
// to null, so a table doesn't collapse to a skeleton and lose scroll.
export function usePolledFetch<T>(
  key: string | null,
  fetcher: () => Promise<T>,
  intervalMs: number | null,
  opts?: { group?: string },
): { data: T | null; error: string | null; loading: boolean; reload: () => void } {
  const group = opts?.group ?? null;
  const [state, setState] = useState<FetchState<T>>({ key: null, group: null, data: null, error: null });
  const [nonce, setNonce] = useState(0);
  const fetcherRef = useRef(fetcher);
  useEffect(() => {
    fetcherRef.current = fetcher;
  }, [fetcher]);

  useEffect(() => {
    if (!key) return;
    let alive = true;
    const run = () => {
      fetcherRef
        .current()
        .then((data) => {
          if (alive) setState({ key, group, data, error: null });
        })
        .catch((e) => {
          if (!alive) return;
          // Keep the last good data for this key on a failed refresh.
          setState((prev) => ({ key, group, data: prev.key === key ? prev.data : null, error: errorMessage(e) }));
        });
    };
    run();
    const id = intervalMs ? setInterval(run, intervalMs) : null;
    return () => {
      alive = false;
      if (id) clearInterval(id);
    };
  }, [key, group, intervalMs, nonce]);

  const current = key !== null && state.key === key;
  const sibling = !current && key !== null && group !== null && state.group === group;
  const reload = useCallback(() => setNonce((n) => n + 1), []);
  return {
    data: current || sibling ? state.data : null,
    error: current ? state.error : null,
    loading: key !== null && !current,
    reload,
  };
}

// ---- live port snapshot -----------------------------------------------------

export interface SnapshotState {
  snapshot: PortSnapshot | null;
  error: string | null;
  // Snapshot ports grouped by device id.
  byDevice: Map<string, PortRate[]>;
  // Snapshot ports by portKey(device, iface).
  byKey: Map<string, PortRate>;
}

function newer(a: PortSnapshot | null, b: PortSnapshot): boolean {
  if (!a || !a.ts) return true;
  if (!b.ts) return false;
  return Date.parse(b.ts) >= Date.parse(a.ts);
}

// usePortSnapshot seeds from REST /traffic/ports/latest and then follows the
// "traffic.ports" WS topic (one message per collector cycle, ~15 s).
export function usePortSnapshot(): SnapshotState {
  const { token } = useAuth();
  const [snapshot, setSnapshot] = useState<PortSnapshot | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!token) return;
    let alive = true;
    api.traffic
      .portsLatest(token)
      .then((s) => {
        if (!alive) return;
        // The WS may already have delivered a fresher cycle.
        setSnapshot((prev) => (newer(prev, s) ? s : prev));
        setError(null);
      })
      .catch((e) => {
        if (alive) setError(errorMessage(e));
      });
    return () => {
      alive = false;
    };
  }, [token]);

  useWebSocket(
    "traffic.ports",
    useCallback((data: unknown) => {
      const s = data as PortSnapshot;
      if (!s || !Array.isArray(s.ports)) return;
      setSnapshot((prev) => (newer(prev, s) ? s : prev));
      setError(null);
    }, []),
  );

  const { byDevice, byKey } = useMemo(() => {
    const byDevice = new Map<string, PortRate[]>();
    const byKey = new Map<string, PortRate>();
    for (const p of snapshot?.ports ?? []) {
      byKey.set(portKey(p.device_id, p.iface), p);
      const list = byDevice.get(p.device_id);
      if (list) list.push(p);
      else byDevice.set(p.device_id, [p]);
    }
    return { byDevice, byKey };
  }, [snapshot]);

  return { snapshot, error, byDevice, byKey };
}

// ---- roles ------------------------------------------------------------------

export interface RolesState {
  data: PortRolesResponse | null;
  error: string | null;
  byKey: Map<string, PortRole>;
}

const ROLES_REFRESH_MS = 60_000;

export function usePortRoles(): RolesState {
  const { token } = useAuth();
  const fetcher = useCallback(() => api.traffic.portRoles(token!), [token]);
  const { data, error } = usePolledFetch(token ? "roles" : null, fetcher, ROLES_REFRESH_MS);
  const byKey = useMemo(() => {
    const m = new Map<string, PortRole>();
    for (const r of data?.roles ?? []) m.set(portKey(r.device_id, r.iface), r);
    return m;
  }, [data]);
  return { data, error, byKey };
}

// ---- devices ----------------------------------------------------------------

export interface DevicesState {
  devices: Device[];
  byId: Map<string, Device>;
  // true once a fetch has settled (successfully or not).
  loaded: boolean;
  // Last fetch error; null after a successful load.
  error: string | null;
  // Refetch the list (retry after an error, or pick up a newly added device).
  reload: () => void;
}

// useDevices loads the device list (again on reload()) and keeps status fresh
// from device.health, for names and the online/offline triad.
export function useDevices(): DevicesState {
  const { token } = useAuth();
  const [devices, setDevices] = useState<Device[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    if (!token) return;
    let alive = true;
    api.devices
      .list(token)
      .then((d) => {
        if (!alive) return;
        setDevices(d);
        setError(null);
        setLoaded(true);
      })
      .catch((e) => {
        if (!alive) return;
        // Keep whatever list we had; the page surfaces the error with a retry.
        setError(errorMessage(e));
        setLoaded(true);
      });
    return () => {
      alive = false;
    };
  }, [token, nonce]);

  useWebSocket(
    "device.health",
    useCallback((data: unknown) => {
      const u = data as { device_id?: string; status?: Device["status"] };
      if (!u.device_id || !u.status) return;
      // The liveness poll publishes every device every cycle; returning prev
      // when nothing changed skips a whole-page re-render.
      setDevices((prev) => {
        const i = prev.findIndex((d) => d.id === u.device_id);
        if (i < 0 || prev[i].status === u.status) return prev;
        const next = prev.slice();
        next[i] = { ...prev[i], status: u.status! };
        return next;
      });
    }, []),
  );

  const reload = useCallback(() => setNonce((n) => n + 1), []);
  const byId = useMemo(() => new Map(devices.map((d) => [d.id, d])), [devices]);
  return { devices, byId, loaded, error, reload };
}

// ---- one device's ports -----------------------------------------------------

export interface DevicePortsState {
  data: DevicePortsResponse;
  // Date.now() when the answer arrived (SnapshotAge adds the time since).
  receivedAt: number;
}

// useDevicePorts follows GET /devices/{id}/ports — the device's faceplate
// ports with their snapshot rates and the device's own read time — once per
// fleet counter cycle (the floor; a weak device's own reads are rarer and
// SnapshotAge ticks the age between answers). Database only: following it
// costs the device nothing. slow refetches once a minute instead (a paused
// device's rates do not move). null until this device's first answer.
export function useDevicePorts(deviceId: string | null, slow = false): DevicePortsState | null {
  const { token } = useAuth();
  const [state, setState] = useState<DevicePortsState | null>(null);
  const slowRef = useRef(slow);
  useEffect(() => {
    slowRef.current = slow;
  }, [slow]);

  useEffect(() => {
    if (!token || !deviceId) return;
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const load = () => {
      api.devices
        .ports(token, deviceId)
        .then((data) => {
          if (!alive) return;
          setState({ data, receivedAt: Date.now() });
          const secs = slowRef.current ? 60 : Math.max(5, data.interval_seconds || 15);
          timer = setTimeout(load, secs * 1000);
        })
        .catch(() => {
          if (alive) timer = setTimeout(load, 15_000);
        });
    };
    load();
    return () => {
      alive = false;
      clearTimeout(timer);
    };
  }, [token, deviceId]);

  // Until the new device's answer arrives, nothing rather than the previous
  // device's ports.
  return state && state.data.device_id === deviceId ? state : null;
}

// ---- live-stream limits -----------------------------------------------------

// useLiveLimits fetches what a per-port live stream of the device may do for
// this user (null deviceId = not needed); a new session number asks again.
// A failed fetch leaves data null and the caller subscribes anyway — the
// server vets every subscription.
export function useLiveLimits(deviceId: string | null, session = 0) {
  const { token } = useAuth();
  const fetcher = useCallback(() => api.devices.liveLimits(token!, deviceId!), [token, deviceId]);
  return usePolledFetch(token && deviceId ? `live-limits|${deviceId}|${session}` : null, fetcher, null);
}

// ---- clock ------------------------------------------------------------------

// The shared 1 s clock lives in @/hooks/use-now (the device-load badges read
// it too); re-exported for the traffic views.
export { useNow } from "@/hooks/use-now";

// Dark-mode flag. The authenticated layout toggles the "dark" class on <html>
// only after the settings fetch resolves — after the page's first render on a
// reload or deep link — so the flag follows the class instead of reading it
// once at mount.
function subscribeDark(onChange: () => void): () => void {
  const mo = new MutationObserver(onChange);
  mo.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
  return () => mo.disconnect();
}

export function useDarkFlag(): boolean {
  return useSyncExternalStore(
    subscribeDark,
    () => document.documentElement.classList.contains("dark"),
    () => false,
  );
}
