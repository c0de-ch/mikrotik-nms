// Plain-language explanations for network-health findings: what an event or
// port state means, what usually causes it, and how to fix it. Shown when a
// row on the Network Health page is expanded.
//
// Rules for anything that changes a device (P0-11):
//  - a command is shown only for the port the finding is about, and only when
//    the NMS knows that port is an end-device port (role access or wireless).
//    Edge, bpdu-guard and loop-protect on a link between switches can take the
//    link — and everything behind it — down;
//  - a per-port command names that port. Never a bare `[find]`, which would
//    hit trunks, uplinks and every other port of the device;
//  - a step that interrupts traffic carries `window`: do it in a maintenance
//    window.

import type { PortRoleName } from "@/lib/api";

export interface HintFix {
  text: string;
  // A RouterOS command that changes the device. Shown apart from the text.
  command?: string;
  // The step interrupts traffic (reconvergence, a switch-chip reset): plan a
  // maintenance window for it.
  window?: boolean;
}

export interface HealthHint {
  meaning: string;
  causes: string[];
  fixes: HintFix[];
}

export interface EventCtx {
  event_type: string;
  device_name?: string;
  port_interface?: string;
  bridge_name?: string;
  mac_address?: string;
  message?: string;
  // What the NMS knows about the port the finding names: its role in the
  // topology ("" or undefined = not known) and the device's board.
  port_role?: PortRoleName | "";
  board?: string;
  // MAC addresses learned behind the port: more than one means a switch or
  // an AP that the NMS does not manage sits behind it.
  port_macs?: number;
}

const fix = (text: string, extra?: Omit<HintFix, "text">): HintFix => ({ text, ...extra });

// An end-device port: the only kind per-port protections are suggested for.
export function isEndDevicePort(role: PortRoleName | "" | undefined): boolean {
  return role === "access" || role === "wireless";
}

// A link the topology depends on: to another managed device, or upstream.
function isInfrastructurePort(role: PortRoleName | "" | undefined): boolean {
  return role === "uplink" || role === "downlink" || role === "peer" || role === "wan";
}

const ROLE_WORDS: Record<PortRoleName, string> = {
  wan: "the WAN port",
  uplink: "the uplink to the next switch",
  downlink: "a link to a downstream switch",
  peer: "a link to another managed device",
  access: "an access port",
  wireless: "a wireless interface",
  vpn: "a VPN interface",
  virtual: "a virtual interface",
  idle: "an idle port",
};

// CRS3xx / CRS5xx: per-port storm control lives on the switch chip, and
// changing loop-protect reinitialises that chip.
function isCRS3xx(board: string | undefined): boolean {
  return /^CRS[35]\d\d/i.test((board || "").trim());
}

// An interface name as a RouterOS console word: plain when it is made of
// letters, digits, ".", "_", "+" and "-"; quoted otherwise ("ether5 printer"
// would otherwise be two words — a syntax error, or a command on "ether5").
export function rosName(name: string): string {
  if (/^[A-Za-z0-9._+-]+$/.test(name)) return name;
  return `"${name.replace(/[\\"$]/g, "\\$&")}"`;
}

// The same inside a [find …] value, which is always quoted.
const rosQuoted = (name: string) => `"${name.replace(/[\\"$]/g, "\\$&")}"`;

// A bridge port has no name of its own: it is addressed by its interface.
const bridgePort = (port: string) => `/interface bridge port set [find interface=${rosQuoted(port)}]`;

// Why no command is offered for this port.
function noCommandReason(port: string, role: PortRoleName | "" | undefined): HintFix {
  if (isInfrastructurePort(role)) {
    return fix(
      `${port} is ${ROLE_WORDS[role as PortRoleName]}: do not set edge, bpdu-guard or loop-protect on it — they could take the link and everything behind it down. Spanning tree protects links between switches.`,
    );
  }
  return fix(
    `The NMS does not know what is connected to ${port}, so it suggests no port protection here. They belong on end-device ports only, never on a link between switches.`,
  );
}

// The protections for one end-device port, most useful first. The bridge-port
// settings apply to any bridge port; storm control and loop-protect are
// Ethernet settings, so they are offered for access ports only.
function endDevicePortProtection(
  port: string,
  role: PortRoleName | "" | undefined,
  board: string | undefined,
  macs: number | undefined,
): HintFix[] {
  const crs = isCRS3xx(board);
  const fixes: HintFix[] = [];
  if ((macs ?? 0) > 1) {
    fixes.push(
      fix(
        `${macs} addresses are learned behind ${port}: a switch or an access point the NMS does not manage is connected there. If it speaks spanning tree, bpdu-guard will disable ${port} — decide first whether that device belongs there.`,
      ),
    );
  }
  fixes.push(
    fix(`Let the bridge take ${port} out of service when a switch shows up behind it (its bridge port then stays disabled until it is disabled and enabled by hand):`, {
      command: `${bridgePort(port)} edge=yes-discover bpdu-guard=yes`,
    }),
  );
  if (role !== "access") return fixes;
  if (crs) {
    fixes.push(
      fix(`Limit broadcast storms on ${port} in the switch chip (percent of the link speed):`, {
        command: `/interface ethernet switch port set ${rosName(port)} storm-rate=1 limit-broadcasts=yes limit-unknown-unicasts=yes`,
      }),
    );
  }
  fixes.push(
    fix(
      crs
        ? `Optional: loop-protect on ${port}. On this switch, changing loop-protect reinitialises the switch chip — every port drops for about 4 seconds.`
        : `Optional: loop-protect on ${port}, so the port disables itself when its own probe frame comes back.`,
      { command: `/interface ethernet set ${rosName(port)} loop-protect=on`, window: crs },
    ),
  );
  return fixes;
}

export function explainEvent(e: EventCtx): HealthHint {
  const port = e.port_interface || "the named port";
  const bridge = e.bridge_name || "the bridge";
  const dev = e.device_name || "the device";
  const namedPort = e.port_interface || "";
  const endDevice = namedPort !== "" && isEndDevicePort(e.port_role);

  switch (e.event_type) {
    case "stp_disabled":
      return {
        meaning: `${bridge} on ${dev} has multiple non-edge ports but spanning tree is off — a single looped cable on this bridge would flood the whole L2 segment with no protocol to stop it.`,
        causes: [
          "Bridge created with protocol-mode=none (RouterOS default in some configs/migrations).",
          "STP intentionally disabled at some point and never re-enabled.",
        ],
        fixes: [
          fix("Enable RSTP. The bridge's ports go through listening and learning once, so expect a short interruption on this bridge:", {
            command: `/interface bridge set ${e.bridge_name || "<bridge>"} protocol-mode=rstp`,
            window: true,
          }),
          fix(
            "Keep one core switch as STP root (a lower priority, e.g. 0x1000) so re-elections are deterministic. Changing a priority makes the whole spanning tree re-elect its root.",
            { window: true },
          ),
          fix("If this bridge genuinely never sees a second path (single uplink, no user ports), you can acknowledge it — but RSTP costs nothing."),
        ],
      };
    case "tcn_storm":
      return {
        meaning: `${bridge} on ${dev} is receiving STP topology-change notifications continuously. Every TCN makes all switches flush their MAC tables, causing brief flooding — this shows up as network-wide micro-drops (audio blips, lag spikes).`,
        causes: [
          "A port that is flapping (link up/down repeatedly) on a NON-edge port — each transition emits a TCN. The bridge's Topology changes column below shows which port's counter is climbing; match it with Port State → Recent link-downs.",
          "A real loop or an unstable downstream switch/AP rebooting repeatedly.",
          "Client ports not marked as edge: a PC rebooting then triggers TCNs it shouldn't.",
        ],
        fixes: [
          fix("Find the flapping port (highest Topology changes on the bridge, or Port State → Recent link-downs) and fix its cable/device — that usually ends the storm."),
          fix(
            "If that port is an end-device port (a client, a server — not a switch or an AP uplink), mark it as edge so its link changes stop generating TCNs. Open the port's row under Port State: the command is shown there when the NMS knows the port is an access port.",
          ),
          fix("If the counter climbs but nothing is flapping, look for a loop: check loop_detected/mac_flap events on the same bridge."),
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
          fix(`Physically trace ${port} on ${dev} and remove the redundant cable/device.`),
          ...(endDevice
            ? endDevicePortProtection(namedPort, e.port_role, e.board, e.port_macs)
            : [noCommandReason(port, e.port_role)]),
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
          fix("Identify the MAC's owner first (Clients page). Wireless client roaming → acknowledge, it's normal."),
          fix("If wired: check the two ports involved for a loop between them, or fix the host's bonding config."),
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
          namedPort && isInfrastructurePort(e.port_role)
            ? fix(`${port} is ${ROLE_WORDS[e.port_role as PortRoleName]}, so it should not be an edge port. Let spanning tree treat it as a link:`, {
                command: `${bridgePort(namedPort)} edge=auto`,
              })
            : fix("If a switch belongs there: set edge=auto on that bridge port so STP treats it as a proper link."),
          fix("If not: find and remove the rogue device — it could form a loop or hijack STP."),
          ...(endDevice
            ? [
                fix(`To have ${port} disabled automatically the next time this happens:`, {
                  command: `${bridgePort(namedPort)} bpdu-guard=yes`,
                }),
              ]
            : []),
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
          fix("If intentional: acknowledge."),
          fix(
            "If a protection tripped: fix the underlying loop/rogue device first. loop-protect recovers by itself after about 5 minutes by default; a port shut by bpdu-guard comes back when its bridge port is disabled and enabled again.",
          ),
          endDevice
            ? fix(`If it was switched off by hand and should be on:`, { command: `/interface enable ${rosName(namedPort)}` })
            : fix(
                isInfrastructurePort(e.port_role)
                  ? `${port} is ${ROLE_WORDS[e.port_role as PortRoleName]}. Re-enable it only when you know why it is off — a disabled link between switches is often a loop someone broke on purpose.`
                  : "Re-enable it only when you know why it is off — a disabled link between switches is often a loop someone broke on purpose.",
              ),
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
          fix("Check whether the far-end device is supposed to be on; if this is a client that simply left, acknowledge."),
          fix("If it should be up: reseat/replace the cable or SFP, check PoE status on the port."),
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
          fix("Swap to a known-good cable AND a different switch port (isolates cable vs port vs NIC)."),
          fix("Disable EEE/green-ethernet on the connected host's NIC."),
          fix("If it keeps flapping with everything swapped, the host NIC is the suspect."),
          endDevice
            ? fix(`While you troubleshoot, mark ${port} as an edge port so its flaps stop causing fleet-wide TCN flushes:`, {
                command: `${bridgePort(namedPort)} edge=yes-discover`,
              })
            : isInfrastructurePort(e.port_role)
              ? fix(
                  `${port} is ${ROLE_WORDS[e.port_role as PortRoleName]}: do not mark it as edge to quiet the topology changes — an edge port that is really a link between switches can form a loop. Fix the link itself.`,
                )
              : fix(
                  "If this is an end-device port (not a link to a switch or an AP), marking it as an edge port stops its flaps from causing TCN flushes. The NMS does not know what is connected here, so it shows no command.",
                ),
        ],
      };
    case "port_loop_protect":
      return {
        meaning: `loop-protect on ${dev} saw its own probe frame return on ${port} and disabled the port — a loop existed behind it.`,
        causes: [
          "A looped cable or looping mini-switch downstream of this port.",
        ],
        fixes: [
          fix(`Trace and remove the loop behind ${port}.`),
          fix("The port auto-recovers after the loop-protect-disable-time (default 5min) once the loop is gone; or re-enable manually."),
        ],
      };
    case "ip_address_proposed":
      return {
        meaning: `Neighbor discovery saw ${dev}'s MAC at a different IP address. The NMS has not contacted it there — an admin approves or dismisses the move on the Devices page.`,
        causes: [
          "The device's management IP comes from DHCP and the lease changed.",
          "Multi-homed device (several SVIs) answering discovery from another address.",
          "A discovery packet forged by another host (MNDP is unauthenticated) — why the move waits for approval.",
        ],
        fixes: [
          fix("If the new address is expected, approve the move: the NMS logs in there with the stored credential and checks board, version and MAC first."),
          fix("Give the device a DHCP reservation (or static address) for its management IP so it stops moving."),
        ],
      };
    case "ip_address_changed":
      return {
        meaning: `${dev} was rediscovered under a different IP address and, after an admin approved it, the NMS verified the device there and followed it (or refused the move — see the message).`,
        causes: [
          "The device's management IP comes from DHCP and the lease changed.",
          "Multi-homed device (several SVIs) answering discovery from another address.",
        ],
        fixes: [
          fix("Give the device a DHCP reservation (or static address) for its management IP so monitoring history stays continuous."),
        ],
      };
    default:
      return {
        meaning: e.message || "Anomaly reported by the device.",
        causes: ["See the raw log message in the Detail column."],
        fixes: [fix("Check the device's log (/log print) around the event time for context.")],
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
  device_name?: string;
  port_role?: PortRoleName | "";
  board?: string;
  port_macs?: number;
}): HealthHint | null {
  const ctx = {
    port_interface: p.interface_name,
    device_name: p.device_name,
    port_role: p.port_role,
    board: p.board,
    port_macs: p.port_macs,
  };
  if (p.flap_count_window >= 3) {
    return explainEvent({ event_type: "port_link_flap", ...ctx });
  }
  if (p.disabled) {
    return explainEvent({ event_type: "port_disabled", ...ctx });
  }
  if (!p.running) {
    return explainEvent({ event_type: "port_link_down", ...ctx });
  }
  return null;
}
