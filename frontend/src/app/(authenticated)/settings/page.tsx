"use client";

import { useEffect, useState, useCallback } from "react";
import { Plus, Trash2, Power, PowerOff, Save, Moon, Sun, Pencil, Eraser, Download, Upload } from "lucide-react";
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
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Label } from "@/components/ui/label";
import { useAuth } from "@/context/auth";
import { api, type DNSServer, type FlowStatus } from "@/lib/api";
import { toast } from "sonner";

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

const offlineThresholdOptions = [
  { label: "30s", value: "30" },
  { label: "1m", value: "60" },
  { label: "2m", value: "120" },
  { label: "3m", value: "180" },
  { label: "5m", value: "300" },
  { label: "10m", value: "600" },
  { label: "15m", value: "900" },
];

const infoIntervalOptions = [
  { label: "5m", value: "300" },
  { label: "15m", value: "900" },
  { label: "30m", value: "1800" },
  { label: "1h", value: "3600" },
  { label: "3h", value: "10800" },
  { label: "6h", value: "21600" },
  { label: "12h", value: "43200" },
  { label: "24h", value: "86400" },
];

const tcnStormThresholdOptions = [
  { label: "10", value: "10" },
  { label: "20", value: "20" },
  { label: "30", value: "30" },
  { label: "50", value: "50" },
  { label: "100", value: "100" },
  { label: "200", value: "200" },
];

const connectivityIntervalOptions = [
  { label: "10s", value: "10" },
  { label: "15s", value: "15" },
  { label: "30s", value: "30" },
  { label: "1m", value: "60" },
  { label: "2m", value: "120" },
  { label: "5m", value: "300" },
];

const speedtestIntervalOptions = [
  { label: "1h", value: "3600" },
  { label: "3h", value: "10800" },
  { label: "6h", value: "21600" },
  { label: "12h", value: "43200" },
  { label: "24h", value: "86400" },
];

const portStatsIntervalOptions = [
  { label: "5s", value: "5" },
  { label: "10s", value: "10" },
  { label: "15s", value: "15" },
  { label: "30s", value: "30" },
  { label: "1m", value: "60" },
  { label: "2m", value: "120" },
  { label: "5m", value: "300" },
];

const portHostsIntervalOptions = [
  { label: "1m", value: "60" },
  { label: "2m", value: "120" },
  { label: "5m", value: "300" },
  { label: "10m", value: "600" },
  { label: "15m", value: "900" },
  { label: "30m", value: "1800" },
  { label: "1h", value: "3600" },
];

const flowTopNOptions = ["20", "50", "100", "200"];
const flowTopNHourlyOptions = ["50", "100", "200", "500"];

// "Listening on udp/2055 · 2 exporters" / "Off — set MIKROTIK_NMS_FLOW_LISTEN".
function flowStatusLine(s: FlowStatus | null | undefined): string {
  if (s === undefined) return "Checking the flow collector…";
  if (s === null) return "Flow collector status unavailable.";
  if (!s.enabled) return "Off — set MIKROTIK_NMS_FLOW_LISTEN in the backend environment and restart the backend.";
  const ports = Array.from(new Set(s.listen.map((l) => /:(\d+)$/.exec(l)?.[1]).filter(Boolean)));
  const n = s.exporters.length;
  const listen = ports.length ? `Listening on udp/${ports.join(", udp/")}` : "Listening";
  const errs = s.listen_errors.length ? ` · ${s.listen_errors.length} listen error${s.listen_errors.length === 1 ? "" : "s"}` : "";
  return `${listen} · ${n} exporter${n === 1 ? "" : "s"}${errs}`;
}

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
  { key: "network_health_interval", label: "Network Health", description: "How often to poll bridge / STP state and check for loops" },
  { key: "firmware_interval", label: "Firmware Check", description: "How often to check for RouterOS updates" },
];

export default function SettingsPage() {
  const { token } = useAuth();
  const [settings, setSettings] = useState<Record<string, string>>({});
  const [dirty, setDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  // Suffixes (>=2) of the extra OPNsense sources shown below the primary one.
  // opnsense_* is the primary; opnsenseN_* are additional sites.
  const [extraOpnsense, setExtraOpnsense] = useState<number[]>([]);
  // Per-source validation result, keyed by suffix ("" = primary, "2".. = extras).
  const [opnsenseTest, setOpnsenseTest] = useState<Record<string, { ok: boolean; msg: string } | "loading">>({});
  const [otelTest, setOtelTest] = useState<{ ok: boolean; msg: string } | "loading" | null>(null);
  const [mailTo, setMailTo] = useState("");
  const [mailTest, setMailTest] = useState<{ ok: boolean; msg: string } | "loading" | null>(null);

  // DNS state
  const [dnsServers, setDnsServers] = useState<DNSServer[]>([]);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editingDNS, setEditingDNS] = useState<DNSServer | null>(null);
  const [form, setForm] = useState({ name: "", address: "", port: "53" });
  const [testIP, setTestIP] = useState("");
  const [testResult, setTestResult] = useState<string | null>(null);

  // Flow collector status line (undefined = loading, null = unavailable).
  const [flowStatus, setFlowStatus] = useState<FlowStatus | null | undefined>(undefined);

  // History purge state
  const [purgeTargets, setPurgeTargets] = useState({
    wifi: false,
    clients: false,
    network_health: false,
    traffic: false,
    flows: false,
  });
  const [purgeAgeDays, setPurgeAgeDays] = useState("0");
  const [purgeConfirmOpen, setPurgeConfirmOpen] = useState(false);
  const [purging, setPurging] = useState(false);
  const purgeAny = Object.values(purgeTargets).some(Boolean);

  // Backup/restore
  const [restoring, setRestoring] = useState(false);
  const [backing, setBacking] = useState(false);

  const downloadBlob = (blob: Blob, suggestedName: string) => {
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = suggestedName;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  };

  const handleDownloadTable = async (table: string) => {
    if (!token) return;
    try {
      const blob = await api.admin.downloadExport(token, table);
      downloadBlob(blob, `${table}-${new Date().toISOString().slice(0, 10)}.json`);
      toast.success(`Downloaded ${table}`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Export failed");
    }
  };

  const handleDownloadFullBackup = async () => {
    if (!token) return;
    setBacking(true);
    try {
      const blob = await api.admin.downloadFullBackup(token);
      downloadBlob(blob, `mikrotik-nms-backup-${new Date().toISOString().slice(0, 19).replace(/:/g, "")}.json`);
      toast.success("Full backup downloaded");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Backup failed");
    } finally {
      setBacking(false);
    }
  };

  const handleImportTable = async (table: string, file: File) => {
    if (!token) return;
    try {
      const res = await api.admin.importTable(token, table, file);
      toast.success(`${table}: ${res.inserted} inserted, ${res.skipped} skipped`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Import failed");
    }
  };

  const handleRestoreFullBackup = async (file: File) => {
    if (!token) return;
    setRestoring(true);
    try {
      const res = await api.admin.restoreFullBackup(token, file);
      const total = Object.entries(res.tables)
        .map(([t, n]) => `${t}: ${n.inserted}/${n.inserted + n.skipped}`)
        .join(", ");
      toast.success(`Restored — ${total}`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Restore failed");
    } finally {
      setRestoring(false);
    }
  };

  const handlePurge = async () => {
    if (!token || !purgeAny) return;
    setPurging(true);
    try {
      const res = await api.admin.purgeHistory(token, {
        ...purgeTargets,
        older_than_days: parseInt(purgeAgeDays, 10) || 0,
      });
      const totals = Object.entries(res.deleted)
        .map(([t, n]) => `${t}: ${n}`)
        .join(", ");
      toast.success(`Purged — ${totals || "nothing matched"}`);
      setPurgeConfirmOpen(false);
      setPurgeTargets({ wifi: false, clients: false, network_health: false, traffic: false, flows: false });
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Purge failed");
    } finally {
      setPurging(false);
    }
  };

  const loadSettings = useCallback(() => {
    if (!token) return;
    api.settings.get(token).then((s) => {
      setSettings(s);
      // Discover configured extra OPNsense sources (opnsenseN_* with any value),
      // merging with any unsaved entry the user just added locally.
      const found: number[] = [];
      for (const k of Object.keys(s)) {
        const m = /^opnsense(\d+)_url$/.exec(k);
        if (!m) continue;
        const n = Number(m[1]);
        if (n >= 2 && (s[`opnsense${n}_url`] || s[`opnsense${n}_api_key`] || s[`opnsense${n}_api_secret`])) {
          found.push(n);
        }
      }
      setExtraOpnsense((prev) => Array.from(new Set([...prev, ...found])).sort((a, b) => a - b));
    }).catch(console.error);
  }, [token]);

  const addOpnsense = () => {
    const next = (extraOpnsense.length ? Math.max(...extraOpnsense) : 1) + 1;
    setExtraOpnsense((prev) => [...prev, next]);
  };

  const removeOpnsense = (n: number) => {
    ["url", "api_key", "api_secret", "verify_tls"].forEach((f) => updateSetting(`opnsense${n}_${f}`, ""));
    setExtraOpnsense((prev) => prev.filter((x) => x !== n));
    setOpnsenseTest((prev) => { const c = { ...prev }; delete c[String(n)]; return c; });
  };

  // Validate the credentials currently in the form (saved or unsaved) against
  // OPNsense — confirms the key/secret and reachability before saving.
  const testOpnsense = async (suffix: string) => {
    if (!token) return;
    setOpnsenseTest((p) => ({ ...p, [suffix]: "loading" }));
    try {
      const r = await api.settings.testOpnsense(token, {
        url: settings[`opnsense${suffix}_url`] || "",
        api_key: settings[`opnsense${suffix}_api_key`] || "",
        api_secret: settings[`opnsense${suffix}_api_secret`] || "",
        verify_tls: settings[`opnsense${suffix}_verify_tls`] === "true",
      });
      setOpnsenseTest((p) => ({ ...p, [suffix]: { ok: r.ok, msg: r.message } }));
    } catch (e) {
      setOpnsenseTest((p) => ({ ...p, [suffix]: { ok: false, msg: e instanceof Error ? e.message : "request failed" } }));
    }
  };

  // Validate button + inline result, shared by the primary and extra cards.
  const opnsenseValidate = (suffix: string) => {
    const t = opnsenseTest[suffix];
    return (
      <div className="flex items-center gap-3">
        <Button variant="outline" size="sm" disabled={t === "loading"} onClick={() => testOpnsense(suffix)}>
          {t === "loading" ? "Validating…" : "Validate"}
        </Button>
        {t && t !== "loading" && (
          <span className={`text-xs ${t.ok ? "text-green-600" : "text-red-600"}`}>
            {t.ok ? "✓ " : "✗ "}{t.msg}
          </span>
        )}
      </div>
    );
  };

  // Verify the OTLP endpoint (saved or unsaved) by exporting a test span before
  // saving + restarting the backend.
  const testOtel = async () => {
    if (!token) return;
    setOtelTest("loading");
    try {
      const r = await api.settings.testOtel(token, {
        endpoint: settings.otel_endpoint || "",
        protocol: settings.otel_protocol || "grpc",
        insecure: settings.otel_insecure !== "false",
        headers: settings.otel_headers || "",
        service_name: settings.otel_service_name || "mikrotik-nms",
      });
      setOtelTest({ ok: r.ok, msg: r.message });
    } catch (e) {
      setOtelTest({ ok: false, msg: e instanceof Error ? e.message : "request failed" });
    }
  };

  // Send a test email using the current form values (saved or unsaved). Empty
  // fields fall back to the saved/env SMTP config on the backend.
  const testMail = async () => {
    if (!token || !mailTo.trim()) return;
    setMailTest("loading");
    try {
      const r = await api.settings.testMail(token, {
        to: mailTo.trim(),
        host: settings.smtp_host || "",
        port: settings.smtp_port || "",
        user: settings.smtp_user || "",
        password: settings.smtp_password || "",
        from: settings.smtp_from_address || "",
        tls_mode: settings.smtp_tls_mode || "starttls",
        skip_verify: settings.smtp_tls_skip_verify === "true",
      });
      setMailTest({ ok: r.ok, msg: r.message });
    } catch (e) {
      setMailTest({ ok: false, msg: e instanceof Error ? e.message : "request failed" });
    }
  };

  const loadDNS = useCallback(() => {
    if (!token) return;
    api.dns.list(token).then(setDnsServers).catch(console.error);
  }, [token]);

  useEffect(() => { loadSettings(); loadDNS(); }, [loadSettings, loadDNS]);

  useEffect(() => {
    if (!token) return;
    let alive = true;
    api.flows
      .status(token)
      .then((s) => alive && setFlowStatus(s))
      .catch(() => alive && setFlowStatus(null));
    return () => {
      alive = false;
    };
  }, [token]);

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
  const openEditDNS = (srv: DNSServer) => {
    setEditingDNS(srv);
    setForm({ name: srv.name, address: srv.address, port: String(srv.port) });
    setDialogOpen(true);
  };

  const openAddDNS = () => {
    setEditingDNS(null);
    setForm({ name: "", address: "", port: "53" });
    setDialogOpen(true);
  };

  const handleSubmitDNS = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!token) return;
    try {
      if (editingDNS) {
        await api.dns.update(token, editingDNS.id, { name: form.name, address: form.address, port: parseInt(form.port) || 53, enabled: editingDNS.enabled });
        toast.success("DNS server updated");
      } else {
        await api.dns.create(token, { name: form.name, address: form.address, port: parseInt(form.port) || 53 });
        toast.success("DNS server added");
      }
      setDialogOpen(false);
      setEditingDNS(null);
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
      <div className="sticky top-0 z-20 flex items-center justify-between border-b bg-background py-4">
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

      {/* Device Status */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Device Status</CardTitle>
          <p className="text-xs text-muted-foreground">
            How long a device may stay unreachable before it&apos;s reported offline. Shorter values
            react faster; longer values avoid flapping on a single missed poll. Takes effect on the
            next health poll — no restart needed.
          </p>
        </CardHeader>
        <CardContent>
          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Offline threshold</p>
              <p className="text-xs text-muted-foreground">
                After a device stops responding to the liveness ping it shows as &quot;not responding&quot;,
                then flips to offline once it&apos;s been unreachable this long.
              </p>
            </div>
            <select
              className="flex h-8 w-28 rounded-md border bg-transparent px-2 text-sm"
              value={settings.offline_threshold_seconds || "120"}
              onChange={(e) => updateSetting("offline_threshold_seconds", e.target.value)}
            >
              {offlineThresholdOptions.map((opt) => (
                <option key={opt.value} value={opt.value}>{opt.label}</option>
              ))}
            </select>
          </div>

          <Separator />

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Info refresh interval</p>
              <p className="text-xs text-muted-foreground">
                How often to refresh full device details (CPU, memory, uptime, version, board, interfaces).
                Online/offline status is checked far more often with a lightweight ping; this heavier refresh
                runs rarely since most details rarely change. Cached values are shown between refreshes.
              </p>
            </div>
            <select
              className="flex h-8 w-28 rounded-md border bg-transparent px-2 text-sm"
              value={settings.info_interval || "3600"}
              onChange={(e) => updateSetting("info_interval", e.target.value)}
            >
              {infoIntervalOptions.map((opt) => (
                <option key={opt.value} value={opt.value}>{opt.label}</option>
              ))}
            </select>
          </div>
        </CardContent>
      </Card>

      {/* Auto-follow IP changes */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Auto-follow IP changes</CardTitle>
          <p className="text-xs text-muted-foreground">
            Automatically re-point a managed device to a new IP discovered via neighbor discovery (MNDP/CDP),
            keyed by the device&apos;s MAC. A move is only committed after an authenticated session to the new IP
            confirms the same board and version, so a forged discovery packet cannot hijack a device. Off by default.
          </p>
        </CardHeader>
        <CardContent>
          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Enable auto-follow</p>
              <p className="text-xs text-muted-foreground">
                Applies on the next topology poll. Moves are recorded as &quot;IP address changed&quot; events on the Network Health page.
              </p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => updateSetting("auto_follow_ip", settings.auto_follow_ip === "true" ? "false" : "true")}
            >
              {settings.auto_follow_ip === "true" ? "On" : "Off"}
            </Button>
          </div>
        </CardContent>
      </Card>

      {/* Port Monitoring */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Port Monitoring</CardTitle>
          <p className="text-xs text-muted-foreground">
            Detect port deactivation, link drops, and link flapping on every monitored device.
            Events appear on the Network Health page; thresholds and the interface filter take effect on the next poll cycle.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Enable port monitoring</p>
              <p className="text-xs text-muted-foreground">When off, only bridge/STP signals are tracked.</p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => updateSetting("port_monitor_enabled", settings.port_monitor_enabled === "true" ? "false" : "true")}
            >
              {settings.port_monitor_enabled === "false" ? "Off" : "On"}
            </Button>
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Interface filter</p>
              <p className="text-xs text-muted-foreground">
                Comma-separated type/name prefixes to monitor. Empty or <code>all</code> means every interface.
                Default: <code>ether,sfp,wlan,bridge,vlan</code>.
              </p>
            </div>
            <Input
              className="w-64"
              value={settings.port_monitor_filter ?? ""}
              onChange={(e) => updateSetting("port_monitor_filter", e.target.value)}
              placeholder="ether,sfp,wlan,bridge,vlan"
            />
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Flap threshold</p>
              <p className="text-xs text-muted-foreground">Transitions within the window required to fire a critical port-flap event.</p>
            </div>
            <Input
              type="number"
              min={2}
              max={50}
              className="w-24"
              value={settings.port_flap_threshold ?? "3"}
              onChange={(e) => updateSetting("port_flap_threshold", e.target.value)}
            />
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Flap window</p>
              <p className="text-xs text-muted-foreground">Sliding window in seconds used to count transitions.</p>
            </div>
            <Input
              type="number"
              min={30}
              max={3600}
              className="w-24"
              value={settings.port_flap_window_seconds ?? "300"}
              onChange={(e) => updateSetting("port_flap_window_seconds", e.target.value)}
            />
          </div>
        </CardContent>
      </Card>

      {/* Network Health */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Network Health</CardTitle>
          <p className="text-xs text-muted-foreground">
            Loop / storm detection tuning. Higher thresholds reduce false positives from routine STP
            topology changes. Takes effect on the next network-health poll cycle — no restart needed.
          </p>
        </CardHeader>
        <CardContent>
          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">TCN storm threshold</p>
              <p className="text-xs text-muted-foreground">
                Topology-change increase within a single poll required to raise a TCN-storm event.
                Deltas at or above 100 are escalated to critical regardless of this setting.
              </p>
            </div>
            <select
              className="flex h-8 w-28 rounded-md border bg-transparent px-2 text-sm"
              value={settings.tcn_storm_threshold || "30"}
              onChange={(e) => updateSetting("tcn_storm_threshold", e.target.value)}
            >
              {tcnStormThresholdOptions.map((opt) => (
                <option key={opt.value} value={opt.value}>{opt.label}</option>
              ))}
            </select>
          </div>
        </CardContent>
      </Card>

      {/* Connectivity monitoring */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Connectivity monitoring</CardTitle>
          <p className="text-xs text-muted-foreground">
            Ping probes for internet targets and watched clients, run from your RouterOS devices
            (Connectivity page). Changes apply on the next poll cycle — no restart needed.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Probe interval</p>
              <p className="text-xs text-muted-foreground">How often each enabled target is probed.</p>
            </div>
            <select
              className="flex h-8 w-28 rounded-md border bg-transparent px-2 text-sm"
              value={settings.connectivity_interval || "30"}
              onChange={(e) => updateSetting("connectivity_interval", e.target.value)}
            >
              {connectivityIntervalOptions.map((opt) => (
                <option key={opt.value} value={opt.value}>{opt.label}</option>
              ))}
            </select>
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Pings per probe</p>
              <p className="text-xs text-muted-foreground">
                ICMP pings sent per probe (1–10). Loss, average RTT and jitter are computed across these.
              </p>
            </div>
            <Input
              type="number"
              min={1}
              max={10}
              className="w-24"
              value={settings.connectivity_ping_count ?? "5"}
              onChange={(e) => updateSetting("connectivity_ping_count", e.target.value)}
            />
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Speed test interval</p>
              <p className="text-xs text-muted-foreground">
                How often scheduled speed tests run. Tests run sequentially (one device at a time) so
                parallel downloads don&apos;t skew each other&apos;s results.
              </p>
            </div>
            <select
              className="flex h-8 w-28 rounded-md border bg-transparent px-2 text-sm"
              value={settings.speedtest_interval || "21600"}
              onChange={(e) => updateSetting("speedtest_interval", e.target.value)}
            >
              {speedtestIntervalOptions.map((opt) => (
                <option key={opt.value} value={opt.value}>{opt.label}</option>
              ))}
            </select>
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Auto-traceroute loss threshold</p>
              <p className="text-xs text-muted-foreground">
                When an internet probe sees at least this much packet loss, capture a traceroute
                automatically (0 = off).
              </p>
            </div>
            <Input
              type="number"
              min={0}
              max={100}
              className="w-24"
              value={settings.traceroute_loss_threshold ?? "50"}
              onChange={(e) => updateSetting("traceroute_loss_threshold", e.target.value)}
            />
          </div>
        </CardContent>
      </Card>

      {/* Traffic analytics */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Traffic analytics</CardTitle>
          <p className="text-xs text-muted-foreground">
            Fleet-wide port counters and bridge host tables. Interval and retention changes apply on the next cycle — no restart needed.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Port counter interval</p>
              <p className="text-xs text-muted-foreground">
                How often every online device&apos;s interface counters are read to compute port rates
                (Traffic page, top talkers, flows).
              </p>
            </div>
            <select
              className="flex h-8 w-28 rounded-md border bg-transparent px-2 text-sm"
              value={settings.port_stats_interval || "15"}
              onChange={(e) => updateSetting("port_stats_interval", e.target.value)}
            >
              {portStatsIntervalOptions.map((opt) => (
                <option key={opt.value} value={opt.value}>{opt.label}</option>
              ))}
            </select>
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Bridge host table interval</p>
              <p className="text-xs text-muted-foreground">
                How often each device&apos;s bridge FDB is read to learn which clients sit behind which port.
              </p>
            </div>
            <select
              className="flex h-8 w-28 rounded-md border bg-transparent px-2 text-sm"
              value={settings.port_hosts_interval || "300"}
              onChange={(e) => updateSetting("port_hosts_interval", e.target.value)}
            >
              {portHostsIntervalOptions.map((opt) => (
                <option key={opt.value} value={opt.value}>{opt.label}</option>
              ))}
            </select>
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">1-minute history (days)</p>
              <p className="text-xs text-muted-foreground">
                Retention for per-port 1-minute buckets (1–90, default 2). The Traffic page reads at most the
                last 24h of these (ranges up to 24h); 7d and 30d use the hourly rollups. They are the bulk of the
                database size — roughly 60 MB per day on a ~200-port network — so keep this short.
              </p>
            </div>
            <Input
              type="number"
              min={1}
              max={90}
              className="w-24"
              value={settings.port_stats_1m_days ?? "2"}
              onChange={(e) => updateSetting("port_stats_1m_days", e.target.value)}
            />
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Hourly history (days)</p>
              <p className="text-xs text-muted-foreground">
                Retention for per-port hourly rollups (7–1825, default 365) used by the 7d and 30d ranges.
              </p>
            </div>
            <Input
              type="number"
              min={7}
              max={1825}
              className="w-24"
              value={settings.port_stats_1h_days ?? "365"}
              onChange={(e) => updateSetting("port_stats_1h_days", e.target.value)}
            />
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Forget stale port hosts after (days)</p>
              <p className="text-xs text-muted-foreground">
                Bridge host entries not seen for this long are removed (1–365).
              </p>
            </div>
            <Input
              type="number"
              min={1}
              max={365}
              className="w-24"
              value={settings.port_hosts_stale_days ?? "7"}
              onChange={(e) => updateSetting("port_hosts_stale_days", e.target.value)}
            />
          </div>
        </CardContent>
      </Card>

      {/* Flow collector */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Flow collector (NetFlow / IPFIX)</CardTitle>
          <p className="text-xs font-medium">{flowStatusLine(flowStatus)}</p>
          <p className="text-xs text-muted-foreground">
            Measured per-conversation traffic from devices that export flows (Traffic → Flows → Measured). These settings
            apply within 30 s — no restart needed. The UDP listen address itself is the <code>MIKROTIK_NMS_FLOW_LISTEN</code>{" "}
            environment variable and needs a restart.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Conversations kept per minute</p>
              <p className="text-xs text-muted-foreground">
                Top conversations stored per interface pair and minute (default 50); the rest is summed into one
                &quot;other&quot; row, so totals stay exact.
              </p>
            </div>
            <select
              className="flex h-8 w-28 rounded-md border bg-transparent px-2 text-sm"
              value={settings.flow_top_n || "50"}
              onChange={(e) => updateSetting("flow_top_n", e.target.value)}
            >
              {Array.from(new Set([...flowTopNOptions, settings.flow_top_n || "50"])).map((v) => (
                <option key={v} value={v}>{v}</option>
              ))}
            </select>
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Conversations kept per hour</p>
              <p className="text-xs text-muted-foreground">Same for the hourly rollups used by 7d and 30d (default 100).</p>
            </div>
            <select
              className="flex h-8 w-28 rounded-md border bg-transparent px-2 text-sm"
              value={settings.flow_top_n_hourly || "100"}
              onChange={(e) => updateSetting("flow_top_n_hourly", e.target.value)}
            >
              {Array.from(new Set([...flowTopNHourlyOptions, settings.flow_top_n_hourly || "100"])).map((v) => (
                <option key={v} value={v}>{v}</option>
              ))}
            </select>
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Row cap per exporter-minute</p>
              <p className="text-xs text-muted-foreground">
                Upper bound on stored rows per exporter and minute across all interfaces (200–10000, default 1500).
              </p>
            </div>
            <Input
              type="number"
              min={200}
              max={10000}
              className="w-24"
              value={settings.flow_max_rows_per_minute ?? "1500"}
              onChange={(e) => updateSetting("flow_max_rows_per_minute", e.target.value)}
            />
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">1-minute flow history (days)</p>
              <p className="text-xs text-muted-foreground">
                Retention for 1-minute flow buckets (1–14, default 3) — ranges up to 24h. The bulk of the flow data; keep it
                short on small disks.
              </p>
            </div>
            <Input
              type="number"
              min={1}
              max={14}
              className="w-24"
              value={settings.flow_1m_days ?? "3"}
              onChange={(e) => updateSetting("flow_1m_days", e.target.value)}
            />
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Hourly flow history (days)</p>
              <p className="text-xs text-muted-foreground">Retention for hourly flow rollups (7–730, default 90) — the 7d and 30d ranges.</p>
            </div>
            <Input
              type="number"
              min={7}
              max={730}
              className="w-24"
              value={settings.flow_1h_days ?? "90"}
              onChange={(e) => updateSetting("flow_1h_days", e.target.value)}
            />
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Rate limit (datagrams/s per exporter)</p>
              <p className="text-xs text-muted-foreground">
                Datagrams above this rate are dropped and counted (100–50000, default 2000).
              </p>
            </div>
            <Input
              type="number"
              min={100}
              max={50000}
              className="w-24"
              value={settings.flow_rate_limit_pps ?? "2000"}
              onChange={(e) => updateSetting("flow_rate_limit_pps", e.target.value)}
            />
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Accept managed devices automatically</p>
              <p className="text-xs text-muted-foreground">
                Flows from a managed device&apos;s own address are accepted as an exporter without an admin adding it. Off:
                they are listed as unknown senders until added.
              </p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() =>
                updateSetting("flow_auto_accept_devices", settings.flow_auto_accept_devices === "false" ? "true" : "false")
              }
            >
              {settings.flow_auto_accept_devices === "false" ? "Off" : "On"}
            </Button>
          </div>

          <div className="space-y-1">
            <p className="font-medium text-sm">Internal prefixes</p>
            <p className="text-xs text-muted-foreground">
              Extra addresses to treat as inside your network (CIDR or IP, comma-separated). Device addresses and ARP/DHCP
              hosts are already internal.
            </p>
            <Textarea
              rows={2}
              className="font-mono text-xs"
              placeholder="10.20.0.0/16, 192.168.50.0/24"
              value={settings.flow_internal_prefixes ?? ""}
              onChange={(e) => updateSetting("flow_internal_prefixes", e.target.value)}
            />
          </div>

          <div className="space-y-1">
            <p className="font-medium text-sm">External prefixes</p>
            <p className="text-xs text-muted-foreground">
              Addresses to always treat as outside, even when a device has an address in them (CIDR or IP,
              comma-separated), e.g. LTE transit 192.168.23.0/24.
            </p>
            <Textarea
              rows={2}
              className="font-mono text-xs"
              placeholder="192.168.23.0/24"
              value={settings.flow_external_prefixes ?? ""}
              onChange={(e) => updateSetting("flow_external_prefixes", e.target.value)}
            />
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Resolve external names</p>
              <p className="text-xs text-muted-foreground">
                Sends reverse-DNS queries for the external IPs shown in the flow top lists. Off by default.
              </p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => updateSetting("flow_resolve_ptr", settings.flow_resolve_ptr === "true" ? "false" : "true")}
            >
              {settings.flow_resolve_ptr === "true" ? "On" : "Off"}
            </Button>
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Advertise address</p>
              <p className="text-xs text-muted-foreground">
                The NMS address devices should send flows to, used in the setup commands. Empty = detected automatically.
              </p>
            </div>
            <Input
              className="w-40 font-mono"
              placeholder="auto"
              value={settings.flow_advertise_address ?? ""}
              onChange={(e) => updateSetting("flow_advertise_address", e.target.value)}
            />
          </div>
        </CardContent>
      </Card>

      {/* Kea DHCP */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Kea DHCP</CardTitle>
          <p className="text-xs text-muted-foreground">
            Connect to one or more Kea DHCP Control Agents to resolve WiFi client MACs to IP addresses and hostnames.
          </p>
        </CardHeader>
        <CardContent>
          <div className="flex items-center gap-3">
            <div className="flex-1 space-y-2">
              <Label>Control Agent URL(s)</Label>
              <Textarea
                rows={2}
                value={settings.kea_url || ""}
                onChange={(e) => updateSetting("kea_url", e.target.value)}
                placeholder={"http://192.0.2.81:8000\nhttp://192.0.2.82:8000"}
              />
            </div>
          </div>
          <p className="text-xs text-muted-foreground mt-2">
            One agent per line (or comma-separated) — add extra agents to cover subnets the primary DHCP source
            doesn&apos;t serve. Leave empty to disable. The client discovery poller queries each agent every 15 minutes.
          </p>
        </CardContent>
      </Card>

      {/* OPNsense Kea */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">OPNsense Kea DHCP</CardTitle>
          <p className="text-xs text-muted-foreground">
            Pull DHCP leases from OPNsense&apos;s Kea REST API so WiFi clients on subnets where MikroTik
            isn&apos;t the DHCP server still get IP / hostname enrichment. Generate an API key+secret in
            OPNsense at <code>System &rarr; Access &rarr; Users &rarr; &lt;you&gt; &rarr; API keys</code>.
          </p>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="space-y-1">
            <Label>Base URL</Label>
            <Input
              value={settings.opnsense_url || ""}
              onChange={(e) => updateSetting("opnsense_url", e.target.value)}
              placeholder="https://opnsense.lan:1443"
            />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1">
              <Label>API Key</Label>
              <Input
                value={settings.opnsense_api_key || ""}
                onChange={(e) => updateSetting("opnsense_api_key", e.target.value)}
                placeholder="key"
              />
            </div>
            <div className="space-y-1">
              <Label>API Secret</Label>
              <Input
                type="password"
                value={settings.opnsense_api_secret || ""}
                onChange={(e) => updateSetting("opnsense_api_secret", e.target.value)}
                placeholder="secret"
              />
            </div>
          </div>
          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Verify TLS certificate</p>
              <p className="text-xs text-muted-foreground">
                Disable for OPNsense&apos;s default self-signed cert. Enable once you&apos;ve installed a trusted cert.
              </p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => updateSetting("opnsense_verify_tls", settings.opnsense_verify_tls === "true" ? "false" : "true")}
            >
              {settings.opnsense_verify_tls === "true" ? "On" : "Off"}
            </Button>
          </div>
          {opnsenseValidate("")}
          <p className="text-xs text-muted-foreground">
            The client-discovery poller queries OPNsense every <code>client_discovery_interval</code> (default 15 minutes).
            Leave any field empty to disable the integration.
          </p>
        </CardContent>
      </Card>

      {/* Additional OPNsense sources (remote sites) — add as many as needed */}
      {extraOpnsense.map((n) => (
        <Card key={`opnsense-${n}`}>
          <CardHeader>
            <div className="flex items-center justify-between gap-2">
              <CardTitle className="text-base">OPNsense Kea DHCP — Additional #{n}</CardTitle>
              <Button variant="ghost" size="sm" onClick={() => removeOpnsense(n)}>Remove</Button>
            </div>
            <p className="text-xs text-muted-foreground">
              A further OPNsense to cover a subnet the others don&apos;t serve (e.g. a remote site).
              Queried alongside the rest; leave empty to disable.
            </p>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="space-y-1">
              <Label>Base URL</Label>
              <Input
                value={settings[`opnsense${n}_url`] || ""}
                onChange={(e) => updateSetting(`opnsense${n}_url`, e.target.value)}
                placeholder="https://192.168.80.1:1443"
              />
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1">
                <Label>API Key</Label>
                <Input
                  value={settings[`opnsense${n}_api_key`] || ""}
                  onChange={(e) => updateSetting(`opnsense${n}_api_key`, e.target.value)}
                  placeholder="key"
                />
              </div>
              <div className="space-y-1">
                <Label>API Secret</Label>
                <Input
                  type="password"
                  value={settings[`opnsense${n}_api_secret`] || ""}
                  onChange={(e) => updateSetting(`opnsense${n}_api_secret`, e.target.value)}
                  placeholder="secret"
                />
              </div>
            </div>
            <div className="flex items-center justify-between gap-4">
              <div className="flex-1">
                <p className="font-medium text-sm">Verify TLS certificate</p>
                <p className="text-xs text-muted-foreground">Disable for a self-signed cert.</p>
              </div>
              <Button
                variant="outline"
                size="sm"
                onClick={() => updateSetting(`opnsense${n}_verify_tls`, settings[`opnsense${n}_verify_tls`] === "true" ? "false" : "true")}
              >
                {settings[`opnsense${n}_verify_tls`] === "true" ? "On" : "Off"}
              </Button>
            </div>
            {opnsenseValidate(String(n))}
          </CardContent>
        </Card>
      ))}
      <Button variant="outline" size="sm" onClick={addOpnsense} className="w-fit">
        + Add OPNsense source
      </Button>

      {/* Observability (OpenTelemetry) */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Observability (OpenTelemetry)</CardTitle>
          <p className="text-xs text-muted-foreground">
            Export the data this app collects — device health/CPU/memory, connectivity loss/RTT/jitter, speed tests,
            wifi client counts, loop/flap events — plus HTTP traces and app logs, to an OTLP endpoint (an OpenTelemetry
            Collector gateway that fans out to <code>metrics → dashboards</code>, <code>logs → Loki</code>,{" "}
            <code>traces → Tempo</code>). Takes effect after a backend restart.
          </p>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Enable export</p>
              <p className="text-xs text-muted-foreground">Send metrics, traces and logs to the OTLP endpoint below.</p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => updateSetting("otel_enabled", settings.otel_enabled === "true" ? "false" : "true")}
            >
              {settings.otel_enabled === "true" ? "On" : "Off"}
            </Button>
          </div>

          <div className="space-y-1">
            <Label>OTLP endpoint</Label>
            <Input
              value={settings.otel_endpoint || ""}
              onChange={(e) => updateSetting("otel_endpoint", e.target.value)}
              placeholder="obs.lan:4317  (host:port, no scheme)"
            />
            <p className="text-xs text-muted-foreground">
              The collector gateway. gRPC defaults to <code>:4317</code>, HTTP to <code>:4318</code>.
            </p>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="flex items-center justify-between gap-2">
              <div>
                <p className="font-medium text-sm">Protocol</p>
                <p className="text-xs text-muted-foreground">OTLP transport</p>
              </div>
              <Button
                variant="outline"
                size="sm"
                onClick={() => updateSetting("otel_protocol", settings.otel_protocol === "http" ? "grpc" : "http")}
              >
                {settings.otel_protocol === "http" ? "HTTP" : "gRPC"}
              </Button>
            </div>
            <div className="flex items-center justify-between gap-2">
              <div>
                <p className="font-medium text-sm">Transport security</p>
                <p className="text-xs text-muted-foreground">Plaintext for a no-TLS collector</p>
              </div>
              <Button
                variant="outline"
                size="sm"
                onClick={() => updateSetting("otel_insecure", settings.otel_insecure === "false" ? "true" : "false")}
              >
                {settings.otel_insecure === "false" ? "TLS" : "Insecure"}
              </Button>
            </div>
          </div>

          <div className="space-y-1">
            <Label>Service name</Label>
            <Input
              value={settings.otel_service_name || ""}
              onChange={(e) => updateSetting("otel_service_name", e.target.value)}
              placeholder="mikrotik-nms"
            />
          </div>

          <div className="space-y-1">
            <Label>Headers (optional)</Label>
            <Input
              value={settings.otel_headers || ""}
              onChange={(e) => updateSetting("otel_headers", e.target.value)}
              placeholder="key=value,key2=value2  (e.g. auth / tenant headers)"
            />
          </div>

          <div className="flex items-center gap-3">
            <Button variant="outline" size="sm" disabled={otelTest === "loading"} onClick={testOtel}>
              {otelTest === "loading" ? "Testing…" : "Test connection"}
            </Button>
            {otelTest && otelTest !== "loading" && (
              <span className={`text-xs ${otelTest.ok ? "text-green-600" : "text-red-600"}`}>
                {otelTest.ok ? "✓ " : "✗ "}{otelTest.msg}
              </span>
            )}
          </div>
        </CardContent>
      </Card>

      {/* Email / SMTP (self-service password reset) */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between gap-2">
            <CardTitle className="text-base">Email (SMTP)</CardTitle>
            <span
              className={`text-xs rounded px-2 py-0.5 ${settings.smtp_configured === "true" ? "bg-green-100 text-green-700" : "bg-muted text-muted-foreground"}`}
            >
              {settings.smtp_configured === "true" ? "Configured" : "Not configured"}
            </span>
          </div>
          <p className="text-xs text-muted-foreground">
            Outgoing mail server for self-service password reset. Requires a host, a public base URL
            (<code>MIKROTIK_NMS_PUBLIC_BASE_URL</code>, env), and — for most relays — a username and password.
            Settings here override the backend environment and apply on the next request (no restart). Leave a
            field empty to fall back to the environment value.
          </p>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="grid grid-cols-3 gap-3">
            <div className="col-span-2 space-y-1">
              <Label>Host</Label>
              <Input
                value={settings.smtp_host || ""}
                onChange={(e) => updateSetting("smtp_host", e.target.value)}
                placeholder="mail.example.com"
              />
            </div>
            <div className="space-y-1">
              <Label>Port</Label>
              <Input
                value={settings.smtp_port || ""}
                onChange={(e) => updateSetting("smtp_port", e.target.value)}
                placeholder="587"
              />
            </div>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1">
              <Label>Username</Label>
              <Input
                value={settings.smtp_user || ""}
                onChange={(e) => updateSetting("smtp_user", e.target.value)}
                placeholder="mailbox@example.com"
                autoComplete="off"
              />
            </div>
            <div className="space-y-1">
              <Label>Password</Label>
              <Input
                type="password"
                value={settings.smtp_password || ""}
                onChange={(e) => updateSetting("smtp_password", e.target.value)}
                placeholder="••••••••"
                autoComplete="new-password"
              />
            </div>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1">
              <Label>From address</Label>
              <Input
                value={settings.smtp_from_address || ""}
                onChange={(e) => updateSetting("smtp_from_address", e.target.value)}
                placeholder="nms@example.com"
              />
            </div>
            <div className="space-y-1">
              <Label>TLS mode</Label>
              <select
                className="flex h-8 w-full rounded-md border bg-transparent px-2 text-sm"
                value={settings.smtp_tls_mode || "starttls"}
                onChange={(e) => updateSetting("smtp_tls_mode", e.target.value)}
              >
                <option value="starttls">STARTTLS (port 587)</option>
                <option value="tls">TLS/SSL (port 465)</option>
                <option value="none">None (plaintext)</option>
              </select>
            </div>
          </div>
          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Skip TLS verification</p>
              <p className="text-xs text-muted-foreground">Only for a self-signed relay on a trusted network.</p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => updateSetting("smtp_tls_skip_verify", settings.smtp_tls_skip_verify === "true" ? "false" : "true")}
            >
              {settings.smtp_tls_skip_verify === "true" ? "On" : "Off"}
            </Button>
          </div>
          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Enable password reset</p>
              <p className="text-xs text-muted-foreground">Kill-switch for the self-service reset flow.</p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => updateSetting("pwreset_enabled", settings.pwreset_enabled === "false" ? "true" : "false")}
            >
              {settings.pwreset_enabled === "false" ? "Off" : "On"}
            </Button>
          </div>
          <Separator />
          <div className="space-y-1">
            <Label>Send a test email</Label>
            <div className="flex items-center gap-2">
              <Input
                type="email"
                value={mailTo}
                onChange={(e) => {
                  setMailTo(e.target.value);
                  setMailTest(null);
                }}
                placeholder="you@example.com"
                autoComplete="off"
              />
              <Button
                variant="outline"
                size="sm"
                className="shrink-0"
                disabled={mailTest === "loading" || !mailTo.trim()}
                onClick={testMail}
              >
                {mailTest === "loading" ? "Sending…" : "Send test"}
              </Button>
            </div>
            {mailTest && mailTest !== "loading" && (
              <span className={`text-xs ${mailTest.ok ? "text-green-600" : "text-red-600"}`}>
                {mailTest.ok ? "✓ " : "✗ "}{mailTest.msg}
              </span>
            )}
            <p className="text-xs text-muted-foreground">
              Sends using the values above (no need to save first). Empty fields fall back to the saved/env config.
            </p>
          </div>
          <p className="text-xs text-muted-foreground">
            The password (and any value matching <code>secret</code>/<code>token</code>) is stored masked and never
            shown to non-admins.
          </p>
        </CardContent>
      </Card>

      {/* DNS Servers */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <CardTitle className="text-base">DNS Servers</CardTitle>
            <Button size="sm" onClick={openAddDNS}>
              <Plus className="mr-2 h-3 w-3" />Add Server
            </Button>
            <Dialog open={dialogOpen} onOpenChange={(open) => { setDialogOpen(open); if (!open) setEditingDNS(null); }}>
              <DialogContent>
                <DialogHeader>
                  <DialogTitle>{editingDNS ? "Edit DNS Server" : "Add DNS Server"}</DialogTitle>
                </DialogHeader>
                <form onSubmit={handleSubmitDNS} className="space-y-4">
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
                  <Button type="submit" className="w-full">{editingDNS ? "Save Changes" : "Add DNS Server"}</Button>
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
                        <Button variant="ghost" size="icon" onClick={() => openEditDNS(srv)} title="Edit">
                          <Pencil className="h-4 w-4" />
                        </Button>
                        <Button variant="ghost" size="icon" onClick={() => handleToggleDNS(srv)} title={srv.enabled ? "Disable" : "Enable"}>
                          {srv.enabled ? <PowerOff className="h-4 w-4" /> : <Power className="h-4 w-4" />}
                        </Button>
                        <Button variant="ghost" size="icon" onClick={() => handleDeleteDNS(srv.id)} title="Delete">
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

      {/* Purge History */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Purge History</CardTitle>
          <p className="text-xs text-muted-foreground">
            Delete historical records from the database. Current-state tables (devices, interfaces,
            mac_lookup, bridges, port-state, settings) are <strong>never</strong> touched — they
            re-populate on the next poll cycle.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="grid grid-cols-2 gap-3">
            {[
              { key: "wifi", label: "WiFi events", desc: "wifi_history — join / leave / roam" },
              { key: "clients", label: "Client history", desc: "client_history — DHCP / ARP snapshots" },
              { key: "network_health", label: "Network health events", desc: "loop_events — STP / loop / port-flap" },
              { key: "traffic", label: "Traffic history", desc: "traffic_samples, port_stats_1m, port_stats_1h — interface bps and per-port 1-minute / hourly buckets" },
              { key: "flows", label: "Flow history", desc: "flow_1m, flow_1h — measured flow-export buckets (exporters and views are kept)" },
            ].map((t) => {
              const k = t.key as keyof typeof purgeTargets;
              const checked = purgeTargets[k];
              return (
                <label
                  key={t.key}
                  className={`flex flex-col gap-1 rounded-md border p-3 cursor-pointer hover:bg-muted/50 ${checked ? "border-foreground" : ""}`}
                >
                  <div className="flex items-center gap-2">
                    <input
                      type="checkbox"
                      checked={checked}
                      onChange={(e) => setPurgeTargets((s) => ({ ...s, [k]: e.target.checked }))}
                    />
                    <span className="text-sm font-medium">{t.label}</span>
                  </div>
                  <span className="text-xs text-muted-foreground font-mono">{t.desc}</span>
                </label>
              );
            })}
          </div>

          <div className="flex items-center justify-between gap-4">
            <div className="flex-1">
              <p className="font-medium text-sm">Older than</p>
              <p className="text-xs text-muted-foreground">
                <code>0</code> = delete <strong>everything</strong> from the chosen tables. Any other value
                keeps rows newer than that many days.
              </p>
            </div>
            <select
              className="flex h-8 w-32 rounded-md border bg-transparent px-2 text-sm"
              value={purgeAgeDays}
              onChange={(e) => setPurgeAgeDays(e.target.value)}
            >
              <option value="0">all rows</option>
              <option value="1">&gt; 1 day</option>
              <option value="3">&gt; 3 days</option>
              <option value="7">&gt; 7 days</option>
              <option value="30">&gt; 30 days</option>
              <option value="90">&gt; 90 days</option>
            </select>
          </div>

          <Button
            variant="destructive"
            disabled={!purgeAny || purging}
            onClick={() => setPurgeConfirmOpen(true)}
            className="gap-2"
          >
            <Eraser className="h-4 w-4" />
            {purging ? "Purging…" : "Purge selected"}
          </Button>
        </CardContent>
      </Card>

      {/* Backup & Restore */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Backup &amp; Restore</CardTitle>
          <p className="text-xs text-muted-foreground">
            Full JSON backup of the whole database (config + history), or per-table export/import.
            Imports use <code>INSERT OR IGNORE</code> — rows with an existing primary key are skipped silently.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="rounded-md border p-3 space-y-2">
            <p className="text-sm font-medium">Full backup</p>
            <p className="text-xs text-muted-foreground">
              Devices, users, settings, DNS servers, all history. One JSON file you can stash anywhere.
            </p>
            <div className="flex gap-2 items-center flex-wrap">
              <Button onClick={handleDownloadFullBackup} disabled={backing} className="gap-2">
                <Download className="h-4 w-4" />
                {backing ? "Backing up…" : "Download backup"}
              </Button>
              <label className="inline-flex">
                <input
                  type="file"
                  accept="application/json,.json"
                  className="hidden"
                  onChange={(e) => {
                    const f = e.target.files?.[0];
                    if (f) {
                      handleRestoreFullBackup(f);
                      e.target.value = "";
                    }
                  }}
                />
                <span className={`inline-flex items-center gap-2 px-3 h-9 rounded-md border text-sm cursor-pointer hover:bg-muted ${restoring ? "opacity-50 pointer-events-none" : ""}`}>
                  <Upload className="h-4 w-4" />
                  {restoring ? "Restoring…" : "Restore from file"}
                </span>
              </label>
            </div>
          </div>

          <div>
            <p className="text-sm font-medium mb-2">Per-table</p>
            <div className="space-y-2">
              {[
                { table: "wifi_history", label: "WiFi events" },
                { table: "client_history", label: "Client history" },
                { table: "loop_events", label: "Network health events" },
                { table: "traffic_samples", label: "Traffic samples" },
                { table: "devices", label: "Devices (config)" },
                { table: "app_settings", label: "App settings (config)" },
                { table: "dns_servers", label: "DNS servers (config)" },
              ].map((row) => (
                <div key={row.table} className="flex items-center justify-between gap-3 text-sm">
                  <div className="flex-1">
                    <span className="font-medium">{row.label}</span>
                    <span className="ml-2 font-mono text-xs text-muted-foreground">{row.table}</span>
                  </div>
                  <Button variant="outline" size="sm" onClick={() => handleDownloadTable(row.table)} className="gap-1">
                    <Download className="h-3.5 w-3.5" />
                    Export
                  </Button>
                  <label className="inline-flex">
                    <input
                      type="file"
                      accept="application/json,.json"
                      className="hidden"
                      onChange={(e) => {
                        const f = e.target.files?.[0];
                        if (f) {
                          handleImportTable(row.table, f);
                          e.target.value = "";
                        }
                      }}
                    />
                    <span className="inline-flex items-center gap-1 h-8 px-2.5 rounded-md border text-xs cursor-pointer hover:bg-muted">
                      <Upload className="h-3.5 w-3.5" />
                      Import
                    </span>
                  </label>
                </div>
              ))}
            </div>
          </div>
        </CardContent>
      </Card>

      <Dialog open={purgeConfirmOpen} onOpenChange={setPurgeConfirmOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Purge history?</DialogTitle>
          </DialogHeader>
          <div className="space-y-2 text-sm">
            <p>This will delete records from:</p>
            <ul className="ml-4 list-disc font-mono text-xs">
              {purgeTargets.wifi && <li>wifi_history</li>}
              {purgeTargets.clients && <li>client_history</li>}
              {purgeTargets.network_health && <li>loop_events</li>}
              {purgeTargets.traffic && (
                <>
                  <li>traffic_samples</li>
                  <li>port_stats_1m</li>
                  <li>port_stats_1h</li>
                </>
              )}
              {purgeTargets.flows && (
                <>
                  <li>flow_1m</li>
                  <li>flow_1h</li>
                </>
              )}
            </ul>
            <p>
              {purgeAgeDays === "0"
                ? "Deletes ALL rows from the tables above (cannot be undone)."
                : `Deletes rows older than ${purgeAgeDays} day${purgeAgeDays === "1" ? "" : "s"} from the tables above.`}
            </p>
          </div>
          <div className="flex gap-2 justify-end mt-4">
            <Button variant="outline" onClick={() => setPurgeConfirmOpen(false)}>Cancel</Button>
            <Button variant="destructive" onClick={handlePurge} disabled={purging}>
              {purging ? "Purging…" : "Yes, purge"}
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  );
}
