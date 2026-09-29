"use client";

import { useCallback, useMemo } from "react";
import { ResponsiveContainer, Sankey, Tooltip, type TooltipContentProps } from "recharts";
import { Waypoints } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { BRAND, SYNTH } from "@/components/graph/graph-style";
import type { FlowDirection, FlowRemoteGrouping, TrafficSankey, TrafficSankeyNode } from "@/lib/api";
import { EmptyState, Notice } from "../notice";
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
} from "../sankey-shared";

const NODE_COLOR: NodeColors = {
  remote: SYNTH.internet,
  app: SYNTH.gateway,
  point: BRAND.primary,
  host: BRAND.grey,
  port: BRAND.grey,
  other: BRAND.grey,
};

function isOther(n: Pick<TrafficSankeyNode, "type">): boolean {
  return n.type === "other";
}

// The upload graph is drawn from the remote side (links reversed), so its
// tooltip reads each link back in traffic direction.
function UploadTip(props: TooltipContentProps) {
  return <SankeyTip {...props} reversed />;
}

// MeasuredSankey: the measured flow graph of one observation point —
// download: remote|app → point → host → port, upload the reverse. Both are
// drawn in the download column order: upload links arrive port → host →
// point → remote, and Recharts puts every node without an incoming link in
// the first column, so hosts without a port link and "Other hosts" would
// land among the ports. Edge width is the average rate over the covered
// part of the range. Click a port (or a host attached to one, or a point
// with a managed port) to open that port's detail.
export function MeasuredSankey({
  data,
  error,
  loading,
  dir,
  remote,
  onOpenPort,
}: {
  data: TrafficSankey | null;
  error: string | null;
  loading: boolean;
  dir: FlowDirection;
  remote: FlowRemoteGrouping;
  onOpenPort: (deviceId: string, iface: string) => void;
}) {
  // The response's own direction, not the prop: while a direction switch is
  // loading, the previous graph stays on screen in its own orientation.
  const reversed = data?.direction === "upload";
  const oriented = useMemo(
    () =>
      data && reversed
        ? { nodes: data.nodes, links: data.links.map((l) => ({ ...l, source: l.target, target: l.source })) }
        : data,
    [data, reversed],
  );
  const dims = useMemo(() => sankeyDims(oriented), [oriented]);
  // Recharts mutates its input: hand it copies.
  const chartData = useMemo(
    () =>
      oriented ? { nodes: oriented.nodes.map((n) => ({ ...n })), links: oriented.links.map((l) => ({ ...l })) } : null,
    [oriented],
  );
  // Host index → the port node it is linked to (its Phase 1 attachment).
  const hostPort = useMemo(() => {
    const m = new Map<string, TrafficSankeyNode>();
    if (!data) return m;
    for (const l of data.links) {
      const s = data.nodes[l.source];
      const t = data.nodes[l.target];
      if (!s || !t) continue;
      if (s.type === "host" && t.type === "port") m.set(s.id, t);
      else if (s.type === "port" && t.type === "host") m.set(t.id, s);
    }
    return m;
  }, [data]);
  const point = data?.point;
  const pointPort = useMemo(
    () => (point && point.port_side && point.device_id && point.iface ? { device: point.device_id, iface: point.iface } : null),
    [point],
  );

  const target = useCallback(
    (n: TrafficSankeyNode): { device: string; iface: string } | null => {
      if (n.type === "port" && n.device_id && n.iface) return { device: n.device_id, iface: n.iface };
      if (n.type === "host") {
        const p = hostPort.get(n.id);
        return p?.device_id && p.iface ? { device: p.device_id, iface: p.iface } : null;
      }
      if (n.type === "point") return pointPort;
      return null;
    },
    [hostPort, pointPort],
  );

  const renderNode = useCallback(
    (props: unknown) => {
      const p = props as NodeShapeProps;
      return <SankeyNodeShape {...p} colors={NODE_COLOR} clickable={!!target(p.payload)} />;
    },
    [target],
  );
  const renderLink = useCallback(
    (props: unknown) => <SankeyLinkShape {...(props as LinkShapeProps)} colors={NODE_COLOR} faint={isOther} />,
    [],
  );
  const onClick = useCallback(
    (item: unknown, type: string) => {
      if (type !== "node") return;
      const n = (item as { payload?: TrafficSankeyNode }).payload;
      const t = n ? target(n) : null;
      if (t) onOpenPort(t.device, t.iface);
    },
    [target, onOpenPort],
  );

  const hasPorts = !!data?.nodes.some((n) => n.type === "port");
  const legend: [string, string][] = [
    remote === "app" ? [NODE_COLOR.app!, "App (remote side)"] : [NODE_COLOR.remote!, "Remote endpoint"],
    [NODE_COLOR.point!, "Observation point"],
    [BRAND.grey, hasPorts ? "Local host / port / other" : "Local host / other"],
  ];

  return (
    <Card className="min-w-0">
      <CardHeader className="gap-1">
        <CardTitle className="flex items-center gap-2 text-base">
          <Waypoints className="h-4 w-4 text-muted-foreground" />
          Flow graph
        </CardTitle>
        <p className="text-xs text-muted-foreground">
          {dir === "download"
            ? `Download: ${remote === "app" ? "apps" : "remote endpoints"} → observation point → local hosts → the ports they are attached to.`
            : `Upload: ports → local hosts → observation point → ${remote === "app" ? "apps" : "remote endpoints"} (drawn from the remote side).`}{" "}
          Top 12 remote and top 15 local entries; the rest is folded into “Other”. Click a port, an attached host or the point to open that port.
        </p>
      </CardHeader>
      <CardContent className="space-y-3">
        {error && <Notice kind="error">Could not load the flow graph: {error}</Notice>}
        {loading && !data ? (
          <div className="h-[320px] animate-pulse rounded-lg bg-muted" />
        ) : data && data.nodes.length === 0 ? (
          <EmptyState icon={Waypoints}>No flows through this point in the range.</EmptyState>
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
                  <Tooltip content={reversed ? UploadTip : SankeyTip} />
                </Sankey>
              </ResponsiveContainer>
            </SankeyScroll>
            <SankeyLegend items={legend} suffix={<span>· edge width = average bits/s</span>} />
          </>
        ) : null}
      </CardContent>
    </Card>
  );
}
