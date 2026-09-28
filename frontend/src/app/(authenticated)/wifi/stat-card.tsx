import type { LucideIcon } from "lucide-react";
import { ChevronRight } from "lucide-react";
import { cn } from "@/lib/utils";

// StatCard is a clickable KPI tile: big number, short hint, and a
// "details" affordance that opens the matching drill-down panel.
export function StatCard({
  label,
  value,
  hint,
  icon: Icon,
  accent,
  active,
  onClick,
}: {
  label: string;
  value: number | string;
  hint?: React.ReactNode;
  icon: LucideIcon;
  accent: string; // tailwind classes for the icon chip, e.g. "bg-blue-500/10 text-blue-600"
  active?: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-haspopup="dialog"
      className={cn(
        "group flex flex-col gap-3 rounded-xl bg-card p-4 text-left text-card-foreground ring-1 ring-foreground/10 transition-all",
        "hover:-translate-y-0.5 hover:shadow-md hover:ring-primary/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary",
        active && "ring-2 ring-primary"
      )}
    >
      <div className="flex items-center justify-between">
        <span className="text-sm font-medium text-muted-foreground">{label}</span>
        <span className={cn("flex h-8 w-8 items-center justify-center rounded-lg", accent)}>
          <Icon className="h-4 w-4" />
        </span>
      </div>
      <div className="text-3xl font-bold tabular-nums">{value}</div>
      <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
        <span className="min-w-0 truncate">{hint}</span>
        <span className="flex shrink-0 items-center gap-0.5 text-primary transition-opacity md:opacity-0 md:group-hover:opacity-100 md:group-focus-visible:opacity-100">
          Details <ChevronRight className="h-3 w-3" />
        </span>
      </div>
    </button>
  );
}
