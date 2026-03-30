package routeros

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

const (
	mndpPort    = 5678
	mndpTimeout = 10 * time.Second
)

// MNDP TLV type IDs
const (
	tlvMACAddress  = 0x0001
	tlvIdentity    = 0x0005
	tlvVersion     = 0x0007
	tlvPlatform    = 0x0008
	tlvUptime      = 0x000A
	tlvSoftwareID  = 0x000B
	tlvBoard       = 0x000C
	tlvUnpack      = 0x000E
	tlvIPv6Address = 0x000F
	tlvInterfName  = 0x0010
	tlvIPAddress   = 0x0011
)

// DiscoveredDevice represents a MikroTik device found via MNDP.
type DiscoveredDevice struct {
	MACAddress string `json:"mac_address"`
	Identity   string `json:"identity"`
	Version    string `json:"version"`
	Platform   string `json:"platform"`
	Board      string `json:"board"`
	IPAddress  string `json:"ip_address"`
	IPv6Address string `json:"ipv6_address"`
	Interface  string `json:"interface"`
	Uptime     string `json:"uptime"`
	SoftwareID string `json:"software_id"`
	SourceAddr string `json:"source_addr"`
}

// ScanMNDP listens for MNDP broadcasts on UDP port 5678 for the given duration.
// It sends a discovery request and collects responses.
func ScanMNDP(duration time.Duration) ([]DiscoveredDevice, error) {
	if duration == 0 {
		duration = mndpTimeout
	}

	addr := &net.UDPAddr{Port: mndpPort}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		// Try without binding to specific port (might be in use)
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
		if err != nil {
			return nil, fmt.Errorf("listen UDP: %w", err)
		}
	}
	defer conn.Close()

	// Send MNDP discovery request (empty packet to broadcast)
	broadcastAddr := &net.UDPAddr{IP: net.IPv4bcast, Port: mndpPort}
	_, _ = conn.WriteToUDP([]byte{0, 0, 0, 0}, broadcastAddr)

	// Also try sending to 255.255.255.255
	_, _ = conn.WriteToUDP([]byte{0, 0, 0, 0}, &net.UDPAddr{IP: net.IPv4(255, 255, 255, 255), Port: mndpPort})

	conn.SetReadDeadline(time.Now().Add(duration))

	seen := make(map[string]DiscoveredDevice) // keyed by MAC
	buf := make([]byte, 4096)

	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Timeout is expected
			break
		}
		if n < 4 {
			continue
		}

		dev := parseMNDP(buf[:n])
		if dev.MACAddress == "" {
			continue
		}

		if remoteAddr != nil {
			dev.SourceAddr = remoteAddr.IP.String()
		}
		// If no IP in TLV, use source address
		if dev.IPAddress == "" && dev.SourceAddr != "" {
			dev.IPAddress = dev.SourceAddr
		}

		seen[dev.MACAddress] = dev
	}

	result := make([]DiscoveredDevice, 0, len(seen))
	for _, d := range seen {
		result = append(result, d)
	}
	return result, nil
}

func parseMNDP(data []byte) DiscoveredDevice {
	dev := DiscoveredDevice{}

	// Skip first 4 bytes (header)
	pos := 4
	for pos+4 <= len(data) {
		tlvType := binary.BigEndian.Uint16(data[pos : pos+2])
		tlvLen := binary.BigEndian.Uint16(data[pos+2 : pos+4])
		pos += 4

		if pos+int(tlvLen) > len(data) {
			break
		}

		value := data[pos : pos+int(tlvLen)]
		pos += int(tlvLen)

		switch tlvType {
		case tlvMACAddress:
			if len(value) == 6 {
				dev.MACAddress = fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X",
					value[0], value[1], value[2], value[3], value[4], value[5])
			}
		case tlvIdentity:
			dev.Identity = string(value)
		case tlvVersion:
			dev.Version = string(value)
		case tlvPlatform:
			dev.Platform = string(value)
		case tlvBoard:
			dev.Board = string(value)
		case tlvIPAddress:
			if len(value) == 4 {
				dev.IPAddress = fmt.Sprintf("%d.%d.%d.%d", value[0], value[1], value[2], value[3])
			}
		case tlvIPv6Address:
			ip := net.IP(value)
			dev.IPv6Address = ip.String()
		case tlvInterfName:
			dev.Interface = string(value)
		case tlvUptime:
			if len(value) == 4 {
				secs := binary.LittleEndian.Uint32(value)
				dev.Uptime = formatUptime(secs)
			}
		case tlvSoftwareID:
			dev.SoftwareID = string(value)
		}
	}

	return dev
}

func formatUptime(seconds uint32) string {
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	mins := (seconds % 3600) / 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}
