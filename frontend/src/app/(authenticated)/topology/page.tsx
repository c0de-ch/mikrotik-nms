"use client";

import { useEffect, useState, useRef } from "react";
import CytoscapeComponent from "react-cytoscapejs";
import type cytoscape from "cytoscape";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ExternalLink, Maximize } from "lucide-react";
import { useAuth } from "@/context/auth";
import { api, type TopologyData, type TopologyNode } from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const cyStylesheet: any[] = [
  {
    selector: "node",
    style: {
      label: "data(label)",
      "text-valign": "bottom",
      "text-margin-y": 8,
      "font-size": 11,
      "background-color": "#6b7280",
      width: 40,
      height: 40,
      "border-width": 2,
      "border-color": "#374151",
    },
  },
  {
    selector: "node[status='online']",
    style: { "background-color": "#22c55e", "border-color": "#16a34a" },
  },
  {
    selector: "node[status='offline']",
    style: { "background-color": "#ef4444", "border-color": "#dc2626" },
  },
  {
    selector: "node[type='switch']",
    style: { shape: "round-rectangle" },
  },
  {
    selector: "node[type='ap']",
    style: { shape: "diamond" },
  },
  {
    selector: "edge",
    style: {
      width: 2,
      "line-color": "#9ca3af",
      "curve-style": "bezier",
      label: "data(label)",
      "font-size": 9,
      "text-rotation": "autorotate",
      color: "#6b7280",
    },
  },
  {
    selector: "edge[status='down']",
    style: { "line-style": "dashed", "line-color": "#ef4444" },
  },
];

export default function TopologyPage() {
  const { token } = useAuth();
  const [topology, setTopology] = useState<TopologyData | null>(null);
  const [selected, setSelected] = useState<TopologyNode | null>(null);
  const cyRef = useRef<cytoscape.Core | null>(null);

  useEffect(() => {
    if (!token) return;
    api.topology.get(token).then(setTopology).catch(console.error);
  }, [token]);

  useWebSocket("topology.update", (data) => {
    setTopology(data as TopologyData);
  });

  const elements = topology
    ? [
        ...topology.nodes.map((n) => ({
          data: { ...n.data },
        })),
        ...topology.edges.map((e) => ({
          data: {
            ...e.data,
            label: `${e.data.source_interface} ↔ ${e.data.target_interface}`,
          },
        })),
      ]
    : [];

  const handleCyReady = (cy: cytoscape.Core) => {
    cyRef.current = cy;
    cy.on("tap", "node", (evt) => {
      const node = evt.target.data() as TopologyNode;
      setSelected(node);
    });
    cy.on("tap", (evt) => {
      if (evt.target === cy) setSelected(null);
    });
  };

  const fitGraph = () => {
    cyRef.current?.fit(undefined, 50);
  };

  return (
    <div className="flex h-[calc(100vh-6rem)] gap-4">
      <div className="relative flex-1 rounded-lg border bg-muted/30">
        {elements.length > 0 ? (
          <CytoscapeComponent
            elements={elements}
            stylesheet={cyStylesheet}
            layout={{ name: "cose", animate: true, nodeDimensionsIncludeLabels: true } as cytoscape.LayoutOptions}
            className="h-full w-full"
            cy={handleCyReady}
          />
        ) : (
          <div className="flex h-full items-center justify-center text-muted-foreground">
            {topology ? "No topology data — add devices and wait for discovery" : "Loading topology..."}
          </div>
        )}
        <div className="absolute right-3 top-3">
          <Button variant="outline" size="icon" onClick={fitGraph} title="Fit to screen">
            <Maximize className="h-4 w-4" />
          </Button>
        </div>
      </div>

      {selected && (
        <Card className="w-72 shrink-0">
          <CardHeader className="pb-3">
            <CardTitle className="text-base">{selected.label}</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2 text-sm">
            <div className="flex justify-between">
              <span className="text-muted-foreground">Status</span>
              <Badge variant={selected.status === "online" ? "default" : "destructive"}>
                {selected.status}
              </Badge>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">Address</span>
              <span>{selected.address}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">Model</span>
              <span>{selected.model || "—"}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">Version</span>
              <span>{selected.ros_version || "—"}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">CPU</span>
              <span>{selected.cpu_load != null ? `${selected.cpu_load}%` : "—"}</span>
            </div>
            <Button className="mt-4 w-full" variant="outline" render={<a href={`winbox://${selected.address}`} />}>
              <ExternalLink className="mr-2 h-4 w-4" />
              Open in WinBox
            </Button>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
