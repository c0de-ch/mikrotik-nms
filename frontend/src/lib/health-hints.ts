// Plain-language explanations for network-health findings: what an event or
// port state means, what usually causes it, and how to fix it. Shown when a
// row on the Network Health page is expanded.

export interface HealthHint {
  meaning: string;
  causes: string[];
  fixes: string[];
}

interface EventCtx {
  event_type: string;
  device_name?: string;
  port_interface?: string;
  bridge_name?: string;
  mac_address?: string;
  message?: string;
}

export function explainEvent(e: EventCtx): HealthHint {
  const port = e.port_interface || "the named port";
  const bridge = e.bridge_name || "the bridge";
  const dev = e.device_name || "the device";

  switch (e.event_type) {
    case "stp_disabled":
      return {
        meaning: `${bridge} on ${dev} has multiple non-edge ports but spanning tree is off — a single looped cable on this bridge would flood the whole L2 segment with no protocol to stop it.`,
        causes: [
          "Bridge created with protocol-mode=none (RouterOS default in some configs/migrations).",
          "STP intentionally disabled at some point and never re-enabled.",
        ],
        fixes: [
          `Enable RSTP: /interface bridge set ${e.bridge_name || "<bridge>"} protocol-mode=rstp`,
          "Keep one core switch as STP root (lower priority, e.g. 0x1000) so re-elections are deterministic.",
          "If this bridge genuinely never sees a second path (single uplink, no user ports), you can acknowledge it — but RSTP costs nothing.",
        ],
      };
    case "tcn_storm":
      return {
        meaning: `${bridge} on ${dev} is receiving STP topology-change notifications continuously. Every TCN makes all switches flush their MAC tables, causing brief flooding — this shows up as network-wide micro-drops (audio blips, lag spikes).`,
        causes: [
          "A port that is flapping (link up/down repeatedly) on a NON-edge port — each transition emits a TCN. Check the Flapping counter in Port State below and match timestamps.",
          "A real loop or an unstable downstream switch/AP rebooting repeatedly.",
          "Client ports not marked as edge: a PC rebooting then triggers TCNs it shouldn't.",
        ],
        fixes: [
          "Find the flapping port (Port State → Recent flaps) and fix its cable/device — that usually ends the storm.",
          "Set edge=yes on access ports (client/server/AP-less ports) so their link changes never generate TCNs: /interface bridge port set [find interface=<port>] edge=yes",
          "If the counter climbs but nothing is flapping, look for a loop: check loop_detected/mac_flap events on the same bridge.",
        ],
      };
    case "loop_detected":
      return {
        meaning: `${dev} saw its own traffic come back on ${port} — a physical L2 loop. Broadcast frames multiply until the segment melts down; this is the most urgent event type.`,
        causes: [
          "A patch cable plugged back into the same switch or into a second switch on the same segment.",
          "An unmanaged mini-switch/hub connecting two wall drops.",
          "The same L2 bridged over two parallel paths (e.g. a VLAN spanned via two links without STP blocking one).",
        ],
        fixes: [
          `Physically trace ${port} on ${dev} and remove the redundant cable/device.`,
          "Enable loop-protect on access ports so the port auto-disables next time: /interface ethernet set [find] loop-protect=on (NOTE: on CRS3xx switches applying this reinitialises the switch chip — ~4s all-port blip; do it in a maintenance window).",
          "Keep loop-protect OFF on inter-switch trunks (RSTP handles those; loop-protect could disable a legitimate uplink).",
        ],
      };
    case "mac_flap":
      return {
        meaning: `MAC ${e.mac_address || "(see row)"} keeps moving between ports on ${dev} — the same address is being learned alternately on two interfaces.`,
        causes: [
          "A WiFi client roaming between APs that uplink to different switch ports — benign if the MAC is a wireless client (check the Clients/WiFi page).",
          "A real L2 loop making frames arrive from both directions.",
          "A dual-homed host (LACP/bond misconfigured, or a VM migrating between hypervisor uplinks).",
          "Two devices sharing a cloned/VRRP MAC.",
        ],
        fixes: [
          "Identify the MAC's owner first (Clients page). Wireless client roaming → acknowledge, it's normal.",
          "If wired: check the two ports involved for a loop between them, or fix the host's bonding config.",
        ],
      };
    case "bpdu_on_edge":
      return {
        meaning: `${port} on ${dev} is marked as an edge port (expected: end device) but received an STP BPDU — something switch-like is plugged into it.`,
        causes: [
          "A user connected a switch, AP, or another router to a client port.",
          "The port is genuinely a trunk/uplink but was misclassified as edge.",
        ],
        fixes: [
          "If a switch belongs there: set edge=no (or auto) on that bridge port so STP treats it as a proper link.",
          "If not: find and remove the rogue device — it could form a loop or hijack STP.",
          "Consider bpdu-guard=yes on true client ports to auto-disable them when this happens.",
        ],
      };
    case "port_disabled":
      return {
        meaning: `${port} on ${dev} was administratively disabled (or auto-disabled by a protection feature).`,
        causes: [
          "Someone disabled it on purpose (check the port comment).",
          "loop-protect or bpdu-guard tripped and shut the port (see message / port_loop_protect events).",
        ],
        fixes: [
          "If intentional: acknowledge.",
          "If a protection tripped: fix the underlying loop/rogue device first, then re-enable: /interface ethernet enable <port> (loop-protect auto-recovers after ~5min by default).",
        ],
      };
    case "port_link_down":
      return {
        meaning: `${port} on ${dev} lost link.`,
        causes: [
          "The connected device was turned off, unplugged, or rebooted.",
          "Cable/SFP failure, or PoE power budget cut.",
        ],
        fixes: [
          "Check whether the far-end device is supposed to be on; if this is a client that simply left, acknowledge.",
          "If it should be up: reseat/replace the cable or SFP, check PoE status on the port.",
        ],
      };
    case "port_link_flap":
      return {
        meaning: `${port} on ${dev} bounced up/down repeatedly within the monitoring window. Each flap on a non-edge port also triggers an STP topology change felt network-wide.`,
        causes: [
          "A marginal cable — especially when the link renegotiates between 1G and 100M (one damaged wire pair). A cable can look fine and still fail gigabit.",
          "Energy-Efficient-Ethernet / green-ethernet on the client NIC aggressively idling the link.",
          "Failing NIC, switch port, or SFP; PoE brownouts.",
        ],
        fixes: [
          "Swap to a known-good cable AND a different switch port (isolates cable vs port vs NIC).",
          "Disable EEE/green-ethernet on the connected host's NIC.",
          "If it keeps flapping with everything swapped, the host NIC is the suspect.",
          "Set edge=yes on this bridge port so the flaps at least stop causing fleet-wide TCN flushes while you troubleshoot.",
        ],
      };
    case "port_loop_protect":
      return {
        meaning: `loop-protect on ${dev} saw its own probe frame return on ${port} and disabled the port — a loop existed behind it.`,
        causes: [
          "A looped cable or looping mini-switch downstream of this port.",
        ],
        fixes: [
          `Trace and remove the loop behind ${port}.`,
          "The port auto-recovers after the loop-protect-disable-time (default 5min) once the loop is gone; or re-enable manually.",
        ],
      };
    case "ip_address_changed":
      return {
        meaning: `${dev} was rediscovered under a different IP address and the NMS followed it.`,
        causes: [
          "The device's management IP comes from DHCP and the lease changed.",
          "Multi-homed device (several SVIs) answering discovery from another address.",
        ],
        fixes: [
          "Give the device a DHCP reservation (or static address) for its management IP so monitoring history stays continuous.",
        ],
      };
    default:
      return {
        meaning: e.message || "Anomaly reported by the device.",
        causes: ["See the raw log message in the Detail column."],
        fixes: ["Check the device's log (/log print) around the event time for context."],
      };
  }
}

// Hints for the Port State table rows.
export function explainPortState(p: {
  interface_name: string;
  running: boolean;
  disabled: boolean;
  flap_count_window: number;
  loop_protect_status: string;
}): HealthHint | null {
  if (p.flap_count_window >= 3) {
    return explainEvent({ event_type: "port_link_flap", port_interface: p.interface_name });
  }
  if (p.disabled) {
    return explainEvent({ event_type: "port_disabled", port_interface: p.interface_name });
  }
  if (!p.running) {
    return explainEvent({ event_type: "port_link_down", port_interface: p.interface_name });
  }
  return null;
}
