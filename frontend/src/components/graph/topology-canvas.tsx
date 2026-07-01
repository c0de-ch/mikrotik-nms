"use client";

import { useEffect, useMemo, useRef, type ComponentProps } from "react";
import cytoscape from "cytoscape";
import cola from "cytoscape-cola";
import CytoscapeComponent from "react-cytoscapejs";
import type { TopologyNode, TopologyEdge } from "@/lib/api";
import { buildStylesheet, edgeVisual, nodeBg, BRAND } from "./graph-style";

// Register the cola force-layout once (guard against HMR double-registration).
let colaRegistered = false;
if (!colaRegistered) {
  try {
    cytoscape.use(cola);
  } catch {
    /* already registered */
  }
  colaRegistered = true;
}

export interface EdgeTraffic {
  rx: number;
  tx: number;
}

interface Props {
  nodes: TopologyNode[];
  edges: TopologyEdge[];
  statusById: Map<string, string>;
  trafficById: Map<string, EdgeTraffic>;
  onSelect: (id: string | null) => void;
  dark: boolean;
}

export default function TopologyCanvas({ nodes, edges, statusById, trafficById, onSelect, dark }: Props) {
  const cyRef = useRef<cytoscape.Core | null>(null);
  const onSelectRef = useRef(onSelect);
  useEffect(() => {
    onSelectRef.current = onSelect;
  }, [onSelect]);

  // Structural elements — only change when the node/edge SET changes, so live
  // health/traffic patches (below) never trigger a layout reshuffle.
  const elements = useMemo<cytoscape.ElementDefinition[]>(() => {
    const els: cytoscape.ElementDefinition[] = [];
    for (const n of nodes) {
      els.push({ data: { id: n.id, label: n.label, type: n.type || "unknown", bg: nodeBg(n.status) } });
    }
    for (const e of edges) {
      els.push({
        data: { id: e.id, source: e.source, target: e.target, color: BRAND.grey, width: 1.8, idle: 1, status: e.status },
      });
    }
    return els;
  }, [nodes, edges]);

  const stylesheet = useMemo(() => buildStylesheet(dark), [dark]);

  // Structural signature: re-run the layout only when the topology set changes.
  const sig = useMemo(
    () => nodes.map((n) => n.id).sort().join(",") + "|" + edges.map((e) => e.id).sort().join(","),
    [nodes, edges],
  );
  const lastSig = useRef("");
  const runLayout = (cy: cytoscape.Core) => {
    try {
      cy.layout({
        name: "cola",
        animate: true,
        maxSimulationTime: 1500,
        nodeSpacing: 22,
        edgeLength: 130,
        fit: true,
        padding: 44,
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
      } as any).run();
    } catch {
      cy.layout({ name: "cose", animate: true, fit: true, padding: 44 }).run();
    }
  };

  useEffect(() => {
    const cy = cyRef.current;
    if (!cy) return;
    if (lastSig.current === sig) return;
    lastSig.current = sig;
    runLayout(cy);
  }, [sig]);

  // Live node status → colour.
  useEffect(() => {
    const cy = cyRef.current;
    if (!cy) return;
    cy.batch(() => {
      for (const n of nodes) {
        const st = statusById.get(n.id) ?? n.status;
        const el = cy.$id(n.id);
        if (el.nonempty()) el.data("bg", nodeBg(st));
      }
    });
  }, [statusById, nodes]);

  // Live per-link throughput → edge width / colour / flow class.
  useEffect(() => {
    const cy = cyRef.current;
    if (!cy) return;
    cy.batch(() => {
      cy.edges().forEach((e) => {
        const t = trafficById.get(e.id());
        if (!t) return;
        const v = edgeVisual(t.rx, t.tx);
        e.data("width", v.width);
        e.data("color", v.color);
        e.removeData("idle");
        if (v.active) e.addClass("active");
        else e.removeClass("active");
      });
    });
  }, [trafficById]);

  // Animate the dashes on active edges — the "flow" effect.
  useEffect(() => {
    let offset = 0;
    const id = setInterval(() => {
      const cy = cyRef.current;
      if (!cy || document.hidden) return;
      const active = cy.edges(".active");
      if (active.length === 0) return;
      offset -= 1.6;
      try {
        active.style("line-dash-offset", offset);
      } catch {
        /* ignore transient style errors during teardown */
      }
    }, 70);
    return () => clearInterval(id);
  }, []);

  return (
    <CytoscapeComponent
      elements={elements}
      stylesheet={stylesheet as ComponentProps<typeof CytoscapeComponent>["stylesheet"]}
      layout={{ name: "preset" }}
      style={{ width: "100%", height: "100%" }}
      minZoom={0.2}
      maxZoom={2.5}
      cy={(cy: cytoscape.Core) => {
        if (cyRef.current === cy) return;
        cyRef.current = cy;
        cy.on("tap", "node", (evt) => onSelectRef.current(evt.target.id()));
        cy.on("tap", (evt) => {
          if (evt.target === cy) onSelectRef.current(null);
        });
        // Initial layout once the instance exists.
        lastSig.current = sig;
        runLayout(cy);
      }}
    />
  );
}
