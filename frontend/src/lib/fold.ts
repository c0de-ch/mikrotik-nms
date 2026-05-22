// Time-bucket grouping helpers used by event tables.
//
// Each event has a timestamp string. We compute a bucket key like
// "2026-05-22 14:00" (for hour) or "2026-05" (for month). Events with the
// same key end up in the same group; groups are emitted newest-first.

export type FoldBucket = "off" | "hour" | "day" | "week" | "month";

export const foldOptions: { value: FoldBucket; label: string }[] = [
  { value: "off", label: "no grouping" },
  { value: "hour", label: "by hour" },
  { value: "day", label: "by day" },
  { value: "week", label: "by week" },
  { value: "month", label: "by month" },
];

function pad2(n: number) {
  return String(n).padStart(2, "0");
}

// isoWeekKey returns a stable identifier for the ISO week containing `d`
// (e.g. "2026-W21"). Defined inline because the JS Date API doesn't expose it.
function isoWeekKey(d: Date): string {
  const t = new Date(Date.UTC(d.getFullYear(), d.getMonth(), d.getDate()));
  const day = t.getUTCDay() || 7; // Mon=1 .. Sun=7
  t.setUTCDate(t.getUTCDate() + 4 - day);
  const yearStart = new Date(Date.UTC(t.getUTCFullYear(), 0, 1));
  const week = Math.ceil(((t.getTime() - yearStart.getTime()) / 86400000 + 1) / 7);
  return `${t.getUTCFullYear()}-W${pad2(week)}`;
}

export function bucketKey(timestamp: string, bucket: FoldBucket): string {
  if (bucket === "off") return timestamp;
  const d = new Date(timestamp);
  if (isNaN(d.getTime())) return timestamp;
  const y = d.getFullYear();
  const mo = pad2(d.getMonth() + 1);
  const da = pad2(d.getDate());
  const h = pad2(d.getHours());
  switch (bucket) {
    case "hour":  return `${y}-${mo}-${da} ${h}:00`;
    case "day":   return `${y}-${mo}-${da}`;
    case "week":  return isoWeekKey(d);
    case "month": return `${y}-${mo}`;
  }
}

export interface FoldedGroup<T> {
  key: string;
  count: number;
  items: T[];
  // Newest event's timestamp in the group — useful for header rendering.
  latest: string;
}

// foldEvents groups input events by bucket. Items inside each group keep
// their original order (caller should sort newest-first beforehand).
// Groups are returned newest-first as well.
export function foldEvents<T extends { recorded_at?: string; collected_at?: string }>(
  events: T[],
  bucket: FoldBucket,
  timestampField: "recorded_at" | "collected_at" = "recorded_at",
): FoldedGroup<T>[] {
  if (bucket === "off") {
    return events.map((e) => ({
      key: (e[timestampField] as string) || "",
      count: 1,
      items: [e],
      latest: (e[timestampField] as string) || "",
    }));
  }
  const byKey = new Map<string, FoldedGroup<T>>();
  for (const ev of events) {
    const ts = (ev[timestampField] as string) || "";
    const k = bucketKey(ts, bucket);
    const existing = byKey.get(k);
    if (existing) {
      existing.items.push(ev);
      existing.count++;
      if (ts > existing.latest) existing.latest = ts;
    } else {
      byKey.set(k, { key: k, count: 1, items: [ev], latest: ts });
    }
  }
  return Array.from(byKey.values()).sort((a, b) => (a.latest < b.latest ? 1 : -1));
}
