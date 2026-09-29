"use client";

import { useMemo } from "react";
import type { FlowPoint } from "@/lib/api";
import { orderPoints } from "./flow-lib";

function optionLabel(p: FlowPoint): string {
  let s = p.name;
  if (p.kind === "derived") s += " · derived";
  if (p.sampling_rate > 1) s += ` · sampled 1:${p.sampling_rate}`;
  if (!p.enabled) s += " · disabled";
  return s;
}

// PointPicker: one observation point at a time, grouped by exporter — the
// rows of two exporters are never added together, so there is no "all"
// option. Disabled points are hidden unless selected.
export function PointPicker({
  points,
  value,
  onChange,
  className,
}: {
  points: FlowPoint[];
  value: number | null;
  onChange: (id: number) => void;
  className?: string;
}) {
  const groups = useMemo(() => {
    const byExporter = new Map<number, { name: string; points: FlowPoint[] }>();
    for (const p of points) {
      if (!p.enabled && p.id !== value) continue;
      const g = byExporter.get(p.exporter_id);
      if (g) g.points.push(p);
      else byExporter.set(p.exporter_id, { name: p.exporter_name, points: [p] });
    }
    return [...byExporter.entries()]
      .map(([id, g]) => ({ id, name: g.name, points: orderPoints(g.points) }))
      .sort((a, b) => a.name.localeCompare(b.name));
  }, [points, value]);

  return (
    <select
      aria-label="Observation point"
      title="Where the flows were measured: one exporter's interface(s), or a port view derived from them"
      className={`h-8 min-w-0 max-w-full rounded-md border bg-background px-2 text-sm sm:max-w-[320px] ${className ?? ""}`}
      value={value ?? ""}
      onChange={(e) => {
        const id = Number(e.target.value);
        if (id > 0) onChange(id);
      }}
    >
      {value === null && <option value="">Choose a view…</option>}
      {groups.map((g) => (
        <optgroup key={g.id} label={g.name}>
          {g.points.map((p) => (
            <option key={p.id} value={p.id}>
              {optionLabel(p)}
            </option>
          ))}
        </optgroup>
      ))}
    </select>
  );
}
