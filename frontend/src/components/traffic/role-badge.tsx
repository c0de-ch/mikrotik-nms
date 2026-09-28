import { Badge } from "@/components/ui/badge";
import type { PortRoleName } from "@/lib/api";
import { cn } from "@/lib/utils";
import { ROLE_META } from "./lib";

// RoleBadge: outline badge with a coloured dot; the text stays in the text
// colour so the role never relies on hue alone.
export function RoleBadge({ role, className }: { role: PortRoleName; className?: string }) {
  const meta = ROLE_META[role] ?? ROLE_META.virtual;
  return (
    <Badge
      variant="outline"
      title={meta.hint}
      className={cn("h-5 gap-1 px-1.5 text-[11px] font-normal text-muted-foreground", role === "idle" && "border-dashed", className)}
    >
      <span className="h-1.5 w-1.5 rounded-full" style={{ background: meta.color }} />
      {meta.label}
    </Badge>
  );
}
