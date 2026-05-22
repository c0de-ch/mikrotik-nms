"use client";

import { useEffect, useState, useCallback } from "react";
import { AreaChart, Area, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer } from "recharts";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ChevronRight, ChevronDown, ArrowLeft, Activity } from "lucide-react";
import { useAuth } from "@/context/auth";
import { api, type Device, type DeviceInterface } from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";

interface ChartPoint {
  time: string;
  rx: number;
  tx: number;
}

interface DeviceTraffic {
  device_id: string;
  identity: string;
  address: string;
  status: string;
  rx_total: number;
  tx_total: number;
}

function formatBps(bps: number): string {
  if (bps >= 1e9) return `${(bps / 1e9).toFixed(1)} Gbps`;
  if (bps >= 1e6) return `${(bps / 1e6).toFixed(1)} Mbps`;
  if (bps >= 1e3) return `${(bps / 1e3).toFixed(1)} Kbps`;
  return `${bps} bps`;
}

function classifyInterface(name: string, type: string): string {
  if (type === "bridge" || name.startsWith("bridge")) return "bridge";
  if (type === "wlan" || name.startsWith("wlan") || name.startsWith("wifi")) return "wireless";
  if (type === "vlan" || name.startsWith("vlan")) return "vlan";
  if (type === "ether" || name.startsWith("ether") || name.startsWith("sfp") || name.startsWith("qsfp")) return "ethernet";
  if (name.startsWith("pppoe") || name.startsWith("l2tp") || name.startsWith("pptp") || name.startsWith("ovpn") || name.startsWith("sstp")) return "tunnel";
  if (name === "lo" || name.startsWith("loop")) return "loopback";
  return "other";
}

type ViewLevel = "overview" | "device" | "interface";

// PortSparkline subscribes to one interface's traffic stream and renders a
// compact RX/TX area chart. Used in the device view so each port shows its
// own live throughput at a glance — clicking the row still drills into the
// full-size chart at viewLevel="interface".
//
// The buffer is intentionally short (60 samples ≈ 1 minute at 1s cadence)
// so the spark line stays readable and we don't hold many MB of points
// across a 24-port switch.
const SPARK_MAX_POINTS = 60;
function PortSparkline({ deviceId, ifaceName }: { deviceId: string; ifaceName: string }) {
  const [points, setPoints] = useState<ChartPoint[]>([]);
  const topic = `traffic.${deviceId}.${ifaceName}`;
  useWebSocket(topic, useCallback((data: unknown) => {
    const d = data as { rx_bps: number; tx_bps: number };
    setPoints((prev) => {
      const next = [...prev, { time: "", rx: d.rx_bps, tx: d.tx_bps }];
      return next.slice(-SPARK_MAX_POINTS);
    });
  }, []));

  const latest = points[points.length - 1];
  return (
    <div className="flex items-center gap-3 w-full">
      <div className="h-8 w-32 sm:w-44 shrink-0">
        {points.length > 1 ? (
          <ResponsiveContainer width="100%" height="100%">
            <AreaChart data={points} margin={{ top: 2, right: 0, bottom: 0, left: 0 }}>
              <Area type="monotone" dataKey="rx" stroke="#5A9CB5" fill="#5A9CB5" fillOpacity={0.25} isAnimationActive={false} />
              <Area type="monotone" dataKey="tx" stroke="#FAAC68" fill="#FAAC68" fillOpacity={0.25} isAnimationActive={false} />
            </AreaChart>
          </ResponsiveContainer>
        ) : (
          <div className="h-full flex items-center text-[10px] text-muted-foreground">awaiting…</div>
        )}
      </div>
      <div className="flex flex-col items-end shrink-0 text-[11px] font-mono tabular-nums">
        <span style={{ color: "#5A9CB5" }} title="RX">↓ {formatBps(latest?.rx ?? 0)}</span>
        <span style={{ color: "#FAAC68" }} title="TX">↑ {formatBps(latest?.tx ?? 0)}</span>
      </div>
    </div>
  );
}

export default function TrafficPage() {
  const { token } = useAuth();
  const [devices, setDevices] = useState<Device[]>([]);
  const [viewLevel, setViewLevel] = useState<ViewLevel>("overview");
  const [selectedDevice, setSelectedDevice] = useState<Device | null>(null);
  const [interfaces, setInterfaces] = useState<DeviceInterface[]>([]);
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(new Set(["bridge", "ethernet"]));
  const [selectedIface, setSelectedIface] = useState<string | null>(null);
  const [chartData, setChartData] = useState<ChartPoint[]>([]);
  const [deviceTraffic, setDeviceTraffic] = useState<Map<string, DeviceTraffic>>(new Map());

  const [trafficSummary, setTrafficSummary] = useState<Map<string, { rx: number; tx: number }>>(new Map());
  const [loadingTraffic, setLoadingTraffic] = useState(false);
  const [showSparklines, setShowSparklines] = useState(true);

  useEffect(() => {
    if (!token) return;
    api.devices.list(token).then(setDevices).catch(console.error);
  }, [token]);

  // Load traffic summary when on overview
  const loadTrafficSummary = useCallback(() => {
    if (!token || viewLevel !== "overview") return;
    setLoadingTraffic(true);
    api.traffic.summary(token).then((data) => {
      const map = new Map<string, { rx: number; tx: number }>();
      for (const d of data) map.set(d.device_id, { rx: d.rx_bps, tx: d.tx_bps });
      setTrafficSummary(map);
    }).catch(console.error).finally(() => setLoadingTraffic(false));
  }, [token, viewLevel]);

  useEffect(() => {
    loadTrafficSummary();
    if (viewLevel !== "overview") return;
    const interval = setInterval(loadTrafficSummary, 15000);
    return () => clearInterval(interval);
  }, [loadTrafficSummary, viewLevel]);

  // Live device health updates for traffic overview
  useWebSocket("device.health", useCallback((data: unknown) => {
    const update = data as { device_id: string; status: string };
    setDevices((prev) =>
      prev.map((d) => d.id === update.device_id ? { ...d, status: update.status as Device["status"] } : d)
    );
  }, []));

  // Load interfaces when a device is selected
  useEffect(() => {
    if (!token || !selectedDevice) return;
    api.devices.interfaces(token, selectedDevice.id).then(setInterfaces).catch(console.error);
  }, [token, selectedDevice]);

  // Load historical traffic for selected interface
  useEffect(() => {
    if (!token || !selectedDevice || !selectedIface) return;
    api.traffic.get(token, selectedDevice.id, selectedIface).then((samples) => {
      setChartData(
        samples.reverse().map((s) => ({
          time: new Date(s.collected_at).toLocaleTimeString(),
          rx: s.rx_bits_per_sec,
          tx: s.tx_bits_per_sec,
        })),
      );
    }).catch(console.error);
  }, [token, selectedDevice, selectedIface]);

  // Live traffic for selected interface
  const wsTopic = selectedDevice && selectedIface ? `traffic.${selectedDevice.id}.${selectedIface}` : "";
  useWebSocket(wsTopic, useCallback((data: unknown) => {
    if (!wsTopic) return;
    const d = data as { rx_bps: number; tx_bps: number };
    setChartData((prev) => {
      const next = [...prev, { time: new Date().toLocaleTimeString(), rx: d.rx_bps, tx: d.tx_bps }];
      return next.slice(-300);
    });
  }, [wsTopic]));

  const onlineDevices = devices.filter((d) => d.status === "online");

  const navigateToDevice = (device: Device) => {
    setSelectedDevice(device);
    setSelectedIface(null);
    setChartData([]);
    setViewLevel("device");
  };

  const navigateToInterface = (ifaceName: string) => {
    setSelectedIface(ifaceName);
    setChartData([]);
    setViewLevel("interface");
  };

  const goBack = () => {
    if (viewLevel === "interface") {
      setSelectedIface(null);
      setChartData([]);
      setViewLevel("device");
    } else if (viewLevel === "device") {
      setSelectedDevice(null);
      setInterfaces([]);
      setViewLevel("overview");
    }
  };

  const toggleGroup = (group: string) => {
    setExpandedGroups((prev) => {
      const next = new Set(prev);
      if (next.has(group)) next.delete(group); else next.add(group);
      return next;
    });
  };

  // Group interfaces by type
  const groupedInterfaces = interfaces
    .filter((i) => !i.disabled)
    .reduce<Record<string, DeviceInterface[]>>((acc, iface) => {
      const group = classifyInterface(iface.name, iface.type);
      if (!acc[group]) acc[group] = [];
      acc[group].push(iface);
      return acc;
    }, {});

  const groupOrder = ["bridge", "ethernet", "wireless", "vlan", "tunnel", "other", "loopback"];
  const groupLabels: Record<string, string> = {
    bridge: "Bridge",
    ethernet: "Ethernet / SFP",
    wireless: "Wireless",
    vlan: "VLAN",
    tunnel: "Tunnels (PPPoE, VPN)",
    loopback: "Loopback",
    other: "Other",
  };

  // Breadcrumb
  const breadcrumb = () => {
    const parts: { label: string; onClick?: () => void }[] = [
      { label: "All Devices", onClick: viewLevel !== "overview" ? () => { setViewLevel("overview"); setSelectedDevice(null); setSelectedIface(null); setChartData([]); } : undefined },
    ];
    if (selectedDevice) {
      parts.push({
        label: selectedDevice.identity || selectedDevice.address,
        onClick: viewLevel === "interface" ? () => { setViewLevel("device"); setSelectedIface(null); setChartData([]); } : undefined,
      });
    }
    if (selectedIface) {
      parts.push({ label: selectedIface });
    }
    return parts;
  };

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-3">
        {viewLevel !== "overview" && (
          <Button variant="ghost" size="icon" onClick={goBack}>
            <ArrowLeft className="h-4 w-4" />
          </Button>
        )}
        <div>
          <h1 className="text-2xl font-bold">Traffic Monitoring</h1>
          <div className="flex items-center gap-1 text-sm text-muted-foreground">
            {breadcrumb().map((part, i) => (
              <span key={i} className="flex items-center gap-1">
                {i > 0 && <ChevronRight className="h-3 w-3" />}
                {part.onClick ? (
                  <button onClick={part.onClick} className="hover:text-foreground transition-colors">{part.label}</button>
                ) : (
                  <span className="text-foreground">{part.label}</span>
                )}
              </span>
            ))}
          </div>
        </div>
      </div>

      {/* LEVEL 1: All devices overview */}
      {viewLevel === "overview" && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {onlineDevices.map((device) => {
            const traffic = trafficSummary.get(device.id);
            return (
              <Card
                key={device.id}
                className="cursor-pointer hover:ring-2 hover:ring-primary/30 transition-all"
                onClick={() => navigateToDevice(device)}
              >
                <CardContent className="pt-4">
                  <div className="flex items-center justify-between mb-2">
                    <div>
                      <p className="font-medium">{device.identity || device.address}</p>
                      <p className="text-xs text-muted-foreground">{device.address} · {device.board}</p>
                    </div>
                    <ChevronRight className="h-4 w-4 text-muted-foreground shrink-0" />
                  </div>
                  {traffic ? (
                    <div className="space-y-1.5 mt-3">
                      <div className="flex items-center justify-between text-xs">
                        <span className="text-muted-foreground">RX</span>
                        <span className="font-medium" style={{ color: "#5A9CB5" }}>{formatBps(traffic.rx)}</span>
                      </div>
                      <div className="h-1.5 rounded-full bg-muted overflow-hidden">
                        <div className="h-full rounded-full transition-all" style={{ width: `${Math.min(100, Math.max(2, traffic.rx / 1e6))}%`, backgroundColor: "#5A9CB5" }} />
                      </div>
                      <div className="flex items-center justify-between text-xs">
                        <span className="text-muted-foreground">TX</span>
                        <span className="font-medium" style={{ color: "#FAAC68" }}>{formatBps(traffic.tx)}</span>
                      </div>
                      <div className="h-1.5 rounded-full bg-muted overflow-hidden">
                        <div className="h-full rounded-full transition-all" style={{ width: `${Math.min(100, Math.max(2, traffic.tx / 1e6))}%`, backgroundColor: "#FAAC68" }} />
                      </div>
                    </div>
                  ) : (
                    <div className="grid grid-cols-2 gap-2 text-xs text-muted-foreground mt-3">
                      <div>CPU: {device.cpu_load ?? "—"}%</div>
                      <div>Mem: {device.memory_used && device.memory_total ? Math.round((device.memory_used / device.memory_total) * 100) : "—"}%</div>
                    </div>
                  )}
                </CardContent>
              </Card>
            );
          })}
          {onlineDevices.length === 0 && (
            <Card className="col-span-full">
              <CardContent className="py-12 text-center text-muted-foreground">
                <Activity className="mx-auto mb-3 h-8 w-8" />
                <p>No online devices to monitor</p>
              </CardContent>
            </Card>
          )}
          {devices.filter((d) => d.status !== "online").length > 0 && (
            <div className="col-span-full text-xs text-muted-foreground">
              {devices.filter((d) => d.status !== "online").length} offline/unknown device(s) hidden
            </div>
          )}
        </div>
      )}

      {/* LEVEL 2: Device interfaces grouped by type */}
      {viewLevel === "device" && selectedDevice && (
        <div className="space-y-2">
          <div className="flex items-center justify-end gap-2 text-xs">
            <label className="inline-flex items-center gap-1.5 cursor-pointer text-muted-foreground">
              <input
                type="checkbox"
                checked={showSparklines}
                onChange={(e) => setShowSparklines(e.target.checked)}
              />
              <span>Live per-port graphs</span>
            </label>
          </div>
          {groupOrder.filter((g) => groupedInterfaces[g]?.length).map((group) => {
            const ifaces = groupedInterfaces[group];
            const isExpanded = expandedGroups.has(group);
            return (
              <Card key={group}>
                <button
                  onClick={() => toggleGroup(group)}
                  className="flex w-full items-center justify-between px-4 py-3 text-left hover:bg-muted/50 transition-colors rounded-t-xl"
                >
                  <div className="flex items-center gap-2">
                    {isExpanded ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
                    <span className="font-medium">{groupLabels[group] || group}</span>
                    <Badge variant="secondary" className="text-xs">{ifaces.length}</Badge>
                  </div>
                </button>
                {isExpanded && (
                  <CardContent className="pt-0 pb-2">
                    <div className="grid gap-1">
                      {ifaces.map((iface) => (
                        <button
                          key={iface.name}
                          onClick={() => navigateToInterface(iface.name)}
                          className="flex items-center justify-between gap-3 px-3 py-2 rounded-md text-sm hover:bg-muted/70 transition-colors text-left"
                        >
                          <div className="flex items-center gap-3 min-w-0 flex-1">
                            <span className="font-medium truncate" title={iface.name}>{iface.name}</span>
                            {iface.mac_address && (
                              <span className="font-mono text-xs text-muted-foreground hidden md:inline">{iface.mac_address}</span>
                            )}
                            {iface.comment && (
                              <span className="text-xs text-muted-foreground truncate hidden lg:inline">({iface.comment})</span>
                            )}
                          </div>
                          <div className="flex items-center gap-3 shrink-0">
                            {showSparklines && iface.running && selectedDevice && (
                              <PortSparkline deviceId={selectedDevice.id} ifaceName={iface.name} />
                            )}
                            <Badge variant={iface.running ? "default" : "secondary"} className="text-xs">
                              {iface.running ? "up" : "down"}
                            </Badge>
                            <ChevronRight className="h-3 w-3 text-muted-foreground" />
                          </div>
                        </button>
                      ))}
                    </div>
                  </CardContent>
                )}
              </Card>
            );
          })}
          {interfaces.length === 0 && (
            <Card>
              <CardContent className="py-8 text-center text-muted-foreground">
                Loading interfaces...
              </CardContent>
            </Card>
          )}
        </div>
      )}

      {/* LEVEL 3: Interface traffic chart */}
      {viewLevel === "interface" && selectedDevice && selectedIface && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base flex items-center gap-2">
              <Activity className="h-4 w-4" />
              {selectedIface} — {selectedDevice.identity || selectedDevice.address}
            </CardTitle>
          </CardHeader>
          <CardContent>
            {chartData.length > 0 ? (
              <ResponsiveContainer width="100%" height={400}>
                <AreaChart data={chartData}>
                  <CartesianGrid strokeDasharray="3 3" />
                  <XAxis dataKey="time" tick={{ fontSize: 11 }} interval="preserveStartEnd" />
                  <YAxis tickFormatter={formatBps} tick={{ fontSize: 11 }} />
                  <Tooltip formatter={(value) => formatBps(Number(value))} />
                  <Area type="monotone" dataKey="rx" stroke="#5A9CB5" fill="#5A9CB5" fillOpacity={0.2} name="RX (download)" />
                  <Area type="monotone" dataKey="tx" stroke="#FAAC68" fill="#FAAC68" fillOpacity={0.2} name="TX (upload)" />
                </AreaChart>
              </ResponsiveContainer>
            ) : (
              <div className="flex h-[400px] items-center justify-center text-muted-foreground">
                <div className="text-center">
                  <Activity className="mx-auto mb-2 h-8 w-8 animate-pulse" />
                  <p>Waiting for traffic data...</p>
                  <p className="text-xs mt-1">Data streams when this view is open</p>
                </div>
              </div>
            )}
          </CardContent>
        </Card>
      )}
    </div>
  );
}
