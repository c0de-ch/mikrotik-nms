import { RX_COLOR, TX_COLOR } from "./lib";

// RateBar is the stacked download/upload bar used in ranked lists. Both
// segments are scaled against the list maximum so bars compare across rows;
// a 2px gap separates the segments.
export function RateBar({ down, up, max, title }: { down: number; up: number; max: number; title?: string }) {
  const scale = max > 0 ? 100 / max : 0;
  const dw = Math.min(100, down * scale);
  const uw = Math.min(100 - dw, up * scale);
  return (
    <div className="flex h-2 w-full gap-[2px] overflow-hidden rounded-full bg-muted" title={title}>
      {dw > 0 && <div className="h-full rounded-l-full" style={{ width: `max(${dw}%, 2px)`, background: RX_COLOR }} />}
      {uw > 0 && <div className="h-full rounded-r-full" style={{ width: `max(${uw}%, 2px)`, background: TX_COLOR }} />}
    </div>
  );
}

// Swatch + label pair for legends (text keeps the text colour).
export function DirKey({ color, children }: { color: string; children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="h-2 w-3 rounded-sm" style={{ background: color }} />
      {children}
    </span>
  );
}
