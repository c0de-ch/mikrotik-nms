function getApiBase() {
  if (process.env.NEXT_PUBLIC_API_URL) return process.env.NEXT_PUBLIC_API_URL;
  if (typeof window !== "undefined") {
    // `next dev` (any port — it auto-falls-back past 3000) and the stock
    // docker-compose mapping (production build published on :3000) serve the
    // frontend with the Go backend on :8080. Anywhere else the reverse proxy
    // serves /api/* on the same origin, so an empty base (relative URLs)
    // works for every hostname the site is reached under — baking an
    // absolute URL breaks access via any other hostname.
    if (process.env.NODE_ENV === "development" || window.location.port === "3000") {
      return `http://${window.location.hostname}:8080`;
    }
    return "";
  }
  return "http://localhost:8080";
}

// Human-readable description of where API requests go, for error messages.
function describeApiTarget(): string {
  if (process.env.NEXT_PUBLIC_API_URL) {
    return `${process.env.NEXT_PUBLIC_API_URL} (build-time NEXT_PUBLIC_API_URL — rebuild the frontend if this address is wrong)`;
  }
  if (typeof window !== "undefined") return getApiBase() || window.location.origin;
  return getApiBase();
}

export function networkError(): ApiError {
  return new ApiError(
    0,
    `Cannot reach the NMS API at ${describeApiTarget()}. The server may be down or unreachable from this network.`,
  );
}

interface FetchOptions extends RequestInit {
  token?: string;
}

// Deduplicate concurrent refresh attempts
let refreshPromise: Promise<{ access_token: string; refresh_token: string } | null> | null = null;

async function tryRefreshToken(): Promise<{ access_token: string; refresh_token: string } | null> {
  const refreshToken = typeof window !== "undefined" ? localStorage.getItem("refresh_token") : null;
  if (!refreshToken) return null;

  let res: Response;
  try {
    res = await fetch(`${getApiBase()}/api/v1/auth/refresh`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: refreshToken }),
    });
  } catch {
    // Network-level failure is an outage, not an expired session — leave the
    // tokens alone so the session resumes once the API is reachable again.
    return null;
  }

  if (res.ok) {
    const tokens = await res.json().catch(() => null);
    if (tokens) {
      localStorage.setItem("access_token", tokens.access_token);
      localStorage.setItem("refresh_token", tokens.refresh_token);
      if (typeof window !== "undefined") {
        window.dispatchEvent(new CustomEvent("auth:refreshed", { detail: tokens }));
      }
      return tokens;
    }
    return null;
  }

  if (typeof window !== "undefined") {
    window.dispatchEvent(new CustomEvent("auth:expired"));
  }
  return null;
}

async function apiFetch<T>(path: string, options: FetchOptions = {}): Promise<T> {
  const { token, ...fetchOptions } = options;

  const doFetch = async (accessToken?: string) => {
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      ...(fetchOptions.headers as Record<string, string>),
    };
    if (accessToken) {
      headers["Authorization"] = `Bearer ${accessToken}`;
    }
    try {
      return await fetch(`${getApiBase()}/api/v1${path}`, {
        ...fetchOptions,
        headers,
        credentials: "include",
      });
    } catch (err) {
      // Keep caller-driven cancellations distinguishable from outages.
      if (err instanceof DOMException && err.name === "AbortError") throw err;
      throw networkError();
    }
  };

  let res = await doFetch(token);

  // Auto-refresh on 401: deduplicate concurrent refresh attempts
  if (res.status === 401 && token) {
    if (!refreshPromise) {
      refreshPromise = tryRefreshToken().finally(() => { refreshPromise = null; });
    }
    const tokens = await refreshPromise;
    if (tokens) {
      res = await doFetch(tokens.access_token);
    }
  }

  if (!res.ok) {
    const body: { error?: string; code?: string } = await res.json().catch(() => ({}));
    let message = body.error || res.statusText;
    // A reverse-proxy 502/503/504 with no JSON body means the backend behind
    // it is down; statusText is terse on HTTP/1.1 and empty on HTTP/2, so
    // neither is something a user can act on.
    if (!body.error && (res.status === 502 || res.status === 503 || res.status === 504)) {
      message = `The NMS backend is not responding (HTTP ${res.status} from the reverse proxy). It may be restarting — try again shortly.`;
    } else if (!message) {
      message = `Request failed (HTTP ${res.status})`;
    }
    throw new ApiError(res.status, message, body.code);
  }

  // 204 No Content (e.g. DELETE /flows/points/{id}) has no body to parse.
  if (res.status === 204) return undefined as T;
  return res.json();
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    // The server's machine-readable reason, when it sent one (e.g.
    // "reauth_required": the action needs the password typed again).
    public code?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

// Auth
export const api = {
  auth: {
    login: (username: string, password: string) =>
      apiFetch<{ access_token: string; refresh_token: string; expires_at: number }>("/auth/login", {
        method: "POST",
        body: JSON.stringify({ username, password }),
      }),
    setup: (username: string, password: string) =>
      apiFetch<{ access_token: string; refresh_token: string; expires_at: number }>("/auth/setup", {
        method: "POST",
        body: JSON.stringify({ username, password }),
      }),
    refresh: () =>
      apiFetch<{ access_token: string; refresh_token: string; expires_at: number }>("/auth/refresh", {
        method: "POST",
      }),
    refreshWithToken: (refreshToken: string) =>
      apiFetch<{ access_token: string; refresh_token: string; expires_at: number }>("/auth/refresh", {
        method: "POST",
        body: JSON.stringify({ refresh_token: refreshToken }),
      }),
    logout: (token: string) =>
      apiFetch("/auth/logout", { method: "POST", token }),
    me: (token: string) =>
      apiFetch<{ id: string; username: string; role: string }>("/auth/me", { token }),
    // Self-service password reset (public, no Authorization header). The server
    // always answers request-reset with a generic {status:"ok"} so the caller
    // must never branch on the response to reveal whether an account exists.
    requestReset: (username: string) =>
      apiFetch<{ status: string }>("/auth/request-reset", {
        method: "POST",
        body: JSON.stringify({ username }),
      }),
    performReset: (token: string, newPassword: string) =>
      apiFetch<{ status: string }>("/auth/perform-reset", {
        method: "POST",
        body: JSON.stringify({ token, new_password: newPassword }),
      }),
  },

  // Discovery
  discovery: {
    scan: (token: string, duration = 10) =>
      apiFetch<DiscoveredDevice[]>(`/discovery?duration=${duration}`, { token }),
    deep: (token: string, cidr?: string) => {
      const qs = cidr ? `?cidr=${encodeURIComponent(cidr)}` : "";
      return apiFetch<DeepDiscoveredDevice[]>(`/discovery/deep${qs}`, { token });
    },
  },

  // Network clients. The list is the cache the scheduled pollers keep (no
  // device is contacted); refresh pulls their reads forward once, at most
  // every refresh.min_interval_seconds fleet-wide.
  clients: {
    cached: (token: string) => apiFetch<ClientListResult>("/clients/cached", { token }),
    refresh: (token: string) =>
      apiFetch<{ refresh: ClientRefreshStatus }>("/clients/refresh", { method: "POST", token }),
  },

  // Devices
  devices: {
    list: (token: string) => apiFetch<Device[]>("/devices", { token }),
    get: (token: string, id: string) => apiFetch<Device>(`/devices/${id}`, { token }),
    create: (token: string, data: CreateDeviceRequest) =>
      apiFetch<Device>("/devices", { method: "POST", token, body: JSON.stringify(data) }),
    update: (token: string, id: string, data: Partial<CreateDeviceRequest>) =>
      apiFetch<Device>(`/devices/${id}`, { method: "PUT", token, body: JSON.stringify(data) }),
    delete: (token: string, id: string) =>
      apiFetch(`/devices/${id}`, { method: "DELETE", token }),
    interfaces: (token: string, id: string) =>
      apiFetch<DeviceInterface[]>(`/devices/${id}/interfaces`, { token }),
    // Physical ports with their rx/tx from the port-counter snapshot (no
    // device call) — powers the switch port grid on the map.
    ports: (token: string, id: string) =>
      apiFetch<DevicePortsResponse>(`/devices/${id}/ports`, { token }),
    // What a live per-port stream of this device may do for the caller
    // (class caps, admin-only, pause) — checked before subscribing.
    liveLimits: (token: string, id: string) =>
      apiFetch<LiveLimits>(`/devices/${id}/live-limits`, { token }),
    // CPU and memory telemetry next to what the NMS sent the device, from
    // the database (no device call) — the "CPU & NMS load" tab.
    load: (token: string, id: string, range: DeviceLoadRange) =>
      apiFetch<DeviceLoadResponse>(`/devices/${id}/load?range=${range}`, { token }),
    neighbors: (token: string, id: string) =>
      apiFetch<Neighbor[]>(`/devices/${id}/neighbors`, { token }),
    // Live from the device: configured IPv4/IPv6 addresses annotated with the
    // VLAN id when the interface is an /interface/vlan. 409 when offline.
    addresses: (token: string, id: string) =>
      apiFetch<DeviceAddress[]>(`/devices/${id}/addresses`, { token }),
    // Polling controls (admin): pause every poller but liveness, switch to
    // the light (C1) cadences, or override the CPU class with a gentler one.
    pausePolling: (token: string, id: string, hours: number) =>
      apiFetch<Device>(`/devices/${id}/polling/pause`, {
        method: "POST",
        token,
        body: JSON.stringify({ hours }),
      }),
    resumePolling: (token: string, id: string) =>
      apiFetch<Device>(`/devices/${id}/polling/resume`, { method: "POST", token }),
    updatePolling: (token: string, id: string, data: UpdatePollingRequest) =>
      apiFetch<Device>(`/devices/${id}/polling`, { method: "PUT", token, body: JSON.stringify(data) }),
    // Operator actions on the device (pauses, profile and class changes), newest first.
    audit: (token: string, id: string, limit = 20) =>
      apiFetch<DeviceAuditEntry[]>(`/devices/${id}/audit?limit=${limit}`, { token }),
    // What the pollers noticed about the device (another API poller, login
    // failures, …), most recently seen first.
    findings: (token: string, id: string, limit = 50) =>
      apiFetch<DeviceFinding[]>(`/devices/${id}/findings?limit=${limit}`, { token }),
    // The hygiene audit and the diagnose pass: the device's hygiene findings
    // with their fix, the last passes and when the next audit runs. From the
    // database — reading it costs the device nothing.
    hygiene: (token: string, id: string) => apiFetch<DeviceHygiene>(`/devices/${id}/hygiene`, { token }),
    // Admin: start a pass now. One of each kind per device and hour (429),
    // refused while the device is paused, has no API session or already
    // runs a pass (409), and while polling is off (503).
    auditNow: (token: string, id: string) =>
      apiFetch<HygienePass>(`/devices/${id}/hygiene/audit`, { method: "POST", token }),
    diagnoseNow: (token: string, id: string) =>
      apiFetch<HygienePass>(`/devices/${id}/hygiene/diagnose`, { method: "POST", token }),
    // IP auto-follow proposals: a managed device seen by MNDP/CDP at a new
    // address. Pending only unless status is given ("all" or one status).
    addressProposals: (token: string, status?: AddressProposalStatus | "all") =>
      apiFetch<AddressProposal[]>(
        `/devices/address-proposals${status ? `?status=${status}` : ""}`,
        { token },
      ),
    // Admin + a password typed in the last 5 min (wrap in withFreshAuth):
    // logs in at the new address with the stored credential, verifies board,
    // version and MAC, and re-points the device. 200 with status "approved"
    // or "failed"; 409 when no longer pending, stale or the device is paused.
    approveAddressProposal: (token: string, id: number) =>
      apiFetch<AddressProposal>(`/devices/address-proposals/${id}/approve`, { method: "POST", token }),
    dismissAddressProposal: (token: string, id: number) =>
      apiFetch<AddressProposal>(`/devices/address-proposals/${id}/dismiss`, { method: "POST", token }),
  },

  // Topology
  topology: {
    get: (token: string) => apiFetch<TopologyData>("/topology", { token }),
  },

  // Traffic
  traffic: {
    summary: (token: string) =>
      apiFetch<{ device_id: string; rx_bps: number; tx_bps: number }[]>("/traffic/summary", { token }),
    // Per-link throughput from the port-counter snapshot for the map's
    // initial paint; the continuous feed is the "topology.traffic" WS topic.
    links: (token: string) =>
      apiFetch<LinkTrafficResponse>("/traffic/links", { token }),
    get: (token: string, deviceId: string, iface: string, from?: string, to?: string) => {
      const params = new URLSearchParams();
      if (from) params.set("from", from);
      if (to) params.set("to", to);
      const qs = params.toString() ? `?${params}` : "";
      return apiFetch<TrafficSample[]>(`/traffic/${deviceId}/${iface}${qs}`, { token });
    },

    // Fleet-wide port counters (port-stats collector). Port-specific calls
    // pass the interface as a query param — RouterOS names can contain
    // spaces and slashes. The live feed is the "traffic.ports" WS topic.
    portsLatest: (token: string) =>
      apiFetch<PortSnapshot>("/traffic/ports/latest", { token }),
    portsTop: (
      token: string,
      p: {
        range: TrafficRange;
        metric: TopMetric;
        dir: TopDir;
        limit?: number;
        physical?: boolean;
        device?: string;
        sparkline?: boolean;
      },
    ) => {
      const qs = new URLSearchParams({ range: p.range, metric: p.metric, dir: p.dir });
      if (p.limit) qs.set("limit", String(p.limit));
      if (p.physical === false) qs.set("physical", "0");
      if (p.device) qs.set("device", p.device);
      if (p.sparkline === false) qs.set("sparkline", "0");
      return apiFetch<TopPortsResponse>(`/traffic/ports/top?${qs}`, { token });
    },
    portRoles: (token: string) =>
      apiFetch<PortRolesResponse>("/traffic/ports/roles", { token }),
    portHistory: (token: string, device: string, iface: string, range: TrafficRange) => {
      const qs = new URLSearchParams({ device, iface, range });
      return apiFetch<PortHistory>(`/traffic/ports/history?${qs}`, { token });
    },
    portBehind: (token: string, device: string, iface: string) => {
      const qs = new URLSearchParams({ device, iface });
      return apiFetch<PortBehind>(`/traffic/ports/behind?${qs}`, { token });
    },
    path: (token: string, device: string, iface?: string) => {
      const qs = new URLSearchParams({ device });
      if (iface) qs.set("iface", iface);
      return apiFetch<TrafficPath>(`/traffic/path?${qs}`, { token });
    },
    // source "counters" (default): the ESTIMATED source→sink tree from port
    // counters + topology (a strict forest). source "flows": the MEASURED
    // graph of one flow observation point (remote|app → point → host → port
    // for download, reversed for upload) — needs point.
    sankey: (
      token: string,
      p: {
        range: TrafficRange;
        dir: FlowDirection;
        device?: string;
        source?: FlowSource;
        point?: number;
        remote?: FlowRemoteGrouping;
      },
    ) => {
      const qs = new URLSearchParams({ range: p.range, dir: p.dir });
      if (p.device) qs.set("device", p.device);
      if (p.source) qs.set("source", p.source);
      if (p.point !== undefined) qs.set("point", String(p.point));
      if (p.remote) qs.set("remote", p.remote);
      return apiFetch<TrafficSankey>(`/traffic/sankey?${qs}`, { token });
    },
  },

  // Flow export (NetFlow / IPFIX). Reads are open to every logged-in user;
  // exporter / interface / point writes are admin-only.
  flows: {
    status: (token: string) => apiFetch<FlowStatus>("/flows/status", { token }),
    setup: (token: string, device?: string) => {
      const qs = new URLSearchParams();
      if (device) qs.set("device", device);
      const q = qs.toString() ? `?${qs}` : "";
      return apiFetch<FlowSetup>(`/flows/setup${q}`, { token });
    },
    points: (token: string) => apiFetch<{ points: FlowPoint[] }>("/flows/points", { token }),
    suggestions: (token: string) =>
      apiFetch<{ suggestions: FlowPointSuggestion[] }>("/flows/points/suggestions", { token }),
    ifaces: (token: string, exporterId: number) =>
      apiFetch<FlowIfacesResponse>(`/flows/exporters/${exporterId}/interfaces`, { token }),
    top: (
      token: string,
      p: { point: number; range: TrafficRange; dir: FlowDirection; group: FlowGroup; limit?: number; resolve?: boolean },
    ) => {
      const qs = new URLSearchParams({ point: String(p.point), range: p.range, dir: p.dir, group: p.group });
      if (p.limit) qs.set("limit", String(p.limit));
      if (p.resolve) qs.set("resolve", "1");
      return apiFetch<FlowTopResponse>(`/flows/top?${qs}`, { token });
    },
    port: (token: string, p: { device: string; iface: string; range: TrafficRange; limit?: number; point?: number }) => {
      const qs = new URLSearchParams({ device: p.device, iface: p.iface, range: p.range });
      if (p.limit) qs.set("limit", String(p.limit));
      if (p.point !== undefined) qs.set("point", String(p.point));
      return apiFetch<FlowPortResponse>(`/flows/port?${qs}`, { token });
    },
    coverage: (token: string, p: { point: number; range: TrafficRange }) => {
      const qs = new URLSearchParams({ point: String(p.point), range: p.range });
      return apiFetch<FlowCoverageResponse>(`/flows/coverage?${qs}`, { token });
    },
    createExporter: (token: string, body: FlowExporterBody) =>
      apiFetch<FlowExporterStatus>("/flows/exporters", { method: "POST", token, body: JSON.stringify(body) }),
    updateExporter: (token: string, id: number, body: FlowExporterBody) =>
      apiFetch<FlowExporterStatus>(`/flows/exporters/${id}`, { method: "PUT", token, body: JSON.stringify(body) }),
    // Deleting leaves the address blocked (see FlowBlockedSender).
    deleteExporter: (token: string, id: number) =>
      apiFetch<void>(`/flows/exporters/${id}`, { method: "DELETE", token }),
    // Removes a block: the sender is an unknown sender again.
    unblockSender: (token: string, address: string) =>
      apiFetch<void>(`/flows/blocked?address=${encodeURIComponent(address)}`, { method: "DELETE", token }),
    updateIface: (token: string, exporterId: number, ifIndex: number, body: { name: string; role: FlowIfaceRole }) =>
      apiFetch<FlowIface>(`/flows/exporters/${exporterId}/interfaces/${ifIndex}`, {
        method: "PUT",
        token,
        body: JSON.stringify(body),
      }),
    createPoint: (token: string, body: FlowPointBody) =>
      apiFetch<FlowPoint>("/flows/points", { method: "POST", token, body: JSON.stringify(body) }),
    updatePoint: (token: string, id: number, body: FlowPointBody) =>
      apiFetch<FlowPoint>(`/flows/points/${id}`, { method: "PUT", token, body: JSON.stringify(body) }),
    deletePoint: (token: string, id: number) =>
      apiFetch<void>(`/flows/points/${id}`, { method: "DELETE", token }),
  },

  // Firmware
  firmware: {
    list: (token: string) => apiFetch<FirmwareStatus[]>("/firmware", { token }),
    // Check All: refreshes MikroTik's release feed (at most hourly) and
    // recomputes every device's comparison — the devices are not contacted.
    check: (token: string) =>
      apiFetch<{ status: "fetching" | "recent"; fetched_at?: string; next_fetch?: string }>("/firmware/check", {
        method: "POST",
        token,
      }),
    releases: (token: string) => apiFetch<FirmwareRelease[]>("/firmware/releases", { token }),
    // The device-changing actions log in with a RouterOS user typed for this
    // one action (never stored by the server or the browser).
    upgrade: (token: string, deviceIds: string[], reboot: boolean, creds: DeviceCredentials) =>
      apiFetch<{ job_id: string }>("/firmware/upgrade", {
        method: "POST",
        token,
        body: JSON.stringify({ device_ids: deviceIds, reboot, ...creds }),
      }),
    // An upgrade job's stored state: what the WS progress frames said so far,
    // for a page that missed some of them.
    job: (token: string, jobId: string) =>
      apiFetch<UpgradeJobState>(`/firmware/upgrade/${encodeURIComponent(jobId)}`, { token }),
    setChannel: (token: string, deviceIds: string[], channel: string, creds: DeviceCredentials) =>
      apiFetch<{ changed: number; errors: string[] }>("/firmware/channel", {
        method: "POST",
        token,
        body: JSON.stringify({ device_ids: deviceIds, channel, ...creds }),
      }),
    upgradeRouterboard: (token: string, deviceIds: string[], reboot: boolean, creds: DeviceCredentials) =>
      apiFetch<{ upgraded: number; errors: string[] }>("/firmware/routerboard", {
        method: "POST",
        token,
        body: JSON.stringify({ device_ids: deviceIds, reboot, ...creds }),
      }),
  },

  // WiFi
  wifi: {
    current: (token: string) => apiFetch<unknown[]>("/wifi/current", { token }),
    history: (token: string, params?: { mac?: string; ap?: string; limit?: number }) => {
      const qs = new URLSearchParams();
      if (params?.mac) qs.set("mac", params.mac);
      if (params?.ap) qs.set("ap", params.ap);
      if (params?.limit) qs.set("limit", String(params.limit));
      const q = qs.toString() ? `?${qs}` : "";
      return apiFetch<unknown[]>(`/wifi/history${q}`, { token });
    },
    macLookup: (token: string) => apiFetch<Record<string, unknown>>("/mac-lookup", { token }),
  },

  // Network health (bridges / STP / loop detection)
  networkHealth: {
    get: (token: string) => apiFetch<NetworkHealth>("/network-health", { token }),
    events: (token: string, limit = 200) =>
      apiFetch<LoopEvent[]>(`/network-health/events?limit=${limit}`, { token }),
    ackEvent: (token: string, id: number) =>
      apiFetch<{ acknowledged: boolean }>(`/network-health/events/${id}/ack`, { method: "POST", token }),
    ackAll: (token: string) =>
      apiFetch<{ acknowledged: number }>("/network-health/events/ack-all", { method: "POST", token }),
  },

  // VLANs (bridge VLAN table + user-editable labels)
  vlans: {
    list: (token: string) => apiFetch<BridgeVLAN[]>("/vlans", { token }),
    labels: (token: string) => apiFetch<VLANLabel[]>("/vlan-labels", { token }),
    updateLabel: (token: string, data: { vlan_id: number; name: string; purpose: string; color: string }) =>
      apiFetch<VLANLabel>("/vlan-labels", { method: "PUT", token, body: JSON.stringify(data) }),
  },

  // Connectivity monitoring (internet-path probes + per-client ping watches)
  connectivity: {
    targets: (token: string) => apiFetch<PingTarget[]>("/connectivity/targets", { token }),
    createTarget: (
      token: string,
      data: {
        kind: "internet" | "client";
        address?: string;
        mac_address?: string;
        label?: string;
        device_id?: string;
        src_address?: string;
        src_interface?: string;
      },
    ) =>
      apiFetch<PingTarget>("/connectivity/targets", { method: "POST", token, body: JSON.stringify(data) }),
    updateTarget: (
      token: string,
      id: string,
      data: Partial<{
        address: string;
        label: string;
        device_id: string;
        enabled: boolean;
        src_address: string;
        src_interface: string;
      }>,
    ) =>
      apiFetch<PingTarget>(`/connectivity/targets/${id}`, { method: "PUT", token, body: JSON.stringify(data) }),
    deleteTarget: (token: string, id: string) =>
      apiFetch<{ status: string }>(`/connectivity/targets/${id}`, { method: "DELETE", token }),
    runTarget: (token: string, id: string) =>
      apiFetch<PingSample>(`/connectivity/targets/${id}/run`, { method: "POST", token }),
    samples: (token: string, id: string, params?: { from?: string; to?: string; limit?: number }) => {
      const qs = new URLSearchParams();
      if (params?.from) qs.set("from", params.from);
      if (params?.to) qs.set("to", params.to);
      if (params?.limit) qs.set("limit", String(params.limit));
      const q = qs.toString() ? `?${qs}` : "";
      return apiFetch<PingSample[]>(`/connectivity/targets/${id}/samples${q}`, { token });
    },
    clientTimeline: (token: string, mac: string, from?: string, to?: string) => {
      const qs = new URLSearchParams();
      if (from) qs.set("from", from);
      if (to) qs.set("to", to);
      const q = qs.toString() ? `?${qs}` : "";
      return apiFetch<ClientTimeline>(`/connectivity/clients/${encodeURIComponent(mac)}/timeline${q}`, { token });
    },

    // Speed tests: scheduled /tool/fetch download measurements from a device.
    speedtests: (token: string) => apiFetch<SpeedTest[]>("/connectivity/speedtests", { token }),
    createSpeedtest: (token: string, data: { device_id: string; url: string; src_address?: string; label?: string }) =>
      apiFetch<SpeedTest>("/connectivity/speedtests", { method: "POST", token, body: JSON.stringify(data) }),
    updateSpeedtest: (
      token: string,
      id: string,
      data: Partial<{ device_id: string; url: string; src_address: string; label: string; enabled: boolean }>,
    ) =>
      apiFetch<SpeedTest>(`/connectivity/speedtests/${id}`, { method: "PUT", token, body: JSON.stringify(data) }),
    deleteSpeedtest: (token: string, id: string) =>
      apiFetch<{ status: string }>(`/connectivity/speedtests/${id}`, { method: "DELETE", token }),
    // Async: 202 {status:"started"}; the resulting sample arrives via the
    // "connectivity.speed" WS topic once the download finishes.
    runSpeedtest: (token: string, id: string) =>
      apiFetch<{ status: string }>(`/connectivity/speedtests/${id}/run`, { method: "POST", token }),
    speedtestSamples: (token: string, id: string, params?: { from?: string; to?: string; limit?: number }) => {
      const qs = new URLSearchParams();
      if (params?.from) qs.set("from", params.from);
      if (params?.to) qs.set("to", params.to);
      if (params?.limit) qs.set("limit", String(params.limit));
      const q = qs.toString() ? `?${qs}` : "";
      return apiFetch<SpeedSample[]>(`/connectivity/speedtests/${id}/samples${q}`, { token });
    },

    // Traceroute on internet targets. Async like runSpeedtest: 202, the run
    // arrives via the "connectivity.traceroute" WS topic.
    runTraceroute: (token: string, targetId: string) =>
      apiFetch<{ status: string }>(`/connectivity/targets/${targetId}/traceroute`, { method: "POST", token }),
    traceroutes: (token: string, targetId: string, limit?: number) => {
      const q = limit ? `?limit=${limit}` : "";
      return apiFetch<TracerouteRun[]>(`/connectivity/targets/${targetId}/traceroutes${q}`, { token });
    },
  },

  // App settings
  settings: {
    get: (token: string) => apiFetch<Record<string, string>>("/settings", { token }),
    update: (token: string, data: Record<string, string>) =>
      apiFetch<Record<string, string>>("/settings", { method: "PUT", token, body: JSON.stringify(data) }),
    testOpnsense: (token: string, data: { url: string; api_key: string; api_secret: string; verify_tls: boolean }) =>
      apiFetch<{ ok: boolean; message: string; leases?: number }>("/settings/opnsense/test", { method: "POST", token, body: JSON.stringify(data) }),
    testOtel: (token: string, data: { endpoint: string; protocol: string; insecure: boolean; headers: string; service_name: string }) =>
      apiFetch<{ ok: boolean; message: string }>("/settings/otel/test", { method: "POST", token, body: JSON.stringify(data) }),
    testMail: (token: string, data: { to: string; host: string; port: string; user: string; password: string; from: string; tls_mode: string; skip_verify: boolean }) =>
      apiFetch<{ ok: boolean; message: string }>("/settings/mail/test", { method: "POST", token, body: JSON.stringify(data) }),
  },

  // Alerts: what the rules currently see, silences, and (admin) where
  // notifications go. A channel's target is write-only.
  alerts: {
    list: (token: string) => apiFetch<AlertList>("/alerts", { token }),
    ack: (token: string, id: number) => apiFetch<Alert>(`/alerts/${id}/ack`, { method: "POST", token }),
    silence: (token: string, data: { scope: "site" | "device" | "rule"; target?: string; hours: number; reason?: string }) =>
      apiFetch<AlertSilence>("/alerts/silences", { method: "POST", token, body: JSON.stringify(data) }),
    endSilence: (token: string, id: number) => apiFetch<void>(`/alerts/silences/${id}`, { method: "DELETE", token }),
    channels: (token: string) => apiFetch<NotificationChannel[]>("/alerts/channels", { token }),
    createChannel: (token: string, data: { name: string; kind: "smtp" | "webhook"; target: string; min_severity: AlertSeverity }) =>
      apiFetch<NotificationChannel>("/alerts/channels", { method: "POST", token, body: JSON.stringify(data) }),
    updateChannel: (token: string, id: number, data: { name?: string; target?: string; min_severity?: AlertSeverity; enabled?: boolean }) =>
      apiFetch<NotificationChannel>(`/alerts/channels/${id}`, { method: "PUT", token, body: JSON.stringify(data) }),
    deleteChannel: (token: string, id: number) => apiFetch<void>(`/alerts/channels/${id}`, { method: "DELETE", token }),
    testChannel: (token: string, id: number) =>
      apiFetch<{ ok: boolean; message: string }>(`/alerts/channels/${id}/test`, { method: "POST", token }),
  },

  // Admin actions
  admin: {
    purgeHistory: (
      token: string,
      data: {
        wifi: boolean;
        clients: boolean;
        network_health: boolean;
        // traffic_samples plus the port_stats_1m / port_stats_1h buckets.
        traffic: boolean;
        // flow_1m / flow_1h (flow-export facts and their meta rows).
        flows?: boolean;
        older_than_days: number;
      },
    ) =>
      apiFetch<{ deleted: Record<string, number> }>("/admin/purge-history", {
        method: "POST",
        token,
        body: JSON.stringify(data),
      }),

    // Export/backup endpoints return raw blobs (the server sets a download
    // filename via Content-Disposition). We don't go through apiFetch because
    // that always parses JSON.
    downloadExport: async (token: string, table: string) => {
      const res = await fetch(`${getApiBase()}/api/v1/admin/export/${encodeURIComponent(table)}`, {
        headers: { Authorization: `Bearer ${token}` },
      }).catch(() => { throw networkError(); });
      if (!res.ok) throw new ApiError(res.status, (await res.text()) || `Request failed (HTTP ${res.status})`);
      return res.blob();
    },
    downloadFullBackup: async (token: string) => {
      const res = await fetch(`${getApiBase()}/api/v1/admin/backup`, {
        headers: { Authorization: `Bearer ${token}` },
      }).catch(() => { throw networkError(); });
      if (!res.ok) throw new ApiError(res.status, (await res.text()) || `Request failed (HTTP ${res.status})`);
      return res.blob();
    },

    importTable: async (token: string, table: string, file: File) => {
      const body = await file.text();
      return apiFetch<{ inserted: number; skipped: number }>(
        `/admin/import/${encodeURIComponent(table)}`,
        { method: "POST", token, body },
      );
    },
    restoreFullBackup: async (token: string, file: File) => {
      const body = await file.text();
      return apiFetch<{ tables: Record<string, { inserted: number; skipped: number }> }>(
        "/admin/restore",
        { method: "POST", token, body },
      );
    },
  },

  // DNS
  dns: {
    list: (token: string) => apiFetch<DNSServer[]>("/dns", { token }),
    create: (token: string, data: { name: string; address: string; port: number }) =>
      apiFetch<DNSServer>("/dns", { method: "POST", token, body: JSON.stringify(data) }),
    update: (token: string, id: string, data: { name: string; address: string; port: number; enabled: boolean }) =>
      apiFetch<DNSServer>(`/dns/${id}`, { method: "PUT", token, body: JSON.stringify(data) }),
    delete: (token: string, id: string) =>
      apiFetch(`/dns/${id}`, { method: "DELETE", token }),
    resolve: (token: string, ips: string[]) =>
      apiFetch<Record<string, string>>("/dns/resolve", { method: "POST", token, body: JSON.stringify({ ips }) }),
  },

  // Users
  users: {
    list: (token: string) => apiFetch<User[]>("/users", { token }),
    create: (token: string, data: { username: string; password: string; role: string }) =>
      apiFetch<User>("/users", { method: "POST", token, body: JSON.stringify(data) }),
    delete: (token: string, id: string) =>
      apiFetch(`/users/${id}`, { method: "DELETE", token }),
  },

  // Health (public): liveness, the instance id the version watchdog compares,
  // and whether this instance polls devices at all.
  health: () => apiFetch<{ status: string; instance_id?: string; pollers?: "local" | "off" }>("/health"),
  // How this instance runs (logged-in users): polling mode + the subnets it may reach.
  system: (token: string) => apiFetch<SystemInfo>("/system", { token }),
};

// Types
export interface SystemInfo {
  instance_id: string;
  pollers: "local" | "off";
  allowed_target_cidrs: string[];
}

export interface Device {
  id: string;
  address: string;
  identity: string;
  platform: string;
  board: string;
  ros_version: string;
  firmware_version: string;
  architecture: string;
  username: string;
  use_tls: boolean;
  // The fingerprint the device's TLS certificate must have (SHA-256,
  // lower-case hex); "" = none — a TLS device is then not logged in to.
  pin_cert_sha256: string;
  api_port: number;
  status: "online" | "offline" | "unknown";
  cpu_load: number | null;
  memory_used: number | null;
  memory_total: number | null;
  uptime: string | null;
  last_seen: string | null;
  last_error: string | null;
  tags: string;
  notes: string;
  created_at: string;
  updated_at: string;
  // CPU class and polling controls. device_class is what the last
  // /system/resource probe derived ("" until probed); class_override an
  // admin's gentler-only override; effective_class what the pollers budget
  // the device as (an unprobed device counts as C1).
  device_class: DeviceClass | "";
  class_override: DeviceClass | "";
  effective_class: DeviceClass;
  class_probed_at: string | null;
  poll_profile: PollProfile;
  polling_paused_until: string | null;
  polling_paused: boolean;
  cpu_count: number | null;
  cpu_mhz_max: number | null;
  // How the pollers treat the device right now — GET /devices/{id} and the
  // polling-control responses only.
  polling?: DevicePolling;
  // What the stored facts say the device does in the network, with the
  // reason (GET /devices and /devices/{id}), and whether flow export may be
  // set up on it (only routers; never a C1 device).
  role?: DeviceRole;
  role_reason?: string;
  flow_export_eligible?: boolean;
}

export type DeviceRole = "router" | "weak-router" | "switch" | "ap-cpe" | "unknown";

// next_runs maps a scheduled read (counters, neighbors, bridge_ports, …) to
// when it runs next; empty while the device is paused or offline.
export interface DevicePolling {
  class: DeviceClass;
  profile: PollProfile;
  paused_until: string | null;
  next_runs: Record<string, string>;
}

export type DeviceClass = "C0" | "C1" | "C2" | "C3";
export type PollProfile = "auto" | "light";

export interface UpdatePollingRequest {
  profile?: PollProfile;
  // null clears the override; omit to leave it alone.
  class_override?: DeviceClass | null;
}

export interface DeviceAuditEntry {
  id: number;
  at: string;
  user_id: string;
  username: string;
  action: string;
  device_id: string;
  detail: string;
}

// One device_findings row: something a poller noticed about the device,
// deduplicated by (kind, detail) — count and last_seen grow on repeats.
// kind: parallel_api_poller (an API login from an address that is not this
// NMS), login_failure, or tls_pin_mismatch (a TLS session refused because
// the device presented another certificate than the pinned one; detail is
// the fingerprint it presented); others may follow.
// One step of a finding's suggested fix. Advice for an operator: the NMS
// never applies it.
export interface FindingFix {
  text: string;
  // RouterOS commands for the device's terminal, shown apart from the text.
  commands?: string[];
  // The commands only print (a check, not a change).
  read_only?: boolean;
  // The step interrupts traffic: plan a maintenance window.
  window?: boolean;
}

export type FindingSeverity = "info" | "warn" | "critical";

export interface DeviceFinding {
  id: number;
  device_id: string;
  kind: string;
  severity: FindingSeverity;
  detail: string;
  first_seen: string;
  last_seen: string;
  count: number;
  // Set on the findings of the hygiene audit (source "audit"): a title, a
  // plain explanation and the suggested fix.
  title?: string;
  explanation?: string;
  fix?: FindingFix[];
  source?: string;
}

// A hygiene finding in a fleet-wide list (GET /network-health).
export interface HygieneFinding extends DeviceFinding {
  device_name: string;
}

export type HygienePassKind = "audit" | "diagnose";

// One pass of the hygiene audit or of the diagnose pass.
export interface HygienePass {
  id: number;
  device_id: string;
  kind: HygienePassKind;
  // scheduled | admin | the alert rule that asked for a diagnose pass
  reason: string;
  requested_by?: string;
  started_at: string;
  // The zero time ("0001-…") while the pass runs.
  finished_at: string;
  // partial: a read failed, or the pass ended early — what was read was
  // judged. aborted: nothing was.
  status: "running" | "complete" | "partial" | "aborted";
  reads: number;
  failed_reads: number;
  // Why a pass ended early, which reads failed, or what it left unchecked.
  detail?: string;
  // What this pass found.
  found: { kind: string; severity: FindingSeverity; title: string; detail: string }[];
  // Only for a pass that judged the device and made no finding: what it saw
  // of the usual device-side causes of a high CPU, as the rest of a sentence
  // that begins with the pass ("found none of …: no graphing, …", "found that
  // traffic-flow is enabled — … — and nothing else: …").
  summary?: string;
}

export interface HygienePassState {
  last: HygienePass | null;
  // When the next scheduled audit runs (audits only; absent while none is
  // scheduled: paused, offline, polling off).
  next_run?: string;
  // When an admin may next ask for this pass; null = now.
  available_at: string | null;
}

// GET /devices/{id}/hygiene
export interface DeviceHygiene {
  pollers: "local" | "off";
  // The kind of pass in progress on the device ("" = none).
  running: "" | HygienePassKind;
  audit: HygienePassState;
  diagnose: HygienePassState;
  // Which stored configuration snapshot is current (the snapshot itself is
  // not returned).
  snapshot: { taken_at: string; hash: string; ros_version: string } | null;
  // The device's hygiene findings, most severe first.
  findings: DeviceFinding[];
}

export type AddressProposalStatus =
  | "pending"
  | "approved"
  | "dismissed"
  | "superseded"
  | "expired"
  | "failed";

// One device_address_proposals row: neighbor discovery saw device_id's MAC
// (mac) at new_address while the NMS polls it at old_address.
export interface AddressProposal {
  id: number;
  device_id: string;
  device_identity: string;
  old_address: string;
  new_address: string;
  mac: string;
  first_seen: string;
  last_seen: string;
  status: AddressProposalStatus;
  decided_by?: string;
  decided_at?: string;
  result?: string;
}

export interface CreateDeviceRequest {
  address: string;
  identity?: string;
  username?: string;
  password?: string;
  use_tls?: boolean;
  // Required with use_tls on create; on update "" clears the pin and an
  // omitted field keeps it. Upper case and colon-separated bytes are accepted.
  pin_cert_sha256?: string;
  api_port?: number;
  tags?: string;
  notes?: string;
}

export interface DeviceInterface {
  id: string;
  device_id: string;
  name: string;
  type: string;
  mac_address: string;
  mtu: number | null;
  running: boolean;
  disabled: boolean;
  comment: string;
}

export interface Neighbor {
  id: string;
  device_id: string;
  local_interface: string;
  neighbor_address: string;
  neighbor_mac: string;
  neighbor_identity: string;
  neighbor_platform: string;
  neighbor_board: string;
  neighbor_version: string;
  neighbor_interface: string;
  discovered_by: string;
  last_seen: string;
}

export interface TopologyData {
  nodes: { data: TopologyNode }[];
  edges: { data: TopologyEdge }[];
}

export interface TopologyNode {
  id: string;
  label: string;
  // internet/gateway/vpn are synthetic egress nodes (status "up", managed=false).
  type: "router" | "switch" | "ap" | "unknown" | "internet" | "gateway" | "vpn";
  status: string;
  model: string;
  ros_version: string;
  cpu_load: number | null;
  address: string;
  managed: boolean;
  // Gateway nodes: the device + port that learned the gateway's MAC in its
  // bridge FDB — the physical attachment point.
  attach_device_id?: string;
  attach_port?: string;
}

export interface TopologyEdge {
  id: string;
  source: string;
  target: string;
  source_interface: string;
  target_interface: string;
  link_type: string;
  status: string;
}

export interface TrafficSample {
  id: number;
  device_id: string;
  interface_name: string;
  rx_bits_per_sec: number;
  tx_bits_per_sec: number;
  rx_packets_per_sec: number;
  tx_packets_per_sec: number;
  collected_at: string;
}

// Per-link throughput for the network map. id == the topology edge id
// (links.id, or a synthetic egress edge id), so it merges straight onto the
// corresponding graph edge. Rates are in source's frame (rx enters source);
// device_id/iface name the end whose port counters were read.
export interface LinkTraffic {
  id: string;
  source: string;
  target: string;
  rx_bps: number;
  tx_bps: number;
  device_id: string;
  iface: string;
}

// Freshness of rates derived from the port-counter snapshot. sampled_at is
// the end of the counter cycle the rates come from (null before the first
// cycle, or for a device the snapshot has nothing of); age_seconds is its
// age when the server answered — add the time since the response arrived
// rather than comparing sampled_at with the local clock.
export interface SnapshotMeta {
  source: "snapshot";
  sampled_at: string | null;
  age_seconds: number | null;
  interval_seconds: number;
  ready: boolean;
}

// REST /traffic/links and the WS "topology.traffic" payload.
export interface LinkTrafficResponse extends SnapshotMeta {
  links: LinkTraffic[];
}

// A device's live-stream ceilings (backend devclass.LiveCaps): the class
// they follow (C1 on the light profile), the shortest sampling period, how
// many ports and viewers may stream at once, and whether only admins may.
export interface LiveCaps {
  class: DeviceClass;
  profile: PollProfile;
  min_period_seconds: number;
  max_ifaces: number;
  leases: number;
  admin_only: boolean;
  // How often the port-counter snapshot — the fallback view — refreshes.
  snapshot_interval_seconds: number;
}

// GET /devices/{id}/live-limits: whether the caller may stream at all (role,
// pause, polling mode); interface and viewer ceilings are decided on
// subscribe (subscribe.error).
export interface LiveLimits extends LiveCaps {
  device_id: string;
  allowed: boolean;
  reason?: string;
  message?: string;
}

// ---- Device load (GET /devices/{id}/load) ------------------------------------

export type DeviceLoadRange = "1h" | "6h" | "24h" | "7d" | "30d";

// One bucket (a minute up to 24h, an hour beyond). Every measurement is null
// when it was not taken; gap is null when the device's counters covered the
// bucket, else why not (paused, unreachable, read_failed, no_session,
// no_baseline, not_polled, not_covered, or no_data: no row at all).
export interface DeviceLoadPoint {
  ts: string;
  gap: string | null;
  covered_s: number | null;
  paused: boolean;
  rebooted: boolean;
  // Quiet-window CPU (%): sampled with the device left alone for 3 s.
  cpu_avg: number | null;
  cpu_max: number | null;
  cpu_p95: number | null; // hourly buckets only
  // CPU 3 s after an interface-counter read (every 10 min).
  cpu_post: number | null;
  free_memory: number | null; // lowest reading, bytes
  // What the NMS sent the device in the bucket.
  nms_cmds: number | null;
  nms_wall_ms: number | null;
  nms_rows: number | null;
  nms_timeouts: number | null;
  nms_reconnects: number | null;
}

export interface DeviceLoadSummary {
  covered_pct: number;
  cpu_avg: number | null;
  cpu_max: number | null;
  // The post-poll average, and the quiet-window average of the same buckets.
  cpu_post_avg: number | null;
  cpu_quiet_at_post: number | null;
  nms_cmds_per_min: number | null;
  nms_wall_ms_per_min: number | null;
  nms_timeouts: number;
  nms_reconnects: number;
  reboots: number;
}

export interface DeviceLoadResponse {
  device_id: string;
  range: DeviceLoadRange;
  resolution: "1m" | "1h";
  step_seconds: number;
  from: string;
  to: string;
  coverage_from: string | null;
  memory_total: number | null;
  points: DeviceLoadPoint[];
  summary: DeviceLoadSummary;
}

// Data of the subscribe.ack / subscribe.error control frame on a
// "traffic.<device>.<iface>" topic: the effective period when granted, the
// refusal code and message (and the fallback topic) when not.
export interface LiveStreamStatus extends LiveCaps {
  period_seconds?: number;
  code?: string;
  error?: string;
  fallback?: string;
}

// ---- Traffic analytics (port-stats collector) --------------------------------
//
// Direction semantics: on any port rx = bits entering the device through it,
// tx = bits leaving. Download is rx on upstream-facing roles (wan, uplink,
// vpn) and tx on downstream-facing ones (downlink, access, wireless). peer,
// virtual and idle ports have no download/upload reading (raw in/out).

export type TrafficRange = "live" | "15m" | "1h" | "6h" | "24h" | "7d" | "30d";
export type TopMetric = "avg" | "max" | "bytes";
export type TopDir = "total" | "rx" | "tx";
export type FlowDirection = "download" | "upload";
// peer: a physical link to another managed device that is not an edge of the
// inferred tree (anchor ↔ anchor, redundant/ring links). It carries the
// neighbour fields like a downlink but no clients.
export type PortRoleName = "wan" | "uplink" | "downlink" | "peer" | "access" | "wireless" | "vpn" | "virtual" | "idle";

// One interface's current rates. The WS "traffic.ports" payload is raw;
// device_name and role are only present on REST /traffic/ports/latest.
export interface PortRate {
  device_id: string;
  iface: string;
  type: string;
  comment?: string;
  running: boolean;
  disabled: boolean;
  rx_bps: number;
  tx_bps: number;
  rx_pps: number;
  tx_pps: number;
  device_name?: string;
  role?: PortRoleName;
}

// ts is the end of the collector's last cycle (null before the first one);
// ready latches once any port has a computed rate (two polls).
export interface PortSnapshot {
  ts: string | null;
  interval_seconds: number;
  ready: boolean;
  ports: PortRate[];
}

export interface PortRoleInfo {
  device_id: string;
  iface: string;
  role: PortRoleName;
  neighbor_device_id?: string;
  neighbor_name?: string;
  neighbor_iface?: string;
  neighbor_count: number;
  client_count: number;
  mac_count: number;
  gateway_ip?: string;
  upstream_node?: string;
  // Bond members: the bond this port belongs to. A member carries its bond's
  // role and neighbour, with no clients of its own.
  master?: string;
}

// PortRoleInfo plus the device name and the server-computed "behind" summary.
export interface PortRole extends PortRoleInfo {
  device_name: string;
  behind: string;
}

// A device's place in the inferred physical tree. parent_id is a device id,
// a synthetic node id ("internet", "gw:<ip>", "vpn:<dev>:<iface>") or "".
export interface DeviceTreeInfo {
  device_id: string;
  name: string;
  parent_id: string;
  parent_iface: string;
  uplink_iface: string;
  depth: number;
  anchor: boolean;
}

export interface PortRolesResponse {
  anchored: boolean;
  devices: DeviceTreeInfo[];
  roles: PortRole[];
}

// rx_bps/tx_bps are null for a step the collector did not cover (backend
// down/restarting) — drawn as a gap, not as zero traffic.
export interface SeriesPoint {
  ts: string;
  rx_bps: number | null;
  tx_bps: number | null;
}

export interface TopPortRow {
  rank: number;
  device_id: string;
  device_name: string;
  iface: string;
  type: string;
  role: PortRoleName;
  behind: string;
  neighbor_device_id?: string;
  client_count: number;
  running: boolean;
  rx_avg: number;
  tx_avg: number;
  rx_max: number;
  tx_max: number;
  rx_bytes: number;
  tx_bytes: number;
  // Sort key: bps for avg/max, bytes for bytes.
  value: number;
  sparkline: SeriesPoint[];
}

export interface TopPortsResponse {
  range: TrafficRange;
  metric: TopMetric;
  dir: TopDir;
  resolution: string;
  from: string | null;
  to: string | null;
  coverage_from: string | null;
  rows: TopPortRow[];
}

export interface SeriesStats {
  rx_avg: number;
  tx_avg: number;
  rx_max: number;
  tx_max: number;
  rx_p95: number;
  tx_p95: number;
  rx_bytes: number;
  tx_bytes: number;
}

export interface PortHistory {
  device_id: string;
  iface: string;
  range: TrafficRange;
  resolution: "1s" | "1m" | "1h";
  step_seconds: number;
  from: string;
  to: string;
  coverage_from: string | null;
  points: SeriesPoint[];
  stats: SeriesStats;
}

export interface BehindClient {
  mac: string;
  ip: string;
  host_name: string;
  vendor: string;
  vid: number;
  wireless: boolean;
  ap: string;
  ssid: string;
  signal: string;
  attached_device_id: string;
  attached_iface: string;
  attached_device_name: string;
  last_seen: string;
}

export interface PortBehind {
  device_id: string;
  iface: string;
  role: PortRoleName;
  neighbor: { device_id: string; name: string; iface: string } | null;
  uplink: { node_id: string; label: string; gateway_ip: string } | null;
  client_count: number;
  mac_count: number;
  clients: BehindClient[];
  vlans: { vid: number; name: string; tagged: boolean }[];
}

// One hop of the Internet → port path. down_bps/up_bps describe the segment
// ENTERING this hop; hop 0 is never measured.
export interface PathHop {
  kind: "internet" | "gateway" | "vpn" | "device" | "clients";
  id: string;
  label: string;
  device_id?: string;
  in_iface?: string;
  out_iface?: string;
  client_count?: number;
  sink: boolean;
  down_bps: number;
  up_bps: number;
  measured: boolean;
}

export interface TrafficPath {
  device_id: string;
  iface: string;
  // "" when the path was requested without an iface.
  role: PortRoleName | "";
  anchored: boolean;
  hops: PathHop[];
}

// type "other" covers the "+N ports" fold ("other:<dev>") and the synthetic
// "local:<dev>" source that carries a device's east-west surplus (outflow
// beyond what it receives from its parent), so every node conserves flow.
// remote / app / point / host only occur in the measured (source=flows)
// graph, whose "other:remote" / "other:local" folds are type "other" too.
export interface TrafficSankeyNode {
  id: string;
  name: string;
  type: "internet" | "gateway" | "vpn" | "device" | "port" | "other" | "remote" | "app" | "point" | "host";
  device_id?: string;
  iface?: string;
  client_count?: number;
  // Measured graph: remote / host endpoints.
  ip?: string;
  class?: FlowEndpointClass;
}

export interface TrafficSankeyLink {
  source: number;
  target: number;
  value: number;
  source_iface?: string;
}

export interface TrafficSankey {
  // "counters" on the Phase 1 path; older backends omit it.
  source?: FlowSource;
  estimated: boolean;
  direction: FlowDirection;
  range: TrafficRange;
  from: string | null;
  to: string | null;
  nodes: TrafficSankeyNode[];
  links: TrafficSankeyLink[];
  // Measured graph only.
  point?: FlowPoint;
  sampling_rate?: number;
  coverage?: number | null;
  total_bps?: number;
}

// ---- Flow export (NetFlow / IPFIX) -------------------------------------------
//
// Measured per-conversation traffic from flow exporters (RouterOS IPFIX,
// OPNsense NetFlow v9). Everything is scoped to ONE observation point (a set
// of one exporter's interfaces, "facing" the internet or the hosts); rows of
// two exporters are never added together. Times are RFC 3339 UTC.

export type FlowSource = "counters" | "flows";
export type FlowRemoteGrouping = "host" | "app";
export type FlowGroup = "src" | "dst" | "pair" | "conv" | "app";
export type FlowExporterKind = "routeros" | "opnsense" | "other";
export type FlowExporterState = "ok" | "stale" | "never" | "disabled";
export type FlowProtocol = "netflow5" | "netflow9" | "ipfix" | "sflow5" | "";
export type FlowTemplateState = "ok" | "waiting" | "persisted" | "n/a";
export type FlowEndpointClass = "internal" | "external" | "self" | "multicast" | "vpn";
export type FlowIfaceRole = "" | "lan" | "wan" | "vpn" | "other";

// Cumulative since process start.
export interface FlowGlobalStats {
  datagrams: number;
  unknown_datagrams: number;
  rejected: number;
  rate_limited: number;
  // From blocked senders, dropped before decode.
  blocked?: number;
  queue_dropped: number;
  kernel_dropped: number;
  writer_dropped: number;
  late_records: number;
  overflow_records: number;
  self_export_dropped: number;
  dup_dropped: number;
  panics: number;
}

export interface FlowRates {
  datagrams: number;
  flows: number;
  bytes: number;
}

export interface FlowExporterStatus {
  id: number;
  name: string;
  address: string;
  kind: FlowExporterKind;
  device_id: string | null;
  enabled: boolean;
  auto: boolean;
  sampling_override: number;
  nat_addresses: string[];
  state: FlowExporterState;
  last_seen: string | null;
  protocol: FlowProtocol;
  sampling_rate: number;
  templates: number;
  template_state: FlowTemplateState;
  // Average over the last 5 min (0 when the collector is off).
  per_minute: FlowRates;
  // datagrams, flows, bytes, decode_errors, template_misses, replayed,
  // pending_dropped, seq_lost, rate_limited, dup_dropped,
  // self_export_dropped, nat_dst_records, reboots, iface_overflow,
  // oversized.
  counters: Record<string, number>;
  // sysuptime / absolute / uptime_est / export.
  time_source: Record<string, number>;
  clock_skew_ms: number;
  last_error: string;
  last_error_at: string | null;
  interfaces_seen: number;
  // The role of the managed device at the exporter's address ("" = not a
  // managed device), and — when that device must not export flows (switch,
  // access point, CPE, C1) — why. The exporter keeps working either way.
  device_role?: DeviceRole | "";
  device_role_reason?: string;
  export_warning?: string;
}

export interface FlowUnknownSender {
  address: string;
  first_seen: string;
  last_seen: string;
  datagrams: number;
  protocol: FlowProtocol;
  device_id: string | null;
  device_name: string;
  device_role?: DeviceRole | "";
  device_role_reason?: string;
}

// A sender whose datagrams are dropped before decode: a deleted exporter
// ("deleted") or a managed device that must not export ("auto"). It stays
// blocked until an admin unblocks it or adds it with unblock.
export interface FlowBlockedSender {
  address: string;
  name: string;
  kind: FlowExporterKind;
  reason: string;
  source: "deleted" | "auto";
  device_id: string;
  device_name: string;
  device_role: DeviceRole | "";
  device_role_reason: string;
  // The device at this address must still not export: its next datagram
  // would block it again, so removing the block alone does nothing.
  still_never: boolean;
  created_by: string;
  created_at: string | null;
  last_seen: string | null;
  datagrams: number;
}

export interface FlowSuggestedExporter {
  address: string;
  name: string;
  kind: FlowExporterKind;
  reason: string;
}

export interface FlowStatus {
  enabled: boolean;
  listen: string[];
  listen_errors: string[];
  started_at: string | null;
  flushed_through: string | null;
  rolled_through: string | null;
  rcvbuf_bytes: number;
  global: FlowGlobalStats;
  exporters: FlowExporterStatus[];
  unknown_senders: FlowUnknownSender[];
  blocked_senders?: FlowBlockedSender[];
  // "" = nothing is accepted automatically; "router" = routers only.
  auto_accept_classes?: "" | "router";
  // REST only (the "flows.status" WS payload omits it).
  suggested_exporters?: FlowSuggestedExporter[];
}

// One numbered step of the rollback. The one risky step carries a warning.
export interface FlowRollbackStep {
  text: string;
  commands: string[];
  warning?: string;
  optional?: boolean;
}

export interface FlowSetup {
  enabled: boolean;
  port: number;
  // The admin-approved collector address ("Flow advertise address" in
  // Settings); "" with source "unknown" while it is not set. Never guessed.
  advertise_address: string;
  advertise_source: "setting" | "unknown";
  // The chosen device: the class it is budgeted as, its role and why, and
  // whether the commands that enable export are shown. Empty without one.
  class: DeviceClass | "";
  role: DeviceRole | "";
  role_reason: string;
  eligible: boolean;
  warnings: string[];
  advice: string[];
  routeros: {
    device_id: string | null;
    // Read-only: does the device export?
    check: string[];
    // Empty unless the device is eligible.
    apply: string[];
    interfaces_note: string;
    rollback: { fixed: boolean; note: string; steps: FlowRollbackStep[] };
  };
  opnsense: {
    destination: string;
    version: string;
    active_timeout: number;
    inactive_timeout: number;
    steps: string[];
  };
}

export interface FlowIface {
  if_index: number;
  name: string;
  type: string;
  vlan_id: number;
  parent: string;
  role: FlowIfaceRole;
  source: string;
  // Learned address prefixes seen on the interface (CSV).
  hint: string;
  first_seen: string | null;
  last_seen: string | null;
}

export interface FlowIfacesResponse {
  exporter_id: number;
  interfaces: FlowIface[];
}

export interface FlowAttach {
  device_id: string;
  iface: string;
}

// FlowPointView: one observation point.
export interface FlowPoint {
  id: number;
  name: string;
  kind: "native" | "derived";
  exporter_id: number;
  exporter_name: string;
  exporter_kind: FlowExporterKind;
  exporter_state: FlowExporterState;
  protocol: FlowProtocol;
  if_indexes: number[];
  if_names: string[];
  facing: "up" | "down";
  // "" when the point has no managed port.
  port_side: "" | "same" | "peer";
  device_id: string | null;
  device_name: string;
  iface: string;
  local_internal: boolean;
  exclude_attach: FlowAttach[];
  note: string;
  auto: boolean;
  enabled: boolean;
  sampling_rate: number;
  coverage_capable: boolean;
  last_data: string | null;
}

// POST / PUT body of /flows/points (also the `point` of a suggestion).
export interface FlowPointBody {
  exporter_id: number;
  name: string;
  kind: "native" | "derived";
  if_indexes: number[];
  facing: "up" | "down";
  port_side: "" | "same" | "peer";
  device_id: string | null;
  iface: string;
  local_internal: boolean;
  exclude_attach: FlowAttach[];
  note: string;
  enabled: boolean;
}

export interface FlowPointSuggestion {
  rule: "gateway-host" | "trunk";
  reason: string;
  exists: boolean;
  point: FlowPointBody | null;
}

export interface FlowEndpoint {
  ip: string;
  class: FlowEndpointClass;
  name: string;
  mac: string;
  vendor: string;
  device_id: string | null;
  attached: { device_id: string; device_name: string; iface: string } | null;
  // Name of the exporter whose NAT address this is (its traffic, NATed).
  nat_of: string | null;
}

export interface FlowApp {
  proto: number;
  proto_name: string;
  port: number;
  // "" when the port is not a known service.
  label: string;
}

export interface FlowOther {
  bytes: number;
  packets: number;
  flows: number;
  avg_bps: number;
  share: number;
}

export interface FlowTopRow extends FlowOther {
  rank: number;
  key: string;
  src: FlowEndpoint | null;
  dst: FlowEndpoint | null;
  app: FlowApp | null;
}

export interface FlowTopResponse {
  point: FlowPoint;
  range: TrafficRange;
  dir: FlowDirection;
  group: FlowGroup;
  resolution: "1m" | "1h";
  from: string;
  to: string;
  coverage_from: string | null;
  seconds: number;
  total_bytes: number;
  total_bps: number;
  sampling_rate: number;
  overflow: number;
  rows: FlowTopRow[];
  other: FlowOther;
}

export interface FlowPortDirection {
  rows: FlowTopRow[];
  total_bytes: number;
  other: FlowOther;
}

export interface FlowPortCoverage {
  download: number | null;
  upload: number | null;
  flow_down_bytes: number;
  flow_up_bytes: number;
  counter_down_bytes: number;
  counter_up_bytes: number;
}

export interface FlowPortResponse {
  reason: "ok" | "no_point" | "collector_disabled";
  point: FlowPoint | null;
  alternatives: FlowPoint[];
  range: TrafficRange;
  resolution: "1m" | "1h";
  from: string | null;
  to: string | null;
  coverage_from: string | null;
  seconds: number;
  coverage: FlowPortCoverage;
  download: FlowPortDirection;
  upload: FlowPortDirection;
}

// Flow values are null where the step has no flow meta; counter values null
// where Phase 1 has no bucket or the point has no managed port.
export interface FlowCoveragePoint {
  ts: string;
  flow_down_bps: number | null;
  flow_up_bps: number | null;
  counter_down_bps: number | null;
  counter_up_bps: number | null;
}

export interface FlowCoverageResponse {
  point: FlowPoint;
  range: TrafficRange;
  step_seconds: number;
  from: string;
  to: string;
  coverage_from: string | null;
  points: FlowCoveragePoint[];
  ratio: { download: number | null; upload: number | null };
}

// WS "flows.flushed": after each minute flush.
export interface FlowFlushedEvent {
  exporter_ids: number[];
  bucket: string;
  flushed_through: string;
}

export interface FlowExporterBody {
  name: string;
  address: string;
  kind: FlowExporterKind;
  device_id: string | null;
  enabled: boolean;
  sampling_override: number;
  nat_addresses: string[];
  // POST only: add the exporter although its address is blocked (a deleted
  // exporter, or a device that must not export). Without it the server
  // answers 409 with code "sender_blocked" and the reason.
  unblock?: boolean;
}

// One physical port of a device with its rate from the port-counter snapshot.
export interface DevicePort {
  name: string;
  type: string;
  running: boolean;
  disabled: boolean;
  comment: string;
  rx_bps: number;
  tx_bps: number;
}

// GET /devices/{id}/ports. sampled_at is null while the snapshot has none
// of the device's interfaces (offline or not polled yet): rates are 0 then.
export interface DevicePortsResponse extends SnapshotMeta {
  device_id: string;
  ports: DevicePort[];
}

export interface FirmwareStatus {
  id: string;
  device_id: string;
  channel: string;
  installed_version: string;
  latest_version: string | null;
  update_available: boolean;
  routerboard_current: string | null;
  routerboard_upgrade: string | null;
  last_checked: string | null;
}

// The newest RouterOS version of one update channel, from MikroTik's feed.
export interface FirmwareRelease {
  channel: string;
  version: string;
  build_time: string | null;
  fetched_at: string;
}

// An upgrade job and the outcome of each of its devices so far. A job ends as
// "completed" whatever its devices did (each row has its own status), or as
// "failed" when it could not run at all.
export interface UpgradeJobState {
  job: { id: string; status: "pending" | "running" | "completed" | "failed" | string };
  devices: { device_id: string; status: string; message: string }[];
}

// A RouterOS login typed for one firmware action.
export interface DeviceCredentials {
  username: string;
  password: string;
}

export interface User {
  id: string;
  username: string;
  role: string;
  created_at: string;
}

export interface DNSServer {
  id: string;
  name: string;
  address: string;
  port: number;
  enabled: boolean;
  created_at: string;
}

// State of the on-demand client refresh (POST /clients/refresh).
export interface ClientRefreshStatus {
  // false on an instance that polls nothing: there is nothing to refresh from.
  available: boolean;
  running: boolean;
  started_at?: string;
  finished_at?: string;
  // Set while a new refresh would be refused as too soon.
  next_allowed_at?: string;
  min_interval_seconds: number;
  // What the last finished refresh did.
  clients: number;
  devices_read: number;
  devices_skipped: number;
  timed_out?: boolean;
}

export interface ClientListResult {
  clients: NetworkClient[];
  total: number;
  active?: number;
  cached: boolean;
  // When the newest cache entry was seen (absent for an empty cache).
  updated_at?: string;
  age_seconds?: number;
  // Absent from a backend older than the refresh endpoint.
  refresh?: ClientRefreshStatus;
}

export interface NetworkClient {
  mac_address: string;
  ip_address: string;
  host_name: string;
  dns_name: string;
  interface: string;
  source: "arp" | "dhcp" | "wifi";
  device_id: string;
  device_name: string;
  ap?: string;
  ssid?: string;
  band?: string;
  channel?: string;
  frequency?: string;
  signal?: string;
  tx_rate?: string;
  rx_rate?: string;
  uptime?: string;
  active?: boolean;
  last_seen?: string;
  vendor?: string;
  randomized?: boolean;
}

export interface BridgePortStatus {
  id: string;
  device_id: string;
  bridge_name: string;
  port_interface: string;
  role: string;
  status: string;
  edge: boolean;
  point_to_point: boolean;
  path_cost: number;
  designated_bridge: string;
  // Runtime fields a device may not report: null means unknown (never read
  // as 0 or false). Optional for backends older than the 7.24 bridge reader.
  topology_changes?: number | null;
  forwarding?: boolean | null;
  learning?: boolean | null;
  inactive?: boolean | null;
  disabled?: boolean | null;
  hw_offload?: boolean | null;
  hw_offload_group?: string;
  pvid?: number | null;
  // Raw RouterOS status ("in-bridge" | "inactive") and the configured edge
  // (auto | yes | no | yes-discover | no-discover).
  port_state?: string;
  edge_config?: string;
  last_polled: string;
}

// How a bridge's port STP state was read: the port print itself (RouterOS
// >= 7.24, cheapest), one monitor over all active ports, a monitor per port
// (last resort), or not at all.
export type StpSource = "print" | "monitor-batched" | "monitor-per-port" | "none";

export interface BridgeWithPorts {
  id: string;
  device_id: string;
  device_name: string;
  bridge_name: string;
  protocol: string;
  stp_enabled: boolean;
  bridge_id: string;
  root_bridge_id: string;
  root_path_cost: number;
  root_port: string;
  // Sum of the member ports' topology-change counters.
  topology_changes: number;
  last_topology_change: string;
  port_count: number;
  stp_source?: StpSource | "";
  last_polled: string;
  ports: BridgePortStatus[];
}

export interface LoopEvent {
  id: number;
  device_id: string;
  device_name: string;
  event_type: string;
  severity: "warn" | "critical";
  bridge_name: string;
  port_interface: string;
  mac_address: string;
  message: string;
  recorded_at: string;
  acknowledged: boolean;
  acknowledged_at?: string | null;
}

export interface InterfaceState {
  id: string;
  device_id: string;
  device_name: string;
  interface_name: string;
  interface_type: string;
  running: boolean;
  disabled: boolean;
  slave: boolean;
  last_link_up: string;
  last_link_down: string;
  flap_count_window: number;
  loop_protect_status: string;
  comment: string;
  last_polled: string;
}

export interface NetworkHealth {
  bridges: BridgeWithPorts[];
  events: LoopEvent[];
  port_states: InterfaceState[];
  // Every device's hygiene findings (the audit and the diagnose pass), most
  // severe first.
  findings: HygieneFinding[];
}

export interface BridgeVLAN {
  id: string;
  device_id: string;
  device_name: string;
  bridge_name: string;
  vlan_ids: string;
  tagged: string;
  untagged: string;
  current_tagged: string;
  current_untagged: string;
  comment: string;
  last_polled: string;
}

export interface VLANLabel {
  vlan_id: number;
  name: string;
  purpose: string;
  color: string;
  updated_at: string;
}

// Connectivity monitoring types. PingTarget is the *enriched* shape the API
// returns: the stored target plus device_name / host_name / last_sample
// joined in by the backend on every GET.
export interface PingTarget {
  id: string;
  kind: "internet" | "client";
  address: string;
  mac_address: string;
  label: string;
  device_id: string;
  enabled: boolean;
  // Optional probe source: when set, /ping (and traceroute) run with
  // =src-address= / =interface= so the probe leaves via a specific VLAN/ISP
  // path instead of the default route. Empty string = unset.
  src_address: string;
  src_interface: string;
  created_at: string;
  device_name: string;
  host_name: string;
  last_sample: PingSample | null;
}

// One probe result. error != "" means the probe could not run at all
// (device offline / no API connection / no known IP); such samples have
// sent=0 and null RTTs.
export interface PingSample {
  id: number;
  target_id: string;
  device_id: string;
  address: string;
  sent: number;
  received: number;
  loss_pct: number;
  rtt_min_ms: number | null;
  rtt_avg_ms: number | null;
  rtt_max_ms: number | null;
  jitter_ms: number | null;
  error: string;
  recorded_at: string;
}

// One IP address configured on a device, read live; vlan_id is set when the
// owning interface is an /interface/vlan.
export interface DeviceAddress {
  address: string;
  ip: string;
  interface: string;
  vlan_id: string;
  family: "ip" | "ipv6";
}

// Speed test: a scheduled /tool/fetch download measurement run from a RouterOS
// device. This is the *enriched* shape (device_name / last_sample joined in).
export interface SpeedTest {
  id: string;
  device_id: string;
  url: string;
  src_address: string;
  label: string;
  enabled: boolean;
  created_at: string;
  device_name: string;
  last_sample: SpeedSample | null;
}

// One speed-test result. mbps is null when the test failed (error != "").
export interface SpeedSample {
  id: number;
  test_id: string;
  device_id: string;
  mbps: number | null;
  bytes: number;
  duration_ms: number;
  error: string;
  recorded_at: string;
}

export interface TracerouteHop {
  hop: number;
  address: string;
  loss_pct: number;
  sent: number;
  last_ms: number | null;
  avg_ms: number | null;
  best_ms: number | null;
  worst_ms: number | null;
  status: string;
}

// One traceroute capture for an internet target (manual run or auto-captured
// when a probe crosses the loss threshold). hops is never null on success.
export interface TracerouteRun {
  id: number;
  target_id: string;
  address: string;
  hops: TracerouteHop[];
  error: string;
  recorded_at: string;
}

export interface ClientSignalSample {
  id: number;
  mac_address: string;
  ap_name: string;
  ssid: string;
  band: string;
  signal_dbm: number | null;
  tx_rate: string;
  rx_rate: string;
  recorded_at: string;
}

// One wifi_history row, same shape as GET /wifi/history rows.
export interface WifiHistoryEntry {
  id: number;
  mac_address: string;
  ip_address: string;
  host_name: string;
  ap_name: string;
  ssid: string;
  band: string;
  channel: string;
  signal: string;
  tx_rate: string;
  rx_rate: string;
  event: string;
  controller_id: string;
  controller_name: string;
  source: string;
  reason: string;
  recorded_at: string;
}

// Correlated history for one watched client: pings + signal samples +
// wifi join/leave/roam events + network-health events that may explain a
// dropoff. All arrays are newest-first and never null.
export interface ClientTimeline {
  pings: PingSample[];
  signals: ClientSignalSample[];
  wifi_events: WifiHistoryEntry[];
  network_events: LoopEvent[];
}

export interface DiscoveredDevice {
  mac_address: string;
  identity: string;
  version: string;
  platform: string;
  board: string;
  ip_address: string;
  ipv6_address: string;
  interface: string;
  uptime: string;
  software_id: string;
  source_addr: string;
}

export interface DeepDiscoveredDevice {
  address: string;
  mac: string;
  identity: string;
  platform: string;
  board: string;
  version: string;
  source: "neighbor" | "port-scan" | "both";
  open_ports: number[];
  seen_from: string;
}

export type AlertSeverity = "info" | "warning" | "critical";

export interface Alert {
  id: number;
  rule: string;
  key: string;
  device_id?: string;
  device_name?: string;
  severity: AlertSeverity;
  state: "firing" | "acked" | "resolved";
  first_at: string;
  last_at: string;
  last_notified_at?: string;
  resolved_at?: string;
  acked_by?: string;
  acked_at?: string;
  summary: string;
  // Rule-specific details. Lists of text lines: "rollback" and "check" (flow
  // export), "likely_cause" (what the diagnose pass found, with
  // "diagnosed_at"), "findings" and "fix" (audit_critical).
  facts?: Record<string, unknown>;
  // false = raised on evidence that can be forged or mistaken (a UDP source
  // address); such an alert is never critical until it is confirmed.
  confirmed?: boolean;
  runbook_url: string;
  // A silence in force keeps this alert from being notified.
  silenced: boolean;
}

export interface AlertSilence {
  id: number;
  scope: "site" | "device" | "rule";
  target: string;
  until: string;
  reason: string;
  created_by: string;
  created_at: string;
}

export interface AlertList {
  alerts: Alert[];
  firing: number;
  acked: number;
  silences: AlertSilence[];
  rules: string[];
  // false on an instance that polls nothing: rules are not evaluated there.
  evaluating: boolean;
}

export interface NotificationChannel {
  id: number;
  name: string;
  kind: "smtp" | "webhook";
  // Enough of the target to recognise it; the address or URL is never returned.
  target_hint: string;
  min_severity: AlertSeverity;
  enabled: boolean;
}
