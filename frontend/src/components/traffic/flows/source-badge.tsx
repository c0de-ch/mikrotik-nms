"use client";

import { GitFork, Radio, Split } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { coverageHint, coverageWarn, fmtPct, protocolLabel } from "./flow-lib";

export type SourceKind = "measured" | "derived" | "estimated";

// Phase 1 wording and style for the estimated (port counter) views.
const ESTIMATED_TITLE =
  "Derived from interface byte counters and the inferred L2 tree — not from flow export. Shared segments and hairpinned traffic can make it approximate.";

// SourceBadge says where the numbers on a view come from — measured flow
// export, a view derived from another device's flow export, or the Phase 1
// estimate — plus sampling and coverage when they apply. Tooltips repeat
// what the page also says in notices, so nothing depends on hover alone.
export function SourceBadge({
  kind,
  protocol,
  exporter,
  sampling,
  coverage,
  coverageLabel,
  note,
  className,
}: {
  kind: SourceKind;
  protocol?: string;
  exporter?: string;
  sampling?: number | null;
  coverage?: number | null;
  coverageLabel?: string;
  note?: string;
  className?: string;
}) {
  const from = `${protocolLabel(protocol)}${exporter ? ` from ${exporter}` : ""}`;
  return (
    <span className={cn("inline-flex flex-wrap items-center gap-1.5", className)}>
      {kind === "estimated" ? (
        <Badge
          variant="outline"
          className="gap-1 border-amber-500/40 font-normal text-amber-800 dark:text-amber-300"
          title={ESTIMATED_TITLE}
        >
          <GitFork className="h-3 w-3" />
          Estimated from port counters + topology
        </Badge>
      ) : kind === "derived" ? (
        <Badge
          variant="outline"
          className="max-w-full gap-1 border-sky-500/40 font-normal text-sky-800 dark:text-sky-300"
          title={note || "A view of this port derived from another device's flow export."}
        >
          <Split className="h-3 w-3" />
          <span className="truncate">Derived · {from}</span>
        </Badge>
      ) : (
        <Badge
          variant="outline"
          className="max-w-full gap-1 border-emerald-500/40 font-normal text-emerald-800 dark:text-emerald-300"
          title="Per-conversation bytes exported by the device itself (flow export) — measured, not estimated."
        >
          <Radio className="h-3 w-3" />
          <span className="truncate">Measured · {from}</span>
        </Badge>
      )}
      {sampling != null && sampling > 1 && <SampledBadge rate={sampling} />}
      {coverage != null && <CoverageBadge value={coverage} label={coverageLabel} />}
    </span>
  );
}

export function SampledBadge({ rate }: { rate: number }) {
  return (
    <Badge
      variant="outline"
      className="font-normal text-muted-foreground"
      title={`The exporter samples 1 in ${rate} packets; bytes and packets are multiplied by ${rate}, so small flows can be missing or over-counted.`}
    >
      Sampled 1:{rate} — values scaled
    </Badge>
  );
}

// CoverageBadge: amber when flows explain too little of the port's bytes,
// or clearly more than the counters saw (the view over-counts, or counter
// samples are missing) — neither is a number to take at face value.
export function CoverageBadge({ value, label }: { value: number; label?: string }) {
  const warn = coverageWarn(value);
  return (
    <Badge
      variant="outline"
      title={coverageHint(value)}
      className={cn(
        "font-normal tabular-nums",
        warn ? "border-amber-500/40 text-amber-800 dark:text-amber-300" : "text-muted-foreground",
      )}
    >
      {label ? `${label} ` : ""}Coverage {fmtPct(value)}
    </Badge>
  );
}
