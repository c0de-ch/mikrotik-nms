"use client";

import { useEffect, useMemo, useRef, type ComponentProps } from "react";
import cytoscape from "cytoscape";
import cola from "cytoscape-cola";
import CytoscapeComponent from "react-cytoscapejs";
import type { MapPosition, TopologyNode } from "@/lib/api";
import { buildStylesheet, edgeVisual, nodeBg, fmtBps, BRAND, SYNTH, SYNTH_TYPES } from "./graph-style";

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
  // No port measures the edge itself: the label says "≈".
  estimated?: boolean;
}

export interface CanvasApi {
  fit: () => void;
  zoomBy: (factor: number) => void;
  fitTo: (ids: string[]) => void;
}

export interface CanvasEdge {
  id: string;
  source: string;
  target: string;
  link_type: string;
  status: string;
}

interface Props {
  nodes: TopologyNode[];
  edges: CanvasEdge[];
  statusById: Map<string, string>;
  trafficById: Map<string, EdgeTraffic>;
  matchIds?: Set<string> | null;
  layoutName?: "force" | "hierarchy";
  onSelect: (id: string | null) => void;
  registerApi?: (api: CanvasApi) => void;
  dark: boolean;
  // The saved layout (null until it has loaded: nothing is placed before).
  // A node with a saved position is put there; the others are laid out
  // around them.
  savedPositions: Record<string, MapPosition> | null;
  // Called with every node's position after the user moved one (drag) or
  // re-arranged the map (layout buttons); absent = nothing is saved.
  onPositionsChanged?: (positions: Record<string, MapPosition>) => void;
  // Bumped to forget the saved layout and lay the whole map out afresh.
  resetToken?: number;
}

export default function TopologyCanvas({ nodes, edges, statusById, trafficById, matchIds, layoutName = "force", onSelect, registerApi, dark, savedPositions, onPositionsChanged, resetToken = 0 }: Props) {
  const cyRef = useRef<cytoscape.Core | null>(null);
  const onSelectRef = useRef(onSelect);
  useEffect(() => {
    onSelectRef.current = onSelect;
  }, [onSelect]);
  const onChangedRef = useRef(onPositionsChanged);
  useEffect(() => {
    onChangedRef.current = onPositionsChanged;
  }, [onPositionsChanged]);
  const savedRef = useRef(savedPositions);
  useEffect(() => {
    savedRef.current = savedPositions;
  }, [savedPositions]);
  // Nodes that already have their place on the canvas (saved, laid out, or
  // dragged): a topology change never moves them, it only places new ones.
  const placed = useRef<Set<string>>(new Set());

  // Structural elements — only change when the node/edge SET changes, so live
  // health/traffic patches (below) never trigger a layout reshuffle.
  const elements = useMemo<cytoscape.ElementDefinition[]>(() => {
    const els: cytoscape.ElementDefinition[] = [];
    for (const n of nodes) {
      const type = n.type || "unknown";
      const bg = type === "internet" || type === "gateway" || type === "vpn" ? SYNTH[type] : nodeBg(n.status);
      els.push({ data: { id: n.id, label: n.label, type, bg } });
    }
    for (const e of edges) {
      els.push({
        data: {
          id: e.id, source: e.source, target: e.target,
          color: BRAND.grey, width: 1.8, idle: 1, status: e.status, linkType: e.link_type,
        },
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
  const layoutRef = useRef(layoutName);
  useEffect(() => {
    layoutRef.current = layoutName;
  }, [layoutName]);
  const emitPositions = (cy: cytoscape.Core) => {
    const cb = onChangedRef.current;
    if (!cb) return;
    const out: Record<string, MapPosition> = {};
    cy.nodes().forEach((n) => {
      const p = n.position();
      out[n.id()] = { x: Math.round(p.x * 10) / 10, y: Math.round(p.y * 10) / 10 };
    });
    cb(out);
  };
  // Lays the whole map out with the selected layout; after() runs once the
  // layout has settled.
  const runLayout = (cy: cytoscape.Core, after?: () => void) => {
    const go = (l: cytoscape.Layouts) => {
      l.one("layoutstop", () => {
        cy.nodes().forEach((n) => { placed.current.add(n.id()); });
        after?.();
      });
      l.run();
    };
    if (layoutRef.current === "hierarchy") {
      // Top-down tree from the Internet (or the best-connected node).
      let rootID = "internet";
      if (cy.$id("internet").empty()) {
        const best = cy.nodes().max((n) => n.degree(false)).ele;
        if (!best || best.empty()) return;
        rootID = best.id();
      }
      go(cy.layout({
        name: "breadthfirst", roots: [rootID], directed: false, spacingFactor: 1.1,
        animate: true, fit: true, padding: 44,
      }));
      return;
    }
    try {
      go(cy.layout({
        name: "cola",
        animate: true,
        maxSimulationTime: 1800,
        nodeSpacing: 26,
        edgeLength: 140,
        fit: true,
        padding: 44,
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
      } as any));
    } catch {
      go(cy.layout({ name: "cose", animate: true, fit: true, padding: 44 }));
    }
  };

  // Places what has no place yet: saved nodes go to their saved position,
  // nodes already on the canvas stay where they are, and new ones are laid
  // out around them (the placed nodes locked meanwhile). With nothing placed
  // at all it is the full layout.
  const placeNew = (cy: cytoscape.Core, fit: boolean) => {
    const saved = savedRef.current ?? {};
    const present = new Set(cy.nodes().map((n) => n.id()));
    for (const id of [...placed.current]) if (!present.has(id)) placed.current.delete(id);
    const known = cy.nodes().filter((n) => placed.current.has(n.id()) || saved[n.id()] !== undefined);
    if (known.empty()) {
      runLayout(cy);
      return;
    }
    cy.batch(() => {
      known.forEach((n) => {
        if (placed.current.has(n.id())) return;
        const p = saved[n.id()];
        n.position({ x: p.x, y: p.y });
        placed.current.add(n.id());
      });
    });
    const fresh = cy.nodes().difference(known);
    if (fresh.empty()) {
      if (fit) cy.fit(undefined, 44);
      return;
    }
    // Start each new node next to its placed neighbours, so the layout only
    // has to settle it locally.
    cy.batch(() => {
      fresh.forEach((n) => {
        const nb = n.neighborhood("node").filter((m) => known.contains(m));
        const c = nb.empty() ? cy.extent() : nb.boundingBox({});
        const cx = (c.x1 + c.x2) / 2, cy0 = (c.y1 + c.y2) / 2;
        n.position({ x: cx + (Math.random() - 0.5) * 80, y: cy0 + (Math.random() - 0.5) * 80 });
      });
    });
    known.lock();
    const l = cy.layout({
      name: "cola", animate: true, maxSimulationTime: 1200, nodeSpacing: 26, edgeLength: 140, fit, padding: 44,
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any);
    l.one("layoutstop", () => {
      known.unlock();
      fresh.forEach((n) => { placed.current.add(n.id()); });
    });
    l.run();
  };

  // First placement once the saved layout has loaded, then on every change
  // of the node/edge set (new nodes only).
  const initialised = useRef(false);
  useEffect(() => {
    const cy = cyRef.current;
    if (!cy || savedPositions === null) return;
    if (!initialised.current) {
      initialised.current = true;
      lastSig.current = sig;
      placeNew(cy, true);
      return;
    }
    if (lastSig.current === sig) return;
    lastSig.current = sig;
    placeNew(cy, false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sig, savedPositions]);

  // Re-arrange the whole map when the user picks a layout (not on mount):
  // that arrangement is the new layout, saved like a drag.
  const prevLayout = useRef(layoutName);
  useEffect(() => {
    if (prevLayout.current === layoutName) return;
    prevLayout.current = layoutName;
    const cy = cyRef.current;
    if (cy) runLayout(cy, () => emitPositions(cy));
  }, [layoutName]);

  // Reset: forget every place and lay the map out afresh (not saved).
  const prevReset = useRef(resetToken);
  useEffect(() => {
    if (prevReset.current === resetToken) return;
    prevReset.current = resetToken;
    const cy = cyRef.current;
    if (!cy) return;
    placed.current.clear();
    runLayout(cy);
  }, [resetToken]);

  // Live node status → colour (device nodes only; synthetic keep their hue).
  useEffect(() => {
    const cy = cyRef.current;
    if (!cy) return;
    cy.batch(() => {
      for (const n of nodes) {
        if (SYNTH_TYPES.has(n.type || "")) continue;
        const st = statusById.get(n.id) ?? n.status;
        const el = cy.$id(n.id);
        if (el.nonempty()) el.data("bg", nodeBg(st));
      }
    });
  }, [statusById, nodes]);

  // Live per-link throughput → edge width / colour / flow class / Mbps label.
  // Edges whose samples stop arriving are reset to idle so a dead link can't
  // keep a frozen "live" animation.
  useEffect(() => {
    const cy = cyRef.current;
    if (!cy) return;
    cy.batch(() => {
      cy.edges().forEach((e) => {
        const t = trafficById.get(e.id());
        if (!t) {
          if (!e.data("idle")) {
            e.data("width", 1.8);
            e.data("color", BRAND.grey);
            e.data("idle", 1);
            e.removeData("thruLabel");
            e.removeClass("active");
          }
          return;
        }
        const v = edgeVisual(t.rx, t.tx);
        e.data("width", v.width);
        e.data("color", v.color);
        e.removeData("idle");
        if (v.total >= 2e6) e.data("thruLabel", (t.estimated ? "≈ " : "") + fmtBps(v.total));
        else e.removeData("thruLabel");
        if (v.active) e.addClass("active");
        else e.removeClass("active");
      });
    });
  }, [trafficById]);

  // Filter dimming / highlight.
  useEffect(() => {
    const cy = cyRef.current;
    if (!cy) return;
    cy.batch(() => {
      if (!matchIds) {
        cy.elements().removeClass("dim");
        cy.nodes().removeClass("match");
        return;
      }
      cy.nodes().forEach((n) => {
        const m = matchIds.has(n.id());
        n.toggleClass("dim", !m);
        // Synthetic nodes are dim-exempt for context but are not "matches".
        n.toggleClass("match", m && !SYNTH_TYPES.has(n.data("type")));
      });
      cy.edges().forEach((e) => {
        const vis = matchIds.has(e.source().id()) && matchIds.has(e.target().id());
        e.toggleClass("dim", !vis);
      });
    });
  }, [matchIds]);

  // Imperative zoom/fit API for the page toolbar.
  const api = useMemo<CanvasApi>(() => ({
    fit: () => cyRef.current?.fit(undefined, 44),
    zoomBy: (factor: number) => {
      const cy = cyRef.current;
      if (!cy) return;
      cy.zoom({ level: cy.zoom() * factor, renderedPosition: { x: cy.width() / 2, y: cy.height() / 2 } });
    },
    fitTo: (ids: string[]) => {
      const cy = cyRef.current;
      if (!cy || ids.length === 0) return;
      const idset = new Set(ids);
      const col = cy.nodes().filter((n) => idset.has(n.id()));
      if (col.length) cy.animate({ fit: { eles: col, padding: 60 } }, { duration: 400 });
    },
  }), []);
  useEffect(() => {
    registerApi?.(api);
  }, [registerApi, api]);

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
        // A new instance (React's StrictMode remounts the component and the
        // wrapper builds a fresh one) has nothing placed yet.
        placed.current = new Set();
        initialised.current = false;
        cy.on("tap", "node", (evt) => onSelectRef.current(evt.target.id()));
        cy.on("tap", (evt) => {
          if (evt.target === cy) onSelectRef.current(null);
        });
        // A node the user let go of keeps its place; the whole arrangement
        // is reported so it can be saved.
        cy.on("dragfree", "node", (evt) => {
          placed.current.add(evt.target.id());
          emitPositions(cy);
        });
        // The first placement waits for the saved layout (effect above).
        if (savedRef.current !== null && !initialised.current) {
          initialised.current = true;
          lastSig.current = sig;
          placeNew(cy, true);
        }
      }}
    />
  );
}
