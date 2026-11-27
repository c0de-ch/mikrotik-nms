package hygiene

import (
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/devclass"
)

// Facts are what the NMS already knows about the device a pass runs on. None
// of them costs the device a command: they come from the inventory read, the
// role classifier, the interface snapshot, the stored bridge port table and
// the link graph.
type Facts struct {
	// Class is the probed CPU class — what the hardware is (C0 while the
	// device was never probed). Role is what it does in the network.
	Class devclass.Class
	Role  devclass.Role
	// RoleReason is the role classifier's sentence, for the explanations.
	RoleReason string
	// RoleMissing: the role could not be told — the lookup failed, or the
	// route facts it rests on are too old to count. Role is then
	// devclass.RoleUnknown for want of facts, not because of what the device
	// is, and every rule that depends on the role judges nothing: what was
	// found before stays, nothing new is raised.
	RoleMissing  bool
	Board        string
	Architecture string
	Version      string // RouterOS version

	// NMSUser is the account the NMS logs in with; NMSAddresses the
	// addresses its own sessions come from (empty = not known: sessions
	// cannot be told apart then).
	NMSUser      string
	NMSAddresses []netip.Addr
	// Collector is the address devices are told to export flows to
	// (flow_advertise_address; "" = not set) and CollectorPort its port.
	Collector     string
	CollectorPort int

	// WifiPackage and LegacyCAPsMAN: the capability probe found the menu.
	WifiPackage   bool
	LegacyCAPsMAN bool

	// InterfacesKnown: a current interface list is at hand, so HasBridge and
	// HasBond are facts and Ports is filled. EthernetKnown: its rows carry
	// the ethernet properties (switch chip, flow control, loop-protect).
	// BridgePortsKnown: the bridge port table (hw, hw-offload, horizon) is
	// merged in.
	InterfacesKnown  bool
	EthernetKnown    bool
	BridgePortsKnown bool
	HasBridge        bool
	HasBond          bool
	Ports            []Port

	// Prev is what the previous pass saw (nil = nothing to compare with);
	// Now is when this pass read the device.
	Prev *Runtime
	Now  time.Time
}

// Port is one interface as the scheduled reads know it.
type Port struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // RouterOS interface type: ether, wifi, vlan, bond, bridge, …
	Running bool   `json:"running"`
	// Switch is the switch chip the port belongs to ("" = none: a port the
	// CPU handles itself, or not an ethernet port at all).
	Switch        string `json:"switch,omitempty"`
	RxFlowControl string `json:"rx_flow_control,omitempty"` // on | off | auto
	TxFlowControl string `json:"tx_flow_control,omitempty"`
	LoopProtect   string `json:"loop_protect,omitempty"` // on | off | default
	// Trunk: the port is an end of a link to another managed device.
	Trunk bool `json:"trunk,omitempty"`
	// Bridge is the bridge the port is a member of ("" = none), with the
	// member's settings. HW and HWOffload are only meaningful when their
	// Known flag is set.
	Bridge         string `json:"bridge,omitempty"`
	BridgeDisabled bool   `json:"bridge_disabled,omitempty"`
	// BridgeInactive: the bridge port table, at its own read, had the port
	// as not in the bridge (down). Its hw-offload is false then by nature.
	BridgeInactive bool   `json:"bridge_inactive,omitempty"`
	HW             bool   `json:"hw,omitempty"`
	HWKnown        bool   `json:"hw_known,omitempty"`
	HWOffload      bool   `json:"hw_offload,omitempty"`
	HWOffloadKnown bool   `json:"hw_offload_known,omitempty"`
	Horizon        string `json:"horizon,omitempty"`
	// Bps is the traffic on the port (rx + tx) as last measured; RateKnown
	// is false while there is no rate for it.
	Bps       int64 `json:"bps,omitempty"`
	RateKnown bool  `json:"rate_known,omitempty"`
}

// HasSwitchChip reports a port that belongs to a switch chip.
func (f Facts) HasSwitchChip() bool {
	for _, p := range f.Ports {
		if p.Switch != "" {
			return true
		}
	}
	return false
}

// offloadBoards are the switches whose chip forwards bridged traffic by
// itself (CRS3xx, CRS5xx): the only ones the offload rules judge.
var offloadBoards = regexp.MustCompile(`(?i)^CRS[35]\d\d`)

// offloadSwitch reports switch hardware whose switch chip offloads bridging.
func (f Facts) offloadSwitch() bool {
	return f.Role == devclass.RoleSwitch && offloadBoards.MatchString(strings.TrimSpace(f.Board))
}

// weak reports C1 hardware.
func (f Facts) weak() bool { return f.Class == devclass.C1 }

// isSelf reports an address the NMS's own sessions come from.
func (f Facts) isSelf(address string) bool {
	a, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, own := range f.NMSAddresses {
		if own.Unmap() == a {
			return true
		}
	}
	return false
}

// name is how the explanations call the device's hardware.
func (f Facts) name() string {
	if b := strings.TrimSpace(f.Board); b != "" {
		return b
	}
	return "this device"
}
