"use client";

import { useState } from "react";
import { Download, FileText, Check, ExternalLink } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useAuth } from "@/context/auth";
import { toast } from "sonner";

function getApiBase() {
  if (process.env.NEXT_PUBLIC_API_URL) return process.env.NEXT_PUBLIC_API_URL;
  if (typeof window !== "undefined") return `http://${window.location.hostname}:8080`;
  return "http://localhost:8080";
}

const exportTypes = [
  {
    id: "manufacturers",
    name: "Manufacturers",
    description: "MikroTik manufacturer entry",
    netboxPath: "Organization > Manufacturers",
    order: 1,
  },
  {
    id: "device_types",
    name: "Device Types",
    description: "Hardware models (RB5009, CCR2004, CRS326, etc.)",
    netboxPath: "Organization > Device Types",
    order: 2,
  },
  {
    id: "device_roles",
    name: "Device Roles",
    description: "Router, Switch, Access Point roles",
    netboxPath: "Organization > Device Roles",
    order: 3,
  },
  {
    id: "devices",
    name: "Devices",
    description: "All managed MikroTik devices with role, type, status, IP",
    netboxPath: "Devices > Devices",
    order: 4,
  },
  {
    id: "interfaces",
    name: "Interfaces",
    description: "All device interfaces with type, MAC, MTU",
    netboxPath: "Devices > Interfaces",
    order: 5,
  },
  {
    id: "ip_addresses",
    name: "IP Addresses",
    description: "Management IPs for all devices",
    netboxPath: "IPAM > IP Addresses",
    order: 6,
  },
  {
    id: "cables",
    name: "Cables",
    description: "Physical connections between devices (from topology discovery)",
    netboxPath: "Connections > Cables",
    order: 7,
  },
];

export default function ExportPage() {
  const { token } = useAuth();
  const [downloading, setDownloading] = useState<Set<string>>(new Set());
  const [downloaded, setDownloaded] = useState<Set<string>>(new Set());

  const downloadCSV = async (type: string) => {
    if (!token) return;
    setDownloading((prev) => new Set(prev).add(type));
    try {
      const res = await fetch(`${getApiBase()}/api/v1/netbox/export/${type}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
      if (!res.ok) throw new Error("Download failed");
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `netbox-${type}.csv`;
      a.click();
      URL.revokeObjectURL(url);
      setDownloaded((prev) => new Set(prev).add(type));
      toast.success(`Downloaded ${type}.csv`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Download failed");
    } finally {
      setDownloading((prev) => {
        const next = new Set(prev);
        next.delete(type);
        return next;
      });
    }
  };

  const downloadAll = async () => {
    for (const t of exportTypes) {
      await downloadCSV(t.id);
    }
  };

  const downloadJSON = async () => {
    if (!token) return;
    try {
      const res = await fetch(`${getApiBase()}/api/v1/netbox/export`, {
        headers: { Authorization: `Bearer ${token}` },
      });
      if (!res.ok) throw new Error("Download failed");
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = "netbox-export.json";
      a.click();
      URL.revokeObjectURL(url);
      toast.success("Downloaded netbox-export.json");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Download failed");
    }
  };

  return (
    <div className="space-y-6 max-w-3xl">
      <div>
        <h1 className="text-2xl font-bold">NetBox Export</h1>
        <p className="text-sm text-muted-foreground mt-1">
          Generate CSV files compatible with{" "}
          <a href="https://netbox.dev" target="_blank" rel="noopener" className="underline">NetBox</a>{" "}
          bulk import. Download individually in the correct order, or all at once.
        </p>
      </div>

      <div className="flex gap-3">
        <Button onClick={downloadAll} variant="outline">
          <Download className="mr-2 h-4 w-4" />
          Download All CSVs
        </Button>
        <Button onClick={downloadJSON} variant="outline">
          <FileText className="mr-2 h-4 w-4" />
          Download as JSON
        </Button>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Import Order</CardTitle>
          <p className="text-xs text-muted-foreground">
            Import into NetBox in this order. Each step depends on the previous one.
          </p>
        </CardHeader>
        <CardContent className="space-y-2">
          {exportTypes.map((t) => (
            <div key={t.id} className="flex items-center gap-4 rounded-lg border p-3 hover:bg-muted/50 transition-colors">
              <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-primary/10 text-primary text-xs font-bold">
                {downloaded.has(t.id) ? <Check className="h-4 w-4" /> : t.order}
              </div>
              <div className="flex-1 min-w-0">
                <div className="flex items-center gap-2">
                  <p className="font-medium text-sm">{t.name}</p>
                  <Badge variant="secondary" className="text-[10px]">{t.netboxPath}</Badge>
                </div>
                <p className="text-xs text-muted-foreground">{t.description}</p>
              </div>
              <Button
                size="sm"
                variant={downloaded.has(t.id) ? "secondary" : "outline"}
                onClick={() => downloadCSV(t.id)}
                disabled={downloading.has(t.id)}
              >
                {downloading.has(t.id) ? (
                  "..."
                ) : downloaded.has(t.id) ? (
                  <><Check className="mr-1 h-3 w-3" />Done</>
                ) : (
                  <><Download className="mr-1 h-3 w-3" />CSV</>
                )}
              </Button>
            </div>
          ))}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">How to Import</CardTitle>
        </CardHeader>
        <CardContent className="text-sm space-y-2 text-muted-foreground">
          <p>1. Download each CSV file in order (or use "Download All")</p>
          <p>2. In NetBox, navigate to the section shown in the badge (e.g. <strong>Organization → Manufacturers</strong>)</p>
          <p>3. Click the <strong>Import</strong> button (top right)</p>
          <p>4. Paste the CSV content or upload the file</p>
          <p>5. Repeat for each file in order</p>
          <p className="pt-2 text-xs">
            Note: Import <strong>Manufacturers</strong> and <strong>Device Types</strong> before <strong>Devices</strong>,
            and <strong>Devices</strong> + <strong>Interfaces</strong> before <strong>Cables</strong>.
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
