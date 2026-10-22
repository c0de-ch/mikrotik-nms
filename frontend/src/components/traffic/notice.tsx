import { AlertTriangle, Info, Loader2 } from "lucide-react";
import { cn } from "@/lib/utils";

// Notice: inline banner for empty / partial / error states in traffic views.
// "hold" is the device-load amber: the data shown is not moving (polling
// paused, or a stale reading). icon replaces the kind's default glyph (the
// class meter for a class-limited view, the pause glyph for a pause).
export function Notice({
  kind = "info",
  icon,
  children,
  className,
}: {
  kind?: "info" | "warn" | "error" | "loading" | "hold";
  icon?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  const Icon = kind === "loading" ? Loader2 : kind === "info" ? Info : AlertTriangle;
  return (
    <div
      role={kind === "error" ? "alert" : "status"}
      className={cn(
        "flex items-start gap-2 rounded-md border px-3 py-2 text-xs",
        kind === "error" && "border-red-500/30 bg-red-500/5 text-red-700 dark:text-red-400",
        kind === "warn" && "border-amber-500/30 bg-amber-500/5 text-amber-800 dark:text-amber-300",
        kind === "hold" && "border-load-hold/30 bg-load-hold/10 text-load-hold",
        (kind === "info" || kind === "loading") && "bg-muted/40 text-muted-foreground",
        className,
      )}
    >
      {icon ? (
        <span className="mt-px flex h-3.5 shrink-0 items-center">{icon}</span>
      ) : (
        <Icon className={cn("mt-px h-3.5 w-3.5 shrink-0", kind === "loading" && "animate-spin")} />
      )}
      <div className="min-w-0">{children}</div>
    </div>
  );
}

// EmptyState: centred placeholder inside a card body.
export function EmptyState({ icon: Icon, children }: { icon?: React.ComponentType<{ className?: string }>; children: React.ReactNode }) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 py-10 text-center text-sm text-muted-foreground">
      {Icon && <Icon className="h-7 w-7 opacity-60" />}
      <div className="max-w-md">{children}</div>
    </div>
  );
}
