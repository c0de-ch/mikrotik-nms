package routeros

import (
	"net"
	"strconv"
	"time"
)

// Ping checks whether a device's API port is reachable with a plain TCP dial.
//
// It deliberately avoids the connection pool and the RouterOS protocol: a
// frequent liveness check must not contend with the heavier pollers on the
// shared per-client mutex, and transient API-level slowness should not flap a
// device's online/offline status. A successful TCP handshake to the API port
// is a reliable, cheap signal that the device is up. It is still a device
// dial, so it honours the dial policy like every other one.
func Ping(address string, port int, timeout time.Duration) error {
	if err := CheckTarget(address); err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(address, strconv.Itoa(port)), timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}
