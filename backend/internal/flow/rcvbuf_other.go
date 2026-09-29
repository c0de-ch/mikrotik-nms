//go:build !unix

package flow

import "net"

// socketRcvBufSize is not available on this platform.
func socketRcvBufSize(*net.UDPConn) int { return 0 }
