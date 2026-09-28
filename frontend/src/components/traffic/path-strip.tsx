"use client";

import { Fragment, useCallback, useEffect, useMemo, useRef } from "react";
import { Globe, Lock, Server, Shield, Users } from "lucide-react";
import { BRAND, SYNTH, fmtBps } from "@/components/graph/graph-style";
import { useAuth } from "@/context/auth";
import { api, type PathHop, type PortRate, type PortRole } from "@/lib/api";
import { cn } from "@/lib/utils";
import { usePolledFetch } from "./hooks";
import { portKey } from "./lib";
import { Notice } from "./notice";

const PATH_REFRESH_MS = 60_000;

// liveHops re-derives each segment's rates from the latest snapshot with the
// contract's path-segment rule: prefer the previous device's out-port
// (down = tx, up = rx), else the hop's own in-port (down = rx, up = tx); a
// clients sink reads the requested port's tx/rx. Unmatched hops keep the
// server's values.
//
// Exception (as on the server): when the previous device's out-port feeds
// several managed devices (neighbor_count > 1 — an unmanaged switch or PtMP
// radio in between), its aggregate would be credited to each of them, so the
// hop's own in-port is measured first.
function liveHops(
  hops: PathHop[],
  rate: (dev: string, iface: string) => PortRate | undefined,
  shared: (dev: string, iface: string) => boolean,
): PathHop[] {
  return hops.map((h, k) => {
    if (k === 0) return h;
    const prev = hops[k - 1];
    if (h.kind === "clients") {
      const r = prev.device_id && prev.out_iface ? rate(prev.device_id, prev.out_iface) : undefined;
      return r ? { ...h, down_bps: r.tx_bps, up_bps: r.rx_bps, measured: true } : h;
    }
    const own = () => {
      if (h.kind !== "device" || !h.device_id || !h.in_iface) return null;
      const r = rate(h.device_id, h.in_iface);
      return r ? { ...h, down_bps: r.rx_bps, up_bps: r.tx_bps, measured: true } : null;
    };
    const prevOut = prev.kind === "device" && prev.device_id && prev.out_iface ? { dev: prev.device_id, iface: prev.out_iface } : null;
    if (prevOut && shared(prevOut.dev, prevOut.iface)) {
      const o = own();
      if (o) return o;
    }
    if (prevOut) {
      const r = rate(prevOut.dev, prevOut.iface);
      if (r) return { ...h, down_bps: r.tx_bps, up_bps: r.rx_bps, measured: true };
    }
    return own() ?? h;
  });
}

function hopColor(kind: PathHop["kind"]): string {
  if (kind === "internet") return SYNTH.internet;
  if (kind === "gateway") return SYNTH.gateway;
  if (kind === "vpn") return SYNTH.vpn;
  if (kind === "device") return BRAND.primary;
  return BRAND.grey;
}

function HopIcon({ kind, className }: { kind: PathHop["kind"]; className?: string }) {
  if (kind === "internet") return <Globe className={className} />;
  if (kind === "gateway") return <Shield className={className} />;
  if (kind === "vpn") return <Lock className={className} />;
  if (kind === "clients") return <Users className={className} />;
  return <Server className={className} />;
}

// PathStrip: Internet → … → this port → what's behind it, with live
// download/upload per segment from the traffic.ports snapshot.
export function PathStrip({
  deviceId,
  iface,
  byKey,
  roles,
  live,
  onOpenPort,
}: {
  deviceId: string;
  iface: string;
  byKey: Map<string, PortRate>;
  roles: Map<string, PortRole>;
  live: boolean;
  onOpenPort: (deviceId: string, iface: string) => void;
}) {
  const { token } = useAuth();
  const fetcher = useCallback(() => api.traffic.path(token!, deviceId, iface), [token, deviceId, iface]);
  const { data, error, loading } = usePolledFetch(token ? `path|${deviceId}|${iface}` : null, fetcher, PATH_REFRESH_MS);

  const hops = useMemo(() => {
    if (!data) return [];
    if (!live) return data.hops;
    return liveHops(
      data.hops,
      (dev, i) => byKey.get(portKey(dev, i)),
      (dev, i) => (roles.get(portKey(dev, i))?.neighbor_count ?? 0) > 1,
    );
  }, [data, live, byKey, roles]);
  const target = useMemo(() => {
    let t = -1;
    hops.forEach((h, i) => {
      if (!h.sink) t = i;
    });
    return t;
  }, [hops]);

  // On narrow screens the strip scrolls; bring the requested port's hop (and
  // what's behind it) into view instead of leaving it off to the right.
  const stripRef = useRef<HTMLDivElement>(null);
  const hopCount = hops.length;
  useEffect(() => {
    const el = stripRef.current;
    const t = el?.querySelector<HTMLElement>("[data-target]");
    if (!el || !t) return;
    const overflow = t.offsetLeft + t.offsetWidth + 120 - el.clientWidth;
    if (overflow > 0) el.scrollLeft = overflow;
  }, [hopCount, deviceId, iface]);

  if (error) return <Notice kind="error">Could not load the path: {error}</Notice>;
  if (loading || !data) return <div className="h-16 animate-pulse rounded-lg bg-muted" />;

  return (
    <div className="space-y-2">
      {!data.anchored && <Notice>No internet uplink detected — showing the L2 tree from its top.</Notice>}
      <div ref={stripRef} className="relative flex items-stretch overflow-x-auto pb-1">
        {hops.map((h, i) => {
          const isTarget = i === target;
          let open: (() => void) | undefined;
          if (h.kind === "device" && h.device_id && !isTarget) {
            // Follow the path: an upstream hop opens its port toward us, a
            // downstream (sink) device opens its uplink port.
            const p = h.sink ? h.in_iface : h.out_iface;
            if (p) open = () => onOpenPort(h.device_id!, p);
          }
          return (
            <Fragment key={`${h.id}-${i}`}>
              {i > 0 && <Segment hop={h} />}
              <HopNode hop={h} isTarget={isTarget} onClick={open} />
            </Fragment>
          );
        })}
      </div>
    </div>
  );
}

function Segment({ hop }: { hop: PathHop }) {
  return (
    <div className="flex min-w-[92px] flex-1 flex-col items-center justify-center gap-0.5 px-1 font-mono text-[10px] tabular-nums text-muted-foreground">
      <span title="Download on this segment">{hop.measured ? `↓ ${fmtBps(hop.down_bps)}` : "—"}</span>
      <div className={cn("relative h-0.5 w-full", hop.measured ? "bg-foreground/30" : "border-t border-dashed border-foreground/30")}>
        <span className="absolute -right-0.5 -top-[3px] h-0 w-0 border-y-4 border-l-[6px] border-y-transparent border-l-foreground/30" />
      </div>
      <span title="Upload on this segment">{hop.measured ? `↑ ${fmtBps(hop.up_bps)}` : ""}</span>
    </div>
  );
}

function HopNode({ hop, isTarget, onClick }: { hop: PathHop; isTarget: boolean; onClick?: () => void }) {
  const label = hop.kind === "clients" ? `${hop.client_count ?? 0} client${hop.client_count === 1 ? "" : "s"}` : hop.label;
  const ports =
    hop.kind === "device" && (hop.in_iface || hop.out_iface)
      ? [hop.in_iface, hop.out_iface].filter(Boolean).join(" → ")
      : null;
  const body = (
    <>
      <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-white" style={{ background: hopColor(hop.kind) }}>
        <HopIcon kind={hop.kind} className="h-3.5 w-3.5" />
      </span>
      <span className="min-w-0">
        <span className="block max-w-[160px] truncate text-xs font-medium" title={label}>
          {label}
        </span>
        {ports && <span className="block max-w-[160px] truncate font-mono text-[10px] text-muted-foreground" title={ports}>{ports}</span>}
      </span>
    </>
  );
  const cls = cn(
    "flex shrink-0 items-center gap-2 rounded-lg border bg-card px-2.5 py-1.5 text-left",
    hop.sink && "border-dashed opacity-80",
    isTarget && "ring-2 ring-primary",
  );
  if (onClick) {
    return (
      <button type="button" onClick={onClick} className={cn(cls, "transition-colors hover:bg-muted")} title="Open this hop's port on the path">
        {body}
      </button>
    );
  }
  return (
    <div className={cls} data-target={isTarget ? "" : undefined}>
      {body}
    </div>
  );
}
