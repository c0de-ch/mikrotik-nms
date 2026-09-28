"use client";

import { cn } from "@/lib/utils";

export interface SegmentOption<T extends string> {
  value: T;
  label: React.ReactNode;
  title?: string;
  disabled?: boolean;
}

// Segmented is the compact button-group toggle used across the traffic views
// (same look as the map's role filter).
export function Segmented<T extends string>({
  value,
  options,
  onChange,
  ariaLabel,
  className,
}: {
  value: T;
  options: SegmentOption<T>[];
  onChange: (v: T) => void;
  ariaLabel: string;
  className?: string;
}) {
  return (
    <div role="radiogroup" aria-label={ariaLabel} className={cn("inline-flex max-w-full overflow-x-auto rounded-md border text-sm", className)}>
      {options.map((o, i) => {
        const active = o.value === value;
        return (
          <button
            key={o.value}
            type="button"
            role="radio"
            aria-checked={active}
            title={o.title}
            disabled={o.disabled}
            onClick={() => onChange(o.value)}
            className={cn(
              "inline-flex h-8 shrink-0 items-center gap-1 px-2.5 whitespace-nowrap transition-colors disabled:cursor-not-allowed disabled:opacity-40",
              i > 0 && "border-l",
              active ? "bg-foreground text-background" : "hover:bg-muted",
            )}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}

// Toggle is a single on/off pill button (e.g. "Physical ports only").
export function Toggle({
  pressed,
  onChange,
  children,
  title,
}: {
  pressed: boolean;
  onChange: (v: boolean) => void;
  children: React.ReactNode;
  title?: string;
}) {
  return (
    <button
      type="button"
      aria-pressed={pressed}
      title={title}
      onClick={() => onChange(!pressed)}
      className={cn(
        "inline-flex h-8 shrink-0 items-center gap-1 rounded-md border px-2.5 text-sm whitespace-nowrap transition-colors",
        pressed ? "bg-foreground text-background" : "hover:bg-muted",
      )}
    >
      {children}
    </button>
  );
}
