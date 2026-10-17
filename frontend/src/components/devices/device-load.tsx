"use client";

import { Pause } from "lucide-react";
import type { Device } from "@/lib/api";
import { useNow } from "@/hooks/use-now";
import {
  CLASSES,
  CLASS_ORDER,
  REASON_SHORT,
  budgetClass,
  budgetReason,
  describeBudget,
  fmtRemaining,
  fmtUntil,
  isPollingPaused,
  pausedUntilMs,
  type BudgetClass,
} from "@/lib/device-load";
import { cn } from "@/lib/utils";

// The device-load marks — one visual language wherever a device is named:
//
//   ClassMeter  three equal bars, filled up to the class the NMS polls the
//               device as (C1 one bar … C3 all three). Monochrome on
//               purpose: a weak device is a fact, not a fault. An outlined
//               bar is assumed rather than known (an unprobed device is
//               budgeted as C1 until its first resource read).
//   PausedBadge the one loud mark: amber ("hold") with a solid pause glyph,
//               because a paused device's numbers stop moving.
//
// Colours come from the --load-* tokens in globals.css (light and dark).

type LoadDevice = Pick<Device, "effective_class" | "poll_profile" | "device_class" | "class_override">;
type PauseDevice = Pick<Device, "polling_paused" | "polling_paused_until">;

// ClassMeter is sized in em, so it follows the text it sits next to.
export function ClassMeter({ cls, assumed, className }: { cls: BudgetClass; assumed?: boolean; className?: string }) {
  const rank = CLASSES[cls].rank;
  return (
    <span aria-hidden className={cn("inline-flex shrink-0 items-center gap-[0.14em]", className)}>
      {CLASS_ORDER.map((c, i) => (
        <span
          key={c}
          className={cn(
            "h-[0.82em] w-[0.34em] rounded-[1px]",
            i >= rank ? "bg-load-track" : assumed ? "border border-load-meter" : "bg-load-meter",
          )}
        />
      ))}
    </span>
  );
}

// PauseGlyph: two solid bars — the pause sign, drawn like the meter's bars.
export function PauseGlyph({ className }: { className?: string }) {
  return <Pause aria-hidden className={cn("h-3 w-3 shrink-0", className)} fill="currentColor" strokeWidth={0} />;
}

// DeviceClassBadge: meter + class id ("C1") and, when the budget is not
// simply the probed class, why ("light", "override", "unprobed") — else,
// with detail="full", the class word ("C1 weak"). A capable device on the
// light profile reads "C1 light", never "C1 weak". With onClick it is a
// filter button.
export function DeviceClassBadge({
  device,
  detail = "compact",
  onClick,
  className,
}: {
  device: LoadDevice;
  detail?: "compact" | "full";
  onClick?: () => void;
  className?: string;
}) {
  const cls = budgetClass(device);
  const info = CLASSES[cls];
  const reason = budgetReason(device);
  const description = describeBudget(device);
  const base = cn("inline-flex items-center gap-1 whitespace-nowrap text-xs leading-none text-foreground/85", className);
  const body = (
    <>
      <ClassMeter cls={cls} assumed={reason === "unprobed"} />
      <span className="font-medium tabular-nums">{cls}</span>
      {reason ? (
        <span className="text-muted-foreground">{REASON_SHORT[reason]}</span>
      ) : detail === "full" ? (
        <span>{info.word}</span>
      ) : (
        <span className="sr-only">{info.word}</span>
      )}
    </>
  );
  if (onClick) {
    return (
      <button
        type="button"
        title={`${description} Select to show only ${cls} devices.`}
        aria-label={`${cls} ${info.word}${reason ? `, ${REASON_SHORT[reason]}` : ""}: show only ${cls} devices`}
        onClick={(e) => {
          e.stopPropagation();
          onClick();
        }}
        className={cn(base, "-mx-1 rounded-sm px-1 py-0.5 hover:bg-muted hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring")}
      >
        {body}
      </button>
    );
  }
  return (
    <span title={description} className={base}>
      {body}
    </span>
  );
}

// PausedBadge renders nothing unless an admin pause is in force; it hides
// itself when the pause runs out (minute clock). detail="full" says until
// when.
export function PausedBadge({
  device,
  detail = "compact",
  onClick,
  className,
}: {
  device: PauseDevice;
  detail?: "compact" | "full";
  onClick?: () => void;
  className?: string;
}) {
  const now = useNow(60_000);
  if (!isPollingPaused(device, now)) return null;
  const until = pausedUntilMs(device) ?? 0;
  const untilText = fmtUntil(until, now);
  const left = now ? fmtRemaining(until - now) : "";
  const description = `NMS polling paused until ${untilText}${left ? ` (${left} left)` : ""}. Only the liveness check runs; rates keep their last reading.`;
  const label = detail === "full" ? `Paused until ${untilText}` : "Paused";
  const base = cn(
    "inline-flex h-5 items-center gap-1 whitespace-nowrap rounded-full bg-load-hold/10 px-1.5 text-[11px] font-medium leading-none text-load-hold",
    className,
  );
  if (onClick) {
    return (
      <button
        type="button"
        title={`${description} Select to show only paused devices.`}
        aria-label={`Polling paused until ${untilText}: show only paused devices`}
        onClick={(e) => {
          e.stopPropagation();
          onClick();
        }}
        className={cn(base, "hover:bg-load-hold/15 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring")}
      >
        <PauseGlyph />
        {label}
      </button>
    );
  }
  return (
    <span title={description} className={base}>
      <PauseGlyph />
      {label}
      {detail === "compact" && <span className="sr-only"> until {untilText}</span>}
    </span>
  );
}

// DeviceLoadBadges: the class badge and, while paused, the paused badge — in
// that order, after the device's status, wherever a device is named.
export function DeviceLoadBadges({
  device,
  detail = "compact",
  className,
}: {
  device: LoadDevice & PauseDevice;
  detail?: "compact" | "full";
  className?: string;
}) {
  return (
    <span className={cn("inline-flex flex-wrap items-center gap-x-2 gap-y-1", className)}>
      <DeviceClassBadge device={device} detail={detail} />
      <PausedBadge device={device} detail={detail} />
    </span>
  );
}
