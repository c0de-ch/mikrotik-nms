# Flow export guide

MikroTik NMS can show **measured** traffic (who talked to whom, on which port and
protocol, how much) next to the **estimated** view it derives from port counters and
topology. The measurements come from flow records that your routers and firewalls
export to a collector built into the backend.

- **Protocols:** NetFlow v5, NetFlow v9, IPFIX and sFlow v5, all on one UDP port (2055 by
  default). The version is read from each datagram.
- **Off by default.** Nothing listens until `MIKROTIK_NMS_FLOW_LISTEN` is set.
- **Aggregated, not raw.** Records are folded into per-minute top-N conversations per
  exporter (plus an exact "other" remainder), then rolled up hourly. No payload is ever
  stored; see [Privacy and security](#9-privacy-and-security).

Placeholders used below: `<NMS-IP>` is the address exporters send to (the host running the
backend), `<router-IP>` is the address the NMS manages a RouterOS device by.

---

## 1. How it fits together

```
router / firewall ──UDP 2055──► backend flow collector ──► SQLite (flow_1m → flow_1h)
   (exporter)                    allow-list · decode ·        │
                                 NAT inside view · top-N      └► Traffic → Flows (Measured)
                                                                 port detail "Top conversations"
```

- **Exporters are identified by their UDP source IP.** Only allow-listed exporters are
  decoded. A RouterOS device the NMS already manages is accepted automatically when its
  exports come from the address the NMS manages it by (setting `flow_auto_accept_devices`,
  on by default). Everything else, such as a firewall, is listed as an *unknown sender*
  until an admin adds it.
- **Observation points.** A point is one exporter plus a set of its interfaces, facing
  either up (towards the Internet) or down (towards hosts). That decides which records are
  downloads and which are uploads. The collector creates a **native** point for every
  exporter interface that carries traffic. **Derived** points show a port on a switch that
  cannot export flows, computed from a neighbouring exporter's records
  ([section 5](#5-switches-that-cannot-export-derived-views)).
- **Exporters are never added together.** The same packet is often seen by several
  exporters (firewall and router, both ends of a trunk). Every view shows exactly one
  exporter's records, so nothing is counted twice.
- **Coverage.** A point that represents a managed port shows what share of that port's
  counter bytes its flows explain.

---

## 2. Enable the collector (NMS side)

| Deploy path | How |
|---|---|
| LXC / bare metal | Uncomment `# MIKROTIK_NMS_FLOW_LISTEN=:2055` in `/etc/mikrotik-nms/env` (the installer adds it commented), then `systemctl restart mikrotik-nms-backend`. See [deploy/lxc/README.md](../deploy/lxc/README.md#flow-collector-netflow--ipfix--sflow) |
| Docker Compose | `FLOW_LISTEN=:2055` in `.env` **and** uncomment `- "2055:2055/udp"` under `backend.ports`. Exporters are identified by their source IP. Rootful Docker, default iptables: kept, nothing else to do. Rootful with `"iptables": false` (userland proxy for all traffic): `network_mode: host` for the backend. Rootless Docker: `network_mode: host` does **not** reach the real host — keep the port mapping and set `DOCKERD_ROOTLESS_ROOTLESSKIT_PORT_DRIVER=slirp4netns` in the rootless daemon's environment (`~/.config/systemd/user/docker.service.d/override.conf`, then restart the user service), or use the pasta network driver with the implicit port driver. Check that the exporter's real IP, not the Docker gateway, shows under `unknown_senders` or `exporters` |
| Kubernetes | Uncomment `MIKROTIK_NMS_FLOW_LISTEN` in `configmap.yaml`, then `kubectl apply -f deploy/k8s/optional/flow-service.yaml` (UDP LoadBalancer, `externalTrafficPolicy: Local`) |
| RouterOS container | Container env `MIKROTIK_NMS_FLOW_LISTEN=:2055`, plus a plain dst-nat of udp/2055 to the container, never masquerade (see the README) |

Then check:

- **The port is open.** On Linux, `ss -lunp | grep ':2055 '`. The backend log shows
  `flow collector:` lines; a bind failure appears in `listen_errors` of
  `GET /api/v1/flows/status`.
- **The address is stable.** Exporters take a literal IP, so give the NMS host a DHCP
  reservation or a static address.
- **Exports can reach it.** Nothing between exporter and NMS may drop udp/2055 or SNAT it.
- **Setup hints show the right address** (optional). Without a setting, the hints use the
  listen address when `MIKROTIK_NMS_FLOW_LISTEN` names one specific IP (e.g. `10.0.0.5:2055`),
  else the address the NMS routes toward the device. If the NMS has several addresses,
  set **Settings → Flow collector → Advertise address** (`flow_advertise_address`). The
  RouterOS commands and OPNsense steps under Traffic → Flows then use it.

---

## 3. RouterOS (traffic-flow, IPFIX)

Works on any RouterOS 7 router. Traffic Flow only sees packets that pass through the
**CPU**: routed, NATed and firewalled traffic. Hardware-offloaded bridging and switching
are invisible, and FastTrack can hide packets too. That is why switches get derived views
([section 5](#5-switches-that-cannot-export-derived-views)) and why the coverage figure
exists.

**Pre-check (read-only).** A router that has never exported shows `enabled=no`, the
default `active-flow-timeout=30m`, and no targets.

```routeros
/ip traffic-flow print
/ip traffic-flow target print
/system resource print
```

**Apply (2 commands).** The Traffic → Flows setup hints show these with your real
addresses (`GET /api/v1/flows/setup?device=<id>`).

```routeros
/ip traffic-flow target add dst-address=<NMS-IP> port=2055 src-address=<router-IP> version=ipfix v9-template-timeout=1m
/ip traffic-flow set enabled=yes interfaces=all active-flow-timeout=1m inactive-flow-timeout=15s
```

- **`src-address=<router-IP>`** pins the export source to the address the NMS manages
  the router by, so it is accepted automatically and attributed to the right device. Without
  it, RouterOS picks the address of the egress interface, and you may have to add the
  exporter by hand.
- **`interfaces=all`** is safe: each packet is accounted once, and the collector drops the
  router's own export packets (`self_export_dropped`).
- **`active-flow-timeout=1m`** makes long downloads show up within a minute instead of 30.
- **`v9-template-timeout=1m`** resends templates every minute. A restarted collector can
  decode again quickly, although the NMS also keeps the last templates in its database.
- **NAT.** IPFIX carries the post-NAT addresses. For masqueraded downloads the collector
  stores the **inside** host, so a WAN point shows your LAN hosts, never the router's
  public address.
- **Interface names.** The flow ifIndex is the RouterOS interface `.id` read as hex (`*D` = 13).
  The NMS syncs the names every 5 minutes over the API it already uses.

**Verify.**

```routeros
/ip traffic-flow print
/ip traffic-flow target print detail
/system resource print
```

`cpu-load` should barely move. At home-lab rates the cost is a hash update per packet on the
CPU path. Within about two minutes the router appears in `GET /api/v1/flows/status` with
`state: "ok"` and `protocol: "ipfix"`.

**Rollback** (restores the defaults):

```routeros
/ip traffic-flow set enabled=no active-flow-timeout=30m
/ip traffic-flow target remove [find dst-address=<NMS-IP> port=2055]
```

---

## 4. OPNsense (NetFlow v9)

OPNsense exports with FreeBSD's in-kernel `ng_netflow`. It offers NetFlow v5 or v9 (no
IPFIX), accounts every packet (no sampling), and captures per interface.

**Capture the LAN side only.** Each captured interface accounts the packets that cross it,
so a routed packet is recorded once on every captured interface it passes through. Copies
captured on the WAN are post-NAT and cannot be attributed to a host. With only the LAN
captured, LAN ingress records are the uploads and LAN egress records are the downloads,
each exactly once and with real host addresses.

**0. Snapshot.** Download the configuration first (System → Configuration → Backups).
Then open **Reporting → NetFlow → Capture**, switch on **advanced mode**, and write down
every current value: listening interfaces, WAN interfaces, capture local, version,
destinations, active and inactive timeout.

**Case A: NetFlow is off** (no listening interfaces). This gives the cleanest feed.

| Field | Value |
|---|---|
| Listening interfaces | your **LAN** interface(s) only |
| WAN interfaces | empty |
| Capture local | leave as found |
| Version | **v9** |
| Destinations | `<NMS-IP>:2055` |
| Active Timeout / Inactive Timeout | **60** / 15 |

Click **Apply**.

**Case B: NetFlow or Insight is already on.**

- Keep the existing listening interfaces, WAN interfaces, capture local setting and
  destinations.
- Make sure the LAN is listening, **add** `<NMS-IP>:2055`, and set the Active Timeout to
  60.
- Click **Apply**.
- In the NMS, put the firewall's WAN transit networks into `flow_external_prefixes` (for
  example a private LTE or ISP transfer net). WAN-side copies then classify correctly, and
  the firewall's LAN points drop them.

**Notes.**

- **Apply** restarts NetFlow. While each capture node is rebuilt, inbound packets on that
  interface are dropped for milliseconds, so apply off-peak.
- **Never** click *Reset NetFlow Data* (`/api/diagnostics/netflow/reset`). It erases the
  Insight history and is not needed for export.
- **Templates.** OPNsense resends v9 templates only every 600 s or 500 packets. The first
  time, the exporter can sit in `template_state: "waiting"` for up to 10 minutes. The NMS
  stores templates, so later restarts decode immediately.
- **API alternative.** Use a dedicated API user that holds only the "Diagnostics: Netflow
  configuration" privilege:
  1. `GET /api/diagnostics/netflow/getconfig` to snapshot the settings.
  2. `POST /api/diagnostics/netflow/setconfig` with the merged lists. `interfaces`,
     `egress_only` and `targets` are replaced as a whole list, so merge them with the
     snapshot.
  3. `POST /api/diagnostics/netflow/reconfigure`.

**In the NMS (admin),** under Traffic → Flows → exporter status:

1. Click **Add** on the firewall's unknown-sender row (or add it by hand):
   - **kind** `opnsense`
   - **address** = the source of its exports, normally its address on the network facing
     the NMS
   - **NAT addresses** = the firewall's other addresses that its NATed traffic shows up as
     on other exporters, for example its leg on a router's LAN. Rows with those addresses
     are labelled "*firewall* (NAT)" instead of being counted as a host.
2. Once flows arrive, open the exporter's **Interfaces** dialog. The learned hint lists the top source prefixes
   per ifIndex. Name the LAN index and give it the role `lan`. Roles decide the facing
   (`lan` faces down, `wan`/`vpn` face up).

**Rollback.** Restore the noted values on the NetFlow page and click **Apply**. If NetFlow
was off before, clear Destinations and Listening interfaces, then Apply. Alternatively,
revert the NetFlow revision under System → Configuration → History, then click Apply on
the NetFlow page.

---

## 5. Switches that cannot export: derived views

Many managed switches forward in hardware. MikroTik CRS switches (Marvell switch chips) are
an example. The CPU sees almost none of the forwarded traffic, so RouterOS Traffic Flow on
such a switch exports next to nothing, and many models have no sFlow either. Forcing the
traffic through the CPU (disabling hardware offload, or `use-ip-firewall=yes`) collapses
throughput on a small switch CPU. **Don't.**

Instead, the NMS derives a view of the switch port from the exporter next to it. It offers
two rules. An admin adds the views with **Traffic → Flows → Add suggested views**, or with
`GET /api/v1/flows/points/suggestions` and then `POST /api/v1/flows/points`.

| Rule | When | What the view shows |
|---|---|---|
| **Gateway host** | A switch port whose only neighbour is a flow-exporting firewall, for example the firewall's LAN NIC is plugged in there (the NMS detects this as the switch's `gateway-host` uplink) | The firewall's LAN-interface records: all IP traffic on that port. Non-IP frames (ARP, LLDP, STP) are not included |
| **Router trunk** | A switch port that is the trunk to a flow-exporting RouterOS router | The router's L3 interfaces carried over that trunk (the VLAN interfaces and bridge for the trunk's VLANs): routed and router-terminated traffic crossing the trunk. L2-switched VLAN traffic that the router does not route is invisible. Hosts on the router's other ports in the same VLANs are excluded. The view is offered for both ends of the trunk |

Each derived view carries a note that explains what it covers. Its **coverage** compares
flow bytes with the port's counter bytes over the minutes that both cover:

- **0.85–1.0 is healthy.** Flows count L3 bytes, counters count L2 bytes.
- **Below 0.7 the UI turns amber.** Typical causes are hardware-offloaded or FastTracked
  traffic, L2-only traffic on a trunk, UDP drops, or a gap while the collector waited for a
  template.

### Optional: port mirror + software probe

If you need wire-level truth, including L2-only traffic:

1. Mirror the port to a spare switch port.
2. Cable the spare port to a probe host.
3. Run a software exporter on the probe, for example
   `softflowd -i <nic> -v 10 -n <NMS-IP>:2055 -t maxlife=60`.
4. Add the probe as an exporter of kind `other`.

Caveats:

- The mirror target must keep up. A busy 10G trunk mirrored into a 1G port drops copies
  silently.
- The probe's flows are keyed by the probe's own NIC, so they appear as one native point.
  The NMS does not attribute them per mirrored port.

Mirroring syntax differs between switch chips and RouterOS versions. The following is an
example for CRS3xx; check your model's manual before applying.

```routeros
/interface bridge port set [find interface=<spare-port>] disabled=yes
/interface ethernet switch set switch1 mirror-target=<spare-port>
/interface ethernet switch port set <port> mirror-ingress=yes mirror-egress=yes
# rollback
/interface ethernet switch port set <port> mirror-ingress=no mirror-egress=no
/interface ethernet switch set switch1 mirror-target=none
/interface bridge port set [find interface=<spare-port>] disabled=no
```

---

## 6. NMS-side checklist

1. Enable the collector and confirm it listens ([section 2](#2-enable-the-collector-nms-side)).
2. Configure the exporters ([section 3](#3-routeros-traffic-flow-ipfix) and
   [section 4](#4-opnsense-netflow-v9)).
3. Managed RouterOS devices are accepted automatically. Add the others from the
   unknown-sender list.
4. For exporters without a device sync (firewalls, probes), name the interfaces and set
   their roles.
5. Add the suggested derived views.
6. Optional settings:
   - `flow_internal_prefixes`: extra internal networks, such as a LAN that no managed
     device routes.
   - `flow_external_prefixes`: private networks that are really "outside", such as WAN
     transit.
   - The exporter's **sampling override**: for a device that samples without announcing
     its rate.

---

## 7. Verify

Log in once, then query the status endpoint (any logged-in user can read it):

```bash
NMS=http://<NMS-IP>        # or your NMS URL
TOKEN=$(curl -s -X POST "$NMS/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"…"}' | jq -r .access_token)
curl -s -H "Authorization: Bearer $TOKEN" "$NMS/api/v1/flows/status" | jq '{enabled, listen, listen_errors,
  exporters: [.exporters[] | {name, address, state, protocol, template_state, sampling_rate, time_source}],
  unknown_senders: [.unknown_senders[] | {address, protocol, datagrams}]}'
```

- **Every exporter is healthy:** `state: "ok"`, the expected `protocol`, and
  `template_state` `ok` (or `persisted` right after a restart).
- **Traffic → Flows** with source **Measured (flow export)** shows the points. Data lags
  about 2–3 minutes, because a minute is written once it is safely complete.
- **Coverage** is sensible for points on managed ports:
  `GET /api/v1/flows/coverage?point=<id>&range=1h` returns `ratio.download` and
  `ratio.upload`.
- A port's detail page shows a **Top conversations** card whenever a point covers that port.

| Symptom | Likely cause |
|---|---|
| Exporter `state: "never"` | Exports don't arrive: wrong target IP or port, a firewall on the path, or the collector is not listening. Check `listen_errors` |
| Exporter shows under `unknown_senders` only | Add it. For a RouterOS device, its export source differs from its NMS address: set `src-address=` |
| `template_state: "waiting"` | Normal for a first start. RouterOS resends within about 1 minute (with `v9-template-timeout=1m`), OPNsense within 10 minutes |
| Coverage well below 0.85 | Hardware offload or FastTrack on the exporter, L2-only traffic on a trunk view, drops (`global.kernel_dropped`, `global.queue_dropped`), or a template gap |
| `counters.rate_limited` growing | The exporter sends more than `flow_rate_limit_pps` datagrams per second. Raise the setting |
| Times look shifted | Check the exporter's clock. `clock_skew_ms` shows the offset, and the collector corrects offsets above 5 s |

---

## 8. Settings

All settings are runtime-tunable on **Settings → Flow collector** (admin) and are picked up
within 30 seconds. `MIKROTIK_NMS_FLOW_LISTEN` is the only environment variable (restart
required).

| Setting | Default | Range | Meaning |
|---|---|---|---|
| `flow_top_n` | 50 | 10–500 | Conversations kept per interface pair per minute. The rest folds into an exact "other" row |
| `flow_top_n_hourly` | 100 | 20–2000 | The same, for the hourly rollup |
| `flow_max_rows_per_minute` | 1500 | 200–10000 | Global cap on stored rows per exporter per minute |
| `flow_1m_days` | 3 | 1–14 | Retention of 1-minute detail |
| `flow_1h_days` | 90 | 7–730 | Retention of hourly rollups |
| `flow_rate_limit_pps` | 2000 | 100–50000 | Datagrams per second accepted per exporter |
| `flow_auto_accept_devices` | true | bool | Accept exports from managed devices' addresses automatically |
| `flow_internal_prefixes` | — | CIDR/IP list | Extra networks treated as internal |
| `flow_external_prefixes` | — | CIDR/IP list | Networks treated as external even if private (e.g. WAN transit) |
| `flow_resolve_ptr` | false | bool | Allow reverse-DNS names for external IPs (sends PTR queries) |
| `flow_advertise_address` | — | IP | Collector address shown in the setup hints |

**Storage.** At the defaults, a few exporters take roughly 150 MB of database space, about
500 MB in the worst case (the row cap hit every minute).

---

## 9. Privacy and security

- **Who can see what.** Per-host flow detail (which internal host talked to which address,
  on which port, how much) is visible to **every logged-in user**, like the Clients page.
  Only admins can change exporters, interfaces, views and settings.
- **What is stored.** Addresses, ports, protocol, and bytes, packets and flows per minute or
  hour. No payload and no URLs. Reverse-DNS lookups are off unless `flow_resolve_ptr` is
  enabled.
- **Removing it.** History expires with the retention settings. Admins can purge it with the
  **Flow history** option of the history purge on the Settings page.
- **Unauthenticated input.** Flow export is plain UDP:
  - The collector checks the source address against the allow-list before decoding
    anything.
  - Unknown senders are listed but never decoded.
  - Each exporter is rate-limited, and all internal structures are bounded.
  - A spoofer on your network can still inject false statistics, so restrict udp/2055 to
    your exporters with a host or network firewall where that matters.
