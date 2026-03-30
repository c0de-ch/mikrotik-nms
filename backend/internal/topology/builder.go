package topology

import (
	"database/sql"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// Builder constructs a deduplicated link topology from raw neighbor data.
type Builder struct {
	db *sql.DB
}

func NewBuilder(db *sql.DB) *Builder {
	return &Builder{db: db}
}

// Build reads all neighbors and devices, resolves links, and writes them to the links table.
// Returns the full topology graph for broadcasting.
func (b *Builder) Build() (*Graph, error) {
	devices, err := queries.ListDevices(b.db)
	if err != nil {
		return nil, err
	}

	neighbors, err := queries.ListAllNeighbors(b.db)
	if err != nil {
		return nil, err
	}

	// Build lookup maps
	deviceByID := make(map[string]queries.Device)
	deviceByAddress := make(map[string]queries.Device)
	deviceByIdentity := make(map[string]queries.Device)
	for _, d := range devices {
		deviceByID[d.ID] = d
		if d.Address != "" {
			deviceByAddress[d.Address] = d
		}
		if d.Identity != "" {
			deviceByIdentity[d.Identity] = d
		}
	}

	// Build interface MAC → device lookup
	macToDevice := make(map[string]macDeviceInfo)
	for _, d := range devices {
		ifaces, err := queries.ListInterfacesByDevice(b.db, d.ID)
		if err != nil {
			continue
		}
		for _, iface := range ifaces {
			if iface.MACAddress != "" {
				macToDevice[strings.ToUpper(iface.MACAddress)] = macDeviceInfo{
					DeviceID:      d.ID,
					InterfaceName: iface.Name,
					InterfaceType: iface.Type,
				}
			}
		}
	}

	// Resolve neighbors → directed edges
	type directedEdge struct {
		fromDeviceID   string
		fromInterface  string
		toDeviceID     string
		toInterface    string
		linkType       string
		discoveredBy   string
	}

	var directed []directedEdge

	for _, n := range neighbors {
		if n.NeighborMAC == "" {
			continue
		}

		// Resolve neighbor to a managed device
		toDeviceID, toInterface := b.resolveNeighbor(n, macToDevice, deviceByAddress, deviceByIdentity)
		if toDeviceID == "" {
			continue // Unresolved neighbor — skip for now
		}

		// Determine link type from local interface
		lt := "ethernet"
		if info, ok := macToDevice[strings.ToUpper(n.NeighborMAC)]; ok {
			if isWirelessType(info.InterfaceType) {
				lt = "wireless"
			}
		}

		directed = append(directed, directedEdge{
			fromDeviceID:  n.DeviceID,
			fromInterface: n.LocalInterface,
			toDeviceID:    toDeviceID,
			toInterface:   toInterface,
			linkType:      lt,
			discoveredBy:  n.DiscoveredBy,
		})
	}

	// Deduplicate: canonical ordering (min_id, max_id) to avoid A→B and B→A duplicates
	type linkKey struct {
		deviceA    string
		interfaceA string
		deviceB    string
		interfaceB string
	}

	seen := make(map[linkKey]directedEdge)
	for _, de := range directed {
		var key linkKey
		if de.fromDeviceID < de.toDeviceID {
			key = linkKey{de.fromDeviceID, de.fromInterface, de.toDeviceID, de.toInterface}
		} else if de.fromDeviceID > de.toDeviceID {
			key = linkKey{de.toDeviceID, de.toInterface, de.fromDeviceID, de.fromInterface}
		} else {
			// Same device (loopback link) — skip
			continue
		}
		// Keep the first one seen; both directions carry the same info
		if _, exists := seen[key]; !exists {
			seen[key] = de
		}
	}

	// Write links to database
	for key, de := range seen {
		link := &queries.Link{
			ID:           uuid.NewString(),
			DeviceAID:    key.deviceA,
			InterfaceA:   key.interfaceA,
			DeviceBID:    key.deviceB,
			InterfaceB:   key.interfaceB,
			LinkType:     de.linkType,
			DiscoveredBy: de.discoveredBy,
			Status:       "up",
		}
		if err := queries.UpsertLink(b.db, link); err != nil {
			log.Printf("topology: upsert link: %v", err)
		}
	}

	// Mark stale links
	staleCutoff := time.Now().Add(-2 * time.Minute) // 2x the 60s topology interval
	if err := queries.MarkStaleLinksDown(b.db, staleCutoff); err != nil {
		log.Printf("topology: mark stale links: %v", err)
	}

	// Remove very old links (24h)
	oldCutoff := time.Now().Add(-24 * time.Hour)
	if _, err := queries.DeleteOldLinks(b.db, oldCutoff); err != nil {
		log.Printf("topology: delete old links: %v", err)
	}

	// Build output graph
	return b.buildGraph(devices)
}

// resolveNeighbor tries to match a neighbor to a managed device.
// Priority: MAC → IP → identity.
func (b *Builder) resolveNeighbor(
	n queries.Neighbor,
	macToDevice map[string]macDeviceInfo,
	deviceByAddress map[string]queries.Device,
	deviceByIdentity map[string]queries.Device,
) (deviceID string, ifaceName string) {
	mac := strings.ToUpper(n.NeighborMAC)

	// 1. Match by MAC address → find which device owns this MAC
	if info, ok := macToDevice[mac]; ok {
		return info.DeviceID, info.InterfaceName
	}

	// 2. Match by IP address
	if n.NeighborAddress != "" {
		if d, ok := deviceByAddress[n.NeighborAddress]; ok {
			iface := n.NeighborInterface
			if iface == "" {
				iface = "unknown"
			}
			return d.ID, iface
		}
	}

	// 3. Match by identity
	if n.NeighborIdentity != "" {
		if d, ok := deviceByIdentity[n.NeighborIdentity]; ok {
			iface := n.NeighborInterface
			if iface == "" {
				iface = "unknown"
			}
			return d.ID, iface
		}
	}

	return "", ""
}

func (b *Builder) buildGraph(devices []queries.Device) (*Graph, error) {
	links, err := queries.ListLinks(b.db)
	if err != nil {
		return nil, err
	}

	// Build set of known device IDs
	nodeIDs := make(map[string]bool, len(devices))

	graph := &Graph{
		Nodes: make([]CyNode, 0, len(devices)),
		Edges: make([]CyEdge, 0, len(links)),
	}

	for _, d := range devices {
		nodeIDs[d.ID] = true
		graph.Nodes = append(graph.Nodes, CyNode{
			Data: Node{
				ID:         d.ID,
				Label:      deviceLabel(d),
				Type:       inferDeviceType(d.Board),
				Status:     d.Status,
				Model:      d.Board,
				ROSVersion: d.ROSVersion,
				CPULoad:    d.CPULoad,
				Address:    d.Address,
				Managed:    true,
			},
		})
	}

	// Only include edges where both endpoints exist as nodes
	for _, l := range links {
		if !nodeIDs[l.DeviceAID] || !nodeIDs[l.DeviceBID] {
			continue
		}
		graph.Edges = append(graph.Edges, CyEdge{
			Data: Edge{
				ID:              l.ID,
				Source:          l.DeviceAID,
				Target:          l.DeviceBID,
				SourceInterface: l.InterfaceA,
				TargetInterface: l.InterfaceB,
				LinkType:        l.LinkType,
				Status:          l.Status,
			},
		})
	}

	return graph, nil
}

type macDeviceInfo struct {
	DeviceID      string
	InterfaceName string
	InterfaceType string
}

func deviceLabel(d queries.Device) string {
	if d.Identity != "" {
		return d.Identity
	}
	return d.Address
}

func inferDeviceType(board string) string {
	if board == "" {
		return "unknown"
	}
	b := strings.ToUpper(board)
	for _, prefix := range []string{"CCR", "RB4011", "RB5009", "HEX", "RB"} {
		if strings.HasPrefix(b, prefix) {
			return "router"
		}
	}
	for _, prefix := range []string{"CSS", "CRS"} {
		if strings.HasPrefix(b, prefix) {
			return "switch"
		}
	}
	for _, prefix := range []string{"CAP", "WAP", "HAP", "AUDIENCE"} {
		if strings.HasPrefix(b, prefix) {
			return "ap"
		}
	}
	return "router"
}

func isWirelessType(ifaceType string) bool {
	t := strings.ToLower(ifaceType)
	return t == "wlan" || t == "wireless" || strings.Contains(t, "wifi") || strings.Contains(t, "60g")
}
