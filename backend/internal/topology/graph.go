package topology

// Node represents a device in the topology graph.
type Node struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Type       string `json:"type"`    // router, switch, ap, unknown
	Status     string `json:"status"`  // online, offline, unknown
	Model      string `json:"model"`
	ROSVersion string `json:"ros_version"`
	CPULoad    *int   `json:"cpu_load"`
	Address    string `json:"address"`
	Managed    bool   `json:"managed"`
}

// Edge represents a link between two devices.
type Edge struct {
	ID              string `json:"id"`
	Source          string `json:"source"`
	Target          string `json:"target"`
	SourceInterface string `json:"source_interface"`
	TargetInterface string `json:"target_interface"`
	LinkType        string `json:"link_type"` // ethernet, wireless, vpn
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
