package topology

// Node represents a device in the topology graph.
type Node struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Type       string `json:"type"`    // router, switch, ap, unknown; synthetic: internet, gateway, vpn
	Status     string `json:"status"`  // online, offline, unknown; synthetic nodes: up
	Model      string `json:"model"`
	ROSVersion string `json:"ros_version"`
	CPULoad    *int   `json:"cpu_load"`
	Address    string `json:"address"`
	Managed    bool   `json:"managed"`
	// Gateway nodes only: the managed device + port that learned the
	// gateway's MAC in its bridge FDB — its physical attachment point.
	AttachDeviceID string `json:"attach_device_id,omitempty"`
	AttachPort     string `json:"attach_port,omitempty"`
}

// Edge represents a link between two devices.
type Edge struct {
	ID              string `json:"id"`
	Source          string `json:"source"`
	Target          string `json:"target"`
	SourceInterface string `json:"source_interface"`
	TargetInterface string `json:"target_interface"`
	LinkType        string `json:"link_type"` // ethernet, wireless; synthetic egress: gateway, internet, vpn
	Status          string `json:"status"`    // up, down
}

// Graph is a full topology snapshot for the frontend.
type Graph struct {
	Nodes []CyNode `json:"nodes"`
	Edges []CyEdge `json:"edges"`
}

// Cytoscape-compatible wrappers
type CyNode struct {
	Data Node `json:"data"`
}

type CyEdge struct {
	Data Edge `json:"data"`
}
