package server

import (
	"golang.org/x/sys/unix"
	"net"
	"syscall"
	"time"
)

// Keep the kernel from hiding seconds of obsolete video behind successful TLS
// writes. TCP_INFO reads are advisory; unsupported listeners still work.
func videoTransport(conn net.Conn) func(int) (time.Duration, int) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return func(rate int) (time.Duration, int) { return 0, rate }
	}
	_ = tcp.SetWriteBuffer(64 << 10)
	raw, err := tcp.SyscallConn()
	if err != nil {
		return func(rate int) (time.Duration, int) { return 0, rate }
	}
	_ = raw.Control(func(fd uintptr) { _ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, 16<<10) })
	return func(rate int) (time.Duration, int) { return videoLinkBudget(raw, rate) }
}
func videoLinkBudget(raw syscall.RawConn, target int) (time.Duration, int) {
	var delay time.Duration
	pacing := target
	_ = raw.Control(func(fd uintptr) {
		info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if err != nil {
			return
		}
		if info.Delivery_rate == 0 && info.Notsent_bytes > 0 {
			delay = 250 * time.Millisecond
			return
		}
		if info.Delivery_rate > 0 {
			delay = time.Duration(float64(info.Notsent_bytes) / float64(info.Delivery_rate) * float64(time.Second))
		}
		pacing = linkPacingRate(target, time.Duration(info.Rtt)*time.Microsecond, delay, info.Delivery_rate)
	})
	return delay, pacing
}
