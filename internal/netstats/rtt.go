// Package netstats reads transport diagnostics without sending probe traffic.
package netstats

import (
	"net"
	"syscall"
	"time"
)

// RTT returns the existing TCP connection's smoothed network round-trip time.
// It excludes decoding and rendering latency. Unsupported transports (including
// userspace tunnel sockets) and connections without a sample return false.
func RTT(conn net.Conn) (time.Duration, bool) {
	if wrapped, ok := conn.(interface{ NetConn() net.Conn }); ok {
		conn = wrapped.NetConn()
	}
	socket, ok := conn.(syscall.Conn)
	if !ok {
		return 0, false
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return 0, false
	}
	var rtt time.Duration
	var known bool
	if err := raw.Control(func(fd uintptr) { rtt, known = socketRTT(fd) }); err != nil {
		return 0, false
	}
	return rtt, known
}
