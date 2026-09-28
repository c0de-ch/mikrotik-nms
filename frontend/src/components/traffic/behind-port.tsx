"use client";

import { useCallback, useMemo, useState } from "react";
import { ArrowLeftRight, ArrowUpRight, Link2, Network, Users, Wifi } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useAuth } from "@/context/auth";
import { api, type BehindClient, type PortBehind, type PortRole } from "@/lib/api";
import { usePolledFetch } from "./hooks";
import { EmptyState, Notice } from "./notice";

const BEHIND_REFRESH_MS = 120_000;
const FILTER_THRESHOLD = 12;

function clientName(c: BehindClient): string {
  return c.host_name || c.ip || c.vendor || c.mac;
}

function neighborVerb(role: PortBehind["role"]): string {
  if (role === "uplink") return "Parent";
  if (role === "peer") return "Peer link to";
  return "Feeds";
}

// BehindPort: what this port leads to — neighbour device, upstream node,
// VLAN membership and every client MAC learned on it (bridge FDB ⋈ client
// cache). On a downlink that is everything behind the downstream device; on
// a peer link it is the other managed device (its clients belong to that
// device's tree, so none are attributed here).
export function BehindPort({
  deviceId,
  iface,
  roleInfo,
  onOpenDevice,
  onOpenPort,
}: {
  deviceId: string;
  iface: string;
  // The port's /traffic/ports/roles entry: neighbour fallback when the
  // behind response carries none.
  roleInfo?: PortRole;
  onOpenDevice: (deviceId: string) => void;
  onOpenPort: (deviceId: string, iface: string) => void;
}) {
  const { token } = useAuth();
  const [filter, setFilter] = useState("");
  const fetcher = useCallback(() => api.traffic.portBehind(token!, deviceId, iface), [token, deviceId, iface]);
  const { data, error, loading } = usePolledFetch(token ? `behind|${deviceId}|${iface}` : null, fetcher, BEHIND_REFRESH_MS);

  const clients = useMemo(() => {
    const list = data?.clients ?? [];
    const q = filter.trim().toLowerCase();
    if (!q) return list;
    return list.filter((c) =>
      [c.host_name, c.ip, c.mac, c.vendor, c.ssid, c.ap, c.attached_device_name, String(c.vid || "")]
        .join(" ")
        .toLowerCase()
        .includes(q),
    );
  }, [data, filter]);

  const upstream = data && (data.role === "uplink" || data.role === "wan");
  const neighbor = useMemo(() => {
    if (data?.neighbor) return data.neighbor;
    if (roleInfo?.neighbor_device_id && roleInfo.role === data?.role) {
      return { device_id: roleInfo.neighbor_device_id, name: roleInfo.neighbor_name || roleInfo.neighbor_device_id.slice(0, 8), iface: roleInfo.neighbor_iface || "" };
    }
    return null;
  }, [data, roleInfo]);
  // Bond member: clients are learned (and counted) on the bond, not here.
  const master = roleInfo?.master || "";

  return (
    <Card>
      <CardHeader className="gap-1">
        <CardTitle className="text-base">Behind this port</CardTitle>
        {data && (
          <p className="text-xs text-muted-foreground">
            {data.client_count} client{data.client_count === 1 ? "" : "s"} · {data.mac_count} MAC{data.mac_count === 1 ? "" : "s"} learned
            {data.client_count > 0 && upstream ? " — learned from the upstream side" : ""}
            {data.client_count > 0 && data.role === "peer" ? " — learned across the peer link; each is attached elsewhere" : ""}
          </p>
        )}
      </CardHeader>
      <CardContent className="space-y-3">
        {error && <Notice kind="error">Could not load what is behind this port: {error}</Notice>}
        {loading && !data && <div className="h-24 animate-pulse rounded-lg bg-muted" />}

        {data && (
          <>
            {(neighbor || data.uplink || data.vlans.length > 0 || master) && (
              <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-sm">
                {master && (
                  <span className="inline-flex min-w-0 items-center gap-1.5">
                    <Link2 className="h-3.5 w-3.5 text-muted-foreground" />
                    <span className="text-muted-foreground">Member of</span>
                    <button
                      type="button"
                      className="font-mono text-xs font-medium hover:underline"
                      onClick={() => onOpenPort(deviceId, master)}
                      title="Open the bond — it carries and counts this member's traffic and clients"
                    >
                      {master}
                    </button>
                  </span>
                )}
                {neighbor && (
                  <span className="inline-flex min-w-0 items-center gap-1.5">
                    {data.role === "uplink" ? (
                      <ArrowUpRight className="h-3.5 w-3.5 text-muted-foreground" />
                    ) : data.role === "peer" ? (
                      <ArrowLeftRight className="h-3.5 w-3.5 text-muted-foreground" />
                    ) : (
                      <Network className="h-3.5 w-3.5 text-muted-foreground" />
                    )}
                    <span className="text-muted-foreground">{neighborVerb(data.role)}</span>
                    <button type="button" className="truncate font-medium hover:underline" onClick={() => onOpenDevice(neighbor.device_id)}>
                      {neighbor.name}
                    </button>
                    {neighbor.iface && (
                      <button
                        type="button"
                        className="font-mono text-xs text-muted-foreground hover:text-foreground hover:underline"
                        onClick={() => onOpenPort(neighbor.device_id, neighbor.iface)}
                        title="Open the neighbour's port facing this one"
                      >
                        via {neighbor.iface}
                      </button>
                    )}
                  </span>
                )}
                {data.uplink && (
                  <span className="inline-flex items-center gap-1.5">
                    <ArrowUpRight className="h-3.5 w-3.5 text-muted-foreground" />
                    <span className="text-muted-foreground">Upstream</span>
                    <span className="font-medium">{data.uplink.label || data.uplink.node_id}</span>
                    {data.uplink.gateway_ip && <span className="font-mono text-xs text-muted-foreground">{data.uplink.gateway_ip}</span>}
                  </span>
                )}
                {data.vlans.length > 0 && (
                  <span className="flex flex-wrap items-center gap-1">
                    {data.vlans.map((v) => (
                      <Badge key={`${v.vid}-${v.tagged}`} variant="outline" className="font-normal" title={v.tagged ? "Tagged on this port" : "Untagged (access) on this port"}>
                        VLAN {v.vid}
                        {v.name ? ` · ${v.name}` : ""}
                        <span className="text-muted-foreground">{v.tagged ? "T" : "U"}</span>
                      </Badge>
                    ))}
                  </span>
                )}
              </div>
            )}

            {data.clients.length === 0 ? (
              <EmptyState icon={Users}>
                {master
                  ? `Member of ${master} — clients behind this link are learned and counted on the bond.`
                  : data.role === "peer"
                    ? `Link to ${neighbor ? neighbor.name : "another managed device"} outside the inferred tree — clients behind it are counted on that side${data.mac_count > 0 ? ` (${data.mac_count} MACs learned here)` : ""}.`
                    : upstream
                      ? `Upstream port — ${data.mac_count} MACs learned`
                      : data.mac_count > 0
                        ? `Only managed devices were learned here (${data.mac_count} MACs).`
                        : data.role === "idle"
                          ? "No MACs learned on this port"
                          : "No clients learned on this port yet — the bridge host table is read every few minutes."}
              </EmptyState>
            ) : (
              <>
                {data.clients.length > FILTER_THRESHOLD && (
                  <Input
                    placeholder="Filter by name, IP, MAC, vendor, VLAN…"
                    value={filter}
                    onChange={(e) => setFilter(e.target.value)}
                    className="h-8 max-w-sm"
                  />
                )}
                <div className="max-h-[440px] overflow-y-auto rounded-md border">
                  <Table className="min-w-[680px]">
                    <TableHeader>
                      <TableRow className="text-xs">
                        <TableHead>Client</TableHead>
                        <TableHead>IP</TableHead>
                        <TableHead>MAC</TableHead>
                        <TableHead>Vendor</TableHead>
                        <TableHead className="text-right">VLAN</TableHead>
                        <TableHead>Attached at</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {clients.map((c) => {
                        const here = c.attached_device_id === deviceId && c.attached_iface === iface;
                        return (
                          <TableRow key={c.mac} className="text-xs">
                            <TableCell className="max-w-[220px]">
                              <div className="flex min-w-0 items-center gap-1.5">
                                {c.wireless && <Wifi className="h-3 w-3 shrink-0 text-muted-foreground" />}
                                <span className="truncate font-medium" title={clientName(c)}>
                                  {clientName(c)}
                                </span>
                              </div>
                              {c.wireless && (c.ssid || c.ap) && (
                                <div className="truncate text-[11px] text-muted-foreground">
                                  {[c.ssid, c.ap, c.signal].filter(Boolean).join(" · ")}
                                </div>
                              )}
                            </TableCell>
                            <TableCell className="font-mono">{c.ip || "—"}</TableCell>
                            <TableCell className="font-mono">{c.mac}</TableCell>
                            <TableCell className="max-w-[160px] truncate" title={c.vendor}>
                              {c.vendor || "—"}
                            </TableCell>
                            <TableCell className="text-right tabular-nums">{c.vid || "—"}</TableCell>
                            <TableCell className="max-w-[200px]">
                              {here ? (
                                <span className="text-muted-foreground">this port</span>
                              ) : c.attached_device_id ? (
                                <button
                                  type="button"
                                  className="truncate text-left hover:underline"
                                  onClick={() => onOpenPort(c.attached_device_id, c.attached_iface)}
                                  title="Open the edge port this client is attached to"
                                >
                                  {c.attached_device_name || c.attached_device_id.slice(0, 8)} · <span className="font-mono">{c.attached_iface}</span>
                                </button>
                              ) : (
                                <span className="text-muted-foreground">—</span>
                              )}
                            </TableCell>
                          </TableRow>
                        );
                      })}
                      {clients.length === 0 && (
                        <TableRow>
                          <TableCell colSpan={6} className="py-6 text-center text-muted-foreground">
                            No client matches “{filter}”.
                          </TableCell>
                        </TableRow>
                      )}
                    </TableBody>
                  </Table>
                </div>
                {data.client_count > data.clients.length && (
                  <p className="text-xs text-muted-foreground">
                    Showing the first {data.clients.length} of {data.client_count} clients.
                  </p>
                )}
              </>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}
