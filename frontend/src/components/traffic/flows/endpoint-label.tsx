"use client";

import { ArrowRight } from "lucide-react";
import type { FlowApp, FlowEndpoint, FlowEndpointClass } from "@/lib/api";
import { cn } from "@/lib/utils";
import { CLASS_META } from "./flow-lib";

// ClassChip: outline chip with a coloured dot; the text stays in the text
// colour so the class never relies on hue alone (same as RoleBadge).
export function ClassChip({ cls, className }: { cls: FlowEndpointClass; className?: string }) {
  const meta = CLASS_META[cls] ?? CLASS_META.external;
  return (
    <span
      title={meta.hint}
      className={cn(
        "inline-flex h-4 shrink-0 items-center gap-1 rounded-full border px-1.5 text-[10px] leading-none text-muted-foreground",
        className,
      )}
    >
      <span className="h-1.5 w-1.5 rounded-full" style={{ background: meta.color }} />
      {meta.label}
    </span>
  );
}

// EndpointLabel: name (bold; the IP when unnamed) + IP + class chip, the
// Phase 1 attachment ("via switch · port", a link to that port) and a
// "NAT for <exporter>" chip for another exporter's NAT address.
export function EndpointLabel({
  ep,
  onOpenPort,
  onNat,
}: {
  ep: FlowEndpoint | null;
  onOpenPort?: (deviceId: string, iface: string) => void;
  onNat?: (exporterName: string) => void;
}) {
  if (!ep) return <span className="text-muted-foreground">—</span>;
  const named = !!ep.name && ep.name !== ep.ip;
  const detail = [ep.mac, ep.vendor].filter(Boolean).join(" · ");
  const natTitle = ep.nat_of
    ? `A NAT address of ${ep.nat_of}: traffic of the hosts behind it appears here as this one address. Per-host detail is in ${ep.nat_of}'s own view.`
    : "";
  return (
    <div className="min-w-0">
      <div className="flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-0.5">
        <span className={cn("min-w-0 truncate font-medium", !named && "font-mono text-[13px]")} title={detail || ep.ip}>
          {named ? ep.name : ep.ip}
        </span>
        {named && <span className="min-w-0 truncate font-mono text-[11px] text-muted-foreground">{ep.ip}</span>}
        <ClassChip cls={ep.class} />
        {ep.nat_of &&
          (onNat ? (
            <button
              type="button"
              title={natTitle}
              onClick={() => onNat(ep.nat_of!)}
              className="inline-flex h-4 shrink-0 items-center rounded-full border border-dashed px-1.5 text-[10px] leading-none text-muted-foreground hover:bg-muted hover:text-foreground"
            >
              NAT for {ep.nat_of}
            </button>
          ) : (
            <span
              title={natTitle}
              className="inline-flex h-4 shrink-0 items-center rounded-full border border-dashed px-1.5 text-[10px] leading-none text-muted-foreground"
            >
              NAT for {ep.nat_of}
            </span>
          ))}
      </div>
      {ep.attached && (
        // Flex row: a <button> renders inline-block, so a plain truncating
        // line would swallow the whole port name into "via …" on phones;
        // here the port name itself shrinks and ellipsizes.
        <div
          className="flex min-w-0 text-[11px] text-muted-foreground"
          title={`via ${ep.attached.device_name} · ${ep.attached.iface}`}
        >
          <span className="shrink-0">via&nbsp;</span>
          {onOpenPort ? (
            <button
              type="button"
              className="min-w-0 truncate text-left hover:text-foreground hover:underline"
              onClick={() => onOpenPort(ep.attached!.device_id, ep.attached!.iface)}
            >
              {ep.attached.device_name} · <span className="font-mono">{ep.attached.iface}</span>
            </button>
          ) : (
            <span className="min-w-0 truncate">
              {ep.attached.device_name} · <span className="font-mono">{ep.attached.iface}</span>
            </span>
          )}
        </div>
      )}
    </div>
  );
}

// AppLabel: "HTTPS tcp/443", or just "tcp/51234" / "icmp" when unknown.
export function AppLabel({ app }: { app: FlowApp | null }) {
  if (!app) return <span className="text-muted-foreground">—</span>;
  const proto = app.port ? `${app.proto_name}/${app.port}` : app.proto_name;
  return (
    <span className="inline-flex min-w-0 items-baseline gap-1.5">
      {app.label && <span className="truncate font-medium">{app.label}</span>}
      <span className={cn("shrink-0 font-mono", app.label ? "text-[11px] text-muted-foreground" : "text-[13px]")}>{proto}</span>
    </span>
  );
}

// ConversationLabel: src → dst (+ app), stacked so it fits a phone; wide
// (full-width cards) puts src and dst side by side from the sm breakpoint.
export function ConversationLabel({
  src,
  dst,
  app,
  wide,
  onOpenPort,
  onNat,
}: {
  src: FlowEndpoint | null;
  dst: FlowEndpoint | null;
  app?: FlowApp | null;
  wide?: boolean;
  onOpenPort?: (deviceId: string, iface: string) => void;
  onNat?: (exporterName: string) => void;
}) {
  return (
    <div className="min-w-0 space-y-0.5">
      <div className={cn("min-w-0 space-y-0.5", wide && "sm:grid sm:grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] sm:gap-x-2 sm:space-y-0")}>
        <EndpointLabel ep={src} onOpenPort={onOpenPort} onNat={onNat} />
        <div className={cn("flex min-w-0 items-start gap-1", wide && "sm:contents")}>
          <ArrowRight className="mt-[3px] h-3 w-3 shrink-0 text-muted-foreground" aria-label="to" />
          <div className="min-w-0 flex-1">
            <EndpointLabel ep={dst} onOpenPort={onOpenPort} onNat={onNat} />
          </div>
        </div>
      </div>
      {app !== undefined && (
        <div className="text-xs">
          <AppLabel app={app} />
        </div>
      )}
    </div>
  );
}
