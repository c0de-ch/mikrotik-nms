// Client-side re-ranking for range=live: the server's live ranking (fetched
// every minute) supplies sparklines and "behind" summaries, while the values
// and order follow every traffic.ports snapshot.

import type { PortRate, PortRole, TopDir, TopMetric, TopPortRow } from "@/lib/api";
import { PHYSICAL_ROLES, comparePortNames, fallbackRole, metricValue, portKey } from "./lib";

export function buildLiveRows({
  ports,
  serverRows,
  roles,
  nameOf,
  metric,
  dir,
  physical,
  includeIdle,
  limit,
}: {
  ports: PortRate[];
  serverRows: TopPortRow[];
  roles: Map<string, PortRole>;
  nameOf: (deviceId: string) => string;
  metric: TopMetric;
  dir: TopDir;
  physical: boolean;
  // Device view: keep idle ports (and server-known ports missing from the
  // snapshot) so the device's list stays complete.
  includeIdle: boolean;
  limit: number;
}): TopPortRow[] {
  const server = new Map(serverRows.map((r) => [portKey(r.device_id, r.iface), r]));
  const out = new Map<string, TopPortRow>();

  for (const p of ports) {
    if (!includeIdle && p.rx_bps + p.tx_bps <= 0) continue;
    const k = portKey(p.device_id, p.iface);
    const sr = server.get(k);
    const ri = roles.get(k);
    const role = ri?.role ?? sr?.role ?? p.role ?? fallbackRole(p.iface, p.type);
    if (physical && !PHYSICAL_ROLES.has(role)) continue;
    out.set(k, {
      rank: 0,
      device_id: p.device_id,
      device_name: ri?.device_name || sr?.device_name || p.device_name || nameOf(p.device_id),
      iface: p.iface,
      type: p.type,
      role,
      behind: ri?.behind ?? sr?.behind ?? "",
      neighbor_device_id: ri?.neighbor_device_id ?? sr?.neighbor_device_id,
      client_count: ri?.client_count ?? sr?.client_count ?? 0,
      running: p.running,
      rx_avg: p.rx_bps,
      tx_avg: p.tx_bps,
      rx_max: p.rx_bps,
      tx_max: p.tx_bps,
      rx_bytes: 0,
      tx_bytes: 0,
      value: 0,
      sparkline: sr?.sparkline ?? [],
    });
  }
  if (includeIdle) {
    for (const [k, sr] of server) {
      if (out.has(k)) continue;
      if (physical && !PHYSICAL_ROLES.has(sr.role)) continue;
      out.set(k, { ...sr, rx_avg: 0, tx_avg: 0, rx_max: 0, tx_max: 0, rx_bytes: 0, tx_bytes: 0 });
    }
  }

  const m: TopMetric = metric === "bytes" ? "avg" : metric;
  const rows = [...out.values()];
  for (const r of rows) r.value = metricValue(r, m, dir);
  rows.sort(
    (a, b) => b.value - a.value || a.device_name.localeCompare(b.device_name) || comparePortNames(a.iface, b.iface),
  );
  return rows.slice(0, limit).map((r, i) => ({ ...r, rank: i + 1 }));
}
