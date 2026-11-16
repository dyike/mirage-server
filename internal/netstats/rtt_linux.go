//go:build linux

package netstats

import (
	"time"

	"golang.org/x/sys/unix"
)

func socketRTT(fd uintptr) (time.Duration, bool) {
	info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil || info.Rtt == 0 {
		return 0, false
	}
	// Linux TCP_INFO reports tcpi_rtt in microseconds (also on Android).
	return time.Duration(info.Rtt) * time.Microsecond, true
}
