"use client";

import { useEffect, useState } from "react";
import { AreaChart, Area, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer } from "recharts";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { useAuth } from "@/context/auth";
import { api, type Device, type DeviceInterface } from "@/lib/api";
import { useWebSocket } from "@/hooks/use-websocket";

interface ChartPoint {
  time: string;
  rx: number;
  tx: number;
}

function formatBps(bps: number): string {
  if (bps >= 1e9) return `${(bps / 1e9).toFixed(1)} Gbps`;
  if (bps >= 1e6) return `${(bps / 1e6).toFixed(1)} Mbps`;
  if (bps >= 1e3) return `${(bps / 1e3).toFixed(1)} Kbps`;
  return `${bps} bps`;
}

export default function TrafficPage() {
  const { token } = useAuth();
  const [devices, setDevices] = useState<Device[]>([]);
  const [interfaces, setInterfaces] = useState<DeviceInterface[]>([]);
  const [selectedDevice, setSelectedDevice] = useState("");
  const [selectedIface, setSelectedIface] = useState("");
  const [chartData, setChartData] = useState<ChartPoint[]>([]);

  useEffect(() => {
    if (!token) return;
    api.devices.list(token).then(setDevices).catch(console.error);
  }, [token]);

  useEffect(() => {
    if (!token || !selectedDevice) return;
    api.devices.interfaces(token, selectedDevice).then(setInterfaces).catch(console.error);
  }, [token, selectedDevice]);

  useEffect(() => {
    if (!token || !selectedDevice || !selectedIface) return;
    api.traffic.get(token, selectedDevice, selectedIface).then((samples) => {
      setChartData(
        samples.reverse().map((s) => ({
          time: new Date(s.collected_at).toLocaleTimeString(),
          rx: s.rx_bits_per_sec,
          tx: s.tx_bits_per_sec,
        })),
      );
    }).catch(console.error);
  }, [token, selectedDevice, selectedIface]);

  const wsTopic = selectedDevice && selectedIface ? `traffic.${selectedDevice}.${selectedIface}` : "";

  useWebSocket(wsTopic, (data) => {
    if (!wsTopic) return;
    const d = data as { rx_bps: number; tx_bps: number; timestamp: string };
    setChartData((prev) => {
      const next = [...prev, { time: new Date().toLocaleTimeString(), rx: d.rx_bps, tx: d.tx_bps }];
      return next.slice(-300); // Keep last 5 minutes at 1/s
    });
  });

  return (
    <div className="space-y-4">
      <h1 className="text-2xl font-bold">Traffic Monitoring</h1>

      <div className="flex gap-4">
        <div className="space-y-1">
          <Label>Device</Label>
          <select
            className="flex h-9 w-[200px] rounded-md border bg-transparent px-3 py-1 text-sm"
            value={selectedDevice}
            onChange={(e) => { setSelectedDevice(e.target.value); setSelectedIface(""); setChartData([]); }}
          >
            <option value="">Select device...</option>
            {devices.filter((d) => d.status === "online").map((d) => (
              <option key={d.id} value={d.id}>{d.identity || d.address}</option>
            ))}
          </select>
        </div>
        <div className="space-y-1">
          <Label>Interface</Label>
          <select
            className="flex h-9 w-[200px] rounded-md border bg-transparent px-3 py-1 text-sm"
            value={selectedIface}
            onChange={(e) => { setSelectedIface(e.target.value); setChartData([]); }}
            disabled={!selectedDevice}
          >
            <option value="">Select interface...</option>
            {interfaces.filter((i) => !i.disabled).map((i) => (
              <option key={i.name} value={i.name}>{i.name}</option>
            ))}
          </select>
        </div>
      </div>

      {selectedIface && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">
              {interfaces.find((i) => i.name === selectedIface)?.name ?? selectedIface} — Traffic
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
                  <Area type="monotone" dataKey="rx" stroke="#22c55e" fill="#22c55e" fillOpacity={0.2} name="RX" />
                  <Area type="monotone" dataKey="tx" stroke="#3b82f6" fill="#3b82f6" fillOpacity={0.2} name="TX" />
                </AreaChart>
              </ResponsiveContainer>
            ) : (
              <div className="flex h-[400px] items-center justify-center text-muted-foreground">
                Waiting for traffic data...
              </div>
            )}
          </CardContent>
        </Card>
      )}
    </div>
  );
}
