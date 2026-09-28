"use client";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { BRAND, fmtBps, portLoadColor, portLoadTextColor } from "@/components/graph/graph-style";
import type { PortRoleName } from "@/lib/api";
import { cn } from "@/lib/utils";
import { ROLE_META, abbrevPort, splitDirection } from "./lib";

export interface HeatmapPort {
  name: string;
  type?: string;
  running: boolean;
  disabled: boolean;
  comment?: string;
  rx_bps: number;
  tx_bps: number;
  role?: string;
  behind?: string;
}

// PortHeatmap renders a device's ports as a grid of cells coloured by live
// load (portLoadColor on rx+tx). Shared by the map's device sheet and the
// traffic device view. With onSelect the cells become buttons.
export function PortHeatmap({
  ports,
  dark,
  onSelect,
  selected,
}: {
  ports: HeatmapPort[];
  dark: boolean;
  onSelect?: (iface: string) => void;
  selected?: string;
}) {
  if (ports.length === 0) return <p className="text-xs text-muted-foreground">No physical ports.</p>;
  return (
    <div className="grid gap-1.5" style={{ gridTemplateColumns: "repeat(auto-fill, minmax(3rem, 1fr))" }}>
      {ports.map((p) => (
        <HeatCell key={p.name} port={p} dark={dark} onSelect={onSelect} selected={selected === p.name} />
      ))}
    </div>
  );
}

function HeatCell({
  port: p,
  dark,
  onSelect,
  selected,
}: {
  port: HeatmapPort;
  dark: boolean;
  onSelect?: (iface: string) => void;
  selected: boolean;
}) {
  const down = p.disabled || !p.running;
  const total = p.rx_bps + p.tx_bps;
  const cellClass = cn(
    "w-full rounded-sm border px-1 py-1 text-center font-mono text-[9px] overflow-hidden whitespace-nowrap text-ellipsis",
    onSelect && "cursor-pointer transition-shadow hover:ring-2 hover:ring-foreground/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
    selected && "ring-2 ring-foreground",
  );
  const style: React.CSSProperties = {
    background: down ? "transparent" : portLoadColor(total, dark),
    color: down ? "var(--muted-foreground)" : portLoadTextColor(total, dark),
    opacity: down ? 0.5 : 1,
    borderStyle: p.disabled ? "dashed" : "solid",
  };
  const label = abbrevPort(p.name);
  const trigger = onSelect ? (
    <button type="button" className={cellClass} style={style} onClick={() => onSelect(p.name)} aria-label={`${p.name}: open port detail`}>
      {label}
    </button>
  ) : (
    <div className={cellClass} style={style}>
      {label}
    </div>
  );
  return (
    <Tooltip>
      <TooltipTrigger render={trigger} />
      <TooltipContent className="flex-col items-start gap-0.5 font-normal">
        <HeatTooltip port={p} down={down} />
      </TooltipContent>
    </Tooltip>
  );
}

function HeatTooltip({ port: p, down }: { port: HeatmapPort; down: boolean }) {
  const role = p.role && p.role in ROLE_META ? (p.role as PortRoleName) : undefined;
  let rates: string;
  if (down) {
    rates = p.disabled ? "disabled" : "down";
  } else {
    // Without a role (the map) there is no download/upload reading: raw
    // in/out, so the tooltip never contradicts /traffic's role-mapped ↓/↑.
    const s = splitDirection(role, p.rx_bps, p.tx_bps);
    rates = s.directional
      ? `↓ ${fmtBps(s.down)}  ↑ ${fmtBps(s.up)}`
      : `in ${fmtBps(p.rx_bps)}  out ${fmtBps(p.tx_bps)}`;
  }
  return (
    <>
      <span className="font-mono font-medium">
        {p.name}
        {p.comment ? <span className="font-sans font-normal opacity-70"> ({p.comment})</span> : null}
      </span>
      {role && (
        <span className="inline-flex items-center gap-1 opacity-80">
          <span className="h-1.5 w-1.5 rounded-full" style={{ background: ROLE_META[role]?.color ?? BRAND.grey }} />
          {ROLE_META[role].label}
          {p.behind ? ` · ${p.behind}` : ""}
        </span>
      )}
      <span className="font-mono tabular-nums">{rates}</span>
    </>
  );
}

// Legend for the heatmap's load buckets (thresholds match portLoadColor).
export function HeatmapLegend({ dark }: { dark: boolean }) {
  const items: [string, string][] = [
    [portLoadColor(0, dark), "idle"],
    [portLoadColor(1e6, dark), "< 20 Mbps"],
    [portLoadColor(50e6, dark), "< 200 Mbps"],
    [portLoadColor(500e6, dark), "≥ 200 Mbps"],
  ];
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
      {items.map(([c, l]) => (
        <span key={l} className="inline-flex items-center gap-1">
          <span className="h-2.5 w-2.5 rounded-sm border" style={{ background: c }} />
          {l}
        </span>
      ))}
      <span className="inline-flex items-center gap-1">
        <span className="h-2.5 w-2.5 rounded-sm border border-dashed opacity-50" />
        down / disabled
      </span>
    </div>
  );
}
