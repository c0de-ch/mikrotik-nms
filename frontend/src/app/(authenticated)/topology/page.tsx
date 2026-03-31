"use client";

import { useEffect, useState, useMemo } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Copy, Search, Server, Wifi, Network as NetworkIcon } from "lucide-react";
import { toast } from "sonner";

function copyText(text: string) {
  if (navigator.clipboard?.writeText) {
    navigator.clipboard.writeText(text);
  } else {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    document.execCommand("copy");
    document.body.removeChild(ta);
  }
}
import { useAuth } from "@/context/auth";
import { api, type TopologyData, type TopologyNode, type TopologyEdge } from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";

interface PortConnection {
  localInterface: string;
  remoteDevice: string;
  remoteInterface: string;
  remoteStatus: string;
  linkType: string;
  linkStatus: string;
}

function DeviceIcon({ type }: { type: string }) {
  if (type === "switch") return <NetworkIcon className="h-5 w-5" />;
  if (type === "ap") return <Wifi className="h-5 w-5" />;
  return <Server className="h-5 w-5" />;
}

function statusColor(status: string) {
  if (status === "online") return "bg-green-500";
  if (status === "offline") return "bg-red-500";
  return "bg-gray-400";
}

export default function TopologyPage() {
  const { token } = useAuth();
  const [topology, setTopology] = useState<TopologyData | null>(null);
  const [search, setSearch] = useState("");

  useEffect(() => {
    if (!token) return;
    api.topology.get(token).then(setTopology).catch(console.error);
  }, [token]);

  useWebSocket("topology.update", (data) => {
    setTopology(data as TopologyData);
  });

  // Build per-device port connections
  const { deviceConnections, nodeMap } = useMemo(() => {
    if (!topology) return { deviceConnections: new Map<string, PortConnection[]>(), nodeMap: new Map<string, TopologyNode>() };

    const nMap = new Map<string, TopologyNode>();
    for (const n of topology.nodes) {
      nMap.set(n.data.id, n.data);
    }

    const conns = new Map<string, PortConnection[]>();

    // Initialize all devices
    for (const n of topology.nodes) {
      conns.set(n.data.id, []);
    }

    for (const e of topology.edges) {
      const src = nMap.get(e.data.source);
      const tgt = nMap.get(e.data.target);
      if (!src || !tgt) continue;

      // Add connection to source device
      conns.get(e.data.source)?.push({
        localInterface: e.data.source_interface,
        remoteDevice: tgt.label,
        remoteInterface: e.data.target_interface,
        remoteStatus: tgt.status,
        linkType: e.data.link_type,
        linkStatus: e.data.status,
      });

      // Add connection to target device
      conns.get(e.data.target)?.push({
        localInterface: e.data.target_interface,
        remoteDevice: src.label,
        remoteInterface: e.data.source_interface,
        remoteStatus: src.status,
        linkType: e.data.link_type,
        linkStatus: e.data.status,
      });
    }

    // Sort ports naturally
    for (const [, ports] of conns) {
      ports.sort((a, b) => a.localInterface.localeCompare(b.localInterface, undefined, { numeric: true }));
    }

    return { deviceConnections: conns, nodeMap: nMap };
  }, [topology]);

  // Filter devices
  const filteredNodes = useMemo(() => {
    if (!topology) return [];
    const q = search.toLowerCase();
    return topology.nodes
      .map((n) => n.data)
      .filter((n) => !q || n.label.toLowerCase().includes(q) || n.address.toLowerCase().includes(q) || n.model.toLowerCase().includes(q))
      .sort((a, b) => {
        // Online first, then by name
        if (a.status !== b.status) return a.status === "online" ? -1 : 1;
        return a.label.localeCompare(b.label);
      });
  }, [topology, search]);

  if (!topology) {
    return (
      <div className="flex h-[50vh] items-center justify-center text-muted-foreground">
        Loading topology...
      </div>
    );
  }

  if (topology.nodes.length === 0) {
    return (
      <div className="flex h-[50vh] items-center justify-center text-muted-foreground">
        No topology data — add devices and wait for discovery
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Network Topology</h1>
          <p className="text-sm text-muted-foreground">
            {topology.nodes.length} devices · {topology.edges.length} connections
          </p>
        </div>
        <div className="relative w-64">
          <Search className="absolute left-2.5 top-2 h-4 w-4 text-muted-foreground" />
          <Input
            placeholder="Search devices..."
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="pl-8"
          />
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {filteredNodes.map((node) => {
          const ports = deviceConnections.get(node.id) || [];
          return (
            <Card key={node.id} className="overflow-hidden">
              <CardHeader className="pb-2">
                <div className="flex items-center gap-3">
                  <div className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-lg text-white ${node.status === "online" ? "bg-primary" : "bg-muted-foreground/50"}`}>
                    <DeviceIcon type={node.type} />
                  </div>
                  <div className="flex-1 min-w-0">
                    <CardTitle className="text-sm truncate">{node.label}</CardTitle>
                    <p className="text-xs text-muted-foreground">{node.address} · {node.model}</p>
                  </div>
                  <div className="flex items-center gap-2 shrink-0">
                    <div className={`h-2.5 w-2.5 rounded-full ${statusColor(node.status)}`} />
                    <Button variant="ghost" size="icon" className="h-7 w-7" title="Copy IP" onClick={(e) => { e.stopPropagation(); copyText(node.address); toast.success(`Copied ${node.address}`); }}>
                      <Copy className="h-3.5 w-3.5" />
                    </Button>
                  </div>
                </div>
              </CardHeader>
              <CardContent className="pt-0">
                {ports.length > 0 ? (
                  <div className="space-y-0.5">
                    {ports.map((port, i) => (
                      <div key={`${port.localInterface}-${i}`} className="flex items-center gap-2 rounded px-2 py-1.5 text-xs hover:bg-muted/50">
                        <span className="font-mono font-medium w-20 shrink-0 truncate" title={port.localInterface}>
                          {port.localInterface}
                        </span>
                        <span className="text-muted-foreground">→</span>
                        <div className="flex-1 min-w-0 flex items-center gap-1.5">
                          <div className={`h-1.5 w-1.5 rounded-full shrink-0 ${statusColor(port.remoteStatus)}`} />
                          <span className="truncate font-medium">{port.remoteDevice}</span>
                          <span className="text-muted-foreground font-mono shrink-0">:{port.remoteInterface}</span>
                        </div>
                        {port.linkType === "wireless" && (
                          <Wifi className="h-3 w-3 text-muted-foreground shrink-0" />
                        )}
                      </div>
                    ))}
                  </div>
                ) : (
                  <p className="text-xs text-muted-foreground py-2">No discovered connections</p>
                )}
                {node.cpu_load != null && (
                  <div className="mt-2 pt-2 border-t flex gap-4 text-[10px] text-muted-foreground">
                    <span>CPU {node.cpu_load}%</span>
                    <span>v{node.ros_version}</span>
                  </div>
                )}
              </CardContent>
            </Card>
          );
        })}
      </div>
    </div>
  );
}
