"use client";

import { useCallback, useMemo } from "react";
import { ResponsiveContainer, Sankey, Tooltip } from "recharts";
import { GitFork, RotateCcw, Server } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { BRAND, SYNTH } from "@/components/graph/graph-style";
import { useAuth } from "@/context/auth";
import { api, type Device, type TrafficSankeyNode } from "@/lib/api";
import type { TrafficParams, UpdateParams } from "./hooks";
import { usePolledFetch } from "./hooks";
import { RANGE_LABEL, deviceName } from "./lib";
import { EmptyState, Notice } from "./notice";
import {
  MARGIN,
  NODE_PADDING,
  NODE_WIDTH,
  SankeyLegend,
  SankeyLinkShape,
  SankeyNodeShape,
  SankeyScroll,
  SankeyTip,
  sankeyDims,
  type LinkShapeProps,
  type NodeColors,
  type NodeShapeProps,
} from "./sankey-shared";
import { Segmented } from "./segmented";

const LIVE_REFRESH_MS = 15_000;
const RANGE_REFRESH_MS = 300_000;

const NODE_COLOR: NodeColors = {
  internet: SYNTH.internet,
  gateway: SYNTH.gateway,
  vpn: SYNTH.vpn,
  device: BRAND.primary,
  port: BRAND.grey,
  other: BRAND.grey,
};

function isLocal(n: Pick<TrafficSankeyNode, "id">): boolean {
  return n.id.startsWith("local:");
}

// FlowSankey: the Flows tab — an ESTIMATED source→sink tree built by the
// server from port counters and the inferred topology (no flow export). One
// direction at a time; clicking a device re-roots the tree at it, clicking a
// port leaf opens its detail.
export function FlowSankey({
  params,
  update,
  devices,
  onOpenPort,
  onOpenDevice,
}: {
  params: TrafficParams;
  update: UpdateParams;
  devices: Device[];
  onOpenPort: (deviceId: string, iface: string) => void;
  onOpenDevice: (deviceId: string) => void;
}) {
  const { token } = useAuth();
  const { range, flow, device } = params;
  const live = range === "live";
  const fetcher = useCallback(
    () => api.traffic.sankey(token!, { range, dir: flow, device: device ?? undefined }),
    [token, range, flow, device],
  );
  const key = token ? `sankey|${range}|${flow}|${device ?? ""}` : null;
  const { data, error, loading } = usePolledFetch(key, fetcher, live ? LIVE_REFRESH_MS : RANGE_REFRESH_MS);

  const sortedDevices = useMemo(() => [...devices].sort((a, b) => deviceName(a).localeCompare(deviceName(b))), [devices]);
  const dims = useMemo(() => sankeyDims(data), [data]);
  const hasLocal = useMemo(() => !!data?.nodes.some(isLocal), [data]);

  // Every east-west source is named "Local / east-west" by the server and
  // they all sit in the first column, far from the device each one feeds —
  // name them after that device ("Local → switch001") so they can be told apart.
  const chartData = useMemo(() => {
    if (!data) return null;
    const feeds = new Map<number, number>();
    for (const l of data.links) feeds.set(l.source, l.target);
    return {
      nodes: data.nodes.map((n, i) => {
        const target = isLocal(n) ? data.nodes[feeds.get(i) ?? -1] : undefined;
        return target ? { ...n, name: `Local → ${target.name}` } : { ...n };
      }),
      links: data.links.map((l) => ({ ...l })),
    };
  }, [data]);

  const renderNode = useCallback((props: unknown) => {
    const p = props as NodeShapeProps;
    return <SankeyNodeShape {...p} colors={NODE_COLOR} clickable={p.payload.type === "device" || p.payload.type === "port"} />;
  }, []);
  const renderLink = useCallback(
    (props: unknown) => <SankeyLinkShape {...(props as LinkShapeProps)} colors={NODE_COLOR} faint={isLocal} />,
    [],
  );
  const onClick = useCallback(
    (item: unknown, type: string) => {
      if (type !== "node") return;
      const n = (item as { payload?: TrafficSankeyNode }).payload;
      if (!n) return;
      if (n.type === "device" && n.device_id) update({ device: n.device_id }, { push: true });
      else if (n.type === "port" && n.device_id && n.iface) onOpenPort(n.device_id, n.iface);
    },
    [update, onOpenPort],
  );

  const rootName = device ? (() => {
    const d = devices.find((x) => x.id === device);
    return d ? deviceName(d) : device.slice(0, 8);
  })() : null;

  return (
    <Card>
      <CardHeader className="gap-3">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="min-w-0">
            <CardTitle className="flex flex-wrap items-center gap-2 text-base">
              Flows
              <Badge variant="outline" className="gap-1 border-amber-500/40 font-normal text-amber-800 dark:text-amber-300" title="Derived from interface byte counters and the inferred L2 tree — not from flow export. Shared segments and hairpinned traffic can make it approximate.">
                <GitFork className="h-3 w-3" />
                Estimated from port counters + topology
              </Badge>
            </CardTitle>
            <p className="text-xs text-muted-foreground">
              {flow === "download" ? "Download: internet → devices → edge ports" : "Upload: edge ports → devices → internet (drawn from the internet side)"}
              {" · "}
              {live ? "current rates" : `average over the last ${RANGE_LABEL[range]}`}
              {rootName ? ` · rooted at ${rootName}` : ""}. Click a device to root the tree there, a port to open it.
            </p>
            {hasLocal && (
              <p className="text-xs text-muted-foreground">
                {flow === "download"
                  ? "Includes local (east-west) traffic: what a device sends toward its ports beyond what it receives from its parent — e.g. from a NAS or server — enters as a “Local → <device>” source."
                  : "Includes local (east-west) traffic: what a device's ports send beyond what it forwards to its parent enters as a “Local → <device>” source."}
              </p>
            )}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Segmented
            ariaLabel="Flow direction"
            value={flow}
            onChange={(f) => update({ flow: f })}
            options={[
              { value: "download", label: "↓ Download" },
              { value: "upload", label: "↑ Upload" },
            ]}
          />
          <select
            aria-label="Root device"
            className="h-8 max-w-[240px] rounded-md border bg-background px-2 text-sm"
            value={device ?? ""}
            onChange={(e) => update({ device: e.target.value || null })}
          >
            <option value="">Whole network</option>
            {sortedDevices.map((d) => (
              <option key={d.id} value={d.id}>
                {deviceName(d)}
              </option>
            ))}
          </select>
          {device && (
            <>
              <Button variant="outline" size="sm" onClick={() => update({ device: null }, { push: true })}>
                <RotateCcw /> Whole network
              </Button>
              <Button variant="ghost" size="sm" onClick={() => onOpenDevice(device)}>
                <Server /> Open device
              </Button>
            </>
          )}
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {error && <Notice kind="error">Could not load flows: {error}</Notice>}
        {loading && !data ? (
          <div className="h-[320px] animate-pulse rounded-lg bg-muted" />
        ) : data && data.nodes.length === 0 ? (
          <EmptyState icon={GitFork}>No flows above 1 kbps (or topology not discovered yet).</EmptyState>
        ) : chartData && dims ? (
          <>
            <SankeyScroll dims={dims} loading={loading}>
              <ResponsiveContainer width="100%" height="100%">
                <Sankey
                  data={chartData}
                  node={renderNode}
                  link={renderLink}
                  nodeWidth={NODE_WIDTH}
                  nodePadding={NODE_PADDING}
                  linkCurvature={0.5}
                  iterations={48}
                  align="left"
                  margin={{ ...MARGIN, right: dims.right }}
                  onClick={onClick}
                >
                  <Tooltip content={SankeyTip} />
                </Sankey>
              </ResponsiveContainer>
            </SankeyScroll>
            <SankeyLegend
              items={(
                [
                  ["internet", "Internet"],
                  ["gateway", "Gateway"],
                  ["vpn", "VPN"],
                  ["device", "Device"],
                  ["port", hasLocal ? "Edge port / local / other" : "Edge port / other"],
                ] as [TrafficSankeyNode["type"], string][]
              ).map(([t, l]) => [NODE_COLOR[t] ?? BRAND.grey, l])}
              suffix={<span>· edge width = bits/s</span>}
            />
          </>
        ) : null}
      </CardContent>
    </Card>
  );
}
