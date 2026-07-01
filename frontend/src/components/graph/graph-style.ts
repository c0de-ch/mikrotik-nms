// Visual helpers for the network map (cytoscape). Colours are picked to read on
// both the light and dark canvas background; brand hues match globals.css.

export const BRAND = {
  primary: "#5A9CB5", // online / low load
  amber: "#E8A13A", //  medium load
  red: "#FA6868", //    offline / saturated
  grey: "#94a3b8", //   unknown / idle
};

// Synthetic (non-device) node colours — distinct from both the status triad
// and the load ramp so they can't be misread as device state.
export const SYNTH = {
  internet: "#8b5cf6", // violet
  gateway: "#0d9488", //  teal — unmanaged firewalls/gateways (OPNsense etc.)
  vpn: "#c026d3", //      fuchsia — tunnel interfaces
};

export const SYNTH_TYPES = new Set(["internet", "gateway", "vpn"]);

export function fmtBps(n: number): string {
  if (!n || n < 0) return "0";
  if (n >= 1e9) return `${(n / 1e9).toFixed(2)} Gbps`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)} Mbps`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(0)} Kbps`;
  return `${n} bps`;
}

export function nodeBg(status: string): string {
  if (status === "online") return BRAND.primary;
  if (status === "offline") return BRAND.red;
  return BRAND.grey;
}

// Colour for a switch-port cell by throughput.
export function portLoadColor(bps: number, dark: boolean): string {
  const mbps = (bps || 0) / 1e6;
  if (mbps < 0.05) return dark ? "#334155" : "#e2e8f0"; // idle
  if (mbps < 20) return BRAND.primary;
  if (mbps < 200) return BRAND.amber;
  return BRAND.red;
}

// Per-edge visual encoding from live throughput (both directions summed).
export function edgeVisual(rxBps: number, txBps: number) {
  const total = Math.max(0, (rxBps || 0) + (txBps || 0));
  // width: 1.5px idle → ~9px at 1Gbps, log-scaled on Mbps.
  const mbps = total / 1e6;
  const width = Math.min(9, 1.6 + Math.log10(1 + mbps) * 2.4);
  let color = BRAND.grey;
  if (mbps >= 500) color = BRAND.red;
  else if (mbps >= 100) color = BRAND.amber;
  else if (mbps >= 1) color = BRAND.primary;
  const active = mbps >= 0.5; // animate the dashes above 0.5 Mbps
  return { total, width: Math.round(width * 10) / 10, color, active };
}

// Loose stylesheet block — the cytoscape css union types vary across @types
// versions, so we keep this permissive and cast at the component boundary.
export type GraphStyle = { selector: string; style: Record<string, string | number | number[]> };

// Cytoscape stylesheet. Node/edge visuals are data-bound so we can patch them
// live (device.health, topology.traffic) without re-styling the whole graph.
export function buildStylesheet(dark: boolean): GraphStyle[] {
  const label = dark ? "#e5e7eb" : "#1f2937";
  const sub = dark ? "#94a3b8" : "#64748b";
  const idleEdge = dark ? "#3a4658" : "#cbd5e1";
  return [
    {
      selector: "node",
      style: {
        "background-color": "data(bg)",
        "border-width": 2,
        "border-color": dark ? "#1e293b" : "#ffffff",
        label: "data(label)",
        color: label,
        "font-size": 10,
        "font-weight": 600,
        "text-valign": "bottom",
        "text-margin-y": 4,
        "text-wrap": "wrap",
        "text-max-width": "120px",
        width: 34,
        height: 34,
        "overlay-opacity": 0,
      },
    },
    { selector: 'node[type="router"]', style: { shape: "round-hexagon", width: 44, height: 40 } },
    { selector: 'node[type="switch"]', style: { shape: "round-rectangle", width: 46, height: 30 } },
    { selector: 'node[type="ap"]', style: { shape: "ellipse", width: 32, height: 32 } },
    { selector: 'node[type="unknown"]', style: { shape: "diamond" } },
    // Synthetic egress nodes: Internet cloud-ish octagon, gateways (unmanaged
    // firewalls) as diamonds, VPN tunnels as small rhomboids.
    { selector: 'node[type="internet"]', style: { shape: "octagon", width: 54, height: 54, "font-size": 12, "font-weight": 700 } },
    { selector: 'node[type="gateway"]', style: { shape: "round-diamond", width: 42, height: 42 } },
    { selector: 'node[type="vpn"]', style: { shape: "rhomboid", width: 34, height: 22, "font-size": 9 } },
    {
      selector: "node.badge",
      style: {
        "font-size": 9,
        color: sub,
      },
    },
    {
      selector: "node:selected",
      style: { "border-width": 4, "border-color": BRAND.amber },
    },
    {
      selector: "edge",
      style: {
        "curve-style": "bezier",
        "line-color": "data(color)",
        width: "data(width)",
        opacity: 0.9,
        "overlay-opacity": 0,
      },
    },
    {
      selector: "edge[?idle]",
      style: { "line-color": idleEdge, opacity: 0.55 },
    },
    {
      selector: "edge.active",
      style: { "line-style": "dashed", "line-dash-pattern": [7, 5] },
    },
    {
      selector: 'edge[status="down"]',
      style: { "line-color": idleEdge, "line-style": "dotted", opacity: 0.35, width: 1 },
    },
    // Egress edges: dashed so they read as logical paths, not cables. Measured
    // ones still get width/colour from live traffic via data(color)/data(width).
    { selector: 'edge[linkType="gateway"]', style: { "line-style": "dashed", "line-dash-pattern": [3, 4], opacity: 0.6 } },
    { selector: 'edge[linkType="internet"]', style: { "line-style": "dashed", "line-dash-pattern": [3, 4], opacity: 0.75 } },
    { selector: 'edge[linkType="vpn"]', style: { "line-style": "dashed", "line-dash-pattern": [2, 3], opacity: 0.75 } },
    // Live-throughput label on busy edges.
    {
      selector: "edge[thruLabel]",
      style: {
        label: "data(thruLabel)",
        "font-size": 8,
        color: sub,
        "text-rotation": "autorotate",
        "text-background-color": dark ? "#0f172a" : "#ffffff",
        "text-background-opacity": 0.75,
        "text-background-padding": "1px",
      },
    },
    {
      selector: "edge:selected",
      style: { "line-color": BRAND.amber, opacity: 1 },
    },
    // Filter dimming / highlight.
    { selector: "node.dim", style: { opacity: 0.12 } },
    { selector: "edge.dim", style: { opacity: 0.05 } },
    { selector: "node.match", style: { "border-color": BRAND.amber, "border-width": 3 } },
  ];
}
