"use client";

import { useEffect, useState, useCallback } from "react";
import { Plus, Trash2, Power, PowerOff, Save, Moon, Sun } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Separator } from "@/components/ui/separator";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useAuth } from "@/context/auth";
import { api, type DNSServer } from "@/lib/api";
import { toast } from "sonner";

function formatInterval(seconds: number): string {
  if (seconds >= 3600) return `${seconds / 3600}h`;
  if (seconds >= 60) return `${seconds / 60}m`;
  return `${seconds}s`;
}

const intervalOptions = [
  { label: "10s", value: "10" },
  { label: "15s", value: "15" },
  { label: "30s", value: "30" },
  { label: "60s", value: "60" },
  { label: "2m", value: "120" },
  { label: "5m", value: "300" },
  { label: "10m", value: "600" },
  { label: "15m", value: "900" },
  { label: "30m", value: "1800" },
  { label: "1h", value: "3600" },
  { label: "3h", value: "10800" },
  { label: "6h", value: "21600" },
  { label: "12h", value: "43200" },
  { label: "24h", value: "86400" },
];

const retentionOptions = [
  { label: "1 day", value: "1" },
  { label: "3 days", value: "3" },
  { label: "7 days", value: "7" },
  { label: "14 days", value: "14" },
  { label: "30 days", value: "30" },
  { label: "90 days", value: "90" },
];

interface IntervalSetting {
  key: string;
  label: string;
  description: string;
}

const intervalSettings: IntervalSetting[] = [
  { key: "health_interval", label: "Health Polling", description: "How often to poll device CPU, memory, uptime" },
  { key: "topology_interval", label: "Topology Discovery", description: "How often to discover neighbors and rebuild topology" },
  { key: "wifi_interval", label: "WiFi Tracking", description: "How often to poll CAPsMAN for client positions" },
  { key: "client_discovery_interval", label: "Client Discovery", description: "How often to scan ARP/DHCP and update MAC cache" },
  { key: "firmware_interval", label: "Firmware Check", description: "How often to check for RouterOS updates" },
];

export default function SettingsPage() {
  const { token } = useAuth();
  const [settings, setSettings] = useState<Record<string, string>>({});
  const [dirty, setDirty] = useState(false);
  const [saving, setSaving] = useState(false);

  // DNS state
  const [dnsServers, setDnsServers] = useState<DNSServer[]>([]);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [form, setForm] = useState({ name: "", address: "", port: "53" });
  const [testIP, setTestIP] = useState("");
  const [testResult, setTestResult] = useState<string | null>(null);

  const loadSettings = useCallback(() => {
    if (!token) return;
    api.settings.get(token).then(setSettings).catch(console.error);
  }, [token]);

  const loadDNS = useCallback(() => {
    if (!token) return;
    api.dns.list(token).then(setDnsServers).catch(console.error);
  }, [token]);

  useEffect(() => { loadSettings(); loadDNS(); }, [loadSettings, loadDNS]);

  const updateSetting = (key: string, value: string) => {
    setSettings((prev) => ({ ...prev, [key]: value }));
    setDirty(true);
  };

  const saveSettings = async () => {
    if (!token) return;
    setSaving(true);
    try {
      const updated = await api.settings.update(token, settings);
      setSettings(updated);
      setDirty(false);
      toast.success("Settings saved. Restart backend for interval changes to take effect.");

      // Apply dark mode immediately
      if (updated.dark_mode === "true") {
        document.documentElement.classList.add("dark");
      } else {
        document.documentElement.classList.remove("dark");
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to save");
    } finally {
      setSaving(false);
    }
  };

  const toggleDarkMode = () => {
    const newValue = settings.dark_mode === "true" ? "false" : "true";
    updateSetting("dark_mode", newValue);
    // Apply immediately for preview
    if (newValue === "true") {
      document.documentElement.classList.add("dark");
    } else {
      document.documentElement.classList.remove("dark");
    }
  };

  // DNS handlers
  const handleAddDNS = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!token) return;
    try {
      await api.dns.create(token, { name: form.name, address: form.address, port: parseInt(form.port) || 53 });
      toast.success("DNS server added");
      setDialogOpen(false);
      setForm({ name: "", address: "", port: "53" });
      loadDNS();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed");
    }
  };

  const handleToggleDNS = async (srv: DNSServer) => {
    if (!token) return;
    try {
      await api.dns.update(token, srv.id, { ...srv, enabled: !srv.enabled });
      loadDNS();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed");
    }
  };

  const handleDeleteDNS = async (id: string) => {
    if (!token) return;
    try {
      await api.dns.delete(token, id);
      toast.success("DNS server removed");
      loadDNS();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed");
    }
  };

  const handleTestResolve = async () => {
    if (!token || !testIP) return;
    try {
      const results = await api.dns.resolve(token, [testIP]);
      setTestResult(results[testIP] || "(no result)");
    } catch {
      setTestResult("(error)");
    }
  };

  return (
    <div className="space-y-6 max-w-3xl">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">Settings</h1>
        <Button onClick={saveSettings} disabled={!dirty || saving}>
          <Save className="mr-2 h-4 w-4" />
          {saving ? "Saving..." : "Save Changes"}
        </Button>
      </div>

      {/* Dark Mode */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Appearance</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="flex items-center justify-between">
            <div>
              <p className="font-medium text-sm">Dark Mode</p>
              <p className="text-xs text-muted-foreground">Switch between light and dark theme</p>
            </div>
            <Button variant="outline" onClick={toggleDarkMode} className="gap-2">
              {settings.dark_mode === "true" ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
              {settings.dark_mode === "true" ? "Light" : "Dark"}
            </Button>
          </div>
        </CardContent>
      </Card>

      {/* Polling Intervals */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Polling Intervals</CardTitle>
          <p className="text-xs text-muted-foreground">
            Configure how often the backend polls MikroTik devices. Changes take effect after backend restart.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          {intervalSettings.map((is) => (
            <div key={is.key} className="flex items-center justify-between gap-4">
              <div className="flex-1">
                <p className="font-medium text-sm">{is.label}</p>
                <p className="text-xs text-muted-foreground">{is.description}</p>
              </div>
              <select
                className="flex h-8 w-28 rounded-md border bg-transparent px-2 text-sm"
                value={settings[is.key] || ""}
                onChange={(e) => updateSetting(is.key, e.target.value)}
              >
                {intervalOptions.map((opt) => (
                  <option key={opt.value} value={opt.value}>{opt.label}</option>
                ))}
              </select>
            </div>
          ))}

          <Separator />

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Data Retention</p>
              <p className="text-xs text-muted-foreground">How long to keep traffic samples, WiFi history, and client history</p>
            </div>
            <select
              className="flex h-8 w-28 rounded-md border bg-transparent px-2 text-sm"
              value={settings.retention_days || "7"}
              onChange={(e) => updateSetting("retention_days", e.target.value)}
            >
              {retentionOptions.map((opt) => (
                <option key={opt.value} value={opt.value}>{opt.label}</option>
              ))}
            </select>
          </div>
        </CardContent>
      </Card>

      {/* DNS Servers */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <CardTitle className="text-base">DNS Servers</CardTitle>
            <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
              <DialogTrigger render={<Button size="sm" />}>
                <Plus className="mr-2 h-3 w-3" />Add Server
              </DialogTrigger>
              <DialogContent>
                <DialogHeader>
                  <DialogTitle>Add DNS Server</DialogTitle>
                </DialogHeader>
                <form onSubmit={handleAddDNS} className="space-y-4">
                  <div className="space-y-2">
                    <Label>Name (optional)</Label>
                    <Input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="e.g. Pi-hole, AD DNS" />
                  </div>
                  <div className="grid grid-cols-3 gap-3">
                    <div className="col-span-2 space-y-2">
                      <Label>Address</Label>
                      <Input value={form.address} onChange={(e) => setForm({ ...form, address: e.target.value })} required placeholder="192.168.1.1" />
                    </div>
                    <div className="space-y-2">
                      <Label>Port</Label>
                      <Input value={form.port} onChange={(e) => setForm({ ...form, port: e.target.value })} placeholder="53" />
                    </div>
                  </div>
                  <Button type="submit" className="w-full">Add DNS Server</Button>
                </form>
              </DialogContent>
            </Dialog>
          </div>
          <p className="text-sm text-muted-foreground">
            DNS servers for reverse IP lookups during client scans.
          </p>
        </CardHeader>
        <CardContent>
          {dnsServers.length > 0 ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Address</TableHead>
                  <TableHead>Port</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="w-[100px]">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {dnsServers.map((srv) => (
                  <TableRow key={srv.id}>
                    <TableCell className="font-medium">{srv.name || "—"}</TableCell>
                    <TableCell className="font-mono text-sm">{srv.address}</TableCell>
                    <TableCell>{srv.port}</TableCell>
                    <TableCell>
                      <Badge variant={srv.enabled ? "default" : "secondary"}>
                        {srv.enabled ? "enabled" : "disabled"}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      <div className="flex gap-1">
                        <Button variant="ghost" size="icon" onClick={() => handleToggleDNS(srv)}>
                          {srv.enabled ? <PowerOff className="h-4 w-4" /> : <Power className="h-4 w-4" />}
                        </Button>
                        <Button variant="ghost" size="icon" onClick={() => handleDeleteDNS(srv.id)}>
                          <Trash2 className="h-4 w-4 text-destructive" />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ) : (
            <p className="text-sm text-muted-foreground py-4 text-center">
              No DNS servers configured. System DNS will be used as fallback.
            </p>
          )}
        </CardContent>
      </Card>

      {/* Test DNS */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Test DNS Resolution</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="flex gap-3 items-end">
            <div className="flex-1 space-y-2">
              <Label>IP Address</Label>
              <Input value={testIP} onChange={(e) => { setTestIP(e.target.value); setTestResult(null); }} placeholder="192.168.1.100" />
            </div>
            <Button onClick={handleTestResolve} disabled={!testIP}>Resolve</Button>
          </div>
          {testResult !== null && (
            <p className="mt-3 text-sm font-mono rounded-md bg-muted p-2">{testIP} → {testResult}</p>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
