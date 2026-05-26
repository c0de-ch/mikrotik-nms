function getApiBase() {
  if (process.env.NEXT_PUBLIC_API_URL) return process.env.NEXT_PUBLIC_API_URL;
  if (typeof window !== "undefined") return `http://${window.location.hostname}:8080`;
  return "http://localhost:8080";
}

interface FetchOptions extends RequestInit {
  token?: string;
}

// Deduplicate concurrent refresh attempts
let refreshPromise: Promise<{ access_token: string; refresh_token: string } | null> | null = null;

async function tryRefreshToken(): Promise<{ access_token: string; refresh_token: string } | null> {
  const refreshToken = typeof window !== "undefined" ? localStorage.getItem("refresh_token") : null;
  if (!refreshToken) return null;

  try {
    const res = await fetch(`${getApiBase()}/api/v1/auth/refresh`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: refreshToken }),
    });
    if (res.ok) {
      const tokens = await res.json();
      localStorage.setItem("access_token", tokens.access_token);
      localStorage.setItem("refresh_token", tokens.refresh_token);
      if (typeof window !== "undefined") {
        window.dispatchEvent(new CustomEvent("auth:refreshed", { detail: tokens }));
      }
      return tokens;
    }
  } catch {
    // refresh failed
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
    return fetch(`${getApiBase()}/api/v1${path}`, {
      ...fetchOptions,
      headers,
      credentials: "include",
    });
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
    const body = await res.json().catch(() => ({ error: res.statusText }));
    throw new ApiError(res.status, body.error || res.statusText);
  }

  return res.json();
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
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

  // Network clients
  clients: {
    scan: (token: string, options?: { limit?: number; timeout?: number }) => {
      const params = new URLSearchParams();
      if (options?.limit) params.set("limit", String(options.limit));
      if (options?.timeout) params.set("timeout", String(options.timeout));
      const qs = params.toString() ? `?${params}` : "";
      return apiFetch<ClientScanResult>(`/clients${qs}`, { token });
    },
    cached: (token: string) =>
      apiFetch<{ clients: NetworkClient[]; total: number; cached: boolean }>("/clients/cached", { token }),
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
    neighbors: (token: string, id: string) =>
      apiFetch<Neighbor[]>(`/devices/${id}/neighbors`, { token }),
  },

  // Topology
  topology: {
    get: (token: string) => apiFetch<TopologyData>("/topology", { token }),
  },

  // Traffic
  traffic: {
    summary: (token: string) =>
      apiFetch<{ device_id: string; rx_bps: number; tx_bps: number }[]>("/traffic/summary", { token }),
    get: (token: string, deviceId: string, iface: string, from?: string, to?: string) => {
      const params = new URLSearchParams();
      if (from) params.set("from", from);
      if (to) params.set("to", to);
      const qs = params.toString() ? `?${params}` : "";
      return apiFetch<TrafficSample[]>(`/traffic/${deviceId}/${iface}${qs}`, { token });
    },
  },

  // Firmware
  firmware: {
    list: (token: string) => apiFetch<FirmwareStatus[]>("/firmware", { token }),
    check: (token: string) =>
      apiFetch("/firmware/check", { method: "POST", token }),
    upgrade: (token: string, deviceIds: string[], reboot: boolean) =>
      apiFetch("/firmware/upgrade", {
        method: "POST",
        token,
        body: JSON.stringify({ device_ids: deviceIds, reboot }),
      }),
    setChannel: (token: string, deviceIds: string[], channel: string) =>
      apiFetch<{ changed: number; errors: string[] }>("/firmware/channel", {
        method: "POST",
        token,
        body: JSON.stringify({ device_ids: deviceIds, channel }),
      }),
    upgradeRouterboard: (token: string, deviceIds: string[], reboot: boolean) =>
      apiFetch<{ upgraded: number; errors: string[] }>("/firmware/routerboard", {
        method: "POST",
        token,
        body: JSON.stringify({ device_ids: deviceIds, reboot }),
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

  // App settings
  settings: {
    get: (token: string) => apiFetch<Record<string, string>>("/settings", { token }),
    update: (token: string, data: Record<string, string>) =>
      apiFetch<Record<string, string>>("/settings", { method: "PUT", token, body: JSON.stringify(data) }),
  },

  // Admin actions
  admin: {
    purgeHistory: (
      token: string,
      data: {
        wifi: boolean;
        clients: boolean;
        network_health: boolean;
        traffic: boolean;
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
      });
      if (!res.ok) throw new ApiError(res.status, await res.text());
      return res.blob();
    },
    downloadFullBackup: async (token: string) => {
      const res = await fetch(`${getApiBase()}/api/v1/admin/backup`, {
        headers: { Authorization: `Bearer ${token}` },
      });
      if (!res.ok) throw new ApiError(res.status, await res.text());
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

  // Health
  health: () => apiFetch<{ status: string }>("/health"),
};

// Types
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
}

export interface CreateDeviceRequest {
  address: string;
  identity?: string;
  username?: string;
  password?: string;
  use_tls?: boolean;
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
  type: "router" | "switch" | "ap" | "unknown";
  status: string;
  model: string;
  ros_version: string;
  cpu_load: number | null;
  address: string;
  managed: boolean;
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

export interface ClientScanResult {
  clients: NetworkClient[];
  total: number;
  limited: boolean;
  timed_out: boolean;
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
  last_polled: string;
}

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
  topology_changes: number;
  last_topology_change: string;
  port_count: number;
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
