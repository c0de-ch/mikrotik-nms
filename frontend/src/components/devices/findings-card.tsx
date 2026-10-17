"use client";

import { useEffect, useMemo, useState } from "react";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useNow } from "@/hooks/use-now";
import { api, type DeviceFinding } from "@/lib/api";
import { fmtAgo, fmtStamp } from "@/lib/device-load";
import { cn } from "@/lib/utils";

// Findings: what the pollers noticed about the device from what they already
// read (today: the account lines of its log). Grouped by kind, so each kind
// explains itself once; every row is one (kind, detail) with how often and
// when it was seen.

const KINDS: Record<string, { label: string; hint: string }> = {
  parallel_api_poller: {
    label: "Another API client",
    hint: "Something other than this NMS logged in to the device's API — another monitoring system, a script or an app. Its reads add load this NMS does not budget for. If the address belongs to this NMS (behind NAT, or a second address), add it to NMS source addresses in Settings.",
  },
  login_failure: {
    label: "Failed login",
    hint: "A login to the device failed. Repeated failures from one address are usually a script with an old password, or someone guessing.",
  },
};

const SEVERITY: Record<string, { label: string; className: string; rank: number }> = {
  critical: { label: "Critical", className: "bg-red-500/15 text-red-700 dark:text-red-400", rank: 0 },
  warn: { label: "Warning", className: "bg-amber-500/15 text-amber-800 dark:text-amber-300", rank: 1 },
  info: { label: "Info", className: "bg-muted text-muted-foreground", rank: 2 },
};

function kindInfo(kind: string) {
  return KINDS[kind] ?? { label: kind.replaceAll("_", " "), hint: "" };
}

export function FindingsCard({ deviceId, token }: { deviceId: string; token: string }) {
  const now = useNow(60_000);
  const [state, setState] = useState<{ id: string; findings: DeviceFinding[]; error: string | null } | null>(null);

  useEffect(() => {
    let alive = true;
    api.devices
      .findings(token, deviceId)
      .then((findings) => {
        if (alive) setState({ id: deviceId, findings, error: null });
      })
      .catch((e) => {
        if (alive) setState({ id: deviceId, findings: [], error: e instanceof Error ? e.message : String(e) });
      });
    return () => {
      alive = false;
    };
  }, [token, deviceId]);

  const current = state?.id === deviceId ? state : null;
  // Kinds ordered by their most severe row, rows by last seen (the API order).
  const groups = useMemo(() => {
    const byKind = new Map<string, DeviceFinding[]>();
    for (const f of current?.findings ?? []) {
      const list = byKind.get(f.kind);
      if (list) list.push(f);
      else byKind.set(f.kind, [f]);
    }
    const worst = (rows: DeviceFinding[]) => Math.min(...rows.map((r) => SEVERITY[r.severity]?.rank ?? 3));
    return [...byKind.entries()].sort((a, b) => worst(a[1]) - worst(b[1]));
  }, [current]);

  return (
    <Card>
      <CardHeader>
        <CardTitle>Findings</CardTitle>
        <CardDescription>What the pollers noticed in what they already read from this device.</CardDescription>
      </CardHeader>
      <CardContent>
        {!current ? (
          <div className="space-y-2" aria-label="Loading findings">
            <Skeleton className="h-4 w-1/3" />
            <Skeleton className="h-4 w-2/3" />
          </div>
        ) : current.error ? (
          <p role="alert" className="text-sm text-red-700 dark:text-red-400">
            Could not load findings: {current.error}
          </p>
        ) : groups.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No findings. Nothing else has been seen logging in to this device&apos;s API.
          </p>
        ) : (
          <div className="space-y-5">
            {groups.map(([kind, rows]) => {
              const info = kindInfo(kind);
              return (
                <section key={kind} aria-label={info.label}>
                  <h3 className="text-sm font-medium">{info.label}</h3>
                  {info.hint && <p className="mt-0.5 max-w-prose text-xs text-muted-foreground">{info.hint}</p>}
                  <ul className="mt-2 divide-y rounded-md border">
                    {rows.map((f) => {
                      const sev = SEVERITY[f.severity] ?? SEVERITY.info;
                      const first = Date.parse(f.first_seen);
                      const last = Date.parse(f.last_seen);
                      return (
                        <li key={f.id} className="grid gap-x-3 gap-y-1 px-3 py-2 sm:grid-cols-[4.5rem_1fr_auto] sm:items-baseline">
                          <span className={cn("inline-flex h-5 w-fit items-center rounded-full px-2 text-[11px] font-medium", sev.className)}>
                            {sev.label}
                          </span>
                          <span className="min-w-0 break-words text-sm">{f.detail || "—"}</span>
                          <span className="text-xs tabular-nums text-muted-foreground sm:text-right">
                            <span title={Number.isFinite(last) ? new Date(last).toLocaleString() : f.last_seen}>
                              {f.count}× · last {Number.isFinite(last) && now ? fmtAgo(Math.max(0, (now - last) / 1000)) : "—"}
                            </span>
                            <span className="block" title={Number.isFinite(first) ? new Date(first).toLocaleString() : f.first_seen}>
                              first {Number.isFinite(first) ? fmtStamp(first, now) : "—"}
                            </span>
                          </span>
                        </li>
                      );
                    })}
                  </ul>
                </section>
              );
            })}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
