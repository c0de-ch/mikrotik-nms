//go:build unix

package flow

import (
	"net"
	"syscall"
)

// socketRcvBufSize returns the effective SO_RCVBUF of conn (Linux reports
// twice the requested size, which includes the kernel's bookkeeping).
func socketRcvBufSize(conn *net.UDPConn) int {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	n := 0
	_ = raw.Control(func(fd uintptr) {
		if v, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF); err == nil {
			n = v
		}
	})
	return n
}
