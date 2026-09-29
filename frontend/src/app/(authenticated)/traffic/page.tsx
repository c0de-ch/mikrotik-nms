"use client";

import { Suspense, useCallback, useEffect, useMemo, useRef } from "react";
import { ArrowLeft, BarChart3, ChevronRight, GitFork, Server } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  useDarkFlag,
  useDevices,
  usePortRoles,
  usePortSnapshot,
  useTrafficParams,
  type TrafficView,
} from "@/components/traffic/hooks";
import { deviceName } from "@/components/traffic/lib";
import { DeviceList } from "@/components/traffic/device-list";
import { DeviceView } from "@/components/traffic/device-view";
import { FlowsTab } from "@/components/traffic/flows/flows-tab";
import { KpiRow } from "@/components/traffic/kpi-row";
import { Notice } from "@/components/traffic/notice";
import { PortDetail } from "@/components/traffic/port-detail";
import { RangePicker } from "@/components/traffic/range-picker";
import { TopTalkers } from "@/components/traffic/top-talkers-table";

// /traffic — fleet-wide port analytics with a URL-addressable drill-down:
// ?view=top|devices|flows &device= &iface= &range= &metric= &dir= &flow= &physical=
// &fsrc=flows|counters &point= &remote=host|app (Flows tab)
// useSearchParams needs a Suspense boundary (Next 16), hence the wrapper.
export default function TrafficPage() {
  return (
    <Suspense fallback={null}>
      <TrafficPageInner />
    </Suspense>
  );
}

function TrafficPageInner() {
  const [params, update] = useTrafficParams();
  const snap = usePortSnapshot();
  const roles = usePortRoles();
  const { devices, byId, loaded, error: devicesError, reload: reloadDevices } = useDevices();
  const dark = useDarkFlag();
  const serverDevices = useMemo(() => new Set(roles.data?.devices.map((d) => d.device_id) ?? []), [roles.data]);

  const nameOf = useCallback(
    (id: string) => {
      const d = byId.get(id);
      if (d) return deviceName(d);
      return roles.data?.devices.find((t) => t.device_id === id)?.name || id.slice(0, 8);
    },
    [byId, roles.data],
  );
  const openPort = useCallback(
    (device: string, iface: string) => update({ view: "devices", device, iface }, { push: true }),
    [update],
  );
  const openDevice = useCallback(
    (device: string) => update({ view: "devices", device, iface: null }, { push: true }),
    [update],
  );

  const { view, device, iface } = params;
  const inDevices = view === "devices";

  // A device id the list doesn't know (added while the page was open, or the
  // list is stale): refetch the list once per id rather than showing a
  // "no longer exists" state for a device the server still reports.
  const refetchedFor = useRef(new Set<string>());
  const unknownDevice = !!device && loaded && !devicesError && !byId.has(device);
  useEffect(() => {
    if (!unknownDevice || !device || refetchedFor.current.has(device)) return;
    refetchedFor.current.add(device);
    reloadDevices();
  }, [unknownDevice, device, reloadDevices]);
  const goUp = () => {
    if (iface) update({ iface: null }, { push: true });
    else update({ device: null }, { push: true });
  };

  const crumbs: { label: string; onClick?: () => void; mono?: boolean }[] = [];
  if (inDevices) {
    crumbs.push({ label: "All devices", onClick: device ? () => update({ device: null, iface: null }, { push: true }) : undefined });
    if (device) crumbs.push({ label: nameOf(device), onClick: iface ? () => update({ iface: null }, { push: true }) : undefined });
    if (device && iface) crumbs.push({ label: iface, mono: true });
  }

  // contain:inline-size keeps wide content (path strip, tables, Sankey) from
  // widening the layout — the sidebar inset has no min-width guard — so those
  // scroll inside their cards instead of the page scrolling sideways.
  return (
    <div className="min-w-0 space-y-4 [contain:inline-size]">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2">
          {inDevices && device && (
            <Button variant="ghost" size="icon" onClick={goUp} aria-label="Up one level">
              <ArrowLeft className="h-4 w-4" />
            </Button>
          )}
          <div className="min-w-0">
            <h1 className="text-2xl font-bold">Traffic</h1>
            {crumbs.length > 1 && (
              <nav aria-label="Breadcrumb" className="flex min-w-0 flex-wrap items-center gap-1 text-sm text-muted-foreground">
                {crumbs.map((c, i) => (
                  <span key={i} className="flex min-w-0 items-center gap-1">
                    {i > 0 && <ChevronRight className="h-3 w-3 shrink-0" />}
                    {c.onClick ? (
                      <button type="button" onClick={c.onClick} className={`truncate transition-colors hover:text-foreground ${c.mono ? "font-mono" : ""}`}>
                        {c.label}
                      </button>
                    ) : (
                      <span className={`truncate text-foreground ${c.mono ? "font-mono" : ""}`}>{c.label}</span>
                    )}
                  </span>
                ))}
              </nav>
            )}
          </div>
        </div>
        <RangePicker value={params.range} onChange={(r) => update({ range: r })} />
      </div>

      {snap.error && !snap.snapshot && <Notice kind="error">Live port counters unavailable: {snap.error}</Notice>}
      {devicesError && (
        <Notice kind="error">
          Could not load devices: {devicesError}.{" "}
          <button type="button" className="font-medium underline underline-offset-2" onClick={reloadDevices}>
            Retry
          </button>
        </Notice>
      )}
      {roles.error && !roles.data && (
        <Notice kind="warn">
          Port roles unavailable — download/upload directions and WAN totals may be wrong ({roles.error}).
        </Notice>
      )}

      <KpiRow
        snapshot={snap.snapshot}
        roles={roles.byKey}
        anchored={roles.data ? roles.data.anchored : null}
        nameOf={nameOf}
        onOpenPort={openPort}
        onOpenDevice={openDevice}
      />

      <Tabs value={view} onValueChange={(v) => update({ view: v as TrafficView, iface: null }, { push: true })}>
        <TabsList>
          <TabsTrigger value="top">
            <BarChart3 className="h-3.5 w-3.5" />
            Top talkers
          </TabsTrigger>
          <TabsTrigger value="devices">
            <Server className="h-3.5 w-3.5" />
            Devices
          </TabsTrigger>
          <TabsTrigger value="flows">
            <GitFork className="h-3.5 w-3.5" />
            Flows
          </TabsTrigger>
        </TabsList>
      </Tabs>

      {view === "top" && (
        <TopTalkers
          params={params}
          update={update}
          snapshot={snap.snapshot}
          roles={roles.byKey}
          nameOf={nameOf}
          onOpenPort={openPort}
          onOpenDevice={openDevice}
        />
      )}

      {inDevices && !device && (
        <DeviceList devices={devices} loaded={loaded} error={devicesError} snapshot={snap.snapshot} onOpenDevice={openDevice} />
      )}

      {inDevices && device && !iface && (
        <DeviceView
          key={device}
          deviceId={device}
          device={byId.get(device)}
          devicesLoaded={loaded}
          devicesError={devicesError}
          knownToServer={serverDevices.has(device)}
          params={params}
          update={update}
          snapshot={snap.snapshot}
          devicePorts={snap.byDevice.get(device) ?? EMPTY}
          roles={roles.byKey}
          nameOf={nameOf}
          dark={dark}
          onOpenPort={openPort}
        />
      )}

      {inDevices && device && iface && (
        <PortDetail
          key={`${device}/${iface}`}
          deviceId={device}
          iface={iface}
          device={byId.get(device)}
          range={params.range}
          snapshot={snap.snapshot}
          byKey={snap.byKey}
          deviceHasLivePorts={(snap.byDevice.get(device)?.length ?? 0) > 0}
          roles={roles.byKey}
          nameOf={nameOf}
          onOpenPort={openPort}
          onOpenDevice={openDevice}
        />
      )}

      {view === "flows" && (
        <FlowsTab params={params} update={update} devices={devices} onOpenPort={openPort} onOpenDevice={openDevice} />
      )}
    </div>
  );
}

const EMPTY: never[] = [];
