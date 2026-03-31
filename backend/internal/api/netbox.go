package api

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// handleNetboxExport generates NetBox-compatible CSV files bundled as JSON.
// Only exports infrastructure data (devices, interfaces, IPs, cables) that
// NetBox doesn't discover on its own.
func (s *Server) handleNetboxExport(w http.ResponseWriter, r *http.Request) {
	devices, err := queries.ListDevices(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list devices")
		return
	}

	links, err := queries.ListLinks(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list links")
		return
	}

	// Build lookup maps
	deviceByID := make(map[string]queries.Device)
	for _, d := range devices {
		deviceByID[d.ID] = d
	}

	// Collect all interfaces
	allIfaces := make(map[string][]queries.Interface)
	for _, d := range devices {
		ifaces, err := queries.ListInterfacesByDevice(s.db, d.ID)
		if err == nil {
			allIfaces[d.ID] = ifaces
		}
	}

	result := map[string]interface{}{
		"manufacturers":   generateManufacturers(),
		"device_types":    generateDeviceTypes(devices),
		"device_roles":    generateDeviceRoles(devices),
		"devices":         generateDevices(devices),
		"interfaces":      generateInterfaces(devices, allIfaces),
		"ip_addresses":    generateIPAddresses(devices, allIfaces),
		"cables":          generateCables(links, deviceByID, allIfaces),
	}

	writeJSON(w, http.StatusOK, result)
}

// handleNetboxExportCSV returns a specific CSV file for NetBox import.
func (s *Server) handleNetboxExportCSV(w http.ResponseWriter, r *http.Request) {
	exportType := r.PathValue("type")

	devices, err := queries.ListDevices(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list devices")
		return
	}

	deviceByID := make(map[string]queries.Device)
	for _, d := range devices {
		deviceByID[d.ID] = d
	}

	allIfaces := make(map[string][]queries.Interface)
	for _, d := range devices {
		ifaces, _ := queries.ListInterfacesByDevice(s.db, d.ID)
		allIfaces[d.ID] = ifaces
	}

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	filename := exportType + ".csv"

	switch exportType {
	case "manufacturers":
		writer.Write([]string{"name", "slug"})
		writer.Write([]string{"MikroTik", "mikrotik"})

	case "device_types":
		writer.Write([]string{"manufacturer", "model", "slug", "u_height", "is_full_depth"})
		seen := make(map[string]bool)
		for _, d := range devices {
			board := d.Board
			if board == "" || seen[board] {
				continue
			}
			seen[board] = true
			slug := slugify(board)
			writer.Write([]string{"MikroTik", board, slug, "1", "true"})
		}

	case "device_roles":
		writer.Write([]string{"name", "slug", "color"})
		writer.Write([]string{"Router", "router", "4caf50"})
		writer.Write([]string{"Switch", "switch", "2196f3"})
		writer.Write([]string{"Access Point", "access-point", "ff9800"})

	case "devices":
		writer.Write([]string{"name", "device_role", "device_type", "manufacturer", "serial", "platform", "status", "primary_ip4", "comments"})
		for _, d := range devices {
			role := netboxDeviceRole(d.Board)
			board := d.Board
			if board == "" {
				board = "Unknown"
			}
			name := d.Identity
			if name == "" {
				name = d.Address
			}
			status := "active"
			if d.Status != "online" {
				status = "offline"
			}
			ip := d.Address + "/32"
			comment := fmt.Sprintf("RouterOS %s, arch: %s", d.ROSVersion, d.Architecture)
			writer.Write([]string{name, role, board, "MikroTik", "", "RouterOS", status, ip, comment})
		}

	case "interfaces":
		writer.Write([]string{"device", "name", "type", "mac_address", "mtu", "enabled", "description"})
		for _, d := range devices {
			devName := d.Identity
			if devName == "" {
				devName = d.Address
			}
			for _, iface := range allIfaces[d.ID] {
				ifType := netboxInterfaceType(iface.Type, iface.Name)
				enabled := "true"
				if iface.Disabled {
					enabled = "false"
				}
				mtu := ""
				if iface.MTU != nil {
					mtu = fmt.Sprintf("%d", *iface.MTU)
				}
				writer.Write([]string{devName, iface.Name, ifType, iface.MACAddress, mtu, enabled, iface.Comment})
			}
		}

	case "ip_addresses":
		writer.Write([]string{"address", "status", "dns_name", "assigned_object_type", "assigned_object_id", "description"})
		for _, d := range devices {
			devName := d.Identity
			if devName == "" {
				devName = d.Address
			}
			// Management IP
			writer.Write([]string{d.Address + "/32", "active", devName, "", "", "Management IP"})
		}

	case "cables":
		writer.Write([]string{"side_a_device", "side_a_type", "side_a_name", "side_b_device", "side_b_type", "side_b_name", "type", "status"})
		links, _ := queries.ListLinks(s.db)
		for _, l := range links {
			devA, okA := deviceByID[l.DeviceAID]
			devB, okB := deviceByID[l.DeviceBID]
			if !okA || !okB {
				continue
			}
			nameA := devA.Identity
			if nameA == "" {
				nameA = devA.Address
			}
			nameB := devB.Identity
			if nameB == "" {
				nameB = devB.Address
			}
			cableType := "cat6"
			if l.LinkType == "wireless" {
				cableType = ""
			}
			status := "connected"
			if l.Status == "down" {
				status = "planned"
			}
			writer.Write([]string{nameA, "dcim.interface", l.InterfaceA, nameB, "dcim.interface", l.InterfaceB, cableType, status})
		}

	default:
		writeError(w, http.StatusBadRequest, "unknown export type. Valid: manufacturers, device_types, device_roles, devices, interfaces, ip_addresses, cables")
		return
	}

	writer.Flush()

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	w.Write(buf.Bytes())
}

func generateManufacturers() []map[string]string {
	return []map[string]string{
		{"name": "MikroTik", "slug": "mikrotik"},
	}
}

func generateDeviceTypes(devices []queries.Device) []map[string]string {
	seen := make(map[string]bool)
	var types []map[string]string
	for _, d := range devices {
		if d.Board == "" || seen[d.Board] {
			continue
		}
		seen[d.Board] = true
		types = append(types, map[string]string{
			"manufacturer": "MikroTik",
			"model":        d.Board,
			"slug":         slugify(d.Board),
			"u_height":     "1",
		})
	}
	return types
}

func generateDeviceRoles(devices []queries.Device) []map[string]string {
	return []map[string]string{
		{"name": "Router", "slug": "router", "color": "4caf50"},
		{"name": "Switch", "slug": "switch", "color": "2196f3"},
		{"name": "Access Point", "slug": "access-point", "color": "ff9800"},
	}
}

func generateDevices(devices []queries.Device) []map[string]string {
	var result []map[string]string
	for _, d := range devices {
		name := d.Identity
		if name == "" {
			name = d.Address
		}
		status := "active"
		if d.Status != "online" {
			status = "offline"
		}
		result = append(result, map[string]string{
			"name":         name,
			"device_role":  netboxDeviceRole(d.Board),
			"device_type":  d.Board,
			"manufacturer": "MikroTik",
			"platform":     "RouterOS",
			"status":       status,
			"primary_ip4":  d.Address + "/32",
			"comments":     fmt.Sprintf("RouterOS %s, %s", d.ROSVersion, d.Architecture),
		})
	}
	return result
}

func generateInterfaces(devices []queries.Device, allIfaces map[string][]queries.Interface) []map[string]string {
	var result []map[string]string
	for _, d := range devices {
		devName := d.Identity
		if devName == "" {
			devName = d.Address
		}
		for _, iface := range allIfaces[d.ID] {
			entry := map[string]string{
				"device":      devName,
				"name":        iface.Name,
				"type":        netboxInterfaceType(iface.Type, iface.Name),
				"mac_address": iface.MACAddress,
				"enabled":     fmt.Sprintf("%t", !iface.Disabled),
				"description": iface.Comment,
			}
			if iface.MTU != nil {
				entry["mtu"] = fmt.Sprintf("%d", *iface.MTU)
			}
			result = append(result, entry)
		}
	}
	return result
}

func generateIPAddresses(devices []queries.Device, allIfaces map[string][]queries.Interface) []map[string]string {
	var result []map[string]string
	for _, d := range devices {
		devName := d.Identity
		if devName == "" {
			devName = d.Address
		}
		result = append(result, map[string]string{
			"address":     d.Address + "/32",
			"status":      "active",
			"dns_name":    devName,
			"description": "Management IP",
		})
	}
	return result
}

func generateCables(links []queries.Link, deviceByID map[string]queries.Device, allIfaces map[string][]queries.Interface) []map[string]string {
	var result []map[string]string
	for _, l := range links {
		devA, okA := deviceByID[l.DeviceAID]
		devB, okB := deviceByID[l.DeviceBID]
		if !okA || !okB {
			continue
		}
		nameA := devA.Identity
		if nameA == "" {
			nameA = devA.Address
		}
		nameB := devB.Identity
		if nameB == "" {
			nameB = devB.Address
		}
		cableType := "cat6"
		if l.LinkType == "wireless" {
			cableType = ""
		}
		result = append(result, map[string]string{
			"side_a_device": nameA,
			"side_a_name":   l.InterfaceA,
			"side_b_device": nameB,
			"side_b_name":   l.InterfaceB,
			"type":          cableType,
			"status":        "connected",
		})
	}
	return result
}

func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "+", "plus")
	s = strings.ReplaceAll(s, "/", "-")
	result := make([]byte, 0, len(s))
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, c)
		}
	}
	return string(result)
}

func netboxDeviceRole(board string) string {
	b := strings.ToUpper(board)
	for _, prefix := range []string{"CSS", "CRS"} {
		if strings.HasPrefix(b, prefix) {
			return "Switch"
		}
	}
	for _, prefix := range []string{"CAP", "WAP", "HAP", "AUDIENCE"} {
		if strings.HasPrefix(b, prefix) {
			return "Access Point"
		}
	}
	return "Router"
}

func netboxInterfaceType(ifType, ifName string) string {
	t := strings.ToLower(ifType)
	n := strings.ToLower(ifName)
	if t == "wlan" || strings.Contains(n, "wlan") || strings.Contains(n, "wifi") {
		return "ieee802.11ax"
	}
	if strings.HasPrefix(n, "sfp") || strings.HasPrefix(n, "qsfp") {
		return "10gbase-x-sfpp"
	}
	if t == "bridge" || strings.HasPrefix(n, "bridge") {
		return "bridge"
	}
	if t == "vlan" || strings.HasPrefix(n, "vlan") {
		return "virtual"
	}
	if strings.HasPrefix(n, "pppoe") || strings.HasPrefix(n, "l2tp") || strings.HasPrefix(n, "ovpn") || strings.HasPrefix(n, "wireguard") {
		return "virtual"
	}
	if n == "lo" || strings.HasPrefix(n, "loop") {
		return "virtual"
	}
	return "1000base-t"
}
